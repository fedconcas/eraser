package web

import (
	"context"
	"crypto/rand"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eraser-privacy/eraser/internal/broker"
	"github.com/eraser-privacy/eraser/internal/config"
	"github.com/eraser-privacy/eraser/internal/history"
	emaTemplate "github.com/eraser-privacy/eraser/internal/template"
	// filippo.io/csrf/gorilla is a drop-in for github.com/gorilla/csrf, taken
	// because gorilla/csrf is subject to GO-2025-3884 (CVE-2025-47909) with
	// no fixed release available. It enforces same-origin via Fetch metadata
	// headers instead of tokens; csrf.Token/TemplateField remain as no-op
	// stubs so the existing templates and the X-CSRF-Token header keep
	// compiling unchanged. Note that tokens are ignored - the same-origin
	// check is what provides the protection now.
	csrf "filippo.io/csrf/gorilla"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

//go:embed static/*
var staticFS embed.FS

//go:embed templates/*
var templatesFS embed.FS

const (
	defaultRateLimit  = 30
	defaultRateWindow = time.Minute
	defaultSessionTTL = 30 * time.Minute
)

type RateLimiter struct {
	mu       sync.Mutex
	requests map[string][]time.Time
	limit    int
	window   time.Duration
}

func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	rl := &RateLimiter{
		requests: make(map[string][]time.Time),
		limit:    limit,
		window:   window,
	}
	go rl.cleanupLoop()
	return rl
}

func (rl *RateLimiter) filterRecent(times []time.Time, windowStart time.Time) []time.Time {
	n := 0
	for _, t := range times {
		if t.After(windowStart) {
			times[n] = t
			n++
		}
	}
	return times[:n]
}

func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	recent := rl.filterRecent(rl.requests[key], now.Add(-rl.window))

	if len(recent) >= rl.limit {
		rl.requests[key] = recent
		return false
	}
	rl.requests[key] = append(recent, now)
	return true
}

func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		rl.mu.Lock()
		windowStart := time.Now().Add(-rl.window)
		for key, times := range rl.requests {
			recent := rl.filterRecent(times, windowStart)
			if len(recent) == 0 {
				delete(rl.requests, key)
			} else {
				rl.requests[key] = recent
			}
		}
		rl.mu.Unlock()
	}
}

type Server struct {
	config atomic.Pointer[config.Config]
	// configMu serialises writers in mutateConfig, for the same reason
	// brokerMu exists below: the atomic pointer alone lets two concurrent
	// POSTs each copy the same snapshot and silently drop one of the two
	// changes (two Exclude clicks in flight, or an exclude landing next to
	// a settings save).
	configMu   sync.Mutex
	configPath string
	// brokerDB is an atomic.Pointer rather than a plain pointer because the
	// tag/exclude endpoints now write it (via mutateBrokers) while page and
	// API handlers read it concurrently. Readers take a snapshot with
	// getBrokerDB() and work from that; writers never mutate a stored
	// snapshot in place - see mutateBrokers for the copy-on-write protocol.
	brokerDB       atomic.Pointer[broker.BrokerDatabase]
	brokerMu       sync.Mutex // serialises writers in mutateBrokers
	brokerPath     string     // data/brokers.yaml on disk; "" in tests (no persistence)
	historyStore   *history.Store
	tmplEngine     *emaTemplate.Engine
	templates      map[string]*template.Template
	httpServer     *http.Server
	port           int
	csrfKey        []byte
	sessions       *SessionStore
	rateLimiter    *RateLimiter
	jobManager     *JobManager
	jobPersistence *JobPersistence
}

