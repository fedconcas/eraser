package main

import (
	"bufio"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/eraser-privacy/eraser/internal/broker"
	"github.com/spf13/cobra"
)

func listBrokersCmd() *cobra.Command {
	var region, category, search, priority, tag string
	var missingEmail bool

	cmd := &cobra.Command{
		Use:   "list-brokers",
		Short: "List all data brokers in the database",
		Long:  "Show all data brokers that will receive removal requests.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runListBrokers(region, category, priority, search, tag, missingEmail)
		},
	}

	cmd.Flags().StringVar(&region, "region", "", "Only show brokers in this region (us, eu, or global)")
	cmd.Flags().StringVar(&category, "category", "", "Only show brokers in this category")
	cmd.Flags().StringVar(&priority, "priority", "", "Only show brokers with this priority (high, medium, or low)")
	cmd.Flags().StringVar(&search, "search", "", "Only show brokers whose name or ID contains this text")
	cmd.Flags().StringVar(&tag, "tag", "", fmt.Sprintf("Only show brokers carrying this disposition tag (%s)", strings.Join(broker.DispositionTags, ", ")))
	cmd.Flags().BoolVar(&missingEmail, "missing-email", false, "Only show brokers with no email on file (need manual follow-up)")

	return cmd
}

func addBrokerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add-broker",
		Short: "Add a new data broker to the database",
		Long:  "Interactively add a new data broker to the local broker database.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAddBroker()
		},
	}
}

func runListBrokers(region, category, priority, search, tag string, missingEmail bool) error {
	brokerDB, err := broker.LoadFromFile(resolveBrokerPath())
	if err != nil {
		return fmt.Errorf("failed to load brokers: %w", err)
	}

	region = strings.ToLower(strings.TrimSpace(region))
	category = strings.ToLower(strings.TrimSpace(category))
	search = strings.ToLower(strings.TrimSpace(search))

	// An unknown --tag would match nothing and read as "no such broker" rather
	// than "no such tag", the same trap --priority already guards against.
	if tag = strings.ToLower(strings.TrimSpace(tag)); tag != "" && !slices.Contains(broker.DispositionTags, tag) {
		return fmt.Errorf("invalid --tag: must be one of %s", strings.Join(broker.DispositionTags, ", "))
	}

	// An unrecognized --priority normalizes to "", which would silently mean
	// "no priority filter" and list everything - tell the user instead.
	if strings.TrimSpace(priority) != "" {
		priority = broker.NormalizePriority(priority)
		if priority == "" {
			return fmt.Errorf("invalid --priority: must be one of %s", strings.Join(broker.Priorities, ", "))
		}
	}

	matched := make([]broker.Broker, 0, len(brokerDB.Brokers))
	for _, b := range brokerDB.Brokers {
		if region != "" && strings.ToLower(b.Region) != region {
			continue
		}
		if category != "" && strings.ToLower(b.Category) != category {
			continue
		}
		if priority != "" && broker.NormalizePriority(b.Priority) != priority {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(b.Name), search) && !strings.Contains(strings.ToLower(b.ID), search) {
			continue
		}
		if missingEmail && b.Email != "" {
			continue
		}
		if tag != "" && !b.HasTag(tag) {
			continue
		}
		matched = append(matched, b)
	}

	if region != "" || category != "" || priority != "" || search != "" || tag != "" || missingEmail {
		fmt.Printf("📋 Data Brokers (%d of %d total match your filters)\n", len(matched), len(brokerDB.Brokers))
	} else {
		fmt.Printf("📋 Data Brokers (%d total)\n", len(matched))
	}
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	for _, b := range matched {
		fmt.Printf("\n%s [%s]\n", b.Name, b.ID)
		if b.Email != "" {
			fmt.Printf("  📧 %s\n", b.Email)
		} else {
			fmt.Printf("  📧 (none - needs manual follow-up)\n")
		}
		if b.Website != "" {
			fmt.Printf("  🌐 %s\n", b.Website)
		}
		if b.OptOutURL != "" {
			fmt.Printf("  🔗 Opt-out: %s\n", b.OptOutURL)
		}
		fmt.Printf("  🌍 Region: %s\n", b.Region)
		if b.Category != "" {
			fmt.Printf("  📁 Category: %s\n", b.Category)
		}
		if b.Priority != "" {
			fmt.Printf("  ⭐ Priority: %s\n", b.Priority)
		}
		// Only spelled out for a tag-driven block - the "no email on file"
		// case is already stated on the address line above.
		if len(b.Tags) > 0 {
			fmt.Printf("  🏷️  %s (%s)\n", strings.Join(b.Tags, ", "), b.NotSendableReason())
		}
	}

	return nil
}

func runAddBroker() error {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("➕ Add New Data Broker")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	b := broker.Broker{}

	b.Name = prompt(reader, "Broker name: ")
	b.ID = strings.ToLower(strings.ReplaceAll(b.Name, " ", "-"))
	b.Email = prompt(reader, "Privacy/removal email: ")
	b.Website = prompt(reader, "Website (optional): ")
	b.OptOutURL = prompt(reader, "Opt-out URL (optional): ")
	b.Region = prompt(reader, "Region (us/eu/global): ")
	b.Category = prompt(reader, "Category (people-search/marketing/background-check/financial-b2b/data-intermediary/device-id-only/requires-id/non-broker): ")
	b.Priority = broker.NormalizePriority(prompt(reader, "Priority (high/medium/low, optional): "))

	// Load existing brokers
	brokerPath := brokerFile
	if brokerPath == "" {
		brokerPath = "data/brokers.yaml"
	}

	var brokerDB *broker.BrokerDatabase
	if _, err := os.Stat(brokerPath); os.IsNotExist(err) {
		brokerDB = &broker.BrokerDatabase{}
	} else {
		var err error
		brokerDB, err = broker.LoadFromFile(brokerPath)
		if err != nil {
			return fmt.Errorf("failed to load brokers: %w", err)
		}
	}

	if err := brokerDB.Add(b); err != nil {
		return err
	}

	if err := brokerDB.Save(brokerPath); err != nil {
		return fmt.Errorf("failed to save brokers: %w", err)
	}

	fmt.Println()
	fmt.Printf("✅ Added %s to broker database\n", b.Name)

	return nil
}
