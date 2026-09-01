package inbox

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/mail"
	"github.com/eraser-privacy/eraser/internal/broker"
	"github.com/eraser-privacy/eraser/internal/config"
)

// maxMIMEPartBytes caps how much of a single MIME part body we read into
// memory when parsing an incoming email. Broker-reply emails are
// attacker-influenced (this inbox is monitored by IMAP and anyone can send
// it mail), so reading a part body without a bound would let a huge
// attachment or body force unbounded memory growth before we've even
// checked its content-type.
const maxMIMEPartBytes = 10 << 20 // 10MB

// Monitor handles IMAP connection and email monitoring
type Monitor struct {
	config  config.InboxConfig
	client  *client.Client
	brokers map[string]broker.Broker // Map of email domain to broker
	// contacted limits broker matching to brokers we've actually written to.
	// nil means "no gate" - only for callers with no history database (the
	// tests); every real caller sets it via SetContactedBrokers.
	contacted map[string]bool
	// contactedSlugs indexes those brokers by normalized ID and name, for
	// resolving a helpdesk tenant label (anywho.zendesk.com -> anywho).
	contactedSlugs map[string]string
	// contactedAddresses maps each address we sent a request to onto its
	// broker, for finding the original recipient quoted in a reply.
	contactedAddresses map[string]string
	// requestSubjects are the lowercased subject lines of the requests this
	// install has sent. A message quoting one is a reply to us. Empty means
	// subject-based matching is off.
	requestSubjects []string
}

// Email represents a parsed email from a broker
type Email struct {
	UID uint32 // IMAP UID for operations like move/delete
	// Folder is the mailbox this email was fetched from. IMAP UIDs are only
	// unique *within* a mailbox, so anything acting on UID (archiving above
	// all) must carry the folder with it - a UID from [Gmail]/Spam applied
	// against INBOX addresses a different message entirely.
	Folder string
	// UIDValidity is the selected mailbox's UIDVALIDITY at fetch time. If the
	// server changes it, every UID we remembered now refers to something else;
	// ArchiveEmails re-checks it before moving anything.
	UIDValidity uint32
	MessageID   string
	From        string
	FromName    string // Sender display name (e.g., "Mail Delivery System")
	FromDomain  string
	Subject     string
	Body        string
	HTMLBody    string
	ReceivedAt  time.Time
	BrokerID    string // Matched broker ID (if found)
	BrokerName  string // Matched broker name (if found)
	// Attributed reports whether BrokerID names a real broker. False with a
	// non-empty BrokerID means this is a genuine reply to one of our requests
	// that we could not pin to a broker - see inbox.IsUnattributed.
	Attributed bool
	// MatchedVia records which rule identified this as a reply, for logging
	// and the dry-run preview.
	MatchedVia string
}

// unmatchableDomains are sender domains that must never identify a broker,
// however they appear in the broker database.
//
// Brokers are matched on the sender's domain, and the shipped database maps
// general-purpose ones: `google-search-removal` carries no contact address and
// a website of google.com, and several brokers list free-mail contact
// addresses. Left alone, every Google notification and every mail from any
// Gmail user classified as a broker reply - 22 Google notices and the user's
// own test mail were recorded that way in a live database. Mail *from* one of
// these domains tells us nothing about who sent it, so it can never be
// evidence of a broker reply.
var unmatchableDomains = map[string]bool{
	"gmail.com":      true,
	"googlemail.com": true,
	"google.com":     true,
	"hotmail.com":    true,
	"outlook.com":    true,
	"live.com":       true,
	"msn.com":        true,
	"yahoo.com":      true,
	"yahoo.co.uk":    true,
	"ymail.com":      true,
	"aol.com":        true,
	"icloud.com":     true,
	"me.com":         true,
	"mac.com":        true,
	"protonmail.com": true,
	"proton.me":      true,
	"gmx.com":        true,
	"gmx.net":        true,
	"mail.com":       true,
	"zoho.com":       true,
	"yandex.com":     true,
	"yandex.ru":      true,
	"fastmail.com":   true,
	"tutanota.com":   true,
	"hey.com":        true,
	"pm.me":          true,
	"web.de":         true,
	"inbox.lv":       true,
	"inbox.ru":       true,
	"mail.ru":        true,
}

