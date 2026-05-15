package storage

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
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
// SetMaxOpenConns(1) ensures the WAL/busy_timeout pragmas applied at open time
// stay in effect for every query (modernc.org/sqlite opens a new file handle
// per connection; a single connection sidesteps needing to re-apply them).
// Pragmas are executed as SQL statements rather than embedded in the DSN to
// avoid implementation-dependent URI parsing behaviour.
func OpenFTS(path string) (*FTSStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open fts sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open fts sqlite pragma: %w", err)
		}
	}
	return &FTSStore{db: db}, nil
}

// Close releases the underlying *sql.DB.
func (f *FTSStore) Close() error { return f.db.Close() }

// MigrateFTS creates the FTS5 virtual table and the content-tracking table if
// they do not exist. All DDL runs inside a single transaction so a partial
// failure does not leave an inconsistent schema. Safe to call on every startup.
func (f *FTSStore) MigrateFTS(ctx context.Context) error {
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("fts migrate: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmts := []string{
		// shadow table that stores the raw text so we can rebuild the FTS
		// index from it (used by ReindexAll).
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
	for i, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("fts migrate stmt %d (%.40q): %w", i, q, err)
		}
	}
	return tx.Commit()
}

// IndexSpan reads the JSON Lines log at logPath, concatenates all chunk data,
// and inserts (or replaces) a row in the FTS index for the given span. Chunks
// with stream="stderr" are included so that error output is also searchable.
// The file is read line-by-line to avoid loading large log files into memory.
//
// MigrateFTS must be called before IndexSpan; calling IndexSpan on an
// uninitialised store will return a "no such table" error.
func (f *FTSStore) IndexSpan(ctx context.Context, spanID, sessionID, logPath string) error {
	content, err := readLogContent(logPath)
	if err != nil {
		return fmt.Errorf("fts index: %w", err)
	}
	_, err = f.db.ExecContext(ctx, `
		INSERT INTO span_contents(span_id, session_id, content)
		VALUES (?, ?, ?)
		ON CONFLICT(span_id) DO UPDATE SET
			session_id = excluded.session_id,
			content    = excluded.content`,
		spanID, sessionID, content,
	)
	if err != nil {
		return fmt.Errorf("fts index: upsert span %s: %w", spanID, err)
	}
	return nil
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

// ReindexAll atomically rebuilds the FTS index from the raw JSON Lines files
// on disk. Call this when the index is suspected to be corrupt or out-of-date.
// baseDir is the shtrace data directory (same as passed to OutputPath).
// store is the metadata Store used to enumerate sessions and spans.
//
// The entire operation runs inside a single transaction so callers always see
// either the old index or the fully rebuilt one — never a partial state.
// The span_contents_ai trigger propagates each INSERT to outputs_fts, so no
// manual 'rebuild' command is needed after the re-insert phase.
func ReindexAll(ctx context.Context, fts *FTSStore, store *Store, baseDir string, warn func(error)) error {
	// math.MaxInt32 fetches all sessions; passing 0 would silently cap at 50.
	sessions, err := store.ListSessions(ctx, math.MaxInt32, nil)
	if err != nil {
		return fmt.Errorf("reindex: list sessions: %w", err)
	}

	tx, err := fts.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reindex: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Clear the content table; triggers propagate the deletes to outputs_fts.
	if _, err := tx.ExecContext(ctx, `DELETE FROM span_contents`); err != nil {
		return fmt.Errorf("reindex: clear content: %w", err)
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
			content, readErr := readLogContent(logPath)
			if readErr != nil {
				// Non-fatal: report but continue with remaining spans.
				if warn != nil {
					warn(fmt.Errorf("reindex span %s: %w", sp.ID, readErr))
				}
				continue
			}
			// The span_contents_ai trigger propagates each insert to outputs_fts.
			// Do NOT call 'rebuild' afterwards: triggers already populate the FTS
			// index, and an additional 'rebuild' would double every entry.
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO span_contents(span_id, session_id, content)
				VALUES (?, ?, ?)`,
				sp.ID, sess.ID, content,
			); err != nil {
				return fmt.Errorf("reindex: insert span %s: %w", sp.ID, err)
			}
		}
	}

	return tx.Commit()
}

// DeleteSessionIndex removes all span_contents entries for sessionID from the
// FTS index. The triggers propagate the deletes to outputs_fts automatically.
func (f *FTSStore) DeleteSessionIndex(ctx context.Context, sessionID string) error {
	_, err := f.db.ExecContext(ctx, `DELETE FROM span_contents WHERE session_id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("fts delete session %s: %w", sessionID, err)
	}
	return nil
}

// FTSPath returns the canonical path of the FTS index file given the data dir.
func FTSPath(baseDir string) string {
	return filepath.Join(baseDir, "outputs.idx")
}

// readLogContent opens a JSON Lines log file and concatenates the Data field
// of every Chunk. The file is scanned line by line to avoid loading large
// files into memory at once. Corrupt lines are silently skipped so that a
// single bad entry does not block indexing of the rest of the span.
func readLogContent(logPath string) (string, error) {
	f, err := os.Open(logPath)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", logPath, err)
	}
	defer func() { _ = f.Close() }()

	var sb strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 10<<20) // 10 MB per-line cap
	for sc.Scan() {
		var c Chunk
		if json.Unmarshal(sc.Bytes(), &c) == nil {
			sb.WriteString(c.Data)
			sb.WriteByte('\n')
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("scan %s: %w", logPath, err)
	}
	return sb.String(), nil
}
