package inbox

import (
	"bytes"
	"testing"

	"github.com/emersion/go-imap"
	"github.com/eraser-privacy/eraser/internal/broker"
	"github.com/eraser-privacy/eraser/internal/config"
)

const ukAccessSubject = "Subject Access Request - Article 15 UK GDPR"
const gdprSubject = "GDPR Data Erasure Request - Article 17 Right to Erasure"

// parseFull runs a whole message through the matcher: sender, subject and
// body all matter now that a reply can be attributed by the address quoted
// back at us.
func parseFull(t *testing.T, m *Monitor, from, subject, body string) *Email {
	t.Helper()
	at := -1
	for i := len(from) - 1; i >= 0; i-- {
		if from[i] == '@' {
			at = i
			break
		}
	}
	if at < 0 {
		t.Fatalf("bad address %q", from)
	}
	section := &imap.BodySectionName{}
	msg := &imap.Message{
		Uid: 11,
		Envelope: &imap.Envelope{
			Subject: subject,
			From:    []*imap.Address{{MailboxName: from[:at], HostName: from[at+1:]}},
		},
		Body: map[*imap.BodySectionName]imap.Literal{
			section: bytes.NewReader(buildMIMEMessage(t, body)),
		},
	}
	email, err := m.parseMessage(msg, section)
	if err != nil {
		t.Fatalf("parseMessage: %v", err)
	}
	if email == nil {
		t.Fatal("parseMessage returned nil")
	}
	return email
}

// liveMonitor mirrors the real account: brokers that were written to, the
// addresses used, and the subjects actually sent.
func liveMonitor(t *testing.T) *Monitor {
	t.Helper()
	brokers := []broker.Broker{
		{ID: "anywho", Name: "AnyWho", Email: "", Website: "https://anywho.com"},
		{ID: "experian-marketing", Name: "Experian Marketing Services", Email: "privacy@experian.com"},
		{ID: "disco-technology", Name: "Disco Technology", Email: "privacy@discotechnology.com"},
		{ID: "freewheel-media", Name: "Freewheel Media Inc", Email: "legalnotices@freewheel.com"},
		{ID: "mylife", Name: "MyLife", Email: "privacy@mylife.com"},
		{ID: "knowwho", Name: "KnowWho", Email: "privacy@knowwho.com"},
		{ID: "hubspot", Name: "HubSpot", Email: "privacy@hubspot.com"},
	}
	m := NewMonitor(config.InboxConfig{Email: "fedconcas@gmail.com"}, brokers)
	m.SetContactedBrokers(map[string]bool{
		"experian-marketing": true, "disco-technology": true, "freewheel-media": true,
		"mylife": true, "knowwho": true, "hubspot": true,
	})
	m.SetContactedAddresses(map[string]string{
		"privacy@experian.com":        "experian-marketing",
		"privacy@discotechnology.com": "disco-technology",
		"legalnotices@freewheel.com":  "freewheel-media",
		"privacy@mylife.com":          "mylife",
		"privacy@knowwho.com":         "knowwho",
		"privacy@hubspot.com":         "hubspot",
	})
	m.SetRequestSubjects([]string{ukAccessSubject, gdprSubject})
	return m
}

