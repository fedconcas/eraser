package inbox

import (
	"bytes"
	"testing"

	"github.com/emersion/go-imap"
	"github.com/eraser-privacy/eraser/internal/broker"
	"github.com/eraser-privacy/eraser/internal/config"
)

// parseFrom runs one sender address through the matcher and reports which
// broker (if any) it was attributed to.
func parseFrom(t *testing.T, m *Monitor, mailbox, host string) *Email {
	t.Helper()
	section := &imap.BodySectionName{}
	msg := &imap.Message{
		Uid: 7,
		Envelope: &imap.Envelope{
			Subject: "Re: your request",
			From:    []*imap.Address{{MailboxName: mailbox, HostName: host}},
		},
		Body: map[*imap.BodySectionName]imap.Literal{
			section: bytes.NewReader(buildMIMEMessage(t, "body")),
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

// The broker database maps general-purpose sender domains: the real
// `google-search-removal` entry has no contact address and a website of
// google.com, and several brokers list free-mail contact addresses. Matching
// on those turned ordinary correspondence into "broker responses" - 22 Google
// notifications and the user's own test mail were recorded that way against a
// live database - and, with auto-archiving on, would have moved that mail out
// of the inbox.
func TestGeneralPurposeDomainsNeverMatchABroker(t *testing.T) {
	brokers := []broker.Broker{
		// Shaped exactly like the real entry that caused this.
		{ID: "google-search-removal", Name: "Google Search", Email: "", Website: "https://www.google.com"},
		{ID: "vror", Name: "Vror", Email: "vror.privacy@gmail.com", Website: "https://vror.example"},
		{ID: "realbroker", Name: "Real Broker", Email: "privacy@realbroker.example", Website: "https://realbroker.example"},
	}
	m := NewMonitor(config.InboxConfig{}, brokers)

	for _, tc := range []struct{ mailbox, host string }{
		{"googlestore-noreply", "google.com"},
		{"jules-noreply", "google.com"},
		{"someone", "gmail.com"},
		{"a.person", "hotmail.com"},
	} {
		t.Run(tc.host+"/"+tc.mailbox, func(t *testing.T) {
			if got := parseFrom(t, m, tc.mailbox, tc.host); got.BrokerID != "" {
				t.Errorf("%s@%s matched broker %q; general-purpose domains must never identify a broker",
					tc.mailbox, tc.host, got.BrokerID)
			}
		})
	}

	// A broker on its own domain still matches - the filter must not be so
	// blunt that it stops the tool working.
	if got := parseFrom(t, m, "privacy", "realbroker.example"); got.BrokerID != "realbroker" {
		t.Errorf("a broker's own domain should still match, got %q", got.BrokerID)
	}
	// vror's website domain is fine even though its contact address is
	// free-mail: only the gmail.com key is dropped.
	if got := parseFrom(t, m, "hello", "vror.example"); got.BrokerID != "vror" {
		t.Errorf("website domain of a broker with a free-mail contact should still match, got %q", got.BrokerID)
	}
}

// A broker with no contact address can never have been written to, so its
// website domain can only ever produce false matches.
func TestBrokerWithoutEmailIsNotMappedByWebsite(t *testing.T) {
	m := NewMonitor(config.InboxConfig{}, []broker.Broker{
		{ID: "no-contact", Name: "No Contact", Email: "", Website: "https://example.org"},
	})
	if got := parseFrom(t, m, "anyone", "example.org"); got.BrokerID != "" {
		t.Errorf("broker with no contact address should not be matchable by website, got %q", got.BrokerID)
	}
}

// Second half of the filter: even on a broker's own domain, a broker we never
// wrote to cannot be replying to us.
func TestOnlyContactedBrokersMatch(t *testing.T) {
	brokers := []broker.Broker{
		{ID: "written-to", Name: "Written To", Email: "privacy@written.example"},
		{ID: "never-written", Name: "Never Written", Email: "privacy@never.example"},
	}

	m := NewMonitor(config.InboxConfig{}, brokers)
	m.SetContactedBrokers(map[string]bool{"written-to": true})

	if got := parseFrom(t, m, "privacy", "written.example"); got.BrokerID != "written-to" {
		t.Errorf("a broker we emailed should match, got %q", got.BrokerID)
	}
	if got := parseFrom(t, m, "privacy", "never.example"); got.BrokerID != "" {
		t.Errorf("a broker we never emailed must not match, got %q", got.BrokerID)
	}

	// A nil set means "no history available" and leaves matching ungated,
	// which is what the existing tests rely on.
	m.SetContactedBrokers(nil)
	if got := parseFrom(t, m, "privacy", "never.example"); got.BrokerID != "never-written" {
		t.Errorf("a nil contacted set should disable the gate, got %q", got.BrokerID)
	}
}

// GroupUIDsByFolder is what keeps a UID addressed against the mailbox it came
// from. Its absence was a live bug: the scan mixes INBOX, archive-folder and
// spam UIDs into one list, and archiving them against INBOX would move
// whichever unrelated messages happened to carry those numbers.
func TestGroupUIDsByFolder(t *testing.T) {
	emails := []Email{
		{UID: 1, Folder: "INBOX"},
		{UID: 900, Folder: "[Gmail]/Spam"},
		{UID: 2, Folder: "INBOX"},
		{UID: 901, Folder: "[Gmail]/Spam"},
		{UID: 5, Folder: "Eraser"},
	}

	got := GroupUIDsByFolder(emails)
	if len(got) != 3 {
		t.Fatalf("expected 3 folder groups, got %d: %+v", len(got), got)
	}
	if len(got["INBOX"]) != 2 || got["INBOX"][0] != 1 || got["INBOX"][1] != 2 {
		t.Errorf("INBOX group wrong: %+v", got["INBOX"])
	}
	if len(got["[Gmail]/Spam"]) != 2 || got["[Gmail]/Spam"][0] != 900 {
		t.Errorf("spam group wrong: %+v", got["[Gmail]/Spam"])
	}
	if len(got["Eraser"]) != 1 {
		t.Errorf("archive group wrong: %+v", got["Eraser"])
	}
}

// UID 0 encodes as "*" in a go-imap SeqSet, which addresses the highest UID
// in the mailbox - the newest message, never one we classified.
func TestGroupUIDsByFolderDropsUnusableEntries(t *testing.T) {
	got := GroupUIDsByFolder([]Email{
		{UID: 0, Folder: "INBOX"}, // would become "*"
		{UID: 3, Folder: ""},      // no mailbox to resolve against
		{UID: 4, Folder: "INBOX"}, // the only usable one
	})

	if len(got) != 1 {
		t.Fatalf("expected only the usable entry to survive, got %+v", got)
	}
	if len(got["INBOX"]) != 1 || got["INBOX"][0] != 4 {
		t.Errorf("expected INBOX=[4], got %+v", got["INBOX"])
	}
}

func TestUIDValidityByFolder(t *testing.T) {
	got := UIDValidityByFolder([]Email{
		{Folder: "INBOX", UIDValidity: 111},
		{Folder: "INBOX", UIDValidity: 111},
		{Folder: "[Gmail]/Spam", UIDValidity: 222},
	})
	if got["INBOX"] != 111 || got["[Gmail]/Spam"] != 222 {
		t.Errorf("unexpected validity map: %+v", got)
	}

	// Two different values for one folder within a single scan means the
	// server renumbered mid-scan; the folder is marked unknown (0) so
	// ArchiveEmails skips its check rather than trusting a stale UID.
	mixed := UIDValidityByFolder([]Email{
		{Folder: "INBOX", UIDValidity: 111},
		{Folder: "INBOX", UIDValidity: 999},
	})
	if mixed["INBOX"] != 0 {
		t.Errorf("conflicting UIDVALIDITY should resolve to 0 (unknown), got %d", mixed["INBOX"])
	}
}

// These are the rules standing between a routine scan and moving unrelated
// mail, so they're checked directly rather than through an IMAP conversation.
func TestCheckArchiveArgs(t *testing.T) {
	tests := []struct {
		name    string
		uids    []uint32
		src     string
		dest    string
		wantErr bool
	}{
		{"valid", []uint32{1, 2}, "INBOX", "Eraser", false},
		{"missing source folder", []uint32{1}, "", "Eraser", true},
		{"missing destination", []uint32{1}, "INBOX", "", true},
		// UID 0 becomes "*" in a SeqSet - the newest message in the mailbox.
		{"zero UID", []uint32{1, 0, 2}, "INBOX", "Eraser", true},
		{"zero UID alone", []uint32{0}, "INBOX", "Eraser", true},
		{"empty list is fine", nil, "INBOX", "Eraser", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkArchiveArgs(tt.uids, tt.src, tt.dest)
			if tt.wantErr && err == nil {
				t.Error("expected an error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("expected no error, got %v", err)
			}
		})
	}
}

// Archiving a folder into itself must be a no-op. It happens on every scan,
// because the scan reads the archive folder back to pick up replies already
// filed there - and on the copy+expunge path it would destroy them.
func TestArchiveEmailsSameFolderIsNoop(t *testing.T) {
	m := &Monitor{client: nil} // would panic or error if it got as far as IMAP

	for _, dest := range []string{"Eraser", "eraser", "ERASER"} {
		if err := m.ArchiveEmails([]uint32{1, 2}, "Eraser", dest, 0); err != nil {
			t.Errorf("archiving Eraser -> %q should be a no-op, got %v", dest, err)
		}
	}

	// INBOX is case-insensitive per RFC 3501 and normalized by go-imap.
	if err := m.ArchiveEmails([]uint32{1}, "INBOX", "inbox", 0); err != nil {
		t.Errorf("INBOX -> inbox should be a no-op, got %v", err)
	}

	// A genuine move still needs a connection.
	if err := m.ArchiveEmails([]uint32{1}, "INBOX", "Eraser", 0); err == nil {
		t.Error("expected an error when not connected")
	}
}

func TestSameMailbox(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"INBOX", "inbox", true},
		{"Eraser", "Eraser", true},
		{"Eraser", "eraser", true},
		{"INBOX", "Eraser", false},
		{"[Gmail]/Spam", "Eraser", false},
	}
	for _, tt := range tests {
		if got := sameMailbox(tt.a, tt.b); got != tt.want {
			t.Errorf("sameMailbox(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}