func NewServer(port int, cfg *config.Config, configPath, brokerPath string, brokerDB *broker.BrokerDatabase, historyStore *history.Store, tmplEngine *emaTemplate.Engine) (*Server, error) {
	csrfKey := make([]byte, 32)
	if _, err := rand.Read(csrfKey); err != nil {
		return nil, fmt.Errorf("failed to generate CSRF key: %w", err)
	}

	// Job persistence lives alongside the config file, so an alternate
	// --config also gets its own isolated pending_job.json instead of
	// always sharing ~/.eraser (matches history.DBPathFor's same reasoning
	// for history.db). configPath is only ever "" from tests constructing
	// a Server directly - preserve the old default there.
	dataDir := filepath.Dir(configPath)
	if configPath == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".eraser")
	}

	s := &Server{
		configPath:     configPath,
		brokerPath:     brokerPath,
		historyStore:   historyStore,
		tmplEngine:     tmplEngine,
		port:           port,
		csrfKey:        csrfKey,
		sessions:       NewSessionStore(defaultSessionTTL),
		rateLimiter:    NewRateLimiter(defaultRateLimit, defaultRateWindow),
		jobManager:     NewJobManager(),
		jobPersistence: NewJobPersistence(dataDir),
	}
	s.config.Store(cfg)
	s.brokerDB.Store(brokerDB)

	tmpl, err := s.parseTemplates()
	if err != nil {
		return nil, fmt.Errorf("failed to parse templates: %w", err)
	}
	s.templates = tmpl
	return s, nil
}

// getBrokerDB returns the current broker database snapshot. Handlers snapshot
// once per request and read from that; never hold the pointer across a
// mutateBrokers call expecting freshness.
func (s *Server) getBrokerDB() *broker.BrokerDatabase {
	return s.brokerDB.Load()
}

// mutateBrokers is the ONLY write path for the broker database. It clones
// the current snapshot, lets fn mutate the clone, persists to brokerPath
// (SaveWithBackup) and only then publishes the new snapshot - so readers
// never observe a half-written state and a failed save leaves the in-memory
// DB untouched too. brokerMu serialises writers: an atomic pointer alone
// would let two concurrent POSTs each copy the same snapshot and silently
// drop one of the two changes. fn receives the clone and reports whether
// anything changed; on unchanged=false the save and publish are skipped.
func (s *Server) mutateBrokers(fn func(db *broker.BrokerDatabase) (changed bool, err error)) error {
	s.brokerMu.Lock()
	defer s.brokerMu.Unlock()

	// Re-read the file rather than editing the snapshot loaded at startup:
	// `eraser tag-broker` / `add-broker` / `cleanup-bounces` write the same
	// file while the server runs, and publishing a stale snapshot over the
	// top of them silently reverted those edits (a retired broker quietly
	// re-entered the send list). Whatever is on disk wins; a file that no
	// longer parses fails the write loudly instead of overwriting it.
	clone, err := s.currentBrokersForWrite()
	if err != nil {
		return err
	}

	changed, err := fn(clone)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}

	if s.brokerPath != "" {
		if err := clone.SaveWithBackup(s.brokerPath); err != nil {
			return fmt.Errorf("failed to save broker database: %w", err)
		}
	}
	s.brokerDB.Store(clone)
	return nil
}

// currentBrokersForWrite returns the database a mutateBrokers write should
// start from: the file on disk when there is one, otherwise a copy of the
// published snapshot (tests, and any embedded-data setup with no writable
// path). Callers hold brokerMu.
func (s *Server) currentBrokersForWrite() (*broker.BrokerDatabase, error) {
	if s.brokerPath != "" {
		db, err := broker.LoadFromFile(s.brokerPath)
		if err != nil {
			return nil, fmt.Errorf("failed to re-read broker database: %w", err)
		}
		return db, nil
	}
	cur := s.brokerDB.Load()
	return &broker.BrokerDatabase{Brokers: slices.Clone(cur.Brokers)}, nil
}

// mutateConfig is the write path for the config, the twin of mutateBrokers:
// it serialises writers, hands fn a copy to mutate (a concurrent reader - or
// a running send job - may be holding the pointer getConfig returned),
// persists it and only then publishes. fn's error is returned unchanged so
// each handler can render its own message; nothing is saved or published in
// that case.
func (s *Server) mutateConfig(fn func(cfg *config.Config) error) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()

	cur := s.getConfig()
	newCfg := config.Config{}
	if cur != nil {
		newCfg = *cur
	}

	if err := fn(&newCfg); err != nil {
		return err
	}

	if s.configPath == "" {
		return fmt.Errorf("config path not set")
	}
	if err := config.Save(s.configPath, &newCfg); err != nil {
		return fmt.Errorf("failed to save configuration: %w", err)
	}
	s.config.Store(&newCfg)
	return nil
}

