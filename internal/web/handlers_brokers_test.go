package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eraser-privacy/eraser/internal/broker"
	"github.com/eraser-privacy/eraser/internal/config"
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
	if b.Sendable() {
		t.Error("tagged broker still passes Sendable()")
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

	got := s.getBrokersWithStatus("default", brokerQuery{})
	if len(got) != 1 || got[0].ID != "spokeo" {
		var ids []string
		for _, b := range got {
			ids = append(ids, b.ID)
		}
		t.Fatalf("default view = %v, want only [spokeo]", ids)
	}

	got = s.getBrokersWithStatus("default", brokerQuery{NonSendable: true})
	if len(got) != 4 {
		var ids []string
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