// The eight replies found sitting unmatched in the live INBOX, each answering
// from a helpdesk tenant or a parent company rather than the address we wrote
// to. Every one must now be recognised as a reply; attribution is best-effort.
func TestRealWorldHelpdeskReplies(t *testing.T) {
	// A ticketing auto-acknowledgement quotes the message it's answering, so
	// the address we originally wrote to appears in the body.
	quoted := func(addr string) string {
		return "Your request has been received.\r\n\r\n> To: " + addr +
			"\r\n> Subject: " + ukAccessSubject + "\r\n> I am writing to exercise my right..."
	}

	tests := []struct {
		name        string
		from        string
		subject     string
		body        string
		wantMatched bool
		wantBroker  string // "" means: matched but unattributed
	}{
		{
			name: "zendesk tenant, body quotes the address we wrote to",
			from: "ccpa-consumer@freewheel.zendesk.com", subject: "[Request received] " + ukAccessSubject,
			body:        quoted("legalnotices@freewheel.com"),
			wantMatched: true, wantBroker: "freewheel-media",
		},
		{
			name: "jira tenant, body quotes the address",
			from: "support@mylifecs.atlassian.net", subject: "Re: " + ukAccessSubject + " - MCC-3204437",
			body:        quoted("privacy@mylife.com"),
			wantMatched: true, wantBroker: "mylife",
		},
		{
			name: "jira tenant, tenant label matches the broker name",
			from: "jira@experian-marketing-services.atlassian.net", subject: "PRIV-106538 [EXTERNAL] " + ukAccessSubject,
			body:        "Ticket created.",
			wantMatched: true, wantBroker: "experian-marketing",
		},
		{
			name: "zendesk tenant, tenant label matches the broker id",
			from: "privacy@discotechnology.zendesk.com", subject: `Your Ticket "` + ukAccessSubject + `" Has Been Received`,
			body:        "Ticket created.",
			wantMatched: true, wantBroker: "disco-technology",
		},
		{
			name: "parent company, body quotes the address",
			from: "bbrownson@quorum.us", subject: "Out of office Re: " + ukAccessSubject,
			body:        quoted("privacy@knowwho.com"),
			wantMatched: true, wantBroker: "knowwho",
		},
		{
			name: "broker subdomain",
			from: "privacyrequest@privacy.hubspot.com", subject: "Privacy Request: " + ukAccessSubject,
			body:        "Verify your email.",
			wantMatched: true, wantBroker: "hubspot",
		},
		{
			name: "generic helpdesk relay, nothing to attribute to",
			from: "1746136851@tickets.helpdesk.com", subject: "Re: " + ukAccessSubject,
			body:        "Your ticket is open.",
			wantMatched: true, wantBroker: "",
		},
		{
			name: "unknown corporate domain, nothing to attribute to",
			from: "legaldsr@perion.com", subject: ukAccessSubject,
			body:        "We have received your request.",
			wantMatched: true, wantBroker: "",
		},
		{
			name: "zendesk tenant for a broker with no contact address stays unattributed",
			from: "support@anywho.zendesk.com", subject: "Re: " + gdprSubject + " 6553159",
			body:        "Ticket 6553159 opened.",
			wantMatched: true, wantBroker: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := liveMonitor(t)
			got := parseFull(t, m, tt.from, tt.subject, tt.body)

			if tt.wantMatched && got.BrokerID == "" {
				t.Fatalf("%s should be recognised as a reply, but wasn't", tt.from)
			}
			if !tt.wantMatched && got.BrokerID != "" {
				t.Fatalf("%s should not have matched, got %q", tt.from, got.BrokerID)
			}
			if tt.wantBroker != "" {
				if !got.Attributed || got.BrokerID != tt.wantBroker {
					t.Errorf("expected attribution to %q, got %q (attributed=%v, via=%s)",
						tt.wantBroker, got.BrokerID, got.Attributed, got.MatchedVia)
				}
				return
			}
			if got.Attributed {
				t.Errorf("expected an unattributed match, got attributed %q", got.BrokerID)
			}
			if !IsUnattributed(got.BrokerID) {
				t.Errorf("unattributed reply should carry a placeholder ID, got %q", got.BrokerID)
			}
		})
	}
}

// TestReplyDomainCatchesADedicatedCaseTrackingDomain pins a live-account
// miss: LexisNexis Risk Solutions is contacted at lexisnexis.com, but its
// case-tracking system replies from lexisnexisrisk.com - a distinct
// registrable domain, not a subdomain of lexisnexis.com and not a helpdesk
// host, so no existing rule could see it. broker.Broker.ReplyDomains exists
// for exactly this.
func TestReplyDomainCatchesADedicatedCaseTrackingDomain(t *testing.T) {
	brokers := []broker.Broker{
		{
			ID: "lexisnexis", Name: "LexisNexis Risk Solutions",
			Email: "privacy.information.mgr@lexisnexis.com", Website: "https://risk.lexisnexis.com",
			ReplyDomains: []string{"lexisnexisrisk.com"},
		},
	}
	m := NewMonitor(config.InboxConfig{Email: "fedconcas@gmail.com"}, brokers)
	m.SetContactedBrokers(map[string]bool{"lexisnexis": true})

	got := parseFull(t, m, "dpo@lexisnexisrisk.com",
		"Case 00142513: Request Acknowledgement - Further Information Required",
		"We are processing your request.")

	if !got.Attributed || got.BrokerID != "lexisnexis" {
		t.Errorf("expected attribution to lexisnexis via its reply domain, got %q (attributed=%v, via=%s)",
			got.BrokerID, got.Attributed, got.MatchedVia)
	}

	t.Run("gated on the contacted-broker check like every other domain rule", func(t *testing.T) {
		m := NewMonitor(config.InboxConfig{Email: "fedconcas@gmail.com"}, brokers)
		m.SetContactedBrokers(map[string]bool{}) // never contacted
		got := parseFull(t, m, "dpo@lexisnexisrisk.com", "Case 00142513: Request Acknowledgement", "")
		if got.BrokerID != "" {
			t.Errorf("a reply domain must not bypass the contacted-broker gate, got %q", got.BrokerID)
		}
	})
}

