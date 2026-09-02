package inbox

import (
	"fmt"
	"strings"
)

// Recognising a reply that arrives from somewhere we never wrote to.
//
// Brokers routinely answer through a helpdesk tenant or a parent company:
// AnyWho replies from anywho.zendesk.com, Experian from
// experian-marketing-services.atlassian.net, MyLife from
// mylifecs.atlassian.net. Matching on the sender's domain alone cannot see
// any of those, and on a live account 8 of 74 genuine replies were being
// missed for exactly this reason.
//
// The signal used instead is the request subject. Every request this tool
// sends carries one of a small set of fixed subject lines, and a reply quotes
// it. That is stronger evidence than a domain match, not weaker: the phrase is
// long, specific to a legal instrument, and issued by us.
//
// Two things it is *not* allowed to become:
//
//   - A way around the sender denylist. Subject text is chosen by the sender,
//     so on its own it would re-admit exactly the class of mail the denylist
//     exists to reject - including our own sent requests, which quote the
//     subject by definition. Every check below still runs first.
//   - A way to move mail we cannot identify out of the spam folder. See
//     ArchiveDecision.

// ReplyMatch is the outcome of testing one message against everything we know
// about the requests we've sent.
type ReplyMatch struct {
	// BrokerID is the broker this reply belongs to, or "" if the message
	// isn't a reply to us at all.
	BrokerID string
	// Attributed reports whether BrokerID names a real broker. When false but
	// BrokerID is set, the message is a genuine reply we couldn't pin to a
	// broker, and BrokerID holds a per-sender placeholder (see
	// unattributedID).
	Attributed bool
	// How the match was made, for logging and the dry-run preview.
	Via string
}

// Matched reports whether the message is a reply to one of our requests.
func (m ReplyMatch) Matched() bool { return m.BrokerID != "" }

// helpdeskSuffixes are shared ticketing hosts. A broker replying through one
// gets its own tenant, so the *leftmost* label is the broker hint:
// anywho.zendesk.com means AnyWho, not Zendesk. Note the direction - reducing
// such a domain to its registrable form would yield zendesk.com, which is
// never the broker and must never be matched.
var helpdeskSuffixes = []string{
	".zendesk.com",
	".atlassian.net",
	".freshdesk.com",
	".freshservice.com",
	".helpscoutapp.com",
	".intercom-mail.com",
	".zohodesk.com",
	".happyfox.com",
	".kayako.com",
	".groovehq.com",
	".helpshift.com",
}

// helpdeskTenant returns the tenant label of a shared ticketing host, and
// whether the domain is one.
func helpdeskTenant(domain string) (string, bool) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	for _, suffix := range helpdeskSuffixes {
		if strings.HasSuffix(domain, suffix) {
			tenant := strings.TrimSuffix(domain, suffix)
			// Only the leftmost label: "eu.acme.zendesk.com" -> "acme" is
			// wrong, take the whole remainder and let the caller match it
			// exactly rather than guessing at regional prefixes.
			return tenant, tenant != ""
		}
	}
	return "", false
}

// privacyPlatformSuffixes are shared sending domains used by third-party
// privacy-request platforms - a service many different companies delegate
// their whole request/response flow to. Unlike a helpdesk tenant, the domain
// here doesn't vary per client: OneTrust sends every client's notifications
// from the same noreply@m.onetrust.com, so the domain says nothing about
// which broker this is. The client is only named in the display name -
// sometimes the company's brand ("On behalf of PropStream"), sometimes a
// parent company's, sometimes literally the address they gave OneTrust as
// their own contact ("privacy@lightcast.io") - so brokerFromSenderDisplayName
// matches on that instead.
var privacyPlatformSuffixes = []string{
	"onetrust.com",
}

func isPrivacyPlatformDomain(domain string) bool {
	domain = strings.ToLower(strings.TrimSpace(domain))
	for _, apex := range privacyPlatformSuffixes {
		if domain == apex || strings.HasSuffix(domain, "."+apex) {
			return true
		}
	}
	return false
}

// domainRoot returns the first label of a domain, normalized the same way a
// helpdesk tenant slug is - "propstream" for "propstream.com", "lightcast"
// for "lightcast.io" - so both compare on equal footing.
func domainRoot(domain string) string {
	root, _, _ := strings.Cut(domain, ".")
	return normalizeSlug(root)
}

