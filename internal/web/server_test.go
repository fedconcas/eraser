package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/eraser-privacy/eraser/internal/broker"
	"github.com/eraser-privacy/eraser/internal/config"
	emaTemplate "github.com/eraser-privacy/eraser/internal/template"
)

// newTestServer builds a minimal *Server suitable for unit tests that don't
// need a real broker file, history database, or HTTP listener. It exercises
// the real NewServer constructor (including template parsing) so tests stay
// honest about what construction actually requires.
func newTestServer(t *testing.T, cfg *config.Config) *Server {
	t.Helper()

	tmplEngine, err := emaTemplate.NewEngine()
	if err != nil {
		t.Fatalf("template.NewEngine: %v", err)
	}

	s, err := NewServer(0, cfg, "", "", &broker.BrokerDatabase{}, nil, tmplEngine)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

// newTestServerWithBrokerFile is newTestServer plus a real brokers file on
// disk, so mutateBrokers persists through SaveWithBackup like production.
func newTestServerWithBrokerFile(t *testing.T, cfg *config.Config, brokerPath string) *Server {
	t.Helper()

	db, err := broker.LoadFromFile(brokerPath)
	if err != nil {
		t.Fatalf("broker.LoadFromFile: %v", err)
	}

	tmplEngine, err := emaTemplate.NewEngine()
	if err != nil {
		t.Fatalf("template.NewEngine: %v", err)
	}

	s, err := NewServer(0, cfg, "", brokerPath, db, nil, tmplEngine)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

// TestMutateBrokersConcurrentAccess mirrors TestServerConfigConcurrentAccess
// for the broker database: writers in mutateBrokers and lock-free readers
// via getBrokerDB run at the same time. Under `go test -race` this catches
// both data races and the lost-write shape of bug an atomic pointer alone
// would leave open (two writers copying the same snapshot and one save
// silently dropping the other's change).
func TestMutateBrokersConcurrentAccess(t *testing.T) {
	s := newTestServer(t, testConfig())
	s.setBrokersForTest([]broker.Broker{
		{ID: "one", Name: "One", Email: "p@one.example"},
		{ID: "two", Name: "Two", Email: "p@two.example"},
	})

	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		id := "one"
		if i%2 == 0 {
			id = "two"
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_ = s.mutateBrokers(func(db *broker.BrokerDatabase) (bool, error) {
				b := db.FindByID(id)
				if b == nil {
					return false, nil
				}
				return b.AddTag(broker.TagB2BOnly), nil
			})
		}(id)

		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, b := range s.getBrokerDB().Brokers {
				_ = b.Sendable()
				_ = b.NotSendableReason()
			}
		}()
	}
	wg.Wait()

	db := s.getBrokerDB()
	for _, id := range []string{"one", "two"} {
		b := db.FindByID(id)
		if b == nil {
			t.Fatalf("broker %s vanished from the database", id)
		}
		if !b.HasTag(broker.TagB2BOnly) {
			t.Errorf("broker %s lost its tag under concurrent mutation - a write was dropped: %+v", id, b)
		}
	}
}

func testConfig(profileIDs ...string) *config.Config {
	if len(profileIDs) == 0 {
		return &config.Config{
			Profile: config.Profile{FirstName: "Test", LastName: "User", Email: "test@example.com"},
		}
	}
	cfg := &config.Config{}
	for _, id := range profileIDs {
		cfg.Profiles = append(cfg.Profiles, config.NamedProfile{
			ID: id,
			Profile: config.Profile{
				FirstName: "Test",
				LastName:  id,
				Email:     id + "@example.com",
			},
		})
	}
	return cfg
}