// getConfig returns the server's current config. Server.config is an
// atomic.Pointer[config.Config] rather than a plain *config.Config because
// it's written concurrently (handleSettingsInbox, handleSetupComplete) while
// being read by many handlers and by background send-job goroutines for the
// duration of a send - see the load-copy-mutate-store pattern at the two
// write sites for how updates stay race-free.
func (s *Server) getConfig() *config.Config {
	return s.config.Load()
}

// maxFormBodyBytes caps the size of request bodies read by ParseForm, so a
// misbehaving or malicious client can't make a form-parsing handler buffer
// an unbounded body in memory.
const maxFormBodyBytes = 1 << 20 // 1MB

// limitFormBody wraps r.Body in a MaxBytesReader before the handler calls
// r.ParseForm()/r.FormValue(). w is passed through to MaxBytesReader so it
// can send a 413 response if the client keeps writing past the limit.
func limitFormBody(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBodyBytes)
}

// parseTemplates loads and parses all HTML templates
// Each page gets its own template set to avoid "content" block conflicts
// partialSource is a partial template plus the name it is registered under,
// so pages can invoke it by file name.
type partialSource struct {
	name    string
	content string
}

func (s *Server) parseTemplates() (map[string]*template.Template, error) {
	funcs := template.FuncMap{
		"formatTime": func(t time.Time) string {
			return t.Format("Jan 2, 2006 3:04 PM")
		},
		"formatDate": func(t time.Time) string {
			return t.Format("Jan 2, 2006")
		},
		"add": func(a, b int) int {
			return a + b
		},
		// join renders a []string profile field (additional emails, name
		// variants, ...) back into the free-text form control it was typed
		// into. Doing it here rather than precomputing a string per handler
		// keeps it to one call site: a missing map key in a render-data map
		// doesn't fail loudly, it prints "<no value>" straight into the
		// input's value, and these forms render from eight different places.
		"join": func(items []string, sep string) string {
			return strings.Join(items, sep)
		},
	}

	// Read layout template
	layoutContent, err := templatesFS.ReadFile("templates/layout.html")
	if err != nil {
		return nil, fmt.Errorf("failed to read layout template: %w", err)
	}

	// Read all partial templates
	var partials []partialSource
	partialTemplates := make(map[string]string)
	err = fs.WalkDir(templatesFS, "templates/partials", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".html") {
			return err
		}
		content, err := templatesFS.ReadFile(path)
		if err != nil {
			return err
		}
		name := path[len("templates/"):]
		partials = append(partials, partialSource{name: name, content: string(content)})
		// Also save for standalone partial templates
		partialTemplates[name] = string(content)
		return nil
	})
	if err != nil && !strings.Contains(err.Error(), "file does not exist") {
		return nil, fmt.Errorf("failed to read partials: %w", err)
	}

	// Map to hold all page templates
	templates := make(map[string]*template.Template)

	// Walk through all page templates and create separate template sets
	err = fs.WalkDir(templatesFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Skip directories, partials, and layout
		if d.IsDir() || strings.Contains(path, "/partials/") || path == "templates/layout.html" {
			return nil
		}
		if !strings.HasSuffix(path, ".html") {
			return nil
		}

		content, err := templatesFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read template %s: %w", path, err)
		}

		// Create a new template for this page
		name := path[len("templates/"):]
		pageTmpl := template.New(name).Funcs(funcs)

		// Parse layout first
		_, err = pageTmpl.Parse(string(layoutContent))
		if err != nil {
			return fmt.Errorf("failed to parse layout for %s: %w", name, err)
		}

		// Parse partials, each under its own file name, so a page can invoke
		// one as {{template "partials/x.html" .}}. Parsing them into the
		// page's namespace anonymously (the old behaviour) left them
		// unresolvable by name, which is why the broker table existed as two
		// hand-duplicated copies - this file and partials/broker-list.html -
		// that had to be edited in lockstep.
		//
		// A partial that wraps itself in {{define "name"}} still works: the
		// define simply overrides the entry created here. Note the trap that
		// creates - if a partial defines a name DIFFERENT from its file name
		// (partials/profile-fields.html does, deliberately), then its
		// file-named entry is empty, and renderPartial on it would emit
		// nothing at all rather than failing. Only renderPartial a partial
		// that has no define, or one whose define matches its file name
		// (partials/template-preview.html).
		for _, partial := range partials {
			_, err = pageTmpl.New(partial.name).Parse(partial.content)
			if err != nil {
				return fmt.Errorf("failed to parse partial for %s: %w", name, err)
			}
		}

		// Parse the page content (this defines "content" block for this specific page)
		_, err = pageTmpl.Parse(string(content))
		if err != nil {
			return fmt.Errorf("failed to parse template %s: %w", name, err)
		}

		// Store in map
		templates[name] = pageTmpl

		return nil
	})

	if err != nil {
		return nil, err
	}

	// Add partial templates as standalone templates (for HTMX responses)
	for name, content := range partialTemplates {
		partialTmpl := template.New(name).Funcs(funcs)
		_, err = partialTmpl.Parse(content)
		if err != nil {
			return nil, fmt.Errorf("failed to parse partial %s: %w", name, err)
		}
		templates[name] = partialTmpl
	}

	return templates, nil
}

