package secret

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestMasker_MasksAWSAccessKey(t *testing.T) {
	m := DefaultMasker()
	in := "deploy --key AKIAABCDEFGHIJKLMNOP done"

	got, count := m.MaskString(in)

	if strings.Contains(got, "AKIAABCDEFGHIJKLMNOP") {
		t.Fatalf("AWS key leaked through: %q", got)
	}
	if count == 0 {
		t.Fatalf("MaskString returned count=0 for masked input")
	}
}

func TestMasker_MasksBearerToken(t *testing.T) {
	m := DefaultMasker()
	in := "Authorization: Bearer abcdefghijklmnopqrstuvwxyz1234567890ABCDEF"

	got, _ := m.MaskString(in)

	if strings.Contains(got, "abcdefghijklmnopqrstuvwxyz1234567890ABCDEF") {
		t.Fatalf("bearer token leaked: %q", got)
	}
	if !strings.Contains(got, "Bearer ") {
		t.Fatalf("Bearer prefix should remain, got %q", got)
	}
}

func TestMasker_MasksGitHubPAT(t *testing.T) {
	m := DefaultMasker()
	in := "token ghp_abcdefghijklmnopqrstuvwxyz0123456789"

	got, count := m.MaskString(in)

	if strings.Contains(got, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("GitHub PAT leaked: %q", got)
	}
	if count == 0 {
		t.Fatalf("expected at least one mask, got 0")
	}
}

func TestMasker_LeavesPlainTextAlone(t *testing.T) {
	m := DefaultMasker()
	in := "hello world, running pytest tests/unit/test_login.py"

	got, count := m.MaskString(in)

	if got != in {
		t.Fatalf("plain text mutated: got %q, want %q", got, in)
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0 for plain text", count)
	}
}

func TestMasker_MaskArgv_ReplacesSecretEntries(t *testing.T) {
	m := DefaultMasker()
	argv := []string{"curl", "-H", "Authorization: Bearer abcdefghijklmnopqrstuvwxyz1234567890ABCDEF", "https://example.com"}

	got := m.MaskArgv(argv)

	if len(got) != len(argv) {
		t.Fatalf("MaskArgv changed argv length: got %d, want %d", len(got), len(argv))
	}
	if strings.Contains(got[2], "abcdefghijklmnopqrstuvwxyz1234567890ABCDEF") {
		t.Fatalf("argv masked output leaked secret: %q", got[2])
	}
	if got[0] != "curl" || got[3] != "https://example.com" {
		t.Fatalf("non-secret argv entries should be unchanged, got %v", got)
	}
}

func TestStreamMasker_MasksAcrossWrites(t *testing.T) {
	m := DefaultMasker()
	var buf bytes.Buffer

	w := NewMaskingWriter(&buf, m)
	if _, err := io.WriteString(w, "Authorization: "); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	if _, err := io.WriteString(w, "Bearer abcdefghijklmnopqrstuvwxyz1234567890ABCDEF\n"); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "abcdefghijklmnopqrstuvwxyz1234567890ABCDEF") {
		t.Fatalf("streamed bearer token leaked: %q", out)
	}
}

func TestMasker_FailsSecure_OnUserPatternCompileError(t *testing.T) {
	// A bad user-supplied pattern should not silently drop masking;
	// fail-secure means the constructor errors out.
	_, err := NewMasker([]string{"(unclosed"})
	if err == nil {
		t.Fatalf("expected NewMasker to fail on bad regex")
	}
}
