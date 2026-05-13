package storage

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestJSONLWriter_WritesStdoutLine(t *testing.T) {
	var buf bytes.Buffer
	clock := fakeClock("2026-05-12T10:00:00.000Z")
	w := NewJSONLWriter(&buf, clock)

	if err := w.WriteChunk(StreamStdout, []byte("hello\n")); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}

	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("output missing trailing newline: %q", out)
	}

	var got Chunk
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("decode json line: %v: %q", err, out)
	}
	if got.Stream != "stdout" {
		t.Fatalf("stream = %q, want stdout", got.Stream)
	}
	if got.Data != "hello\n" {
		t.Fatalf("data = %q, want hello\\n", got.Data)
	}
	if got.TS == "" {
		t.Fatalf("ts missing")
	}
}

func TestJSONLWriter_PreservesStreamLabelForPTY(t *testing.T) {
	var buf bytes.Buffer
	w := NewJSONLWriter(&buf, fakeClock("2026-05-12T10:00:00.000Z"))

	if err := w.WriteChunk(StreamPTY, []byte("colored\x1b[31m output")); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}

	var got Chunk
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Stream != "pty" {
		t.Fatalf("stream = %q, want pty (mode A)", got.Stream)
	}
}

func TestJSONLWriter_MultipleChunks_OneLineEach(t *testing.T) {
	var buf bytes.Buffer
	w := NewJSONLWriter(&buf, fakeClock("2026-05-12T10:00:00.000Z"))

	if err := w.WriteChunk(StreamStdout, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteChunk(StreamStderr, []byte("b")); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), buf.String())
	}
}

func fakeClock(s string) func() time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return func() time.Time { return t }
}
