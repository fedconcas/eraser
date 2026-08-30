package main

import (
	"strings"
	"testing"
)

// The `init` prompts show the current value as the default and re-split
// whatever comes back, so each list field's join separator and its split
// separator have to agree. They didn't for name_variants: it joined with ", "
// and split on ",", which meant a variant containing a comma - enterable from
// the web UI, which splits that field on line breaks only - was silently torn
// into two the next time someone ran `init` and pressed Enter to keep it.
func TestInitListPromptsRoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		sep   string // separator used to join the prompt default
		split string // separator the answer is split on
		items []string
	}{
		{
			name:  "name variants with a comma inside one entry",
			sep:   "; ",
			split: ";",
			items: []string{"Smith, John", "Maris Popens"},
		},
		{
			name:  "previous addresses with commas inside every entry",
			sep:   "; ",
			split: ";",
			items: []string{"123 Main St, San Francisco, CA 94102", "45 Old Road, Riga, LV-1005"},
		},
		{
			name:  "additional emails",
			sep:   ", ",
			split: ",",
			items: []string{"old@example.com", "work@company.com"},
		},
		{
			name:  "additional phones",
			sep:   ", ",
			split: ",",
			items: []string{"+371 20111111", "+1 555 0000"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// What promptWithDefault would display, then accept unchanged
			// when the user just presses Enter.
			shown := strings.Join(tt.items, tt.sep)
			got := splitAndTrimBy(shown, tt.split)

			if len(got) != len(tt.items) {
				t.Fatalf("round trip changed the entry count: got %d %+v, want %d %+v",
					len(got), got, len(tt.items), tt.items)
			}
			for i := range tt.items {
				if got[i] != tt.items[i] {
					t.Errorf("entry %d: got %q, want %q", i, got[i], tt.items[i])
				}
			}
		})
	}
}
