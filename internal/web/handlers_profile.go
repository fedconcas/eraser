package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/eraser-privacy/eraser/internal/config"
	"github.com/eraser-privacy/eraser/internal/email"
	"github.com/go-chi/chi/v5"
)

// errProfileNotFound and errLastProfile are the profile writes' failure
// modes, reported out of mutateConfig so the handler renders the right
// status without the config having been saved.
var (
	errProfileNotFound = errors.New("profile not found")
	errLastProfile     = errors.New("cannot delete the only configured profile")
)

// handleAPISwitchProfile sets the active-profile cookie (after validating
// the requested ID is actually configured) and redirects back to the page
// the switcher was submitted from, so every profile-scoped page picks up
// the new selection on next render.
func (s *Server) handleAPISwitchProfile(w http.ResponseWriter, r *http.Request) {
	limitFormBody(w, r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	id := r.FormValue("profile_id")
	if cfg := s.getConfig(); cfg != nil {
		for _, p := range cfg.GetProfiles() {
			if p.ID == id {
				http.SetCookie(w, &http.Cookie{
					Name:     activeProfileCookie,
					Value:    id,
					Path:     "/",
					SameSite: http.SameSiteLaxMode,
					MaxAge:   365 * 24 * 60 * 60,
				})
				break
			}
		}
	}

	redirect := r.FormValue("redirect")
	if redirect == "" || !strings.HasPrefix(redirect, "/") || strings.HasPrefix(redirect, "//") {
		redirect = "/"
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

// buildProfileFromForm parses and validates the profile-form fields shared
// by the setup wizard (handleSetupProfile) and this "add profile" settings
// form: first/middle/last name, email, additional emails, and the optional
// address fields. Returns the parsed profile and a field->message map of
// validation errors (empty if valid) - factored out so the two handlers can't
// drift on what "a valid profile" means, the way they previously did as two
// independent copies of the same three checks.
//
// prev is the profile being edited (config.Profile{} when creating a new
// one), and every field this form does *not* collect is carried over from it.
// That matters because both callers assign the result over the whole stored
// struct: building a fresh Profile here silently erased anything without an
// input the moment someone edited their city in the web UI, quietly weakening
// every subsequent removal request with no visible sign. date_of_birth still
// has no input in the shared partial and relies on exactly this. Start from
// prev, overwrite what the form owns.
//
// Fields the form *does* own are read back unconditionally, so clearing a
// field in the UI actually clears it. Ownership is decided by whether the
// input was present in the submission (r.Form) rather than whether it was
// left blank - an absent input means "this page doesn't edit that field, keep
// it", while a present-but-empty one means "the user cleared it". Keying off
// emptiness instead would make the list fields impossible to empty once set,
// since the old values would resurrect on every save.
func buildProfileFromForm(r *http.Request, prev config.Profile) (config.Profile, map[string]string) {
	// r.FormValue parses on demand but discards the error and, more
	// importantly here, we need r.Form itself for the presence checks below.
	_ = r.ParseForm()

	profile := prev
	profile.FirstName = strings.TrimSpace(r.FormValue("first_name"))
	profile.MiddleName = strings.TrimSpace(r.FormValue("middle_name"))
	profile.LastName = strings.TrimSpace(r.FormValue("last_name"))
	profile.Email = strings.TrimSpace(r.FormValue("email"))
	profile.Address = strings.TrimSpace(r.FormValue("address"))
	profile.City = strings.TrimSpace(r.FormValue("city"))
	profile.State = strings.TrimSpace(r.FormValue("state"))
	profile.ZipCode = strings.TrimSpace(r.FormValue("zip_code"))
	profile.Country = strings.TrimSpace(r.FormValue("country"))
	profile.Phone = strings.TrimSpace(r.FormValue("phone"))
	if _, ok := r.Form["dob"]; ok {
		profile.DateOfBirth = strings.TrimSpace(r.FormValue("dob"))
	}

	errors := make(map[string]string)
	if profile.FirstName == "" {
		errors["first_name"] = "First name is required"
	}
	if profile.LastName == "" {
		errors["last_name"] = "Last name is required"
	}
	if profile.Email == "" {
		errors["email"] = "Email is required"
	} else if err := email.ValidateEmail(profile.Email); err != nil {
		errors["email"] = "Please enter a valid email address"
	}

	// Split before validating, not after: ValidateEmail rejects commas and
	// semicolons outright, so a whole list handed to it never passes.
	profile.AdditionalEmails = parseListField(r, errors, listField{
		name:    "additional_emails",
		label:   "email addresses",
		seps:    listSepsWithComma,
		primary: profile.Email,
		prev:    prev.AdditionalEmails,
		validate: func(v string) bool {
			return email.ValidateEmail(v) == nil
		},
		invalidMsg: "Not a valid email address",
	})
	profile.NameVariants = parseListField(r, errors, listField{
		name:  "name_variants",
		label: "name variants",
		seps:  listSepsLinesOnly,
		prev:  prev.NameVariants,
	})
	profile.PreviousAddresses = parseListField(r, errors, listField{
		name:  "previous_addresses",
		label: "previous addresses",
		seps:  listSepsLinesOnly,
		prev:  prev.PreviousAddresses,
	})
	profile.AdditionalPhones = parseListField(r, errors, listField{
		name:    "additional_phones",
		label:   "phone numbers",
		seps:    listSepsWithComma,
		primary: profile.Phone,
		prev:    prev.AdditionalPhones,
	})

	return profile, errors
}

// Separator sets for the profile's list textareas. A comma is only accepted
// as a separator for fields whose values can never *contain* one: an email
// address can't (email.ValidateEmail rejects it outright) and neither can a
// phone number, so accepting both there costs nothing and matches the format
// the CLI prompts ask for. A street address routinely contains commas
// ("123 Main St, Riga") and a name variant can ("Smith, John"), so those split
// on line breaks only - splitting them on commas would quietly shred one
// entry into bogus fragments and send them to a broker as separate former
// residences. The CLI draws the same distinction, which is why its addresses
// prompt is the one that uses a semicolon (see cmd_init.go).
//
// Both "\n" and "\r" are separators rather than just "\n". TrimSpace already
// strips the trailing "\r" a browser's CRLF line ending leaves behind, so
// that much is handled either way; carrying "\r" is for the rarer input
// containing a bare CR, which would otherwise survive inside an entry and be
// written straight into the outgoing email body.
const (
	listSepsLinesOnly = "\n\r"
	listSepsWithComma = ",\n\r"
)

// Guard rails on the list textareas. These are not format validation (see
// listField.validate for the one field that has any) - they just stop a
// pathological paste from reaching an outgoing legal request. Each list
// renders as a single unwrapped line in the email body, and RFC 5321 caps a
// line at 998 octets before MTAs may reject or rewrap the message, so an
// unbounded field is a deliverability problem as well as a config-size one.
// Real profiles carry a handful of entries; these limits are far above that
// and only bite on obvious abuse.
const (
	maxListEntries  = 25
	maxListEntryLen = 200
)

// listField describes one of the profile's list-valued textareas.
type listField struct {
	name    string // form field name, also the key used in the errors map
	label   string // human-readable plural, for error messages
	seps    string // separator character set, see listSeps* above
	primary string // value this list must not merely repeat ("" if none)
	prev    []string
	// validate reports whether one entry is acceptable. nil means the field
	// has no format rules - true of everything except email addresses, since
	// there's no sane canonical form for a name, a street address or an
	// international phone number, and rejecting a legitimate value would
	// block someone from exercising a legal right.
	validate   func(string) bool
	invalidMsg string
}

// parseListField reads one list textarea, applying the same presence rule the
// scalar fields use: an absent input means this page doesn't edit the field,
// so the previous value is kept; a present-but-empty one means the user
// cleared it. On any error the raw entries are returned rather than the
// normalized ones, so the re-rendered textarea still shows the text the error
// refers to.
func parseListField(r *http.Request, errors map[string]string, f listField) []string {
	if _, ok := r.Form[f.name]; !ok {
		return f.prev
	}

	entries := config.SplitAndTrimAny(r.FormValue(f.name), f.seps)

	if len(entries) > maxListEntries {
		errors[f.name] = fmt.Sprintf("Too many %s (%d) - the limit is %d", f.label, len(entries), maxListEntries)
		return entries
	}
	var tooLong, invalid []string
	for _, e := range entries {
		if len(e) > maxListEntryLen {
			tooLong = append(tooLong, e[:40]+"...")
			continue
		}
		if f.validate != nil && !f.validate(e) {
			invalid = append(invalid, e)
		}
	}
	if len(tooLong) > 0 {
		errors[f.name] = fmt.Sprintf("Too long (limit %d characters): %s", maxListEntryLen, strings.Join(tooLong, ", "))
		return entries
	}
	if len(invalid) > 0 {
		errors[f.name] = f.invalidMsg + ": " + strings.Join(invalid, ", ")
		return entries
	}

	return config.NormalizeList(f.primary, entries)
}

// handleSettingsProfileNew adds a second (or third, ...) named profile from
// the web UI - previously only possible via `eraser profile add` on the
// CLI. Only collects the same core fields the setup wizard's profile step
// does; email/SMTP configuration is shared across all profiles, so there's
// nothing else to ask for here.
func (s *Server) handleSettingsProfileNew(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		limitFormBody(w, r)
		// A brand new profile has nothing to carry forward.
		profile, errors := buildProfileFromForm(r, config.Profile{})

		if len(errors) > 0 {
			s.renderWithCSRF(w, r, "settings/profile-new.html", map[string]interface{}{
				"Title":   "Add Profile",
				"Profile": profile,
				"Errors":  errors,
			})
			return
		}

		if err := s.mutateConfig(func(cfg *config.Config) error {
			existing := cfg.GetProfiles()
			cfg.Profiles = append(append([]config.NamedProfile{}, existing...), config.NamedProfile{
				ID:      config.SlugifyProfileID(profile.FirstName, profile.LastName, existing),
				Profile: profile,
			})
			return nil
		}); err != nil {
			s.renderWithCSRF(w, r, "settings/profile-new.html", map[string]interface{}{
				"Title":   "Add Profile",
				"Profile": profile,
				"Errors":  map[string]string{"_": "Failed to save configuration: " + err.Error()},
			})
			return
		}

		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	s.renderWithCSRF(w, r, "settings/profile-new.html", map[string]interface{}{
		"Title":   "Add Profile",
		"Profile": config.Profile{},
		"Errors":  map[string]string{},
	})
}

// handleSettingsProfileEdit edits an existing profile's fields. The
// profile's ID itself is never changed here - NamedProfile.ID is stored
// verbatim in history.db, so changing it would orphan that profile's
// existing send history - only its Profile fields (name, email, address...)
// are updated.
func (s *Server) handleSettingsProfileEdit(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "profileID")

	cfg := s.getConfig()
	if cfg == nil {
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	existing, err := cfg.GetProfile(id)
	if err != nil {
		http.Error(w, "Profile not found", http.StatusNotFound)
		return
	}

	if r.Method == "POST" {
		limitFormBody(w, r)
		// Carry forward the CLI-only fields on the profile being edited.
		// Not named "errors": this handler also checks errors.Is below.
		profile, fieldErrors := buildProfileFromForm(r, existing.Profile)

		if len(fieldErrors) > 0 {
			s.renderWithCSRF(w, r, "settings/profile-edit.html", map[string]interface{}{
				"Title":     "Edit Profile",
				"ProfileID": id,
				"Profile":   profile,
				"Errors":    fieldErrors,
			})
			return
		}

		err := s.mutateConfig(func(cfg *config.Config) error {
			if len(cfg.Profiles) == 0 {
				// Legacy single-profile mode (no profiles: list yet) - write
				// back to the top-level profile: block rather than promoting
				// to a profiles: list just because it was edited.
				cfg.Profile = profile
				return nil
			}
			updated := make([]config.NamedProfile, len(cfg.Profiles))
			copy(updated, cfg.Profiles)
			for i, p := range updated {
				if strings.EqualFold(p.ID, existing.ID) {
					updated[i].Profile = profile
					cfg.Profiles = updated
					return nil
				}
			}
			return errProfileNotFound
		})
		switch {
		case errors.Is(err, errProfileNotFound):
			http.Error(w, "Profile not found", http.StatusNotFound)
			return
		case err != nil:
			s.renderWithCSRF(w, r, "settings/profile-edit.html", map[string]interface{}{
				"Title":     "Edit Profile",
				"ProfileID": id,
				"Profile":   profile,
				"Errors":    map[string]string{"_": "Failed to save configuration: " + err.Error()},
			})
			return
		}

		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	s.renderWithCSRF(w, r, "settings/profile-edit.html", map[string]interface{}{
		"Title":     "Edit Profile",
		"ProfileID": existing.ID,
		"Profile":   existing.Profile,
		"Errors":    map[string]string{},
	})
}

// handleSettingsProfileDelete removes a profile from the profiles: list.
// It never deletes that profile's send history - removal_requests rows
// stay in history.db tagged with the now-orphaned profile ID, and become
// visible again if a profile with the same ID is re-added later. Refuses
// to remove the only configured profile, since every profile-scoped
// handler assumes there's always at least one.
func (s *Server) handleSettingsProfileDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "profileID")

	cfg := s.getConfig()
	if cfg == nil {
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	// The list is re-read inside the write, so "the last profile" is decided
	// against the config actually being saved rather than a snapshot another
	// request may already have replaced.
	err := s.mutateConfig(func(cfg *config.Config) error {
		profiles := cfg.GetProfiles()
		if len(profiles) <= 1 {
			return errLastProfile
		}
		remaining := make([]config.NamedProfile, 0, len(profiles)-1)
		found := false
		for _, p := range profiles {
			if strings.EqualFold(p.ID, id) {
				found = true
				continue
			}
			remaining = append(remaining, p)
		}
		if !found {
			return errProfileNotFound
		}
		cfg.Profiles = remaining
		return nil
	})
	switch {
	case errors.Is(err, errLastProfile):
		http.Error(w, "Can't delete the only configured profile", http.StatusBadRequest)
		return
	case errors.Is(err, errProfileNotFound):
		http.Error(w, "Profile not found", http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, "Failed to save configuration: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// If the deleted profile was the active one, clear the cookie instead
	// of leaving it pointing at an ID that no longer resolves - activeProfile
	// falls back to the first configured profile once it's gone.
	if cookie, err := r.Cookie(activeProfileCookie); err == nil && strings.EqualFold(cookie.Value, id) {
		http.SetCookie(w, &http.Cookie{
			Name:     activeProfileCookie,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			SameSite: http.SameSiteLaxMode,
		})
	}

	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}
