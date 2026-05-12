package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runHarness wires a CLI run against a temp data dir so tests can assert on
// the on-disk state without polluting the user's $HOME.
func runHarness(t *testing.T, args ...string) (stdout, stderr string, exit int, dataDir string) {
	t.Helper()
	dataDir = t.TempDir()
	t.Setenv("SHTRACE_DATA_DIR", dataDir)
	// Make sure no parent session leaks in from the test runner's env.
	t.Setenv("SHTRACE_SESSION_ID", "")
	t.Setenv("SHTRACE_PARENT_SPAN_ID", "")
	t.Setenv("SHTRACE_TAGS", "")

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