// Start starts the web server and opens the browser
func (s *Server) Start() error {
	router := s.setupRouter()

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf("127.0.0.1:%d", s.port),
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Check for pending job and offer to resume
	s.checkPendingJob()

	// Open browser after a short delay
	go func() {
		time.Sleep(500 * time.Millisecond)
		url := fmt.Sprintf("http://localhost:%d", s.port)
		openBrowser(url)
	}()

	fmt.Printf("Starting Eraser web UI at http://localhost:%d\n", s.port)
	fmt.Println("Press Ctrl+C to stop")

	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}

	return nil
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

// setupRouter configures all routes
func (s *Server) setupRouter() *chi.Mux {
	r := chi.NewRouter()

	// Middleware
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	// Before CSRF and before any session/token work: a rebound request must
	// be turned away without ever reaching the token store.
	r.Use(hostCheck(s.port))
	r.Use(middleware.Compress(5))
	r.Use(securityHeaders)

	// CSRF protection - secure for localhost only
	csrfMiddleware := csrf.Protect(
		s.csrfKey,
		csrf.Secure(false), // Allow HTTP for localhost
		csrf.Path("/"),
		csrf.HttpOnly(true),
		csrf.SameSite(csrf.SameSiteLaxMode), // Lax mode for form submissions
		csrf.RequestHeader("X-CSRF-Token"),  // For HTMX AJAX requests
		// Scheme-qualified, and http:// specifically: this package prefixes
		// bare hosts with https://, so the old bare-host list would silently
		// match nothing over plain-HTTP localhost. Both spellings are listed
		// so that browsing on 127.0.0.1 while posting to localhost (or vice
		// versa) keeps working exactly as it does today.
		csrf.TrustedOrigins([]string{
			fmt.Sprintf("http://localhost:%d", s.port),
			fmt.Sprintf("http://127.0.0.1:%d", s.port),
		}),
	)
	r.Use(csrfMiddleware)

	// Static files
	staticSub, _ := fs.Sub(staticFS, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	// Routes
	r.Get("/", s.handleDashboard)
	// The landing tab: which law you are actually exercising, and which
	// companies are worth writing to first, depending on where you live.
	r.Get("/welcome", s.handleWelcome)
	r.Get("/brokers", s.handleBrokers)
	r.Get("/history", s.handleHistory)
	r.Get("/settings", s.handleSettings)
	r.Post("/settings/inbox", s.handleSettingsInbox)
	r.Post("/settings/template", s.handleSettingsTemplate)
	r.Get("/settings/profiles/new", s.handleSettingsProfileNew)
	r.Post("/settings/profiles/new", s.handleSettingsProfileNew)
	r.Get("/settings/profiles/{profileID}/edit", s.handleSettingsProfileEdit)
	r.Post("/settings/profiles/{profileID}/edit", s.handleSettingsProfileEdit)
	r.Post("/settings/profiles/{profileID}/delete", s.handleSettingsProfileDelete)
	r.Get("/pipeline", s.handlePipeline)
	r.Get("/tasks", s.handleTasks)
	r.Get("/tasks/{taskID}", s.handleTaskDetail)
	r.Get("/tasks/{taskID}/helper", s.handleTaskHelper)
	r.Post("/tasks/{taskID}/complete", s.handleTaskComplete)
	r.Post("/tasks/{taskID}/skip", s.handleTaskSkip)
	r.Get("/forms", s.handleForms)
	r.Post("/forms/{brokerID}/complete", s.handleFormComplete)
	r.Post("/forms/{brokerID}/skip", s.handleFormSkip)

	// Setup wizard routes
	r.Route("/setup", func(r chi.Router) {
		r.Get("/", s.handleSetupWelcome)
		r.Get("/profile", s.handleSetupProfile)
		r.Post("/profile", s.handleSetupProfile)
		r.Get("/email", s.handleSetupEmail)
		r.Post("/email", s.handleSetupEmail)
		r.Get("/test", s.handleSetupTest)
		r.Post("/test/send", s.handleSetupTestSend)
		r.Get("/complete", s.handleSetupComplete)
	})

	// API routes (for HTMX)
	r.Route("/api", func(r chi.Router) {
		r.Get("/brokers", s.handleAPIBrokers)
		r.Get("/brokers/{brokerID}/status", s.handleAPIBrokerStatus)
		r.Post("/brokers/{brokerID}/tag", s.handleAPITagBroker)
		r.Post("/brokers/{brokerID}/exclude", s.handleAPIExcludeBroker)
		r.Delete("/history/failed", s.handleAPIDeleteFailed)
		r.Delete("/history", s.handleAPIDeleteAllHistory)
		r.Post("/send/{brokerID}", s.handleAPISendOne)
		r.Post("/send-all", s.handleAPISendAll)
		r.Get("/job/active", s.handleAPIJobActive)
		r.Get("/job/{jobID}/status", s.handleAPIJobStatus)
		r.Post("/job/{jobID}/cancel", s.handleAPIJobCancel)
		r.Get("/pipeline/responses", s.handleAPIResponses)
		r.Post("/inbox/scan", s.handleAPIInboxScan)
		r.Post("/inbox/rescan", s.handleAPIInboxRescan)
		r.Post("/inbox/reclassify", s.handleAPIReclassify)
		r.Post("/profile", s.handleAPISwitchProfile)
		r.Get("/template/preview", s.handleAPITemplatePreview)
	})

	return r
}

// hostCheck rejects any request whose Host header isn't one of the loopback
// names this server is actually reachable at.
//
// The server binds 127.0.0.1 only (see Start), but that does NOT stop a
// malicious web page the user happens to visit from reaching it via DNS
// rebinding: the page's domain re-resolves to 127.0.0.1, after which the
// browser treats the attacker's origin as same-origin with this server and
// its JavaScript can read every page here - including the stored app
// password on /settings. Such a request carries the attacker's hostname in
// Host, so pinning Host to the loopback names is what actually closes it.
//
// Origin/Sec-Fetch-Site checks (i.e. the CSRF layer) do NOT help here: after
// rebinding the browser genuinely considers the request same-origin and
// labels it as such. This middleware is the only defense, which is why it
// matches exactly - no prefix or suffix matching. Hostnames like
// "localhost.evil.com" or the *.nip.io / localtest.me wildcard services all
// resolve to loopback and would slip past a looser comparison.
func hostCheck(port int) func(http.Handler) http.Handler {
	allowed := map[string]bool{
		"localhost":                       true,
		fmt.Sprintf("localhost:%d", port): true,
		"127.0.0.1":                       true,
		fmt.Sprintf("127.0.0.1:%d", port): true,
		"[::1]":                           true,
		fmt.Sprintf("[::1]:%d", port):     true,
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// An empty Host is rejected along with everything else: Go's
			// http.Server already 400s HTTP/1.1 requests that omit it, and
			// every browser sends it, so allowing empty would buy no
			// compatibility while reopening the exact bypass being closed.
			if !allowed[strings.ToLower(r.Host)] {
				http.Error(w, "Forbidden: unexpected Host header", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// securityHeaders adds security headers to all responses
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Prevent clickjacking
		w.Header().Set("X-Frame-Options", "DENY")

		// Prevent MIME type sniffing
		w.Header().Set("X-Content-Type-Options", "nosniff")

		// Control referrer information
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

		// Content Security Policy - restrict resource loading
		// 'unsafe-inline' needed for Tailwind CSS and inline scripts (HTMX attributes)
		// 'unsafe-eval' is needed because /static/js/tailwind-jit.js is still the
		// Tailwind CDN's runtime JIT compiler (self-hosted, but still eval-based) -
		// see the comment in layout.html. Google Fonts is the one remaining
		// external origin; Tailwind and HTMX are self-hosted under /static/.
		csp := "default-src 'self'; " +
			"script-src 'self' 'unsafe-inline' 'unsafe-eval'; " +
			"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
			"img-src 'self' data:; " +
			"font-src 'self' https://fonts.gstatic.com; " +
			"connect-src 'self'; " +
			"object-src 'none'; " +
			"frame-ancestors 'none'; " +
			"form-action 'self'; " +
			"base-uri 'self'"
		w.Header().Set("Content-Security-Policy", csp)

		// Prevent caching of sensitive pages - credentials should never be cached
		// Static files are excluded from this via separate cache headers
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
		}

		// Disable unnecessary browser features
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")

		next.ServeHTTP(w, r)
	})
}

// openBrowser opens the default browser to the specified URL
func openBrowser(url string) {
	var cmd string
	var args []string

	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
		args = []string{url}
	case "linux":
		cmd = "xdg-open"
		args = []string{url}
	case "windows":
		cmd = "cmd"
		args = []string{"/c", "start", url}
	default:
		return
	}

	_ = exec.Command(cmd, args...).Start()
}

