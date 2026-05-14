package runner

import (
	"testing"
)

// TestOSCParser_NormalBytes verifies plain bytes pass through unchanged.
func TestOSCParser_NormalBytes(t *testing.T) {
	p := &oscParser{}
	cleaned, seqs := p.Feed([]byte("hello world\n"))
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
	cleaned, seqs := p.Feed(input)
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
	cleaned, seqs := p.Feed(input)
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
	cleaned, seqs := p.Feed(input)
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

	cleaned1, seqs1 := p.Feed([]byte("before\033]133;"))
	if string(cleaned1) != "before" {
		t.Errorf("first chunk cleaned = %q, want %q", cleaned1, "before")
	}
	if len(seqs1) != 0 {
		t.Errorf("first chunk seqs = %v, want none", seqs1)
	}

	cleaned2, seqs2 := p.Feed([]byte("B\007after"))
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
	cleaned, seqs := p.Feed(input)
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
	cleaned, seqs := p.Feed(input)
	if string(cleaned) != "cmd outputprompt" {
		t.Errorf("cleaned = %q, want %q", cleaned, "cmd outputprompt")
	}
	if len(seqs) != 2 || seqs[0] != "133;B" || seqs[1] != "133;D;0" {
		t.Errorf("seqs = %v, want [133;B 133;D;0]", seqs)
	}
}

// TestParseOSC133_B verifies B marker parsing.
func TestParseOSC133_B(t *testing.T) {
	kind, code, ok := parseOSC133("133;B")
	if !ok || kind != "B" || code != 0 {
		t.Errorf("parseOSC133(133;B) = (%q, %d, %v), want (B, 0, true)", kind, code, ok)
	}
}

// TestParseOSC133_D verifies D marker parsing with exit code.
func TestParseOSC133_D(t *testing.T) {
	kind, code, ok := parseOSC133("133;D;127")
	if !ok || kind != "D" || code != 127 {
		t.Errorf("parseOSC133(133;D;127) = (%q, %d, %v), want (D, 127, true)", kind, code, ok)
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
	kind, code, ok := parseOSC133("133;D;0")
	if !ok || kind != "D" || code != 0 {
		t.Errorf("parseOSC133(133;D;0) = (%q, %d, %v), want (D, 0, true)", kind, code, ok)
	}
}
