package runner

import (
	"strconv"
	"testing"

	"github.com/harakeishi/shtrace/internal/secret"
	"github.com/harakeishi/shtrace/internal/storage"
)

// feedExtract is a helper that converts the ordered event slice from Feed into
// (cleaned bytes, OSC sequence payloads) for concise test assertions.
func feedExtract(events []parserEvent) (cleaned []byte, seqs []string) {
	for _, e := range events {
		cleaned = append(cleaned, e.Bytes...)
		if e.Seq != "" {
			seqs = append(seqs, e.Seq)
		}
	}
	return
}

// TestOSCParser_NormalBytes verifies plain bytes pass through unchanged.
func TestOSCParser_NormalBytes(t *testing.T) {
	p := &oscParser{}
	cleaned, seqs := feedExtract(p.Feed([]byte("hello world\n")))
	if string(cleaned) != "hello world\n" {
		t.Errorf("cleaned = %q, want %q", cleaned, "hello world\n")
	}
	if len(seqs) != 0 {
		t.Errorf("seqs = %v, want none", seqs)
	}
}

// TestOSCParser_OSC133B verifies a command-start marker is parsed and stripped.
func TestOSCParser_OSC133B(t *testing.T) {
	p := &oscParser{}
	input := []byte("prompt\033]133;B\007output")
	cleaned, seqs := feedExtract(p.Feed(input))
	if string(cleaned) != "promptoutput" {
		t.Errorf("cleaned = %q, want %q", cleaned, "promptoutput")
	}
	if len(seqs) != 1 || seqs[0] != "133;B" {
		t.Errorf("seqs = %v, want [133;B]", seqs)
	}
}

// TestOSCParser_OSC133D verifies a command-end marker with exit code is parsed.
func TestOSCParser_OSC133D(t *testing.T) {
	p := &oscParser{}
	input := []byte("\033]133;D;42\007")
	cleaned, seqs := feedExtract(p.Feed(input))
	if len(cleaned) != 0 {
		t.Errorf("cleaned = %q, want empty", cleaned)
	}
	if len(seqs) != 1 || seqs[0] != "133;D;42" {
		t.Errorf("seqs = %v, want [133;D;42]", seqs)
	}
}

// TestOSCParser_STTerminator verifies ESC-backslash terminates an OSC sequence.
func TestOSCParser_STTerminator(t *testing.T) {
	p := &oscParser{}
	input := []byte("\033]133;B\033\\after")
	cleaned, seqs := feedExtract(p.Feed(input))
	if string(cleaned) != "after" {
		t.Errorf("cleaned = %q, want %q", cleaned, "after")
	}
	if len(seqs) != 1 || seqs[0] != "133;B" {
		t.Errorf("seqs = %v, want [133;B]", seqs)
	}
}

// TestOSCParser_SplitAcrossFeeds verifies an OSC sequence split across two Feed
// calls is reassembled correctly.
func TestOSCParser_SplitAcrossFeeds(t *testing.T) {
	p := &oscParser{}

	cleaned1, seqs1 := feedExtract(p.Feed([]byte("before\033]133;")))
	if string(cleaned1) != "before" {
		t.Errorf("first chunk cleaned = %q, want %q", cleaned1, "before")
	}
	if len(seqs1) != 0 {
		t.Errorf("first chunk seqs = %v, want none", seqs1)
	}

	cleaned2, seqs2 := feedExtract(p.Feed([]byte("B\007after")))
	if string(cleaned2) != "after" {
		t.Errorf("second chunk cleaned = %q, want %q", cleaned2, "after")
	}
	if len(seqs2) != 1 || seqs2[0] != "133;B" {
		t.Errorf("second chunk seqs = %v, want [133;B]", seqs2)
	}
}

