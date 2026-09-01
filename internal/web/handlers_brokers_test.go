package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/eraser-privacy/eraser/internal/broker"
	"github.com/eraser-privacy/eraser/internal/config"
	"github.com/eraser-privacy/eraser/internal/history"
	"github.com/eraser-privacy/eraser/internal/inbox"
)

// writeTestBrokerFile seeds a minimal brokers.yaml for the persistence tests
// and returns its path.
func writeTestBrokerFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "brokers.yaml")
	content := `brokers:
  - id: spokeo
    name: Spokeo
    email: privacy@spokeo.com
    region: us
    category: people-search
  - id: demandscience
    name: DemandScience
    email: privacy@demandscience.example
    region: us
    category: marketing
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test brokers file: %v", err)
	}
	return path
}

func postBrokerAction(t *testing.T, s *Server, brokerID, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = withURLParam(req, "brokerID", brokerID)
	w := httptest.NewRecorder()
	switch {
	case strings.HasSuffix(path, "/tag"):
		s.handleAPITagBroker(w, req)
	case strings.HasSuffix(path, "/exclude"):
		s.handleAPIExcludeBroker(w, req)
	default:
		t.Fatalf("unhandled path %s", path)
	}
	return w
}

// TestHandleAPITagBrokerPersists covers the full round trip: adding a tag
// must update the in-memory snapshot AND the brokers file on disk (with a
// backup), removing it must undo both, and an empty-Notes broker must gain
// the audit note the shipped-data test requires.
func TestHandleAPITagBrokerPersists(t *testing.T) {
	brokerPath := writeTestBrokerFile(t)
	s := newTestServerWithBrokerFile(t, testConfig(), brokerPath)

	w := postBrokerAction(t, s, "demandscience", "/api/brokers/demandscience/tag", url.Values{
		"action": {"add"},
		"tag":    {broker.TagUSDataOnly},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("add tag = %d, want 200: %s", w.Code, w.Body.String())
	}

	b := s.getBrokerDB().FindByID("demandscience")
	if b == nil {
		t.Fatal("broker missing from in-memory snapshot after tagging")
	}
	if !b.HasTag(broker.TagUSDataOnly) {
		t.Errorf("in-memory broker not tagged: %+v", b)
	}
	// us-data-only records scope, it does not retire the broker: the row
	// must stay sendable (and stay in the default view).
	if !b.Sendable() {
		t.Error("us-data-only took the broker out of the send list")
	}
	if strings.TrimSpace(b.Notes) == "" {
		t.Error("tagging a broker with empty Notes must fill in the audit note")
	}

	// Persisted to disk, with the backup written alongside.
	db, err := broker.LoadFromFile(brokerPath)
	if err != nil {
		t.Fatalf("reload brokers file: %v", err)
	}
	if reloaded := db.FindByID("demandscience"); reloaded == nil || !reloaded.HasTag(broker.TagUSDataOnly) {
		t.Errorf("brokers file on disk does not carry the tag: %+v", reloaded)
	}
	if _, err := os.Stat(brokerPath + ".bak"); err != nil {
		t.Errorf("expected a backup file next to brokers.yaml: %v", err)
	}

	// Remove the tag again.
	w = postBrokerAction(t, s, "demandscience", "/api/brokers/demandscience/tag", url.Values{
		"action": {"remove"},
		"tag":    {broker.TagUSDataOnly},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("remove tag = %d, want 200: %s", w.Code, w.Body.String())
	}
	if b = s.getBrokerDB().FindByID("demandscience"); b == nil || b.HasTag(broker.TagUSDataOnly) {
		t.Errorf("tag not removed from in-memory snapshot: %+v", b)
	}
	db, err = broker.LoadFromFile(brokerPath)
	if err != nil {
		t.Fatalf("reload brokers file: %v", err)
	}
	if reloaded := db.FindByID("demandscience"); reloaded == nil || reloaded.HasTag(broker.TagUSDataOnly) {
		t.Errorf("tag still present on disk after removal: %+v", reloaded)
	}
}

func TestHandleAPITagBrokerValidation(t *testing.T) {
	brokerPath := writeTestBrokerFile(t)
	s := newTestServerWithBrokerFile(t, testConfig(), brokerPath)

	tests := []struct {
		name string
		form url.Values
		want int
	}{
		{"unknown tag is rejected", url.Values{"action": {"add"}, "tag": {"b2b_only"}}, http.StatusBadRequest},
		{"bad action is rejected", url.Values{"action": {"toggle"}, "tag": {broker.TagB2BOnly}}, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := postBrokerAction(t, s, "spokeo", "/api/brokers/spokeo/tag", tt.form)
			if w.Code != tt.want {
				t.Errorf("got %d, want %d: %s", w.Code, tt.want, w.Body.String())
			}
		})
	}

	if w := postBrokerAction(t, s, "no-such-broker", "/api/brokers/no-such-broker/tag", url.Values{
		"action": {"add"},
		"tag":    {broker.TagB2BOnly},
	}); w.Code != http.StatusNotFound {
		t.Errorf("unknown broker = %d, want 404: %s", w.Code, w.Body.String())
	}

	// A rejected request must not have saved anything.
	db, err := broker.LoadFromFile(brokerPath)
	if err != nil {
		t.Fatalf("reload brokers file: %v", err)
	}
	if b := db.FindByID("spokeo"); b == nil || len(b.Tags) != 0 {
		t.Errorf("rejected request changed the data file: %+v", b)
	}
}

// TestHandleAPIExcludeBrokerRoundTrip covers the user-level exclusion list:
// excluding a broker hides it from getBrokersWithStatus (and therefore the
// page and bulk send), include restores it, and both persist to config.yaml.
func TestHandleAPIExcludeBrokerRoundTrip(t *testing.T) {
	brokerPath := writeTestBrokerFile(t)
	s := newTestServerWithBrokerFile(t, testConfig(), brokerPath)
	s.configPath = filepath.Join(t.TempDir(), "config.yaml")
	if err := config.Save(s.configPath, s.getConfig()); err != nil {
		t.Fatalf("seed config.yaml: %v", err)
	}

	w := postBrokerAction(t, s, "spokeo", "/api/brokers/spokeo/exclude", url.Values{"action": {"exclude"}})
	if w.Code != http.StatusOK {
		t.Fatalf("exclude = %d, want 200: %s", w.Code, w.Body.String())
	}

	q := brokerQuery{}
	for _, b := range s.getBrokersWithStatus("default", q) {
		if b.ID == "spokeo" {
			t.Fatal("excluded broker still appears in getBrokersWithStatus")
		}
	}

	// The opt-in view shows it again (marked Excluded) - that row's Include
	// button is the UI's way back in.
	optIn := s.getBrokersWithStatus("default", brokerQuery{NonSendable: true})
	var excludedRow *BrokerWithStatus
	for i := range optIn {
		if optIn[i].ID == "spokeo" {
			excludedRow = &optIn[i]
		}
	}
	if excludedRow == nil || !excludedRow.Excluded {
		t.Fatalf("excluded broker missing/unmarked in the opt-in view: %+v", excludedRow)
	}

	// Persisted to disk.
	cfg, err := config.Load(s.configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if len(cfg.Options.ExcludedBrokers) != 1 || cfg.Options.ExcludedBrokers[0] != "spokeo" {
		t.Errorf("excluded_brokers on disk = %v, want [spokeo]", cfg.Options.ExcludedBrokers)
	}

	// Include restores it.
	w = postBrokerAction(t, s, "spokeo", "/api/brokers/spokeo/exclude", url.Values{"action": {"include"}})
	if w.Code != http.StatusOK {
		t.Fatalf("include = %d, want 200: %s", w.Code, w.Body.String())
	}
	found := false
	for _, b := range s.getBrokersWithStatus("default", q) {
		if b.ID == "spokeo" {
			found = true
		}
	}
	if !found {
		t.Error("broker still hidden after include")
	}

	cfg, err = config.Load(s.configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if len(cfg.Options.ExcludedBrokers) != 0 {
		t.Errorf("excluded_brokers after include = %v, want empty", cfg.Options.ExcludedBrokers)
	}
}

// TestBrokersPageDefaultsToSendableOnly pins the page behavior the feature
// exists for: brokers Sendable() rejects stay out of the default view but
// come back when the page opts in.
func TestBrokersPageDefaultsToSendableOnly(t *testing.T) {
	s := newTestServer(t, testConfig())
	s.setBrokersForTest([]broker.Broker{
		{ID: "spokeo", Name: "Spokeo", Email: "privacy@spokeo.com", Region: "us", Category: "people-search"},
		{ID: "acme-b2b", Name: "Acme B2B", Email: "privacy@acme.example", Region: "us", Category: "marketing", Tags: []string{broker.TagB2BOnly}, Notes: "told us 2026"},
		{ID: "us-only-co", Name: "US Only Co", Email: "privacy@usonly.example", Region: "us", Category: "marketing", Tags: []string{broker.TagUSDataOnly}, Notes: "told us 2026"},
		{ID: "no-address", Name: "No Address", Email: "", Region: "us", Category: "people-search"},
	})

	// us-only-co is tagged but still emailable, so the default view holds
	// both it and the untagged broker - only the b2b-only company and the
	// one with no address are hidden.
	got := s.getBrokersWithStatus("default", brokerQuery{})
	var ids []string
	for _, b := range got {
		ids = append(ids, b.ID)
	}
	if len(got) != 2 || ids[0] != "spokeo" || ids[1] != "us-only-co" {
		t.Fatalf("default view = %v, want [spokeo us-only-co]", ids)
	}

	got = s.getBrokersWithStatus("default", brokerQuery{NonSendable: true})
	if len(got) != 4 {
		ids = nil
		for _, b := range got {
			ids = append(ids, b.ID)
		}
		t.Errorf("NonSendable view = %v, want all four brokers", ids)
	}

	// The missing-email filter implies opting in: every broker it matches
	// is non-sendable by definition.
	got = s.getBrokersWithStatus("default", brokerQuery{MissingEmail: true})
	if len(got) != 1 || got[0].ID != "no-address" {
		t.Errorf("MissingEmail view = %+v, want [no-address]", got)
	}
}

// TestHandleAPISendOneRefusesExcludedBroker guards the door that resolving
// by ID opens: the opt-in view lists excluded brokers (so the row can offer
// Include), but the single-send endpoint never passes through
// getBrokersWithStatus's list-level exclusion, so it has to enforce
// excluded_brokers itself or an excluded broker stayed emailable.
func TestHandleAPISendOneRefusesExcludedBroker(t *testing.T) {
	cfg := testConfig()
	cfg.Email.Provider = "smtp"
	cfg.Options.ExcludedBrokers = []string{"spokeo"}
	s := newTestServer(t, cfg)
	s.setBrokersForTest([]broker.Broker{
		{ID: "spokeo", Name: "Spokeo", Email: "privacy@spokeo.com", Region: "us", Category: "people-search"},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/send/spokeo", nil)
	req = withURLParam(req, "brokerID", "spokeo")
	w := httptest.NewRecorder()
	s.handleAPISendOne(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("send to excluded broker = %d, want 400: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Excluded") {
		t.Errorf("response should say the broker is excluded, got: %s", w.Body.String())
	}
}

// TestHandleAPIExcludeBrokerConcurrent pins the lost-update fix: four
// exclusions posted at once each used to copy the same config snapshot, so
// whichever saved last won and the other three brokers silently stayed in
// the send list - with config.yaml and the in-memory config disagreeing
// about which. Every exclusion must survive, in memory and on disk.
func TestHandleAPIExcludeBrokerConcurrent(t *testing.T) {
	brokerPath := writeTestBrokerFile(t)
	s := newTestServerWithBrokerFile(t, testConfig(), brokerPath)
	s.configPath = filepath.Join(t.TempDir(), "config.yaml")
	if err := config.Save(s.configPath, s.getConfig()); err != nil {
		t.Fatalf("seed config.yaml: %v", err)
	}

	ids := []string{"spokeo", "demandscience", "acxiom", "epsilon"}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			w := postBrokerAction(t, s, id, "/api/brokers/"+id+"/exclude", url.Values{"action": {"exclude"}})
			if w.Code != http.StatusOK {
				t.Errorf("exclude %s = %d: %s", id, w.Code, w.Body.String())
			}
		}(id)
	}
	wg.Wait()

	inMemory := broker.ToSet(s.getConfig().Options.ExcludedBrokers)
	cfg, err := config.Load(s.configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	onDisk := broker.ToSet(cfg.Options.ExcludedBrokers)
	for _, id := range ids {
		if !inMemory[id] {
			t.Errorf("%s missing from the in-memory exclusion list %v", id, s.getConfig().Options.ExcludedBrokers)
		}
		if !onDisk[id] {
			t.Errorf("%s missing from excluded_brokers on disk %v", id, cfg.Options.ExcludedBrokers)
		}
	}
}

// TestMutateBrokersKeepsOutOfProcessEdits covers the other half of the same
// hazard on the broker file: `eraser tag-broker` writes brokers.yaml while
// `eraser serve` is running. The server used to publish its startup snapshot
// over the top of that edit on the next web action, quietly un-retiring a
// broker the user had classified.
func TestMutateBrokersKeepsOutOfProcessEdits(t *testing.T) {
	brokerPath := writeTestBrokerFile(t)
	s := newTestServerWithBrokerFile(t, testConfig(), brokerPath)

	// Out-of-process edit: tag spokeo the way the CLI would.
	db, err := broker.LoadFromFile(brokerPath)
	if err != nil {
		t.Fatalf("load brokers: %v", err)
	}
	if _, err := broker.ApplyDisposition(db.FindByID("spokeo"), broker.TagB2BOnly, false, "their reply said consumer data is not held", "CLI"); err != nil {
		t.Fatalf("ApplyDisposition: %v", err)
	}
	if err := db.Save(brokerPath); err != nil {
		t.Fatalf("save brokers: %v", err)
	}

	// An unrelated web action on a different broker must not roll it back.
	w := postBrokerAction(t, s, "demandscience", "/api/brokers/demandscience/tag", url.Values{
		"action": {"add"},
		"tag":    {broker.TagUSDataOnly},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("tag = %d, want 200: %s", w.Code, w.Body.String())
	}

	reloaded, err := broker.LoadFromFile(brokerPath)
	if err != nil {
		t.Fatalf("reload brokers: %v", err)
	}
	if b := reloaded.FindByID("spokeo"); b == nil || !b.HasTag(broker.TagB2BOnly) {
		t.Errorf("the CLI's tag was clobbered by the web write: %+v", b)
	}
	if b := s.getBrokerDB().FindByID("spokeo"); b == nil || !b.HasTag(broker.TagB2BOnly) {
		t.Errorf("in-memory snapshot did not pick up the out-of-process tag: %+v", b)
	}
	if b := reloaded.FindByID("demandscience"); b == nil || !b.HasTag(broker.TagUSDataOnly) {
		t.Errorf("the web write itself did not persist: %+v", b)
	}
}

// TestApplyBounceFindingsClearsDeadAddresses covers the self-healing list:
// a hard bounce seen during an inbox scan clears that broker's address (and
// persists it), a soft bounce leaves it alone, and the broker itself is
// never deleted.
func TestApplyBounceFindingsClearsDeadAddresses(t *testing.T) {
	brokerPath := writeTestBrokerFile(t)
	s := newTestServerWithBrokerFile(t, testConfig(), brokerPath)

	// Both are the shape a real NDR arrives in: from the local mail system,
	// not from the broker. Only the first says the address is permanently
	// dead.
	hard := &inbox.Email{
		From:    "mailer-daemon@googlemail.com",
		Subject: "Undeliverable: Erasure request",
		Body:    "Delivery to the following recipient failed permanently:\n550 5.1.1 <privacy@spokeo.com>: Recipient address rejected: User unknown",
	}
	soft := &inbox.Email{
		From:    "mailer-daemon@googlemail.com",
		Subject: "Undeliverable: Erasure request",
		Body:    "Delivery to privacy@demandscience.example has failed: 452 4.2.2 The recipient's mailbox is full, the server will retry.",
	}

	var findings []bounceFinding
	for _, e := range []*inbox.Email{hard, soft} {
		classified := inbox.ClassifyResponse(e)
		if classified.Type != inbox.ResponseBounced {
			t.Fatalf("test fixture %q did not classify as a bounce: %s", e.Subject, classified.Type)
		}
		if f, ok := collectBounce(classified); ok {
			findings = append(findings, f)
		}
	}

	if len(findings) != 1 || findings[0].Address != "privacy@spokeo.com" {
		t.Fatalf("collectBounce findings = %+v, want only the hard bounce for privacy@spokeo.com", findings)
	}

	cleared := s.applyBounceFindings(findings)
	if len(cleared) != 1 || cleared[0] != "Spokeo" {
		t.Fatalf("cleared = %v, want [Spokeo]", cleared)
	}

	b := s.getBrokerDB().FindByID("spokeo")
	if b == nil {
		t.Fatal("the broker was removed instead of having its address cleared")
	}
	if b.Email != "" {
		t.Errorf("in-memory address = %q, want cleared", b.Email)
	}
	if b.Sendable() {
		t.Error("a broker with no address must not be sendable")
	}

	// The soft-bounced broker keeps its address.
	if other := s.getBrokerDB().FindByID("demandscience"); other == nil || other.Email == "" {
		t.Errorf("a soft bounce cost a live broker its address: %+v", other)
	}

	// Persisted, so the next run of the app starts from the healed list.
	db, err := broker.LoadFromFile(brokerPath)
	if err != nil {
		t.Fatalf("reload brokers file: %v", err)
	}
	reloaded := db.FindByID("spokeo")
	if reloaded == nil || reloaded.Email != "" {
		t.Errorf("brokers.yaml on disk still carries the dead address: %+v", reloaded)
	}
	if !strings.Contains(reloaded.Notes, "privacy@spokeo.com") {
		t.Errorf("Notes = %q, want the dropped address recorded for the audit trail", reloaded.Notes)
	}

	// Re-applying the same finding is a no-op, not a second write.
	if again := s.applyBounceFindings(findings); len(again) != 0 {
		t.Errorf("re-applying a handled bounce reported %v, want nothing", again)
	}
}

// TestResolveBounceRecipientsAttributesByAddressNotSender covers the gap a
// live account exposed: a delivery-failure notice generated by Gmail's own
// mail system (mailer-daemon@googlemail.com) can never be attributed by
// sender-domain matching, because the broker never sent it. It must instead
// be resolved by the address named inside the notice body.
func TestResolveBounceRecipientsAttributesByAddressNotSender(t *testing.T) {
	brokerPath := writeTestBrokerFile(t)
	s := newTestServerWithBrokerFile(t, testConfig(), brokerPath)

	candidates := []inbox.Email{
		{
			UID: 101, Folder: "INBOX",
			From:    "Mail Delivery Subsystem <mailer-daemon@googlemail.com>",
			Subject: "Delivery incomplete",
			Body:    "There was a temporary problem while delivering your message to privacy@spokeo.com. Gmail will retry.",
		},
		{
			// Already resolved by the ordinary sender-domain match (e.g. the
			// broker's own mail server rejected it) - must not be attributed
			// a second time.
			UID: 102, Folder: "INBOX",
			From:    "mailer-daemon@demandscience.example",
			Subject: "Undeliverable",
			Body:    "Delivery to privacy@demandscience.example failed.",
		},
		{
			// Names an address no broker in the database carries.
			UID: 103, Folder: "INBOX",
			From:    "mailer-daemon@googlemail.com",
			Subject: "Delivery incomplete",
			Body:    "There was a problem delivering your message to someone@unrelated.example.",
		},
	}
	already := []inbox.Email{{UID: 102, Folder: "INBOX", BrokerID: "demandscience", Attributed: true}}

	got := s.resolveBounceRecipients(candidates, already)

	if len(got) != 1 {
		t.Fatalf("resolveBounceRecipients returned %d email(s), want 1: %+v", len(got), got)
	}
	if got[0].UID != 101 || got[0].BrokerID != "spokeo" || !got[0].Attributed {
		t.Errorf("got %+v, want UID 101 attributed to spokeo", got[0])
	}
	if got[0].MatchedVia == "" {
		t.Error("MatchedVia should record how this was resolved")
	}
}

// TestResolveBounceRecipientsRespectsContactedGate mirrors the gate every
// other matching rule respects: an address that resolves to a real broker in
// the database, but one this install never actually wrote to, must not be
// attributed.
func TestResolveBounceRecipientsRespectsContactedGate(t *testing.T) {
	brokerPath := writeTestBrokerFile(t)
	s := newTestServerWithBrokerFile(t, testConfig(), brokerPath)

	dbPath := filepath.Join(t.TempDir(), "history.db")
	store, err := history.NewStore(dbPath)
	if err != nil {
		t.Fatalf("history.NewStore: %v", err)
	}
	defer store.Close()
	if err := store.Add(&history.Record{
		ProfileID: history.DefaultProfileID, BrokerID: "spokeo", BrokerName: "Spokeo",
		Email: "privacy@spokeo.com", Template: "gdpr", Status: history.StatusSent,
	}); err != nil {
		t.Fatalf("seed removal request: %v", err)
	}
	s.historyStore = store

	candidates := []inbox.Email{
		{
			UID: 201, Folder: "INBOX", From: "mailer-daemon@googlemail.com",
			Subject: "Delivery incomplete",
			Body:    "problem delivering your message to privacy@spokeo.com.",
		},
		{
			// A real broker address in the database, but never contacted.
			UID: 202, Folder: "INBOX", From: "mailer-daemon@googlemail.com",
			Subject: "Delivery incomplete",
			Body:    "problem delivering your message to privacy@demandscience.example.",
		},
	}

	got := s.resolveBounceRecipients(candidates, nil)
	if len(got) != 1 || got[0].BrokerID != "spokeo" {
		t.Fatalf("resolveBounceRecipients = %+v, want only the contacted broker (spokeo)", got)
	}
}
