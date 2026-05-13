// Package secret implements the secret-masking layer described in the plan.
// It is fail-secure: any patterns the user adds must compile, or construction
// fails so we never silently disable a guard.
package secret

import (
	"fmt"
	"io"
	"regexp"
)

// NewMaskerWithLiterals returns a Masker that uses the built-in patterns plus
// any extra user-supplied regexes and additional literal strings. Literals are
// escaped with regexp.QuoteMeta before use, so they match as-is in output.
// Empty strings in literals are silently skipped to prevent an empty-pattern
// regex from matching every position in the output.
func NewMaskerWithLiterals(extraPatterns []string, literals []string) (*Masker, error) {
	quoted := make([]string, 0, len(literals))
	for _, lit := range literals {
		if lit == "" {
			continue
		}
		quoted = append(quoted, regexp.QuoteMeta(lit))
	}
	combined := make([]string, 0, len(extraPatterns)+len(quoted))
	combined = append(combined, extraPatterns...)
	combined = append(combined, quoted...)
	return NewMasker(combined)
}

// defaultPatterns are the built-in secret patterns. They are intentionally
// conservative so we can keep extending them without breaking callers.
//
// References:
//   - AWS access key id: starts with AKIA or ASIA followed by 16 alnum chars
//   - Bearer/JWT: header value space-separated long opaque token
//   - GitHub PAT: ghp_, gho_, ghu_, ghs_, ghr_ prefixes
//   - OpenAI API key: sk-* style
var defaultPatterns = []string{
	`AKIA[0-9A-Z]{16}`,
	`ASIA[0-9A-Z]{16}`,
	`gh[pousr]_[A-Za-z0-9]{20,}`,
	`sk-[A-Za-z0-9]{20,}`,
	`(?i)bearer\s+[A-Za-z0-9._\-]{20,}`,
	`eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`,
}

// Replacement is the string substituted in place of detected secrets.
// It is exported so callers can compare against it without hard-coding "***".
const Replacement = "***"

const replacement = Replacement

// Masker rewrites known-secret substrings to a fixed redaction marker.
type Masker struct {
	patterns []*regexp.Regexp
}

// DefaultMasker returns a Masker initialised with the built-in patterns.
func DefaultMasker() *Masker {
	m, err := NewMasker(nil)
	if err != nil {
		// defaultPatterns is a developer-controlled constant; any failure
		// here is a bug, not a runtime condition.
		panic(fmt.Sprintf("shtrace: default secret patterns failed to compile: %v", err))
	}
	return m
}

// NewMasker returns a Masker that uses the built-in patterns plus any extra
// user-supplied regexes. Bad user patterns produce an error (fail-secure).
func NewMasker(extra []string) (*Masker, error) {
	all := make([]string, 0, len(defaultPatterns)+len(extra))
	all = append(all, defaultPatterns...)
	all = append(all, extra...)

	compiled := make([]*regexp.Regexp, 0, len(all))
	for _, p := range all {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("compile pattern %q: %w", p, err)
		}
		compiled = append(compiled, re)
	}
	return &Masker{patterns: compiled}, nil
}

// MaskString returns s with every match replaced by `***`, plus the number of
// replacements made (the count is what `shtrace pr-comment` will surface).
func (m *Masker) MaskString(s string) (string, int) {
	count := 0
	out := s
	for _, re := range m.patterns {
		// For "bearer <token>" we want to keep the bearer prefix readable, so
		// the regex match itself carries that prefix. ReplaceAllStringFunc
		// lets us count occurrences while substituting.
		out = re.ReplaceAllStringFunc(out, func(match string) string {
			count++
			// Preserve a leading "Bearer " (case-insensitive) so logs stay
			// diagnosable.
			if len(match) >= 7 {
				lower := match[:7]
				if equalFoldASCII(lower, "Bearer ") {
					return match[:7] + replacement
				}
			}
			return replacement
		})
	}
	return out, count
}

// MaskArgv applies MaskString to each argv entry.
func (m *Masker) MaskArgv(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		masked, _ := m.MaskString(a)
		out[i] = masked
	}
	return out
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// maskingWriter is a streaming masker that buffers a small tail across writes
// so a secret split across two Write calls still gets caught.
type maskingWriter struct {
	w      io.Writer
	masker *Masker
	buf    []byte
}

// safetyTail is the number of bytes we hold back so a secret straddling the
// boundary between two writes is still detected. It must exceed the longest
// expected secret literal we want to catch.
const safetyTail = 256

// NewMaskingWriter wraps w so that every write is masked before reaching w.
// Callers must Close() the writer to flush the tail buffer.
func NewMaskingWriter(w io.Writer, m *Masker) io.WriteCloser {
	return &maskingWriter{w: w, masker: m}
}

func (mw *maskingWriter) Write(p []byte) (int, error) {
	mw.buf = append(mw.buf, p...)
	// Only flush up to len(buf)-safetyTail bytes; keep the tail so a secret
	// straddling writes can still match.
	if len(mw.buf) <= safetyTail {
		return len(p), nil
	}
	flushable := mw.buf[:len(mw.buf)-safetyTail]
	masked, _ := mw.masker.MaskString(string(flushable))
	if _, err := mw.w.Write([]byte(masked)); err != nil {
		return 0, err
	}
	mw.buf = mw.buf[len(mw.buf)-safetyTail:]
	return len(p), nil
}

func (mw *maskingWriter) Close() error {
	if len(mw.buf) == 0 {
		return nil
	}
	masked, _ := mw.masker.MaskString(string(mw.buf))
	mw.buf = nil
	_, err := mw.w.Write([]byte(masked))
	return err
}