// brokerFromSenderDisplayName resolves a broker from the sender's display
// name, for messages routed through a shared privacy-request platform whose
// domain never identifies the client (see privacyPlatformSuffixes).
//
// Matches on the registrable root of a contacted broker's own domain
// appearing in the display name - not the broker's ID or Name, which is
// often not what the platform shows: a broker recorded in the database as
// "Equimine" but contacted at propstream.com gets a notification whose
// display name says only "On behalf of PropStream". A minimum root length
// and a uniqueness requirement guard against a short or generic root
// matching more than one contacted broker.
//
// A broker's ReplyNames are checked the same way, for the case a domain root
// can never cover: the display name isn't the client's own brand at all, but
// a parent company's ("GBG Privacy and Data Compliance Team" for Acuant,
// Inc., a GBG subsidiary) - a fact nothing else in the record captures, so it
// has to be recorded explicitly.
func (m *Monitor) brokerFromSenderDisplayName(email *Email) (string, bool) {
	if !isPrivacyPlatformDomain(email.FromDomain) {
		return "", false
	}
	name := normalizeSlug(email.FromName)
	if name == "" {
		return "", false
	}

	found := make(map[string]bool, 2)
	for domain, b := range m.brokers {
		if m.contacted != nil && !m.contacted[b.ID] {
			continue
		}
		if root := domainRoot(domain); len(root) >= 5 && strings.Contains(name, root) {
			found[b.ID] = true
			continue
		}
		for _, alias := range b.ReplyNames {
			if slug := normalizeSlug(alias); len(slug) >= 3 && strings.Contains(name, slug) {
				found[b.ID] = true
				break
			}
		}
	}
	if len(found) != 1 {
		return "", false
	}
	for id := range found {
		return id, true
	}
	return "", false
}

// unattributedID is the broker ID recorded for a genuine reply we can't pin to
// a broker.
//
// It embeds the sender's domain rather than being a single shared constant,
// because several places key on broker ID to tell responses apart:
// FindBrokerResponseBySubject dedupes on (profile, broker, subject) and the
// forms list dedupes on broker alone. Since every request carries an identical
// subject, one shared sentinel would collapse every unattributed reply into a
// single row and discard the rest. Per-domain IDs keep them distinct.
func unattributedID(fromDomain string) string {
	d := strings.ToLower(strings.TrimSpace(fromDomain))
	if d == "" {
		d = "unknown"
	}
	return "unattributed:" + d
}

// IsUnattributed reports whether a broker ID is one of the placeholders above.
func IsUnattributed(brokerID string) bool {
	return strings.HasPrefix(brokerID, "unattributed:")
}

// UnattributedDomain returns the sender domain embedded in a placeholder ID.
func UnattributedDomain(brokerID string) string {
	return strings.TrimPrefix(brokerID, "unattributed:")
}

// UnattributedName is the display name shown for a reply we couldn't attribute.
func UnattributedName(brokerID string) string {
	if d := UnattributedDomain(brokerID); d != "" && d != brokerID {
		return fmt.Sprintf("Unidentified sender (%s)", d)
	}
	return "Unidentified sender"
}

