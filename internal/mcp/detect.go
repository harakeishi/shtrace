package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// Pre-compiled regexes for each framework's output-summary line.
// Compiled once at package init to avoid repeated compilation per span.
var (
	pytestCmdRe = regexp.MustCompile(`(?i)\bpytest\b`)
	// pytestResultRe matches both orderings of pytest's summary line:
	//   "N passed, N failed in Zs"  (pass-first, no failures or minor failures)
	//   "N failed, N passed in Zs"  (fail-first, when failures exist)
	// Group 1: failed count (fail-first branch)
	// Group 2: passed count (fail-first branch)
	// Group 3: passed count (pass-first branch)
	// Group 4: failed count (pass-first branch)
	// Group 5: skipped count (either branch)
	pytestResultRe = regexp.MustCompile(
		`(?i)(?:(\d+)\s+failed,\s*(\d+)\s+passed|(\d+)\s+passed(?:,\s*(\d+)\s+failed)?)(?:,\s*(\d+)\s+skipped)?`)

	jestCmdRe    = regexp.MustCompile(`(?i)\bjest\b`)
	// jestResultRe matches Jest's "Tests:" summary line.
	// Jest v29+ may include a "todo" count before "passed"; it is matched but
	// not captured (non-capturing group) so the capture group indices are stable:
	//   m[1]=failed  m[2]=skipped  m[3]=passed  m[4]=total
	jestResultRe = regexp.MustCompile(`(?i)Tests:\s+(?:(\d+)\s+failed,\s*)?(?:(\d+)\s+skipped,\s*)?(?:\d+\s+todo,\s*)?(\d+)\s+passed(?:,\s*(\d+)\s+total)?`)

	vitestCmdRe = regexp.MustCompile(`(?i)\bvitest\b`)
	// vitest changed its output format between versions:
	//   v0.x (pass-first): "Tests  4 passed | 1 failed (5)"
	//   v1.x (fail-first): "Tests  1 failed | 4 passed (5)"
	// Two separate patterns are used; fail-first is tried first.
	vitestFailFirstRe = regexp.MustCompile(`(?i)Tests\s+(\d+)\s+failed\s*\|\s*(\d+)\s+passed(?:\s+\((\d+)\))?`)
	vitestPassFirstRe = regexp.MustCompile(`(?i)Tests\s+(\d+)\s+passed(?:\s*\|\s*(\d+)\s+failed)?(?:\s+\((\d+)\))?`)

	phpunitCmdRe     = regexp.MustCompile(`(?i)\bphpunit\b`)
	phpunitOKRe      = regexp.MustCompile(`(?i)OK\s+\((\d+)\s+tests?`)
	phpunitFailRe    = regexp.MustCompile(`(?i)Tests:\s*(\d+).*?Failures:\s*(\d+)`)

	goTestCmdRe  = regexp.MustCompile(`(?i)\bgo\b`)
	goTestOKRe   = regexp.MustCompile(`^ok\s+`)
	goTestFailRe = regexp.MustCompile(`^FAIL\s+`)

	rspecCmdRe    = regexp.MustCompile(`(?i)\brspec\b`)
	rspecResultRe = regexp.MustCompile(`(?i)(\d+)\s+examples?,\s*(\d+)\s+failures?`)
)

// frameworkDetector bundles a recogniser for the command name and a function
// that extracts a TestRun from the last lines of recorded output.
type frameworkDetector struct {
	name    string
	cmdRe   *regexp.Regexp
	extract func(lines []string, sp storage.Span) TestRun
}

