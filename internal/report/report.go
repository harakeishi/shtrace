// Package report renders a single session into a self-contained HTML
// document. The output is a single file (CSS inline, no external assets) so
// that reviewers can open it with `file://` after `gh run download` without
// any server or asset hosting.
package report

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"os"
	"sort"
	"time"

	"github.com/harakeishi/shtrace/internal/storage"
)

//go:embed template.html
var htmlTemplate string

// Source is the minimum interface the renderer needs from storage. The
// concrete *storage.Store satisfies it; tests can supply a fake.
//
// GetSession returns storage.ErrSessionNotFound for an absent id and a
// wrapped storage.ErrSessionCorrupt for a row whose columns fail to parse,
// so the renderer can surface the distinction to the user.
type Source interface {
	GetSession(ctx context.Context, id string) (storage.Session, error)
	ListSessions(ctx context.Context, limit int, warn func(error)) ([]storage.Session, error)
	SpansForSession(ctx context.Context, sessionID string, warn func(error)) ([]storage.Span, error)
}

// Options controls a single Render call.
type Options struct {
	// SessionID picks the session to render. If empty, Latest must be true.
	SessionID string
	// Latest renders the most recently started session in the store.
	Latest bool
	// DataDir is the base shtrace data dir; output logs live under
	// DataDir/outputs/<session>/<span>.log.
	DataDir string
	// Warn receives non-fatal per-row parse failures from the store. nil is
	// allowed and silences warnings.
	Warn func(error)
}

// chunkView is one rendered line in the timeline view. Stream maps to a
// CSS class via the template's streamClass funcmap; Data is the recorded
// payload after sanitizeForHTML stripping. The per-chunk timestamp is
// intentionally omitted because the template does not display it — adding
// it back is cheap when a future timeline layout wants it.
type chunkView struct {
	Stream string
	Data   string
}

type spanView struct {
	ID           string
	ParentSpanID string
	Command      string
	Argv         string
	Cwd          string
	Mode         string
	StartedAt    string
	DurationMS   int64
	ExitCode     string
	ExitClass    string // "ok" | "fail" | "unknown"
	Chunks       []chunkView
	CorruptLines int
}

// tagKV materialises a map[string]string into a deterministic, sortable
// shape so two `shtrace report` calls against the same session produce
// byte-identical HTML. Map iteration order in Go is randomised per-run.
type tagKV struct {
	K string
	V string
}

type pageData struct {
	SessionID   string
	StartedAt   string
	EndedAt     string
	Tags        []tagKV
	GeneratedAt string
	Spans       []spanView
	TotalChunks int
}