// TestOSCParser_EscapeNotOSC verifies that ESC followed by a non-] byte is
// forwarded verbatim (e.g. other ANSI escape sequences).
func TestOSCParser_EscapeNotOSC(t *testing.T) {
	p := &oscParser{}
	// ESC [ is the CSI introducer used by ANSI colour codes.
	input := []byte("\033[32mgreen\033[0m")
	cleaned, seqs := feedExtract(p.Feed(input))
	if string(cleaned) != string(input) {
		t.Errorf("cleaned = %q, want %q", cleaned, input)
	}
	if len(seqs) != 0 {
		t.Errorf("seqs = %v, want none", seqs)
	}
}

// TestOSCParser_MultipleSequences verifies multiple OSC sequences in one chunk.
func TestOSCParser_MultipleSequences(t *testing.T) {
	p := &oscParser{}
	input := []byte("\033]133;B\007cmd output\033]133;D;0\007prompt")
	cleaned, seqs := feedExtract(p.Feed(input))
	if string(cleaned) != "cmd outputprompt" {
		t.Errorf("cleaned = %q, want %q", cleaned, "cmd outputprompt")
	}
	if len(seqs) != 2 || seqs[0] != "133;B" || seqs[1] != "133;D;0" {
		t.Errorf("seqs = %v, want [133;B 133;D;0]", seqs)
	}
}

// TestOSCParser_EventOrder verifies that bytes before B, between B and D, and
// after D appear in the correct positions in the event stream — specifically
// that bytes between B and D are emitted after the B event, so the caller can
// route them to the correct span writer.
func TestOSCParser_EventOrder(t *testing.T) {
	p := &oscParser{}
	// Simulates: prompt text, B marker, command output, D marker, next prompt.
	input := []byte("$ \033]133;B\007output\n\033]133;D;0\007$ ")
	events := p.Feed(input)

	// Expected event order: bytes("$ "), seq("133;B"), bytes("output\n"),
	// seq("133;D;0"), bytes("$ ")
	if len(events) != 5 {
		t.Fatalf("len(events) = %d, want 5; events = %v", len(events), events)
	}
	if string(events[0].Bytes) != "$ " {
		t.Errorf("events[0].Bytes = %q, want %q", events[0].Bytes, "$ ")
	}
	if events[1].Seq != "133;B" {
		t.Errorf("events[1].Seq = %q, want %q", events[1].Seq, "133;B")
	}
	if string(events[2].Bytes) != "output\n" {
		t.Errorf("events[2].Bytes = %q, want %q", events[2].Bytes, "output\n")
	}
	if events[3].Seq != "133;D;0" {
		t.Errorf("events[3].Seq = %q, want %q", events[3].Seq, "133;D;0")
	}
	if string(events[4].Bytes) != "$ " {
		t.Errorf("events[4].Bytes = %q, want %q", events[4].Bytes, "$ ")
	}
}

// TestOSCParser_InSTNonBackslash verifies that an ESC inside an OSC payload
// that is not followed by \ keeps the parser in OSC state (not Normal).
func TestOSCParser_InSTNonBackslash(t *testing.T) {
	p := &oscParser{}
	// \033]133; then \033X (not ST) then more payload then BEL
	input := []byte("\033]133;\033Xrest\007after")
	cleaned, seqs := feedExtract(p.Feed(input))
	// The entire sequence "133;\033Xrest" should be the payload.
	if string(cleaned) != "after" {
		t.Errorf("cleaned = %q, want %q", cleaned, "after")
	}
	if len(seqs) != 1 || seqs[0] != "133;\033Xrest" {
		t.Errorf("seqs = %v, want [133;\\033Xrest]", seqs)
	}
}

