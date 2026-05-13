//go:build !windows

package runner

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/harakeishi/shtrace/internal/secret"
	"github.com/harakeishi/shtrace/internal/storage"
)

func TestPTYRunner_RecordsPTYStream(t *testing.T) {
	rec := &recordingWriter{}

	res, err := RunPTY(context.Background(), PTYOptions{
		Argv:   []string{"sh", "-c", "printf hello"},
		Writer: rec,
		Tty:    nil, // no real terminal in CI
		Masker: secret.DefaultMasker(),
	})
	if err != nil {
		t.Fatalf("RunPTY: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}

	var all string
	for _, c := range rec.snapshot() {
		if c.Stream != string(storage.StreamPTY) {
			t.Errorf("unexpected stream %q, want %q", c.Stream, storage.StreamPTY)
		}
		all += c.Data
	}
	if !strings.Contains(all, "hello") {
		t.Fatalf("recorded PTY output %q does not contain 'hello'", all)
	}
}

func TestPTYRunner_PropagatesExitCode(t *testing.T) {
	rec := &recordingWriter{}

	res, err := RunPTY(context.Background(), PTYOptions{
		Argv:   []string{"sh", "-c", "exit 3"},
		Writer: rec,
		Tty:    nil,
		Masker: secret.DefaultMasker(),
	})
	if err != nil {
		t.Fatalf("RunPTY: %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", res.ExitCode)
	}
}

// TestPTYRunner_MasksSecretsInRecordedChunks verifies that the Replacement
// marker appears instead of the raw secret, independent of which regex fired.
func TestPTYRunner_MasksSecretsInRecordedChunks(t *testing.T) {
	rec := &recordingWriter{}

	const rawSecret = "abcdefghijklmnopqrstuvwxyz1234567890ABCDEF"
	_, err := RunPTY(context.Background(), PTYOptions{
		Argv:   []string{"sh", "-c", "printf 'Authorization: Bearer " + rawSecret + "\\n'"},
		Writer: rec,
		Tty:    nil,
		Masker: secret.DefaultMasker(),
	})
	if err != nil {
		t.Fatalf("RunPTY: %v", err)
	}

	var all string
	for _, c := range rec.snapshot() {
		all += c.Data
	}
	if strings.Contains(all, rawSecret) {
		t.Fatalf("recorded PTY chunks leaked secret: %q", all)
	}
	if !strings.Contains(all, secret.Replacement) {
		t.Fatalf("expected replacement marker %q in recorded output, got: %q", secret.Replacement, all)
	}
}

// TestPTYRunner_ForwardsTTYOutput verifies that PTY output is written to the
// Tty writer (pass-through to the user's terminal). os.Pipe() provides a real
// *os.File so the Tty forwarding path in RunPTY is exercised.
//
// Note: pw is a pipe, not a real TTY. term.MakeRaw and pty.InheritSize will
// fail on it (silently, by design) so raw-mode and resize are not tested here
// — those require a real PTY master which is not available in CI. This test
// intentionally sets Tty != nil, which also registers a SIGWINCH handler; this
// is the only test that does so and it does not call t.Parallel() to avoid
// interference with other tests.
func TestPTYRunner_ForwardsTTYOutput(t *testing.T) {
	rec := &recordingWriter{}

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, readErr := pr.Read(buf)
			sb.Write(buf[:n])
			if readErr != nil {
				break
			}
		}
		done <- sb.String()
	}()

	defer func() { _ = pr.Close() }()

	// sync.Once ensures pw is closed exactly once:
	// - on the normal path, we close it explicitly after RunPTY so the reader
	//   goroutine sees EOF before we block on <-done.
	// - if RunPTY panics, the deferred Once.Do fires instead, preventing the
	//   reader goroutine from blocking on pr.Read indefinitely.
	var closePWOnce sync.Once
	closePW := func() { _ = pw.Close() }
	defer closePWOnce.Do(closePW)

	_, runErr := RunPTY(context.Background(), PTYOptions{
		Argv:   []string{"sh", "-c", "printf world"},
		Writer: rec,
		Tty:    pw,
		Masker: secret.DefaultMasker(),
	})
	closePWOnce.Do(closePW) // signal EOF to reader before blocking on <-done
	ttyOutput := <-done

	if runErr != nil {
		t.Fatalf("RunPTY: %v", runErr)
	}
	if !strings.Contains(ttyOutput, "world") {
		t.Fatalf("tty output %q does not contain 'world'", ttyOutput)
	}
}
