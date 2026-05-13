package secret

import "math"

// MinSecretLen is the minimum value length before entropy-based masking is
// applied. Values shorter than this are never auto-masked.
const MinSecretLen = 20

// EntropyThreshold is the minimum Shannon entropy (bits per character) above
// which a value is classified as a potential secret. Calibrated via the
// corpus in threshold_test.go.
//
// The value 3.9 was chosen so that:
//   - Typical English text (~3.8 bits/char) is not masked.
//   - Real API keys and random tokens (≥4.0 bits/char) are masked.
//   - Base64-encoded content is masked as a deliberate fail-secure choice.
const EntropyThreshold = 3.9

// safeEnvKeys are variable names whose values are never auto-masked, even
// when they happen to be high-entropy. Key-name allowlisting is a
// supplementary measure; entropy classification wins for all unlisted names.
var safeEnvKeys = map[string]bool{
	"PATH":    true,
	"HOME":    true,
	"USER":    true,
	"SHELL":   true,
	"TERM":    true,
	"LANG":    true,
	"LC_ALL":  true,
	"TMPDIR":  true,
	"TMP":     true,
	"TEMP":    true,
	"PWD":     true,
	"OLDPWD":  true,
	"LOGNAME": true,
	"DISPLAY": true,
	"HOSTNAME": true,
	"MANPATH": true,
}

// ShannonEntropy returns the Shannon entropy in bits per character of s.
// Returns 0 for the empty string.
func ShannonEntropy(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	freq := make(map[rune]int, 64)
	runes := []rune(s)
	for _, r := range runes {
		freq[r]++
	}
	n := float64(len(runes))
	var h float64
	for _, count := range freq {
		p := float64(count) / n
		h -= p * math.Log2(p)
	}
	return h
}

// IsHighEntropyValue reports whether value is long enough and has high enough
// Shannon entropy to be treated as a potential secret.
func IsHighEntropyValue(value string) bool {
	if len(value) < MinSecretLen {
		return false
	}
	return ShannonEntropy(value) >= EntropyThreshold
}

// MaskEnv returns a sanitised copy of env with high-entropy values replaced
// by the redaction marker, plus a slice of the original plaintext values so
// the caller can add them as literal masks to the I/O stream masker.
//
// Names listed in safeEnvKeys bypass entropy classification. All other
// high-entropy values are masked regardless of their key name (fail-secure).
func MaskEnv(env map[string]string) (masked map[string]string, secrets []string) {
	masked = make(map[string]string, len(env))
	for k, v := range env {
		if safeEnvKeys[k] || !IsHighEntropyValue(v) {
			masked[k] = v
			continue
		}
		masked[k] = replacement
		secrets = append(secrets, v)
	}
	return masked, secrets
}
