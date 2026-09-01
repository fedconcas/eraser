package web

import (
	"html/template"
	"log"
	"strings"

	"github.com/eraser-privacy/eraser/internal/broker"
	"github.com/eraser-privacy/eraser/internal/inbox"
)

// bounceFinding is one proven-dead address spotted during an inbox scan.
type bounceFinding struct {
	BrokerID string // may be empty: an NDR is often not attributable to a broker
	Address  string
	Evidence string
}

// collectBounce turns one classified reply into a bounce finding, or reports
// false when there is nothing safe to act on.
//
// Two gates, both required. The classifier's ResponseBounced says "this is a
// delivery failure"; inbox.IsHardBounce says "and the address is permanently
// dead", which is the only kind that may clear a broker's contact details -
// a full mailbox or a greylisting deferral must never cost a live broker its
// address. The recipient also has to be extractable: an NDR we cannot pin to
// an address tells us nothing about which broker to touch.
func collectBounce(classified inbox.ClassifiedResponse) (bounceFinding, bool) {
	if classified.Type != inbox.ResponseBounced {
		return bounceFinding{}, false
	}
	addr := strings.TrimSpace(classified.BouncedRecipient)
	if addr == "" || !inbox.IsHardBounce(classified.Email) {
		return bounceFinding{}, false
	}
	f := bounceFinding{Address: addr, Evidence: inbox.BounceEvidence(classified.Email)}
	if classified.Email != nil {
		f.BrokerID = classified.Email.BrokerID
	}
	return f, true
}

// applyBounceFindings clears the addresses in findings from the broker
// database, so the list heals itself from the user's own inbox instead of
// waiting for a hand-run cleanup-bounces or a new release of the shipped
// data file. Returns the names of the brokers whose address was dropped.
//
// One mutateBrokers call for the whole batch: that is one file write and one
// published snapshot per scan, not one per bounce.
//
// The broker is matched by the bounced address itself, not by the reply's
// BrokerID - an NDR comes from the user's own mail system, so its From
// header attributes to nothing, and the address inside it is the reliable
// join key. The BrokerID, when the reply matcher did manage to set one, is
// only used as a fallback for a broker whose stored address has since been
// edited to a different spelling.
func (s *Server) applyBounceFindings(findings []bounceFinding) []string {
	if len(findings) == 0 {
		return nil
	}

	var cleared []string
	err := s.mutateBrokers(func(db *broker.BrokerDatabase) (bool, error) {
		for _, f := range findings {
			b := db.FindByEmail(f.Address)
			if b == nil && f.BrokerID != "" {
				b = db.FindByID(f.BrokerID)
			}
			if b == nil {
				// A bounce from an address this database never carried:
				// a reply to something else the user sent, or an address
				// already cleared by an earlier scan.
				continue
			}
			if broker.MarkEmailUnreachable(b, f.Address, f.Evidence) {
				cleared = append(cleared, b.Name)
			}
		}
		return len(cleared) > 0, nil
	})
	if err != nil {
		// The scan itself succeeded and its results are already stored;
		// failing to prune the broker list is worth a log line, not a failed
		// scan. The addresses stay put and the next scan tries again.
		log.Printf("Warning: failed to clear %d bounced address(es) from the broker list: %v", len(findings), err)
		return nil
	}
	for _, name := range cleared {
		log.Printf("Cleared an undeliverable address from %s (bounce seen during an inbox scan)", name)
	}
	return cleared
}

// bounceSummaryHTML renders the line the scan summary shows for addresses it
// pruned. Empty when nothing changed, so the summary stays quiet on the
// ordinary scan that found no dead addresses.
func bounceSummaryHTML(cleared []string) string {
	if len(cleared) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(`<p class="mt-2 text-sm">📮 Removed `)
	if len(cleared) == 1 {
		sb.WriteString("an undeliverable address from <strong>")
		sb.WriteString(template.HTMLEscapeString(cleared[0]))
		sb.WriteString("</strong>")
	} else {
		sb.WriteString("undeliverable addresses from <strong>")
		sb.WriteString(template.HTMLEscapeString(strings.Join(cleared, ", ")))
		sb.WriteString("</strong>")
	}
	sb.WriteString(` - the broker stays in your list under "Include non-sendable" and can be given a new address.</p>`)
	return sb.String()
}
