package runner

import (
	"context"
	"io"
	"testing"

	"github.com/harakeishi/shtrace/internal/secret"
	"github.com/harakeishi/shtrace/internal/storage"
)

// discardChunkWriter is a ChunkWriter that drops everything. It isolates
// runner/masker cost from any storage write cost in the benchmarks below.
type discardChunkWriter struct{}

func (discardChunkWriter) WriteChunk(storage.Stream, []byte) error { return nil }

// BenchmarkRunPipe_SmallOutput measures the floor cost of wrapping a command
// that prints a handful of bytes: process spawn + two-goroutine pipe drain +
// masker setup, with negligible data through the pipes.
func BenchmarkRunPipe_SmallOutput(b *testing.B) {
	m := secret.DefaultMasker()
	for i := 0; i < b.N; i++ {
		_, err := RunPipe(context.Background(), PipeOptions{
			Argv:   []string{"sh", "-c", "printf hi"},
			Writer: discardChunkWriter{},
			Stdout: io.Discard,
			Stderr: io.Discard,
			Masker: m,
		})
		if err != nil {
			b.Fatalf("RunPipe: %v", err)
		}
	}
}

// BenchmarkRunPipe_MediumOutput measures the steady-state cost of streaming
// ~64 KiB of stdout through the masker tail buffer and the chunk writer. The
// payload is printable ASCII so the masker exercises its full regex set; the
// data is deterministic so runs are comparable across machines.
func BenchmarkRunPipe_MediumOutput(b *testing.B) {
	m := secret.DefaultMasker()
	// "yes" emits its argument followed by newline; head -c bounds the byte
	// count so the benchmark is repeatable.
	argv := []string{"sh", "-c", "yes 'shtrace benchmark line of moderate length' | head -c 65536"}
	b.SetBytes(65536)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := RunPipe(context.Background(), PipeOptions{
			Argv:   argv,
			Writer: discardChunkWriter{},
			Stdout: io.Discard,
			Stderr: io.Discard,
			Masker: m,
		})
		if err != nil {
			b.Fatalf("RunPipe: %v", err)
		}
	}
}
