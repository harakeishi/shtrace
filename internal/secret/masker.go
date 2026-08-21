// Package secret implements the secret-masking layer described in the plan.
// It is fail-secure: any patterns the user adds must compile, or construction
// fails so we never silently disable a guard.
package secret

import (
	"fmt"
	"io"
	"regexp"
	"unicode/utf8"
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

// pattern is a built-in secret rule. When keepPrefix is true the regex's first
// capture group is a readable prefix (a scheme, a key name, a "Bearer" word)
// that is preserved so masked output stays diagnosable.
type pattern struct {
	expr       string
	keepPrefix bool
}

// defaultPatterns are the built-in secret rules, applied in order. They are
// intentionally conservative so we can keep extending them without breaking
// callers.
//
// Order matters: MaskString applies rules sequentially and replaced text cannot
// re-match, so the specific vendor formats must precede the generic
// name=value fallback at the end.
//
// References:
//   - AWS access key id: AKIA/ASIA followed by 16 alnum chars
//   - AWS secret access key: 40 base64-ish chars, no distinguishing prefix
//   - GitHub: ghp_/gho_/ghu_/ghs_/ghr_ classic, github_pat_ fine-grained
//   - OpenAI: sk-… including sk-proj-… project keys
//   - Slack: xoxb-/xoxp-/xoxa-/xoxs-/xapp-
//   - Google API key: AIza followed by 35 chars
//   - Bearer/JWT: header value space-separated long opaque token
//   - PEM: -----BEGIN … PRIVATE KEY----- blocks
var defaultPatterns = []pattern{
	{expr: `AKIA[0-9A-Z]{16}`},
	{expr: `ASIA[0-9A-Z]{16}`},
	{expr: `github_pat_[A-Za-z0-9_]{20,}`},
	{expr: `gh[pousr]_[A-Za-z0-9]{20,}`},
	// The character class must include - and _ or sk-proj-… keys stop matching
	// at the first hyphen and the remainder is stored in the clear. The
	// trailing [A-Za-z0-9] keeps sentence punctuation out of the match.
	{expr: `sk-[A-Za-z0-9_\-]{18,}[A-Za-z0-9]`},
	{expr: `xox[baprs]-[A-Za-z0-9\-]{10,}`},
	{expr: `xapp-[0-9]-[A-Za-z0-9\-]{10,}`},
	{expr: `AIza[0-9A-Za-z_\-]{35}`},
	{expr: `(?i)(bearer\s+)[A-Za-z0-9._\-]{20,}`, keepPrefix: true},
	{expr: `eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`},
	// An AWS secret access key has no prefix — a bare 40-char base64-ish match
	// would redact every git SHA-1 and build hash — so it is anchored to a
	// nearby key name instead.
	{expr: `(?i)(aws_?secret_?access_?key\s*[=:]\s*)["']?[A-Za-z0-9/+=]{40}["']?`, keepPrefix: true},
	{expr: `(?i)(--secret-access-key[\s=]+)["']?[A-Za-z0-9/+=]{40}["']?`, keepPrefix: true},
	// Connection URLs: mask only the password so scheme, user and host stay
	// readable.
	{expr: `([A-Za-z][A-Za-z0-9+.\-]*://[^\s:/@]*:)[^\s/@]+(@)`, keepPrefix: true},
	// The streaming maskers only hold a 256-byte tail across writes, so a
	// multi-line PEM block cannot be matched reliably as one span. Markers and
	// body lines are therefore matched independently. The body rule is not
	// anchored to a preceding BEGIN marker (that anchor would not survive a
	// write boundary either); a 60+ char unbroken base64 line is treated as key
	// material regardless of context.
	{expr: `-----BEGIN [A-Z ]*PRIVATE KEY-----`},
	{expr: `-----END [A-Z ]*PRIVATE KEY-----`},
	{expr: `(?m)^[A-Za-z0-9/+]{60,}={0,2}$`},
	// Generic fallback for name=value assignments echoed by scripts. The value
	// needs 6+ chars so prose like "password: " or "token: n/a" is left alone.
	// Runs last: replaced text cannot re-match, so it must not pre-empt the
	// vendor rules above.
	{expr: `(?i)([A-Za-z0-9_\-]*(?:token|secret|password|passwd|api[_-]?key)[A-Za-z0-9_\-]*\s*[=:]\s*)["']?[^\s"']{6,}["']?`, keepPrefix: true},
}

// Replacement is the string substituted in place of detected secrets.
// It is exported so callers can compare against it without hard-coding "***".
const Replacement = "***"

const replacement = Replacement

// Masker rewrites known-secret substrings to a fixed redaction marker.
type Masker struct {
	rules []rule
}

// rule is a compiled pattern plus its prefix-preserving behaviour.
type rule struct {
	re         *regexp.Regexp
	keepPrefix bool
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
	all := make([]pattern, 0, len(defaultPatterns)+len(extra))
	all = append(all, defaultPatterns...)
	for _, p := range extra {
		all = append(all, pattern{expr: p})
	}

	rules := make([]rule, 0, len(all))
	for _, p := range all {
		re, err := regexp.Compile(p.expr)
		if err != nil {
			return nil, fmt.Errorf("compile pattern %q: %w", p.expr, err)
		}
		if p.keepPrefix && re.NumSubexp() < 1 {
			return nil, fmt.Errorf("pattern %q declares keepPrefix but has no capture group", p.expr)
		}
		rules = append(rules, rule{re: re, keepPrefix: p.keepPrefix})
	}
	return &Masker{rules: rules}, nil
}

// MaskString returns s with every match replaced by Replacement, plus the
// number of replacements made (the count is what `shtrace pr-comment` will
// surface).
//
// Patterns are applied sequentially: once a span of text has been replaced by
// Replacement it cannot be matched again by a later pattern. In practice this
// means the count is accurate for non-overlapping matches; the rare case where
// a single span would match multiple patterns is counted only once.
func (m *Masker) MaskString(s string) (string, int) {
	count := 0
	out := s
	for _, r := range m.rules {
		if r.keepPrefix {
			// Rebuild the match from its captures so the readable prefix
			// (scheme, key name, "Bearer") and any trailing delimiter survive,
			// with only the secret span replaced.
			out = r.re.ReplaceAllStringFunc(out, func(match string) string {
				count++
				g := r.re.FindStringSubmatch(match)
				suffix := ""
				if len(g) > 2 {
					suffix = g[2]
				}
				return g[1] + replacement + suffix
			})
			continue
		}
		out = r.re.ReplaceAllStringFunc(out, func(string) string {
			count++
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

// UTF8Boundary returns the largest byte index ≤ pos at which s begins a valid
// UTF-8 rune, ensuring s[:result] never contains an incomplete multi-byte
// sequence. If pos ≥ len(s) it is clamped to len(s)-1.
func UTF8Boundary(s string, pos int) int {
	if pos >= len(s) {
		pos = len(s) - 1
	}
	for pos > 0 && !utf8.RuneStart(s[pos]) {
		pos--
	}
	return pos
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
	if len(mw.buf) <= safetyTail {
		return len(p), nil
	}
	// Mask the full buffer (not just the flushable prefix) so that a secret
	// whose start falls inside the safety tail of the previous flush is still
	// caught. Emit all but the last safetyTail characters of the masked output
	// and store those characters — already masked — as the new buffer. Storing
	// masked bytes is safe: replacement markers cannot match any pattern.
	masked, _ := mw.masker.MaskString(string(mw.buf))
	if len(masked) <= safetyTail {
		mw.buf = []byte(masked)
		return len(p), nil
	}
	cutoff := UTF8Boundary(masked, len(masked)-safetyTail)
	if _, err := mw.w.Write([]byte(masked[:cutoff])); err != nil {
		return 0, err
	}
	mw.buf = []byte(masked[cutoff:])
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
