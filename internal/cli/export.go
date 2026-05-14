package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/harakeishi/shtrace/internal/report"
	"github.com/harakeishi/shtrace/internal/session"
	"github.com/harakeishi/shtrace/internal/storage"
)

// exportManifest is the top-level descriptor written as manifest.json inside
// the export tarball. OriginalID is set only when --rename was used during a
// previous import that in turn re-exported the session.
type exportManifest struct {
	SessionID    string `json:"session_id"`
	ExportedAt   string `json:"exported_at"`
	ShtraceVersion string `json:"shtrace_version"`
}

// exportSession is the session.json payload inside the tarball.
type exportSession struct {
	ID        string            `json:"id"`
	StartedAt string            `json:"started_at"`
	EndedAt   *string           `json:"ended_at,omitempty"`
	Tags      map[string]string `json:"tags"`
	Spans     []exportSpan      `json:"spans"`
}

type exportSpan struct {
	ID           string   `json:"id"`
	SessionID    string   `json:"session_id"`
	ParentSpanID string   `json:"parent_span_id,omitempty"`
	Command      string   `json:"command"`
	Argv         []string `json:"argv"`
	Cwd          string   `json:"cwd"`
	Mode         string   `json:"mode"`
	StartedAt    string   `json:"started_at"`
	EndedAt      string   `json:"ended_at"`
	ExitCode     *int     `json:"exit_code,omitempty"`
}

const shtraceVersion = "0.1.0"

// runExport implements `shtrace export`.
func runExport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	sessionID, output, latest, withReport, err := parseExportArgs(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		_, _ = fmt.Fprintln(stderr, "usage: shtrace export (--session <id> | --latest) [--with-report] [--output <path.tar.gz>]")
		return 2
	}

	env := envMap()
	dataDir, err := storage.ResolveDataDir(env, runtime.GOOS)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		return 1
	}
	store, err := storage.Open(dataDir + "/sessions.db")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: open store: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: migrate: %v\n", err)
		return 1
	}

	warn := func(e error) { _, _ = fmt.Fprintf(stderr, "shtrace: warning: %v\n", e) }

	if latest {
		sessions, err := store.ListSessions(ctx, 1, warn)
		if err != nil || len(sessions) == 0 {
			_, _ = fmt.Fprintln(stderr, "shtrace: no sessions found")
			return 1
		}
		sessionID = sessions[0].ID
	}

	sess, err := store.GetSession(ctx, sessionID)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: session %s: %v\n", sessionID, err)
		return 1
	}
	spans, err := store.SpansForSession(ctx, sessionID, warn)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: spans for %s: %v\n", sessionID, err)
		return 1
	}

	if output == "" {
		output = fmt.Sprintf("shtrace-%s-%s.tar.gz", sessionID, time.Now().UTC().Format("20060102T150405Z"))
	}

	if err := writeExportTarGz(ctx, output, sess, spans, dataDir, withReport, store, warn); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: export: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "exported session %s to %s\n", sessionID, output)
	return 0
}

