package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// realShtraceBinary builds shtrace once per test run. The shims must point at a
// genuine binary: under `go test` os.Executable() is the test binary, and a
// shim pointing there would re-run the whole suite instead of wrapping.
var realShtraceBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "shtrace-bin-*")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "shtrace")
	out, err := exec.Command("go", "build", "-o", bin, "github.com/harakeishi/shtrace/cmd/shtrace").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build shtrace: %v: %s", err, out)
	}
	return bin, nil
})

// autowrapHarness gives each test its own HOME so enable/disable never touch
// the developer's real rc files.
func autowrapHarness(t *testing.T) (home string, dataDir string) {
	t.Helper()
	home = t.TempDir()
	dataDir = t.TempDir()
	bin, err := realShtraceBinary()
	if err != nil {
		t.Fatalf("build shtrace binary: %v", err)
	}
	t.Setenv("SHTRACE_SELF_PATH", bin)
	t.Setenv("HOME", home)
	t.Setenv("SHTRACE_DATA_DIR", dataDir)
	t.Setenv("SHTRACE_SESSION_ID", "")
	t.Setenv("SHTRACE_PARENT_SPAN_ID", "")
	t.Setenv("SHTRACE_TAGS", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("GITHUB_WORKSPACE", "")
	return home, dataDir
}

func runCLI(t *testing.T, args ...string) (stdout, stderr string, exit int) {
	t.Helper()
	var so, se bytes.Buffer
	exit = Run(context.Background(), args, &so, &se)
	return so.String(), se.String(), exit
}

func TestInjectBlock_AddsMarkedBlockToEmptyContent(t *testing.T) {
	got := injectBlock("", "export FOO=1")
	if !strings.Contains(got, blockBegin) || !strings.Contains(got, blockEnd) {
		t.Fatalf("injected content missing markers: %q", got)
	}
	if !strings.Contains(got, "export FOO=1") {
		t.Fatalf("injected content missing body: %q", got)
	}
}

func TestInjectBlock_IsIdempotent(t *testing.T) {
	once := injectBlock("# user content\n", "export FOO=1")
	twice := injectBlock(once, "export FOO=1")
	if once != twice {
		t.Fatalf("injectBlock not idempotent:\nonce=%q\ntwice=%q", once, twice)
	}
	if strings.Count(twice, blockBegin) != 1 {
		t.Fatalf("expected exactly one block, got %d", strings.Count(twice, blockBegin))
	}
}

func TestInjectBlock_ReplacesExistingBlockAndPreservesUserContent(t *testing.T) {
	orig := "# before\n" + injectBlock("", "export OLD=1") + "# after\n"
	updated := injectBlock(orig, "export NEW=2")
	if strings.Contains(updated, "export OLD=1") {
		t.Fatalf("old block body survived replacement: %q", updated)
	}
	if !strings.Contains(updated, "export NEW=2") {
		t.Fatalf("new block body missing: %q", updated)
	}
	if !strings.Contains(updated, "# before") || !strings.Contains(updated, "# after") {
		t.Fatalf("user content was modified: %q", updated)
	}
	if strings.Count(updated, blockBegin) != 1 {
		t.Fatalf("expected exactly one block, got %d", strings.Count(updated, blockBegin))
	}
}

func TestRemoveBlock_RemovesOnlyTheBlock(t *testing.T) {
	orig := "# before\n" + injectBlock("", "export FOO=1") + "# after\n"
	got, removed := removeBlock(orig)
	if !removed {
		t.Fatal("removeBlock reported no removal")
	}
	if strings.Contains(got, blockBegin) || strings.Contains(got, "export FOO=1") {
		t.Fatalf("block survived removal: %q", got)
	}
	if !strings.Contains(got, "# before") || !strings.Contains(got, "# after") {
		t.Fatalf("user content was lost: %q", got)
	}
}

func TestRemoveBlock_NoBlockIsNoOp(t *testing.T) {
	const orig = "# just user content\n"
	got, removed := removeBlock(orig)
	if removed {
		t.Fatal("removeBlock reported removal on content without a block")
	}
	if got != orig {
		t.Fatalf("content changed: %q", got)
	}
}

func TestResolveRealShell_SkipsShimDirAndFindsRealBinary(t *testing.T) {
	shimDir := t.TempDir()
	realDir := t.TempDir()

	// A decoy shim that must never be selected as "the real shell".
	writeExecutable(t, filepath.Join(shimDir, "bash"), "#!/bin/sh\nexit 0\n")
	realBash := filepath.Join(realDir, "bash")
	writeExecutable(t, realBash, "#!/bin/sh\nexit 0\n")

	pathEnv := strings.Join([]string{shimDir, realDir}, string(os.PathListSeparator))
	got := resolveRealShell("bash", pathEnv, shimDir)
	if got != realBash {
		t.Fatalf("resolveRealShell = %q, want %q", got, realBash)
	}
}

func TestResolveRealShell_FallsBackWhenNotFound(t *testing.T) {
	shimDir := t.TempDir()
	got := resolveRealShell("bash", t.TempDir(), shimDir)
	if got != "/bin/bash" {
		t.Fatalf("resolveRealShell fallback = %q, want /bin/bash", got)
	}
}

func TestShimScript_ContainsGuardsAndQuotesPaths(t *testing.T) {
	script := shimScript("/opt/my tools/shtrace", "/bin/bash")

	// Recursion guard: an already-traced process must exec the real shell.
	if !strings.Contains(script, "SHTRACE_SESSION_ID") {
		t.Fatalf("shim missing recursion guard: %q", script)
	}
	// Interactive shells are out of scope for v1.
	if !strings.Contains(script, "-i") {
		t.Fatalf("shim missing interactive passthrough: %q", script)
	}
	// Paths with spaces must be single-quoted so the shim does not word-split.
	if !strings.Contains(script, "'/opt/my tools/shtrace'") {
		t.Fatalf("shim did not quote the shtrace path: %q", script)
	}
	if !strings.Contains(script, "'/bin/bash'") {
		t.Fatalf("shim did not quote the real shell path: %q", script)
	}
}

func TestEnable_CreatesShimsAndInjectsRCBlocks(t *testing.T) {
	home, _ := autowrapHarness(t)

	stdout, stderr, exit := runCLI(t, "shtrace", "enable")
	if exit != 0 {
		t.Fatalf("enable exit = %d, stderr=%s", exit, stderr)
	}

	for _, name := range []string{"bash", "zsh", "sh"} {
		p := filepath.Join(home, ".shtrace", "shims", name)
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("shim %s missing: %v", name, err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("shim %s is not executable: %v", name, info.Mode())
		}
	}

	bashEnv := filepath.Join(home, ".shtrace", "bashenv.sh")
	body := readFile(t, bashEnv)
	if !strings.Contains(body, "BASH_EXECUTION_STRING") {
		t.Fatalf("bashenv.sh missing execution-string guard: %q", body)
	}

	bashrc := readFile(t, filepath.Join(home, ".bashrc"))
	if !strings.Contains(bashrc, blockBegin) {
		t.Fatalf(".bashrc missing shtrace block: %q", bashrc)
	}
	if !strings.Contains(bashrc, "BASH_ENV") {
		t.Fatalf(".bashrc block should set BASH_ENV: %q", bashrc)
	}

	zshenv := readFile(t, filepath.Join(home, ".zshenv"))
	if !strings.Contains(zshenv, "ZSH_EXECUTION_STRING") {
		t.Fatalf(".zshenv missing execution-string guard: %q", zshenv)
	}

	if !strings.Contains(stdout, "shtrace disable") {
		t.Fatalf("enable output should explain how to disable, got %q", stdout)
	}
	if !strings.Contains(stdout, "shtrace gc") {
		t.Fatalf("enable output should mention disk management, got %q", stdout)
	}
}

func TestEnable_IsIdempotentAndPreservesUserRCContent(t *testing.T) {
	home, _ := autowrapHarness(t)

	bashrcPath := filepath.Join(home, ".bashrc")
	if err := os.WriteFile(bashrcPath, []byte("export USER_SETTING=1\n"), 0o644); err != nil {
		t.Fatalf("seed .bashrc: %v", err)
	}

	if _, se, exit := runCLI(t, "shtrace", "enable"); exit != 0 {
		t.Fatalf("first enable exit = %d: %s", exit, se)
	}
	first := readFile(t, bashrcPath)
	if _, se, exit := runCLI(t, "shtrace", "enable"); exit != 0 {
		t.Fatalf("second enable exit = %d: %s", exit, se)
	}
	second := readFile(t, bashrcPath)

	if first != second {
		t.Fatalf("enable is not idempotent:\nfirst=%q\nsecond=%q", first, second)
	}
	if strings.Count(second, blockBegin) != 1 {
		t.Fatalf("expected one block after two enables, got %d", strings.Count(second, blockBegin))
	}
	if !strings.Contains(second, "export USER_SETTING=1") {
		t.Fatalf("user rc content was lost: %q", second)
	}
}

func TestDisable_RemovesBlocksAndShims(t *testing.T) {
	home, _ := autowrapHarness(t)

	bashrcPath := filepath.Join(home, ".bashrc")
	if err := os.WriteFile(bashrcPath, []byte("export USER_SETTING=1\n"), 0o644); err != nil {
		t.Fatalf("seed .bashrc: %v", err)
	}
	if _, se, exit := runCLI(t, "shtrace", "enable"); exit != 0 {
		t.Fatalf("enable exit = %d: %s", exit, se)
	}

	if _, se, exit := runCLI(t, "shtrace", "disable"); exit != 0 {
		t.Fatalf("disable exit = %d: %s", exit, se)
	}

	bashrc := readFile(t, bashrcPath)
	if strings.Contains(bashrc, blockBegin) {
		t.Fatalf(".bashrc block survived disable: %q", bashrc)
	}
	if !strings.Contains(bashrc, "export USER_SETTING=1") {
		t.Fatalf("user rc content was lost: %q", bashrc)
	}

	if _, err := os.Stat(filepath.Join(home, ".shtrace", "shims")); !os.IsNotExist(err) {
		t.Fatalf("shim dir survived disable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".shtrace", "bashenv.sh")); !os.IsNotExist(err) {
		t.Fatalf("bashenv.sh survived disable: %v", err)
	}
}

func TestDisable_WithoutEnableIsNoOp(t *testing.T) {
	autowrapHarness(t)
	_, stderr, exit := runCLI(t, "shtrace", "disable")
	if exit != 0 {
		t.Fatalf("disable exit = %d, stderr=%s", exit, stderr)
	}
}

func TestDoctor_ReportsNotEnabledBeforeEnable(t *testing.T) {
	autowrapHarness(t)
	stdout, _, exit := runCLI(t, "shtrace", "doctor")
	if exit != 0 {
		t.Fatalf("doctor exit = %d", exit)
	}
	if !strings.Contains(stdout, "NG") {
		t.Fatalf("doctor should report NG before enable, got %q", stdout)
	}
}

func TestDoctor_ReportsShimDirAndBlocksAfterEnable(t *testing.T) {
	home, _ := autowrapHarness(t)
	if _, se, exit := runCLI(t, "shtrace", "enable"); exit != 0 {
		t.Fatalf("enable exit = %d: %s", exit, se)
	}

	stdout, _, exit := runCLI(t, "shtrace", "doctor")
	if exit != 0 {
		t.Fatalf("doctor exit = %d", exit)
	}
	shimDir := filepath.Join(home, ".shtrace", "shims")
	if !strings.Contains(stdout, shimDir) {
		t.Fatalf("doctor should report the shim dir path, got %q", stdout)
	}
	if !strings.Contains(stdout, ".bashrc") || !strings.Contains(stdout, ".zshenv") {
		t.Fatalf("doctor should report rc file status, got %q", stdout)
	}
}

// The probe actually runs `bash -c true` through the shim and checks that a new
// session lands in the store, so a shim that silently fails to wrap is caught.
func TestDoctor_ProbeRecordsSessionThroughShim(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("bash not available")
	}
	_, dataDir := autowrapHarness(t)
	if _, se, exit := runCLI(t, "shtrace", "enable"); exit != 0 {
		t.Fatalf("enable exit = %d: %s", exit, se)
	}

	stdout, stderr, exit := runCLI(t, "shtrace", "doctor")
	if exit != 0 {
		t.Fatalf("doctor exit = %d, stderr=%s", exit, stderr)
	}
	if !strings.Contains(stdout, "probe") {
		t.Fatalf("doctor should report a probe result, got %q", stdout)
	}
	if strings.Contains(stdout, "probe: NG") {
		t.Fatalf("probe failed:\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "sessions.db")); err != nil {
		t.Fatalf("probe did not record a session: %v", err)
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
