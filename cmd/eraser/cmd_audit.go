package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eraser-privacy/eraser/internal/broker"
	"github.com/eraser-privacy/eraser/internal/history"
	"github.com/eraser-privacy/eraser/internal/inbox"
	"github.com/spf13/cobra"
)

func auditBrokersCmd() *cobra.Command {
	var workers int
	var timeout time.Duration
	var reportFile string
	var historyCheck bool
	var repliesFile string

	cmd := &cobra.Command{
		Use:   "audit-brokers",
		Short: "Find defunct brokers and review stored broker replies",
		Long: `Check every broker's website and email domain for signs it no longer
exists, and surface the evidence needed to classify the rest.

Verdicts:
  alive         website responds and the email domain accepts mail
  website-dead  website gone (connection refused / DNS NXDOMAIN)
  email-dead    no MX records (and no implicit-MX A/AAAA) for the email domain
  defunct       both website and email dead - strong retirement candidate
  unknown       a check timed out or was inconclusive - NEVER treated as dead,
                a flaky network must not manufacture defunct verdicts
  skipped       nothing to check (no website and no email on file)

The command is read-only: it never modifies brokers.yaml. Apply verdicts by
hand (or with 'eraser tag-broker') after reviewing the report.

--history lists brokers whose stored replies were classified b2b_only but
that do not carry the b2b-only tag yet, with the exact command to fix each.

--replies FILE exports every stored broker reply for offline review by a
cheap LLM - it reads bodies recorded by web inbox scans, flags statements
like "we only serve businesses" or "we only hold data on US customers",
and you apply the verdicts with 'eraser tag-broker'. Format is Markdown by
default, JSONL when FILE ends in .jsonl.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAuditBrokers(workers, timeout, reportFile, historyCheck, repliesFile)
		},
	}

	cmd.Flags().IntVar(&workers, "workers", 8, "Concurrent checks")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Second, "Per-check timeout")
	cmd.Flags().StringVar(&reportFile, "report", "", "Write a Markdown review checklist to this file")
	cmd.Flags().BoolVar(&historyCheck, "history", false, "List b2b_only-classified replies whose broker lacks the b2b-only tag")
	cmd.Flags().StringVar(&repliesFile, "replies", "", "Export stored broker replies for offline review (Markdown, or JSONL if the file ends in .jsonl)")

	return cmd
}

// auditVerdict is the outcome of liveness-checking one broker.
type auditVerdict string

const (
	verdictAlive       auditVerdict = "alive"
	verdictWebsiteDead auditVerdict = "website-dead"
	verdictEmailDead   auditVerdict = "email-dead"
	verdictDefunct     auditVerdict = "defunct"
	verdictUnknown     auditVerdict = "unknown"
	verdictSkipped     auditVerdict = "skipped"
)

// AuditResult pairs one broker with its verdict and the evidence behind it.
type AuditResult struct {
	Broker       broker.Broker
	Verdict      auditVerdict
	WebsiteAlive *bool // nil = not checked or inconclusive
	MXAlive      *bool
	Detail       string
}

// auditChecker is the injectable seam for tests: the production
// implementation hits the network, tests substitute a stub. Semantics: a nil
// error means the bool is a definitive answer; a non-nil error means "could
// not determine" and must never be read as dead.
type auditChecker interface {
	WebsiteAlive(ctx context.Context, url string) (bool, error)
	MXExists(domain string) (bool, error)
}

// auditBrokers fans the broker list out over a worker pool and returns one
// result per broker, in input order.
func auditBrokers(ctx context.Context, brokers []broker.Broker, chk auditChecker, workers int) []AuditResult {
	if workers < 1 {
		workers = 1
	}
	type job struct {
		idx int
		b   broker.Broker
	}
	jobs := make(chan job)
	results := make([]AuditResult, len(brokers))

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				results[j.idx] = auditOne(ctx, j.b, chk)
			}
		}()
	}
	for i, b := range brokers {
		jobs <- job{idx: i, b: b}
	}
	close(jobs)
	wg.Wait()
	return results
}

// auditOne runs whichever checks apply to one broker and folds the outcomes
// into a verdict. The folding rule is deliberately conservative: any
// inconclusive check forces "unknown", because the cost of a false
// "defunct" (retiring a live company) is a user decision built on top of
// this output.
func auditOne(ctx context.Context, b broker.Broker, chk auditChecker) AuditResult {
	website := strings.TrimSpace(b.Website)
	domain := emailDomain(b.Email)
	if website == "" && domain == "" {
		return AuditResult{Broker: b, Verdict: verdictSkipped, Detail: "no website or email to check"}
	}

	res := AuditResult{Broker: b}
	var notes []string

	if website != "" {
		ok, err := chk.WebsiteAlive(ctx, website)
		if err == nil {
			v := ok
			res.WebsiteAlive = &v
			if ok {
				notes = append(notes, "website responds")
			} else {
				notes = append(notes, "website dead")
			}
		} else {
			notes = append(notes, "website check inconclusive")
		}
	}
	if domain != "" {
		ok, err := chk.MXExists(domain)
		if err == nil {
			v := ok
			res.MXAlive = &v
			if ok {
				notes = append(notes, "email domain accepts mail")
			} else {
				notes = append(notes, "email domain has no MX")
			}
		} else {
			notes = append(notes, "MX check inconclusive")
		}
	}
	res.Detail = strings.Join(notes, "; ")

	webKnown := res.WebsiteAlive != nil
	mxKnown := res.MXAlive != nil
	switch {
	case !webKnown && !mxKnown:
		res.Verdict = verdictUnknown
	case webKnown && *res.WebsiteAlive && (!mxKnown || *res.MXAlive):
		// Website alive is enough to call the company live; an unchecked or
		// healthy MX doesn't change that.
		res.Verdict = verdictAlive
	case webKnown && !*res.WebsiteAlive && mxKnown && !*res.MXAlive:
		res.Verdict = verdictDefunct
	case webKnown && !*res.WebsiteAlive && mxKnown && *res.MXAlive:
		res.Verdict = verdictWebsiteDead
	case webKnown && *res.WebsiteAlive && mxKnown && !*res.MXAlive:
		res.Verdict = verdictEmailDead
	case !webKnown && mxKnown && !*res.MXAlive:
		res.Verdict = verdictEmailDead
	case !webKnown && mxKnown && *res.MXAlive:
		res.Verdict = verdictAlive
	case webKnown && !*res.WebsiteAlive && !mxKnown:
		// Website dead but mail inconclusive - not enough for "defunct".
		res.Verdict = verdictUnknown
	default:
		res.Verdict = verdictUnknown
	}
	return res
}

// emailDomain extracts the lower-cased domain part of an email address, or
// "" if the address is empty/malformed.
func emailDomain(email string) string {
	email = strings.TrimSpace(email)
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(email[at+1:])
}

// netAuditChecker is the production auditChecker: stdlib HTTP + DNS only.
type netAuditChecker struct {
	client *http.Client
}

func newNetAuditChecker(timeout time.Duration) *netAuditChecker {
	return &netAuditChecker{
		client: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("stopped after 10 redirects")
				}
				return nil
			},
		},
	}
}

// WebsiteAlive reports whether the URL serves any HTTP response at all. Any
// response (even 4xx/5xx) proves the site exists - a parked or bot-blocking
// 403 is not evidence of defunctness. Only connect-level failures count as
// dead; timeouts and other transient errors return an error so the caller
// records "unknown" instead.
func (c *netAuditChecker) WebsiteAlive(ctx context.Context, rawURL string) (bool, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		// Not a URL at all - nothing to reach.
		return false, nil
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		u.Scheme = "https"
	}

	// Some servers answer HEAD with 405/501 without being dead, so a
	// refused HEAD falls back to GET before deciding.
	status, err := c.fetchStatus(ctx, http.MethodHead, u.String())
	if err == nil && (status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented) {
		status, err = c.fetchStatus(ctx, http.MethodGet, u.String())
	}
	if err != nil {
		if isTimeout(err) {
			return false, err // inconclusive
		}
		return false, nil // refused, NXDOMAIN, TLS failure... = unreachable
	}
	return true, nil
}

func (c *netAuditChecker) fetchStatus(ctx context.Context, method, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return 0, err
	}
	// A bare Go client gets bot-blocked by plenty of otherwise-live sites;
	// look like an ordinary browser instead.
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// MXExists reports whether the domain can receive mail: explicit MX records,
// or (per the RFC 5321 implicit-MX fallback) an A/AAAA record on the domain
// itself. Definitive "no such records" answers return false with nil error;
// transient DNS/network trouble returns an error.
func (c *netAuditChecker) MXExists(domain string) (bool, error) {
	resolver := net.DefaultResolver
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	mx, err := resolver.LookupMX(ctx, domain)
	if err == nil {
		return len(mx) > 0, nil
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		// Authoritative NXDOMAIN: no MX can exist on a domain that isn't
		// there.
		return false, nil
	}
	// Some resolvers answer "domain exists, zero MX records" as an error
	// without IsNotFound - treat that the same as an explicit empty answer,
	// but fall through to the implicit-MX check first.
	addrs, aerr := resolver.LookupHost(ctx, domain)
	if aerr == nil {
		return len(addrs) > 0, nil
	}
	if errors.As(aerr, &dnsErr) && dnsErr.IsNotFound {
		return false, nil
	}
	return false, err // inconclusive
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func runAuditBrokers(workers int, timeout time.Duration, reportFile string, historyCheck bool, repliesFile string) error {
	brokerDB, err := broker.LoadFromFile(resolveBrokerPath())
	if err != nil {
		return fmt.Errorf("failed to load brokers: %w", err)
	}

	fmt.Println("🔍 Auditing brokers for defunctness")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	ctx := context.Background()
	results := auditBrokers(ctx, brokerDB.Brokers, newNetAuditChecker(timeout), workers)

	byVerdict := map[auditVerdict][]AuditResult{}
	for _, r := range results {
		byVerdict[r.Verdict] = append(byVerdict[r.Verdict], r)
	}

	fmt.Printf("\nChecked %d brokers:\n", len(results))
	fmt.Printf("  ✅ %-12s %d\n", "alive", len(byVerdict[verdictAlive]))
	fmt.Printf("  💀 %-12s %d\n", "defunct", len(byVerdict[verdictDefunct]))
	fmt.Printf("  🌐 %-12s %d\n", "website-dead", len(byVerdict[verdictWebsiteDead]))
	fmt.Printf("  📧 %-12s %d\n", "email-dead", len(byVerdict[verdictEmailDead]))
	fmt.Printf("  ❓ %-12s %d\n", "unknown", len(byVerdict[verdictUnknown]))
	fmt.Printf("  ⏭️  %-12s %d\n", "skipped", len(byVerdict[verdictSkipped]))

	printVerdictList := func(icon, title string, v auditVerdict, hint string) {
		list := byVerdict[v]
		if len(list) == 0 {
			return
		}
		fmt.Printf("\n%s %s (%d)%s\n", icon, title, len(list), hint)
		sort.Slice(list, func(i, j int) bool { return list[i].Broker.Name < list[j].Broker.Name })
		for _, r := range list {
			tags := ""
			if len(r.Broker.Tags) > 0 {
				tags = fmt.Sprintf(" [tagged: %s]", strings.Join(r.Broker.Tags, ", "))
			}
			fmt.Printf("  - %s [%s] (%s)%s\n", r.Broker.Name, r.Broker.ID, r.Detail, tags)
		}
	}
	printVerdictList("💀", "Defunct candidates", verdictDefunct, " - both website and email dead; review, then retire")
	printVerdictList("🌐", "Website dead", verdictWebsiteDead, " - site gone but mail domain still resolves")
	printVerdictList("📧", "Email dead", verdictEmailDead, " - site live but the email domain cannot receive mail")
	printVerdictList("❓", "Unknown", verdictUnknown, " - rerun later or check by hand; never treated as dead")

	if reportFile != "" {
		if err := writeAuditReport(reportFile, results, byVerdict); err != nil {
			return fmt.Errorf("failed to write report: %w", err)
		}
		fmt.Printf("\n📝 Review checklist written to %s\n", reportFile)
	}

	if historyCheck {
		if err := runAuditHistory(brokerDB); err != nil {
			return err
		}
	}

	if repliesFile != "" {
		if err := exportBrokerReplies(repliesFile, brokerDB); err != nil {
			return err
		}
	}

	return nil
}

// writeAuditReport renders a Markdown checklist a human (or an LLM pass) can
// work through: every non-alive verdict becomes a checkbox item with the
// evidence and current tags.
func writeAuditReport(path string, results []AuditResult, byVerdict map[auditVerdict][]AuditResult) error {
	var sb strings.Builder
	sb.WriteString("# Broker audit checklist\n\n")
	sb.WriteString(fmt.Sprintf("Checked %d brokers: %d alive, %d defunct candidates, %d website-dead, %d email-dead, %d unknown, %d skipped.\n\n",
		len(results),
		len(byVerdict[verdictAlive]), len(byVerdict[verdictDefunct]), len(byVerdict[verdictWebsiteDead]),
		len(byVerdict[verdictEmailDead]), len(byVerdict[verdictUnknown]), len(byVerdict[verdictSkipped])))
	sb.WriteString("Work the sections top-down. Clear a dead address with `eraser mark-bounced`,\n")
	sb.WriteString("exclude a defunct broker on the brokers page, and apply classification\n")
	sb.WriteString("evidence (b2b-only / us-data-only) with `eraser tag-broker <id> <tag>`.\n")

	sections := []struct {
		title string
		v     auditVerdict
		ask   string
	}{
		{"Defunct candidates", verdictDefunct, "Both website and email are dead. Confirm the company is gone, then retire it."},
		{"Website dead", verdictWebsiteDead, "Site is unreachable but mail still resolves. Check whether the email address still belongs to them."},
		{"Email dead", verdictEmailDead, "The email domain cannot receive mail. Find a replacement address or mark it bounced."},
		{"Unknown", verdictUnknown, "A check was inconclusive (timeout etc.). Rerun later - this is never counted as dead."},
	}
	for _, sec := range sections {
		list := byVerdict[sec.v]
		if len(list) == 0 {
			continue
		}
		sb.WriteString(fmt.Sprintf("\n## %s (%d)\n\n%s\n\n", sec.title, len(list), sec.ask))
		sort.Slice(list, func(i, j int) bool { return list[i].Broker.Name < list[j].Broker.Name })
		for _, r := range list {
			tags := "no tags"
			if len(r.Broker.Tags) > 0 {
				tags = "tags: " + strings.Join(r.Broker.Tags, ", ")
			}
			sb.WriteString(fmt.Sprintf("- [ ] **%s** (`%s`) - %s (%s)\n", r.Broker.Name, r.Broker.ID, r.Detail, tags))
			if r.Broker.Website != "" {
				sb.WriteString(fmt.Sprintf("      website: %s\n", r.Broker.Website))
			}
			if r.Broker.Email != "" {
				sb.WriteString(fmt.Sprintf("      email: %s\n", r.Broker.Email))
			}
		}
	}
	return os.WriteFile(path, []byte(sb.String()), 0644)
}

// runAuditHistory lists brokers the inbox classifier already called b2b_only
// (via a stored reply) that do not carry the b2b-only tag yet - the tag is
// what actually stops future sends, so an untagged one is a live wire.
func runAuditHistory(brokerDB *broker.BrokerDatabase) error {
	store, err := history.NewStore(history.DBPathFor(resolveConfigPath()))
	if err != nil {
		return fmt.Errorf("failed to open history database: %w", err)
	}
	defer func() { _ = store.Close() }()

	responses, err := store.GetAllBrokerResponses()
	if err != nil {
		return fmt.Errorf("failed to read broker responses: %w", err)
	}

	fmt.Println("\n🏢 Replies classified b2b_only whose broker lacks the tag")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	seen := map[string]bool{}
	found := 0
	for _, r := range responses {
		if r.ResponseType != string(inbox.ResponseB2BOnly) || seen[r.BrokerID] {
			continue
		}
		seen[r.BrokerID] = true
		b := brokerDB.FindByID(r.BrokerID)
		if b != nil && b.HasTag(broker.TagB2BOnly) {
			continue
		}
		found++
		name := r.BrokerName
		if name == "" {
			name = "(unknown)"
		}
		fmt.Printf("  - %s [%s] replied that it holds no consumer data\n", name, r.BrokerID)
		fmt.Printf("    verify, then run: eraser tag-broker %s %s\n", r.BrokerID, broker.TagB2BOnly)
	}
	if found == 0 {
		fmt.Println("  (none - every b2b_only reply is already tagged)")
	}
	return nil
}

// exportBrokerReplies writes every stored broker reply to FILE for offline
// review (e.g. by a cheap LLM flagging b2b-only / us-data-only statements).
// Bodies are only present for replies recorded by a web inbox scan -
// `eraser monitor` never stores one - and empty entries say so.
func exportBrokerReplies(path string, brokerDB *broker.BrokerDatabase) error {
	store, err := history.NewStore(history.DBPathFor(resolveConfigPath()))
	if err != nil {
		return fmt.Errorf("failed to open history database: %w", err)
	}
	defer func() { _ = store.Close() }()

	responses, err := store.GetAllBrokerResponses()
	if err != nil {
		return fmt.Errorf("failed to read broker responses: %w", err)
	}
	sort.Slice(responses, func(i, j int) bool {
		if responses[i].BrokerID != responses[j].BrokerID {
			return responses[i].BrokerID < responses[j].BrokerID
		}
		return responses[i].ReceivedAt.Before(responses[j].ReceivedAt)
	})

	currentTags := func(id string) []string {
		if b := brokerDB.FindByID(id); b != nil {
			return b.Tags
		}
		return nil
	}

	jsonl := strings.HasSuffix(strings.ToLower(path), ".jsonl")
	var sb strings.Builder
	if !jsonl {
		sb.WriteString("# Broker replies export\n\n")
		sb.WriteString(fmt.Sprintf("%d stored replies. Review each body for statements like \"we only serve\n", len(responses)))
		sb.WriteString("businesses\" or \"we only hold data on US customers\", then apply verdicts\n")
		sb.WriteString("with `eraser tag-broker <broker-id> <tag>`.\n")
	}

	enc := json.NewEncoder(&sb)
	for _, r := range responses {
		tags := currentTags(r.BrokerID)
		if jsonl {
			_ = enc.Encode(map[string]interface{}{
				"broker_id":      r.BrokerID,
				"broker_name":    r.BrokerName,
				"profile_id":     r.ProfileID,
				"classification": r.ResponseType,
				"current_tags":   tags,
				"subject":        r.EmailSubject,
				"from":           r.EmailFrom,
				"received":       r.ReceivedAt.Format(time.RFC3339),
				"body":           r.EmailBody,
				"body_missing":   strings.TrimSpace(r.EmailBody) == "",
			})
			continue
		}

		sb.WriteString(fmt.Sprintf("\n---\n\n## %s (`%s`)\n\n", r.BrokerName, r.BrokerID))
		sb.WriteString(fmt.Sprintf("- Classification: %s\n", r.ResponseType))
		if len(tags) > 0 {
			sb.WriteString(fmt.Sprintf("- Current tags: %s\n", strings.Join(tags, ", ")))
		}
		sb.WriteString(fmt.Sprintf("- Subject: %s\n", r.EmailSubject))
		sb.WriteString(fmt.Sprintf("- Received: %s\n", r.ReceivedAt.Format("Jan 2, 2006")))
		if strings.TrimSpace(r.EmailBody) == "" {
			sb.WriteString("- Body: (none stored - replies recorded by `eraser monitor` have no body;\n")
			sb.WriteString("  only web inbox scans store one)\n")
		} else {
			sb.WriteString("\nBody:\n\n")
			for _, line := range strings.Split(r.EmailBody, "\n") {
				sb.WriteString("> " + line + "\n")
			}
		}
	}

	if err := os.WriteFile(path, []byte(sb.String()), 0644); err != nil {
		return fmt.Errorf("failed to write replies export: %w", err)
	}
	fmt.Printf("\n📤 Exported %d broker replies to %s\n", len(responses), path)
	return nil
}