// NewMonitor creates a new inbox monitor
func NewMonitor(cfg config.InboxConfig, brokerList []broker.Broker) *Monitor {
	// Build a map of email domains to brokers for quick lookup
	brokerMap := make(map[string]broker.Broker)
	var skipped []string
	addDomain := func(domain string, b broker.Broker) {
		if domain == "" {
			return
		}
		if unmatchableDomains[domain] {
			skipped = append(skipped, fmt.Sprintf("%s (%s)", domain, b.ID))
			return
		}
		brokerMap[domain] = b
	}

	for _, b := range brokerList {
		// Extract domain from broker email
		if b.Email != "" {
			parts := strings.Split(b.Email, "@")
			if len(parts) == 2 {
				addDomain(strings.ToLower(parts[1]), b)
			}
		}
		// Also map by website domain - but only for a broker we could
		// actually have written to. A broker with no contact address can
		// never reply, so mapping its website domain can only ever produce
		// false matches; that is exactly how google.com entered the map.
		if b.Website != "" && b.Email != "" {
			addDomain(extractDomain(b.Website), b)
		}
		// Same guard for declared reply domains (see broker.Broker.ReplyDomains).
		if b.Email != "" {
			for _, d := range b.ReplyDomains {
				addDomain(strings.ToLower(strings.TrimSpace(d)), b)
			}
		}
	}

	if len(skipped) > 0 {
		log.Printf("Inbox matching: ignoring %d general-purpose sender domain(s) from the broker database: %s",
			len(skipped), strings.Join(skipped, ", "))
	}

	return &Monitor{
		config:  cfg,
		brokers: brokerMap,
	}
}

// extractDomain extracts the domain from a URL
func extractDomain(url string) string {
	url = strings.TrimPrefix(url, "https://")
	url = strings.TrimPrefix(url, "http://")
	url = strings.TrimPrefix(url, "www.")
	parts := strings.Split(url, "/")
	if len(parts) > 0 {
		return strings.ToLower(parts[0])
	}
	return ""
}

// Connect establishes IMAP connection
func (m *Monitor) Connect(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", m.config.Server, m.config.Port)

	log.Printf("Connecting to IMAP server %s...", addr)

	c, err := client.DialTLS(addr, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to IMAP server: %w", err)
	}

	log.Printf("Connected, logging in as %s...", m.config.Email)

	if err := c.Login(m.config.Email, m.config.Password); err != nil {
		_ = c.Logout()
		return fmt.Errorf("failed to login: %w", err)
	}

	m.client = c
	log.Printf("Login successful")
	return nil
}

// Disconnect closes the IMAP connection
func (m *Monitor) Disconnect() error {
	if m.client != nil {
		return m.client.Logout()
	}
	return nil
}

// uidSearchCtx runs UidSearch in a goroutine and honors ctx cancellation,
// mirroring the pattern WatchForNewEmails uses for its blocking IDLE call
// (the go-imap v1.2.1 client has no native context support, so this is the
// only cancellation mechanism it offers). Note that if ctx is canceled
// before the IMAP server responds, this returns early but the goroutine
// keeps running the command against the shared connection until the server
// replies - same caveat WatchForNewEmails documents for its IDLE loop.
func (m *Monitor) uidSearchCtx(ctx context.Context, criteria *imap.SearchCriteria) ([]uint32, error) {
	type result struct {
		uids []uint32
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		uids, err := m.client.UidSearch(criteria)
		resultCh <- result{uids, err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-resultCh:
		return r.uids, r.err
	}
}

// fetchMessagesCtx runs UidFetch in a goroutine, parses messages as they
// stream in, and honors ctx cancellation - same pattern/caveat as
// uidSearchCtx above. bufSize sizes the messages channel so the UidFetch
// goroutine never blocks writing to it even if we stop reading early.
func (m *Monitor) fetchMessagesCtx(ctx context.Context, seqSet *imap.SeqSet, items []imap.FetchItem, section *imap.BodySectionName, bufSize int) ([]Email, error) {
	messages := make(chan *imap.Message, bufSize)
	done := make(chan error, 1)
	go func() {
		done <- m.client.UidFetch(seqSet, items, messages)
	}()

	var emails []Email
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case msg, ok := <-messages:
			if !ok {
				if err := <-done; err != nil {
					return nil, fmt.Errorf("failed to fetch messages: %w", err)
				}
				return emails, nil
			}
			email, err := m.parseMessage(msg, section)
			if err != nil {
				log.Printf("Warning: failed to parse message: %v", err)
				continue
			}
			if email != nil {
				emails = append(emails, *email)
			}
		}
	}
}

