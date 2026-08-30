// Command import-datenanfragen matches data/brokers.yaml against a local
// checkout of the datenanfragen.de CC0 company database
// (https://github.com/datenanfragen/data) by domain, and proposes/applies a
// small set of conservative updates:
//
//   - a form-only disposition tag, when the record explicitly recommends
//     "webform" as the transport medium, or has no email address but does
//     have a webform (never overrides an existing disposition tag, never
//     applied to an ambiguous match - see companyToBrokers below - and
//     never overrides a broker whose email has already produced a real
//     disclosure/success reply in history.db - see provenWorking below)
//   - opt_out_url filled in from the record's webform, only if empty and
//     the match isn't ambiguous
//   - recovered email addresses are reported only, never auto-applied -
//     filling a broker's contact address from a third-party record without
//     a human eyeballing it first is a real-world-consequence mistake
//     waiting to happen
//
// Run without -apply for a dry-run report - READ IT. domainKey is a crude
// heuristic and cands[0] takes the first of possibly several companies
// sharing a domain with no name disambiguation; the report line shows the
// candidate count so a human can catch a bad pick (see "(Nx)" in the
// report). Two live examples this caught: oracle-america matched the
// generic "oracle" record rather than an Oracle America-specific one, and
// seamless-contacts' own existing notes already document a working email
// fallback that a blind form-only tag would have overridden - both were
// judged not safe to auto-apply and hand-excluded from the brokers.yaml
// edit that shipped from this tool's first run. Re-run periodically as the
// dataset updates upstream, or after adding new brokers to brokers.yaml,
// but always read the report before landing anything from -apply's output
// in a PR.
//
// -apply writes through broker.BrokerDatabase.SaveWithBackup, whose YAML
// round-trip renormalises the *whole* file (quoting, key order) - a no-op
// load-and-save of the shipped database alone produces an ~800-line
// cosmetic diff that buries the real changes (see docs/code-patterns.md,
// "Broker IDs are join keys into history.db"). For a PR, don't commit
// -apply's output directly: take the dry-run report, and hand-apply just
// the intended fields to data/brokers.yaml as surgical edits instead, so
// the diff shows only what changed.
//
// Usage:
//
//	git clone --depth 1 https://github.com/datenanfragen/data /tmp/datenanfragen-data
//	go run ./tools/import-datenanfragen -data /tmp/datenanfragen-data            # dry run
//	go run ./tools/import-datenanfragen -data /tmp/datenanfragen-data -apply     # write
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/eraser-privacy/eraser/internal/broker"
	"github.com/eraser-privacy/eraser/internal/history"
)

type company struct {
	Slug                     string   `json:"slug"`
	Name                     string   `json:"name"`
	Email                    string   `json:"email"`
	Webform                  string   `json:"webform"`
	Web                      string   `json:"web"`
	Sources                  []string `json:"sources"`
	SuggestedTransportMedium string   `json:"suggested-transport-medium"`
	Quality                  string   `json:"quality"`
}

// domainKey extracts a rough registrable-domain identifier for matching -
// good enough to line up "spokeo.com" with a broker record, not intended
// for anything security sensitive (no public-suffix-list lookup).
func domainKey(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return ""
	}
	raw = strings.TrimPrefix(raw, "https://")
	raw = strings.TrimPrefix(raw, "http://")
	raw = strings.TrimPrefix(raw, "mailto:")
	if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.Index(raw, "@"); i >= 0 {
		raw = raw[i+1:]
	}
	raw = strings.TrimPrefix(raw, "www.")
	parts := strings.Split(raw, ".")
	if len(parts) < 2 {
		return raw
	}
	// crude ccSLD handling (co.uk, com.au, org.uk, ...)
	ccSLDs := map[string]bool{"co": true, "com": true, "org": true, "net": true, "gov": true, "ac": true}
	last := parts[len(parts)-1]
	secondLast := parts[len(parts)-2]
	if len(last) == 2 && ccSLDs[secondLast] && len(parts) >= 3 {
		return parts[len(parts)-3]
	}
	return secondLast
}

