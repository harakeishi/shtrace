package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// FTSStore wraps the outputs.idx SQLite FTS5 database.
// The main sessions.db holds metadata; this database indexes the text content
// of recorded outputs so that search_commands (MCP Phase 2) can do full-text
// queries without touching the raw JSON Lines files.
type FTSStore struct {
	db *sql.DB
}

// SearchResult is one hit from a full-text search.
type SearchResult struct {
	SpanID    string
	SessionID string
	Snippet   string
}

// OpenFTS opens (or creates) the FTS index at path.
func OpenFTS(path string) (*FTSStore, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open fts sqlite: %w", err)
	}
	return &FTSStore{db: db}, nil
}

// Close releases the underlying *sql.DB.
func (f *FTSStore) Close() error { return f.db.Close() }

// MigrateFTS creates the FTS5 virtual table and the content-tracking table if
// they do not exist. Safe to call on every startup.
func (f *FTSStore) MigrateFTS(ctx context.Context) error {
	stmts := []string{
		// shadow table that stores the raw text so we can rebuild the FTS
		// index from it (used by Reindex).
		`CREATE TABLE IF NOT EXISTS span_contents (
			span_id    TEXT NOT NULL,
			session_id TEXT NOT NULL,
			content    TEXT NOT NULL,
			PRIMARY KEY (span_id)
		)`,
		// FTS5 content table backed by span_contents.
		// content= makes FTS5 read from span_contents for highlight/snippet;
		// content_rowid= links the rowid back to span_contents.rowid.
		`CREATE VIRTUAL TABLE IF NOT EXISTS outputs_fts USING fts5(
			span_id,
			session_id,
			content,
			content='span_contents',
			content_rowid='rowid'
		)`,
		// Triggers keep the FTS index in sync with span_contents.
		`CREATE TRIGGER IF NOT EXISTS span_contents_ai
			AFTER INSERT ON span_contents BEGIN
				INSERT INTO outputs_fts(rowid, span_id, session_id, content)
				VALUES (new.rowid, new.span_id, new.session_id, new.content);
			END`,
		`CREATE TRIGGER IF NOT EXISTS span_contents_ad
			AFTER DELETE ON span_contents BEGIN
				INSERT INTO outputs_fts(outputs_fts, rowid, span_id, session_id, content)
				VALUES ('delete', old.rowid, old.span_id, old.session_id, old.content);
			END`,
		`CREATE TRIGGER IF NOT EXISTS span_contents_au
			AFTER UPDATE ON span_contents BEGIN
				INSERT INTO outputs_fts(outputs_fts, rowid, span_id, session_id, content)
				VALUES ('delete', old.rowid, old.span_id, old.session_id, old.content);
				INSERT INTO outputs_fts(rowid, span_id, session_id, content)
				VALUES (new.rowid, new.span_id, new.session_id, new.content);
			END`,
	}
	for _, q := range stmts {
		if _, err := f.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("fts migrate (%.40q): %w", q, err)
		}
	}
	return nil
}

// IndexSpan reads the JSON Lines log at logPath, concatenates all chunk data,
// and inserts (or replaces) a row in the FTS index for the given span. Chunks
// with stream="stderr" are included so that error output is also searchable.
func (f *FTSStore) IndexSpan(ctx context.Context, spanID, sessionID, logPath string) error {
	raw, err := os.ReadFile(logPath)
	if err != nil {
		return fmt.Errorf("fts index: read %s: %w", logPath, err)
	}

	var sb strings.Builder
	for _, line := range splitFTSLines(raw) {
		if len(line) == 0 {
			continue
		}
		var c Chunk
		if err := json.Unmarshal(line, &c); err != nil {
			continue // skip corrupt lines; index whatever we can
		}
		sb.WriteString(c.Data)
		sb.WriteByte('\n')
	}

	_, err = f.db.ExecContext(ctx, `
		INSERT INTO span_contents(span_id, session_id, content)
		VALUES (?, ?, ?)
		ON CONFLICT(span_id) DO UPDATE SET
			session_id = excluded.session_id,
			content    = excluded.content`,
		spanID, sessionID, sb.String(),
	)
	return err
}

// Search performs a full-text query and returns up to limit results.
// query uses standard SQLite FTS5 query syntax (e.g. "error AND build").
func (f *FTSStore) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := f.db.QueryContext(ctx, `
		SELECT sc.span_id, sc.session_id,
		       snippet(outputs_fts, 2, '[', ']', '...', 20)
		FROM outputs_fts
		JOIN span_contents sc ON sc.rowid = outputs_fts.rowid
		WHERE outputs_fts MATCH ?
		ORDER BY rank
		LIMIT ?`, query, limit)
	if err != nil {
		return nil, fmt.Errorf("fts search: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.SpanID, &r.SessionID, &r.Snippet); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Reindex drops and rebuilds the FTS index from the raw JSON Lines files on
// disk. Call this when the index is suspected to be corrupt or out-of-date.
// baseDir is the shtrace data directory (same as passed to OutputPath).
// store is the metadata Store used to enumerate sessions and spans.
func ReindexAll(ctx context.Context, fts *FTSStore, store *Store, baseDir string) error {
	// Wipe the FTS content table; triggers will propagate deletes to the
	// virtual table automatically.
	if _, err := fts.db.ExecContext(ctx, `DELETE FROM span_contents`); err != nil {
		return fmt.Errorf("reindex: clear content: %w", err)
	}

	sessions, err := store.ListSessions(ctx, 0, nil)
	if err != nil {
		return fmt.Errorf("reindex: list sessions: %w", err)
	}

	for _, sess := range sessions {
		spans, err := store.SpansForSession(ctx, sess.ID, nil)
		if err != nil {
			return fmt.Errorf("reindex: spans for %s: %w", sess.ID, err)
		}
		for _, sp := range spans {
			logPath := OutputPath(baseDir, sess.ID, sp.ID)
			if _, statErr := os.Stat(logPath); statErr != nil {
				continue // log file missing — skip silently
			}
			if err := fts.IndexSpan(ctx, sp.ID, sess.ID, logPath); err != nil {
				// Non-fatal: report but continue with remaining spans.
				fmt.Fprintf(os.Stderr, "shtrace: reindex span %s: %v\n", sp.ID, err)
			}
		}
	}
	return nil
}

// FTSPath returns the canonical path of the FTS index file given the data dir.
func FTSPath(baseDir string) string {
	return filepath.Join(baseDir, "outputs.idx")
}

func splitFTSLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			out = append(out, b[start:i])
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}
