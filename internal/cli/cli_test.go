package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func openSQLiteForTest(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path)
}

// runHarness wires a CLI run against a temp data dir so tests can assert on
// the on-disk state without polluting the user's $HOME. It also zeroes out
// every env var that ResolveDataDir consults, so the only thing that can
// satisfy resolution is the explicit SHTRACE_DATA_DIR — a future regression
// in precedence won't silently leak test artifacts into ~/.local/share.
func runHarness(t *testing.T, args ...string) (stdout, stderr string, exit int, dataDir string) {
	t.Helper()
	dataDir = t.TempDir()
	t.Setenv("SHTRACE_DATA_DIR", dataDir)
	// Parent-session env that might bleed in from the test runner.
	t.Setenv("SHTRACE_SESSION_ID", "")
	t.Setenv("SHTRACE_PARENT_SPAN_ID", "")
	t.Setenv("SHTRACE_TAGS", "")
	// Belt-and-braces: clear everything that ResolveDataDir consults.
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("GITHUB_WORKSPACE", "")

	var so, se bytes.Buffer
	exit = Run(context.Background(), args, &so, &se)
	return so.String(), se.String(), exit, dataDir
}

func TestCLI_RunCommand_RecordsSessionAndOutputFile(t *testing.T) {
	stdout, _, exit, dataDir := runHarness(t, "shtrace", "--", "sh", "-c", "printf hello")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}
	if !strings.Contains(stdout, "hello") {
		t.Fatalf("stdout should pass through child output, got %q", stdout)
	}

	// sessions.db must exist
	if _, err := os.Stat(filepath.Join(dataDir, "sessions.db")); err != nil {
		t.Fatalf("sessions.db missing: %v", err)
	}

	// outputs/<session>/<span>.log must exist and contain a stdout chunk
	outputs := filepath.Join(dataDir, "outputs")
	matches := walkLogFiles(t, outputs)
	if len(matches) != 1 {
		t.Fatalf("expected 1 log file under %s, found %d", outputs, len(matches))
	}
	body, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(body), `"stream":"stdout"`) {
		t.Fatalf("log file missing stdout chunk: %q", body)
	}
	if !strings.Contains(string(body), `"data":"hello"`) {
		t.Fatalf("log file missing data: %q", body)
	}
}

func TestCLI_RunCommand_PropagatesExitCode(t *testing.T) {
	_, _, exit, _ := runHarness(t, "shtrace", "--", "sh", "-c", "exit 3")
	if exit != 3 {
		t.Fatalf("exit = %d, want 3", exit)
	}
}

func TestCLI_LsShowsRecordedSession(t *testing.T) {
	_, _, exit, _ := runHarness(t, "shtrace", "--", "sh", "-c", "printf ls-test")
	if exit != 0 {
		t.Fatalf("setup run exit = %d", exit)
	}

	// Same data dir from runHarness was set on the test env; reuse it by
	// invoking ls with the env still in place.
	var so, se bytes.Buffer
	code := Run(context.Background(), []string{"shtrace", "ls"}, &so, &se)
	if code != 0 {
		t.Fatalf("ls exit = %d, stderr=%s", code, se.String())
	}
	if !strings.Contains(so.String(), "sh") {
		t.Fatalf("ls output should mention the recorded command, got %q", so.String())
	}
}