// activeProfileCookie names the cookie that remembers which profile the web
// UI is currently scoped to. Plain (not HttpOnly/Secure) since it carries no
// secret - just a UI preference - and the server always validates it against
// the configured profile list before trusting it, so a tampered value just
// falls back to the first profile rather than granting anything.
const activeProfileCookie = "eraser_profile"

// activeProfile resolves which profile the current request should act on.
// Unlike the CLI's --profile flag (where an ambiguous, unspecified profile
// is a hard error - see config.GetProfile), the web UI always has a
// definite answer: the eraser_profile cookie if it still names a configured
// profile, else the first configured profile. This is what every
// profile-scoped handler below should call instead of reaching for
// s.config.Profile directly.
func (s *Server) activeProfile(r *http.Request) config.NamedProfile {
	cfg := s.getConfig()
	if cfg == nil {
		return config.NamedProfile{ID: config.DefaultProfileID}
	}
	profiles := cfg.GetProfiles()
	if cookie, err := r.Cookie(activeProfileCookie); err == nil {
		for _, p := range profiles {
			if p.ID == cookie.Value {
				return p
			}
		}
	}
	return profiles[0]
}

// Helper methods

type Stats struct {
	TotalBrokers int
	Sent         int
	Failed       int
	Pending      int
}

// BrokerWithStatus combines broker info with history status
type BrokerWithStatus struct {
	broker.Broker
	Status    string // "never", "sent", "failed"
	LastSent  string // formatted date or empty
	TotalSent int
	// Excluded marks brokers on the config's excluded_brokers list. They
	// only appear when the query opts into non-sendable brokers, so the row
	// controls can offer an "Include" button to undo an exclusion.
	Excluded bool
}