// TestHelpdeskAndSubdomainMatchWithoutQuotingOurSubject pins the real-world
// failure this fixes: a ticketing platform rewrites the subject on most of
// the messages a ticket produces - a satisfaction survey, a "ticket updated"
// notice, a canned acknowledgement - and quotes our request text on none of
// them. Checked against a live account, rules 2 and 3 were firing on
// approximately zero recognised replies because of this: the tenant/subdomain
// match is exact, private-state evidence on its own and must not need a
// subject match to fire.
func TestHelpdeskAndSubdomainMatchWithoutQuotingOurSubject(t *testing.T) {
	tests := []struct {
		name       string
		from       string
		subject    string
		wantBroker string
	}{
		{
			name:       "zendesk satisfaction survey, no request subject anywhere",
			from:       "support@discotechnology.zendesk.com",
			subject:    "How would you rate the support you received?",
			wantBroker: "disco-technology",
		},
		{
			name:       "jira ticket-updated notice, no request subject anywhere",
			from:       "jira@experian-marketing-services.atlassian.net",
			subject:    "Your ticket has been updated",
			wantBroker: "experian-marketing",
		},
		{
			name:       "broker subdomain, subject bears no resemblance to our request",
			from:       "privacyrequest@privacy.hubspot.com",
			subject:    "Ticket #48213 closed",
			wantBroker: "hubspot",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := liveMonitor(t)
			got := parseFull(t, m, tt.from, tt.subject, "no useful body text")
			if !got.Attributed || got.BrokerID != tt.wantBroker {
				t.Errorf("expected attribution to %q, got %q (attributed=%v, via=%s)",
					tt.wantBroker, got.BrokerID, got.Attributed, got.MatchedVia)
			}
		})
	}
}

