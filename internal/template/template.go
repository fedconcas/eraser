package template

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/eraser-privacy/eraser/internal/broker"
	"github.com/eraser-privacy/eraser/internal/config"
	"github.com/eraser-privacy/eraser/internal/history"
)

//go:embed templates/*.tmpl
var embeddedTemplates embed.FS

// EmailData contains all data available to email templates
type EmailData struct {
	// User profile
	FirstName      string
	LastName       string
	FullName       string
	Email          string
	OtherEmails    string // comma-separated additional emails, empty if none
	OtherNames     string // semicolon-separated name variants, empty if none
	OtherAddresses string // semicolon-separated previous addresses, empty if none
	Address        string
	City           string
	State          string
	ZipCode        string
	Country        string
	Phone          string
	OtherPhones    string // comma-separated additional phone numbers, empty if none
	DateOfBirth    string

	// Broker info
	BrokerName    string
	BrokerEmail   string
	BrokerWebsite string
	BrokerOptOut  string

	// Metadata
	Date     string
	Year     int
	Month    string
	Template string
}

// Email represents a rendered email ready to send
type Email struct {
	Subject string
	Body    string
}

// Engine handles email template rendering
type Engine struct {
	templates map[string]*template.Template
}

// NewEngine creates a new template engine
func NewEngine() (*Engine, error) {
	e := &Engine{
		templates: make(map[string]*template.Template),
	}

	templateNames := []string{"gdpr", "ccpa", "generic", "uk-access", "uk-erasure", "uk-combined"}
	for _, name := range templateNames {
		content, err := embeddedTemplates.ReadFile("templates/" + name + ".tmpl")
		if err != nil {
			return nil, fmt.Errorf("failed to read embedded template %s: %w", name, err)
		}

		tmpl, err := template.New(name).Parse(string(content))
		if err != nil {
			return nil, fmt.Errorf("failed to parse template %s: %w", name, err)
		}

		e.templates[name] = tmpl
	}

	return e, nil
}

// Render generates an email from a template
func (e *Engine) Render(templateName string, profile config.Profile, b broker.Broker) (*Email, error) {
	tmpl, ok := e.templates[templateName]
	if !ok {
		return nil, fmt.Errorf("unknown template: %s", templateName)
	}

	now := time.Now()
	data := EmailData{
		FirstName:   profile.FirstName,
		LastName:    profile.LastName,
		FullName:    profile.FullName(),
		Email:       profile.Email,
		OtherEmails: strings.Join(profile.AdditionalEmails, ", "),
		// Semicolons, like previous addresses below: a name variant may itself
		// contain a comma ("Smith, John"), and joining those with ", " would
		// read to the broker as two separate variants. Invisible for the
		// common case of a single variant.
		OtherNames:     strings.Join(profile.NameVariants, "; "),
		OtherAddresses: strings.Join(profile.PreviousAddresses, "; "),
		Address:        profile.Address,
		City:           profile.City,
		State:          profile.State,
		ZipCode:        profile.ZipCode,
		Country:        profile.Country,
		Phone:          profile.Phone,
		OtherPhones:    strings.Join(profile.AdditionalPhones, ", "),
		DateOfBirth:    profile.DateOfBirth,
		BrokerName:     b.Name,
		BrokerEmail:    b.Email,
		BrokerWebsite:  b.Website,
		BrokerOptOut:   b.OptOutURL,
		Date:           now.Format("January 2, 2006"),
		Year:           now.Year(),
		Month:          now.Format("January"),
		Template:       templateName,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("failed to render template: %w", err)
	}

	subject := e.getSubject(templateName, b.Name)

	return &Email{
		Subject: subject,
		Body:    buf.String(),
	}, nil
}

func (e *Engine) getSubject(templateName, brokerName string) string {
	return RequestSubjectFor(templateName)
}

// RequestSubjectFor returns the subject line a request sent with this
// template carries. Every broker gets the same subject for a given template -
// nothing about the recipient appears in it.
//
// Exported because inbox matching reads it back: a reply quoting one of these
// is a reply to us, which recognises brokers that answer from a helpdesk
// domain we'd never match on sender alone. Adding a template means adding its
// subject here (see GenericRequestSubject for the one that needs care).
func RequestSubjectFor(templateName string) string {
	switch templateName {
	case "gdpr":
		return "GDPR Data Erasure Request - Article 17 Right to Erasure"
	case "ccpa":
		return "CCPA Data Deletion Request - Right to Delete Personal Information"
	case "uk-access":
		return "Subject Access Request - Article 15 UK GDPR"
	case "uk-erasure":
		return "Request for Erasure - Article 17 UK GDPR"
	case "uk-combined":
		return "Subject Access Request (Art. 15) and Request for Erasure (Art. 17) - UK GDPR"
	default:
		return GenericRequestSubject
	}
}