// FetchRecentEmails fetches emails from the last N days
func (m *Monitor) FetchRecentEmails(ctx context.Context, days int) ([]Email, error) {
	if m.client == nil {
		return nil, fmt.Errorf("not connected to IMAP server")
	}

	// Select the mailbox (usually INBOX)
	mbox, err := m.client.Select(m.config.Folder, false)
	if err != nil {
		return nil, fmt.Errorf("failed to select mailbox %s: %w", m.config.Folder, err)
	}

	log.Printf("Mailbox %s has %d messages", m.config.Folder, mbox.Messages)

	if mbox.Messages == 0 {
		return nil, nil
	}

	// Search for emails from the last N days (use UID search)
	since := time.Now().AddDate(0, 0, -days)
	criteria := imap.NewSearchCriteria()
	criteria.Since = since

	uids, err := m.uidSearchCtx(ctx, criteria)
	if err != nil {
		return nil, fmt.Errorf("failed to search emails: %w", err)
	}

	log.Printf("Found %d emails since %s", len(uids), since.Format("2006-01-02"))

	if len(uids) == 0 {
		return nil, nil
	}

	// Fetch the messages using UIDs
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uids...)

	// Peek, so scanning doesn't mark every message in the mailbox as read: a
	// plain BODY[] fetch implicitly sets \Seen (RFC 3501), which silently
	// marked hundreds of inbox messages read on each scan. The archive-folder
	// fetch below has always peeked; this path just never did.
	section := &imap.BodySectionName{Peek: true}
	items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags, imap.FetchUid, section.FetchItem()}

	emails, err := m.fetchMessagesCtx(ctx, seqSet, items, section, len(uids))
	if err != nil {
		return nil, err
	}

	stampSource(emails, m.config.Folder, mbox.UidValidity)
	return emails, nil
}

// stampSource records which mailbox a batch of emails came from, and that
// mailbox's UIDVALIDITY, so their UIDs stay resolvable later.
func stampSource(emails []Email, folder string, uidValidity uint32) {
	for i := range emails {
		emails[i].Folder = folder
		emails[i].UIDValidity = uidValidity
	}
}

// parseMessage converts an IMAP message to our Email struct
func (m *Monitor) parseMessage(msg *imap.Message, section *imap.BodySectionName) (*Email, error) {
	if msg == nil || msg.Envelope == nil {
		return nil, nil
	}

	email := &Email{
		UID:        msg.Uid,
		Subject:    msg.Envelope.Subject,
		ReceivedAt: msg.Envelope.Date,
	}

	// Get message ID
	if msg.Envelope.MessageId != "" {
		email.MessageID = msg.Envelope.MessageId
	}

	// Get sender
	if len(msg.Envelope.From) > 0 {
		from := msg.Envelope.From[0]
		email.From = from.Address()
		email.FromName = from.PersonalName
		if from.HostName != "" {
			email.FromDomain = strings.ToLower(from.HostName)
		}
	}

	// Body first: attributing a reply by the address we originally wrote to
	// (quoted back at us in the auto-acknowledgement) needs the body parsed,
	// and it's the most precise rule available. See matchReply for the order
	// the rules are tried in and the guards that run before any of them.
	if err := m.parseBody(email, msg, section); err != nil {
		return nil, err
	}

	if match := m.matchReply(email); match.Matched() {
		email.BrokerID = match.BrokerID
		email.Attributed = match.Attributed
		email.MatchedVia = match.Via
		if match.Attributed {
			if b, ok := m.brokerByID(match.BrokerID); ok {
				email.BrokerName = b.Name
			}
		} else {
			email.BrokerName = UnattributedName(match.BrokerID)
		}
	}

	return email, nil
}

// brokerByID finds a broker in the domain map by its ID.
func (m *Monitor) brokerByID(id string) (broker.Broker, bool) {
	for _, b := range m.brokers {
		if b.ID == id {
			return b, true
		}
	}
	return broker.Broker{}, false
}

// parseBody fills in the text and HTML bodies of email from the fetched
// message.
func (m *Monitor) parseBody(email *Email, msg *imap.Message, section *imap.BodySectionName) error {
	r := msg.GetBody(section)
	if r == nil {
		return nil
	}

	mr, err := mail.CreateReader(r)
	if err != nil {
		return nil // No body available; not fatal
	}

	// Process each part
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}

		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			ct, _, _ := h.ContentType()
			body, _ := io.ReadAll(io.LimitReader(p.Body, maxMIMEPartBytes))

			if strings.HasPrefix(ct, "text/plain") && email.Body == "" {
				email.Body = string(body)
			} else if strings.HasPrefix(ct, "text/html") && email.HTMLBody == "" {
				email.HTMLBody = string(body)
			}
		}
	}

	return nil
}

// FetchBrokerEmails fetches only emails from known broker domains
func (m *Monitor) FetchBrokerEmails(ctx context.Context, days int) ([]Email, error) {
	allEmails, err := m.FetchRecentEmails(ctx, days)
	if err != nil {
		return nil, err
	}

	var brokerEmails []Email
	for _, email := range allEmails {
		if email.BrokerID != "" {
			brokerEmails = append(brokerEmails, email)
		}
	}

	log.Printf("Found %d emails from known brokers (out of %d total)", len(brokerEmails), len(allEmails))
	return brokerEmails, nil
}