// matchReply decides whether a message is a reply to one of our requests, and
// to which broker.
//
// The guards run before anything else and are the reason this can't undo the
// sender-domain filtering:
//
//   - a general-purpose sender domain is never a broker reply, whatever it
//     claims. This is what stops our own request - sent from the user's own
//     free-mail address - from being read as a reply to itself.
//   - our own address is never a reply to us, whatever domain it's on. The
//     monitored folder is configurable and may include sent mail.
//   - a bounce is not a reply. Delivery reports quote the original subject, and
//     `eraser cleanup-bounces` reads them out of the inbox to correct dead
//     broker addresses; classifying them here would file them away and
//     silently break that.
//
// quotesOurRequest (subject or body fingerprint) is only load-bearing for
// rule 4 and the final unattributed fallback, not for rules 2-3b. A ticketing
// platform rewrites the subject on most of the messages a ticket produces - a
// satisfaction survey, "ticket updated", a follow-up - and quotes it
// faithfully only on the very first auto-acknowledgement, if that; a message
// that echoes what it answers (a Gmail conversation thread, a support
// system's "in reply to" quote) may still carry the body fingerprint even
// then. Gating the helpdesk-tenant, subdomain and platform-sender-name rules
// behind this match left them firing on close to nothing against a live
// account: helpdesk tenant slug, contacted-domain-subdomain or a contacted
// broker's own domain root in the sender name is already exact,
// private-state evidence (an outsider can't know which brokers this install
// wrote to), same strength as rule 1's domain match, and needs no
// corroboration from subject or body text.
func (m *Monitor) matchReply(email *Email) ReplyMatch {
	// Rule 1: sender domain resolves to a broker, as before.
	if email.FromDomain != "" {
		if b, ok := m.brokers[email.FromDomain]; ok {
			if m.contacted == nil || m.contacted[b.ID] {
				return ReplyMatch{BrokerID: b.ID, Attributed: true, Via: "sender domain"}
			}
		}
	}

	if email.FromDomain == "" || unmatchableDomains[email.FromDomain] {
		return ReplyMatch{}
	}
	if m.config.Email != "" && strings.EqualFold(strings.TrimSpace(email.From), strings.TrimSpace(m.config.Email)) {
		return ReplyMatch{}
	}
	if isBounceEmail(email, strings.ToLower(email.Subject), strings.ToLower(email.Body+" "+email.HTMLBody)) {
		return ReplyMatch{}
	}

	// Rule 2: a helpdesk tenant naming the broker.
	if tenant, ok := helpdeskTenant(email.FromDomain); ok {
		if id, ok := m.contactedBySlug(tenant); ok {
			return ReplyMatch{BrokerID: id, Attributed: true, Via: "helpdesk tenant"}
		}
	}

	// Rule 3: a subdomain of a broker we wrote to (privacy.example.com).
	// Only for non-helpdesk hosts - reducing a helpdesk domain this way gives
	// the helpdesk, not the broker.
	if _, isHelpdesk := helpdeskTenant(email.FromDomain); !isHelpdesk {
		if parent, ok := parentDomain(email.FromDomain); ok && !unmatchableDomains[parent] {
			if b, ok := m.brokers[parent]; ok {
				if m.contacted == nil || m.contacted[b.ID] {
					return ReplyMatch{BrokerID: b.ID, Attributed: true, Via: "sender subdomain"}
				}
			}
		}
	}

	// Rule 3b: a shared privacy-request platform naming the client in the
	// sender's display name, not the domain (see brokerFromSenderDisplayName).
	if id, ok := m.brokerFromSenderDisplayName(email); ok {
		return ReplyMatch{BrokerID: id, Attributed: true, Via: "privacy-platform sender name"}
	}

	if !m.quotesOurRequest(email) {
		return ReplyMatch{}
	}

	// Rule 4, last resort: an address we wrote to, quoted in the reply body.
	//
	// Ranked last rather than first, against the intuition that a reply
	// echoes the message it answers. Measured against the real account, these
	// auto-acknowledgements do not quote anything - they are three lines and a
	// ticket number, typically prefaced "Please type your reply above this
	// line". Of eight real replies, none carried the address we wrote to, and
	// the one body that did contain a contacted address held a *different*
	// broker's (privacy@tapad.com inside an Experian ticket), which run first
	// would have mis-attributed a reply the tenant rule gets right.
	//
	// Hence both the position and the uniqueness requirement below: a body
	// naming several brokers we've written to identifies none of them. Gated
	// on the subject match, unlike rules 2-3: an address sitting in body text
	// is weaker, attacker-reachable evidence, so it needs the subject as
	// corroboration that this is genuinely shaped like a reply to us.
	if id, ok := m.brokerFromQuotedAddress(email); ok {
		return ReplyMatch{BrokerID: id, Attributed: true, Via: "quoted request address"}
	}

	// A genuine reply we can't pin down. Worth recording and surfacing, but
	// not worth treating as identified.
	return ReplyMatch{BrokerID: unattributedID(email.FromDomain), Via: "request subject or body"}
}

