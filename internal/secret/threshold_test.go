package secret

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// corpus is the calibration set for IsHighEntropyValue. Each entry records the
// expected classification and the reasoning behind it, so future threshold
// adjustments can be validated against a known baseline.
//
// Note: documentation placeholders (e.g. "AKIAIOSFODNN7EXAMPLE") are
// intentionally low-entropy for readability. Real credentials are caught by
// the pattern-based masker (masker.go). The entropy check is a second-line
// defence for unknown secret formats with genuinely random content.
var corpus = []struct {
	name     string
	value    string
	wantMask bool
}{
	// --- Real secrets: high-entropy random tokens (must be masked) ---
	{
		// Realistic AWS access key ID: AKIA + 16 random base-36 chars.
		// entropy ≈ 4.2 bits/char.
		name:     "realistic AWS-style access key",
		value:    "AKIAT3LGPZEQH5XMR4NW",
		wantMask: true,
	},
	{
		// Truly random 32-char alphanumeric token (broader charset than hex).
		// entropy ≈ 4.9 bits/char.
		name:     "random 32-char alphanumeric token",
		value:    "N4vK8xP2mQ6wT1hL9rY5bJ3cZ7sG0dFe",
		wantMask: true,
	},
	// --- Note on pure-hex secrets ---
	// Pure hex strings (0-9a-f) are bounded to log2(16) = 4.0 bits/char.
	// A 32-char random hex string typically scores 3.8-4.0 bits, sitting
	// right at the threshold boundary. Known hex-format secrets (MD5 hashes
	// used as keys, etc.) are better caught by the pattern-based masker.
	// Entropy-based detection targets higher-entropy alphanumeric/base64
	// tokens that lack a recognisable prefix pattern.
	{
		// OpenAI sk- style: sk- + 29 random alphanumeric chars.
		// entropy ≈ 5.0 bits/char.
		name:     "OpenAI sk- key body",
		value:    "sk-abcdefghijklmnopqrstuvwxyz1234",
		wantMask: true,
	},
	{
		// Generic random alphanumeric token (mixed case + digits).
		// entropy ≈ 5.0 bits/char.
		name:     "random alphanumeric 32-char token",
		value:    "Xk9f2pL8nQ3rJ7vM0wA1sD4tY6uI5oP",
		wantMask: true,
	},
	{
		// JWT: three base64url segments joined by dots.
		// entropy well above threshold due to base64url alphabet.
		name:     "long JWT (high entropy body)",
		value:    "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c",
		wantMask: true,
	},
	{
		// 32 random lowercase + digit chars (GitHub PAT body style).
		// entropy ≈ 5.0 bits/char.
		name:     "GitHub PAT body without prefix",
		value:    "abcdefghijklmnopqrstuvwxyz012345",
		wantMask: true,
	},

	// --- Legitimate values (must NOT be masked) ---
	{
		name:     "short value below MinSecretLen",
		value:    "production",
		wantMask: false,
	},
	{
		name:     "semver string",
		value:    "1.2.3",
		wantMask: false,
	},
	{
		// Typical English env-var text, entropy ≈ 3.8 bits/char, below threshold.
		name:     "normal English sentence",
		value:    "hello world this is a normal environment variable value",
		wantMask: false,
	},
	{
		name:     "repeated character string",
		value:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		wantMask: false,
	},
	{
		name:     "numeric only value",
		value:    "12345678901234567890",
		wantMask: false,
	},

	// --- Documentation placeholder: low entropy by design ---
	// "AKIAIOSFODNN7EXAMPLE" is the canonical AWS documentation key.
	// Its entropy (≈ 3.7 bits/char) is below EntropyThreshold — that is
	// intentional: the pattern-based masker catches it via the AKIA regex.
	// The entropy check is not expected to mask it independently.
	{
		name:     "AWS doc placeholder (caught by pattern, not entropy)",
		value:    "AKIAIOSFODNN7EXAMPLE",
		wantMask: false, // entropy ≈ 3.7; pattern masker handles it instead
	},

	// --- Gray area: base64 of normal data ---
	// Base64 encoding maps bytes to a 64-char alphabet, raising the apparent
	// entropy to ~4.5 bits/char even for ordinary text. We accept this as a
	// deliberate fail-secure false-positive.
	{
		name:  "base64 of normal English text (accepted false-positive)",
		value: "dGhpcyBpcyBhIHRlc3QgYmFzZTY0IHN0cmluZyB0aGF0IGlzIG5vcm1hbA==",
		// base64("this is a test base64 string that is normal")
		wantMask: true,
	},
}