// TestOSCParser_OscBufOverflow verifies that an unterminated or oversized OSC
// sequence is discarded rather than causing unbounded memory growth.
func TestOSCParser_OscBufOverflow(t *testing.T) {
	p := &oscParser{}
	// Start an OSC sequence and feed more bytes than maxOSCBuf.
	big := make([]byte, maxOSCBuf+10)
	for i := range big {
		big[i] = 'x'
	}
	// Start OSC, feed big payload, terminate with BEL.
	input := append([]byte("\033]"), big...)
	input = append(input, '\007')
	input = append(input, []byte("after")...)

	cleaned, seqs := feedExtract(p.Feed(input))
	// Overflow sequence should be discarded (not emitted as a seq).
	if len(seqs) != 0 {
		t.Errorf("seqs = %v, want none (overflow should be discarded)", seqs)
	}
	// Bytes after the OSC terminator should be clean.
	if string(cleaned) != "after" {
		t.Errorf("cleaned = %q, want %q", cleaned, "after")
	}
}

// TestStripOSCUnsafe verifies that BEL and ESC are removed and all other bytes
// (including valid UTF-8 multibyte sequences) are preserved unchanged.
func TestStripOSCUnsafe(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"normal", "normal"},
		{"\007", ""},
		{"\033", ""},
		{"before\007after", "beforeafter"},
		{"before\033after", "beforeafter"},
		{"a\007\033b", "ab"},
		{"\007\033", ""},
		// multibyte UTF-8 must pass through unmodified (BEL/ESC are < 0x80)
		{"こんにちは", "こんにちは"},
		{"cmd\007arg", "cmdarg"},
	}
	for _, c := range cases {
		got := stripOSCUnsafe(c.in)
		if got != c.want {
			t.Errorf("stripOSCUnsafe(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestParseOSC133_B verifies B marker parsing without command text.
func TestParseOSC133_B(t *testing.T) {
	kind, arg, ok := parseOSC133("133;B")
	if !ok || kind != "B" || arg != "" {
		t.Errorf("parseOSC133(133;B) = (%q, %q, %v), want (B, \"\", true)", kind, arg, ok)
	}
}

// TestParseOSC133_BWithCommand verifies B marker carries the command string.
func TestParseOSC133_BWithCommand(t *testing.T) {
	kind, arg, ok := parseOSC133("133;B;ls -la /tmp")
	if !ok || kind != "B" || arg != "ls -la /tmp" {
		t.Errorf("parseOSC133(133;B;ls -la /tmp) = (%q, %q, %v), want (B, \"ls -la /tmp\", true)", kind, arg, ok)
	}
}

// TestParseOSC133_D verifies D marker carries the exit code as a string.
func TestParseOSC133_D(t *testing.T) {
	kind, arg, ok := parseOSC133("133;D;127")
	if !ok || kind != "D" || arg != "127" {
		t.Errorf("parseOSC133(133;D;127) = (%q, %q, %v), want (D, \"127\", true)", kind, arg, ok)
	}
}

// TestParseOSC133_NonShtrace verifies non-133 sequences are ignored.
func TestParseOSC133_NonShtrace(t *testing.T) {
	_, _, ok := parseOSC133("1;foo")
	if ok {
		t.Errorf("parseOSC133(1;foo) should not be ok")
	}
}

// TestParseOSC133_Zero verifies exit code 0 is handled.
func TestParseOSC133_Zero(t *testing.T) {
	kind, arg, ok := parseOSC133("133;D;0")
	if !ok || kind != "D" || arg != "0" {
		t.Errorf("parseOSC133(133;D;0) = (%q, %q, %v), want (D, \"0\", true)", kind, arg, ok)
	}
}

// TestShellOutputLoop_CommandPassedToBegin verifies that the command text from
// an OSC 133 B marker is passed to ShellSpan.Begin, so the CLI can record the
// actual command rather than just the shell name.
func TestShellOutputLoop_CommandPassedToBegin(t *testing.T) {
	// Simulate a PTY stream: prompt, B marker with command, output, D marker.
	stream := []byte("$ \033]133;B;echo hello\007hello\n\033]133;D;0\007$ ")

	type spanRecord struct {
		cmd  string
		exit int
	}
	var spans []spanRecord
	// pendingCmd holds the command from the most recent Begin call. It is safe
	// to share because shell commands are strictly sequential: End is always
	// called (implicitly via B or explicitly via D) before the next Begin fires.
	var pendingCmd string

	span := ShellSpan{
		Begin: func(command string) (ChunkWriter, error) {
			pendingCmd = command
			return &discardWriter{}, nil
		},
		End: func(exitCode int) error {
			cmd := pendingCmd // consume once per span
			pendingCmd = ""
			spans = append(spans, spanRecord{cmd: cmd, exit: exitCode})
			return nil
		},
	}

	masker, _ := newTestMasker()
	processShellEvents(stream, span, masker)

	if len(spans) != 1 {
		t.Fatalf("len(spans) = %d, want 1", len(spans))
	}
	if spans[0].cmd != "echo hello" {
		t.Errorf("span command = %q, want %q", spans[0].cmd, "echo hello")
	}
	if spans[0].exit != 0 {
		t.Errorf("span exit = %d, want 0", spans[0].exit)
	}
}

// newTestMasker returns a no-op Masker suitable for tests.
func newTestMasker() (*secret.Masker, error) {
	return secret.NewMaskerWithLiterals(nil, nil)
}

// discardWriter is a ChunkWriter that discards all output.
type discardWriter struct{}

func (d *discardWriter) WriteChunk(_ storage.Stream, _ []byte) error { return nil }

// processShellEvents is a test helper that feeds bytes through the OSC parser
// and fires span lifecycle callbacks, equivalent to the core of shellOutputLoop.
func processShellEvents(input []byte, span ShellSpan, masker *secret.Masker) {
	var parser oscParser
	var writer ChunkWriter
	var spanEnd func(int) error
	var pending []byte

	flushPending := func() {
		if writer == nil || len(pending) == 0 {
			return
		}
		masked, _ := masker.MaskString(string(pending))
		_ = writer.WriteChunk(storage.StreamPTY, []byte(masked))
		pending = pending[:0]
	}

	endSpan := func(code int) {
		flushPending()
		pending = nil // release backing array between spans
		if spanEnd != nil {
			_ = spanEnd(code)
		}
		writer = nil
		spanEnd = nil
	}

	// writeCleaned mirrors the safety-tail masking in production shellOutputLoop:
	// bytes are buffered in pending and flushed in safetyTail-sized chunks so
	// that secrets straddling chunk boundaries are caught by the masker.
	writeCleaned := func(b []byte) {
		if writer == nil || len(b) == 0 {
			return
		}
		pending = append(pending, b...)
		if len(pending) > safetyTail {
			masked, _ := masker.MaskString(string(pending))
			if len(masked) > safetyTail {
				cutoff := secret.UTF8Boundary(masked, len(masked)-safetyTail)
				_ = writer.WriteChunk(storage.StreamPTY, []byte(masked[:cutoff]))
				pending = []byte(masked[cutoff:])
			} else {
				pending = []byte(masked)
			}
		}
	}

	for _, ev := range parser.Feed(input) {
		if ev.Seq != "" {
			kind, arg, ok := parseOSC133(ev.Seq)
			if !ok {
				continue
			}
			switch kind {
			case "B":
				if spanEnd != nil {
					endSpan(-1)
				}
				if span.Begin != nil {
					w, err := span.Begin(stripOSCUnsafe(arg))
					if err == nil && w != nil {
						writer = w
						spanEnd = span.End
					}
				}
			case "D":
				if spanEnd != nil {
					code, _ := strconv.Atoi(arg)
					endSpan(code)
				}
			}
			continue
		}
		writeCleaned(ev.Bytes)
	}
	// If the input ended with an open span (B but no D), close it implicitly
	// so End is always called — mirroring the post-RunShell cleanup in cli.go.
	if spanEnd != nil {
		endSpan(-1)
	}
}
