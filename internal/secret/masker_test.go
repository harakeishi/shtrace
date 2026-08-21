package secret

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestMasker_MasksAWSAccessKey(t *testing.T) {
	// The AKIA fixture is assembled at run time: a literal AKIA + 16 upper
	// alnum in the source trips GitHub push protection even when fake.
	akia := "AKIA" + "EXAMPLEEXAMPLE00"

	for _, key := range []string{akia, "ASIAEXAMPLEEXAMPLE00"} {
		m := DefaultMasker()
		in := "deploy --key " + key + " done"

		got, count := m.MaskString(in)

		if strings.Contains(got, key) {
			t.Fatalf("AWS key leaked through: %q", got)
		}
		if count == 0 {
			t.Fatalf("MaskString returned count=0 for masked input %q", in)
		}
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

func TestMasker_MasksBearerToken_TabSeparated(t *testing.T) {
	m := DefaultMasker()
	in := "Authorization: Bearer\tabcdefghijklmnopqrstuvwxyz1234567890ABCDEF"

	got, _ := m.MaskString(in)

	if strings.Contains(got, "abcdefghijklmnopqrstuvwxyz1234567890ABCDEF") {
		t.Fatalf("tab-separated bearer token leaked: %q", got)
	}
	// The "Bearer\t" prefix should be preserved so logs stay diagnosable.
	if !strings.Contains(got, "Bearer\t") {
		t.Fatalf("Bearer\\t prefix should remain, got %q", got)
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

func TestStreamMasker_SecretSplitAcrossLargeWrite(t *testing.T) {
	// Reproduce the flush-boundary split bug: a single Write that delivers
	// more than safetyTail bytes, where a literal secret straddles the
	// cutoff position. The secret must NOT appear in the final output.
	secret := "LITERALSECRET_ABCDEFGH" // 22-char literal
	m, err := NewMaskerWithLiterals(nil, []string{secret})
	if err != nil {
		t.Fatalf("NewMaskerWithLiterals: %v", err)
	}

	// Build a payload whose total size exceeds safetyTail (256) so that the
	// writer attempts a flush. Place the secret near the flush boundary so
	// it would have been split by the old "mask flushable only" approach.
	prefix := strings.Repeat("A", safetyTail-4) // puts secret near boundary
	suffix := strings.Repeat("B", safetyTail)
	payload := prefix + secret + suffix

	var buf bytes.Buffer
	w := NewMaskingWriter(&buf, m)
	if _, err := io.WriteString(w, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if strings.Contains(buf.String(), secret) {
		t.Errorf("secret leaked through flush boundary: %q", buf.String())
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

// substringsOf returns overlapping windows of s, used to assert that no
// recognisable fragment of a secret survives masking (partial-match leakage).
func substringsOf(s string, window int) []string {
	if len(s) <= window {
		return []string{s}
	}
	out := make([]string, 0, len(s)-window+1)
	for i := 0; i+window <= len(s); i++ {
		out = append(out, s[i:i+window])
	}
	return out
}

func TestMasker_MasksCredentialFormats(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		secret string
	}{
		{
			name:   "openai project key",
			in:     "OPENAI_API_KEY=sk-proj-AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJ",
			secret: "sk-proj-AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJ",
		},
		{
			name:   "openai classic key",
			in:     "using key sk-AAAABBBBCCCCDDDDEEEEFFFF for the run",
			secret: "sk-AAAABBBBCCCCDDDDEEEEFFFF",
		},
		{
			name:   "github fine-grained pat",
			in:     "gh auth login --with-token github_pat_11AAAAAAA0AAAAAAAAAAAA_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
			secret: "github_pat_11AAAAAAA0AAAAAAAAAAAA_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
		},
		{
			name:   "aws secret access key assignment",
			in:     "AWS_SECRET_ACCESS_KEY=EXAMPLEFAKEEXAMPLEFAKEEXAMPLEFAKEEXAMPLE",
			secret: "EXAMPLEFAKEEXAMPLEFAKEEXAMPLEFAKEEXAMPLE",
		},
		{
			name:   "aws secret access key flag",
			in:     "aws s3 ls --secret-access-key EXAMPLEFAKEEXAMPLEFAKEEXAMPLEFAKEEXAMPLE",
			secret: "EXAMPLEFAKEEXAMPLEFAKEEXAMPLEFAKEEXAMPLE",
		},
		{
			name:   "connection url password",
			in:     "postgres://appuser:hunter2pw@db.example.invalid:5432/app",
			secret: "hunter2pw",
		},
		{
			name:   "connection url without user",
			in:     "redis://:onlypassword@cache.example.invalid:6379/0",
			secret: "onlypassword",
		},
		{
			name:   "slack bot token",
			in:     "SLACK_TOKEN=xoxb-EXAMPLE-FAKE-NOTAREALSLACKTOKEN-000000",
			secret: "xoxb-EXAMPLE-FAKE-NOTAREALSLACKTOKEN-000000",
		},
		{
			name:   "slack user token",
			in:     "posting with xoxp-EXAMPLE-FAKE-NOTAREALSLACKTOKEN-000000",
			secret: "xoxp-EXAMPLE-FAKE-NOTAREALSLACKTOKEN-000000",
		},
		{
			name:   "slack app token",
			in:     "xapp-1-A0000000000-1111111111111-aaaabbbbccccdddd",
			secret: "xapp-1-A0000000000-1111111111111-aaaabbbbccccdddd",
		},
		{
			name:   "google api key",
			in:     "maps key AIzaSyA00000000000000000000000000000000 loaded",
			secret: "AIzaSyA00000000000000000000000000000000",
		},
		{
			name:   "pem begin marker",
			in:     "-----BEGIN RSA PRIVATE KEY-----",
			secret: "BEGIN RSA PRIVATE KEY",
		},
		{
			name:   "pem body line",
			in:     "MIIEowIBAAKCAQEAxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
			secret: "MIIEowIBAAKCAQEAxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		},
		{
			name:   "generic password assignment",
			in:     "DB_PASSWORD=supersecretvalue",
			secret: "supersecretvalue",
		},
		{
			name:   "generic token colon form",
			in:     "registry_token: abcdef123456",
			secret: "abcdef123456",
		},
	}

	m := DefaultMasker()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, count := m.MaskString(tt.in)

			if count == 0 {
				t.Fatalf("count = 0, want > 0 for %q (got %q)", tt.in, got)
			}
			if strings.Contains(got, tt.secret) {
				t.Fatalf("secret survived masking: %q", got)
			}
			// Partial leakage: no 8-byte window of the secret may remain.
			for _, frag := range substringsOf(tt.secret, 8) {
				if strings.Contains(got, frag) {
					t.Fatalf("secret fragment %q leaked in %q", frag, got)
				}
			}
		})
	}
}

func TestMasker_KeepsContextReadable(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "keeps env var name",
			in:   "DB_PASSWORD=supersecretvalue",
			want: "DB_PASSWORD=" + Replacement,
		},
		{
			name: "keeps scheme user and host",
			in:   "postgres://appuser:hunter2pw@db.example.invalid:5432/app",
			want: "postgres://appuser:" + Replacement + "@db.example.invalid:5432/app",
		},
		{
			name: "keeps aws key name",
			in:   "AWS_SECRET_ACCESS_KEY=EXAMPLEFAKEEXAMPLEFAKEEXAMPLEFAKEEXAMPLE",
			want: "AWS_SECRET_ACCESS_KEY=" + Replacement,
		},
	}

	m := DefaultMasker()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := m.MaskString(tt.in); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMasker_DoesNotMaskOrdinaryOutput(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"prose", "the deployment finished, no errors were reported"},
		{"git sha", "commit da39a3ee5e6b4b0d3255bfef95601890afd80709 by alice"},
		{"file path", "/usr/local/lib/python3.11/site-packages/foo/bar.py"},
		{"password prose no value", "password: "},
		{"password prose sentence", `error: password authentication failed for user "app"`},
		{"short assignment", "TOKEN=abc"},
		{"plain url", "see https://docs.example.invalid/page for details"},
		{"passwd file mention", "passwd file at /etc/passwd"},
		{"certificate marker", "-----BEGIN CERTIFICATE-----"},
		{"content length header", "Content-Length: 1048576"},
		{"docker tag with sha", "pull registry.example.invalid/app:sha-da39a3ee5e6b4b0d3255bfef95601890afd80709"},
	}

	m := DefaultMasker()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, count := m.MaskString(tt.in)
			if got != tt.in {
				t.Fatalf("ordinary output mutated: got %q, want %q", got, tt.in)
			}
			if count != 0 {
				t.Fatalf("count = %d, want 0", count)
			}
		})
	}
}

func TestStreamMasker_MasksPEMBodyLineByLine(t *testing.T) {
	// Full-block PEM masking is not achievable within the 256-byte safety tail,
	// so each line must be masked on its own even when the block is delivered
	// in fragments.
	m := DefaultMasker()
	body := strings.Repeat("MIIEowIBAAKCAQEAx", 8)

	var buf bytes.Buffer
	w := NewMaskingWriter(&buf, m)
	for _, chunk := range []string{
		"-----BEGIN RSA PRIVATE KEY-----\n",
		body + "\n",
		strings.Repeat("Z", 300) + "\n",
		"-----END RSA PRIVATE KEY-----\n",
	} {
		if _, err := io.WriteString(w, chunk); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	out := buf.String()
	for _, leaked := range []string{body, "PRIVATE KEY", strings.Repeat("Z", 300)} {
		if strings.Contains(out, leaked) {
			t.Fatalf("PEM material leaked: %q", out)
		}
	}
}