// TestPrivacyPlatformSenderNameAttribution pins a second live-account miss,
// distinct from the helpdesk-tenant case: OneTrust sends every client's
// notifications from the same noreply@m.onetrust.com regardless of which
// broker it's for, so neither sender-domain nor subdomain matching can ever
// see it. The client is named only in the display name, and not always by
// the name the broker is recorded under - "Equimine" in the database, "On
// behalf of PropStream" in the notification, both routed through
// propstream.com.
func TestPrivacyPlatformSenderNameAttribution(t *testing.T) {
	brokers := []broker.Broker{
		{ID: "equimine", Name: "Equimine", Email: "privacyinquiry@propstream.com"},
		{ID: "lightcast", Name: "Lightcast", Email: "legal@lightcast.io"},
		{ID: "acuant", Name: "Acuant, Inc.", Email: "compliance@gbgplc.com", ReplyNames: []string{"GBG"}},
	}
	m := NewMonitor(config.InboxConfig{Email: "fedconcas@gmail.com"}, brokers)
	m.SetContactedBrokers(map[string]bool{"equimine": true, "lightcast": true, "acuant": true})

	tests := []struct {
		name       string
		fromName   string
		wantBroker string
	}{
		{"brand name differs from the broker's recorded name", "On behalf of PropStream", "equimine"},
		{"display name is literally the address the broker gave the platform", "privacy@lightcast.io", "lightcast"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			email := &Email{
				From: "noreply@m.onetrust.com", FromName: tt.fromName, FromDomain: "m.onetrust.com",
				Subject: "(Request ID: N4GW552JCR) Your Privacy Request",
			}
			got := m.matchReply(email)
			if !got.Attributed || got.BrokerID != tt.wantBroker {
				t.Errorf("expected attribution to %q, got %q (attributed=%v, via=%s)",
					tt.wantBroker, got.BrokerID, got.Attributed, got.Via)
			}
		})
	}

	t.Run("a parent company's brand resolves via ReplyNames", func(t *testing.T) {
		// GBG's own template names the parent company, not Acuant - nothing
		// about that relationship is derivable from the broker's own domain
		// or name, so it has to be recorded explicitly (see the acuant
		// broker's reply_names above).
		email := &Email{
			From: "noreply@m.onetrust.com", FromName: "GBG Privacy and Data Compliance Team", FromDomain: "m.onetrust.com",
			Subject: "(Request ID: N2TNMC8BKF) Request logged successfully",
		}
		got := m.matchReply(email)
		if !got.Attributed || got.BrokerID != "acuant" {
			t.Errorf("expected attribution to acuant via ReplyNames, got %q (attributed=%v, via=%s)",
				got.BrokerID, got.Attributed, got.Via)
		}
	})

	t.Run("a parent-company brand absent from ReplyNames stays unattributed", func(t *testing.T) {
		email := &Email{
			From: "noreply@m.onetrust.com", FromName: "Some Unrelated Parent Co", FromDomain: "m.onetrust.com",
			Subject: "Your ticket has been updated",
		}
		got := m.matchReply(email)
		if got.Attributed {
			t.Errorf("expected no confident attribution, got %q via %s", got.BrokerID, got.Via)
		}
	})

	t.Run("not gated on the request subject, like the other domain-evidence rules", func(t *testing.T) {
		email := &Email{
			From: "noreply@m.onetrust.com", FromName: "On behalf of PropStream", FromDomain: "m.onetrust.com",
			Subject: "Your ticket has been updated", // never quotes our request text
		}
		got := m.matchReply(email)
		if !got.Attributed || got.BrokerID != "equimine" {
			t.Errorf("got %+v, want attributed to equimine regardless of subject", got)
		}
	})

	t.Run("only applies to a known privacy-platform sender domain", func(t *testing.T) {
		email := &Email{
			From: "billing@propstream-invoices.example", FromName: "On behalf of PropStream",
			FromDomain: "propstream-invoices.example",
			Subject:    "Your ticket has been updated",
		}
		got := m.matchReply(email)
		if got.BrokerID != "" {
			t.Errorf("an ordinary sender must not be attributed by display name alone, got %q", got.BrokerID)
		}
	})

	t.Run("a short or generic root does not match", func(t *testing.T) {
		short := NewMonitor(config.InboxConfig{Email: "fedconcas@gmail.com"}, []broker.Broker{
			{ID: "cb", Name: "CB Inc", Email: "privacy@cb.io"},
		})
		short.SetContactedBrokers(map[string]bool{"cb": true})
		email := &Email{
			From: "noreply@m.onetrust.com", FromName: "On behalf of Bob's Cabinets", FromDomain: "m.onetrust.com",
			Subject: "Your ticket has been updated",
		}
		if got := short.matchReply(email); got.Attributed {
			t.Errorf("a 2-letter domain root must not attribute, got %q", got.BrokerID)
		}
	})
}

// Subject matching must not become a way around the sender filtering that was
// added to stop ordinary mail being classified as broker replies.
func TestSubjectMatchingDoesNotBypassSenderGuards(t *testing.T) {
	body := "> Subject: " + ukAccessSubject

	t.Run("our own sent request is not a reply to itself", func(t *testing.T) {
		m := liveMonitor(t)
		got := parseFull(t, m, "fedconcas@gmail.com", ukAccessSubject, body)
		if got.BrokerID != "" {
			t.Errorf("our own address must never match, got %q", got.BrokerID)
		}
	})

	t.Run("own address on a non-free-mail domain is still excluded", func(t *testing.T) {
		m := liveMonitor(t)
		m.config.Email = "me@myowndomain.example"
		got := parseFull(t, m, "me@myowndomain.example", ukAccessSubject, body)
		if got.BrokerID != "" {
			t.Errorf("our own address must never match, got %q", got.BrokerID)
		}
	})

	t.Run("general-purpose sender forging the subject is rejected", func(t *testing.T) {
		m := liveMonitor(t)
		for _, from := range []string{"noreply-accounts@google.com", "someone@gmail.com", "x@hotmail.com"} {
			if got := parseFull(t, m, from, "Re: "+ukAccessSubject, body); got.BrokerID != "" {
				t.Errorf("%s forged the subject and matched %q; the denylist must still apply", from, got.BrokerID)
			}
		}
	})

	t.Run("a delivery failure notice is not a reply", func(t *testing.T) {
		m := liveMonitor(t)
		got := parseFull(t, m, "postmaster@cowen.com", "Undeliverable: "+ukAccessSubject,
			"Your message could not be delivered. The recipient address was rejected: 550 5.1.1 user unknown")
		if got.BrokerID != "" {
			t.Errorf("a bounce must not be filed as a broker reply (it feeds cleanup-bounces), got %q", got.BrokerID)
		}
	})

	t.Run("unrelated mail with no request subject is untouched", func(t *testing.T) {
		m := liveMonitor(t)
		if got := parseFull(t, m, "hello@newsletter.example", "Our weekly roundup", "nothing to see"); got.BrokerID != "" {
			t.Errorf("ordinary mail matched %q", got.BrokerID)
		}
	})

	t.Run("subject matching is off when no templates were sent", func(t *testing.T) {
		m := liveMonitor(t)
		m.SetRequestSubjects(nil)
		if got := parseFull(t, m, "support@mylifecs.atlassian.net", "Re: "+ukAccessSubject, ""); got.BrokerID != "" {
			t.Errorf("with no known request subjects nothing should match by subject, got %q", got.BrokerID)
		}
	})
}

