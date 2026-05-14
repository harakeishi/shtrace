package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
	"runtime"
	"strconv"
	"strings"

	"github.com/harakeishi/shtrace/internal/storage"
)

// testFramework holds the result of detecting one test framework in a span.
type testFramework struct {
	Name   string
	Passed int
	Failed int
	Total  int
}

// runPRComment implements `shtrace pr-comment`.
//
// It reads session data, detects test runs, and posts a comment to the
// specified GitHub pull request using the GitHub REST API.
// Required env vars (auto-set in GitHub Actions):
//   GITHUB_TOKEN (or GH_TOKEN), GITHUB_REPOSITORY, GITHUB_RUN_ID, GITHUB_SERVER_URL
func runPRComment(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	sessionID, latest, prNumber, err := parsePRCommentArgs(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		_, _ = fmt.Fprintln(stderr, "usage: shtrace pr-comment (--session <id> | --latest) --pr <number>")
		return 2
	}

	env := envMap()
	dataDir, err := storage.ResolveDataDir(env, runtime.GOOS)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		return 1
	}
	store, err := storage.Open(dataDir + "/sessions.db")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: open store: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: migrate: %v\n", err)
		return 1
	}

	warn := func(e error) { _, _ = fmt.Fprintf(stderr, "shtrace: warning: %v\n", e) }

	if latest {
		sessions, err := store.ListSessions(ctx, 1, warn)
		if err != nil || len(sessions) == 0 {
			_, _ = fmt.Fprintln(stderr, "shtrace: no sessions found")
			return 1
		}
		sessionID = sessions[0].ID
	}

	sess, err := store.GetSession(ctx, sessionID)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: session %s: %v\n", sessionID, err)
		return 1
	}
	spans, err := store.SpansForSession(ctx, sessionID, warn)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: spans: %v\n", err)
		return 1
	}

	// Detect test results from span output logs.
	// Cap each log read at 4 MB — sufficient to find any test summary line.
	const maxLogRead = 4 << 20
	var allFrameworks []testFramework
	for _, sp := range spans {
		logPath := storage.OutputPath(dataDir, sess.ID, sp.ID)
		f, err := os.Open(logPath)
		if err != nil {
			warn(fmt.Errorf("read log %s: %v", logPath, err))
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, maxLogRead))
		_ = f.Close()
		if err != nil {
			warn(fmt.Errorf("read log %s: %v", logPath, err))
			continue
		}
		fws := detectTestFrameworks(data)
		allFrameworks = append(allFrameworks, fws...)
	}

	token := env["GITHUB_TOKEN"]
	if token == "" {
		token = env["GH_TOKEN"]
	}
	repo := env["GITHUB_REPOSITORY"]
	runID := env["GITHUB_RUN_ID"]
	serverURL := env["GITHUB_SERVER_URL"]
	if serverURL == "" {
		serverURL = "https://github.com"
	}

	if _, err := strconv.Atoi(prNumber); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: invalid PR number %q: must be numeric\n", prNumber)
		return 2
	}

	body := buildPRCommentBody(sess, spans, allFrameworks, runID, repo, serverURL)
	_, _ = fmt.Fprint(stdout, body)
	_, _ = fmt.Fprintln(stdout)

	if token == "" || repo == "" {
		_, _ = fmt.Fprintln(stderr, "shtrace: GITHUB_TOKEN and GITHUB_REPOSITORY are required to post the comment")
		return 1
	}

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/issues/%s/comments", repo, prNumber)
	if err := postGitHubComment(ctx, apiURL, token, body); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: post comment: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "posted comment to PR #%s\n", prNumber)
	return 0
}

