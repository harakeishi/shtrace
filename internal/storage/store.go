package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

// InsertSession upserts a session row. Tags are stored as JSON. The upsert
// uses COALESCE on ended_at so that a re-insert with nil ended_at (e.g., a
// nested child invocation re-entering the same row) does not clobber an
// already-set end time.
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
		ON CONFLICT(id) DO UPDATE SET
			ended_at = COALESCE(excluded.ended_at, sessions.ended_at),
			tags_json = excluded.tags_json`,
		sess.ID, sess.StartedAt.UTC().Format(time.RFC3339Nano), endedAt, string(tagsJSON),
	)
	return err
}

// EnsureSession inserts the session row if it doesn't exist; if it does, this
// is a complete no-op. Child shtrace invocations call this to satisfy the
// spans.session_id foreign key when the parent wrote to a different on-disk
// DB (different SHTRACE_DATA_DIR resolution).
func (s *Store) EnsureSession(ctx context.Context, sess Session) error {
	tagsJSON, err := json.Marshal(sess.Tags)
	if err != nil {
		return fmt.Errorf("marshal tags: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO sessions(id, started_at, tags_json)
		VALUES(?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		sess.ID, sess.StartedAt.UTC().Format(time.RFC3339Nano), string(tagsJSON),
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
//
// Per-row parse failures (malformed timestamp, malformed tags_json) are
// reported via warn and the offending row is skipped, so a single corrupt row
// does not take the whole listing offline. The function still returns an
// error for unrecoverable conditions (the underlying query failed, a column
// scan failed). Pass nil for warn to ignore per-row warnings.
func (s *Store) ListSessions(ctx context.Context, limit int, warn func(error)) ([]Session, error) {
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
	defer func() { _ = rows.Close() }()

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
		t, err := time.Parse(time.RFC3339Nano, startedAt)
		if err != nil {
			reportWarn(warn, fmt.Errorf("session %s: parse started_at %q: %w", sess.ID, startedAt, err))
			continue
		}
		sess.StartedAt = t
		if endedAt.Valid {
			t, err := time.Parse(time.RFC3339Nano, endedAt.String)
			if err != nil {
				reportWarn(warn, fmt.Errorf("session %s: parse ended_at %q: %w", sess.ID, endedAt.String, err))
				continue
			}
			sess.EndedAt = &t
		}
		sess.Tags = map[string]string{}
		if err := json.Unmarshal([]byte(tagsJSON), &sess.Tags); err != nil {
			reportWarn(warn, fmt.Errorf("session %s: parse tags_json: %w", sess.ID, err))
			continue
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// ErrSessionNotFound is returned by GetSession when no row matches the given
// id. Callers can errors.Is against this to distinguish "absent" from
// "corrupt" — which matters for `shtrace report --session <id>`, where the
// two cases need different user-facing messages.
var ErrSessionNotFound = errors.New("session not found")

// ErrSessionCorrupt is returned by GetSession when the row exists but one
// of its columns fails to parse. GetSession joins ErrSessionCorrupt with
// the underlying parse error via errors.Join, so:
//
//   - errors.Is(err, ErrSessionCorrupt) is true.
//   - errors.As against concrete parse error types (e.g. *time.ParseError,
//     *json.SyntaxError) works through the join.
//   - The underlying error's message appears in err.Error() for diagnostics.
//
// Note: errors.Unwrap returns nil on a joined error (the join exposes
// Unwrap() []error, not Unwrap() error). Use errors.Is / errors.As, not
// errors.Unwrap, to inspect causes.
var ErrSessionCorrupt = errors.New("session row corrupt")

// GetSession returns the single session row with the given id.
//
// The two failure modes are reported via the sentinel errors above so a
// caller (e.g. `shtrace report --session X`) can render "not found" and
// "corrupt row" distinctly instead of conflating them — important because
// ListSessions silently skips corrupt rows, which would otherwise make a
// targeted lookup of a corrupt session indistinguishable from a typo.
func (s *Store) GetSession(ctx context.Context, id string) (Session, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, started_at, ended_at, tags_json
		FROM sessions
		WHERE id = ?`, id)

	var (
		sess      Session
		startedAt string
		endedAt   sql.NullString
		tagsJSON  string
	)
	if err := row.Scan(&sess.ID, &startedAt, &endedAt, &tagsJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, ErrSessionNotFound
		}
		return Session{}, err
	}
	t, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return Session{}, errors.Join(ErrSessionCorrupt, fmt.Errorf("parse started_at %q: %w", startedAt, err))
	}
	sess.StartedAt = t
	if endedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, endedAt.String)
		if err != nil {
			return Session{}, errors.Join(ErrSessionCorrupt, fmt.Errorf("parse ended_at %q: %w", endedAt.String, err))
		}
		sess.EndedAt = &t
	}
	sess.Tags = map[string]string{}
	if err := json.Unmarshal([]byte(tagsJSON), &sess.Tags); err != nil {
		return Session{}, errors.Join(ErrSessionCorrupt, fmt.Errorf("parse tags_json: %w", err))
	}
	return sess, nil
}

// SpansForSession returns spans for sessionID, ordered by start time ascending
// so callers can render a chronological timeline.
//
// Per-row parse failures are reported via warn and the row is skipped; the
// function still returns an error for unrecoverable query/scan failures.
// Pass nil for warn to ignore per-row warnings.
func (s *Store) SpansForSession(ctx context.Context, sessionID string, warn func(error)) ([]Span, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, parent_span_id, command, argv_json, cwd, mode, started_at, ended_at, exit_code
		FROM spans
		WHERE session_id = ?
		ORDER BY started_at ASC, id ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

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
		if err := json.Unmarshal([]byte(argvJSON), &sp.Argv); err != nil {
			reportWarn(warn, fmt.Errorf("span %s: parse argv_json: %w", sp.ID, err))
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, started)
		if err != nil {
			reportWarn(warn, fmt.Errorf("span %s: parse started_at %q: %w", sp.ID, started, err))
			continue
		}
		sp.StartedAt = t
		t, err = time.Parse(time.RFC3339Nano, ended)
		if err != nil {
			reportWarn(warn, fmt.Errorf("span %s: parse ended_at %q: %w", sp.ID, ended, err))
			continue
		}
		sp.EndedAt = t
		if exitCode.Valid {
			v := int(exitCode.Int64)
			sp.ExitCode = &v
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// DeleteSession removes a session and all of its spans from the metadata DB
// in a single transaction.
func (s *Store) DeleteSession(ctx context.Context, sessionID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete session: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM spans WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("delete session: delete spans: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, sessionID); err != nil {
		return fmt.Errorf("delete session: delete session row: %w", err)
	}
	return tx.Commit()
}

// SpanExists reports whether a span with the given spanID belongs to sessionID.
// It performs a single-row indexed lookup, avoiding a full table scan.
func (s *Store) SpanExists(ctx context.Context, sessionID, spanID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spans WHERE id = ? AND session_id = ?`,
		spanID, sessionID,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("span exists: %w", err)
	}
	return n > 0, nil
}

func reportWarn(warn func(error), err error) {
	if warn != nil {
		warn(err)
	}
}