// brokerQuery is the set of filters the brokers page, its HTMX fragment
// endpoint and the bulk-send job all narrow the broker list with. It's a
// struct rather than a positional argument list because there are enough
// of them now that call sites were getting hard to read (and easy to
// transpose).
type brokerQuery struct {
	Search       string
	Category     string
	Region       string
	Priority     string
	Status       string
	MissingEmail bool
	// Tag narrows to brokers carrying one broker.DispositionTags value.
	// It is deliberately separate from Category: a disposition is what a
	// company told us, a category is what sector it trades in, and the two
	// have to be filterable independently.
	Tag string
	// NonSendable opts INTO showing brokers that can't be emailed: ones
	// Sendable() rejects (b2b-only, form-only, us-data-only, no email) and
	// ones on the user's excluded_brokers list (marked Excluded). The default
	// view is sendable-only: the page lists brokers the user can actually
	// message, and the rest stay in the data file, reachable through this
	// flag - which is also how an excluded broker becomes visible again so it
	// can be un-excluded from the UI.
	NonSendable bool
}

// getBrokersWithStatus returns brokers with their history status
func (s *Server) getBrokersWithStatus(profileID string, q brokerQuery) []BrokerWithStatus {
	// Get all broker statuses from history, scoped to the active profile
	var brokerStatuses map[string]history.BrokerStatus
	if s.historyStore != nil {
		brokerStatuses, _ = s.historyStore.GetAllBrokerStatuses(profileID)
	}
	if brokerStatuses == nil {
		brokerStatuses = make(map[string]history.BrokerStatus)
	}

	// excluded_brokers/excluded_categories (config.yaml) used to only be
	// enforced by the CLI's `send` command via broker.Filter - the web UI's
	// list and bulk-send both went through this function instead, which
	// never looked at either option, so a configured exclusion silently had
	// no effect here. Apply the same two checks broker.Filter does.
	excludedBrokers, excludedCats := s.excludedBrokerSets()

	search := strings.ToLower(strings.TrimSpace(q.Search))
	category := strings.ToLower(strings.TrimSpace(q.Category))
	region := strings.ToLower(strings.TrimSpace(q.Region))
	statusFilter := strings.ToLower(strings.TrimSpace(q.Status))
	// An unrecognized priority normalizes to "" - i.e. "no priority filter" -
	// rather than matching nothing, matching how a bogus category or region
	// in the query string already behaves.
	priority := broker.NormalizePriority(q.Priority)
	tag := strings.ToLower(strings.TrimSpace(q.Tag))

	var result []BrokerWithStatus
	db := s.getBrokerDB()
	// The page default is "brokers you can message": everything Sendable()
	// rejects stays in the data file but out of the list. MissingEmail
	// implies opting in - missing-email brokers are all non-sendable, so
	// without this that filter would always render empty.
	showNonSendable := q.NonSendable || q.MissingEmail
	for _, b := range db.Brokers {
		excluded := brokerExcluded(excludedBrokers, b)
		// Excluded brokers are only visible in the opt-in view - that is
		// where the row's Include button lets the user undo an exclusion.
		// Category exclusions stay a hard skip: there is no per-category
		// undo control to put back what that view would surface.
		if excluded && !q.NonSendable {
			continue
		}
		if excludedCats[strings.ToLower(b.Category)] {
			continue
		}
		if !showNonSendable && !b.Sendable() {
			continue
		}

		// Search filter
		if search != "" {
			name := strings.ToLower(b.Name)
			email := strings.ToLower(b.Email)
			if !strings.Contains(name, search) && !strings.Contains(email, search) {
				continue
			}
		}

		// Category filter
		if category != "" && strings.ToLower(b.Category) != category {
			continue
		}

		// Region filter
		if region != "" && strings.ToLower(b.Region) != region {
			continue
		}

		// Priority filter
		if priority != "" && broker.NormalizePriority(b.Priority) != priority {
			continue
		}

		// Missing-email filter - brokers with no contact address on file,
		// mirrors the CLI's `list-brokers --missing-email`
		if q.MissingEmail && b.Email != "" {
			continue
		}

		// Disposition-tag filter - mirrors the CLI's `list-brokers --tag`
		if tag != "" && !b.HasTag(tag) {
			continue
		}

		bws := BrokerWithStatus{
			Broker:   b,
			Status:   "never",
			Excluded: excluded,
		}

		if status, ok := brokerStatuses[b.ID]; ok {
			bws.Status = string(status.Status)
			bws.TotalSent = status.TotalSent
			if !status.LastSent.IsZero() {
				bws.LastSent = status.LastSent.Format("Jan 2, 2006")
			}
		}

		// Status filter - "pending" means never sent
		if statusFilter != "" {
			if statusFilter == "pending" && bws.Status != "never" {
				continue
			} else if statusFilter == "sent" && bws.Status != "sent" {
				continue
			} else if statusFilter == "failed" && bws.Status != "failed" {
				continue
			}
		}

		result = append(result, bws)
	}

	return result
}

