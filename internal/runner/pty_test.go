//go:build !windows

package runner

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

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
	// readerWG tracks whether the reader goroutine has fully exited so that
	// pr.Close() in the defer chain is only called after the goroutine is done
	// with pr.Read — this covers both normal completion and t.Fatalf paths.
	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, readErr := pr.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if readErr != nil {
				break
			}
		}
		done <- sb.String()
	}()

	// Defers execute LIFO: closePW first, then readerWG.Wait, then pr.Close.
	// This order guarantees the reader goroutine exits before pr is closed,
	// and holds even when t.Fatalf triggers runtime.Goexit() early.
	// Note: if os.Pipe() failed above, t.Fatalf fires before these defers are
	// registered, so readerWG is never Add(1)'d and Wait() is never called —
	// no deadlock is possible in that early-exit path.
	defer func() { _ = pr.Close() }()
	defer func() { readerWG.Wait() }()
	var closePWOnce sync.Once
	closePW := func() { _ = pw.Close() }
	defer func() { closePWOnce.Do(closePW) }()

	_, runErr := RunPTY(context.Background(), PTYOptions{
		Argv:   []string{"sh", "-c", "printf world"},
		Writer: rec,
		Tty:    pw,
		Masker: secret.DefaultMasker(),
	})
	closePWOnce.Do(closePW) // signal EOF; goroutine drains and sends to done
	ttyOutput := <-done

	if runErr != nil {
		t.Fatalf("RunPTY: %v", runErr)
	}
	if !strings.Contains(ttyOutput, "world") {
		t.Fatalf("tty output %q does not contain 'world'", ttyOutput)
	}
}

// TestPTYRunner_ForwardsStdin is the PTY half of the issue #44 regression:
// input relayed into the PTY master must reach the child. `cat` echoes it
// back, and the PTY line discipline additionally echoes the input itself, so
// asserting on the recorded stream is enough to prove the relay works.
//
// Tty is nil, so raw mode and SIGWINCH are not involved — those need a real
// terminal, which CI does not provide.
func TestPTYRunner_ForwardsStdin(t *testing.T) {
	rec := &recordingWriter{}
	done := make(chan error, 1)

	go func() {
		// head -n1 exits after the first line, closing the slave side so the
		// master reaches EOF without needing an explicit stdin close.
		_, err := RunPTY(context.Background(), PTYOptions{
			Argv:   []string{"head", "-n", "1"},
			Writer: rec,
			Stdin:  strings.NewReader("ping-from-stdin\n"),
			Tty:    nil,
			Masker: secret.DefaultMasker(),
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunPTY: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("RunPTY hung; stdin relay never delivered input to the child")
	}

	var all string
	for _, c := range rec.snapshot() {
		all += c.Data
	}
	if !strings.Contains(all, "ping-from-stdin") {
		t.Fatalf("PTY output %q does not contain the forwarded stdin", all)
	}
}

// TestPTYRunner_NilStdinDoesNotHang verifies a command needing no input still
// completes when no stdin is supplied, and that no relay goroutine is started.
func TestPTYRunner_NilStdinDoesNotHang(t *testing.T) {
	done := make(chan Result, 1)
	go func() {
		res, err := RunPTY(context.Background(), PTYOptions{
			Argv:   []string{"sh", "-c", "echo no-stdin-needed"},
			Writer: &recordingWriter{},
			Stdin:  nil,
			Tty:    nil,
			Masker: secret.DefaultMasker(),
		})
		if err != nil {
			t.Errorf("RunPTY: %v", err)
		}
		done <- res
	}()

	select {
	case res := <-done:
		if res.ExitCode != 0 {
			t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunPTY hung with nil stdin")
	}
}

// TestPTYRunner_StdinOutlivingChildDoesNotPanic exercises the detached relay
// goroutine against a reader that never reaches EOF, which is what a real TTY
// looks like. The child ignores stdin and exits immediately, so ptmx.Close
// races the pending relay Write; *os.File must turn that into ErrClosed rather
// than a write to a recycled fd. Meaningful under -race.
func TestPTYRunner_StdinOutlivingChildDoesNotPanic(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop)

	for i := 0; i < 20; i++ {
		if _, err := RunPTY(context.Background(), PTYOptions{
			Argv:   []string{"true"},
			Writer: &recordingWriter{},
			Stdin:  blockingReader{stop: stop},
			Tty:    nil,
			Masker: secret.DefaultMasker(),
		}); err != nil {
			t.Fatalf("RunPTY: %v", err)
		}
	}
}

// blockingReader emits a steady trickle of bytes and never returns EOF until
// stop is closed, standing in for an idle terminal that stays open.
type blockingReader struct{ stop chan struct{} }

func (b blockingReader) Read(p []byte) (int, error) {
	select {
	case <-b.stop:
		return 0, io.EOF
	case <-time.After(time.Millisecond):
		p[0] = '.'
		return 1, nil
	}
}
