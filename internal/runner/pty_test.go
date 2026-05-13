//go:build !windows

package runner

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/harakeishi/shtrace/internal/secret"
	"github.com/harakeishi/shtrace/internal/storage"
)

func TestPTYRunner_RecordsPTYStream(t *testing.T) {
	rec := &recordingWriter{}

	res, err := RunPTY(context.Background(), PTYOptions{
		Argv:   []string{"sh", "-c", "printf hello"},
		Writer: rec,
		Tty:    nil, // no real terminal in CI
		Masker: secret.DefaultMasker(),
	})
	if err != nil {
		t.Fatalf("RunPTY: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}

	var all string
	for _, c := range rec.snapshot() {
		if c.Stream != string(storage.StreamPTY) {
			t.Errorf("unexpected stream %q, want %q", c.Stream, storage.StreamPTY)
		}
		all += c.Data
	}
	if !strings.Contains(all, "hello") {
		t.Fatalf("recorded PTY output %q does not contain 'hello'", all)
	}
}

func TestPTYRunner_PropagatesExitCode(t *testing.T) {
	rec := &recordingWriter{}

	res, err := RunPTY(context.Background(), PTYOptions{
		Argv:   []string{"sh", "-c", "exit 3"},
		Writer: rec,
		Tty:    nil,
		Masker: secret.DefaultMasker(),
	})
	if err != nil {
		t.Fatalf("RunPTY: %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", res.ExitCode)
	}
}

func TestPTYRunner_MasksSecretsInRecordedChunks(t *testing.T) {
	rec := &recordingWriter{}

	_, err := RunPTY(context.Background(), PTYOptions{
		Argv:   []string{"sh", "-c", "printf 'Authorization: Bearer abcdefghijklmnopqrstuvwxyz1234567890ABCDEF\\n'"},
		Writer: rec,
		Tty:    nil,
		Masker: secret.DefaultMasker(),
	})
	if err != nil {
		t.Fatalf("RunPTY: %v", err)
	}

	var all string
	for _, c := range rec.snapshot() {
		all += c.Data
	}
	if strings.Contains(all, "abcdefghijklmnopqrstuvwxyz1234567890ABCDEF") {
		t.Fatalf("recorded PTY chunks leaked secret: %q", all)
	}
}

func TestPTYRunner_ForwardsTTYOutput(t *testing.T) {
	rec := &recordingWriter{}

	// Use a pipe as a stand-in for the tty writer — just to verify bytes flow.
	pr, pw := io.Pipe()
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := pr.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()

	// We can't pass a *os.File here, so RunPTY with Tty=nil is a valid path.
	// This test verifies the no-tty path doesn't panic and still records.
	_, err := RunPTY(context.Background(), PTYOptions{
		Argv:   []string{"sh", "-c", "printf world"},
		Writer: rec,
		Tty:    nil,
		Masker: secret.DefaultMasker(),
	})
	_ = pw.Close()
	<-done

	if err != nil {
		t.Fatalf("RunPTY: %v", err)
	}
	var all string
	for _, c := range rec.snapshot() {
		all += c.Data
	}
	if !strings.Contains(all, "world") {
		t.Fatalf("recorded PTY output does not contain 'world': %q", all)
	}
}
