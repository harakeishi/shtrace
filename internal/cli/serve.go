package cli

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/harakeishi/shtrace/internal/storage"
)

const defaultServePort = 7474

// serveUsage is the canonical help line for shtrace serve.
const serveUsage = "usage: shtrace serve [--port <port>]"

func parseServeArgs(args []string) (port int, err error) {
	port = defaultServePort
	for i := 0; i < len(args); i++ {
		a := args[i]
		var raw string
		switch {
		case a == "--port":
			if i+1 >= len(args) {
				return 0, fmt.Errorf("--port requires a value")
			}
			raw = args[i+1]
			i++
		case strings.HasPrefix(a, "--port="):
			raw = strings.TrimPrefix(a, "--port=")
		default:
			return 0, fmt.Errorf("unknown serve flag %q", a)
		}
		n, parseErr := strconv.Atoi(raw)
		if parseErr != nil || n < 1 || n > 65535 {
			return 0, fmt.Errorf("--port %q is not a valid port number", raw)
		}
		port = n
	}
	return port, nil
}

func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	port, err := parseServeArgs(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		_, _ = fmt.Fprintln(stderr, serveUsage)
		return 2
	}

	env := envMap()
	dataDir, err := storage.ResolveDataDir(env, runtime.GOOS)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		return 1
	}

	store, err := storage.Open(filepath.Join(dataDir, "sessions.db"))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: open store: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: migrate: %v\n", err)
		return 1
	}

	var fts *storage.FTSStore
	ftsPath := storage.FTSPath(dataDir)
	if _, statErr := os.Stat(ftsPath); statErr == nil {
		ftsStore, ftsErr := storage.OpenFTS(ftsPath)
		if ftsErr != nil {
			_, _ = fmt.Fprintf(stderr, "shtrace: open fts (search disabled): %v\n", ftsErr)
		} else if migrErr := ftsStore.MigrateFTS(ctx); migrErr != nil {
			_, _ = fmt.Fprintf(stderr, "shtrace: fts migrate (search disabled): %v\n", migrErr)
			_ = ftsStore.Close()
		} else {
			fts = ftsStore
			defer func() { _ = fts.Close() }()
		}
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: listen %s: %v\n", addr, err)
		return 1
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/sessions", makeSessionsHandler(store))
	mux.HandleFunc("/api/spans", makeAllSpansHandler(store))
	mux.HandleFunc("/api/sessions/", makeSpansHandler(store))
	mux.HandleFunc("/api/output/", makeOutputHandler(store, dataDir))
	mux.HandleFunc("/api/search", makeSearchHandler(fts))
	mux.HandleFunc("/", makeUIHandler())

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	_, _ = fmt.Fprintf(stdout, "shtrace serve: listening on http://%s  (Ctrl-C to stop)\n", addr)

	serveDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if shutErr := srv.Shutdown(shutCtx); shutErr != nil {
				_, _ = fmt.Fprintf(stderr, "shtrace: serve shutdown: %v\n", shutErr)
			}
		case <-serveDone:
		}
	}()

	serveErr := srv.Serve(ln)
	close(serveDone)
	if serveErr != nil && serveErr != http.ErrServerClosed {
		_, _ = fmt.Fprintf(stderr, "shtrace: serve: %v\n", serveErr)
		return 1
	}
	return 0
}

// writeJSON sends v as JSON with a 200 status. Marshalling errors are reported
// as 500 because they indicate a programming bug, not a client error.
func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

type apiSession struct {
	ID        string            `json:"id"`
	StartedAt string            `json:"started_at"`
	EndedAt   *string           `json:"ended_at"`
	Tags      map[string]string `json:"tags"`
	Label     string            `json:"label,omitempty"`
}

type apiSpan struct {
	ID           string   `json:"id"`
	SessionID    string   `json:"session_id"`
	ParentSpanID string   `json:"parent_span_id"`
	Command      string   `json:"command"`
	Argv         []string `json:"argv"`
	Cwd          string   `json:"cwd"`
	Mode         string   `json:"mode"`
	StartedAt    string   `json:"started_at"`
	EndedAt      string   `json:"ended_at"`
	ExitCode     *int     `json:"exit_code"`
	Group        string   `json:"group"`
}

// apiSpansPage is the /api/spans payload: the span timeline plus the sessions
// those spans belong to, so the UI can label session chips without a second
// round trip per session.
type apiSpansPage struct {
	Spans    []apiSpan    `json:"spans"`
	Sessions []apiSession `json:"sessions"`
}

type apiSearchResult struct {
	SpanID    string `json:"span_id"`
	SessionID string `json:"session_id"`
	Snippet   string `json:"snippet"`
}