func TestThresholdCorpus(t *testing.T) {
	for _, tc := range corpus {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := IsHighEntropyValue(tc.value)
			if got != tc.wantMask {
				t.Errorf("IsHighEntropyValue(%q) = %v, want %v  (entropy=%.3f, runeLen=%d)",
					tc.value, got, tc.wantMask,
					ShannonEntropy(tc.value), len([]rune(tc.value)))
			}
		})
	}
}

func TestShannonEntropy(t *testing.T) {
	const eps = 1e-9
	tests := []struct {
		s       string
		minBits float64
		maxBits float64
	}{
		{"", 0, 0},
		{"aaaa", 0, 0},                                         // all identical chars → 0 entropy
		{"ab", 1.0, 1.0},                                       // two equally-likely chars → 1 bit
		{"abcdefghijklmnopqrstuvwxyz0123456789", 5.0, 6.0},     // high entropy
		{"こんにちは世界テスト日本語", 3.0, 4.0},                // Unicode: each rune unique → high entropy
	}
	for _, tc := range tests {
		tc := tc
		t.Run(fmt.Sprintf("%q", tc.s), func(t *testing.T) {
			got := ShannonEntropy(tc.s)
			if tc.minBits == tc.maxBits {
				if math.Abs(got-tc.minBits) > eps {
					t.Errorf("ShannonEntropy(%q) = %.10f, want %.4f (±%g)", tc.s, got, tc.minBits, eps)
				}
			} else if got < tc.minBits || got > tc.maxBits {
				t.Errorf("ShannonEntropy(%q) = %.4f, want [%.4f, %.4f]", tc.s, got, tc.minBits, tc.maxBits)
			}
		})
	}
}

func TestIsHighEntropyValue_LengthBoundary(t *testing.T) {
	// Build a high-entropy string that straddles the MinSecretLen boundary.
	// Use a string where every character is unique to maximise entropy.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	just19 := alphabet[:MinSecretLen-1] // 19 chars — below threshold
	exactly20 := alphabet[:MinSecretLen] // 20 chars — at threshold

	if IsHighEntropyValue(just19) {
		t.Errorf("IsHighEntropyValue(%q) = true for %d-char string, want false",
			just19, MinSecretLen-1)
	}
	if !IsHighEntropyValue(exactly20) {
		t.Errorf("IsHighEntropyValue(%q) = false for %d-char string with high entropy, want true",
			exactly20, MinSecretLen)
	}
}

func TestMaskEnv_MasksHighEntropyValues(t *testing.T) {
	env := map[string]string{
		// High-entropy random alphanumeric token — must be masked.
		"API_KEY":     "Xk9f2pL8nQ3rJ7vM0wA1sD4tY6uI5oP",
		"HOME":        "/home/user",
		"PATH":        "/usr/local/bin:/usr/bin:/bin",
		"APP_VERSION": "1.2.3",
		"DEBUG":       "true",
	}

	masked, secrets := MaskEnv(env)

	if masked["API_KEY"] != Replacement {
		t.Errorf("API_KEY should be masked, got %q", masked["API_KEY"])
	}
	if masked["HOME"] != "/home/user" {
		t.Errorf("HOME should be unchanged (allowlist), got %q", masked["HOME"])
	}
	if masked["PATH"] != "/usr/local/bin:/usr/bin:/bin" {
		t.Errorf("PATH should be unchanged (allowlist), got %q", masked["PATH"])
	}
	if masked["APP_VERSION"] != "1.2.3" {
		t.Errorf("APP_VERSION should be unchanged (low entropy), got %q", masked["APP_VERSION"])
	}
	if masked["DEBUG"] != "true" {
		t.Errorf("DEBUG should be unchanged (short), got %q", masked["DEBUG"])
	}
	if len(secrets) != 1 {
		t.Fatalf("expected exactly one secret value, got %v", secrets)
	}
	if secrets[0] != "Xk9f2pL8nQ3rJ7vM0wA1sD4tY6uI5oP" {
		t.Errorf("secrets[0] = %q, want %q", secrets[0], "Xk9f2pL8nQ3rJ7vM0wA1sD4tY6uI5oP")
	}
}