// TestServerConfigConcurrentAccess exercises Server.config (an
// atomic.Pointer[config.Config]) with concurrent readers (getConfig, as
// every handler does) and writers (s.config.Store, as handleSettingsInbox
// and handleSetupComplete do via load-copy-mutate-store). Run with
// `go test -race`: before the atomic.Pointer fix, Server.config was a plain
// *config.Config mutated in place, which the race detector flags as soon as
// a read and a write actually overlap - which is exactly what this test
// forces by running many of each concurrently and without any external
// synchronization between them.
func TestServerConfigConcurrentAccess(t *testing.T) {
	s := newTestServer(t, testConfig())

	const readers = 8
	const writers = 4
	const iterations = 500

	var wg sync.WaitGroup
	var reads int64

	// Readers: exactly what handlers do via s.getConfig().
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				cfg := s.getConfig()
				if cfg == nil {
					t.Errorf("getConfig returned nil")
					return
				}
				// Touch a field to make sure we actually dereference the
				// pointer (and would race on a plain *config.Config if a
				// writer is mutating the same struct in place).
				_ = cfg.Options.Template
				_ = cfg.GetProfiles()
				atomic.AddInt64(&reads, 1)
			}
		}()
	}

	// Writers: load -> copy -> mutate -> store, same pattern as
	// handleSettingsInbox/handleSetupComplete.
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				old := s.getConfig()
				updated := *old // copy
				updated.Options.Template = fmt.Sprintf("writer-%d-iter-%d", w, j)
				s.config.Store(&updated)
			}
		}(i)
	}

	wg.Wait()

	if got := atomic.LoadInt64(&reads); got != readers*iterations {
		t.Fatalf("expected %d successful reads, got %d", readers*iterations, got)
	}

	// Sanity: config is still readable and non-nil after all the churn.
	if cfg := s.getConfig(); cfg == nil {
		t.Fatal("getConfig returned nil after concurrent writes")
	}
}

// TestServerConfigLoadCopyMutateStoreIsolation confirms that storing a
// mutated copy never affects a config pointer obtained by an earlier
// getConfig() call - i.e. writers really do produce a new *config.Config
// rather than mutating the one readers may still be holding.
func TestServerConfigLoadCopyMutateStoreIsolation(t *testing.T) {
	s := newTestServer(t, testConfig())

	before := s.getConfig()
	beforeTemplate := before.Options.Template

	updated := *before
	updated.Options.Template = "changed"
	s.config.Store(&updated)

	if before.Options.Template != beforeTemplate {
		t.Fatalf("earlier config snapshot was mutated in place: got %q, want %q", before.Options.Template, beforeTemplate)
	}

	after := s.getConfig()
	if after.Options.Template != "changed" {
		t.Fatalf("getConfig after Store: got %q, want %q", after.Options.Template, "changed")
	}
}

// TestGetBrokersWithStatusRespectsExclusions is a regression test:
// excluded_brokers/excluded_categories used to only be enforced by the CLI's
// `send` command (via broker.Filter) - the web UI's brokers list and bulk
// "Send to All" both go through getBrokersWithStatus instead, which never
// looked at either option, so a configured exclusion silently had no effect
// there.
func TestGetBrokersWithStatusRespectsExclusions(t *testing.T) {
	cfg := testConfig()
	cfg.Options.ExcludedBrokers = []string{"spokeo"}
	cfg.Options.ExcludedCategories = []string{"requires-id"}
	s := newTestServer(t, cfg)
	s.setBrokersForTest([]broker.Broker{
		{ID: "spokeo", Name: "Spokeo", Email: "privacy@spokeo.com", Region: "us", Category: "people-search"},
		{ID: "altisource-holdings", Name: "Altisource Holdings, LLC", Email: "privacy@altisource.example", Region: "us", Category: "requires-id"},
		{ID: "beenverified", Name: "BeenVerified", Email: "privacy@beenverified.example", Region: "us", Category: "people-search"},
	})

	// Default view: everything excluded is gone.
	got := s.getBrokersWithStatus("default", brokerQuery{})
	if len(got) != 1 || got[0].ID != "beenverified" {
		t.Errorf("default view = %+v, want only beenverified", got)
	}

	// Opt-in view: a broker-level exclusion reappears (marked Excluded, so
	// the row can offer an Include button), but a category exclusion stays
	// hard-hidden - there is no per-category undo control.
	got = s.getBrokersWithStatus("default", brokerQuery{NonSendable: true})
	if len(got) != 2 {
		t.Fatalf("opt-in view = %+v, want spokeo (excluded) and beenverified", got)
	}
	var spokeo *BrokerWithStatus
	for i := range got {
		if got[i].ID == "spokeo" {
			spokeo = &got[i]
		}
		if got[i].ID == "altisource-holdings" {
			t.Error("category-excluded broker leaked into the opt-in view")
		}
	}
	if spokeo == nil {
		t.Fatal("excluded broker missing from the opt-in view")
	}
	if !spokeo.Excluded {
		t.Error("excluded broker not marked Excluded in the opt-in view")
	}
}