// Render writes a self-contained HTML report for the requested session to w.
//
// It returns the resolved session ID (helpful when Latest was used) so the
// caller can include it in a CLI status line.
func Render(ctx context.Context, src Source, w io.Writer, opts Options) (string, error) {
	if opts.SessionID == "" && !opts.Latest {
		return "", fmt.Errorf("report: SessionID or Latest is required")
	}

	sessionID := opts.SessionID
	var sess *storage.Session
	if opts.Latest {
		// limit=1 is enough: ListSessions returns newest-first.
		ss, err := src.ListSessions(ctx, 1, opts.Warn)
		if err != nil {
			return "", fmt.Errorf("report: list sessions: %w", err)
		}
		if len(ss) == 0 {
			return "", fmt.Errorf("report: no sessions found")
		}
		sessionID = ss[0].ID
		sess = &ss[0]
	} else {
		// Look up the row directly so a corrupt target session surfaces as
		// "corrupt", not "not found" — and so we don't scan the whole table
		// for a single id.
		s, err := src.GetSession(ctx, sessionID)
		switch {
		case errors.Is(err, storage.ErrSessionNotFound):
			return "", fmt.Errorf("report: session %s not found", sessionID)
		case errors.Is(err, storage.ErrSessionCorrupt):
			return "", fmt.Errorf("report: session %s row is corrupt: %w", sessionID, err)
		case err != nil:
			return "", fmt.Errorf("report: get session: %w", err)
		}
		sess = &s
	}

	spans, err := src.SpansForSession(ctx, sessionID, opts.Warn)
	if err != nil {
		return "", fmt.Errorf("report: spans: %w", err)
	}

	// SpansForSession already orders by started_at ASC; re-sort defensively
	// so a future schema change can't silently scramble the timeline.
	sort.SliceStable(spans, func(i, j int) bool {
		if spans[i].StartedAt.Equal(spans[j].StartedAt) {
			return spans[i].ID < spans[j].ID
		}
		return spans[i].StartedAt.Before(spans[j].StartedAt)
	})

	page := pageData{
		SessionID:   sess.ID,
		StartedAt:   sess.StartedAt.UTC().Format(time.RFC3339),
		Tags:        sortTags(sess.Tags),
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if sess.EndedAt != nil {
		page.EndedAt = sess.EndedAt.UTC().Format(time.RFC3339)
	}

	for _, sp := range spans {
		argvJSON, _ := json.Marshal(sp.Argv)
		sv := spanView{
			ID:           sp.ID,
			ParentSpanID: sp.ParentSpanID,
			Command:      sp.Command,
			Argv:         string(argvJSON),
			Cwd:          sp.Cwd,
			Mode:         sp.Mode,
			StartedAt:    sp.StartedAt.UTC().Format(time.RFC3339Nano),
			DurationMS:   sp.EndedAt.Sub(sp.StartedAt).Milliseconds(),
		}
		switch {
		case sp.ExitCode == nil:
			sv.ExitCode = "?"
			sv.ExitClass = "unknown"
		case *sp.ExitCode == 0:
			sv.ExitCode = "0"
			sv.ExitClass = "ok"
		default:
			sv.ExitCode = fmt.Sprintf("%d", *sp.ExitCode)
			sv.ExitClass = "fail"
		}

		// Load and parse the JSONL log for this span.
		logPath := storage.OutputPath(opts.DataDir, sessionID, sp.ID)
		chunks, corrupt, readErr := readChunks(logPath)
		if readErr != nil && !os.IsNotExist(readErr) {
			// A missing log just means the span had no output (or its file
			// was pruned). Anything else is reported via warn and the span
			// still renders without chunks.
			if opts.Warn != nil {
				opts.Warn(fmt.Errorf("report: read log %s: %w", logPath, readErr))
			}
		}
		sv.Chunks = chunks
		sv.CorruptLines = corrupt
		page.Spans = append(page.Spans, sv)
		page.TotalChunks += len(chunks)
	}

	tmpl, err := template.New("report").Funcs(template.FuncMap{
		// stream returns a CSS class name for a chunk's stream label so the
		// CSS can colour stdout/stderr/pty distinctly.
		"streamClass": func(s string) string {
			switch s {
			case "stderr":
				return "stream-stderr"
			case "pty":
				return "stream-pty"
			default:
				return "stream-stdout"
			}
		},
	}).Parse(htmlTemplate)
	if err != nil {
		return "", fmt.Errorf("report: parse template: %w", err)
	}
	if err := tmpl.Execute(w, page); err != nil {
		return "", fmt.Errorf("report: execute template: %w", err)
	}
	return sessionID, nil
}

// readChunks loads a span's JSONL log and returns the parsed chunks plus a
// count of lines that failed to parse. A missing file returns os.IsNotExist;
// other I/O errors are surfaced.
//
// Each chunk's Data is sanitised with sanitizeForHTML so raw C0 control
// bytes (e.g. \x1b from ANSI escape sequences, NULs) do not bleed into the
// browser as invisible or display-corrupting glyphs. Full ANSI-to-HTML
// rendering is a Phase 4 follow-up (#15); this is the minimum that keeps
// recorded output readable in the meantime.
func readChunks(path string) ([]chunkView, int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	var out []chunkView
	corrupt := 0
	start := 0
	for i := 0; i <= len(b); i++ {
		if i == len(b) || b[i] == '\n' {
			line := b[start:i]
			start = i + 1
			if len(line) == 0 {
				continue
			}
			var c storage.Chunk
			if err := json.Unmarshal(line, &c); err != nil {
				corrupt++
				continue
			}
			out = append(out, chunkView{Stream: c.Stream, Data: sanitizeForHTML(c.Data)})
		}
	}
	return out, corrupt, nil
}

// sanitizeForHTML strips C0 control bytes (0x00–0x1F) other than \t, \n, and
// \r from s. Those three are normal text-formatting characters in a <pre>
// block; the rest (most importantly \x1b, which starts every ANSI escape
// sequence) render as invisible glyphs or U+FFFD in a browser and produce
// surprising visual artifacts in the report. Stripping them leaves the
// surrounding readable text intact — e.g. "\x1b[31mhello" becomes "[31mhello",
// which is noisy but at least legible.
//
// 0x7F (DEL) is also stripped. Bytes ≥ 0x80 are passed through unchanged so
// multi-byte UTF-8 sequences survive; html/template still encodes < > & " '.
func sanitizeForHTML(s string) string {
	// Fast path: scan once, only allocate if we actually find a byte to drop.
	for i := 0; i < len(s); i++ {
		if isControlByte(s[i]) {
			return sanitizeSlow(s)
		}
	}
	return s
}

func sanitizeSlow(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if !isControlByte(s[i]) {
			out = append(out, s[i])
		}
	}
	return string(out)
}

func isControlByte(b byte) bool {
	return (b < 0x20 && b != '\t' && b != '\n' && b != '\r') || b == 0x7f
}

// sortTags converts a tag map to a slice ordered by key so two renders of
// the same session emit byte-identical HTML (callers diff reports across CI
// runs).
func sortTags(m map[string]string) []tagKV {
	out := make([]tagKV, 0, len(m))
	for k, v := range m {
		out = append(out, tagKV{K: k, V: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].K < out[j].K })
	return out
}
