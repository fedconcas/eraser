package main

import (
	"context"
	"fmt"

	"github.com/eraser-privacy/eraser/internal/broker"
	"github.com/eraser-privacy/eraser/internal/config"
	"github.com/eraser-privacy/eraser/internal/history"
	"github.com/eraser-privacy/eraser/internal/inbox"
	"github.com/spf13/cobra"
)

func cleanupBouncesCmd() *cobra.Command {
	var (
		remove bool
		days   int
	)

	cmd := &cobra.Command{
		Use:   "cleanup-bounces",
		Short: "Find and clear broker email addresses that bounced",
		Long: `Scan your inbox for bounced/undeliverable emails and identify broker
email addresses that no longer accept mail.

By default this only reports. With --remove, the dead ADDRESS is cleared -
the broker entry itself is kept, with the dropped address and the bounce
wording recorded in its notes. It keeps its name, category, website and
opt-out URL, still shows up in 'list-brokers --missing-email' and under
"Include non-sendable" in the web UI, and can be given a working address
later. (Deleting the whole company, which this used to do, threw all of
that away and silently shrank the shipped database.)

Only permanent failures are acted on. A full mailbox, a greylisting
deferral or any other temporary failure is reported and left alone - the
address is fine, the message just did not get through this time. The web
UI's inbox scan applies the same rule automatically.

Examples:
  eraser cleanup-bounces                 # Show bounced addresses (dry run)
  eraser cleanup-bounces --remove        # Clear the dead addresses
  eraser cleanup-bounces --days 30       # Look back 30 days`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCleanupBounces(remove, days)
		},
	}

	cmd.Flags().BoolVar(&remove, "remove", false, "Clear the dead addresses (the broker entries themselves are kept)")
	cmd.Flags().IntVar(&days, "days", 30, "Number of days to scan for bounced emails")

	return cmd
}

func runCleanupBounces(remove bool, days int) error {
	// Load config
	cfg, err := config.Load(resolveConfigPath())
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Check if inbox is configured
	if !cfg.Inbox.Enabled {
		return fmt.Errorf("inbox monitoring not configured. Run 'eraser init' to set up")
	}

	// Load broker database
	brokerPath := resolveBrokerPath()
	brokerDB, err := broker.LoadFromFile(brokerPath)
	if err != nil {
		return fmt.Errorf("failed to load brokers: %w", err)
	}

	fmt.Println("🔍 Scanning inbox for bounced emails...")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// Create inbox monitor
	monitor := inbox.NewMonitor(cfg.Inbox, brokerDB.Brokers)

	// Connect
	ctx := context.Background()
	if err := monitor.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect to inbox: %w", err)
	}
	defer func() { _ = monitor.Disconnect() }()

	// Fetch bounce emails
	bounceEmails, err := monitor.FetchBounceEmails(ctx, days)
	if err != nil {
		return fmt.Errorf("failed to fetch bounce emails: %w", err)
	}

	if len(bounceEmails) == 0 {
		fmt.Println("✓ No bounced emails found!")
		return nil
	}

	fmt.Printf("Found %d bounced email(s):\n\n", len(bounceEmails))

	// Track the addresses worth acting on, and the ones deliberately left
	// alone, so the summary can say why.
	type bouncedBroker struct {
		email    string
		broker   *broker.Broker
		evidence string
	}
	var dead []bouncedBroker
	var transient int

	for _, email := range bounceEmails {
		// Extract the bounced recipient
		bouncedRecipient := inbox.ExtractBouncedRecipient(&email)
		if bouncedRecipient == "" {
			fmt.Printf("⚠️  Could not extract bounced address from: %s\n", email.Subject)
			continue
		}

		// Find the broker
		b := brokerDB.FindByEmail(bouncedRecipient)
		if b == nil {
			fmt.Printf("⚠️  %s - not found in broker database\n", bouncedRecipient)
			continue
		}

		// A temporary failure says nothing about the address. Clearing one
		// would drop a live broker out of every future send with nothing to
		// put it back, so it is reported and skipped - same rule the web
		// UI's scan applies (see internal/inbox/bounce.go).
		if !inbox.IsHardBounce(&email) {
			transient++
			fmt.Printf("⏳ %s\n", bouncedRecipient)
			fmt.Printf("   Broker: %s (%s)\n", b.Name, b.ID)
			fmt.Printf("   Temporary failure - address kept: %s\n", truncateString(email.Subject, 60))
			fmt.Println()
			continue
		}

		fmt.Printf("❌ %s\n", bouncedRecipient)
		fmt.Printf("   Broker: %s (%s)\n", b.Name, b.ID)
		fmt.Printf("   Subject: %s\n", truncateString(email.Subject, 60))
		fmt.Printf("   Date: %s\n", email.ReceivedAt.Format("2006-01-02"))
		fmt.Println()

		dead = append(dead, bouncedBroker{
			email:    bouncedRecipient,
			broker:   b,
			evidence: inbox.BounceEvidence(&email),
		})
	}

	if len(dead) == 0 {
		if transient > 0 {
			fmt.Printf("✓ No permanently dead addresses (%d temporary failure(s) left alone)\n", transient)
			return nil
		}
		fmt.Println("✓ No broker email addresses need to be cleared")
		return nil
	}

	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	if !remove {
		fmt.Printf("\n📊 Found %d broker(s) whose address no longer accepts mail\n", len(dead))
		fmt.Println("Run with --remove to clear those addresses (the brokers themselves are kept)")
		return nil
	}

	fmt.Printf("\n📮 Clearing %d dead address(es)...\n\n", len(dead))

	cleared := 0
	for _, bb := range dead {
		// Clears the address and records it, with the bounce wording, in the
		// broker's notes - the same helper the web UI's scan uses.
		if broker.MarkEmailUnreachable(bb.broker, bb.email, bb.evidence) {
			fmt.Printf("✓ Cleared %s from %s\n", bb.email, bb.broker.Name)
			cleared++
		}
	}

	if cleared == 0 {
		fmt.Println("Nothing to change - those addresses were already cleared")
		return nil
	}

	// Save with backup
	if err := brokerDB.SaveWithBackup(brokerPath); err != nil {
		return fmt.Errorf("failed to save broker database: %w", err)
	}

	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("✓ Cleared %d dead address(es); the broker entries were kept\n", cleared)
	fmt.Println("  See them with: eraser list-brokers --missing-email")
	fmt.Printf("  Backup saved to: %s.bak\n", brokerPath)

	return nil
}

