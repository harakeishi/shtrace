package runner

import (
	"context"
	"io"
	"os/exec"
	"testing"

	"github.com/harakeishi/shtrace/internal/secret"
	"github.com/harakeishi/shtrace/internal/storage"
)

// discardChunkWriter drops every chunk; it isolates runner/masker cost from
// any storage write cost in the benchmarks below.
type discardChunkWriter struct{}

var _ ChunkWriter = discardChunkWriter{}

func (discardChunkWriter) WriteChunk(storage.Stream, []byte) error { return nil }

// BenchmarkRunPipe_SpawnFloor reports the wall-clock cost of wrapping a
// command that prints a handful of bytes. The result is dominated by sh +
// exec start-up (typically 1-3 ms), so this bench is a spawn-floor sentinel:
// a regression here points at runner setup, masker init, or goroutine
// teardown — not at streaming throughput (see BenchmarkForwardStream_Throughput).
// It does not subtract a bare-`printf hi` baseline, so the absolute number is
// "wrapped wall-clock", not "wrapper overhead".
func BenchmarkRunPipe_SpawnFloor(b *testing.B) {
	if _, err := exec.LookPath("sh"); err != nil {
		b.Skipf("sh not available: %v", err)
	}
	m := secret.DefaultMasker()
	for b.Loop() {
		res, err := RunPipe(context.Background(), PipeOptions{
			Argv:   []string{"sh", "-c", "printf hi"},
			Writer: discardChunkWriter{},
			Stdout: io.Discard,
			Stderr: io.Discard,
			Masker: m,
		})
		if err != nil {
			b.Fatalf("RunPipe: %v", err)
		}
		if res.ExitCode != 0 {
			b.Fatalf("ExitCode = %d, want 0", res.ExitCode)
		}
	}
}

// BenchmarkForwardStream_Throughput measures masker + chunk-writer throughput
// in isolation, with no child process. A 4 MiB printable-ASCII payload is fed
// to forwardStream via io.Pipe and drained by the same discardChunkWriter the
// real runner uses; b.SetBytes is set to the payload size so the bench reports
// MB/s directly. The payload is large enough to amortise per-iteration
// goroutine + pipe setup so the reported number reflects steady-state
// streaming cost.
func BenchmarkForwardStream_Throughput(b *testing.B) {
	const payloadSize = 4 << 20 // 4 MiB
	line := []byte("shtrace bench line of moderate length without secrets here\n")
	payload := make([]byte, payloadSize)
	for i := 0; i < len(payload); i += len(line) {
		copy(payload[i:], line)
	}
	m := secret.DefaultMasker()
	b.SetBytes(int64(payloadSize))
	for b.Loop() {
		r, w := io.Pipe()
		done := make(chan struct{})
		go func() {
			forwardStream(r, storage.StreamStdout, io.Discard, discardChunkWriter{}, m)
			close(done)
		}()
		if _, err := w.Write(payload); err != nil {
			b.Fatalf("Write: %v", err)
		}
		_ = w.Close()
		<-done
	}
}