// FetchBrokerEmailsFromFolder fetches broker emails from a specific folder
func (m *Monitor) FetchBrokerEmailsFromFolder(ctx context.Context, folder string, days int) ([]Email, error) {
	if m.client == nil {
		return nil, fmt.Errorf("not connected to IMAP server")
	}

	// Select the specified folder
	mbox, err := m.client.Select(folder, false)
	if err != nil {
		return nil, fmt.Errorf("failed to select folder %s: %w", folder, err)
	}
	log.Printf("Folder %s has %d messages", folder, mbox.Messages)

	if mbox.Messages == 0 {
		return nil, nil
	}

	// Search for emails from the last N days
	since := time.Now().AddDate(0, 0, -days)
	criteria := imap.NewSearchCriteria()
	criteria.Since = since

	uids, err := m.uidSearchCtx(ctx, criteria)
	if err != nil {
		return nil, fmt.Errorf("failed to search emails in %s: %w", folder, err)
	}

	log.Printf("Found %d emails since %s in %s", len(uids), since.Format("2006-01-02"), folder)

	if len(uids) == 0 {
		return nil, nil
	}

	// Fetch emails in batches
	var allEmails []Email
	batchSize := 50
	for i := 0; i < len(uids); i += batchSize {
		end := i + batchSize
		if end > len(uids) {
			end = len(uids)
		}

		seqSet := new(imap.SeqSet)
		for _, uid := range uids[i:end] {
			seqSet.AddNum(uid)
		}

		// Fetch message details
		section := &imap.BodySectionName{Peek: true}
		items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchUid, section.FetchItem()}

		batchEmails, err := m.fetchMessagesCtx(ctx, seqSet, items, section, batchSize)
		if err != nil {
			if ctx.Err() != nil {
				// ctx was canceled/timed out - abort the whole fetch rather
				// than continuing to hammer a connection the caller has
				// given up on.
				return nil, err
			}
			log.Printf("Warning: error fetching batch: %v", err)
			continue
		}
		stampSource(batchEmails, folder, mbox.UidValidity)
		allEmails = append(allEmails, batchEmails...)
	}

	// Filter to broker emails only
	var brokerEmails []Email
	for _, email := range allEmails {
		if email.BrokerID != "" {
			brokerEmails = append(brokerEmails, email)
		}
	}

	log.Printf("Found %d emails from known brokers in %s (out of %d total)", len(brokerEmails), folder, len(allEmails))
	return brokerEmails, nil
}

// FetchBounceEmails fetches emails that look like bounce/undeliverable notifications
func (m *Monitor) FetchBounceEmails(ctx context.Context, days int) ([]Email, error) {
	allEmails, err := m.FetchRecentEmails(ctx, days)
	if err != nil {
		return nil, err
	}

	// Bounce sender patterns
	bounceSenders := []string{
		"mailer-daemon", "postmaster", "mail delivery",
		"mail delivery system", "mail delivery subsystem",
		"mailerdaemon", "mailsystem",
	}

	// Bounce subject patterns
	bounceSubjects := []string{
		"undeliverable", "delivery failed", "delivery status notification",
		"returned mail", "mail delivery failed", "delivery failure",
		"message not delivered", "could not be delivered",
	}

	var bounceEmails []Email
	for _, email := range allEmails {
		fromLower := strings.ToLower(email.From)
		fromNameLower := strings.ToLower(email.FromName)
		subjectLower := strings.ToLower(email.Subject)

		isBounce := false

		// Check sender
		for _, sender := range bounceSenders {
			if strings.Contains(fromLower, sender) || strings.Contains(fromNameLower, sender) {
				isBounce = true
				break
			}
		}

		// Check subject if not already identified as bounce
		if !isBounce {
			for _, pattern := range bounceSubjects {
				if strings.Contains(subjectLower, pattern) {
					isBounce = true
					break
				}
			}
		}

		if isBounce {
			bounceEmails = append(bounceEmails, email)
		}
	}

	log.Printf("Found %d bounce emails (out of %d total)", len(bounceEmails), len(allEmails))
	return bounceEmails, nil
}

