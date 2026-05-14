// Shell mode: spawns an interactive bash/zsh in a PTY and records each
// interactive command as a separate span by parsing OSC 133 shell-integration
// escape sequences injected by the shell's startup file.

//go:build !windows

package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/harakeishi/shtrace/internal/secret"
	"github.com/harakeishi/shtrace/internal/storage"
)

// ShellSpan holds the span lifecycle callbacks that RunShell calls for each
// interactive command the user runs.
type ShellSpan struct {
	// Begin is called when an interactive command starts. It returns a
	// ChunkWriter for the command's output. Returning a nil writer suppresses
	// recording for that command.
	Begin func() (ChunkWriter, error)
	// End is called when the command finishes with the given exit code.
	End func(exitCode int) error
}

// ShellOptions configures RunShell.
type ShellOptions struct {
	Shell  string       // "bash" or "zsh"
	Env    []string     // child environment; nil inherits os.Environ
	Cwd    string       // working directory; empty inherits current
	Tty    *os.File     // terminal for PTY forwarding (typically os.Stdout)
	Stderr io.Writer    // diagnostic messages
	Masker *secret.Masker
	Span   ShellSpan
}

// RunShell starts an interactive shell in a PTY. Each command the user runs
// in the shell becomes a separate span via OSC 133 markers injected by the
// shell's rc file.
func RunShell(ctx context.Context, opt ShellOptions) error {
	if opt.Masker == nil {
		return fmt.Errorf("runner: Masker is required")
	}

	tmpDir, err := os.MkdirTemp("", "shtrace-shell-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	argv, childEnv, err := shellInvocation(opt.Shell, tmpDir, opt.Env)
	if err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = childEnv
	if opt.Cwd != "" {
		cmd.Dir = opt.Cwd
	}

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("start shell pty: %w", err)
	}
	defer ptmx.Close()

	if opt.Tty != nil {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGWINCH)
		_ = pty.InheritSize(opt.Tty, ptmx)

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range sigCh {
				_ = pty.InheritSize(opt.Tty, ptmx)
			}
		}()
		defer func() {
			signal.Stop(sigCh)
			close(sigCh)
			wg.Wait()
		}()

		oldState, rawErr := term.MakeRaw(int(opt.Tty.Fd()))
		if rawErr != nil {
			if opt.Stderr != nil {
				_, _ = fmt.Fprintf(opt.Stderr, "shtrace: warning: MakeRaw: %v\n", rawErr)
			}
		} else {
			defer func() { _ = term.Restore(int(opt.Tty.Fd()), oldState) }()
		}
	}

	// Forward user's stdin to the shell (required for interactive use).
	go func() { _, _ = io.Copy(ptmx, os.Stdin) }()

	// Read PTY output: forward to terminal and record spans.
	shellOutputLoop(ptmx, opt)

	if err := cmd.Wait(); err != nil {
		// A non-zero exit from the shell is expected (e.g. the user ran a
		// failing command last, or typed exit N). Surface it only when it is
		// not an ExitError so the caller can propagate the exit code naturally.
		var exitErr *exec.ExitError
		if !isExitError(err, &exitErr) {
			return err
		}
	}
	return nil
}

// shellOutputLoop reads from the PTY master, forwards all bytes to the user
// terminal, and drives span recording by reacting to OSC 133 markers.
func shellOutputLoop(ptmx *os.File, opt ShellOptions) {
	var (
		parser  oscParser
		writer  ChunkWriter
		spanEnd func(int) error
	)

	buf := make([]byte, 32*1024)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			raw := buf[:n]

			// Forward all bytes (including OSC escape sequences) to the
			// user's terminal. Modern terminals silently consume OSC 133.
			if opt.Tty != nil {
				_, _ = opt.Tty.Write(raw)
			}

			// Strip OSC sequences from the recorded stream and collect events.
			cleaned, seqs := parser.Feed(raw)

			for _, seq := range seqs {
				kind, code, ok := parseOSC133(seq)
				if !ok {
					continue
				}
				switch kind {
				case "B": // command output begins
					if spanEnd != nil {
						// Previous span did not receive a D marker; close it.
						if eerr := spanEnd(-1); eerr != nil && opt.Stderr != nil {
							_, _ = fmt.Fprintf(opt.Stderr, "shtrace: span end (implicit): %v\n", eerr)
						}
						writer = nil
						spanEnd = nil
					}
					if opt.Span.Begin != nil {
						w, berr := opt.Span.Begin()
						if berr != nil {
							if opt.Stderr != nil {
								_, _ = fmt.Fprintf(opt.Stderr, "shtrace: span begin: %v\n", berr)
							}
						} else {
							writer = w
							spanEnd = opt.Span.End
						}
					}
				case "D": // command done
					if spanEnd != nil {
						if eerr := spanEnd(code); eerr != nil && opt.Stderr != nil {
							_, _ = fmt.Fprintf(opt.Stderr, "shtrace: span end: %v\n", eerr)
						}
						writer = nil
						spanEnd = nil
					}
				}
			}

			// Record cleaned output into the current active span.
			if writer != nil && len(cleaned) > 0 {
				masked, _ := opt.Masker.MaskString(string(cleaned))
				_ = writer.WriteChunk(storage.StreamPTY, []byte(masked))
			}
		}
		if err != nil {
			return
		}
	}
}