func TestCLI_ShowSplitsStdoutAndStderr(t *testing.T) {
	_, _, exit, _ := runHarness(t, "shtrace", "--", "sh", "-c", "printf out-line; printf err-line 1>&2")
	if exit != 0 {
		t.Fatalf("setup run exit = %d", exit)
	}

	var so, se bytes.Buffer
	code := Run(context.Background(), []string{"shtrace", "ls", "--json"}, &so, &se)
	if code != 0 {
		t.Fatalf("ls --json exit = %d: %s", code, se.String())
	}
	var entries []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(so.Bytes(), &entries); err != nil {
		t.Fatalf("decode ls: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("no sessions")
	}

	var sso, sse bytes.Buffer
	code = Run(context.Background(), []string{"shtrace", "show", entries[0].ID}, &sso, &sse)
	if code != 0 {
		t.Fatalf("show exit = %d: %s", code, sse.String())
	}

	if !strings.Contains(sso.String(), "out-line") {
		t.Fatalf("show stdout missing stdout data: %q", sso.String())
	}
	if strings.Contains(sso.String(), "err-line") {
		t.Fatalf("show stdout should not carry stderr data: %q", sso.String())
	}
	if !strings.Contains(sse.String(), "err-line") {
		t.Fatalf("show stderr missing stderr data: %q", sse.String())
	}
}

func TestCLI_ShowReportsCorruptLogToStderr(t *testing.T) {
	_, _, exit, dataDir := runHarness(t, "shtrace", "--", "sh", "-c", "printf hi")
	if exit != 0 {
		t.Fatalf("setup run exit = %d", exit)
	}

	logs := walkLogFiles(t, filepath.Join(dataDir, "outputs"))
	if len(logs) == 0 {
		t.Fatalf("expected at least one log file")
	}
	// Append a corrupt line that show must surface — silently dropping it
	// would hide real data-integrity bugs.
	f, err := os.OpenFile(logs[0], os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open log for append: %v", err)
	}
	if _, err := f.WriteString("{not-json\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close()

	var so, se bytes.Buffer
	code := Run(context.Background(), []string{"shtrace", "ls", "--json"}, &so, &se)
	if code != 0 {
		t.Fatalf("ls exit = %d", code)
	}
	var entries []struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(so.Bytes(), &entries)
	if len(entries) == 0 {
		t.Fatalf("no sessions")
	}

	var sso, sse bytes.Buffer
	if code := Run(context.Background(), []string{"shtrace", "show", entries[0].ID}, &sso, &sse); code != 0 {
		t.Fatalf("show exit = %d (corrupt log should not block normal output): %s", code, sse.String())
	}
	if !strings.Contains(sse.String(), "corrupt") && !strings.Contains(sse.String(), "skipped") {
		t.Fatalf("expected show to report the corrupt line on stderr, got %q", sse.String())
	}
}

// TestCLI_LsSurvivesCorruptSessionRow: a single corrupt session row must
// not take the whole `shtrace ls` offline (Round-2 finding). The corrupt
// row should surface as a warning on stderr; healthy rows still listed.
func TestCLI_LsSurvivesCorruptSessionRow(t *testing.T) {
	// First, record a healthy session via the normal CLI path.
	_, _, exit, dataDir := runHarness(t, "shtrace", "--", "sh", "-c", "printf healthy")
	if exit != 0 {
		t.Fatalf("setup run exit = %d", exit)
	}

	// Inject a corrupt row directly into sessions.db.
	db, err := openSQLiteForTest(filepath.Join(dataDir, "sessions.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO sessions(id, started_at, tags_json) VALUES('corrupt', 'not-a-ts', '{}')`); err != nil {
		t.Fatalf("inject corrupt row: %v", err)
	}
	db.Close()

	var so, se bytes.Buffer
	code := Run(context.Background(), []string{"shtrace", "ls"}, &so, &se)
	if code != 0 {
		t.Fatalf("ls exit = %d (should still succeed): stderr=%q", code, se.String())
	}
	// Healthy session line should still be there.
	if !strings.Contains(so.String(), "sh") {
		t.Fatalf("healthy session not listed: stdout=%q", so.String())
	}
	// The corrupt row should be surfaced as a warning.
	if !strings.Contains(se.String(), "warning") || !strings.Contains(se.String(), "corrupt") {
		t.Fatalf("expected stderr warning for corrupt row, got %q", se.String())
	}
}

func TestCLI_ShowOutputsTheRecordedJSONL(t *testing.T) {
	_, _, exit, _ := runHarness(t, "shtrace", "--", "sh", "-c", "printf show-test")
	if exit != 0 {
		t.Fatalf("setup run exit = %d", exit)
	}

	// pick the latest session id from the on-disk state
	var so, se bytes.Buffer
	code := Run(context.Background(), []string{"shtrace", "ls", "--json"}, &so, &se)
	if code != 0 {
		t.Fatalf("ls --json exit = %d: %s", code, se.String())
	}
	var entries []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(so.Bytes(), &entries); err != nil {
		t.Fatalf("decode ls --json: %v: %q", err, so.String())
	}
	if len(entries) == 0 {
		t.Fatalf("ls --json returned no entries")
	}

	var so2, se2 bytes.Buffer
	code = Run(context.Background(), []string{"shtrace", "show", entries[0].ID}, &so2, &se2)
	if code != 0 {
		t.Fatalf("show exit = %d: %s", code, se2.String())
	}
	if !strings.Contains(so2.String(), "show-test") {
		t.Fatalf("show output missing recorded data: %q", so2.String())
	}
}

func walkLogFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".log") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}