// WatchForNewEmails monitors for new emails (blocking)
func (m *Monitor) WatchForNewEmails(ctx context.Context, callback func(Email)) error {
	if m.client == nil {
		return fmt.Errorf("not connected to IMAP server")
	}

	// Select mailbox
	_, err := m.client.Select(m.config.Folder, false)
	if err != nil {
		return fmt.Errorf("failed to select mailbox: %w", err)
	}

	// Start IDLE
	updates := make(chan client.Update)
	m.client.Updates = updates

	stop := make(chan struct{})
	idleDone := make(chan error, 1)

	go func() {
		idleDone <- m.client.Idle(stop, nil)
	}()

	log.Printf("Watching for new emails (press Ctrl+C to stop)...")

	for {
		select {
		case <-ctx.Done():
			close(stop)
			<-idleDone // wait for the Idle() goroutine to actually exit before
			// this function returns, so the caller doesn't touch m.client
			// concurrently with the IMAP connection still being in use
			return ctx.Err()
		case update := <-updates:
			switch u := update.(type) {
			case *client.MailboxUpdate:
				log.Printf("New mail detected: %d messages", u.Mailbox.Messages)
				// Fetch the latest message
				close(stop)
				<-idleDone

				emails, err := m.FetchRecentEmails(ctx, 1)
				if err != nil {
					log.Printf("Error fetching new email: %v", err)
				} else if len(emails) > 0 {
					// Process the newest email
					for _, email := range emails {
						if email.BrokerID != "" {
							callback(email)
						}
					}
				}

				// Restart IDLE
				stop = make(chan struct{})
				go func() {
					idleDone <- m.client.Idle(stop, nil)
				}()
			}
		case err := <-idleDone:
			if err != nil {
				return fmt.Errorf("IDLE error: %w", err)
			}
		}
	}
}

// EnsureFolderExists creates a folder/label if it doesn't already exist.
func (m *Monitor) EnsureFolderExists(name string) error {
	if m.client == nil {
		return fmt.Errorf("not connected to IMAP server")
	}
	return m.ensureFolder(name)
}

// ensureFolder creates name if absent, tolerating the "it already exists"
// case without depending on the server's error text.
//
// It attempts CREATE first and only investigates on failure. RFC 3501 gives
// "already exists" no distinguishable form - it's a tagged NO, and go-imap's
// Create returns status.Err() while discarding the response code that RFC
// 5530's [ALREADYEXISTS] would arrive in - so the error alone can't be
// classified. Re-listing afterwards answers the only question that matters:
// is the folder there now? That also makes a concurrent creation a success
// rather than a failure, which the previous list-then-create order got wrong.
func (m *Monitor) ensureFolder(name string) error {
	if name == "" {
		return fmt.Errorf("folder name is required")
	}

	createErr := m.client.Create(name)
	if createErr == nil {
		log.Printf("Created folder '%s'", name)
		return nil
	}

	exists, listErr := m.folderExists(name)
	if listErr != nil {
		return fmt.Errorf("failed to create folder '%s' (%v) and could not verify whether it exists: %w", name, createErr, listErr)
	}
	if exists {
		return nil
	}

	// A Gmail label with "Show in IMAP" turned off is invisible to LIST yet
	// still blocks CREATE, which lands here and would otherwise read as an
	// inscrutable failure.
	return fmt.Errorf("failed to create folder '%s': %w (if this is a Gmail label that already exists, enable \"Show in IMAP\" for it in Gmail's label settings)", name, createErr)
}

func (m *Monitor) folderExists(name string) (bool, error) {
	mailboxes := make(chan *imap.MailboxInfo, 20)
	done := make(chan error, 1)
	go func() {
		done <- m.client.List("", "*", mailboxes)
	}()

	exists := false
	for mbox := range mailboxes {
		if strings.EqualFold(imap.CanonicalMailboxName(mbox.Name), imap.CanonicalMailboxName(name)) {
			exists = true
		}
	}
	if err := <-done; err != nil {
		return false, fmt.Errorf("failed to list folders: %w", err)
	}
	return exists, nil
}

// SetContactedBrokers restricts broker matching to brokers this install has
// actually sent a removal request to (see history.Store.ContactedBrokerIDs).
// Passing nil disables the gate, which is only appropriate where there is no
// history database to consult.
func (m *Monitor) SetContactedBrokers(ids map[string]bool) {
	m.contacted = ids

	// Index them by normalized ID and name so a helpdesk tenant label can be
	// resolved. Built here rather than per-message: the broker list is large
	// and the set only changes when the gate does.
	m.contactedSlugs = make(map[string]string, len(ids)*2)
	for _, b := range m.brokers {
		if ids != nil && !ids[b.ID] {
			continue
		}
		if slug := normalizeSlug(b.ID); slug != "" {
			m.contactedSlugs[slug] = b.ID
		}
		if slug := normalizeSlug(b.Name); slug != "" {
			// Don't let a name collision silently reassign an ID match.
			if _, taken := m.contactedSlugs[slug]; !taken {
				m.contactedSlugs[slug] = b.ID
			}
		}
	}
}

