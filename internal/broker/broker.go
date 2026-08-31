package broker

import (
	"fmt"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

func isValidURL(rawURL string) bool {
	if rawURL == "" {
		return true
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	return scheme == "http" || scheme == "https"
}

// Priority values, in descending order of importance. Also the display
// order used by the CLI and the web UI's priority selector - don't derive
// that list from the values present in the database (the way categories and
// regions are derived), since priority has a meaningful rank order.
const (
	PriorityHigh   = "high"
	PriorityMedium = "medium"
	PriorityLow    = "low"
)

var Priorities = []string{PriorityHigh, PriorityMedium, PriorityLow}

// NormalizePriority lowercases/trims a priority value and returns "" for
// anything that isn't one of Priorities. It's used on values coming *in*
// (a --priority flag, a query param, an add-broker prompt) - deliberately
// not on load, since sanitizeBroker's result is what Save writes back to
// data/brokers.yaml, and normalizing there would erase a hand-added tag
// the code doesn't recognize the next time anyone edits the file.
func NormalizePriority(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case PriorityHigh:
		return PriorityHigh
	case PriorityMedium:
		return PriorityMedium
	case PriorityLow:
		return PriorityLow
	default:
		return ""
	}
}

// PriorityRank orders priorities high < medium < low < unclassified, for
// sorting. Filtering doesn't use it - a priority filter is an exact match,
// not a "this and above" threshold.
func PriorityRank(p string) int {
	switch NormalizePriority(p) {
	case PriorityHigh:
		return 0
	case PriorityMedium:
		return 1
	case PriorityLow:
		return 2
	default:
		return 3
	}
}

// Disposition tags. These record what a broker *replied*, so the app can
// stop emailing a party that has already told us email is the wrong channel
// - or that it holds no consumer data at all. They live in Tags rather than
// in Category because the two facts are orthogonal (a broker can be both)
// and because Category means *sector*: overwriting it would break
// SelectCategories, i.e. `send --category marketing` would silently stop
// covering a retagged broker.
//
// A tag is an assertion about the company, not about one user's results.
// "They told us they are B2B-only" belongs here; "they had no record of me"
// does not - that is a per-user, per-moment outcome and lives in history.db.
const (
	// TagB2BOnly marks a company that told us it holds no consumer data at
	// all. Nothing to erase, so nothing to send, ever.
	TagB2BOnly = "b2b-only"
	// TagFormOnly marks a company that does hold consumer data but refuses
	// requests by email and requires its web form. Still a live target -
	// just one you action through OptOutURL, not SMTP.
	TagFormOnly = "form-only"
	// TagUSDataOnly marks a company that told us it only holds data on US
	// customers. For this fork's EU user there is nothing of theirs to
	// erase, so nothing to send - same disposition as b2b-only, different
	// reason, kept distinct so the audit trail says which one it was.
	TagUSDataOnly = "us-data-only"
)

var DispositionTags = []string{TagB2BOnly, TagFormOnly, TagUSDataOnly}

// IsDispositionTag reports whether tag (case-insensitive) is part of the
// closed DispositionTags vocabulary. Callers validating user input must
// reject anything else rather than treating it as "no disposition".
func IsDispositionTag(tag string) bool {
	tag = strings.ToLower(strings.TrimSpace(tag))
	for _, t := range DispositionTags {
		if t == tag {
			return true
		}
	}
	return false
}

// HasTag reports whether b carries tag (case-insensitive).
func (b Broker) HasTag(tag string) bool {
	tag = strings.ToLower(strings.TrimSpace(tag))
	for _, t := range b.Tags {
		if strings.ToLower(strings.TrimSpace(t)) == tag {
			return true
		}
	}
	return false
}

// Sendable reports whether an email request may be sent to this broker.
//
// This is the ONE gate every send path must consult. It used to be an
// open-coded `b.Email != ""` check repeated in four places, which is how
// two of them (handleAPISendOne and the pending-job resume in
// internal/web/handlers_jobs.go) came to enforce less than the other two:
// both look a broker up by ID and so never saw the list-level filtering
// that carried the config exclusions. A broker the user has been told is
// B2B-only must not be emailable through *any* door, including a bulk job
// that was persisted before it was tagged and auto-resumes on next start.
func (b Broker) Sendable() bool {
	if strings.TrimSpace(b.Email) == "" {
		return false
	}
	return !b.HasTag(TagB2BOnly) && !b.HasTag(TagFormOnly) && !b.HasTag(TagUSDataOnly)
}

// NotSendableReason explains a false Sendable() in words fit for a UI or a
// CLI line. Returns "" when the broker is sendable.
func (b Broker) NotSendableReason() string {
	switch {
	case b.HasTag(TagB2BOnly):
		return "B2B only - holds no consumer data"
	case b.HasTag(TagUSDataOnly):
		return "Only holds data on US customers"
	case b.HasTag(TagFormOnly):
		return "Accepts requests only through their web form"
	case strings.TrimSpace(b.Email) == "":
		return "No email on file"
	}
	return ""
}

// AddTag adds tag (canonical lowercase) unless already present. Returns
// whether the tag list changed. It appends into a CLONED slice: Broker
// structs are shared by value between copy-on-write snapshots of the
// broker database (see Server.mutateBrokers in internal/web), and
// appending into a shared backing array could corrupt a snapshot an
// in-flight reader is holding.
func (b *Broker) AddTag(tag string) bool {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if tag == "" || b.HasTag(tag) {
		return false
	}
	b.Tags = append(slices.Clone(b.Tags), tag)
	return true
}

// RemoveTag drops tag (case-insensitive) if present and returns whether the
// tag list changed. Also clones, for the same reason as AddTag.
func (b *Broker) RemoveTag(tag string) bool {
	tag = strings.ToLower(strings.TrimSpace(tag))
	out := make([]string, 0, len(b.Tags))
	removed := false
	for _, t := range b.Tags {
		if strings.ToLower(strings.TrimSpace(t)) == tag {
			removed = true
			continue
		}
		out = append(out, t)
	}
	if !removed {
		return false
	}
	b.Tags = out
	return true
}

// Sendable filters list down to the brokers an email may actually go to.
func Sendable(list []Broker) []Broker {
	out := make([]Broker, 0, len(list))
	for _, b := range list {
		if b.Sendable() {
			out = append(out, b)
		}
	}
	return out
}

func sanitizeBroker(b *Broker) {
	if !isValidURL(b.OptOutURL) {
		b.OptOutURL = ""
	}
	if !isValidURL(b.Website) {
		b.Website = ""
	}
}

type Broker struct {
	ID        string `yaml:"id"`
	Name      string `yaml:"name"`
	Email     string `yaml:"email"`
	Website   string `yaml:"website,omitempty"`
	OptOutURL string `yaml:"opt_out_url,omitempty"`
	// Description is one short sentence on what this company does, shown as
	// a column in the web UI so a user choosing who to write to can tell a
	// people-search site from an ad-tech SSP without leaving the page.
	// Sourced per docs; empty is fine and renders as a dash.
	Description string `yaml:"description,omitempty"`
	Region      string `yaml:"region"`             // "us", "eu", "global"
	Category    string `yaml:"category,omitempty"` // "people-search", "marketing", "background-check", etc.
	// Priority is how much this broker matters to a person trying to get
	// removed: "high", "medium" or "low" (see Priorities). It's a filter,
	// not a schedule - nothing sends automatically based on it. An empty
	// or unrecognized value means "unclassified" and is only matched by a
	// blank (all-priorities) filter. See docs/broker-priority.md for how
	// the shipped values were derived.
	Priority   string   `yaml:"priority,omitempty"`
	Notes      string   `yaml:"notes,omitempty"`
	RequiresID bool     `yaml:"requires_id,omitempty"` // If they require ID verification
	Tags       []string `yaml:"tags,omitempty"`
}

type BrokerDatabase struct {
	Brokers []Broker `yaml:"brokers"`
}

func LoadFromFile(path string) (*BrokerDatabase, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read broker file: %w", err)
	}

	var db BrokerDatabase
	if err := yaml.Unmarshal(data, &db); err != nil {
		return nil, fmt.Errorf("failed to parse broker file: %w", err)
	}

	for i := range db.Brokers {
		sanitizeBroker(&db.Brokers[i])
	}
	return &db, nil
}

