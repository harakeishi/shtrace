// Package report renders a single session into a self-contained HTML
// document. The output is a single file (CSS inline, no external assets) so
// that reviewers can open it with `file://` after `gh run download` without
// any server or asset hosting.
package report

import (
	"context"
	_ "embed"
	"encoding/json"
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
type Source interface {
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

// Chunk is one rendered line in the timeline view.
type chunkView struct {
	TS     string
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
	EndedAt      string
	DurationMS   int64
	ExitCode     string
	ExitClass    string // "ok" | "fail" | "unknown"
	Chunks       []chunkView
	CorruptLines int
}

type pageData struct {
	SessionID     string
	StartedAt     string
	EndedAt       string
	Tags          map[string]string
	ShtraceVer    string
	GeneratedAt   string
	Spans         []spanView
	TotalChunks   int
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
		// Pull the session row via ListSessions and scan for the match.
		// The store doesn't expose a single-row getter today; rather than
		// add one just for the report path, we ask for a reasonable window
		// and look up the ID. 1000 covers every realistic local store.
		ss, err := src.ListSessions(ctx, 1000, opts.Warn)
		if err != nil {
			return "", fmt.Errorf("report: list sessions: %w", err)
		}
		for i := range ss {
			if ss[i].ID == sessionID {
				sess = &ss[i]
				break
			}
		}
		if sess == nil {
			return "", fmt.Errorf("report: session %s not found", sessionID)
		}
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
		Tags:        sess.Tags,
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
			EndedAt:      sp.EndedAt.UTC().Format(time.RFC3339Nano),
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
			out = append(out, chunkView{TS: c.TS, Stream: c.Stream, Data: c.Data})
		}
	}
	return out, corrupt, nil
}
