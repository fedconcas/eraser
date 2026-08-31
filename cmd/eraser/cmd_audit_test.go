package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eraser-privacy/eraser/internal/broker"
	"github.com/eraser-privacy/eraser/internal/history"
	"github.com/eraser-privacy/eraser/internal/inbox"
)

// stubAuditChecker returns canned answers for both checks. A non-nil error
// means "inconclusive", matching the production checker's contract: only a
// nil error makes the bool a definitive answer.
type stubAuditChecker struct {
	websiteOK  bool
	websiteErr error
	mxOK       bool
	mxErr      error
}

func (s stubAuditChecker) WebsiteAlive(ctx context.Context, url string) (bool, error) {
	return s.websiteOK, s.websiteErr
}

func (s stubAuditChecker) MXExists(domain string) (bool, error) {
	return s.mxOK, s.mxErr
}

var errInconclusive = errors.New("check timed out")

func TestAuditOneVerdicts(t *testing.T) {
	b := broker.Broker{ID: "acme", Name: "Acme", Website: "https://acme.example", Email: "privacy@acme.example"}

	cases := []struct {
		name string
		chk  stubAuditChecker
		want auditVerdict
	}{
		{"both alive", stubAuditChecker{websiteOK: true, mxOK: true}, verdictAlive},
		{"both dead", stubAuditChecker{}, verdictDefunct},
		{"website dead, mail alive", stubAuditChecker{mxOK: true}, verdictWebsiteDead},
		{"website alive, mail dead", stubAuditChecker{websiteOK: true}, verdictEmailDead},
		{"both inconclusive", stubAuditChecker{websiteErr: errInconclusive, mxErr: errInconclusive}, verdictUnknown},
		{"website inconclusive, mail dead", stubAuditChecker{websiteErr: errInconclusive}, verdictEmailDead},
		{"website inconclusive, mail alive", stubAuditChecker{websiteErr: errInconclusive, mxOK: true}, verdictAlive},
		{"website dead, mail inconclusive", stubAuditChecker{mxErr: errInconclusive}, verdictUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := auditOne(context.Background(), b, tc.chk)
			if res.Verdict != tc.want {
				t.Errorf("verdict = %q, want %q (detail: %s)", res.Verdict, tc.want, res.Detail)
			}
		})
	}
}

// A broker with neither a website nor an email cannot be checked - calling it
// unknown would flood the rerun list with brokers that simply have no data.
func TestAuditOneSkippedWithoutAnythingToCheck(t *testing.T) {
	res := auditOne(context.Background(), broker.Broker{ID: "ghost", Name: "Ghost"}, stubAuditChecker{})
	if res.Verdict != verdictSkipped {
		t.Errorf("verdict = %q, want %q", res.Verdict, verdictSkipped)
	}
}

// A broker that has an email but no website can still reach a definitive
// verdict from the MX check alone.
func TestAuditOneEmailOnly(t *testing.T) {
	b := broker.Broker{ID: "mail-only", Name: "Mail Only", Email: "privacy@mail.example"}

	res := auditOne(context.Background(), b, stubAuditChecker{mxOK: true})
	if res.Verdict != verdictAlive {
		t.Errorf("verdict = %q, want %q", res.Verdict, verdictAlive)
	}
	res = auditOne(context.Background(), b, stubAuditChecker{mxOK: false})
	if res.Verdict != verdictEmailDead {
		t.Errorf("verdict = %q, want %q", res.Verdict, verdictEmailDead)
	}
}

func TestAuditBrokersPreservesInputOrder(t *testing.T) {
	brokers := []broker.Broker{
		{ID: "a", Name: "A", Website: "https://a.example"},
		{ID: "b", Name: "B", Email: "x@b.example"},
		{ID: "c", Name: "C"},
		{ID: "d", Name: "D", Website: "https://d.example", Email: "x@d.example"},
	}
	results := auditBrokers(context.Background(), brokers, stubAuditChecker{}, 3)
	if len(results) != len(brokers) {
		t.Fatalf("got %d results, want %d", len(results), len(brokers))
	}
	for i, r := range results {
		if r.Broker.ID != brokers[i].ID {
			t.Errorf("result[%d] is broker %q, want %q", i, r.Broker.ID, brokers[i].ID)
		}
	}
}