func writeExportTarGz(
	ctx context.Context,
	outPath string,
	sess storage.Session,
	spans []storage.Span,
	dataDir string,
	withReport bool,
	src report.Source,
	warn func(error),
) error {
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", outPath, err)
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(outPath)
		}
	}()

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	addJSON := func(name string, v any) error {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		b = append(b, '\n')
		hdr := &tar.Header{
			Name:    "shtrace-export/" + name,
			Mode:    0o644,
			Size:    int64(len(b)),
			ModTime: time.Now().UTC(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = tw.Write(b)
		return err
	}

	manifest := exportManifest{
		SessionID:      sess.ID,
		ExportedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		ShtraceVersion: shtraceVersion,
	}
	if err := addJSON("manifest.json", manifest); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	eSpans := make([]exportSpan, 0, len(spans))
	for _, sp := range spans {
		es := exportSpan{
			ID:           sp.ID,
			SessionID:    sp.SessionID,
			ParentSpanID: sp.ParentSpanID,
			Command:      sp.Command,
			Argv:         sp.Argv,
			Cwd:          sp.Cwd,
			Mode:         sp.Mode,
			StartedAt:    sp.StartedAt.UTC().Format(time.RFC3339Nano),
			EndedAt:      sp.EndedAt.UTC().Format(time.RFC3339Nano),
			ExitCode:     sp.ExitCode,
		}
		eSpans = append(eSpans, es)
	}

	var endedAtStr *string
	if sess.EndedAt != nil {
		s := sess.EndedAt.UTC().Format(time.RFC3339Nano)
		endedAtStr = &s
	}
	eSession := exportSession{
		ID:        sess.ID,
		StartedAt: sess.StartedAt.UTC().Format(time.RFC3339Nano),
		EndedAt:   endedAtStr,
		Tags:      sess.Tags,
		Spans:     eSpans,
	}
	if err := addJSON("session.json", eSession); err != nil {
		return fmt.Errorf("write session.json: %w", err)
	}

	for _, sp := range spans {
		logPath := storage.OutputPath(dataDir, sess.ID, sp.ID)
		logData, err := os.ReadFile(logPath)
		if err != nil {
			warn(fmt.Errorf("read log %s: %v (skipping)", logPath, err))
			continue
		}
		hdr := &tar.Header{
			Name:    "shtrace-export/outputs/" + sp.ID + ".log",
			Mode:    0o644,
			Size:    int64(len(logData)),
			ModTime: time.Now().UTC(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("tar header for %s: %w", sp.ID, err)
		}
		if _, err := tw.Write(logData); err != nil {
			return fmt.Errorf("tar write for %s: %w", sp.ID, err)
		}
	}

	if withReport {
		var buf bytes.Buffer
		if _, err := report.Render(ctx, src, &buf, report.Options{
			SessionID: sess.ID,
			DataDir:   dataDir,
			Warn:      warn,
		}); err != nil {
			return fmt.Errorf("render report: %w", err)
		}
		b := buf.Bytes()
		hdr := &tar.Header{
			Name:    "shtrace-export/report.html",
			Mode:    0o644,
			Size:    int64(len(b)),
			ModTime: time.Now().UTC(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("tar header for report.html: %w", err)
		}
		if _, err := tw.Write(b); err != nil {
			return fmt.Errorf("tar write for report.html: %w", err)
		}
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}
	if err := gw.Close(); err != nil {
		return fmt.Errorf("close gzip: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close file: %w", err)
	}
	ok = true
	return nil
}

func parseExportArgs(args []string) (sessionID, output string, latest, withReport bool, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--session":
			if i+1 >= len(args) {
				return "", "", false, false, fmt.Errorf("--session requires a value")
			}
			sessionID = args[i+1]
			i++
		case strings.HasPrefix(a, "--session="):
			sessionID = strings.TrimPrefix(a, "--session=")
		case a == "--output", a == "-o":
			if i+1 >= len(args) {
				return "", "", false, false, fmt.Errorf("%s requires a value", a)
			}
			output = args[i+1]
			i++
		case strings.HasPrefix(a, "--output="):
			output = strings.TrimPrefix(a, "--output=")
		case a == "--latest":
			latest = true
		case a == "--with-report":
			withReport = true
		default:
			return "", "", false, false, fmt.Errorf("unknown export flag %q", a)
		}
	}
	if sessionID == "" && !latest {
		return "", "", false, false, fmt.Errorf("either --session <id> or --latest is required")
	}
	if sessionID != "" && latest {
		return "", "", false, false, fmt.Errorf("--session and --latest are mutually exclusive")
	}
	return sessionID, output, latest, withReport, nil
}

// runImport implements `shtrace import`.
func runImport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	path, overwrite, rename, err := parseImportArgs(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		_, _ = fmt.Fprintln(stderr, "usage: shtrace import <path.tar.gz> [--overwrite | --rename]")
		return 2
	}

	env := envMap()
	dataDir, err := storage.ResolveDataDir(env, runtime.GOOS)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: mkdir data dir: %v\n", err)
		return 1
	}
	store, err := storage.Open(dataDir + "/sessions.db")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: open store: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: migrate: %v\n", err)
		return 1
	}

	warn := func(e error) { _, _ = fmt.Fprintf(stderr, "shtrace: warning: %v\n", e) }

	result, err := importTarGz(ctx, path, store, dataDir, overwrite, rename, warn)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: import: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "imported session %s", result.SessionID)
	if result.OriginalID != "" {
		_, _ = fmt.Fprintf(stdout, " (renamed from %s)", result.OriginalID)
	}
	_, _ = fmt.Fprintln(stdout)
	return 0
}

type importResult struct {
	SessionID  string
	OriginalID string
}