// shellInvocation returns the argv and environment for spawning the shell with
// the shtrace rc file sourced.
func shellInvocation(shell, tmpDir string, env []string) (argv, childEnv []string, err error) {
	switch shell {
	case "bash":
		rcPath := filepath.Join(tmpDir, "shtrace-rc.bash")
		if err := os.WriteFile(rcPath, []byte(bashRC), 0o600); err != nil {
			return nil, nil, fmt.Errorf("write bash rc: %w", err)
		}
		return []string{"bash", "--rcfile", rcPath, "-i"}, env, nil

	case "zsh":
		// ZDOTDIR redirects all zsh dot-file lookups to tmpDir, so we inject
		// our hooks without disturbing the user's ~/.zshrc layout.
		if err := os.WriteFile(filepath.Join(tmpDir, ".zshrc"), []byte(zshRC), 0o600); err != nil {
			return nil, nil, fmt.Errorf("write zsh rc: %w", err)
		}
		if env == nil {
			env = os.Environ()
		}
		childEnv = append(append([]string{}, env...), "ZDOTDIR="+tmpDir)
		return []string{"zsh", "-i"}, childEnv, nil

	default:
		return nil, nil, fmt.Errorf("unsupported shell %q (supported: bash, zsh)", shell)
	}
}

// bashRC is sourced as --rcfile when starting a bash shell. It loads the
// user's own .bashrc first, then installs OSC 133 hooks for span detection.
const bashRC = `# shtrace: source user rc
[ -f "$HOME/.bashrc" ] && . "$HOME/.bashrc"

# shtrace span boundary detection via OSC 133 markers.
# __shtrace_cmd_fired prevents duplicate B markers for compound commands.
__shtrace_cmd_fired=0

__shtrace_debug_hook() {
    [ "$__shtrace_cmd_fired" = "1" ] && return
    __shtrace_cmd_fired=1
    printf '\033]133;B\007'
}
trap '__shtrace_debug_hook' DEBUG

__shtrace_precmd() {
    local rc=$?
    if [ "$__shtrace_cmd_fired" = "1" ]; then
        __shtrace_cmd_fired=0
        printf '\033]133;D;%d\007' "$rc"
    fi
}
if [ -n "$PROMPT_COMMAND" ]; then
    PROMPT_COMMAND="__shtrace_precmd; $PROMPT_COMMAND"
else
    PROMPT_COMMAND="__shtrace_precmd"
fi
`

// zshRC is written to ZDOTDIR/.zshrc so zsh sources it on startup.
const zshRC = `# shtrace: source user rc (skip if ZDOTDIR was already our dir)
[ -f "$HOME/.zshrc" ] && [ "$HOME/.zshrc" != "$ZDOTDIR/.zshrc" ] && . "$HOME/.zshrc"

# shtrace span boundary detection via OSC 133 markers.
preexec() {
    printf '\033]133;B\007'
}
precmd() {
    printf '\033]133;D;%d\007' "$?"
}
`

// oscParser strips OSC escape sequences from a PTY byte stream. It returns the
// cleaned bytes (with OSC sequences removed) plus the payload strings of any
// complete OSC sequences encountered (without their ESC] / BEL / ST framing).
//
// OSC syntax: ESC ] <payload> BEL  or  ESC ] <payload> ESC \
type oscParser struct {
	state  int
	oscBuf []byte
}

const (
	oscStateNormal = 0 // plain bytes
	oscStateESC    = 1 // saw \033, waiting for next byte
	oscStateInOSC  = 2 // inside an OSC sequence, collecting payload
	oscStateInST   = 3 // saw \033 while in OSC (may be start of ST = \033\)
)

// Feed processes a chunk of raw PTY bytes. Multiple calls accumulate state, so
// partial sequences that straddle calls are handled correctly.
func (p *oscParser) Feed(input []byte) (cleaned []byte, seqs []string) {
	for i := 0; i < len(input); i++ {
		b := input[i]
		switch p.state {
		case oscStateNormal:
			if b == '\033' {
				p.state = oscStateESC
			} else {
				cleaned = append(cleaned, b)
			}
		case oscStateESC:
			if b == ']' {
				p.state = oscStateInOSC
				p.oscBuf = p.oscBuf[:0]
			} else {
				// Not an OSC introducer — emit the ESC plus this byte verbatim.
				cleaned = append(cleaned, '\033', b)
				p.state = oscStateNormal
			}
		case oscStateInOSC:
			switch b {
			case '\007': // BEL terminates the OSC
				seqs = append(seqs, string(p.oscBuf))
				p.oscBuf = p.oscBuf[:0]
				p.state = oscStateNormal
			case '\033': // might be start of ST (ESC \)
				p.state = oscStateInST
			default:
				p.oscBuf = append(p.oscBuf, b)
			}
		case oscStateInST:
			if b == '\\' {
				// ESC \ = String Terminator, completes the OSC sequence.
				seqs = append(seqs, string(p.oscBuf))
				p.oscBuf = p.oscBuf[:0]
			} else {
				// Not ST — treat the ESC as a literal inside the OSC payload.
				p.oscBuf = append(p.oscBuf, '\033', b)
			}
			p.state = oscStateNormal
		}
	}
	return
}

// parseOSC133 parses an OSC payload and returns the marker kind (A/B/C/D),
// the exit code (for D markers), and whether it was an OSC 133 sequence.
func parseOSC133(payload string) (kind string, exitCode int, ok bool) {
	if !strings.HasPrefix(payload, "133;") {
		return "", 0, false
	}
	rest := payload[4:]
	if idx := strings.IndexByte(rest, ';'); idx >= 0 {
		kind = rest[:idx]
		exitCode, _ = strconv.Atoi(rest[idx+1:])
	} else {
		kind = rest
	}
	return kind, exitCode, true
}

func isExitError(err error, out **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		if out != nil {
			*out = e
		}
		return true
	}
	return false
}