func TestGetBrokersWithStatusPriorityFilter(t *testing.T) {
	s := newTestServer(t, testConfig())
	s.setBrokersForTest([]broker.Broker{
		{ID: "spokeo", Name: "Spokeo", Region: "us", Category: "people-search", Priority: "high"},
		{ID: "acxiom", Name: "Acxiom", Region: "us", Category: "marketing", Priority: "high"},
		{ID: "smalltown", Name: "SmallTown Data", Region: "us", Category: "marketing", Priority: "low"},
		{ID: "untagged", Name: "Untagged Broker", Region: "us", Category: "marketing"},
	})

	tests := []struct {
		name  string
		query brokerQuery
		want  []string
	}{
		{"blank priority returns everything", brokerQuery{}, []string{"spokeo", "acxiom", "smalltown", "untagged"}},
		{"high only", brokerQuery{Priority: "high"}, []string{"spokeo", "acxiom"}},
		{"low only", brokerQuery{Priority: "low"}, []string{"smalltown"}},
		{"case-insensitive", brokerQuery{Priority: "HIGH"}, []string{"spokeo", "acxiom"}},
		{"unrecognized priority is treated as no filter, like a bogus category", brokerQuery{Priority: "urgent"}, []string{"spokeo", "acxiom", "smalltown", "untagged"}},
		{"medium matches nothing here", brokerQuery{Priority: "medium"}, nil},
		{"combines with category", brokerQuery{Priority: "high", Category: "marketing"}, []string{"acxiom"}},
		{"combines with a category that has no high brokers", brokerQuery{Priority: "low", Category: "people-search"}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Fixtures carry no email; this test predates and is not about
			// the sendable-only page default.
			tt.query.NonSendable = true
			got := s.getBrokersWithStatus("default", tt.query)

			var ids []string
			for _, b := range got {
				ids = append(ids, b.ID)
			}
			if !reflect.DeepEqual(ids, tt.want) {
				t.Errorf("getBrokersWithStatus(%+v) = %v, want %v", tt.query, ids, tt.want)
			}
		})
	}
}

// TestBrokersPageRendersPriorityFilter exercises the real templates end to
// end. The brokers page's table and partials/broker-list.html are
// hand-duplicated markup (see docs/code-patterns.md), so this is the guard
// that both actually parse and that the priority filter survives the trip
// through the handler - a template typo in either one is otherwise only
// visible by loading the page by hand.
func TestBrokersPageRendersPriorityFilter(t *testing.T) {
	s := newTestServer(t, testConfig())
	s.setBrokersForTest([]broker.Broker{
		{ID: "spokeo", Name: "Spokeo", Email: "privacy@spokeo.com", Region: "us", Category: "people-search", Priority: "high"},
		{ID: "smalltown", Name: "SmallTown Data", Email: "p@smalltown.example", Region: "us", Category: "marketing", Priority: "low"},
	})
	router := s.setupRouter()

	tests := []struct {
		name         string
		url          string
		wantContains []string
		wantMissing  string
	}{
		{"page, unfiltered", "/brokers", []string{"All Priorities", "Spokeo", "SmallTown Data"}, ""},
		{"page, high only", "/brokers?priority=high", []string{"Spokeo"}, "SmallTown Data"},
		{"htmx fragment, high only", "/api/brokers?priority=high", []string{"Spokeo"}, "SmallTown Data"},
		{"htmx fragment, low only", "/api/brokers?priority=low", []string{"SmallTown Data"}, "Spokeo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			req.Host = "127.0.0.1"
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200", tt.url, rec.Code)
			}
			body := rec.Body.String()
			for _, want := range tt.wantContains {
				if !strings.Contains(body, want) {
					t.Errorf("GET %s: response is missing %q", tt.url, want)
				}
			}
			if tt.wantMissing != "" && strings.Contains(body, tt.wantMissing) {
				t.Errorf("GET %s: priority filter let %q through", tt.url, tt.wantMissing)
			}
		})
	}
}

