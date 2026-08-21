// Package runner executes wrapped commands in either mode B (pipe;
// stdout/stderr split) or mode A (PTY; merged). This file covers mode B.
package runner

import (
	"context"
	"errors"
	"io"
	"os"
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
	// Stdin is forwarded to the child; nil means the child gets an empty
	// stdin, never the parent's. RunPipe takes ownership for the duration of
	// the call: a non-*os.File reader is drained by a relay goroutine that is
	// detached at return, so it may consume bytes past the child's exit. Do
	// not reuse such a reader across calls.
	Stdin  io.Reader
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
	// exec only hands an fd straight to the child for *os.File. For any other
	// reader it spawns an internal copier that cmd.Wait blocks on until the
	// reader hits EOF — so a reader that stays open (a TTY, a socket) hangs
	// Wait long after the child is gone. Normalizing through os.Pipe keeps
	// cmd.Wait tied to the child alone and lets us detach the relay instead.
	stdinCleanup, err := attachStdin(cmd, opt.Stdin)
	if err != nil {
		return Result{}, err
	}
	defer stdinCleanup()

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

// attachStdin wires r to cmd.Stdin as a real *os.File and returns a cleanup to
// run once cmd.Wait has returned.
//
// An *os.File is passed straight through: the child inherits the fd and no
// copier exists to outlive it. Anything else is relayed into an os.Pipe by a
// goroutine that is deliberately not awaited — a reader parked on a TTY cannot
// be interrupted, so waiting for it would hang every interactive run at exit.
// Closing the write end in cleanup makes the pending relay Write fail with
// ErrClosed rather than reach a recycled fd; a relay blocked in Read simply
// outlives the call, which is why Stdin ownership transfers to RunPipe.
func attachStdin(cmd *exec.Cmd, r io.Reader) (func(), error) {
	if r == nil {
		return func() {}, nil
	}
	if f, ok := r.(*os.File); ok {
		cmd.Stdin = f
		return func() {}, nil
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdin = pr

	go func() {
		_, _ = io.Copy(pw, r)
		_ = pw.Close()
	}()

	return func() {
		_ = pw.Close()
		_ = pr.Close()
	}, nil
}

// forward is the goroutine wrapper around forwardStream.
func forward(wg *sync.WaitGroup, src io.Reader, stream storage.Stream, tee io.Writer, rec ChunkWriter, m *secret.Masker) {
	defer wg.Done()
	forwardStream(src, stream, tee, rec, m)
}

// forwardStream reads from src and routes each chunk to (a) the user tee
// (raw bytes — the user already sees them on their terminal) and (b) the
// recorder, after secret masking. secret.StreamMasker owns the buffering that
// keeps a secret straddling a pipe-buffer boundary detectable.
func forwardStream(src io.Reader, stream storage.Stream, tee io.Writer, rec ChunkWriter, m *secret.Masker) {
	sm := secret.NewStreamMasker(m)
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if tee != nil {
				_, _ = tee.Write(buf[:n])
			}
			if out := sm.Write(buf[:n]); len(out) > 0 {
				_ = rec.WriteChunk(stream, out)
			}
		}
		if err != nil {
			if out := sm.Close(); len(out) > 0 {
				_ = rec.WriteChunk(stream, out)
			}
			return
		}
	}
}
