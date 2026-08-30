package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/eraser-privacy/eraser/internal/broker"
	"github.com/eraser-privacy/eraser/internal/config"
	"github.com/eraser-privacy/eraser/internal/history"
	"github.com/eraser-privacy/eraser/internal/inbox"
	emaTemplate "github.com/eraser-privacy/eraser/internal/template"
	"github.com/spf13/cobra"
)

func monitorCmd() *cobra.Command {
	var days int
	var once bool
	var watch bool
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "monitor",
		Short: "Monitor inbox for broker responses",
		Long: `Connect to your email inbox via IMAP and monitor for responses from data brokers.

This command will:
- Fetch recent emails from known broker domains
- Classify responses (form required, confirmation needed, success, etc.)
- Extract form URLs and confirmation links
- Store results for the pipeline to process

Requires inbox configuration in config.yaml with IMAP settings.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMonitor(days, once, watch, dryRun)
		},
	}

	cmd.Flags().IntVar(&days, "days", 7, "Number of days to look back for emails")
	cmd.Flags().BoolVar(&once, "once", false, "Check inbox once and exit (don't watch for new emails)")
	cmd.Flags().BoolVar(&watch, "watch", false, "Continuously watch for new emails")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "List the broker emails that would be archived, with sender and subject, and move nothing")

	return cmd
}

func runMonitor(days int, once bool, watch bool, dryRun bool) error {
	cfg, err := config.Load(resolveConfigPath())
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Validate inbox config
	if err := cfg.ValidateInbox(); err != nil {
		fmt.Println("📧 Inbox monitoring is not configured.")
		fmt.Println()
		fmt.Println("To enable inbox monitoring, add the following to your config.yaml:")
		fmt.Println()
		fmt.Println("inbox:")
		fmt.Println("  enabled: true")
		fmt.Println("  provider: gmail")
		fmt.Println("  email: your-email@gmail.com")
		fmt.Println("  password: your-app-password  # Use an App Password, not your main password")
		fmt.Println()
		fmt.Println("For Gmail, you'll need to:")
		fmt.Println("  1. Enable 2-Step Verification")
		fmt.Println("  2. Generate an App Password at https://myaccount.google.com/apppasswords")
		fmt.Println("  3. Enable IMAP in Gmail settings")
		return err
	}

	// Load brokers for domain matching
	brokerDB, err := broker.LoadFromFile(resolveBrokerPath())
	if err != nil {
		return fmt.Errorf("failed to load brokers: %w", err)
	}

	// Initialize history store
	store, err := history.NewStore(history.DBPathFor(resolveConfigPath()))
	if err != nil {
		return fmt.Errorf("failed to initialize history: %w", err)
	}
	defer func() { _ = store.Close() }()

	// Create inbox monitor, matching only brokers we've actually written to -
	// a bare sender-domain match against the broker database pulls in
	// ordinary mail (see inbox.unmatchableDomains and
	// history.Store.ContactedBrokerIDs).
	monitor := inbox.NewMonitor(cfg.Inbox, brokerDB.Brokers)
	contacted, err := store.ContactedBrokerIDs()
	if err != nil {
		return fmt.Errorf("failed to load contacted brokers: %w", err)
	}
	monitor.SetContactedBrokers(contacted)

	// Recognise replies that arrive from a helpdesk tenant or parent company
	// by the request subject they quote, and attribute them by the address we
	// originally wrote to. Both derived from what was actually sent.
	if templates, err := store.SentTemplates(); err != nil {
		fmt.Printf("⚠️  Could not load sent templates, subject-based reply matching disabled: %v\n", err)
	} else {
		monitor.SetRequestSubjects(emaTemplate.RequestSubjects(templates))
	}
	if addrs, err := store.ContactedBrokerAddresses(); err != nil {
		fmt.Printf("⚠️  Could not load contacted addresses: %v\n", err)
	} else {
		monitor.SetContactedAddresses(addrs)
	}

	if len(contacted) == 0 {
		// Not an error on a fresh install, but it's also the state "Clear All
		// History" leaves behind - after which a scan matches nothing and
		// looks just like a quiet inbox.
		fmt.Println("ℹ️  No sent requests on record, so no incoming mail can be matched to a broker.")
		fmt.Println("   If you cleared your history, replies to requests sent before that are no longer matchable.")
		fmt.Println()
	}

	// Connect to IMAP
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\nShutting down...")
		cancel()
	}()

	if err := monitor.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect to inbox: %w", err)
	}
	defer func() { _ = monitor.Disconnect() }()

	fmt.Printf("📬 Monitoring inbox for broker responses (last %d days)...\n", days)
	fmt.Println()

	// Fetch emails from known broker domains
	emails, err := monitor.FetchBrokerEmails(ctx, days)
	if err != nil {
		return fmt.Errorf("failed to fetch emails: %w", err)
	}

	// Brokers replying to a bulk request are routinely filed as spam, so
	// those replies would otherwise never be seen. Discovery failing isn't
	// fatal - the account may not advertise \Junk.
	discoveredSpam := ""
	if cfg.Inbox.ScanSpam {
		spamFolder, found, err := monitor.SpamFolder()
		discoveredSpam = spamFolder
		switch {
		case err != nil:
			fmt.Printf("⚠️  Could not locate the spam folder: %v\n", err)
		case !found:
			fmt.Println("⚠️  scan_spam is on but no mailbox advertises \\Junk; set inbox.spam_folder to name it explicitly")
		default:
			spamEmails, err := monitor.FetchBrokerEmailsFromFolder(ctx, spamFolder, days)
			if err != nil {
				fmt.Printf("⚠️  Could not read spam folder %s: %v\n", spamFolder, err)
			} else if len(spamEmails) > 0 {
				fmt.Printf("📨 Found %d broker email(s) in %s\n", len(spamEmails), spamFolder)
				emails = append(emails, spamEmails...)
			}
		}
	}

	if dryRun {
		printArchivePreview(emails, cfg.Inbox, discoveredSpam)
		return nil
	}

	if len(emails) == 0 {
		fmt.Println("No emails from known brokers found.")
		if !watch {
			return nil
		}
	}

	// Classify and process each email
	fmt.Printf("Found %d emails from data brokers\n", len(emails))
	fmt.Println()

	var responses []inbox.ClassifiedResponse
	for _, email := range emails {
		classified := inbox.ClassifyResponse(&email)
		responses = append(responses, classified)

		// A shared inbox carries replies for every profile's sent requests
		// together, so attribute this reply to whichever profile actually
		// emailed this broker rather than to whatever --profile this scan
		// happens to be running as.
		profileID, err := store.ResolveProfileForBroker(email.BrokerID)
		if err != nil {
			profileID = history.DefaultProfileID
		}

		// Store in database
		brokerResp := &history.BrokerResponse{
			ProfileID:    profileID,
			BrokerID:     email.BrokerID,
			BrokerName:   email.BrokerName,
			ResponseType: string(classified.Type),
			EmailFrom:    email.From,
			EmailSubject: email.Subject,
			FormURL:      classified.FormURL,
			ConfirmURL:   classified.ConfirmURL,
			Confidence:   classified.Confidence,
			// A reply we recognised but couldn't pin to a broker always wants
			// a human look, whatever the classifier made of its wording.
			NeedsReview: classified.NeedsReview || inbox.IsUnattributed(email.BrokerID),
			ReceivedAt:  email.ReceivedAt,
		}

		if err := store.AddBrokerResponse(brokerResp); err != nil {
			fmt.Printf("⚠️  Failed to store response: %v\n", err)
		}

		// Update pipeline status for the broker
		var pipelineStatus history.PipelineStatus
		switch classified.Type {
		case inbox.ResponseSuccess:
			pipelineStatus = history.PipelineConfirmed
		case inbox.ResponseFormRequired:
			pipelineStatus = history.PipelineFormRequired
		case inbox.ResponseConfirmationRequired:
			pipelineStatus = history.PipelineAwaitingConfirmation
		case inbox.ResponseRejected:
			pipelineStatus = history.PipelineRejected
		case inbox.ResponsePending:
			pipelineStatus = history.PipelineAwaitingResponse
		case inbox.ResponseDisclosure:
			pipelineStatus = history.PipelineDisclosureReceived
		case inbox.ResponseB2BOnly:
			// A refusal, and a permanent one - the company says it holds no
			// consumer data at all. Recorded as rejected here; acting on it
			// means tagging the broker b2b-only in brokers.yaml, which is a
			// data edit and stays a human decision (see the hint printed by
			// printClassifiedResponse).
			pipelineStatus = history.PipelineRejected
		case inbox.ResponseBounced:
			// Used to fall through to awaiting_response, so a request whose
			// address is dead sat in the pipeline looking like one still
			// waiting for an answer. It is not: nothing will ever arrive.
			//
			// Deliberately does NOT call store.MarkFailed. That clears the
			// 25-day resend cooldown, and re-sending to an address the MTA
			// just rejected is the one thing you don't want a scan to set up
			// automatically. `cleanup-bounces` and `mark-bounced` are the
			// commands that make that change, with the address cleared from
			// brokers.yaml in the same motion.
			pipelineStatus = history.PipelineFailed
		default:
			pipelineStatus = history.PipelineAwaitingResponse
		}

		// Ignore error if no matching record
		_ = store.UpdatePipelineStatus(profileID, email.BrokerID, pipelineStatus)

		// Print summary
		printClassifiedResponse(classified)
	}

	// Archive processed emails if enabled. Grouped by source mailbox, since
	// an IMAP UID is only meaningful against the folder it came from, and
	// ArchiveEmails creates the destination itself.
	if cfg.Inbox.AutoArchive && len(emails) > 0 {
		archiveFolder := cfg.Inbox.ArchiveFolder

		// A reply found in spam is only rescued when we could attribute it to
		// a broker - see inbox.ArchiveDecision. Unattributed spam is still
		// recorded above, so it shows up for review.
		movable, held := inbox.ArchivableUIDs(emails, discoveredSpam)
		if len(held) > 0 {
			fmt.Printf("🔒 Left %d message(s) in place (unattributed, found in spam) - review them in the UI\n", len(held))
		}
		validity := inbox.UIDValidityByFolder(movable)

		for srcFolder, uids := range inbox.GroupUIDsByFolder(movable) {
			if err := monitor.ArchiveEmails(uids, srcFolder, archiveFolder, validity[srcFolder]); err != nil {
				fmt.Printf("⚠️  Could not archive %d email(s) from '%s': %v\n", len(uids), srcFolder, err)
				continue
			}
			if !strings.EqualFold(srcFolder, archiveFolder) {
				fmt.Printf("📁 Archived %d emails from '%s' to '%s'\n", len(uids), srcFolder, archiveFolder)
			}
		}
	}

	// Print summary
	summary := inbox.SummarizeResponses(responses)
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("📊 Summary:")
	fmt.Printf("  Total responses:     %d\n", summary.Total)
	fmt.Printf("  ✅ Success:          %d\n", summary.Success)
	fmt.Printf("  📝 Form required:    %d\n", summary.FormRequired)
	fmt.Printf("  🔗 Confirm required: %d\n", summary.ConfirmRequired)
	fmt.Printf("  ❌ Rejected:         %d\n", summary.Rejected)
	fmt.Printf("  ⏳ Pending:          %d\n", summary.Pending)
	fmt.Printf("  ❓ Unknown:          %d\n", summary.Unknown)
	fmt.Printf("  👁️  Need review:      %d\n", summary.NeedReview)

	if once {
		return nil
	}

	// Watch for new emails if requested
	if watch {
		fmt.Println()
		fmt.Println("👀 Watching for new emails... (Ctrl+C to stop)")

		err := monitor.WatchForNewEmails(ctx, func(email inbox.Email) {
			fmt.Println()
			fmt.Printf("📨 New email from %s (%s)\n", email.BrokerName, email.From)

			classified := inbox.ClassifyResponse(&email)
			printClassifiedResponse(classified)

			profileID, err := store.ResolveProfileForBroker(email.BrokerID)
			if err != nil {
				profileID = history.DefaultProfileID
			}

			// Store response
			brokerResp := &history.BrokerResponse{
				ProfileID:    profileID,
				BrokerID:     email.BrokerID,
				BrokerName:   email.BrokerName,
				ResponseType: string(classified.Type),
				EmailFrom:    email.From,
				EmailSubject: email.Subject,
				FormURL:      classified.FormURL,
				ConfirmURL:   classified.ConfirmURL,
				Confidence:   classified.Confidence,
				NeedsReview:  classified.NeedsReview,
				ReceivedAt:   email.ReceivedAt,
			}
			if err := store.AddBrokerResponse(brokerResp); err != nil {
				fmt.Printf("⚠️  Failed to store response: %v\n", err)
			}
		})

		if err != nil && err != context.Canceled {
			return fmt.Errorf("watch error: %w", err)
		}
	}

	return nil
}

func printClassifiedResponse(r inbox.ClassifiedResponse) {
	var icon string
	switch r.Type {
	case inbox.ResponseSuccess:
		icon = "✅"
	case inbox.ResponseFormRequired:
		icon = "📝"
	case inbox.ResponseConfirmationRequired:
		icon = "🔗"
	case inbox.ResponseRejected:
		icon = "❌"
	case inbox.ResponsePending:
		icon = "⏳"
	case inbox.ResponseDisclosure:
		icon = "📄"
	case inbox.ResponseB2BOnly:
		icon = "🏢"
	case inbox.ResponseBounced:
		icon = "📭"
	default:
		icon = "❓"
	}

	fmt.Printf("%s %s - %s\n", icon, r.Email.BrokerName, r.Type)
	fmt.Printf("   Subject: %s\n", r.Email.Subject)

	if r.FormURL != "" {
		fmt.Printf("   📝 Form URL: %s\n", r.FormURL)
	}
	if r.ConfirmURL != "" {
		fmt.Printf("   🔗 Confirm URL: %s\n", r.ConfirmURL)
	}
	if r.Type == inbox.ResponseDisclosure {
		fmt.Printf("   📄 Subject access response - read this before erasing. Look for the\n")
		fmt.Printf("      source they got your data from and the recipients they sold it to.\n")
	}
	// Both of these need a data edit that this scan deliberately doesn't
	// make for you - say which command makes it, or the classification is
	// just a line of output that scrolls past.
	if r.Type == inbox.ResponseB2BOnly {
		fmt.Printf("   🏢 Says it holds no consumer data at all. If that checks out, tag it so\n")
		fmt.Printf("      nothing is ever sent again: add `tags: [b2b-only]` to %s in\n", r.Email.BrokerID)
		fmt.Printf("      data/brokers.yaml, with a note quoting the reply.\n")
	}
	if r.Type == inbox.ResponseBounced {
		fmt.Printf("   📭 Delivery failed - nothing will arrive. Run `eraser cleanup-bounces` to\n")
		fmt.Printf("      find a replacement address, or `eraser mark-bounced %s` to record it.\n", r.Email.BrokerID)
		if r.BouncedRecipient != "" {
			fmt.Printf("      Dead address: %s\n", r.BouncedRecipient)
		}
	}
	if r.NeedsReview {
		fmt.Printf("   ⚠️  Confidence: %.0f%% - manual review recommended\n", r.Confidence*100)
	}
}

// printArchivePreview lists exactly which messages a real run would move, and
// out of which mailbox, without touching anything.
//
// It prints sender and subject per message rather than a count, because a
// count is precisely what hides the failure that matters here: matching a
// broker on sender domain alone once pulled in ordinary Google and Gmail
// correspondence, and "47 broker emails" reads as fine right up until you
// notice your receipts are gone. Reading the senders is the check.
func printArchivePreview(emails []inbox.Email, cfg config.InboxConfig, spamFolder string) {
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("🔍 Dry run - nothing will be moved or recorded")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	if len(emails) == 0 {
		fmt.Println("\nNo emails from known brokers found.")
		return
	}

	for _, e := range emails {
		label := e.BrokerID
		if inbox.IsUnattributed(e.BrokerID) {
			label = "UNATTRIBUTED (" + inbox.UnattributedDomain(e.BrokerID) + ")"
		}
		fmt.Printf("\n  [%s] %s   via %s\n      from: %s\n      subj: %s\n",
			e.Folder, label, e.MatchedVia, e.From, e.Subject)
	}

	movable, held := inbox.ArchivableUIDs(emails, spamFolder)
	groups := inbox.GroupUIDsByFolder(movable)
	if len(held) > 0 {
		fmt.Printf("\n  %d message(s) would be left in place (unattributed, found in spam):\n", len(held))
		for _, e := range held {
			fmt.Printf("      %s - %s\n", e.From, e.Subject)
		}
	}

	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	for folder, uids := range groups {
		if strings.EqualFold(folder, cfg.ArchiveFolder) {
			fmt.Printf("  %-24s %3d message(s) - already filed, would not move\n", folder, len(uids))
			continue
		}
		fmt.Printf("  %-24s %3d message(s) would move to '%s'\n", folder, len(uids), cfg.ArchiveFolder)
	}

	if !cfg.AutoArchive {
		fmt.Println()
		fmt.Println("  auto_archive is off, so a real run would not move anything either.")
		fmt.Println("  Set inbox.auto_archive: true in config.yaml to enable moving.")
	}
	fmt.Println()
	fmt.Println("  Check the senders above before enabling. Anything that isn't a")
	fmt.Println("  reply from a broker you wrote to should be reported, not archived.")
}