// SetContactedAddresses supplies the addresses this install has sent requests
// to, mapped to their brokers (see history.Store.ContactedBrokerAddresses).
// Used to attribute a reply by the original recipient quoted in its body.
func (m *Monitor) SetContactedAddresses(addrs map[string]string) {
	m.contactedAddresses = addrs
}

// SetRequestSubjects enables subject-based reply matching for the given
// subject lines (see template.RequestSubjects). Passing none disables it,
// leaving sender-domain matching as the only rule.
func (m *Monitor) SetRequestSubjects(subjects []string) {
	m.requestSubjects = m.requestSubjects[:0]
	for _, s := range subjects {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			m.requestSubjects = append(m.requestSubjects, s)
		}
	}
}

// ContactedBrokerCount reports how many brokers the matching gate will admit,
// and whether a gate is in force at all.
//
// Callers should surface a zero count rather than let it pass quietly: the
// gate reads removal_requests, and "Clear All History" empties that table
// (history.Store.DeleteAllHistory) while leaving the inbox settings untouched.
// The scan then matches nothing at all and reports "no broker emails found",
// which looks identical to a genuinely quiet inbox. Sending requests again
// repopulates it; replies to requests sent before the wipe stay unmatchable,
// because the record of having written to those brokers is gone.
func (m *Monitor) ContactedBrokerCount() (int, bool) {
	if m.contacted == nil {
		return 0, false
	}
	return len(m.contacted), true
}

// GroupUIDsByFolder buckets emails by the mailbox they were fetched from, so
// each archive call addresses UIDs against the mailbox they actually belong
// to. IMAP UIDs are unique only within a mailbox, so a flat list drawn from
// several folders cannot be acted on safely.
//
// Emails with UID 0 are dropped: go-imap's SeqSet encodes 0 as "*", which
// addresses the highest UID in the mailbox - i.e. the newest message, which
// by definition is not one we selected. Emails with no folder recorded are
// dropped for the same reason, since there is no mailbox to resolve their
// UID against.
func GroupUIDsByFolder(emails []Email) map[string][]uint32 {
	groups := make(map[string][]uint32)
	for _, e := range emails {
		if e.UID == 0 || e.Folder == "" {
			continue
		}
		groups[e.Folder] = append(groups[e.Folder], e.UID)
	}
	return groups
}

// UIDValidityByFolder returns the UIDVALIDITY observed per folder at fetch
// time, for ArchiveEmails to re-check before it moves anything. A folder
// reporting two different values within one scan is a server-side
// renumbering; the zero value means "unknown, skip the check".
func UIDValidityByFolder(emails []Email) map[string]uint32 {
	seen := make(map[string]uint32)
	for _, e := range emails {
		if e.Folder == "" || e.UIDValidity == 0 {
			continue
		}
		if prev, ok := seen[e.Folder]; ok && prev != e.UIDValidity {
			log.Printf("WARNING: folder %q reported two UIDVALIDITY values (%d, %d) during one scan - not archiving from it", e.Folder, prev, e.UIDValidity)
			seen[e.Folder] = 0
			continue
		}
		if seen[e.Folder] == 0 {
			seen[e.Folder] = e.UIDValidity
		}
	}
	return seen
}

// deletedUIDsBesides returns the UIDs currently flagged \Deleted in the
// selected mailbox that are NOT in ours - used by ArchiveEmails to detect
// (not prevent) the Expunge(nil)-removes-everything hazard documented there.
func (m *Monitor) deletedUIDsBesides(ours []uint32) ([]uint32, error) {
	criteria := imap.NewSearchCriteria()
	criteria.WithFlags = []string{imap.DeletedFlag}

	allDeleted, err := m.client.UidSearch(criteria)
	if err != nil {
		return nil, err
	}

	ourUIDs := make(map[uint32]bool, len(ours))
	for _, uid := range ours {
		ourUIDs[uid] = true
	}

	var unexpected []uint32
	for _, uid := range allDeleted {
		if !ourUIDs[uid] {
			unexpected = append(unexpected, uid)
		}
	}
	return unexpected, nil
}