func toSet(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, s := range items {
		m[strings.ToLower(s)] = true
	}
	return m
}

// Filter returns brokers matching the given regions (empty means "all"),
// excluding any broker whose ID/name is in excluded or whose category
// (case-insensitive) is in excludedCategories - e.g. "requires-id" to skip
// brokers that demand a government ID document before acting on a request.
func (db *BrokerDatabase) Filter(regions []string, excluded []string, excludedCategories []string) []Broker {
	regionSet, excludedSet, excludedCatSet := toSet(regions), toSet(excluded), toSet(excludedCategories)

	var result []Broker
	for _, b := range db.Brokers {
		if excludedSet[strings.ToLower(b.ID)] || excludedSet[strings.ToLower(b.Name)] {
			continue
		}
		if excludedCatSet[strings.ToLower(b.Category)] {
			continue
		}
		if len(regionSet) > 0 {
			r := strings.ToLower(b.Region)
			if !regionSet[r] && !regionSet["global"] && r != "global" {
				continue
			}
		}
		result = append(result, b)
	}
	return result
}

// SelectCategories narrows list to brokers whose category matches one of
// cats (case-insensitive). An empty cats returns list unchanged.
//
// This is the positive counterpart to Filter's excludedCategories: with 700+
// brokers against a daily send cap, saying "only these" is a far more
// practical way to prioritise than enumerating everything to skip. The
// people-search category in particular is the handful of brokers that
// actually surface someone's address in a search engine, so it's worth
// clearing first.
func SelectCategories(list []Broker, cats []string) []Broker {
	if len(cats) == 0 {
		return list
	}
	want := toSet(cats)
	var out []Broker
	for _, b := range list {
		if want[strings.ToLower(b.Category)] {
			out = append(out, b)
		}
	}
	return out
}

