package web

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/eraser-privacy/eraser/internal/broker"
	"github.com/eraser-privacy/eraser/internal/config"
	"github.com/go-chi/chi/v5"
)

// brokerQueryFromForm rebuilds the brokers-page filters from a POST body.
// The row controls in partials/broker-list.html hx-include the filter bar,
// so the current filter state arrives in r.Form, NOT r.URL.Query() - reading
// the query string here would silently reset every filter on each tag or
// exclude action.
func brokerQueryFromForm(r *http.Request) brokerQuery {
	return brokerQuery{
		Search:       r.FormValue("search"),
		Category:     r.FormValue("category"),
		Region:       r.FormValue("region"),
		Priority:     r.FormValue("priority"),
		Status:       r.FormValue("status"),
		MissingEmail: r.FormValue("missing_email") == "true",
		Tag:          r.FormValue("tag"),
		NonSendable:  r.FormValue("non_sendable") == "true",
	}
}

// renderBrokerListFragment re-renders the broker table after a row action so
// the change is immediately visible. A broker that just became non-sendable
// or excluded may legitimately vanish from the current filtered view - that
// is the point of the action, not a rendering bug.
func (s *Server) renderBrokerListFragment(w http.ResponseWriter, r *http.Request) {
	q := brokerQueryFromForm(r)
	brokers := s.getBrokersWithStatus(s.activeProfile(r).ID, q)
	s.renderPartial(w, "partials/broker-list.html", map[string]interface{}{
		"Brokers":         brokers,
		"Filtered":        len(brokers),
		"Total":           len(s.getBrokerDB().Brokers),
		"DispositionTags": broker.DispositionTags,
		// Tag/exclude actions change how many rows are emailable, and the
		// counter lives in the page header, outside this fragment - the
		// partial's OutOfBand span swaps the new number in.
		"SendableCount": len(sendable(brokers)),
		"OutOfBand":     true,
	})
}

// errBrokerNotFound is mutateBrokers' way of reporting a missing ID up
// through the write path; the handler turns it into a 404.
var errBrokerNotFound = fmt.Errorf("broker not found")

// handleAPITagBroker adds or removes a disposition tag on one broker and
// persists the change to the brokers file. Tagged brokers are hard-blocked
// by broker.Sendable(), so this is the web UI's way of retiring a company
// from every send path at once (e.g. it replied that it is B2B-only, or
// only holds data on US customers).
func (s *Server) handleAPITagBroker(w http.ResponseWriter, r *http.Request) {
	limitFormBody(w, r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}

	brokerID := chi.URLParam(r, "brokerID")
	action := r.FormValue("action")
	tag := r.FormValue("tag")
	// The brokers-page row control is a single <select name="tag_change">
	// whose value encodes both halves ("add:b2b-only"); direct API callers
	// pass action and tag separately. Normalize before validating. The
	// field is not called "tag" because hx-include selectors on the page
	// match [name='tag'] and must only ever hit the filter bar's
	// disposition dropdown.
	if action == "" && tag == "" {
		if pre, rest, found := strings.Cut(r.FormValue("tag_change"), ":"); found {
			action, tag = pre, rest
		}
	}

	// The vocabulary is closed: a typo must fail loudly, not read as "no
	// disposition" and quietly put the broker back in the send list.
	// broker.ApplyDisposition validates it again inside the write, but this
	// check is what turns a bad tag into a 400 with the valid list.
	if !broker.IsDispositionTag(tag) {
		http.Error(w, fmt.Sprintf("Unknown tag %q; valid tags: %s", tag, strings.Join(broker.DispositionTags, ", ")), http.StatusBadRequest)
		return
	}
	if action != "add" && action != "remove" {
		http.Error(w, "action must be add or remove", http.StatusBadRequest)
		return
	}

	err := s.mutateBrokers(func(db *broker.BrokerDatabase) (bool, error) {
		b := db.FindByID(brokerID)
		if b == nil {
			return false, errBrokerNotFound
		}
		// Same helper the CLI's tag-broker calls, so the two doors apply
		// identical rules (validation, the add/remove switch, and the audit
		// note a tagged broker must carry).
		return broker.ApplyDisposition(b, tag, action == "remove", "", "web UI")
	})
	if err == errBrokerNotFound {
		http.Error(w, "Broker not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.renderBrokerListFragment(w, r)
}

// handleAPIExcludeBroker adds or removes a broker from
// options.excluded_brokers - the user-level "never show, never send" list
// that both the web UI and the CLI's send command enforce. Unlike a
// disposition tag this makes no claim about the company itself; it just
// takes one broker out of this user's campaign.
func (s *Server) handleAPIExcludeBroker(w http.ResponseWriter, r *http.Request) {
	limitFormBody(w, r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}

	brokerID := chi.URLParam(r, "brokerID")
	action := r.FormValue("action")
	if action != "exclude" && action != "include" {
		http.Error(w, "action must be exclude or include", http.StatusBadRequest)
		return
	}

	// The exclusion list is matched against both broker IDs and broker
	// names (getBrokersWithStatus, and broker.Filter on the CLI side), so
	// "include" must remove either spelling, and we read the name before
	// the write.
	var brokerName string
	if b := s.getBrokerDB().FindByID(brokerID); b != nil {
		brokerName = b.Name
	}

	// mutateConfig, not a bare load-copy-save-store: two Exclude clicks in
	// flight together used to each copy the same config snapshot, and
	// whichever saved second dropped the other's exclusion - leaving a
	// broker the user had excluded still in the next bulk send.
	err := s.mutateConfig(func(cfg *config.Config) error {
		excluded := make([]string, 0, len(cfg.Options.ExcludedBrokers)+1)
		seen := make(map[string]bool, len(cfg.Options.ExcludedBrokers))
		for _, e := range cfg.Options.ExcludedBrokers {
			key := strings.ToLower(strings.TrimSpace(e))
			if key == strings.ToLower(brokerID) || (brokerName != "" && key == strings.ToLower(brokerName)) {
				continue
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			excluded = append(excluded, e)
		}
		if action == "exclude" {
			excluded = append(excluded, brokerID)
		}
		cfg.Options.ExcludedBrokers = excluded
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.renderBrokerListFragment(w, r)
}