func TestMaskEnv_SecretsAreSorted(t *testing.T) {
	// Verify that the returned secrets slice is sorted regardless of map
	// iteration order, so downstream code sees a deterministic result.
	env := map[string]string{
		"KEY_A": "Xk9f2pL8nQ3rJ7vM0wA1sD4tY6uI5oP",
		"KEY_B": "N4vK8xP2mQ6wT1hL9rY5bJ3cZ7sG0dFe",
		"KEY_C": "AKIAT3LGPZEQH5XMR4NW",
	}
	_, secrets := MaskEnv(env)
	for i := 1; i < len(secrets); i++ {
		if secrets[i] < secrets[i-1] {
			t.Errorf("secrets not sorted: secrets[%d]=%q < secrets[%d]=%q",
				i, secrets[i], i-1, secrets[i-1])
		}
	}
}

func TestMaskEnv_FailSecure_UnknownKey(t *testing.T) {
	env := map[string]string{
		"UNKNOWN_VAR": "Xk9f2pL8nQ3rJ7vM0wA1sD4tY6uI5oP",
	}
	masked, _ := MaskEnv(env)
	if masked["UNKNOWN_VAR"] != Replacement {
		t.Errorf("high-entropy value with unknown key name must be masked (fail-secure), got %q", masked["UNKNOWN_VAR"])
	}
}

func TestMaskEnv_PreservesAllowlistedKeys(t *testing.T) {
	// Test all keys in safeEnvKeys to catch future additions that are untested.
	for key := range safeEnvKeys {
		// Use a high-entropy-looking value to confirm the allowlist takes precedence.
		env := map[string]string{key: "Xk9f2pL8nQ3rJ7vM0wA1sD4tY6uI5oP"}
		masked, secrets := MaskEnv(env)
		if masked[key] != "Xk9f2pL8nQ3rJ7vM0wA1sD4tY6uI5oP" {
			t.Errorf("allowlisted key %s was masked, want original value", key)
		}
		if len(secrets) != 0 {
			t.Errorf("allowlisted key %s produced secrets %v, want none", key, secrets)
		}
	}
}

func TestNewMaskerWithLiterals_MasksLiteralString(t *testing.T) {
	m, err := NewMaskerWithLiterals(nil, []string{"supersecret12345"})
	if err != nil {
		t.Fatalf("NewMaskerWithLiterals: %v", err)
	}
	out, count := m.MaskString("the value is supersecret12345 here")
	if strings.Contains(out, "supersecret12345") {
		t.Errorf("literal not masked: %q", out)
	}
	if count == 0 {
		t.Errorf("count = 0, expected ≥1")
	}
}

func TestNewMaskerWithLiterals_EscapesRegexSpecialChars(t *testing.T) {
	// Literal contains regex special chars that must not be interpreted as a pattern.
	literal := "sk-[abcde]"
	m, err := NewMaskerWithLiterals(nil, []string{literal})
	if err != nil {
		t.Fatalf("NewMaskerWithLiterals: %v", err)
	}
	// The literal "sk-[abcde]" should match as-is, not as a character class.
	out, count := m.MaskString("token sk-[abcde] end")
	if strings.Contains(out, "sk-[abcde]") {
		t.Errorf("literal with special chars not masked: %q", out)
	}
	if count == 0 {
		t.Errorf("count = 0")
	}
	// "sk-a" should NOT be masked (it was not the literal).
	out2, count2 := m.MaskString("prefix sk-a suffix padding padding padding")
	if !strings.Contains(out2, "sk-a") {
		t.Errorf("sk-a should not be masked by literal sk-[abcde], got %q", out2)
	}
	_ = count2
}

func TestNewMaskerWithLiterals_SkipsEmptyLiterals(t *testing.T) {
	// An empty literal must not produce an all-positions-match regex.
	m, err := NewMaskerWithLiterals(nil, []string{"", "validtoken123456789012345"})
	if err != nil {
		t.Fatalf("NewMaskerWithLiterals: %v", err)
	}
	out, _ := m.MaskString("hello world")
	if out != "hello world" {
		t.Errorf("empty literal caused unexpected masking: %q", out)
	}
}

func TestNewMaskerWithLiterals_NilInputs(t *testing.T) {
	m, err := NewMaskerWithLiterals(nil, nil)
	if err != nil {
		t.Fatalf("nil inputs should not error: %v", err)
	}
	out, count := m.MaskString("plain text")
	if count != 0 {
		t.Errorf("nil-input masker masked plain text, count=%d, out=%q", count, out)
	}
}
