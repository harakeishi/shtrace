package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harakeishi/shtrace/internal/secret"
	"github.com/harakeishi/shtrace/internal/storage"
)

type recordingWriter struct {
	mu     sync.Mutex
	chunks []storage.Chunk
}

func (r *recordingWriter) WriteChunk(stream storage.Stream, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chunks = append(r.chunks, storage.Chunk{Stream: string(stream), Data: string(data)})
	return nil
}

func (r *recordingWriter) snapshot() []storage.Chunk {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]storage.Chunk, len(r.chunks))
	copy(out, r.chunks)
	return out
}

func TestPipeRunner_RecordsStdoutAndStderr(t *testing.T) {
	rec := &recordingWriter{}
	var teeOut, teeErr bytes.Buffer

	res, err := RunPipe(context.Background(), PipeOptions{
		Argv:   []string{"sh", "-c", "printf out; printf err 1>&2"},
		Writer: rec,
		Stdout: &teeOut,
		Stderr: &teeErr,
		Masker: secret.DefaultMasker(),
	})
	if err != nil {
		t.Fatalf("RunPipe: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}

	var stdouts, stderrs []string
	for _, c := range rec.snapshot() {
		switch c.Stream {
		case "stdout":
			stdouts = append(stdouts, c.Data)
		case "stderr":
			stderrs = append(stderrs, c.Data)
		default:
			t.Fatalf("unexpected stream label %q", c.Stream)
		}
	}
	if strings.Join(stdouts, "") != "out" {
		t.Fatalf("stdout chunks = %q, want out", stdouts)
	}
	if strings.Join(stderrs, "") != "err" {
		t.Fatalf("stderr chunks = %q, want err", stderrs)
	}

	if teeOut.String() != "out" {
		t.Fatalf("tee stdout = %q, want out", teeOut.String())
	}
	if teeErr.String() != "err" {
		t.Fatalf("tee stderr = %q, want err", teeErr.String())
	}
}

func TestPipeRunner_PropagatesExitCode(t *testing.T) {
	rec := &recordingWriter{}

	res, err := RunPipe(context.Background(), PipeOptions{
		Argv:   []string{"sh", "-c", "exit 7"},
		Writer: rec,
		Stdout: io.Discard,
		Stderr: io.Discard,
		Masker: secret.DefaultMasker(),
	})
	if err != nil {
		t.Fatalf("RunPipe: %v", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", res.ExitCode)
	}
}

func TestPipeRunner_MasksSecretsInRecordedChunks(t *testing.T) {
	rec := &recordingWriter{}
	var teeOut bytes.Buffer

	_, err := RunPipe(context.Background(), PipeOptions{
		Argv:   []string{"sh", "-c", "printf 'Authorization: Bearer abcdefghijklmnopqrstuvwxyz1234567890ABCDEF\\n'"},
		Writer: rec,
		Stdout: &teeOut,
		Stderr: io.Discard,
		Masker: secret.DefaultMasker(),
	})
	if err != nil {
		t.Fatalf("RunPipe: %v", err)
	}

	all := ""
	for _, c := range rec.snapshot() {
		all += c.Data
	}
	if strings.Contains(all, "abcdefghijklmnopqrstuvwxyz1234567890ABCDEF") {
		t.Fatalf("recorded chunks leaked secret: %q", all)
	}
	// Tee to the user terminal should *not* be masked — the user already sees
	// it on their own screen.
	if !strings.Contains(teeOut.String(), "abcdefghijklmnopqrstuvwxyz1234567890ABCDEF") {
		t.Fatalf("tee output should pass through raw, got %q", teeOut.String())
	}
}

// jsonChunk is here only so we don't accidentally pin the test to internals of
// storage; if the schema drifts the parse will fail loudly.
type jsonChunk struct {
	Stream string `json:"stream"`
	Data   string `json:"data"`
}

// TestForwardStream_MasksSecretSplitAcrossReads is the regression test for
// the pipe-boundary leak: the previous implementation masked each Read in
// isolation, so a Bearer token straddling two reads slipped through.
func TestForwardStream_MasksSecretSplitAcrossReads(t *testing.T) {
	r, w := io.Pipe()
	rec := &recordingWriter{}
	var tee bytes.Buffer
	m := secret.DefaultMasker()

	done := make(chan struct{})
	go func() {
		forwardStream(r, storage.StreamStdout, &tee, rec, m)
		close(done)
	}()

	full := "Authorization: Bearer abcdefghijklmnopqrstuvwxyz1234567890ABCDEF\n"
	// Split inside the bearer token so neither half matches the regex on
	// its own. io.Pipe is synchronous, so the first Write completes only
	// after the goroutine has Read it.
	splitAt := len("Authorization: Bearer abcd")
	if _, err := w.Write([]byte(full[:splitAt])); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	if _, err := w.Write([]byte(full[splitAt:])); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	<-done

	recorded := ""
	for _, c := range rec.snapshot() {
		recorded += c.Data
	}
	if strings.Contains(recorded, "abcdefghijklmnopqrstuvwxyz1234567890ABCDEF") {
		t.Fatalf("secret leaked across read boundary; recorded=%q", recorded)
	}
	// Tee should still see the raw bytes (user's own terminal output).
	if !strings.Contains(tee.String(), "abcdefghijklmnopqrstuvwxyz1234567890ABCDEF") {
		t.Fatalf("tee should pass raw bytes through, got %q", tee.String())
	}
}

// TestForwardStream_FlushesAfterLargeOutput verifies the tail-buffer scheme
// still catches a secret that lands at the very end of the stream, even
// after many bytes of harmless content have flushed past.
func TestForwardStream_FlushesAfterLargeOutput(t *testing.T) {
	r, w := io.Pipe()
	rec := &recordingWriter{}
	m := secret.DefaultMasker()

	done := make(chan struct{})
	go func() {
		forwardStream(r, storage.StreamStdout, io.Discard, rec, m)
		close(done)
	}()

	padding := strings.Repeat("x", 4096)
	secretStr := "ghp_abcdefghijklmnopqrstuvwxyz0123456789\n"
	go func() {
		_, _ = w.Write([]byte(padding))
		_, _ = w.Write([]byte(secretStr))
		_ = w.Close()
	}()
	<-done

	recorded := ""
	for _, c := range rec.snapshot() {
		recorded += c.Data
	}
	if strings.Contains(recorded, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("PAT leaked after padding; recorded tail=%q", recorded[max(0, len(recorded)-200):])
	}
}

// TestForwardStream_LiteralSecretAtFlushBoundary is the regression test for
// the flush-boundary split: when a single Read delivers more than safetyTail
// bytes and a literal secret straddles the cutoff position, the old
// "mask flushable only" approach leaked it. This test verifies the fix.
func TestForwardStream_LiteralSecretAtFlushBoundary(t *testing.T) {
	const litSecret = "LITERAL_SECRET_TOKEN_ABCDEFGH"
	m, err := secret.NewMaskerWithLiterals(nil, []string{litSecret})
	if err != nil {
		t.Fatalf("NewMaskerWithLiterals: %v", err)
	}

	// Place the secret near the safetyTail boundary so it straddles the
	// flush cutoff when delivered in a single large pipe write.
	prefix := strings.Repeat("A", safetyTail-4)
	suffix := strings.Repeat("B", safetyTail)
	payload := prefix + litSecret + suffix

	r, w := io.Pipe()
	rec := &recordingWriter{}

	done := make(chan struct{})
	go func() {
		forwardStream(r, storage.StreamStdout, io.Discard, rec, m)
		close(done)
	}()

	if _, err := w.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = w.Close()
	<-done

	recorded := ""
	for _, c := range rec.snapshot() {
		recorded += c.Data
	}
	if strings.Contains(recorded, litSecret) {
		t.Errorf("literal secret leaked at flush boundary; recorded=%q", recorded)
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func TestPipeRunner_WritesJSONLToBackingWriter(t *testing.T) {
	var buf bytes.Buffer
	w := storage.NewJSONLWriter(&buf, nil)

	_, err := RunPipe(context.Background(), PipeOptions{
		Argv:   []string{"sh", "-c", "printf hi"},
		Writer: w,
		Stdout: io.Discard,
		Stderr: io.Discard,
		Masker: secret.DefaultMasker(),
	})
	if err != nil {
		t.Fatalf("RunPipe: %v", err)
	}

	if !strings.HasSuffix(buf.String(), "\n") {
		t.Fatalf("expected trailing newline in JSONL output")
	}
	var c jsonChunk
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &c); err != nil {
		t.Fatalf("decode line: %v: %q", err, buf.String())
	}
	if c.Stream != "stdout" || c.Data != "hi" {
		t.Fatalf("unexpected chunk: %+v", c)
	}
}

// TestPipeRunner_ForwardsStdin is the regression test for issue #44: the
// wrapped command must receive the caller's stdin. Mirrors the reported
// reproduction `printf 'a\nb\nc\n' | shtrace -- wc -l`.
func TestPipeRunner_ForwardsStdin(t *testing.T) {
	rec := &recordingWriter{}
	var teeOut bytes.Buffer

	res, err := RunPipe(context.Background(), PipeOptions{
		Argv:   []string{"wc", "-l"},
		Writer: rec,
		Stdin:  strings.NewReader("a\nb\nc\n"),
		Stdout: &teeOut,
		Stderr: io.Discard,
		Masker: secret.DefaultMasker(),
	})
	if err != nil {
		t.Fatalf("RunPipe: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	if got := strings.TrimSpace(teeOut.String()); got != "3" {
		t.Fatalf("wc -l counted %q, want \"3\" (stdin not forwarded)", got)
	}
}

// TestPipeRunner_ForwardsStdinFromFile covers the *os.File path, which exec
// hands to the child as a raw fd instead of spawning an internal copier. This
// is what the CLI does with os.Stdin, so the two paths are exercised.
func TestPipeRunner_ForwardsStdinFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = f.Close() }()

	rec := &recordingWriter{}
	var teeOut bytes.Buffer
	if _, err := RunPipe(context.Background(), PipeOptions{
		Argv:   []string{"cat"},
		Writer: rec,
		Stdin:  f,
		Stdout: &teeOut,
		Stderr: io.Discard,
		Masker: secret.DefaultMasker(),
	}); err != nil {
		t.Fatalf("RunPipe: %v", err)
	}
	if got := teeOut.String(); got != "hello\n" {
		t.Fatalf("cat output = %q, want %q", got, "hello\n")
	}
}

// TestPipeRunner_NilStdinDoesNotHang guards the other half of #44: a child
// that reads stdin must see EOF rather than block forever when the caller
// supplied no stdin. Run under a timeout so a regression fails instead of
// hanging the suite.
func TestPipeRunner_NilStdinDoesNotHang(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stdin io.Reader
	}{
		{"nil", nil},
		{"empty", strings.NewReader("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var teeOut bytes.Buffer
			done := make(chan error, 1)
			go func() {
				_, err := RunPipe(context.Background(), PipeOptions{
					Argv:   []string{"cat"},
					Writer: &recordingWriter{},
					Stdin:  tc.stdin,
					Stdout: &teeOut,
					Stderr: io.Discard,
					Masker: secret.DefaultMasker(),
				})
				done <- err
			}()

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("RunPipe: %v", err)
				}
				if teeOut.Len() != 0 {
					t.Fatalf("expected no output, got %q", teeOut.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("RunPipe hung with no stdin; child never saw EOF")
			}
		})
	}
}

// TestPipeRunner_BlockingStdinDoesNotHangWait guards the os.Pipe
// normalization in attachStdin. A non-*os.File reader that never reaches EOF
// (a TTY, a socket) used to make exec's internal copier outlive the child and
// block cmd.Wait forever; the child's exit must end the call regardless.
func TestPipeRunner_BlockingStdinDoesNotHangWait(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop)

	var teeOut bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := RunPipe(context.Background(), PipeOptions{
			Argv:   []string{"sh", "-c", "echo hi"},
			Writer: &recordingWriter{},
			Stdin:  idleReader{stop: stop},
			Stdout: &teeOut,
			Stderr: io.Discard,
			Masker: secret.DefaultMasker(),
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunPipe: %v", err)
		}
		if got := strings.TrimSpace(teeOut.String()); got != "hi" {
			t.Fatalf("output = %q, want \"hi\"", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunPipe hung: cmd.Wait is waiting on the stdin copier, not the child")
	}
}

// idleReader blocks in Read without ever yielding a byte, which is how an idle
// terminal behaves. Distinct from pty_test.go's blockingReader, which trickles
// bytes and so cannot expose a Wait that is stuck on the copier.
type idleReader struct{ stop chan struct{} }

func (r idleReader) Read([]byte) (int, error) {
	<-r.stop
	return 0, io.EOF
}

// TestPipeRunner_StdinIsNotRecorded pins the deliberate choice to forward
// stdin without recording it: recorded chunks must not contain the input,
// since it may carry passwords typed at a prompt.
func TestPipeRunner_StdinIsNotRecorded(t *testing.T) {
	rec := &recordingWriter{}
	const input = "hunter2-plaintext-password\n"

	if _, err := RunPipe(context.Background(), PipeOptions{
		Argv:   []string{"wc", "-c"},
		Writer: rec,
		Stdin:  strings.NewReader(input),
		Stdout: io.Discard,
		Stderr: io.Discard,
		Masker: secret.DefaultMasker(),
	}); err != nil {
		t.Fatalf("RunPipe: %v", err)
	}

	for _, c := range rec.snapshot() {
		if c.Stream == "stdin" {
			t.Errorf("stdin must not be recorded, got chunk %q", c.Data)
		}
		if strings.Contains(c.Data, "hunter2") {
			t.Errorf("recorded chunk leaked stdin content: %q", c.Data)
		}
	}
}