// TestGetBrokersWithStatusTagFilter covers the disposition-tag filter. The
// point of the filter is that a tag is orthogonal to the sector, so the
// combining cases matter more than the plain ones: retagging a broker must
// never be a way of losing it out of its category.
func TestGetBrokersWithStatusTagFilter(t *testing.T) {
	s := newTestServer(t, testConfig())
	s.setBrokersForTest([]broker.Broker{
		{ID: "demandscience", Name: "DemandScience", Region: "us", Category: "marketing", Tags: []string{broker.TagB2BOnly}},
		{ID: "cuebiq", Name: "Cuebiq", Region: "us", Category: "marketing", Tags: []string{broker.TagFormOnly}},
		{ID: "spokeo", Name: "Spokeo", Region: "us", Category: "people-search"},
	})

	tests := []struct {
		name  string
		query brokerQuery
		want  []string
	}{
		{"blank tag returns everything", brokerQuery{}, []string{"demandscience", "cuebiq", "spokeo"}},
		{"b2b-only", brokerQuery{Tag: broker.TagB2BOnly}, []string{"demandscience"}},
		{"form-only", brokerQuery{Tag: broker.TagFormOnly}, []string{"cuebiq"}},
		{"case-insensitive", brokerQuery{Tag: "B2B-Only"}, []string{"demandscience"}},
		{"unknown tag matches nothing", brokerQuery{Tag: "not-a-tag"}, nil},
		{"a tagged broker keeps its category", brokerQuery{Tag: broker.TagB2BOnly, Category: "marketing"}, []string{"demandscience"}},
		{"and is absent from another one", brokerQuery{Tag: broker.TagB2BOnly, Category: "people-search"}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The whole point of this filter is to surface disposition-tagged
			// brokers, which are non-sendable; opt in to seeing them.
			tt.query.NonSendable = true
			var ids []string
			for _, b := range s.getBrokersWithStatus("default", tt.query) {
				ids = append(ids, b.ID)
			}
			if !reflect.DeepEqual(ids, tt.want) {
				t.Errorf("getBrokersWithStatus(%+v) = %v, want %v", tt.query, ids, tt.want)
			}
		})
	}
}

// TestBrokersPageRendersTagFilter is the template-level half: the dropdown
// must offer every DispositionTags value even when the loaded data has none
// of them (it is a closed vocabulary, not a derived one), the badge must
// render, and the filter must survive the trip through both the page handler
// and the HTMX fragment endpoint.
func TestBrokersPageRendersTagFilter(t *testing.T) {
	s := newTestServer(t, testConfig())
	s.setBrokersForTest([]broker.Broker{
		{ID: "demandscience", Name: "DemandScience", Email: "p@demandscience.example", Region: "us", Category: "marketing", Tags: []string{broker.TagB2BOnly}},
		{ID: "spokeo", Name: "Spokeo", Email: "privacy@spokeo.com", Region: "us", Category: "people-search"},
	})
	router := s.setupRouter()

	tests := []struct {
		name         string
		url          string
		wantContains []string
		wantMissing  string
	}{
		{"dropdown offers the whole closed vocabulary", "/brokers", []string{"All Dispositions", broker.TagB2BOnly, broker.TagFormOnly}, ""},
		{"page, b2b-only", "/brokers?tag=b2b-only&non_sendable=true", []string{"DemandScience", "B2B only - holds no consumer data"}, "Spokeo"},
		{"htmx fragment, b2b-only", "/api/brokers?tag=b2b-only&non_sendable=true", []string{"DemandScience"}, "Spokeo"},
		{"htmx fragment, form-only matches nobody here", "/api/brokers?tag=form-only&non_sendable=true", nil, "DemandScience"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			req.Host = "127.0.0.1"
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200", tt.url, rec.Code)
			}
			body := rec.Body.String()
			for _, want := range tt.wantContains {
				if !strings.Contains(body, want) {
					t.Errorf("GET %s: response is missing %q", tt.url, want)
				}
			}
			if tt.wantMissing != "" && strings.Contains(body, tt.wantMissing) {
				t.Errorf("GET %s: tag filter let %q through", tt.url, tt.wantMissing)
			}
		})
	}
}