func buildPRCommentBody(sess storage.Session, spans []storage.Span, frameworks []testFramework, runID, repo, serverURL string) string {
	var sb strings.Builder

	sb.WriteString("## shtrace execution report\n\n")
	sb.WriteString(fmt.Sprintf("**Session:** `%s`  \n", sess.ID))
	sb.WriteString(fmt.Sprintf("**Spans:** %d  \n", len(spans)))

	// Test result summary.
	if len(frameworks) > 0 {
		sb.WriteString("\n### Test results\n\n")
		sb.WriteString("| Framework | Passed | Failed | Total |\n")
		sb.WriteString("|---|---|---|---|\n")
		for _, fw := range frameworks {
			sb.WriteString(fmt.Sprintf("| %s | %d | %d | %d |\n", fw.Name, fw.Passed, fw.Failed, fw.Total))
		}
	} else {
		sb.WriteString("\n_No test runs detected._\n")
	}

	// Artifact download instructions.
	if runID != "" && repo != "" {
		runURL := fmt.Sprintf("%s/%s/actions/runs/%s", serverURL, repo, runID)
		sb.WriteString("\n### Viewing the full report\n\n")
		sb.WriteString("```sh\n")
		sb.WriteString(fmt.Sprintf("gh run download --repo %s %s --name shtrace-report\n", repo, runID))
		sb.WriteString("tar xf shtrace-report.tar.gz\n")
		sb.WriteString("open shtrace-export/report.html   # macOS\n")
		sb.WriteString("xdg-open shtrace-export/report.html  # Linux\n")
		sb.WriteString("```\n")
		sb.WriteString(fmt.Sprintf("\n[View workflow run](%s)\n", runURL))
	}

	return sb.String()
}

func postGitHubComment(ctx context.Context, url, token, body string) error {
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(b))
		if readErr != nil {
			msg = "(error reading response: " + readErr.Error() + ")"
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	return nil
}

func parsePRCommentArgs(args []string) (sessionID string, latest bool, prNumber string, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--session":
			if i+1 >= len(args) {
				return "", false, "", fmt.Errorf("--session requires a value")
			}
			sessionID = args[i+1]
			i++
		case strings.HasPrefix(a, "--session="):
			sessionID = strings.TrimPrefix(a, "--session=")
		case a == "--latest":
			latest = true
		case a == "--pr":
			if i+1 >= len(args) {
				return "", false, "", fmt.Errorf("--pr requires a value")
			}
			prNumber = args[i+1]
			i++
		case strings.HasPrefix(a, "--pr="):
			prNumber = strings.TrimPrefix(a, "--pr=")
		default:
			return "", false, "", fmt.Errorf("unknown pr-comment flag %q", a)
		}
	}
	if sessionID == "" && !latest {
		return "", false, "", fmt.Errorf("either --session <id> or --latest is required")
	}
	if sessionID != "" && latest {
		return "", false, "", fmt.Errorf("--session and --latest are mutually exclusive")
	}
	if prNumber == "" {
		return "", false, "", fmt.Errorf("--pr <number> is required")
	}
	return sessionID, latest, prNumber, nil
}

// detectTestFrameworks scans JSON Lines output data and returns detected test
// framework results. It handles pytest, go test, jest/vitest, phpunit, and rspec.
func detectTestFrameworks(data []byte) []testFramework {
	lines := splitLines(data)
	var texts []string
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var c storage.Chunk
		if err := json.Unmarshal(line, &c); err == nil {
			texts = append(texts, c.Data)
		}
	}
	combined := strings.Join(texts, "\n")

	var results []testFramework

	// pytest: "X passed, Y failed in Z.ZZs" or "X passed in Z.ZZs"
	if fw, ok := detectPytest(combined); ok {
		results = append(results, fw)
	}
	// go test: "ok" / "FAIL" lines and "--- PASS/FAIL" patterns
	if fw, ok := detectGoTest(combined); ok {
		results = append(results, fw)
	}
	// jest/vitest: "Tests: X passed, Y failed, Z total"
	if fw, ok := detectJest(combined); ok {
		results = append(results, fw)
	}
	// phpunit: "OK (X tests, Y assertions)" or "FAILURES! Tests: X, ..."
	if fw, ok := detectPHPUnit(combined); ok {
		results = append(results, fw)
	}
	// rspec: "X examples, Y failures"
	if fw, ok := detectRSpec(combined); ok {
		results = append(results, fw)
	}

	return results
}

