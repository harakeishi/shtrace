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
	defer func() { _ = os.RemoveAll(tmpDir) }()

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
	defer func() { _ = ptmx.Close() }()

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
	// This goroutine exits naturally once ptmx is closed (the next write
	// attempt fails), which happens via the deferred ptmx.Close above.
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
// Events from the parser are processed in stream order so that command output
// appearing between a B and D marker is correctly attributed to its span.
func shellOutputLoop(ptmx *os.File, opt ShellOptions) {
	var (
		parser  oscParser
		writer  ChunkWriter
		spanEnd func(int) error
		pending []byte // masking safety-tail buffer for the current span
	)

	// flushPending emits all buffered bytes through the masker to writer,
	// then resets the buffer.
	flushPending := func() {
		if writer == nil || len(pending) == 0 {
			pending = pending[:0]
			return
		}
		masked, _ := opt.Masker.MaskString(string(pending))
		_ = writer.WriteChunk(storage.StreamPTY, []byte(masked))
		pending = pending[:0]
	}

	// endSpan flushes any remaining buffered output, calls spanEnd(code),
	// and resets writer/spanEnd/pending. The implicit flag indicates the span
	// ended without a D marker (abnormal path).
	endSpan := func(code int, implicit bool) {
		flushPending()
		if spanEnd != nil {
			label := "shtrace: span end"
			if implicit {
				label = "shtrace: span end (implicit)"
			}
			if eerr := spanEnd(code); eerr != nil && opt.Stderr != nil {
				_, _ = fmt.Fprintf(opt.Stderr, "%s: %v\n", label, eerr)
			}
		}
		writer = nil
		spanEnd = nil
	}

	// writeCleaned applies the safety-tail masking pattern (same as
	// forwardStream in pipe.go) so secrets straddling read boundaries are caught.
	writeCleaned := func(b []byte) {
		if writer == nil || len(b) == 0 {
			return
		}
		pending = append(pending, b...)
		if len(pending) > safetyTail {
			masked, _ := opt.Masker.MaskString(string(pending))
			if len(masked) > safetyTail {
				cutoff := secret.UTF8Boundary(masked, len(masked)-safetyTail)
				_ = writer.WriteChunk(storage.StreamPTY, []byte(masked[:cutoff]))
				pending = []byte(masked[cutoff:])
			} else {
				pending = []byte(masked)
			}
		}
	}

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

			// Process parser events in stream order so bytes and OSC markers
			// are handled relative to each other (not bytes-then-markers).
			for _, ev := range parser.Feed(raw) {
				if ev.Seq != "" {
					kind, code, ok := parseOSC133(ev.Seq)
					if !ok {
						continue
					}
					switch kind {
					case "B": // command output begins
						if spanEnd != nil {
							endSpan(-1, true)
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
							endSpan(code, false)
						}
					}
					continue
				}
				writeCleaned(ev.Bytes)
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
// Any DEBUG trap already set by the user's .bashrc is chained so it continues
// to fire (bash has no add-trap-hook equivalent, so we read and replay it).
const bashRC = `# shtrace: source user rc
[ -f "$HOME/.bashrc" ] && . "$HOME/.bashrc"

# shtrace span boundary detection via OSC 133 markers.
# __shtrace_cmd_fired prevents duplicate B markers for compound commands.
__shtrace_cmd_fired=0

# Preserve any DEBUG trap the user's rc installed so we can chain it.
# trap -p prints: trap -- 'BODY' DEBUG  — extract the body with sed.
# This works for most real-world trap bodies; it may misparse bodies that
# contain literal escaped single-quotes (rare in practice).
__shtrace_prev_debug=''
if __shtrace_trap_out=$(trap -p DEBUG 2>/dev/null) && [ -n "$__shtrace_trap_out" ]; then
    __shtrace_prev_debug=$(printf '%s' "$__shtrace_trap_out" | \
        sed "s/^trap -- '//;s/' DEBUG\$//")
fi

__shtrace_debug_hook() {
    [ "$__shtrace_cmd_fired" = "1" ] && return
    __shtrace_cmd_fired=1
    printf '\033]133;B\007'
    [ -n "$__shtrace_prev_debug" ] && eval "$__shtrace_prev_debug"
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
// add-zsh-hook is used so user-defined preexec/precmd hooks are preserved.
const zshRC = `# shtrace: source user rc (skip if ZDOTDIR was already our dir)
[ -f "$HOME/.zshrc" ] && [ "$HOME/.zshrc" != "$ZDOTDIR/.zshrc" ] && . "$HOME/.zshrc"

# shtrace span boundary detection via OSC 133 markers.
# add-zsh-hook is used instead of direct assignment so existing
# preexec/precmd hooks (e.g. from powerlevel10k, zsh-syntax-highlighting)
# are not overwritten.
autoload -Uz add-zsh-hook
__shtrace_preexec() { printf '\033]133;B\007' }
__shtrace_precmd()  { printf '\033]133;D;%d\007' "$?" }
add-zsh-hook preexec __shtrace_preexec
add-zsh-hook precmd  __shtrace_precmd
`

// maxOSCBuf is the upper bound on the number of bytes accumulated in oscBuf.
// If an unterminated OSC sequence exceeds this limit the sequence is discarded
// to prevent unbounded memory growth.
const maxOSCBuf = 64 * 1024

// parserEvent is one item yielded by oscParser.Feed in stream order.
// Exactly one of Bytes or Seq is non-empty.
type parserEvent struct {
	Bytes []byte // cleaned (non-OSC) bytes at this position in the stream
	Seq   string // complete OSC sequence payload (without ESC]/BEL/ST framing)
}

// oscParser strips OSC escape sequences from a PTY byte stream and emits
// cleaned bytes and OSC payloads in stream order, so the caller can process
// them relative to each other.
//
// OSC syntax: ESC ] <payload> BEL  or  ESC ] <payload> ESC \
type oscParser struct {
	state       int
	oscBuf      []byte
	oscOverflow bool // true when the current OSC payload exceeded maxOSCBuf
}

const (
	oscStateNormal = 0 // plain bytes
	oscStateESC    = 1 // saw \033, waiting for next byte
	oscStateInOSC  = 2 // inside an OSC sequence, collecting payload
	oscStateInST   = 3 // saw \033 while in OSC (may be start of ST = \033\)
)

// Feed processes a chunk of raw PTY bytes. Multiple calls accumulate parser
// state, so partial sequences that straddle calls are handled correctly.
// Events are emitted in the order the corresponding bytes appeared in the input.
func (p *oscParser) Feed(input []byte) []parserEvent {
	var events []parserEvent
	var cur []byte // accumulates cleaned bytes between OSC sequences

	flushCur := func() {
		if len(cur) > 0 {
			b := make([]byte, len(cur))
			copy(b, cur)
			events = append(events, parserEvent{Bytes: b})
			cur = cur[:0]
		}
	}

	emitSeq := func() {
		if !p.oscOverflow {
			flushCur()
			events = append(events, parserEvent{Seq: string(p.oscBuf)})
		}
		p.oscBuf = p.oscBuf[:0]
		p.oscOverflow = false
	}

	for i := 0; i < len(input); i++ {
		b := input[i]
		switch p.state {
		case oscStateNormal:
			if b == '\033' {
				p.state = oscStateESC
			} else {
				cur = append(cur, b)
			}
		case oscStateESC:
			if b == ']' {
				p.state = oscStateInOSC
				p.oscBuf = p.oscBuf[:0]
				p.oscOverflow = false
			} else {
				// Not an OSC introducer — emit the ESC plus this byte verbatim.
				cur = append(cur, '\033', b)
				p.state = oscStateNormal
			}
		case oscStateInOSC:
			switch b {
			case '\007': // BEL terminates the OSC
				emitSeq()
				p.state = oscStateNormal
			case '\033': // might be start of ST (ESC \)
				p.state = oscStateInST
			default:
				if len(p.oscBuf) < maxOSCBuf {
					p.oscBuf = append(p.oscBuf, b)
				} else {
					p.oscOverflow = true
				}
			}
		case oscStateInST:
			if b == '\\' {
				// ESC \ = String Terminator, completes the OSC sequence.
				emitSeq()
				p.state = oscStateNormal
			} else {
				// Not ST — treat the ESC as a literal inside the OSC payload.
				if len(p.oscBuf) < maxOSCBuf {
					p.oscBuf = append(p.oscBuf, '\033', b)
				} else {
					p.oscOverflow = true
				}
				// Remain inside the OSC sequence; do not return to Normal.
				p.state = oscStateInOSC
			}
		}
	}
	flushCur()
	return events
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