// TestSendAllExcludesMissingEmailBrokers is a regression test: "Send to All"
// claimed in a comment that it never targeted address-less brokers, but only
// passed MissingEmail:false (which means "don't narrow *to* them"), so every
// web-form-only broker, every address cleared by cleanup-bounces, and the
// whole non-broker category went into the job and handed SMTP an empty To:.
func TestSendAllExcludesMissingEmailBrokers(t *testing.T) {
	s := newTestServer(t, testConfig())
	s.setBrokersForTest([]broker.Broker{
		{ID: "spokeo", Name: "Spokeo", Email: "privacy@spokeo.com", Region: "us", Category: "people-search", Priority: "high"},
		{ID: "whitepages", Name: "Whitepages", Email: "", Region: "us", Category: "people-search", Priority: "high"},
		{ID: "donotcall-registry", Name: "Do Not Call Registry", Email: "", Region: "us", Category: "non-broker", Priority: "high"},
		// Both of these have a perfectly valid address and would have gone
		// into the job before the disposition tags existed.
		{ID: "acme-b2b", Name: "Acme B2B", Email: "privacy@acme-b2b.example", Region: "us", Category: "marketing", Priority: "low", Tags: []string{broker.TagB2BOnly}},
		{ID: "formy", Name: "Formy", Email: "privacy@formy.example", Region: "us", Category: "marketing", Priority: "low", Tags: []string{broker.TagFormOnly}},
	})

	got := sendable(s.getBrokersWithStatus("default", brokerQuery{}))

	if len(got) != 1 || got[0].ID != "spokeo" {
		var ids []string
		for _, b := range got {
			ids = append(ids, b.ID)
		}
		t.Errorf("bulk send target list = %v, want only [spokeo] - address-less, b2b-only and form-only brokers must not be emailed", ids)
	}
}

// TestSendAllSelectionCannotBypassGates is the guard on the explicit-selection
// path. Posting broker_ids is user input: if the handler resolved those IDs
// against s.brokerDB directly it would be a way around the config exclusions,
// the active filters and the sendable gate all at once - which is exactly the
// class of bug that let excluded brokers stay reachable through the
// single-broker endpoint. IDs must only ever narrow the already-filtered list.
func TestSendAllSelectionCannotBypassGates(t *testing.T) {
	cfg := testConfig()
	cfg.Options.ExcludedBrokers = []string{"excluded-one"}
	cfg.Options.ExcludedCategories = []string{"skip-me"}
	s := newTestServer(t, cfg)
	s.setBrokersForTest([]broker.Broker{
		{ID: "ok-one", Name: "OK One", Email: "privacy@ok1.example", Region: "us", Category: "marketing", Priority: "high"},
		{ID: "ok-two", Name: "OK Two", Email: "privacy@ok2.example", Region: "us", Category: "marketing", Priority: "high"},
		{ID: "no-address", Name: "No Address", Email: "", Region: "us", Category: "marketing", Priority: "high"},
		{ID: "b2b", Name: "B2B Co", Email: "privacy@b2b.example", Region: "us", Category: "marketing", Priority: "high", Tags: []string{broker.TagB2BOnly}},
		{ID: "formy", Name: "Formy", Email: "privacy@formy.example", Region: "us", Category: "marketing", Priority: "high", Tags: []string{broker.TagFormOnly}},
		{ID: "excluded-one", Name: "Excluded One", Email: "privacy@ex1.example", Region: "us", Category: "marketing", Priority: "high"},
		{ID: "cat-excluded", Name: "Cat Excluded", Email: "privacy@ex2.example", Region: "us", Category: "skip-me", Priority: "high"},
	})

	candidates := sendable(s.getBrokersWithStatus("default", brokerQuery{}))

	// Simulate the handler's intersection over a selection that asks for
	// everything, including all the brokers that must never be emailed.
	picked := map[string]bool{
		"ok-one": true, "no-address": true, "b2b": true,
		"formy": true, "excluded-one": true, "cat-excluded": true,
	}
	var got []string
	for _, b := range candidates {
		if picked[b.ID] {
			got = append(got, b.ID)
		}
	}

	if len(got) != 1 || got[0] != "ok-one" {
		t.Errorf("selection resolved to %v, want only [ok-one]; a posted ID must not reach an excluded, address-less, b2b-only or form-only broker", got)
	}
}