func toAPISpan(sp storage.Span) apiSpan {
	return apiSpan{
		ID:           sp.ID,
		SessionID:    sp.SessionID,
		ParentSpanID: sp.ParentSpanID,
		Command:      sp.Command,
		Argv:         sp.Argv,
		Cwd:          sp.Cwd,
		Mode:         sp.Mode,
		StartedAt:    sp.StartedAt.Format(time.RFC3339),
		EndedAt:      sp.EndedAt.Format(time.RFC3339),
		ExitCode:     sp.ExitCode,
		Group:        commandGroup(sp.Argv),
	}
}

func toAPISession(sess storage.Session, label string) apiSession {
	a := apiSession{
		ID:        sess.ID,
		StartedAt: sess.StartedAt.Format(time.RFC3339),
		Tags:      sess.Tags,
		Label:     label,
	}
	if sess.EndedAt != nil {
		t := sess.EndedAt.Format(time.RFC3339)
		a.EndedAt = &t
	}
	return a
}

// sessionLabel is the human-readable name for a session: the argv of its
// earliest root span. Returns "" when the session has no root span, so the UI
// falls back to the bare id.
func sessionLabel(roots map[string][]string, sessionID string) string {
	argv, ok := roots[sessionID]
	if !ok || len(argv) == 0 {
		return ""
	}
	return strings.Join(argv, " ")
}

func makeSessionsHandler(store *storage.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		const sessionCap = 500
		// Request one extra to detect whether the list was capped.
		sessions, err := store.ListSessions(r.Context(), sessionCap+1, nil)
		if err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		capped := len(sessions) > sessionCap
		if capped {
			sessions = sessions[:sessionCap]
		}
		roots, err := store.RootSpanCommands(r.Context(), nil)
		if err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		out := make([]apiSession, 0, len(sessions))
		for _, s := range sessions {
			out = append(out, toAPISession(s, sessionLabel(roots, s.ID)))
		}
		// Marshal before setting any headers so that a marshal failure
		// (http.Error → 500) does not emit X-Shtrace-Sessions-Capped
		// alongside a non-200 status, which would be contradictory.
		b, marshalErr := json.Marshal(out)
		if marshalErr != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if capped {
			w.Header().Set("X-Shtrace-Sessions-Capped", "true")
		}
		_, _ = w.Write(b)
	}
}

// makeAllSpansHandler serves the cross-session span timeline that the web UI's
// list view renders, together with the sessions those spans belong to.
func makeAllSpansHandler(store *storage.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		const (
			defaultSpanLimit = 1000
			maxSpanLimit     = 5000
		)
		limit := defaultSpanLimit
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, convErr := strconv.Atoi(raw)
			if convErr != nil || n < 1 || n > maxSpanLimit {
				http.Error(w, "limit must be an integer between 1 and 5000", http.StatusBadRequest)
				return
			}
			limit = n
		}

		// Request one extra to detect whether the list was capped.
		spans, err := store.RecentSpans(r.Context(), limit+1, nil)
		if err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		capped := len(spans) > limit
		if capped {
			spans = spans[:limit]
		}

		roots, err := store.RootSpanCommands(r.Context(), nil)
		if err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		sessions, err := store.ListSessions(r.Context(), maxSpanLimit, nil)
		if err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}

		needed := make(map[string]bool, len(spans))
		for _, sp := range spans {
			needed[sp.SessionID] = true
		}
		page := apiSpansPage{
			Spans:    make([]apiSpan, 0, len(spans)),
			Sessions: make([]apiSession, 0, len(needed)),
		}
		for _, sp := range spans {
			page.Spans = append(page.Spans, toAPISpan(sp))
		}
		for _, sess := range sessions {
			if !needed[sess.ID] {
				continue
			}
			page.Sessions = append(page.Sessions, toAPISession(sess, sessionLabel(roots, sess.ID)))
		}

		b, marshalErr := json.Marshal(page)
		if marshalErr != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if capped {
			w.Header().Set("X-Shtrace-Spans-Capped", "true")
		}
		_, _ = w.Write(b)
	}
}

func makeSpansHandler(store *storage.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// path: /api/sessions/{id}/spans
		path := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
		if !strings.HasSuffix(path, "/spans") {
			http.NotFound(w, r)
			return
		}
		sessionID := strings.TrimSuffix(path, "/spans")
		if sessionID == "" || strings.Contains(sessionID, "/") || strings.Contains(sessionID, "..") {
			http.Error(w, "invalid session id", http.StatusBadRequest)
			return
		}

		if _, err := store.GetSession(r.Context(), sessionID); err != nil {
			if errors.Is(err, storage.ErrSessionNotFound) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}

		spans, err := store.SpansForSession(r.Context(), sessionID, nil)
		if err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		out := make([]apiSpan, 0, len(spans))
		for _, sp := range spans {
			out = append(out, toAPISpan(sp))
		}
		writeJSON(w, out)
	}
}