// GenericRequestSubject is the fallback subject, used by the `generic`
// template and by any unrecognised name.
//
// It's called out separately because it's the weak one for reply matching:
// four ordinary words that a privacy newsletter or a competitor's marketing
// mail can contain verbatim, unlike the other five which name an article of
// the GDPR. Matching on it is only sound for someone who actually sends the
// generic template, which is why the matcher derives its subject set from the
// templates this install has really used rather than from all of them.
const GenericRequestSubject = "Personal Data Removal Request"

// RequestSubjects returns the subject lines for the named templates, skipping
// duplicates and unknown names.
func RequestSubjects(templateNames []string) []string {
	seen := make(map[string]bool, len(templateNames))
	var subjects []string
	for _, name := range templateNames {
		s := RequestSubjectFor(name)
		if seen[s] {
			continue
		}
		seen[s] = true
		subjects = append(subjects, s)
	}
	return subjects
}

// RequestBodyFingerprintFor returns a sentence from this template's body
// that never varies by recipient or sender profile - present in every
// request sent with it, quoted back verbatim in most replies that echo the
// message they're answering (a support-ticket auto-ack, a forwarded copy in
// a Gmail conversation thread). Complements RequestSubjectFor: a reply whose
// subject was rewritten by a ticketing system may still carry this in its
// quoted body.
//
// Every template's first paragraph opens with "To ... {{.BrokerName}},",
// which does vary; this is the fixed sentence right after it.
func RequestBodyFingerprintFor(templateName string) string {
	switch templateName {
	case "gdpr":
		return "I am writing to exercise my right to erasure under Article 17 of the General Data Protection Regulation (GDPR)."
	case "ccpa":
		return "I am a California resident writing to exercise my rights under the California Consumer Privacy Act (CCPA) and the California Privacy Rights Act (CPRA)."
	case "uk-access":
		return "I am writing to make a subject access request under Article 15 of the UK General Data Protection Regulation (UK GDPR) and the Data Protection Act 2018."
	case "uk-erasure":
		return "I am writing to exercise my right to erasure under Article 17 of the UK General Data Protection Regulation (UK GDPR) and the Data Protection Act 2018."
	case "uk-combined":
		return "I am writing to exercise two rights under the UK General Data Protection Regulation (UK GDPR) and the Data Protection Act 2018."
	default:
		return GenericRequestBodyFingerprint
	}
}

// GenericRequestBodyFingerprint is the fallback, used by the `generic`
// template and by any unrecognised name. Weak for the same reason
// GenericRequestSubject is - ordinary wording a newsletter could contain -
// which is why the matcher only enables it for an install that actually
// sends the generic template.
const GenericRequestBodyFingerprint = "I am writing to request the removal of my personal information from your database and any associated services."

// RequestBodyFingerprints returns the body fingerprints for the named
// templates, skipping duplicates and unknown names.
func RequestBodyFingerprints(templateNames []string) []string {
	seen := make(map[string]bool, len(templateNames))
	var fingerprints []string
	for _, name := range templateNames {
		f := RequestBodyFingerprintFor(name)
		if seen[f] {
			continue
		}
		seen[f] = true
		fingerprints = append(fingerprints, f)
	}
	return fingerprints
}

// RequestTypeFor reports which right a template exercises. Unknown templates
// are treated as erasure: every template that shipped before request types
// existed was a deletion request, and existing history rows default to the
// same value, so an unrecognised custom template keeps the old behaviour
// rather than silently creating a second sendable category per broker.
func RequestTypeFor(templateName string) string {
	switch templateName {
	case "uk-access":
		return history.RequestAccess
	case "uk-combined":
		return history.RequestCombined
	default:
		return history.RequestErasure
	}
}

// TemplateNames lists every built-in template, for CLI help and validation.
func TemplateNames() []string {
	return []string{"gdpr", "ccpa", "generic", "uk-access", "uk-erasure", "uk-combined"}
}

// IsKnownTemplate reports whether name is a built-in template.
func (e *Engine) IsKnownTemplate(name string) bool {
	_, ok := e.templates[name]
	return ok
}
