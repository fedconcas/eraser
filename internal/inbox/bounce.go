package inbox

import (
	"regexp"
	"strings"
)

// Hard vs soft bounces.
//
// ClassifyResponse already recognises a delivery failure (ResponseBounced),
// but it deliberately does not care *why* delivery failed - a full mailbox,
// a greylisting deferral and a nonexistent address all read as "bounced" for
// the purpose of flagging the reply. That is fine for a record a human then
// reads; it is not enough to act on automatically.
//
// The web UI's scan clears a broker's address when it proves undeliverable,
// so it needs the stricter question: does this notification say the address
// will *never* accept mail again? A "mailbox full" that cleared an address
// would silently drop a live broker out of every future send, and nothing
// would ever put it back. So the rule here is deliberately asymmetric:
// permanence must be stated (a 5.x.x enhanced status code, a 5xx reply code,
// or wording that names the recipient as unknown), and any transient signal
// anywhere in the message vetoes it.

var (
	// hardBounceStatus matches an RFC 3463 enhanced status code in the
	// permanent class (5.x.x) - the most reliable signal in a DSN, and the
	// one machine-readable field an NDR is required to carry.
	hardBounceStatus = regexp.MustCompile(`(?i)status:?\s*5\.\d{1,3}\.\d{1,3}\b`)

	// hardBounceReply matches an SMTP reply code in the permanent range,
	// as quoted in the human-readable part of most NDRs.
	hardBounceReply = regexp.MustCompile(`\b5(?:50|51|52|53|54|58|59)\b`)

	// hardBounceWording is the fallback for NDRs that quote no code at all:
	// phrasings that name the *recipient* as the permanent problem.
	hardBounceWording = []*regexp.Regexp{
		regexp.MustCompile(`(?i)user\s+unknown`),
		regexp.MustCompile(`(?i)no\s+such\s+(user|recipient|address|mailbox)`),
		// Allows a few words between the noun and the verb ("the account
		// you tried to reach does not exist"), but never a sentence break.
		regexp.MustCompile(`(?i)(mailbox|recipient|address|account)\b[^.\n]{0,48}?(does\s+not|doesn'?t)\s+exist`),
		regexp.MustCompile(`(?i)(recipient|address|mailbox|account)\s+(is\s+)?(unknown|invalid|not\s+found|disabled|deactivated)`),
		regexp.MustCompile(`(?i)invalid\s+(recipient|address|mailbox)`),
		regexp.MustCompile(`(?i)unknown\s+(recipient|user|address)`),
		regexp.MustCompile(`(?i)address\s+(rejected|not\s+found)`),
		regexp.MustCompile(`(?i)permanent(ly)?\s+(failure|error|rejected|unavailable)`),
		regexp.MustCompile(`(?i)domain\s+(does\s+not|doesn'?t)\s+exist`),
	}

	// softBounceSignals veto a hard verdict wherever they appear. These are
	// the failures that resolve themselves: the address is fine, the message
	// just did not get through this time.
	softBounceSignals = []*regexp.Regexp{
		regexp.MustCompile(`(?i)status:?\s*4\.\d{1,3}\.\d{1,3}\b`),
		regexp.MustCompile(`\b4(?:21|22|31|32|41|50|51|52)\b`),
		regexp.MustCompile(`(?i)(mailbox|quota|account)\s+(is\s+)?(full|over\s+quota|exceeded)`),
		regexp.MustCompile(`(?i)over\s+quota`),
		regexp.MustCompile(`(?i)insufficient\s+(system\s+)?storage`),
		regexp.MustCompile(`(?i)tempor(ary|arily)`),
		regexp.MustCompile(`(?i)tempfail`),
		regexp.MustCompile(`(?i)greylist`),
		regexp.MustCompile(`(?i)deferred`),
		regexp.MustCompile(`(?i)will\s+(be\s+)?retr(y|ied)`),
		regexp.MustCompile(`(?i)try\s+again\s+later`),
		regexp.MustCompile(`(?i)delivery\s+(is\s+)?delayed`),
		regexp.MustCompile(`(?i)not\s+yet\s+been\s+delivered`),
		regexp.MustCompile(`(?i)rate\s+limit`),
		regexp.MustCompile(`(?i)too\s+many\s+(connections|messages)`),
		regexp.MustCompile(`(?i)service\s+(is\s+)?unavailable`),
	}
)

// IsHardBounce reports whether email is a delivery failure that says the
// recipient address is permanently unusable - the only kind safe to act on
// by clearing a broker's address.
//
// It answers "false" for anything inconclusive, including every soft or
// transient failure, and does not itself check that the email is a bounce at
// all: callers pair it with ClassifyResponse's ResponseBounced verdict.
func IsHardBounce(email *Email) bool {
	if email == nil {
		return false
	}
	content := email.Body
	if email.HTMLBody != "" {
		content += " " + stripHTMLSimple(email.HTMLBody)
	}
	content += " " + email.Subject

	// A transient signal anywhere wins. A DSN that reports both a permanent
	// and a temporary failure is reporting on more than one recipient, and
	// there is no way from here to tell which one was ours.
	for _, soft := range softBounceSignals {
		if soft.MatchString(content) {
			return false
		}
	}

	if hardBounceStatus.MatchString(content) || hardBounceReply.MatchString(content) {
		return true
	}
	for _, hard := range hardBounceWording {
		if hard.MatchString(content) {
			return true
		}
	}
	return false
}

// BounceEvidence returns a one-line quote of the wording that made this a
// hard bounce, for the audit note left on the broker. Falls back to the
// subject when no single line stands out.
func BounceEvidence(email *Email) string {
	if email == nil {
		return ""
	}
	content := email.Body
	if content == "" && email.HTMLBody != "" {
		content = stripHTMLSimple(email.HTMLBody)
	}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || len(line) > 160 {
			continue
		}
		if hardBounceStatus.MatchString(line) || hardBounceReply.MatchString(line) {
			return line
		}
		for _, hard := range hardBounceWording {
			if hard.MatchString(line) {
				return line
			}
		}
	}
	return strings.TrimSpace(email.Subject)
}