func TestEmailDomain(t *testing.T) {
	cases := map[string]string{
		"privacy@Spokeo.COM": "spokeo.com",
		"no-at-sign":         "",
		"trailing@":          "",
		"  x@y.io  ":         "y.io",
		"":                   "",
	}
	for in, want := range cases {
		if got := emailDomain(in); got != want {
			t.Errorf("emailDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteAuditReportListsOnlyNonAlive(t *testing.T) {
	dead := AuditResult{
		Broker:  broker.Broker{ID: "gone", Name: "Gone Co", Website: "https://gone.example", Email: "x@gone.example", Tags: []string{"form-only"}},
		Verdict: verdictDefunct,
		Detail:  "website dead; email domain has no MX",
	}
	alive := AuditResult{
		Broker:  broker.Broker{ID: "live", Name: "Live Co"},
		Verdict: verdictAlive,
	}
	byVerdict := map[auditVerdict][]AuditResult{
		verdictDefunct: {dead},
		verdictAlive:   {alive},
	}

	path := filepath.Join(t.TempDir(), "report.md")
	if err := writeAuditReport(path, []AuditResult{dead, alive}, byVerdict); err != nil {
		t.Fatalf("writeAuditReport: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, "Defunct candidates") {
		t.Error("report is missing the Defunct candidates section")
	}
	if !strings.Contains(s, "- [ ] **Gone Co** (`gone`)") {
		t.Error("report is missing a checklist item for the defunct broker")
	}
	if !strings.Contains(s, "tags: form-only") {
		t.Error("report is missing the broker's current tags")
	}
	if strings.Contains(s, "Live Co") {
		t.Error("alive broker appears in the review checklist - only non-alive verdicts belong there")
	}
}

// exportBrokerReplies reads from the history database next to the config
// file, so the test points cfgFile at a temp dir and seeds that database.
func seedHistoryStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	prevCfg := cfgFile
	cfgFile = filepath.Join(dir, "config.yaml") // does not need to exist - only its directory matters
	t.Cleanup(func() { cfgFile = prevCfg })

	store, err := history.NewStore(history.DBPathFor(cfgFile))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	received := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	err = store.AddBrokerResponse(&history.BrokerResponse{
		ProfileID:    "default",
		BrokerID:     "spokeo",
		BrokerName:   "Spokeo",
		ResponseType: string(inbox.ResponseB2BOnly),
		EmailFrom:    "privacy@spokeo.com",
		EmailSubject: "We only serve businesses",
		EmailBody:    "We are a B2B provider and hold no consumer data.",
		ReceivedAt:   received,
	})
	if err != nil {
		t.Fatalf("AddBrokerResponse: %v", err)
	}
	// A reply recorded by `eraser monitor` has no body - the export must say
	// so rather than rendering an empty block.
	err = store.AddBrokerResponse(&history.BrokerResponse{
		ProfileID:    "default",
		BrokerID:     "usdata",
		BrokerName:   "US Data Co",
		ResponseType: "success",
		EmailFrom:    "dpo@usdata.example",
		EmailSubject: "Deletion complete",
		ReceivedAt:   received.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("AddBrokerResponse: %v", err)
	}
	return dir
}

func testExportBrokerDB() *broker.BrokerDatabase {
	return &broker.BrokerDatabase{Brokers: []broker.Broker{
		{ID: "spokeo", Name: "Spokeo", Tags: []string{broker.TagB2BOnly}},
		{ID: "usdata", Name: "US Data Co"},
	}}
}

func TestExportBrokerRepliesMarkdown(t *testing.T) {
	seedHistoryStore(t)
	path := filepath.Join(t.TempDir(), "replies.md")

	if err := exportBrokerReplies(path, testExportBrokerDB()); err != nil {
		t.Fatalf("exportBrokerReplies: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)

	for _, want := range []string{
		"## Spokeo (`spokeo`)",
		"Classification: b2b_only",
		"Current tags: b2b-only",
		"Subject: We only serve businesses",
		"> We are a B2B provider and hold no consumer data.",
		"## US Data Co (`usdata`)",
		"(none stored",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("markdown export missing %q", want)
		}
	}
	// Sorted by broker ID: spokeo before usdata.
	if strings.Index(s, "Spokeo") > strings.Index(s, "US Data Co") {
		t.Error("entries are not sorted by broker ID")
	}
}

func TestExportBrokerRepliesJSONL(t *testing.T) {
	seedHistoryStore(t)
	path := filepath.Join(t.TempDir(), "replies.jsonl")

	if err := exportBrokerReplies(path, testExportBrokerDB()); err != nil {
		t.Fatalf("exportBrokerReplies: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d JSONL lines, want 2", len(lines))
	}
	byBroker := map[string]map[string]interface{}{}
	for _, line := range lines {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("invalid JSONL line %q: %v", line, err)
		}
		byBroker[m["broker_id"].(string)] = m
	}

	spokeo := byBroker["spokeo"]
	if spokeo == nil {
		t.Fatal("spokeo entry missing")
	}
	if spokeo["body_missing"].(bool) {
		t.Error("spokeo entry marked body_missing but a body was stored")
	}
	if !strings.Contains(spokeo["body"].(string), "B2B provider") {
		t.Errorf("spokeo body = %q, want the stored reply", spokeo["body"])
	}
	if spokeo["classification"] != "b2b_only" {
		t.Errorf("spokeo classification = %v", spokeo["classification"])
	}

	usdata := byBroker["usdata"]
	if usdata == nil {
		t.Fatal("usdata entry missing")
	}
	if !usdata["body_missing"].(bool) {
		t.Error("usdata entry should be marked body_missing")
	}
}
