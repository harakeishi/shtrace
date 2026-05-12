package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

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
