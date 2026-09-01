package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eraser-privacy/eraser/internal/broker"
)

const tagTestBrokersYAML = `brokers:
  - id: spokeo
    name: Spokeo
    email: privacy@spokeo.com
    region: us
    category: people-search
`

func TestRunTagBroker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "brokers.yaml")
	if err := os.WriteFile(path, []byte(tagTestBrokersYAML), 0644); err != nil {
		t.Fatal(err)
	}

	prevBrokerFile := brokerFile
	brokerFile = path
	t.Cleanup(func() { brokerFile = prevBrokerFile })

	// An unknown tag or broker ID is a hard error, not a silent no-op: a
	// typo like "b2b_only" would otherwise read as success while leaving the
	// broker in the send list.
	if err := runTagBroker("spokeo", "made-up-tag", "", false); err == nil {
		t.Error("unknown tag accepted, want an error listing the valid tags")
	}
	if err := runTagBroker("ghost", broker.TagB2BOnly, "", false); err == nil {
		t.Error("unknown broker ID accepted, want an error")
	}

	if err := runTagBroker("spokeo", broker.TagUSDataOnly, "", false); err != nil {
		t.Fatalf("tag add failed: %v", err)
	}
	db, err := broker.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b := db.FindByID("spokeo")
	if b == nil || !b.HasTag(broker.TagUSDataOnly) {
		t.Fatal("us-data-only tag was not persisted")
	}
	if strings.TrimSpace(b.Notes) == "" {
		t.Error("tag added without --note should auto-fill Notes so the decision stays auditable")
	}
	// us-data-only classifies without blocking: a US user must still be able
	// to write to a company that only holds US data.
	if !b.Sendable() {
		t.Error("us-data-only must not take the broker out of the send list")
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Errorf("backup file not written: %v", err)
	}

	// Re-adding a tag it already carries is a no-op that must NOT rewrite
	// the file - Save renormalizes the whole document, so a no-op write
	// would churn brokers.yaml for nothing.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := runTagBroker("spokeo", broker.TagUSDataOnly, "", false); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("no-op tag add rewrote brokers.yaml")
	}

	if err := runTagBroker("spokeo", broker.TagUSDataOnly, "", true); err != nil {
		t.Fatalf("tag remove failed: %v", err)
	}
	db, err = broker.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b = db.FindByID("spokeo")
	if b.HasTag(broker.TagUSDataOnly) {
		t.Error("tag still present after --remove")
	}
	if !b.Sendable() {
		t.Error("broker should be sendable again after --remove")
	}

	// An explicit --note is recorded verbatim, replacing the auto-fill.
	note := "their reply said they only serve businesses"
	if err := runTagBroker("spokeo", broker.TagB2BOnly, note, false); err != nil {
		t.Fatal(err)
	}
	db, err = broker.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := db.FindByID("spokeo").Notes; got != note {
		t.Errorf("Notes = %q, want the verbatim --note value", got)
	}
}
