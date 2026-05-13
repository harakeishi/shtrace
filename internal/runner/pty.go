// This file covers mode A: PTY-based execution. When shtrace's stdout is a
// TTY, the child is launched inside a PTY so that colour output, progress
// bars, and interactive prompts are preserved. stdout and stderr are merged
// onto the single PTY master fd and recorded with stream="pty".

//go:build !windows

package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/harakeishi/shtrace/internal/secret"
	"github.com/harakeishi/shtrace/internal/storage"
)

// PTYOptions configures one mode A invocation.
type PTYOptions struct {
	Argv   []string
	Env    []string // optional; nil means inherit os.Environ
	Cwd    string   // optional; empty means inherit current cwd
	Writer ChunkWriter
	Tty    *os.File // terminal to forward PTY output to (typically os.Stdout)
	Masker *secret.Masker
}

// RunPTY spawns argv inside a PTY, forwards all output (stdout+stderr merged)
// to the caller's terminal verbatim, and records each chunk as stream="pty"
// after secret masking.
func RunPTY(ctx context.Context, opt PTYOptions) (Result, error) {
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

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = ptmx.Close() }()

	// Mirror terminal resize events to the PTY.
	if opt.Tty != nil {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGWINCH)
		go func() {
			for range ch {
				_ = pty.InheritSize(opt.Tty, ptmx)
			}
		}()
		ch <- syscall.SIGWINCH // set initial size
		defer func() {
			signal.Stop(ch)
			close(ch)
		}()
	}

	// Set the caller's terminal to raw mode so escape sequences pass through.
	if opt.Tty != nil {
		oldState, err := term.MakeRaw(int(opt.Tty.Fd()))
		if err == nil {
			defer func() { _ = term.Restore(int(opt.Tty.Fd()), oldState) }()
		}
	}

	// Single goroutine: PTY merges stdout+stderr onto one fd.
	forwardStream(ptmx, storage.StreamPTY, opt.Tty, opt.Writer, opt.Masker)

	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return Result{ExitCode: exitErr.ExitCode()}, nil
		}
		return Result{}, err
	}
	return Result{ExitCode: 0}, nil
}