// ArchiveEmails moves messages out of srcFolder into destFolder.
//
// srcFolder is required and must be the mailbox the UIDs were fetched from -
// IMAP UIDs mean nothing outside their own mailbox, so passing INBOX UIDs
// while the caller meant Spam moves unrelated mail. Use GroupUIDsByFolder to
// build the per-folder batches. wantUIDValidity, when non-zero, is checked
// against the mailbox's current UIDVALIDITY and aborts the move if the server
// has renumbered since the fetch.
func (m *Monitor) ArchiveEmails(uids []uint32, srcFolder, destFolder string, wantUIDValidity uint32) error {
	// Argument checks first, before touching the connection, so the rules
	// that keep this from moving the wrong mail are verifiable without an
	// IMAP server.
	if err := checkArchiveArgs(uids, srcFolder, destFolder); err != nil {
		return err
	}
	if len(uids) == 0 {
		return nil
	}
	// A move onto itself is never what the caller wanted, and is actively
	// destructive on the copy+expunge path (the copy is a no-op, then the
	// expunge removes the only remaining copy). The scan reads the archive
	// folder back to pick up already-filed replies, so this case arises
	// naturally rather than only through misuse.
	if sameMailbox(srcFolder, destFolder) {
		return nil
	}

	if m.client == nil {
		return fmt.Errorf("not connected to IMAP server")
	}

	mbox, err := m.client.Select(srcFolder, false)
	if err != nil {
		return fmt.Errorf("failed to select mailbox %q: %w", srcFolder, err)
	}
	if wantUIDValidity != 0 && mbox.UidValidity != wantUIDValidity {
		return fmt.Errorf("archive: %q UIDVALIDITY changed (%d -> %d) since these messages were read; the UIDs now refer to different messages, so nothing was moved",
			srcFolder, wantUIDValidity, mbox.UidValidity)
	}

	if err := m.ensureFolder(destFolder); err != nil {
		return err
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uids...)

	// Decide up front whether a real MOVE is available, rather than calling
	// UidMove and reacting to an error. go-imap's UidMove silently falls back
	// to COPY + STORE(\Deleted) + EXPUNGE(nil) with no checks of its own when
	// the server lacks MOVE, which means the careful guarded fallback below
	// would never run - the unguarded one inside the library would.
	moveSupported, err := m.client.Support("MOVE")
	if err != nil {
		return fmt.Errorf("archive: could not determine MOVE support: %w", err)
	}

	if !moveSupported {
		// Copy-then-expunge is not a safe substitute everywhere. On Gmail,
		// \Deleted + EXPUNGE inside Spam, Trash or All Mail permanently
		// destroys the message rather than just unfiling it - and Gmail's
		// COPY only adds a label to the same underlying message, so the
		// "copy" made a moment earlier is destroyed along with it. Refuse
		// rather than degrade.
		if attrs, err := m.folderAttributes(srcFolder); err != nil {
			log.Printf("Warning: could not read attributes of %q before archiving: %v", srcFolder, err)
		} else {
			for _, a := range attrs {
				switch a {
				case imap.JunkAttr, imap.TrashAttr, imap.AllAttr:
					return fmt.Errorf("archive: refusing to move mail out of %q (%s) without server MOVE support - the copy+expunge fallback permanently deletes mail in this mailbox", srcFolder, a)
				}
			}
		}

		log.Printf("Server does not support MOVE; falling back to COPY+DELETE for %d message(s) in %q", len(uids), srcFolder)

		// Fallback to COPY + DELETE if MOVE not supported
		if err := m.client.UidCopy(seqSet, destFolder); err != nil {
			return fmt.Errorf("failed to copy emails to '%s': %w", destFolder, err)
		}

		// Mark as deleted
		item := imap.FormatFlagsOp(imap.AddFlags, true)
		flags := []interface{}{imap.DeletedFlag}
		if err := m.client.UidStore(seqSet, item, flags, nil); err != nil {
			return fmt.Errorf("failed to mark emails as deleted: %w", err)
		}

		// go-imap v1.2.1's client does not implement the UIDPLUS extension
		// (there is no UidExpunge/"UID EXPUNGE" method - Expunge(ch) is the
		// only option), and plain IMAP EXPUNGE removes EVERY \Deleted-flagged
		// message in the mailbox, not just the ones we just flagged above. A
		// concurrent process (or a stale flag left over from an earlier
		// partial failure) could cause real mail loss here.
		//
		// We can't close that race with this library version, but we can
		// avoid silently hiding it: check right before expunging whether any
		// UID besides our own is already flagged \Deleted, and log loudly if
		// so, then afterward verify the number of messages actually expunged
		// matches what we expected and warn on mismatch.
		if unexpected, err := m.deletedUIDsBesides(uids); err != nil {
			log.Printf("Warning: could not verify \\Deleted flag scope before expunge: %v", err)
		} else if len(unexpected) > 0 {
			log.Printf("WARNING: %d message(s) besides the %d just archived are already flagged \\Deleted and will ALSO be removed by this expunge (UIDs: %v) - go-imap v1.2.1 has no UID EXPUNGE to scope this call", len(unexpected), len(uids), unexpected)
		}

		expunged := make(chan uint32, len(uids)+8)
		expungeDone := make(chan error, 1)
		go func() {
			expungeDone <- m.client.Expunge(expunged)
		}()

		var expungedCount int
		for range expunged {
			expungedCount++
		}

		if err := <-expungeDone; err != nil {
			return fmt.Errorf("failed to expunge deleted emails: %w", err)
		}

		if expungedCount != len(uids) {
			log.Printf("WARNING: expunge removed %d message(s) but this operation only intended to remove %d - other \\Deleted-flagged mail may have been removed too", expungedCount, len(uids))
		}

		log.Printf("Archived %d emails from '%s' to '%s'", len(uids), srcFolder, destFolder)
		return nil
	}

	if err := m.client.UidMove(seqSet, destFolder); err != nil {
		return fmt.Errorf("failed to move emails from '%s' to '%s': %w", srcFolder, destFolder, err)
	}

	log.Printf("Archived %d emails from '%s' to '%s'", len(uids), srcFolder, destFolder)
	return nil
}

