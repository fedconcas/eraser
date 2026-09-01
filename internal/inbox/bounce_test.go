package inbox

import "testing"

// TestIsHardBounce pins the asymmetry the auto-clear depends on: a
// permanent failure must be stated, and any transient signal vetoes it. A
// false positive here silently drops a live broker out of every future send.
func TestIsHardBounce(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		body    string
		want    bool
	}{
		{
			name:    "enhanced status code 5.1.1",
			subject: "Undeliverable: Erasure request",
			body:    "Your message could not be delivered.\nStatus: 5.1.1\nDiagnostic-Code: smtp; 550 unknown",
			want:    true,
		},
		{
			name:    "550 user unknown",
			subject: "Mail delivery failed",
			body:    "550 5.1.1 <privacy@gone.example>: Recipient address rejected: User unknown",
			want:    true,
		},
		{
			name:    "wording only, no code",
			subject: "Returned mail",
			body:    "The account you tried to reach does not exist.",
			want:    true,
		},
		{
			name:    "domain does not exist",
			subject: "Delivery Status Notification (Failure)",
			body:    "DNS Error: the domain does not exist",
			want:    true,
		},
		{
			name:    "mailbox full is soft",
			subject: "Undeliverable: Erasure request",
			body:    "552 Requested mail action aborted: mailbox full. The mailbox is over quota.",
			want:    false,
		},
		{
			name:    "greylisted is soft",
			subject: "Delivery delayed",
			body:    "451 4.7.1 Greylisted, try again later",
			want:    false,
		},
		{
			name:    "temporary failure with a 5xx elsewhere is soft",
			subject: "Delivery Status Notification (Delay)",
			body:    "Status: 4.4.1\nThis is a temporary failure; the server will retry. (ref 550)",
			want:    false,
		},
		{
			name:    "delayed, will retry",
			subject: "Warning: message delayed 24 hours",
			body:    "Delivery is delayed and will be retried for another 48 hours.",
			want:    false,
		},
		{
			name:    "vague failure without permanence is not hard",
			subject: "Mail delivery failed",
			body:    "Your message was not delivered.",
			want:    false,
		},
		{
			name:    "quota wording alongside user unknown still vetoes",
			subject: "Undeliverable",
			body:    "user unknown for one recipient; another mailbox is full",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsHardBounce(&Email{Subject: tt.subject, Body: tt.body})
			if got != tt.want {
				t.Errorf("IsHardBounce() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsHardBounceNilEmail(t *testing.T) {
	if IsHardBounce(nil) {
		t.Error("a nil email must never read as a hard bounce")
	}
}

func TestBounceEvidenceQuotesTheFailingLine(t *testing.T) {
	e := &Email{
		Subject: "Undeliverable: Erasure request",
		Body:    "This is the mail system at example.com.\n550 5.1.1 <privacy@gone.example>: User unknown\nSorry.",
	}
	got := BounceEvidence(e)
	want := "550 5.1.1 <privacy@gone.example>: User unknown"
	if got != want {
		t.Errorf("BounceEvidence() = %q, want %q", got, want)
	}
}

func TestBounceEvidenceFallsBackToSubject(t *testing.T) {
	e := &Email{Subject: "Returned mail: see transcript", Body: "The account you tried to reach does not exist."}
	if got := BounceEvidence(e); got == "" {
		t.Error("BounceEvidence returned nothing for an email with usable wording")
	}
}