// SelectIDs narrows list to brokers whose ID or name matches one of ids
// (case-insensitive). An empty ids returns list unchanged. Name is accepted
// alongside ID to match Filter's excluded-broker handling, which does the same.
func SelectIDs(list []Broker, ids []string) []Broker {
	if len(ids) == 0 {
		return list
	}
	want := toSet(ids)
	var out []Broker
	for _, b := range list {
		if want[strings.ToLower(b.ID)] || want[strings.ToLower(b.Name)] {
			out = append(out, b)
		}
	}
	return out
}

// Categories returns the distinct categories present, in first-seen order,
// for use in "unknown category" error messages.
func (db *BrokerDatabase) Categories() []string {
	seen := make(map[string]bool)
	var out []string
	for _, b := range db.Brokers {
		c := strings.ToLower(b.Category)
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

// FilterByPriority returns the brokers whose priority is in priorities.
// An empty/nil priorities means "all", so it composes with Filter without
// needing a sentinel. Note this does NOT mirror the "global" escape hatch
// Filter applies to regions: priorities of []string{"high"} really does
// exclude every medium, low and unclassified broker.
func FilterByPriority(brokers []Broker, priorities []string) []Broker {
	if len(priorities) == 0 {
		return brokers
	}
	want := toSet(priorities)

	result := make([]Broker, 0, len(brokers))
	for _, b := range brokers {
		if want[NormalizePriority(b.Priority)] {
			result = append(result, b)
		}
	}
	return result
}

// SortByPriority stably reorders brokers high-then-medium-then-low, leaving
// the database's own order intact within each band. `send` uses this so a
// run that gets truncated by the daily send cap spends its budget on the
// brokers that matter most, rather than on whatever happens to sit at the
// top of the file.
func SortByPriority(brokers []Broker) {
	sort.SliceStable(brokers, func(i, j int) bool {
		return PriorityRank(brokers[i].Priority) < PriorityRank(brokers[j].Priority)
	})
}

func (db *BrokerDatabase) FindByID(id string) *Broker {
	id = strings.ToLower(id)
	for i := range db.Brokers {
		if strings.ToLower(db.Brokers[i].ID) == id {
			return &db.Brokers[i]
		}
	}
	return nil
}

func (db *BrokerDatabase) Save(path string) error {
	data, err := yaml.Marshal(db)
	if err != nil {
		return fmt.Errorf("failed to serialize brokers: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

func (db *BrokerDatabase) Add(broker Broker) error {
	if db.FindByID(broker.ID) != nil {
		return fmt.Errorf("broker with ID %q already exists", broker.ID)
	}
	db.Brokers = append(db.Brokers, broker)
	return nil
}

// FindByEmail finds a broker by their email address
func (db *BrokerDatabase) FindByEmail(email string) *Broker {
	email = strings.ToLower(email)
	for i := range db.Brokers {
		if strings.ToLower(db.Brokers[i].Email) == email {
			return &db.Brokers[i]
		}
	}
	return nil
}

// RemoveByEmail removes a broker by their email address
// Returns the removed broker, or nil if not found
func (db *BrokerDatabase) RemoveByEmail(email string) *Broker {
	email = strings.ToLower(email)
	for i := range db.Brokers {
		if strings.ToLower(db.Brokers[i].Email) == email {
			removed := db.Brokers[i]
			db.Brokers = append(db.Brokers[:i], db.Brokers[i+1:]...)
			return &removed
		}
	}
	return nil
}

// RemoveByID removes a broker by their ID
// Returns the removed broker, or nil if not found
func (db *BrokerDatabase) RemoveByID(id string) *Broker {
	id = strings.ToLower(id)
	for i := range db.Brokers {
		if strings.ToLower(db.Brokers[i].ID) == id {
			removed := db.Brokers[i]
			db.Brokers = append(db.Brokers[:i], db.Brokers[i+1:]...)
			return &removed
		}
	}
	return nil
}

// SaveWithBackup saves the database to file, creating a backup first
func (db *BrokerDatabase) SaveWithBackup(path string) error {
	// Create backup
	if _, err := os.Stat(path); err == nil {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read file for backup: %w", err)
		}
		backupPath := path + ".bak"
		if err := os.WriteFile(backupPath, data, 0644); err != nil {
			return fmt.Errorf("failed to create backup: %w", err)
		}
	}

	return db.Save(path)
}
