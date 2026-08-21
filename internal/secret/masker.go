// Package secret implements the secret-masking layer described in the plan.
// It is fail-secure: any patterns the user adds must compile, or construction
// fails so we never silently disable a guard.
package secret

import (
	"fmt"
	"io"
	"regexp"
	"strings"
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
//
// literals is a lowercase pre-filter: every group must contribute at least one
// present substring for the regex to have any chance of matching. MaskString
// skips the scan otherwise, which matters because the rules with a
// variable-length prefix have no literal for RE2 to anchor on and cost ~100x
// the prefixed rules per byte.
type pattern struct {
	expr       string
	keepPrefix bool
	literals   [][]string
	// lineAnchored marks a rule whose (?m)^…$ anchors require the input to be
	// cut on real line boundaries. Streaming skips these on a mid-line flush.
	lineAnchored bool
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
	// Braces are in the value class so ${VAR:-default} is inspected rather than
	// skipped; isVarRef still exempts a bare ${VAR}.
	{expr: `(?i)(bearer\s+)[A-Za-z0-9._${}:\-]{20,}`, keepPrefix: true, literals: [][]string{{"bearer"}}},
	{expr: `eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`},
	// An AWS secret access key has no prefix — a bare 40-char base64-ish match
	// would redact every git SHA-1 and build hash — so it is anchored to a
	// nearby key name instead.
	{expr: `(?i)(aws_?secret_?access_?key\s*[=:]\s*)["']?[A-Za-z0-9/+=]{40}["']?`, keepPrefix: true, literals: [][]string{{"access"}}},
	{expr: `(?i)(--secret-access-key[\s=]+)["']?[A-Za-z0-9/+=]{40}["']?`, keepPrefix: true, literals: [][]string{{"secret-access-key"}}},
	// Connection URLs: mask only the password so scheme, user and host stay
	// readable.
	{expr: `([A-Za-z][A-Za-z0-9+.\-]*://[^\s:/@]*:)[^\s/@]+(@)`, keepPrefix: true, literals: [][]string{{"://"}}},
	// Markers and body lines are matched independently so a PEM block is still
	// caught in the non-streaming path, where there is no block state. A 60+
	// char unbroken base64 line is treated as key material regardless of
	// context. StreamMasker additionally tracks BEGIN/END state, which is what
	// catches the short final body line.
	{expr: `-----BEGIN [A-Z ]*PRIVATE KEY-----`, literals: [][]string{{"-----begin"}}},
	{expr: `-----END [A-Z ]*PRIVATE KEY-----`, literals: [][]string{{"-----end"}}},
	// RE2's multiline $ matches only before \n, so \r must be consumed
	// explicitly or CRLF output (every PTY write, via ONLCR) leaves the body
	// unmasked.
	{expr: `(?m)^[A-Za-z0-9/+]{60,}={0,2}\r?$`, lineAnchored: true},
	// Generic fallback for name=value assignments echoed by scripts. The value
	// needs 6+ chars so prose like "password: " or "token: n/a" is left alone.
	// Runs last: replaced text cannot re-match, so it must not pre-empt the
	// vendor rules above.
	{expr: `(?i)([A-Za-z0-9_\-]*(?:token|secret|password|passwd|api[_-]?key)[A-Za-z0-9_\-]*\s*[=:]\s*)["']?[^\s"']{6,}["']?`, keepPrefix: true, literals: [][]string{{"token", "secret", "password", "passwd", "api"}, {"=", ":"}}},
}

// Replacement is the string substituted in place of detected secrets.
// It is exported so callers can compare against it without hard-coding "***".
const Replacement = "***"

const replacement = Replacement

// Masker rewrites known-secret substrings to a fixed redaction marker.
type Masker struct {
	rules []rule
	// anyLiterals records whether at least one rule declares literals, so
	// MaskString can skip building the lowercase copy the pre-filter needs.
	anyLiterals bool
}

// rule is a compiled pattern plus its prefix-preserving behaviour.
type rule struct {
	re           *regexp.Regexp
	keepPrefix   bool
	literals     [][]string
	lineAnchored bool
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
	anyLiterals := false
	for _, p := range all {
		re, err := regexp.Compile(p.expr)
		if err != nil {
			return nil, fmt.Errorf("compile pattern %q: %w", p.expr, err)
		}
		if p.keepPrefix && re.NumSubexp() < 1 {
			return nil, fmt.Errorf("pattern %q declares keepPrefix but has no capture group", p.expr)
		}
		if len(p.literals) > 0 {
			anyLiterals = true
		}
		rules = append(rules, rule{re: re, keepPrefix: p.keepPrefix, literals: p.literals, lineAnchored: p.lineAnchored})
	}
	return &Masker{rules: rules, anyLiterals: anyLiterals}, nil
}

// mayMatch reports whether the rule can possibly match input whose lowercase
// form is lower: every literal group must contribute a hit. A rule without
// literals is always scanned.
func (r rule) mayMatch(lower string) bool {
	for _, group := range r.literals {
		found := false
		for _, lit := range group {
			if strings.Contains(lower, lit) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
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
	return m.maskString(s, true)
}

// maskString masks s, optionally skipping rules whose line anchors are only
// meaningful when s is cut on real line boundaries.
func (m *Masker) maskString(s string, lineAligned bool) (string, int) {
	count := 0
	out := s
	// One lowercase copy feeds every rule's literal pre-filter; recomputed only
	// after a rule actually rewrote the text.
	lower := ""
	if m.anyLiterals {
		lower = strings.ToLower(out)
	}
	for _, r := range m.rules {
		if r.lineAnchored && !lineAligned {
			continue
		}
		if !r.mayMatch(lower) {
			continue
		}
		before := out
		if r.keepPrefix {
			// Rebuild the match from its captures so the readable prefix
			// (scheme, key name, "Bearer") and any trailing delimiter survive,
			// with only the secret span replaced.
			out = r.re.ReplaceAllStringFunc(out, func(match string) string {
				g := r.re.FindStringSubmatch(match)
				suffix := ""
				if len(g) > 2 {
					suffix = g[2]
				}
				// A value that is only a shell variable reference is what a script
				// echoes before expansion — redacting it hides context without
				// hiding a secret.
				if isVarRef(match[len(g[1]) : len(match)-len(suffix)]) {
					return match
				}
				count++
				return g[1] + replacement + suffix
			})
		} else {
			out = r.re.ReplaceAllStringFunc(out, func(string) string {
				count++
				return replacement
			})
		}
		if m.anyLiterals && out != before {
			lower = strings.ToLower(out)
		}
	}
	return out, count
}

// varRef matches a value that is nothing but an unexpanded variable reference:
// $NAME, ${NAME}, $(...) or the Windows %NAME% form, optionally quoted. The
// braced form is restricted to a bare name because ${VAR:-default} can carry a
// real credential in its default and must stay maskable.
var varRef = regexp.MustCompile(`^["']?(?:\$[A-Za-z_][A-Za-z0-9_]*|\$\{[A-Za-z_][A-Za-z0-9_]*\}|\$\([^)]*\)|%[A-Za-z_][A-Za-z0-9_]*%)["']?$`)

func isVarRef(value string) bool {
	return varRef.MatchString(value)
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

// maskingWriter adapts StreamMasker to io.WriteCloser.
type maskingWriter struct {
	w  io.Writer
	sm *StreamMasker
}

// NewMaskingWriter wraps w so that every write is masked before reaching w.
// Callers must Close() the writer to flush the buffered tail.
func NewMaskingWriter(w io.Writer, m *Masker) io.WriteCloser {
	return &maskingWriter{w: w, sm: NewStreamMasker(m)}
}

func (mw *maskingWriter) Write(p []byte) (int, error) {
	if out := mw.sm.Write(p); len(out) > 0 {
		if _, err := mw.w.Write(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (mw *maskingWriter) Close() error {
	out := mw.sm.Close()
	if len(out) == 0 {
		return nil
	}
	_, err := mw.w.Write(out)
	return err
}