// excludedBrokerSets returns the config's excluded_brokers set and its
// excluded_categories set. One set, not one per spelling: an entry matches a
// broker by ID or by name (broker.Filter on the CLI side does the same, from
// the same broker.ToSet normalization), so the two maps this used to return
// always held identical contents. Shared by getBrokersWithStatus, the
// single-send endpoint and the pending-job resume so every gate reads the
// config the same way - use brokerExcluded to test against it.
func (s *Server) excludedBrokerSets() (brokers, categories map[string]bool) {
	cfg := s.getConfig()
	if cfg == nil {
		return map[string]bool{}, map[string]bool{}
	}
	return broker.ToSet(cfg.Options.ExcludedBrokers), broker.ToSet(cfg.Options.ExcludedCategories)
}

// brokerExcluded reports whether b is named in an excluded_brokers set, by
// ID or by name. The one place that rule is written, so the list gate and
// the three send gates can never disagree about who gets emailed.
func brokerExcluded(excluded map[string]bool, b broker.Broker) bool {
	return excluded[strings.ToLower(strings.TrimSpace(b.ID))] ||
		excluded[strings.ToLower(strings.TrimSpace(b.Name))]
}

func (s *Server) getUniqueValues(getter func(broker.Broker) string) []string {
	seen := make(map[string]bool)
	var vals []string
	for _, b := range s.getBrokerDB().Brokers {
		if v := getter(b); v != "" && !seen[v] {
			seen[v] = true
			vals = append(vals, v)
		}
	}
	return vals
}