// subjectLooksLikeOurRequest reports whether a subject quotes one of the
// request subjects this install actually sends.
func (m *Monitor) subjectLooksLikeOurRequest(subject string) bool {
	if subject == "" || len(m.requestSubjects) == 0 {
		return false
	}
	s := strings.ToLower(subject)
	for _, want := range m.requestSubjects {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// quotesOurRequest reports whether a message quotes one of the request
// subjects or request body fingerprints this install actually sends - either
// is sufficient on its own. A ticketing system rewrites the subject on most
// of a ticket's messages, but a reply that echoes the message it answers (a
// Gmail conversation thread, a support system quoting "in reply to") still
// carries the fixed opening sentence of our own template in its body, even
// when its subject has been rewritten to something unrecognisable.
func (m *Monitor) quotesOurRequest(email *Email) bool {
	if m.subjectLooksLikeOurRequest(email.Subject) {
		return true
	}
	if len(m.requestBodyFingerprints) == 0 {
		return false
	}
	body := strings.ToLower(email.Body)
	if html := stripHTMLSimple(email.HTMLBody); html != "" {
		body += " " + strings.ToLower(html)
	}
	if body == "" {
		return false
	}
	for _, want := range m.requestBodyFingerprints {
		if strings.Contains(body, want) {
			return true
		}
	}
	return false
}

// brokerFromQuotedAddress looks for an address we wrote to in the reply's
// body, and resolves it only when the body points at exactly one broker.
//
// Bodies are attacker-influenced, but an address only resolves if it's one we
// actually sent a request to, which an outsider has no way to know. The
// uniqueness rule covers the honest ambiguity instead: a corporate group's
// ticket can mention sibling brands' privacy addresses, and picking whichever
// the regex happened to reach first would attribute the reply to the wrong
// broker. Both bodies are searched, since some senders put the useful text
// only in the HTML part.
func (m *Monitor) brokerFromQuotedAddress(email *Email) (string, bool) {
	if len(m.contactedAddresses) == 0 {
		return "", false
	}

	found := make(map[string]bool, 2)
	for _, body := range []string{email.Body, email.HTMLBody} {
		if body == "" {
			continue
		}
		for _, addr := range emailRegex.FindAllString(body, -1) {
			key := strings.ToLower(strings.Trim(addr, ".,;:<>()[]\"'"))
			if id, ok := m.contactedAddresses[key]; ok {
				found[id] = true
			}
		}
	}

	if len(found) != 1 {
		return "", false
	}
	for id := range found {
		return id, true
	}
	return "", false
}

// contactedBySlug resolves a helpdesk tenant label against the IDs and names
// of brokers we've written to. The match is exact on the slug: a prefix or
// substring rule would let a short tenant label ("alc", "ncr", "pipl" are all
// real broker IDs) capture an unrelated company's helpdesk.
func (m *Monitor) contactedBySlug(slug string) (string, bool) {
	if slug == "" || len(m.contactedSlugs) == 0 {
		return "", false
	}
	id, ok := m.contactedSlugs[normalizeSlug(slug)]
	return id, ok
}

// normalizeSlug reduces a name or label to comparable form: lowercase, with
// everything outside [a-z0-9] removed. "Experian Marketing Services",
// "experian-marketing-services" and "experianmarketingservices" all collapse
// together, which is what lets a helpdesk tenant label line up with a broker
// name that punctuates differently.
func normalizeSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// parentDomain drops the leftmost label: privacy.hubspot.com -> hubspot.com.
// Returns false when there's nothing to drop, or when the result would be a
// bare public suffix.
func parentDomain(domain string) (string, bool) {
	parts := strings.Split(strings.ToLower(domain), ".")
	if len(parts) < 3 {
		return "", false
	}
	return strings.Join(parts[1:], "."), true
}

// ArchiveDecision reports whether one recognised reply may be moved out of the
// mailbox it was found in, and why not when it can't.
//
// The asymmetry between ordinary folders and the spam folder is deliberate.
// Subject text is chosen by whoever sent the message, and the subjects this
// tool uses are published in its own source, so "quotes our request subject"
// is something anyone can forge. In the inbox that costs little: the message
// is filed into a folder, nothing is deleted, and it was already delivered.
// In the spam folder it would be a way for a stranger to pull mail *out* of
// spam - clearing its spam status and filing it where the user expects
// trustworthy correspondence, with any links it carries then in front of
// `eraser confirm` and `eraser fill`.
//
// So a reply is only rescued from spam when it was actually attributed to a
// broker, which needs the address we wrote to, a matching helpdesk tenant, or
// the broker's own domain - none of which an outsider can know or forge,
// because they depend on which brokers this particular user wrote to.
// Unattributed spam is still classified and recorded (with NeedsReview) so it
// surfaces in the UI: Gmail purges spam after 30 days, and "we won't move it"
// must not become "you'll never see it".
func ArchiveDecision(email Email, sourceIsSpam bool) (allowed bool, reason string) {
	if email.BrokerID == "" {
		return false, "not recognised as a reply"
	}
	if sourceIsSpam && !email.Attributed {
		return false, "in spam and not attributed to a broker - recorded for review, left in place"
	}
	return true, ""
}

// ArchivableUIDs filters emails down to those that may be moved, given which
// folder is the spam folder. Pairs with GroupUIDsByFolder, which then buckets
// them by source mailbox.
func ArchivableUIDs(emails []Email, spamFolder string) ([]Email, []Email) {
	var allowed, held []Email
	for _, e := range emails {
		isSpam := spamFolder != "" && strings.EqualFold(e.Folder, spamFolder)
		if ok, _ := ArchiveDecision(e, isSpam); ok {
			allowed = append(allowed, e)
			continue
		}
		held = append(held, e)
	}
	return allowed, held
}