func markBouncedCmd() *cobra.Command {
	var note string

	cmd := &cobra.Command{
		Use:   "mark-bounced <broker-id> [<broker-id>...]",
		Short: "Correct the record for broker(s) whose email actually bounced",
		Long: `'send' records a broker as sent the moment your SMTP provider accepts
the message for delivery - that's the only signal a normal send gets. A
bounce is a separate email that arrives later (sometimes minutes, sometimes
longer), and without inbox monitoring configured, nothing links it back to
that history record automatically.

If you've spotted a bounce yourself - by reading your inbox, or now that a
Gmail connector lets an assistant read it for you - use this to correct the
record once you're done acting on it (e.g. after fixing the broker's contact
info in data/brokers.yaml). It flips that broker's most recent "sent" record
to "failed", which:

  - removes it from the 25-day resend cooldown, so the next 'eraser send'
    retries it automatically (no need for --resend)
  - makes 'eraser status' reflect what actually happened, instead of
    claiming a delivery that didn't succeed

This only corrects history - it doesn't resend anything by itself.

Example:
  eraser mark-bounced crawlbee ivy-tech-re jverify --note "contact fixed 2026-08-20"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMarkBounced(args, note)
		},
	}

	cmd.Flags().StringVar(&note, "note", "", "Optional note recorded with the correction (why/when it bounced)")

	return cmd
}

func runMarkBounced(brokerIDs []string, note string) error {
	cfg, err := config.Load(resolveConfigPath())
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	activeProfile, err := resolveProfile(cfg)
	if err != nil {
		return err
	}

	store, err := history.NewStore(history.DBPathFor(resolveConfigPath()))
	if err != nil {
		return fmt.Errorf("failed to initialize history: %w", err)
	}
	defer func() { _ = store.Close() }()

	errMsg := "bounced - manually confirmed"
	if note != "" {
		errMsg = errMsg + ": " + note
	}

	updated, skipped := 0, 0
	for _, id := range brokerIDs {
		n, err := store.MarkFailed(activeProfile.ID, id, errMsg)
		if err != nil {
			return fmt.Errorf("failed to mark %s: %w", id, err)
		}
		if n == 0 {
			fmt.Printf("⚠️  %s - no \"sent\" record found to correct (never sent, or already marked failed)\n", id)
			skipped++
			continue
		}
		fmt.Printf("✓ %s - marked failed, will be retried on next 'eraser send'\n", id)
		updated++
	}

	fmt.Println()
	if skipped > 0 {
		fmt.Printf("Updated %d broker(s), %d skipped.\n", updated, skipped)
	} else {
		fmt.Printf("Updated %d broker(s).\n", updated)
	}
	return nil
}

// truncateString truncates a string to the specified length
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