func importTarGz(ctx context.Context, path string, store *storage.Store, dataDir string, overwrite, rename bool, warn func(error)) (importResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return importResult{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return importResult{}, fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = gr.Close() }()
	tr := tar.NewReader(gr)

	files := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return importResult{}, fmt.Errorf("read tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return importResult{}, fmt.Errorf("read %s: %w", hdr.Name, err)
		}
		// Strip the "shtrace-export/" prefix.
		name := strings.TrimPrefix(hdr.Name, "shtrace-export/")
		files[name] = b
	}

	manifestData, ok := files["manifest.json"]
	if !ok {
		return importResult{}, fmt.Errorf("manifest.json not found in archive")
	}
	var manifest exportManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return importResult{}, fmt.Errorf("parse manifest.json: %w", err)
	}

	sessionData, ok := files["session.json"]
	if !ok {
		return importResult{}, fmt.Errorf("session.json not found in archive")
	}
	var eSession exportSession
	if err := json.Unmarshal(sessionData, &eSession); err != nil {
		return importResult{}, fmt.Errorf("parse session.json: %w", err)
	}

	// Check for session ID collision.
	originalID := ""
	targetID := eSession.ID
	if _, err := store.GetSession(ctx, targetID); err == nil {
		// Session exists.
		switch {
		case overwrite:
			if err := store.DeleteSession(ctx, targetID); err != nil {
				return importResult{}, fmt.Errorf("overwrite: delete existing session %s: %w", targetID, err)
			}
		case rename:
			originalID = targetID
			newID, err := session.DefaultIDGenerator().NewSessionID()
			if err != nil {
				return importResult{}, fmt.Errorf("rename: generate new id: %w", err)
			}
			targetID = newID
		default:
			return importResult{}, fmt.Errorf("session %s already exists; use --overwrite or --rename", targetID)
		}
	}

	startedAt, err := time.Parse(time.RFC3339Nano, eSession.StartedAt)
	if err != nil {
		return importResult{}, fmt.Errorf("parse started_at: %w", err)
	}
	var endedAt *time.Time
	if eSession.EndedAt != nil {
		t, err := time.Parse(time.RFC3339Nano, *eSession.EndedAt)
		if err != nil {
			warn(fmt.Errorf("parse ended_at: %v (ignoring)", err))
		} else {
			endedAt = &t
		}
	}
	if err := store.InsertSession(ctx, storage.Session{
		ID:        targetID,
		StartedAt: startedAt,
		EndedAt:   endedAt,
		Tags:      eSession.Tags,
	}); err != nil {
		return importResult{}, fmt.Errorf("insert session: %w", err)
	}

	for _, es := range eSession.Spans {
		spanStartedAt, err := time.Parse(time.RFC3339Nano, es.StartedAt)
		if err != nil {
			warn(fmt.Errorf("span %s: parse started_at: %v (skipping)", es.ID, err))
			continue
		}
		spanEndedAt, err := time.Parse(time.RFC3339Nano, es.EndedAt)
		if err != nil {
			warn(fmt.Errorf("span %s: parse ended_at: %v (skipping)", es.ID, err))
			continue
		}

		// Span IDs are independent UUIDs; only session_id is rewritten.
		spanID := es.ID

		if err := store.InsertSpan(ctx, storage.Span{
			ID:           spanID,
			SessionID:    targetID,
			ParentSpanID: es.ParentSpanID,
			Command:      es.Command,
			Argv:         es.Argv,
			Cwd:          es.Cwd,
			Mode:         es.Mode,
			StartedAt:    spanStartedAt,
			EndedAt:      spanEndedAt,
			ExitCode:     es.ExitCode,
		}); err != nil {
			warn(fmt.Errorf("span %s: insert: %v (skipping)", spanID, err))
			continue
		}

		// Copy the output log file.
		logKey := "outputs/" + es.ID + ".log"
		logData, ok := files[logKey]
		if !ok {
			warn(fmt.Errorf("span %s: log file not found in archive", es.ID))
			continue
		}
		outPath := storage.OutputPath(dataDir, targetID, spanID)
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			warn(fmt.Errorf("span %s: mkdir: %v", spanID, err))
			continue
		}
		if err := os.WriteFile(outPath, logData, 0o644); err != nil {
			warn(fmt.Errorf("span %s: write log: %v", spanID, err))
		}
	}

	return importResult{SessionID: targetID, OriginalID: originalID}, nil
}

func parseImportArgs(args []string) (path string, overwrite, rename bool, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--overwrite":
			overwrite = true
		case "--rename":
			rename = true
		default:
			if strings.HasPrefix(a, "--") {
				return "", false, false, fmt.Errorf("unknown import flag %q", a)
			}
			if path != "" {
				return "", false, false, fmt.Errorf("unexpected argument %q (path already set to %q)", a, path)
			}
			path = a
		}
	}
	if path == "" {
		return "", false, false, fmt.Errorf("path to .tar.gz is required")
	}
	if overwrite && rename {
		return "", false, false, fmt.Errorf("--overwrite and --rename are mutually exclusive")
	}
	return path, overwrite, rename, nil
}