func makeOutputHandler(store *storage.Store, dataDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// path: /api/output/{sessionID}/{spanID}
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/output/"), "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			http.Error(w, "usage: /api/output/{sessionID}/{spanID}", http.StatusBadRequest)
			return
		}
		sessionID, spanID := parts[0], parts[1]
		// SplitN(…, 2) guarantees parts[0] (sessionID) never contains "/".
		// parts[1] (spanID) may contain "/" when extra path segments are present.
		if strings.Contains(spanID, "/") {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		// Reject ".." components to prevent path traversal even if crafted DB
		// rows bypass the SpanExists guard.
		if strings.Contains(sessionID, "..") || strings.Contains(spanID, "..") {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}

		// Validate the span belongs to the session before serving the file.
		// Uses an indexed single-row lookup to prevent path traversal via
		// crafted IDs without loading all spans into memory.
		ok, err := store.SpanExists(r.Context(), sessionID, spanID)
		if err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "span not found", http.StatusNotFound)
			return
		}

		logPath := storage.OutputPath(dataDir, sessionID, spanID)
		f, err := os.Open(logPath)
		if err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "output file not found", http.StatusNotFound)
				return
			}
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		defer func() { _ = f.Close() }()

		const maxLogBytes = 10 << 20 // 10 MiB
		b, err := io.ReadAll(io.LimitReader(f, maxLogBytes+1))
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		truncated := len(b) > maxLogBytes
		if truncated {
			// Trim to the last complete newline so we never feed a
			// half-written JSON line to the decoder, which would
			// incorrectly increment the corrupt counter.
			if nl := bytes.LastIndexByte(b[:maxLogBytes], '\n'); nl >= 0 {
				b = b[:nl+1]
			} else {
				b = b[:maxLogBytes]
			}
		}

		// Decode JSON Lines and return plain text with ANSI escape sequences stripped.
		var sb strings.Builder
		corrupt := 0
		for _, line := range splitLines(b) {
			if len(line) == 0 {
				continue
			}
			var c storage.Chunk
			if err := json.Unmarshal(line, &c); err != nil {
				corrupt++
				continue
			}
			sb.WriteString(stripANSI(c.Data))
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if corrupt > 0 {
			w.Header().Set("X-Shtrace-Corrupt-Lines", strconv.Itoa(corrupt))
		}
		if truncated {
			w.Header().Set("X-Shtrace-Truncated", "true")
		}
		_, _ = io.WriteString(w, sb.String())
	}
}

func makeSearchHandler(fts *storage.FTSStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if fts == nil {
			http.Error(w, "search index not available — run 'shtrace reindex' first", http.StatusServiceUnavailable)
			return
		}
		q := r.URL.Query().Get("q")
		if q == "" {
			writeJSON(w, []apiSearchResult{})
			return
		}
		results, err := fts.Search(r.Context(), q, 50)
		if err != nil {
			http.Error(w, "search error", http.StatusInternalServerError)
			return
		}
		out := make([]apiSearchResult, 0, len(results))
		for _, res := range results {
			out = append(out, apiSearchResult{
				SpanID:    res.SpanID,
				SessionID: res.SessionID,
				Snippet:   res.Snippet,
			})
		}
		writeJSON(w, out)
	}
}

// ansiEscapeRe matches ANSI/VT100 escape sequences.
// Order matters: OSC and CSI must be tried before the single-char fallback
// so that \x1b] and \x1b[ aren't consumed by the shorter alternative first.
var ansiEscapeRe = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])`)

// stripANSI removes ANSI escape sequences from s.
func stripANSI(s string) string {
	return ansiEscapeRe.ReplaceAllString(s, "")
}


//go:embed ui
var uiFS embed.FS

// uiContentSecurityPolicy locks the UI down to same-origin assets. Serving CSS
// and JS as separate files removes the need for 'unsafe-inline'.
const uiContentSecurityPolicy = "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func makeUIHandler() http.HandlerFunc {
	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		panic("embed ui: " + err.Error())
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		if !fs.ValidPath(name) {
			http.NotFound(w, r)
			return
		}
		b, readErr := fs.ReadFile(sub, name)
		if readErr != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", uiContentType(name))
		w.Header().Set("Content-Security-Policy", uiContentSecurityPolicy)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(b)))
			return
		}
		_, _ = w.Write(b)
	}
}

func uiContentType(name string) string {
	switch filepath.Ext(name) {
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	default:
		return "text/html; charset=utf-8"
	}
}