func loadCompanies(dir string) ([]company, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []company
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var c company
		if err := json.Unmarshal(data, &c); err != nil {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

type proposal struct {
	brokerID       string
	brokerName     string
	companySlug    string
	candidateCount int // how many datenanfragen records shared this domain; cands[0] was picked from these with no name disambiguation
	addFormOnlyTag bool
	setOptOutURL   string
	ambiguous      bool // same company record matched more than one broker, OR candidateCount > 1; report-only, never auto-applied
	recoveredEmail string
	source         string
}

func main() {
	apply := flag.Bool("apply", false, "write changes to brokers.yaml (default: dry-run report only)")
	dataDir := flag.String("data", "", "path to a local checkout of datenanfragen/data (git clone --depth 1 https://github.com/datenanfragen/data)")
	brokersPath := flag.String("brokers", "data/brokers.yaml", "path to brokers.yaml")
	historyPath := flag.String("history", "", "path to history.db (default: $HOME/.eraser/history.db; empty/missing is fine, just skips the proven-working guard)")
	flag.Parse()

	if *dataDir == "" {
		fmt.Fprintln(os.Stderr, "error: -data <path to datenanfragen/data checkout> is required")
		os.Exit(1)
	}

	db, err := broker.LoadFromFile(*brokersPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error loading brokers:", err)
		os.Exit(1)
	}

	companies, err := loadCompanies(filepath.Join(*dataDir, "companies"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error loading datenanfragen companies:", err)
		os.Exit(1)
	}
	fmt.Printf("Loaded %d brokers, %d datenanfragen company records\n", len(db.Brokers), len(companies))

	byDomain := map[string][]company{}
	for _, c := range companies {
		key := domainKey(c.Web)
		if key == "" {
			key = domainKey(c.Email)
		}
		if key == "" {
			continue
		}
		byDomain[key] = append(byDomain[key], c)
	}

	// Never downgrade a broker whose email has already produced a real
	// disclosure/success reply - a static third-party record proposing
	// "form-only" is not stronger evidence than the broker's own working
	// reply in our history.
	hp := *historyPath
	if hp == "" {
		if home, err := os.UserHomeDir(); err == nil {
			hp = filepath.Join(home, ".eraser", "history.db")
		}
	}
	provenWorking := map[string]bool{}
	historyGuardActive := false
	switch {
	case hp == "":
		fmt.Println("⚠️  Could not determine a history.db path (no $HOME) - the proven-working guard is OFF, form-only tags could override a broker with a real working reply on record.")
	default:
		store, err := history.NewStore(hp)
		if err != nil {
			fmt.Printf("⚠️  Could not open history.db at %s: %v - the proven-working guard is OFF.\n", hp, err)
			break
		}
		resps, err := store.GetAllBrokerResponses()
		_ = store.Close()
		if err != nil {
			fmt.Printf("⚠️  Could not read broker responses from %s: %v - the proven-working guard is OFF.\n", hp, err)
			break
		}
		historyGuardActive = true
		for _, r := range resps {
			if r.ResponseType == "success" || r.ResponseType == "disclosure" {
				provenWorking[r.BrokerID] = true
			}
		}
	}
	fmt.Printf("Proven-working guard: %v (%d brokers with a recorded success/disclosure reply)\n", historyGuardActive, len(provenWorking))

	companyToBrokers := map[string][]string{}
	var proposals []proposal
	matchedDomains := 0
	recoveredCount := 0
	formOnlyCount := 0

	for i := range db.Brokers {
		b := &db.Brokers[i]
		// "non-broker" entries (e.g. google-search-removal) are hand-curated
		// special cases, not data brokers with a domain that means anything
		// in this dataset - a generic domain like google.com would otherwise
		// match a real datenanfragen record and produce a nonsense proposal.
		if b.Category == "non-broker" {
			continue
		}
		key := domainKey(b.Website)
		if key == "" {
			key = domainKey(b.Email)
		}
		if key == "" {
			continue
		}
		cands, ok := byDomain[key]
		if !ok {
			continue
		}
		matchedDomains++
		c := cands[0] // first of possibly several; candidateCount below flags that for the human reviewer
		companyToBrokers[c.Slug] = append(companyToBrokers[c.Slug], b.ID)

		p := proposal{brokerID: b.ID, brokerName: b.Name, companySlug: c.Slug, candidateCount: len(cands)}
		if len(cands) > 1 {
			p.ambiguous = true // e.g. oracle-america matching the generic "oracle" record among several oracle.com-domain entries
		}
		if len(c.Sources) > 0 {
			p.source = c.Sources[0]
		}

		isFormOnly := c.SuggestedTransportMedium == "webform" || (c.Email == "" && c.Webform != "")
		if isFormOnly && !p.ambiguous && !b.HasTag(broker.TagFormOnly) && !b.HasTag(broker.TagB2BOnly) && !provenWorking[b.ID] {
			p.addFormOnlyTag = true
			formOnlyCount++
			if strings.TrimSpace(b.OptOutURL) == "" && c.Webform != "" {
				p.setOptOutURL = c.Webform
			}
		} else if !p.ambiguous && strings.TrimSpace(b.OptOutURL) == "" && c.Webform != "" {
			p.setOptOutURL = c.Webform
		}

		if strings.TrimSpace(b.Email) == "" && c.Email != "" {
			p.recoveredEmail = c.Email
			recoveredCount++
		}

		if p.addFormOnlyTag || p.setOptOutURL != "" || p.recoveredEmail != "" {
			proposals = append(proposals, p)
		}
	}

	// A company record that matched more than one broker (e.g. Dun &
	// Bradstreet's AT and DE entities both resolving to dnb.com) means
	// neither the opt_out_url nor a form-only tag is confidently right for
	// just one of them - downgrade both to report-only. This is the same
	// "ambiguous" bucket as candidateCount > 1 above, just discovered after
	// the fact (it takes seeing every broker's match to know a slug was
	// reused).
	for i := range proposals {
		if len(companyToBrokers[proposals[i].companySlug]) > 1 {
			if !proposals[i].ambiguous && proposals[i].addFormOnlyTag {
				formOnlyCount--
			}
			proposals[i].ambiguous = true
			proposals[i].addFormOnlyTag = false
			proposals[i].setOptOutURL = ""
		}
	}
	// Ambiguous proposals with nothing left to report (no recovered email
	// suggestion either) would otherwise vanish silently.
	filtered := proposals[:0]
	for _, p := range proposals {
		if p.addFormOnlyTag || p.setOptOutURL != "" || p.recoveredEmail != "" || p.ambiguous {
			filtered = append(filtered, p)
		}
	}
	proposals = filtered

	sort.Slice(proposals, func(i, j int) bool { return proposals[i].brokerID < proposals[j].brokerID })

	fmt.Printf("Domain matches: %d\n", matchedDomains)
	fmt.Printf("Proposed form-only tags: %d\n", formOnlyCount)
	fmt.Printf("Recovered email addresses (report-only, not applied): %d\n", recoveredCount)
	fmt.Println()
	fmt.Println("=== Proposals ===")
	for _, p := range proposals {
		var actions []string
		if p.addFormOnlyTag {
			actions = append(actions, "tag=form-only")
		}
		if p.setOptOutURL != "" {
			actions = append(actions, "opt_out_url="+p.setOptOutURL)
		}
		if p.recoveredEmail != "" {
			actions = append(actions, "SUGGEST email="+p.recoveredEmail)
		}
		if p.ambiguous {
			actions = append(actions, fmt.Sprintf("AMBIGUOUS (%d datenanfragen candidates and/or matched by >1 broker) - review by hand, nothing auto-applied", p.candidateCount))
		}
		fmt.Printf("%-45s (%s)  %s  [%s]\n", p.brokerID, p.companySlug, strings.Join(actions, ", "), p.source)
	}

	if !*apply {
		fmt.Println()
		fmt.Println("Dry run only - re-run with -apply to write form-only tags and opt_out_url fills to", *brokersPath)
		return
	}

	applied := 0
	for _, p := range proposals {
		for i := range db.Brokers {
			b := &db.Brokers[i]
			if b.ID != p.brokerID {
				continue
			}
			changed := false
			if p.addFormOnlyTag && !b.HasTag(broker.TagFormOnly) {
				b.Tags = append(b.Tags, broker.TagFormOnly)
				changed = true
			}
			if p.setOptOutURL != "" && !p.ambiguous && strings.TrimSpace(b.OptOutURL) == "" {
				b.OptOutURL = p.setOptOutURL
				changed = true
			}
			if changed {
				note := fmt.Sprintf("Updated from datenanfragen.de record %q", p.companySlug)
				if p.source != "" {
					note += " (" + p.source + ")"
				}
				if strings.TrimSpace(b.Notes) == "" {
					b.Notes = note
				} else if !strings.Contains(b.Notes, "datenanfragen.de") {
					b.Notes = b.Notes + " | " + note
				}
				applied++
			}
			break
		}
	}

	// SaveWithBackup renormalises the whole file (quoting, key order) - a
	// no-op load-and-save of the shipped database produces an ~800-line
	// diff that buries these `applied` changes. Prefer running this against
	// a fresh clone and reviewing/hand-applying the reported diff via
	// surgical edits for a PR; -apply is for local/scratch use where the
	// cosmetic churn doesn't matter.
	if err := db.SaveWithBackup(*brokersPath); err != nil {
		fmt.Fprintln(os.Stderr, "error saving:", err)
		os.Exit(1)
	}
	fmt.Printf("Applied %d changes and saved (backup created) to %s\n", applied, *brokersPath)
}
