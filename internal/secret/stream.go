package secret

import (
	"bytes"
	"regexp"
	"strings"
)

// SafetyTail is the number of bytes a StreamMasker holds back when a chunk
// contains no line break, so a secret straddling two writes is still matched
// as one span. It must exceed the longest expected secret literal.
const SafetyTail = 256

const safetyTail = SafetyTail

// pemBegin/pemEnd bound a private-key block. The body between them is masked
// line by line regardless of length, because PEM wraps at 64 chars and the
// final remainder line is usually too short for the generic body rule. A block
// left unterminated masks the rest of the stream — fail-secure, and bounded by
// the span or stream lifetime.
var (
	pemBegin = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	pemEnd   = regexp.MustCompile(`-----END [A-Z ]*PRIVATE KEY-----`)
)

// StreamMasker masks a byte stream that arrives in arbitrary chunks. It is the
// single implementation behind every streaming call site.
//
// Masking is line-oriented. Whole lines are masked and emitted; a trailing
// partial line stays buffered until its newline arrives. This matters because
// several rules are anchored with (?m)^…$: masking a partial line would let a
// chunk boundary act as a line boundary and redact ordinary output whose
// remainder had not been read yet. Only when a partial line grows past
// safetyTail without a newline is it masked early, to bound memory.
type StreamMasker struct {
	masker *Masker
	buf    []byte
	inPEM  bool
	// atLineStart reports whether buf begins at a real line start. It goes
	// false after a mid-line force-flush, and line-anchored rules stay off
	// until the next newline restores alignment.
	atLineStart bool
}

// NewStreamMasker returns a StreamMasker driven by m.
func NewStreamMasker(m *Masker) *StreamMasker {
	return &StreamMasker{masker: m, atLineStart: true}
}

// Write feeds p into the stream and returns the masked bytes that are ready to
// emit. The returned slice is only valid until the next call.
func (s *StreamMasker) Write(p []byte) []byte {
	s.buf = append(s.buf, p...)

	// Emit only through the last complete line; the remainder may still grow.
	idx := bytes.LastIndexByte(s.buf, '\n')
	if idx < 0 {
		if len(s.buf) <= safetyTail {
			return nil
		}
		// No newline in sight: mask what we can and hold back a tail so a
		// secret spanning the cut is still caught next time. The buffer is
		// mid-line, so line-anchored rules must not run on it.
		masked, _ := s.masker.maskString(string(s.buf), false)
		if len(masked) <= safetyTail {
			s.buf = []byte(masked)
			return nil
		}
		cutoff := UTF8Boundary(masked, len(masked)-safetyTail)
		s.buf = []byte(masked[cutoff:])
		s.atLineStart = false
		return []byte(masked[:cutoff])
	}

	complete := string(s.buf[:idx+1])
	rest := append([]byte(nil), s.buf[idx+1:]...)
	s.buf = rest
	out := s.maskLines(complete)
	s.atLineStart = true // the emitted span ended on a newline
	return []byte(out)
}

// Close masks and returns whatever remains buffered.
func (s *StreamMasker) Close() []byte {
	if len(s.buf) == 0 {
		return nil
	}
	out := s.maskLines(string(s.buf))
	s.buf = nil
	return []byte(out)
}

// maskLines applies the rule set line by line, carrying PEM block state across
// calls so every body line is redacted even when it is shorter than the
// standalone body rule's minimum.
func (s *StreamMasker) maskLines(chunk string) string {
	if !s.inPEM && !pemBegin.MatchString(chunk) {
		// Fast path: no block state to track for this chunk.
		out, _ := s.masker.maskString(chunk, s.atLineStart)
		return out
	}

	var b strings.Builder
	b.Grow(len(chunk))
	aligned := s.atLineStart
	for len(chunk) > 0 {
		line := chunk
		if i := strings.IndexByte(chunk, '\n'); i >= 0 {
			line, chunk = chunk[:i+1], chunk[i+1:]
		} else {
			chunk = ""
		}
		b.WriteString(s.maskLine(line, aligned))
		aligned = true // every subsequent line starts after a newline
	}
	return b.String()
}

// maskLine masks one line (newline included, if present) and updates PEM state.
// lineAligned reports whether this line starts at a real line boundary.
func (s *StreamMasker) maskLine(line string, lineAligned bool) string {
	body, eol := splitEOL(line)

	switch {
	case pemBegin.MatchString(body):
		s.inPEM = true
		out, _ := s.masker.maskString(body, lineAligned)
		return out + eol
	case s.inPEM && pemEnd.MatchString(body):
		s.inPEM = false
		out, _ := s.masker.maskString(body, lineAligned)
		return out + eol
	case s.inPEM:
		if strings.TrimSpace(body) == "" {
			return line
		}
		return replacement + eol
	default:
		out, _ := s.masker.maskString(body, lineAligned)
		return out + eol
	}
}

// splitEOL separates a line's trailing newline (and any CR) from its content.
func splitEOL(line string) (body, eol string) {
	n := len(line)
	if n > 0 && line[n-1] == '\n' {
		n--
		if n > 0 && line[n-1] == '\r' {
			n--
		}
	}
	return line[:n], line[n:]
}