// The helpdesk host itself is never the broker.
func TestHelpdeskHostIsNeverTheBroker(t *testing.T) {
	m := liveMonitor(t)
	m.SetContactedBrokers(map[string]bool{"zendesk": true, "mylife": true})

	got := parseFull(t, m, "noreply@zendesk.com", "Re: "+ukAccessSubject, "hello")
	if got.Attributed {
		t.Errorf("zendesk.com itself must not attribute to a broker, got %q", got.BrokerID)
	}
}

func TestHelpdeskTenant(t *testing.T) {
	tests := []struct {
		domain     string
		wantTenant string
		wantOK     bool
	}{
		{"anywho.zendesk.com", "anywho", true},
		{"experian-marketing-services.atlassian.net", "experian-marketing-services", true},
		{"acme.freshdesk.com", "acme", true},
		{"zendesk.com", "", false},
		{"atlassian.net", "", false},
		{"example.com", "", false},
	}
	for _, tt := range tests {
		gotTenant, gotOK := helpdeskTenant(tt.domain)
		if gotOK != tt.wantOK || gotTenant != tt.wantTenant {
			t.Errorf("helpdeskTenant(%q) = (%q, %v), want (%q, %v)",
				tt.domain, gotTenant, gotOK, tt.wantTenant, tt.wantOK)
		}
	}
}

