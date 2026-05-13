// Package runner executes wrapped commands in either mode B (pipe;
// stdout/stderr split) or mode A (PTY; merged). This file covers mode B.
package runner

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"

	"github.com/harakeishi/shtrace/internal/secret"
	"github.com/harakeishi/shtrace/internal/storage"
)

// ChunkWriter is the narrow recording interface the runner depends on. The
// runner calls WriteChunk from two goroutines (one per stdout/stderr pipe), so
// implementations must be safe for concurrent use. storage.JSONLWriter is.
type ChunkWriter interface {
	WriteChunk(stream storage.Stream, data []byte) error
}

// PipeOptions configures one mode B invocation.
type PipeOptions struct {
	Argv   []string
	Env    []string // optional; nil means inherit os.Environ
	Cwd    string   // optional; empty means inherit current cwd
	Writer ChunkWriter
	Stdout io.Writer // tee target; pass io.Discard if the caller doesn't want a pass-through
	Stderr io.Writer
	Masker *secret.Masker
}

// Result captures runner outcome that the caller wants to persist.
type Result struct {
	ExitCode int
}

// RunPipe spawns argv with separate stdout/stderr pipes and forwards each
// chunk to (a) the user terminal (Stdout/Stderr) raw, and (b) the recorder
// after applying secret masking.
func RunPipe(ctx context.Context, opt PipeOptions) (Result, error) {
	if len(opt.Argv) == 0 {
		return Result{}, errors.New("runner: empty argv")
	}
	if opt.Writer == nil {
		return Result{}, errors.New("runner: Writer is required")
	}
	if opt.Masker == nil {
		return Result{}, errors.New("runner: Masker is required (fail-secure)")
	}

	cmd := exec.CommandContext(ctx, opt.Argv[0], opt.Argv[1:]...)
	if opt.Env != nil {
		cmd.Env = opt.Env
	}
	if opt.Cwd != "" {
		cmd.Dir = opt.Cwd
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, err
	}

	if err := cmd.Start(); err != nil {
		return Result{}, err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go forward(&wg, stdoutPipe, storage.StreamStdout, opt.Stdout, opt.Writer, opt.Masker)
	go forward(&wg, stderrPipe, storage.StreamStderr, opt.Stderr, opt.Writer, opt.Masker)
	wg.Wait()

	err = cmd.Wait()
	res := Result{ExitCode: 0}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			err = nil
		}
	}
	return res, err
}

// forward is the goroutine wrapper around forwardStream.
func forward(wg *sync.WaitGroup, src io.Reader, stream storage.Stream, tee io.Writer, rec ChunkWriter, m *secret.Masker) {
	defer wg.Done()
	forwardStream(src, stream, tee, rec, m)
}

// safetyTail is the number of trailing bytes we hold back per stream before
// masking, so a secret straddling two Reads still matches its regex. It must
// exceed the longest expected secret literal we want to catch.
const safetyTail = 256

// forwardStream reads from src and routes each chunk to (a) the user tee
// (raw bytes — the user already sees them on their terminal) and (b) the
// recorder, after secret masking. It buffers a trailing window between Reads
// so a secret that straddles a pipe-buffer boundary is still caught.
//
// Masking is always applied to the full pending buffer (not just the flushable
// prefix) so that a secret whose start falls inside the safety tail of the
// previous flush is still caught. After masking the full buffer we emit all
// but the last safetyTail characters of the masked output and store those
// safetyTail characters — already masked — as the new pending buffer. Storing
// masked bytes (rather than original bytes) is safe: replacement markers
// cannot match any secret pattern, so re-scanning them on the next iteration
// is a no-op.
func forwardStream(src io.Reader, stream storage.Stream, tee io.Writer, rec ChunkWriter, m *secret.Masker) {
	var pending []byte
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if tee != nil {
				_, _ = tee.Write(buf[:n])
			}
			pending = append(pending, buf[:n]...)
			if len(pending) > safetyTail {
				masked, _ := m.MaskString(string(pending))
				if len(masked) > safetyTail {
					cutoff := secret.UTF8Boundary(masked, len(masked)-safetyTail)
					_ = rec.WriteChunk(stream, []byte(masked[:cutoff]))
					pending = []byte(masked[cutoff:])
				} else {
					pending = []byte(masked)
				}
			}
		}
		if err != nil {
			if len(pending) > 0 {
				masked, _ := m.MaskString(string(pending))
				_ = rec.WriteChunk(stream, []byte(masked))
			}
			return
		}
	}
}
