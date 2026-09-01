package web

import (
	"net/http"

	"github.com/eraser-privacy/eraser/internal/broker"
)

// The landing page's argument, in one line: for most people the companies
// worth writing to FIRST are not the ones holding a file with their name on
// it, but the handful that join hashed identifiers together. Those few sit
// between every advertiser and every publisher, so opting out with them
// reaches further than a hundred letters to companies that never knew your
// name.
//
// The list is curated by ID rather than derived from a category, because
// "does identity resolution" is not something data/brokers.yaml records and
// guessing it from `category: marketing` (722 of the ~850 entries) would be
// noise. Each entry is resolved against the live database at render time:
// an ID that has been retired or renamed simply drops off the page instead
// of rendering a row that links nowhere.
var identityResolvers = []struct {
	ID   string
	Role string
}{
	{"liveramp", "Runs one of the largest identity graphs: turns a hashed email address into a persistent ID that other companies can match against."},
	{"acxiom", "Audience data and identity resolution at global scale; its segments are resold through most major ad platforms."},
	{"epsilon", "Builds \"connected identity\" profiles that join offline purchase history to online activity."},
	{"experian-marketing", "The marketing arm of the credit bureau, and a large identity/onboarding provider in its own right."},
	{"transunion-marketing", "Marketing and identity resolution, including the former Neustar identity business acquired in 2021."},
	{"tapad", "Cross-device graph (owned by Experian): links the phone, laptop and TV believed to belong to one household."},
	{"merkle", "Identity and audience management inside the dentsu group; operates its own consumer identity graph."},
	{"lotame", "Data exchange and identity solution selling audience segments built from third-party signals."},
	{"id5-technology", "Shared advertising ID used across publishers as a cookie replacement."},
	{"zeotap-gmbh", "European identity and customer-data platform joining telco, retail and web signals."},
	{"nielsen", "Measurement panels and audience segments derived from them."},
}

// resolvedResolver is one identity-resolution company, as far as the loaded
// database knows about it.
type resolvedResolver struct {
	broker.Broker
	Role string
}

// identityResolverRows returns the identity-resolution companies present in
// the loaded database, in the curated order above.
func (s *Server) identityResolverRows() []resolvedResolver {
	db := s.getBrokerDB()
	rows := make([]resolvedResolver, 0, len(identityResolvers))
	for _, r := range identityResolvers {
		b := db.FindByID(r.ID)
		if b == nil {
			continue
		}
		rows = append(rows, resolvedResolver{Broker: *b, Role: r.Role})
	}
	return rows
}

// handleWelcome renders the landing page: what this tool is for, and which
// law you are actually exercising depending on where you live. It is the
// first tab rather than the dashboard because the answer differs sharply
// between an EU/UK user (a hard erasure right, but usually no name-and-
// address record to erase) and a US user (no federal erasure right, but the
// companies genuinely do hold a file on you).
func (s *Server) handleWelcome(w http.ResponseWriter, r *http.Request) {
	s.renderWithCSRF(w, r, "welcome.html", map[string]interface{}{
		"Title":     "Start Here",
		"Resolvers": s.identityResolverRows(),
	})
}
