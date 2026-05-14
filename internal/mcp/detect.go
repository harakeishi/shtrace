package mcp

import (
	"bufio"
	"encoding/json"
	"os"
	"regexp"
	"strings"

	"github.com/harakeishi/shtrace/internal/storage"
)

// TestRun summarises one detected test-framework execution within a span.
type TestRun struct {
	SpanID    string `json:"span_id"`
	SessionID string `json:"session_id"`
	Framework string `json:"framework"`
	Command   string `json:"command"`
	Passed    *int   `json:"passed,omitempty"`
	Failed    *int   `json:"failed,omitempty"`
	Skipped   *int   `json:"skipped,omitempty"`
	Total     *int   `json:"total,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Summary   string `json:"summary,omitempty"`
}

// framework detector bundles a recogniser for the command name and a function
// that extracts a TestRun from the last lines of recorded output.
type frameworkDetector struct {
	name    string
	cmdRe   *regexp.Regexp
	extract func(lines []string, sp storage.Span) TestRun
}

var detectors = []frameworkDetector{
	{
		name:  "pytest",
		cmdRe: regexp.MustCompile(`(?i)\bpytest\b`),
		extract: func(lines []string, sp storage.Span) TestRun {
			// e.g. "5 passed, 1 failed, 2 warnings in 0.12s"
			re := regexp.MustCompile(`(?i)(\d+)\s+passed(?:,\s*(\d+)\s+failed)?(?:,\s*(\d+)\s+(?:skipped|warning))?`)
			for i := len(lines) - 1; i >= 0 && i >= len(lines)-30; i-- {
				m := re.FindStringSubmatch(lines[i])
				if m == nil {
					continue
				}
				run := newRun(sp, "pytest")
				run.Summary = strings.TrimSpace(lines[i])
				p := atoi(m[1])
				run.Passed = &p
				if m[2] != "" {
					f := atoi(m[2])
					run.Failed = &f
				}
				return run
			}
			return newRun(sp, "pytest")
		},
	},
	{
		name:  "jest",
		cmdRe: regexp.MustCompile(`(?i)\bjest\b`),
		extract: func(lines []string, sp storage.Span) TestRun {
			// e.g. "Tests:       2 passed, 3 total"
			re := regexp.MustCompile(`(?i)Tests:\s+(?:(\d+)\s+failed,\s*)?(?:(\d+)\s+skipped,\s*)?(\d+)\s+passed(?:,\s*(\d+)\s+total)?`)
			for i := len(lines) - 1; i >= 0 && i >= len(lines)-30; i-- {
				m := re.FindStringSubmatch(lines[i])
				if m == nil {
					continue
				}
				run := newRun(sp, "jest")
				run.Summary = strings.TrimSpace(lines[i])
				if m[1] != "" {
					f := atoi(m[1])
					run.Failed = &f
				}
				if m[2] != "" {
					sk := atoi(m[2])
					run.Skipped = &sk
				}
				p := atoi(m[3])
				run.Passed = &p
				if m[4] != "" {
					t := atoi(m[4])
					run.Total = &t
				}
				return run
			}
			return newRun(sp, "jest")
		},
	},
	{
		name:  "vitest",
		cmdRe: regexp.MustCompile(`(?i)\bvitest\b`),
		extract: func(lines []string, sp storage.Span) TestRun {
			// e.g. "Tests  2 passed | 1 failed (3)"
			re := regexp.MustCompile(`(?i)Tests\s+(\d+)\s+passed(?:\s+\|\s+(\d+)\s+failed)?(?:\s+\((\d+)\))?`)
			for i := len(lines) - 1; i >= 0 && i >= len(lines)-30; i-- {
				m := re.FindStringSubmatch(lines[i])
				if m == nil {
					continue
				}
				run := newRun(sp, "vitest")
				run.Summary = strings.TrimSpace(lines[i])
				p := atoi(m[1])
				run.Passed = &p
				if m[2] != "" {
					f := atoi(m[2])
					run.Failed = &f
				}
				if m[3] != "" {
					t := atoi(m[3])
					run.Total = &t
				}
				return run
			}
			return newRun(sp, "vitest")
		},
	},
	{
		name:  "phpunit",
		cmdRe: regexp.MustCompile(`(?i)\bphpunit\b`),
		extract: func(lines []string, sp storage.Span) TestRun {
			// e.g. "OK (5 tests, 12 assertions)" or "Tests: 3, Assertions: 6, Failures: 1."
			reOK := regexp.MustCompile(`(?i)OK\s+\((\d+)\s+tests?`)
			reFail := regexp.MustCompile(`(?i)Tests:\s*(\d+).*?Failures:\s*(\d+)`)
			for i := len(lines) - 1; i >= 0 && i >= len(lines)-30; i-- {
				if m := reOK.FindStringSubmatch(lines[i]); m != nil {
					run := newRun(sp, "phpunit")
					run.Summary = strings.TrimSpace(lines[i])
					t := atoi(m[1])
					run.Total = &t
					run.Passed = &t
					return run
				}
				if m := reFail.FindStringSubmatch(lines[i]); m != nil {
					run := newRun(sp, "phpunit")
					run.Summary = strings.TrimSpace(lines[i])
					t := atoi(m[1])
					f := atoi(m[2])
					p := t - f
					run.Total = &t
					run.Failed = &f
					run.Passed = &p
					return run
				}
			}
			return newRun(sp, "phpunit")
		},
	},
	{
		name:  "go test",
		cmdRe: regexp.MustCompile(`(?i)\bgo\b`),
		extract: func(lines []string, sp storage.Span) TestRun {
			// Only match if argv contains "test"
			hasTestArg := false
			for _, a := range sp.Argv {
				if a == "test" {
					hasTestArg = true
					break
				}
			}
			if !hasTestArg {
				return TestRun{} // signal no match
			}
			// Count "--- PASS" and "--- FAIL" lines
			passed, failed := 0, 0
			var summary string
			reOK := regexp.MustCompile(`^ok\s+`)
			reFail := regexp.MustCompile(`^FAIL\s+`)
			for _, l := range lines {
				switch {
				case strings.HasPrefix(l, "--- PASS"):
					passed++
				case strings.HasPrefix(l, "--- FAIL"):
					failed++
				case reOK.MatchString(l) || reFail.MatchString(l):
					summary = strings.TrimSpace(l)
				}
			}
			run := newRun(sp, "go test")
			run.Passed = &passed
			run.Failed = &failed
			t := passed + failed
			run.Total = &t
			run.Summary = summary
			return run
		},
	},
	{
		name:  "rspec",
		cmdRe: regexp.MustCompile(`(?i)\brspec\b`),
		extract: func(lines []string, sp storage.Span) TestRun {
			// e.g. "5 examples, 1 failure"
			re := regexp.MustCompile(`(?i)(\d+)\s+examples?,\s*(\d+)\s+failures?`)
			for i := len(lines) - 1; i >= 0 && i >= len(lines)-30; i-- {
				m := re.FindStringSubmatch(lines[i])
				if m == nil {
					continue
				}
				run := newRun(sp, "rspec")
				run.Summary = strings.TrimSpace(lines[i])
				t := atoi(m[1])
				f := atoi(m[2])
				p := t - f
				run.Total = &t
				run.Failed = &f
				run.Passed = &p
				return run
			}
			return newRun(sp, "rspec")
		},
	},
}

func newRun(sp storage.Span, framework string) TestRun {
	cmd := sp.Command
	if len(sp.Argv) > 0 {
		cmd = strings.Join(sp.Argv, " ")
	}
	return TestRun{
		SpanID:    sp.ID,
		SessionID: sp.SessionID,
		Framework: framework,
		Command:   cmd,
		ExitCode:  sp.ExitCode,
	}
}

// DetectTestRuns inspects the spans of a session and returns any detected
// test-framework executions with pass/fail counts.
func DetectTestRuns(spans []storage.Span, dataDir string) []TestRun {
	var out []TestRun
	for _, sp := range spans {
		run := detectSpan(sp, dataDir)
		if run != nil {
			out = append(out, *run)
		}
	}
	return out
}

func detectSpan(sp storage.Span, dataDir string) *TestRun {
	cmd := sp.Command
	for _, det := range detectors {
		if !det.cmdRe.MatchString(cmd) {
			continue
		}
		lines := readOutputLines(storage.OutputPath(dataDir, sp.SessionID, sp.ID))
		run := det.extract(lines, sp)
		if run.SpanID == "" {
			// go test detector signals no match with empty SpanID
			continue
		}
		return &run
	}
	return nil
}

func readOutputLines(logPath string) []string {
	f, err := os.Open(logPath)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 10<<20)
	for sc.Scan() {
		var c storage.Chunk
		if json.Unmarshal(sc.Bytes(), &c) == nil {
			for _, l := range strings.Split(c.Data, "\n") {
				lines = append(lines, l)
			}
		}
	}
	return lines
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			n = n*10 + int(r-'0')
		}
	}
	return n
}