var detectors = []frameworkDetector{
	{
		name:  "pytest",
		cmdRe: pytestCmdRe,
		extract: func(lines []string, sp storage.Span) TestRun {
			for i := len(lines) - 1; i >= 0 && i >= len(lines)-30; i-- {
				m := pytestResultRe.FindStringSubmatch(lines[i])
				if m == nil {
					continue
				}
				run := newRun(sp, "pytest")
				run.Summary = strings.TrimSpace(lines[i])
				// Groups 1,2: fail-first  "N failed, N passed"
				// Groups 3,4: pass-first  "N passed[, N failed]"
				// Group 5: skipped (either branch)
				if m[1] != "" {
					// fail-first branch
					f := atoi(m[1])
					p := atoi(m[2])
					run.Failed = &f
					run.Passed = &p
				} else {
					// pass-first branch
					p := atoi(m[3])
					run.Passed = &p
					if m[4] != "" {
						f := atoi(m[4])
						run.Failed = &f
					}
				}
				if m[5] != "" {
					sk := atoi(m[5])
					run.Skipped = &sk
				}
				return run
			}
			return newRun(sp, "pytest")
		},
	},
	{
		name:  "jest",
		cmdRe: jestCmdRe,
		extract: func(lines []string, sp storage.Span) TestRun {
			for i := len(lines) - 1; i >= 0 && i >= len(lines)-30; i-- {
				m := jestResultRe.FindStringSubmatch(lines[i])
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
		cmdRe: vitestCmdRe,
		extract: func(lines []string, sp storage.Span) TestRun {
			for i := len(lines) - 1; i >= 0 && i >= len(lines)-30; i-- {
				// Try fail-first (vitest v1+) before pass-first (v0.x).
				if m := vitestFailFirstRe.FindStringSubmatch(lines[i]); m != nil {
					// m[1]=failed  m[2]=passed  m[3]=total
					run := newRun(sp, "vitest")
					run.Summary = strings.TrimSpace(lines[i])
					f := atoi(m[1])
					p := atoi(m[2])
					run.Failed = &f
					run.Passed = &p
					if m[3] != "" {
						t := atoi(m[3])
						run.Total = &t
					}
					return run
				}
				if m := vitestPassFirstRe.FindStringSubmatch(lines[i]); m != nil {
					// m[1]=passed  m[2]=failed (opt)  m[3]=total (opt)
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
			}
			return newRun(sp, "vitest")
		},
	},
	{
		name:  "phpunit",
		cmdRe: phpunitCmdRe,
		extract: func(lines []string, sp storage.Span) TestRun {
			for i := len(lines) - 1; i >= 0 && i >= len(lines)-30; i-- {
				if m := phpunitOKRe.FindStringSubmatch(lines[i]); m != nil {
					run := newRun(sp, "phpunit")
					run.Summary = strings.TrimSpace(lines[i])
					t := atoi(m[1])
					run.Total = &t
					run.Passed = &t
					return run
				}
				if m := phpunitFailRe.FindStringSubmatch(lines[i]); m != nil {
					run := newRun(sp, "phpunit")
					run.Summary = strings.TrimSpace(lines[i])
					t := atoi(m[1])
					f := atoi(m[2])
					p := max0(t - f)
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
		cmdRe: goTestCmdRe,
		extract: func(lines []string, sp storage.Span) TestRun {
			hasTestArg := false
			for _, a := range sp.Argv {
				if a == "test" {
					hasTestArg = true
					break
				}
			}
			if !hasTestArg {
				return TestRun{} // signal no match via empty SpanID
			}
			passed, failed := 0, 0
			var summary string
			for _, l := range lines {
				switch {
				case strings.HasPrefix(l, "--- PASS"):
					passed++
				case strings.HasPrefix(l, "--- FAIL"):
					failed++
				case goTestOKRe.MatchString(l) || goTestFailRe.MatchString(l):
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
		cmdRe: rspecCmdRe,
		extract: func(lines []string, sp storage.Span) TestRun {
			for i := len(lines) - 1; i >= 0 && i >= len(lines)-30; i-- {
				m := rspecResultRe.FindStringSubmatch(lines[i])
				if m == nil {
					continue
				}
				run := newRun(sp, "rspec")
				run.Summary = strings.TrimSpace(lines[i])
				t := atoi(m[1])
				f := atoi(m[2])
				p := max0(t - f)
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
		logPath, err := safeOutputPath(dataDir, sp.SessionID, sp.ID)
		if err != nil {
			continue
		}
		lines := readOutputLines(logPath)
		run := det.extract(lines, sp)
		if run.SpanID == "" {
			// go test detector signals no match via empty SpanID
			continue
		}
		return &run
	}
	return nil
}

// safeOutputPath returns the log file path and verifies it stays within
// dataDir to guard against path traversal via malicious DB values.
func safeOutputPath(dataDir, sessionID, spanID string) (string, error) {
	p := storage.OutputPath(dataDir, sessionID, spanID)
	// filepath.Join already cleans the path; verify the result is still
	// inside dataDir so that "../" components in sessionID/spanID cannot
	// escape the data directory.
	rel, err := filepath.Rel(dataDir, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("unsafe output path for session=%q span=%q", sessionID, spanID)
	}
	return p, nil
}

// maxOutputLines is the maximum number of output lines retained by
// readOutputLines. All framework detectors only inspect the last 30 lines;
// keeping 200 provides a comfortable safety margin while bounding memory use
// even for very large build logs.
const maxOutputLines = 200

func readOutputLines(logPath string) []string {
	f, err := os.Open(logPath)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var lines []string
	sc := bufio.NewScanner(f)
	// 64 KB initial, 1 MB max per JSON line.
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var c storage.Chunk
		if json.Unmarshal(sc.Bytes(), &c) == nil {
			lines = append(lines, strings.Split(c.Data, "\n")...)
		}
		// Cap in-loop to bound peak memory for very large logs.
		// We keep 2× maxOutputLines as a flush threshold so we don't
		// slice on every iteration; the final trim below enforces the exact cap.
		if len(lines) > maxOutputLines*2 {
			lines = lines[len(lines)-maxOutputLines:]
		}
	}
	// sc.Err() is non-nil when scanning stopped due to an I/O error or a line
	// exceeding the 1 MB buffer limit. Return whatever lines were collected so
	// far (best-effort): detectors may still find a summary if the error
	// occurred near the end of a large log.
	// Errors are intentionally not propagated — detection is advisory and a
	// partial read is more useful than no result.
	_ = sc.Err()

	// Final trim to the exact tail the detectors need.
	if len(lines) > maxOutputLines {
		lines = lines[len(lines)-maxOutputLines:]
	}
	return lines
}

// max0 returns n if n >= 0, otherwise 0. Guards passed = total - failed when
// a corrupt or malformed output line produces failed > total.
func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
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