// sameMailbox reports whether two mailbox names refer to the same folder,
// normalizing the INBOX spelling the way the server would.
func sameMailbox(a, b string) bool {
	return strings.EqualFold(imap.CanonicalMailboxName(a), imap.CanonicalMailboxName(b))
}

// checkArchiveArgs validates the inputs that decide *which* messages get
// moved. Kept separate and free of IMAP state so the rules can be tested
// directly - these are the checks standing between a routine scan and moving
// somebody's unrelated mail.
func checkArchiveArgs(uids []uint32, srcFolder, destFolder string) error {
	if srcFolder == "" {
		return fmt.Errorf("archive: source folder is required (UIDs are only meaningful within the mailbox they came from)")
	}
	if destFolder == "" {
		return fmt.Errorf("archive: destination folder is required")
	}
	// Reject UID 0 rather than filtering it out: go-imap encodes 0 in a
	// SeqSet as "*", which addresses the highest UID in the mailbox - the
	// newest message, which is certainly not one we classified. A zero here
	// means an upstream fetch lost the UID, and silently dropping it would
	// hide that.
	for _, uid := range uids {
		if uid == 0 {
			return fmt.Errorf("archive: refusing to act on UID 0 (encodes as \"*\" and would address the newest message in %q)", srcFolder)
		}
	}
	return nil
}

// folderAttributes returns the LIST attributes (\Junk, \Trash, \All, ...)
// advertised for one mailbox.
func (m *Monitor) folderAttributes(name string) ([]string, error) {
	mailboxes := make(chan *imap.MailboxInfo, 20)
	done := make(chan error, 1)
	go func() {
		done <- m.client.List("", "*", mailboxes)
	}()

	var attrs []string
	for mbox := range mailboxes {
		if strings.EqualFold(imap.CanonicalMailboxName(mbox.Name), imap.CanonicalMailboxName(name)) {
			attrs = mbox.Attributes
		}
	}
	if err := <-done; err != nil {
		return nil, err
	}
	return attrs, nil
}

// FindSpecialFolder returns the mailbox advertising the given SPECIAL-USE
// attribute (e.g. imap.JunkAttr for the spam folder).
//
// Discovery by attribute rather than by name is what makes this work across
// accounts: Gmail's spam mailbox is "[Gmail]/Spam" in English but localized
// elsewhere, and other servers call it "Junk". Note the server has to
// volunteer these attributes in its LIST response - go-imap v1.2.1 cannot
// send LIST ... RETURN (SPECIAL-USE) - so a server that only reports them on
// request will yield nothing here, and the caller should treat "not found" as
// "skip", not as an error.
func (m *Monitor) FindSpecialFolder(attr string) (string, bool, error) {
	if m.client == nil {
		return "", false, fmt.Errorf("not connected to IMAP server")
	}

	mailboxes := make(chan *imap.MailboxInfo, 20)
	done := make(chan error, 1)
	go func() {
		done <- m.client.List("", "*", mailboxes)
	}()

	var found string
	for mbox := range mailboxes {
		for _, a := range mbox.Attributes {
			if strings.EqualFold(a, attr) && found == "" {
				found = mbox.Name
			}
		}
	}
	if err := <-done; err != nil {
		return "", false, fmt.Errorf("failed to list folders: %w", err)
	}
	return found, found != "", nil
}

// SpamFolder resolves which mailbox to scan for broker replies that were
// filed as spam: the configured override if set, otherwise whichever mailbox
// advertises \Junk.
func (m *Monitor) SpamFolder() (string, bool, error) {
	if m.config.SpamFolder != "" {
		return m.config.SpamFolder, true, nil
	}
	return m.FindSpecialFolder(imap.JunkAttr)
}