func TestParentDomain(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"privacy.hubspot.com", "hubspot.com", true},
		{"a.b.example.com", "b.example.com", true},
		{"example.com", "", false},
		{"localhost", "", false},
	}
	for _, tt := range tests {
		got, ok := parentDomain(tt.in)
		if ok != tt.wantOK || got != tt.want {
			t.Errorf("parentDomain(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestNormalizeSlug(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"Experian Marketing Services", "experianmarketingservices"},
		{"experian-marketing-services", "experianmarketingservices"},
		{"disco-technology", "discotechnology"},
		{"MyLife", "mylife"},
		{"", ""},
	} {
		if got := normalizeSlug(tt.in); got != tt.want {
			t.Errorf("normalizeSlug(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// The spam rule: a reply is only pulled out of the spam folder when we could
// actually attribute it, because the subject that identifies it as a reply is
// chosen by whoever sent it.
func TestArchiveDecision(t *testing.T) {
	attributed := Email{BrokerID: "mylife", Attributed: true}
	unattributed := Email{BrokerID: "unattributed:tickets.helpdesk.com"}
	unmatched := Email{}

	tests := []struct {
		name    string
		email   Email
		inSpam  bool
		allowed bool
	}{
		{"attributed in inbox moves", attributed, false, true},
		{"unattributed in inbox moves", unattributed, false, true},
		{"attributed in spam is rescued", attributed, true, true},
		{"unattributed in spam is left in place", unattributed, true, false},
		{"unmatched never moves", unmatched, false, false},
		{"unmatched in spam never moves", unmatched, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := ArchiveDecision(tt.email, tt.inSpam)
			if got != tt.allowed {
				t.Errorf("allowed = %v, want %v (reason %q)", got, tt.allowed, reason)
			}
			if !got && reason == "" {
				t.Error("a refusal should explain itself")
			}
		})
	}
}

func TestArchivableUIDsSplitsBySpamRule(t *testing.T) {
	emails := []Email{
		{UID: 1, Folder: "INBOX", BrokerID: "mylife", Attributed: true},
		{UID: 2, Folder: "INBOX", BrokerID: "unattributed:x.example"},
		{UID: 3, Folder: "[Gmail]/Spam", BrokerID: "freewheel-media", Attributed: true},
		{UID: 4, Folder: "[Gmail]/Spam", BrokerID: "unattributed:y.example"},
	}

	movable, held := ArchivableUIDs(emails, "[Gmail]/Spam")
	if len(movable) != 3 {
		t.Errorf("expected 3 movable, got %d: %+v", len(movable), movable)
	}
	if len(held) != 1 || held[0].UID != 4 {
		t.Errorf("expected only the unattributed spam message held, got %+v", held)
	}

	// With no spam folder configured, the spam rule can't apply.
	movable, held = ArchivableUIDs(emails, "")
	if len(movable) != 4 || len(held) != 0 {
		t.Errorf("with no spam folder everything recognised should move, got %d movable %d held", len(movable), len(held))
	}
}

func TestUnattributedIDHelpers(t *testing.T) {
	id := unattributedID("Tickets.Helpdesk.com")
	if !IsUnattributed(id) {
		t.Fatalf("%q should be recognised as unattributed", id)
	}
	if got := UnattributedDomain(id); got != "tickets.helpdesk.com" {
		t.Errorf("domain = %q", got)
	}
	if got := UnattributedName(id); got != "Unidentified sender (tickets.helpdesk.com)" {
		t.Errorf("name = %q", got)
	}
	if IsUnattributed("mylife") {
		t.Error("a real broker ID must not read as unattributed")
	}

	// Distinct senders must get distinct IDs: several places dedupe responses
	// by broker ID, and every request carries an identical subject, so a
	// single shared sentinel would collapse them into one row.
	if unattributedID("a.example") == unattributedID("b.example") {
		t.Error("different senders must not share an unattributed ID")
	}
}

// Ordering and the uniqueness rule, both driven by what real replies look
// like. Ticketing auto-acknowledgements don't quote the message they answer,
// and a corporate group's ticket can name a sibling brand's privacy address -
// so the body is the last thing consulted, and only when it points at exactly
// one broker.
func TestQuotedAddressAttributionIsLastAndRequiresUniqueness(t *testing.T) {
	t.Run("helpdesk tenant wins over an unrelated address in the body", func(t *testing.T) {
		m := liveMonitor(t)
		// Shaped after the real Experian ticket, whose body carried Tapad's
		// privacy address - a sibling brand, not the broker replying.
		m.SetContactedAddresses(map[string]string{
			"privacy@tapad.com":    "tapad",
			"privacy@experian.com": "experian-marketing",
		})
		got := parseFull(t, m,
			"jira@experian-marketing-services.atlassian.net",
			"PRIV-106538 [EXTERNAL] "+ukAccessSubject,
			"Please contact privacy@tapad.com for details.")

		if got.BrokerID != "experian-marketing" {
			t.Errorf("expected the helpdesk tenant to win, got %q via %s", got.BrokerID, got.MatchedVia)
		}
	})

	t.Run("a body naming two contacted brokers attributes to neither", func(t *testing.T) {
		m := liveMonitor(t)
		got := parseFull(t, m, "someone@unknownhost.example", "Re: "+ukAccessSubject,
			"Forwarded to privacy@mylife.com and privacy@knowwho.com for handling.")

		if got.Attributed {
			t.Errorf("an ambiguous body must not attribute, got %q", got.BrokerID)
		}
		if !IsUnattributed(got.BrokerID) {
			t.Errorf("expected an unattributed match, got %q", got.BrokerID)
		}
	})

	t.Run("a body naming exactly one contacted broker does attribute", func(t *testing.T) {
		m := liveMonitor(t)
		got := parseFull(t, m, "someone@unknownhost.example", "Re: "+ukAccessSubject,
			"Your request to privacy@mylife.com is being processed.")

		if !got.Attributed || got.BrokerID != "mylife" {
			t.Errorf("expected attribution to mylife, got %q (attributed=%v)", got.BrokerID, got.Attributed)
		}
		if got.MatchedVia != "quoted request address" {
			t.Errorf("expected the body rule to be credited, got %q", got.MatchedVia)
		}
	})

	t.Run("an address only present in the HTML part still counts", func(t *testing.T) {
		m := liveMonitor(t)
		e := &Email{
			From: "x@unknownhost.example", FromDomain: "unknownhost.example",
			Subject:  "Re: " + ukAccessSubject,
			HTMLBody: `<a href="mailto:privacy@mylife.com">contact</a>`,
		}
		if match := m.matchReply(e); match.BrokerID != "mylife" {
			t.Errorf("expected mylife from the HTML body, got %q", match.BrokerID)
		}
	})
}
