package storage

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// Stream labels for the unified JSON Lines format. mode B emits stdout/stderr;
// mode A emits pty because the PTY interleaves both streams onto a single fd.
type Stream string

const (
	StreamStdout Stream = "stdout"
	StreamStderr Stream = "stderr"
	StreamPTY    Stream = "pty"
)

// Chunk is one line in the JSON Lines output log.
type Chunk struct {
	TS     string `json:"ts"`
	Stream string `json:"stream"`
	Data   string `json:"data"`
}

// JSONLWriter serialises Chunks into JSON Lines. It is safe for concurrent
// callers because mode B reads stdout and stderr from two goroutines.
type JSONLWriter struct {
	mu    sync.Mutex
	w     io.Writer
	clock func() time.Time
}

// NewJSONLWriter returns a writer that emits JSON Lines onto w. clock is
// injected so tests can pin timestamps.
func NewJSONLWriter(w io.Writer, clock func() time.Time) *JSONLWriter {
	if clock == nil {
		clock = time.Now
	}
	return &JSONLWriter{w: w, clock: clock}
}

// WriteChunk encodes one chunk as a single JSON line.
func (j *JSONLWriter) WriteChunk(stream Stream, data []byte) error {
	c := Chunk{
		TS:     j.clock().UTC().Format(time.RFC3339Nano),
		Stream: string(stream),
		Data:   string(data),
	}
	b, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal chunk: %w", err)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.w.Write(b); err != nil {
		return err
	}
	_, err = j.w.Write([]byte("\n"))
	return err
}