func detectPytest(s string) (testFramework, bool) {
	// e.g. "5 passed, 2 failed in 0.12s" or "5 passed in 0.12s"
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "passed") || !strings.Contains(line, " in ") {
			continue
		}
		fw := testFramework{Name: "pytest"}
		if n, err := strconv.Atoi(extractBefore(line, " passed")); err == nil {
			fw.Passed = n
		}
		if strings.Contains(line, "failed") {
			if n, err := strconv.Atoi(extractBefore(line, " failed")); err == nil {
				fw.Failed = n
			}
		}
		fw.Total = fw.Passed + fw.Failed
		if fw.Total > 0 {
			return fw, true
		}
	}
	return testFramework{}, false
}

func detectGoTest(s string) (testFramework, bool) {
	fw := testFramework{Name: "go test"}
	found := false
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "--- PASS:") {
			fw.Passed++
			found = true
		} else if strings.HasPrefix(line, "--- FAIL:") {
			fw.Failed++
			found = true
		}
	}
	if found {
		fw.Total = fw.Passed + fw.Failed
		return fw, true
	}
	return testFramework{}, false
}

func detectJest(s string) (testFramework, bool) {
	// "Tests:       5 passed, 1 failed, 6 total"
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Tests:") {
			continue
		}
		fw := testFramework{Name: "jest/vitest"}
		if strings.Contains(line, "passed") {
			if n, err := strconv.Atoi(extractBefore(line, " passed")); err == nil {
				fw.Passed = n
			}
		}
		if strings.Contains(line, "failed") {
			if n, err := strconv.Atoi(extractBefore(line, " failed")); err == nil {
				fw.Failed = n
			}
		}
		if strings.Contains(line, "total") {
			if n, err := strconv.Atoi(extractBefore(line, " total")); err == nil {
				fw.Total = n
			}
		}
		if fw.Total > 0 || fw.Passed > 0 || fw.Failed > 0 {
			return fw, true
		}
	}
	return testFramework{}, false
}

func detectPHPUnit(s string) (testFramework, bool) {
	// "OK (5 tests, 10 assertions)" or "Tests: 5, Assertions: 10, Failures: 2"
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "OK (") && strings.Contains(line, "tests") {
			fw := testFramework{Name: "phpunit"}
			// "OK (5 tests, ..." — extract number after "OK ("
			if n, err := strconv.Atoi(extractBefore(strings.TrimPrefix(line, "OK ("), " tests")); err == nil {
				fw.Total = n
			}
			fw.Passed = fw.Total
			if fw.Total > 0 {
				return fw, true
			}
		}
		if strings.Contains(line, "FAILURES!") || strings.Contains(line, "Tests:") && strings.Contains(line, "Failures:") {
			fw := testFramework{Name: "phpunit"}
			if n, err := strconv.Atoi(extractBefore(extractAfter(line, "Tests: "), ",")); err == nil {
				fw.Total = n
			}
			if n, err := strconv.Atoi(extractBefore(extractAfter(line, "Failures: "), ",")); err == nil {
				fw.Failed = n
			}
			fw.Passed = max(0, fw.Total-fw.Failed)
			if fw.Total > 0 {
				return fw, true
			}
		}
	}
	return testFramework{}, false
}

func detectRSpec(s string) (testFramework, bool) {
	// "5 examples, 0 failures"
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "examples") || !strings.Contains(line, "failures") {
			continue
		}
		fw := testFramework{Name: "rspec"}
		if n, err := strconv.Atoi(extractBefore(line, " examples")); err == nil {
			fw.Total = n
		}
		if n, err := strconv.Atoi(extractBefore(line, " failures")); err == nil {
			fw.Failed = n
		}
		fw.Passed = max(0, fw.Total-fw.Failed)
		if fw.Total > 0 {
			return fw, true
		}
	}
	return testFramework{}, false
}

func extractBefore(s, sep string) string {
	idx := strings.LastIndex(s, sep)
	if idx < 0 {
		return ""
	}
	// walk back past spaces to find the number
	i := idx - 1
	for i >= 0 && s[i] == ' ' {
		i--
	}
	j := i
	for j >= 0 && (s[j] >= '0' && s[j] <= '9') {
		j--
	}
	return s[j+1 : i+1]
}

func extractAfter(s, sep string) string {
	idx := strings.Index(s, sep)
	if idx < 0 {
		return ""
	}
	return s[idx+len(sep):]
}