func (s *Server) getUniqueCategories() []string {
	return s.getUniqueValues(func(b broker.Broker) string { return b.Category })
}

func (s *Server) getUniqueRegions() []string {
	return s.getUniqueValues(func(b broker.Broker) string { return b.Region })
}

func (s *Server) getStats(profileID string) Stats {
	stats := Stats{
		TotalBrokers: len(s.getBrokerDB().Brokers),
	}

	if s.historyStore != nil {
		_, sent, failed, err := s.historyStore.GetStats(profileID)
		if err == nil {
			stats.Sent = sent
			stats.Failed = failed
		}
	}

	stats.Pending = stats.TotalBrokers - stats.Sent - stats.Failed
	if stats.Pending < 0 {
		stats.Pending = 0
	}

	return stats
}

func (s *Server) getRecentHistory(profileID string, limit int) []history.Record {
	if s.historyStore == nil {
		return nil
	}
	records, _ := s.historyStore.GetRecentRequests(profileID, limit)
	return records
}

func (s *Server) renderPartial(w http.ResponseWriter, name string, data interface{}) {
	tmpl, ok := s.templates[name]
	if !ok {
		http.Error(w, "Template not found: "+name, http.StatusInternalServerError)
		return
	}
	// Execute the template directly without layout wrapper
	err := tmpl.Execute(w, data)
	if err != nil {
		http.Error(w, "Template error: "+err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) renderWithCSRF(w http.ResponseWriter, r *http.Request, name string, data map[string]interface{}) {
	// Add CSRF token to data. Resolved once, not twice: with the
	// Fetch-metadata-based implementation csrf.Token is a stub returning a
	// fresh random string per call, so calling it twice would put two
	// different values in the page. Harmless while tokens are ignored, but
	// wrong the moment anything starts comparing them.
	csrfToken := csrf.Token(r)
	data["CSRFToken"] = csrfToken
	data["CSRFField"] = template.HTML(fmt.Sprintf(`<input type="hidden" name="gorilla.csrf.Token" value="%s">`, csrfToken))

	// Every page gets the profile switcher's data, regardless of whether the
	// handler itself needed the active profile - Profiles has length 1 for a
	// single-profile config, in which case layout.html hides the switcher.
	// Set unconditionally (even pre-setup, when cfg is nil) so layout.html's
	// `len .Profiles` never sees a missing/nil field.
	if cfg := s.getConfig(); cfg != nil {
		data["Profiles"] = cfg.GetProfiles()
	} else {
		data["Profiles"] = []config.NamedProfile{}
	}
	data["ActiveProfile"] = s.activeProfile(r)
	data["CurrentPath"] = r.URL.Path

	tmpl, ok := s.templates[name]
	if !ok {
		http.Error(w, "Template not found: "+name, http.StatusInternalServerError)
		return
	}
	err := tmpl.ExecuteTemplate(w, "layout", data)
	if err != nil {
		http.Error(w, "Template error: "+err.Error(), http.StatusInternalServerError)
	}
}
