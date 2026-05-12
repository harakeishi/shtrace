package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Session is one logical shtrace session — the root invocation plus any
// nested children that share SHTRACE_SESSION_ID.
type Session struct {
	ID        string
	StartedAt time.Time
	EndedAt   *time.Time
	Tags      map[string]string
}

// Span is one shtrace invocation inside a session.
type Span struct {
	ID           string
	SessionID    string
	ParentSpanID string
	Command      string
	Argv         []string
	Cwd          string
	Mode         string // "pty" or "pipe"
	StartedAt    time.Time
	EndedAt      time.Time
	ExitCode     *int
}

// Store wraps the metadata SQLite database. Output bodies live in JSON Lines
// files on disk; the DB only carries metadata.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at path with WAL mode enabled
// so concurrent shtrace invocations don't deadlock each other.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the underlying *sql.DB.
func (s *Store) Close() error { return s.db.Close() }

// Migrate creates the schema if it does not exist. It is safe to call on
// every startup.
func (s *Store) Migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS sessions (
			id          TEXT PRIMARY KEY,
			started_at  TEXT NOT NULL,
			ended_at    TEXT,
			tags_json   TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE TABLE IF NOT EXISTS spans (
			id              TEXT PRIMARY KEY,
			session_id      TEXT NOT NULL,
			parent_span_id  TEXT NOT NULL DEFAULT '',
			command         TEXT NOT NULL,
			argv_json       TEXT NOT NULL,
			cwd             TEXT NOT NULL,
			mode            TEXT NOT NULL,
			started_at      TEXT NOT NULL,
			ended_at        TEXT NOT NULL,
			exit_code       INTEGER,
			FOREIGN KEY(session_id) REFERENCES sessions(id)
		)`,
		`CREATE INDEX IF NOT EXISTS spans_session_idx ON spans(session_id)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("migrate (%.40q): %w", q, err)
		}
	}
	return nil
}

// InsertSession upserts a session row. Tags are stored as JSON; the empty map
// is acceptable.
func (s *Store) InsertSession(ctx context.Context, sess Session) error {
	tagsJSON, err := json.Marshal(sess.Tags)
	if err != nil {
		return fmt.Errorf("marshal tags: %w", err)
	}
	var endedAt any
	if sess.EndedAt != nil {
		endedAt = sess.EndedAt.UTC().Format(time.RFC3339Nano)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO sessions(id, started_at, ended_at, tags_json)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET ended_at=excluded.ended_at, tags_json=excluded.tags_json`,
		sess.ID, sess.StartedAt.UTC().Format(time.RFC3339Nano), endedAt, string(tagsJSON),
	)
	return err
}

// InsertSpan upserts a span row.
func (s *Store) InsertSpan(ctx context.Context, sp Span) error {
	argvJSON, err := json.Marshal(sp.Argv)
	if err != nil {
		return fmt.Errorf("marshal argv: %w", err)
	}
	var exit any
	if sp.ExitCode != nil {
		exit = *sp.ExitCode
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO spans(id, session_id, parent_span_id, command, argv_json, cwd, mode, started_at, ended_at, exit_code)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			ended_at = excluded.ended_at,
			exit_code = excluded.exit_code`,
		sp.ID, sp.SessionID, sp.ParentSpanID, sp.Command, string(argvJSON),
		sp.Cwd, sp.Mode,
		sp.StartedAt.UTC().Format(time.RFC3339Nano),
		sp.EndedAt.UTC().Format(time.RFC3339Nano),
		exit,
	)
	return err
}

// ListSessions returns sessions newest-first, up to limit.
func (s *Store) ListSessions(ctx context.Context, limit int) ([]Session, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, started_at, ended_at, tags_json
		FROM sessions
		ORDER BY started_at DESC, id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var (
			sess        Session
			startedAt   string
			endedAt     sql.NullString
			tagsJSON    string
		)
		if err := rows.Scan(&sess.ID, &startedAt, &endedAt, &tagsJSON); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339Nano, startedAt); err == nil {
			sess.StartedAt = t
		}
		if endedAt.Valid {
			if t, err := time.Parse(time.RFC3339Nano, endedAt.String); err == nil {
				sess.EndedAt = &t
			}
		}
		sess.Tags = map[string]string{}
		_ = json.Unmarshal([]byte(tagsJSON), &sess.Tags)
		out = append(out, sess)
	}
	return out, rows.Err()
}

// SpansForSession returns spans for sessionID, ordered by start time ascending
// so callers can render a chronological timeline.
func (s *Store) SpansForSession(ctx context.Context, sessionID string) ([]Span, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, parent_span_id, command, argv_json, cwd, mode, started_at, ended_at, exit_code
		FROM spans
		WHERE session_id = ?
		ORDER BY started_at ASC, id ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Span
	for rows.Next() {
		var (
			sp        Span
			argvJSON  string
			started   string
			ended     string
			exitCode  sql.NullInt64
		)
		if err := rows.Scan(&sp.ID, &sp.SessionID, &sp.ParentSpanID, &sp.Command, &argvJSON, &sp.Cwd, &sp.Mode, &started, &ended, &exitCode); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(argvJSON), &sp.Argv)
		if t, err := time.Parse(time.RFC3339Nano, started); err == nil {
			sp.StartedAt = t
		}
		if t, err := time.Parse(time.RFC3339Nano, ended); err == nil {
			sp.EndedAt = t
		}
		if exitCode.Valid {
			v := int(exitCode.Int64)
			sp.ExitCode = &v
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}
