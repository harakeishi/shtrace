package storage

import (
	"testing"
)

func TestResolveDataDir_PrefersSHTRACEDataDir(t *testing.T) {
	env := map[string]string{
		"SHTRACE_DATA_DIR":   "/tmp/explicit-shtrace",
		"XDG_DATA_HOME":      "/tmp/xdg",
		"HOME":               "/home/u",
		"GITHUB_WORKSPACE":   "/runner/work/repo",
	}

	got, err := ResolveDataDir(env, "linux")
	if err != nil {
		t.Fatalf("ResolveDataDir: %v", err)
	}
	if got != "/tmp/explicit-shtrace" {
		t.Fatalf("ResolveDataDir = %q, want explicit override", got)
	}
}

func TestResolveDataDir_UsesXDGOnLinux(t *testing.T) {
	env := map[string]string{
		"XDG_DATA_HOME": "/tmp/xdg",
		"HOME":          "/home/u",
	}

	got, err := ResolveDataDir(env, "linux")
	if err != nil {
		t.Fatalf("ResolveDataDir: %v", err)
	}
	if got != "/tmp/xdg/shtrace" {
		t.Fatalf("ResolveDataDir = %q, want /tmp/xdg/shtrace", got)
	}
}

func TestResolveDataDir_FallsBackToHomeOnLinux(t *testing.T) {
	env := map[string]string{
		"HOME": "/home/u",
	}

	got, err := ResolveDataDir(env, "linux")
	if err != nil {
		t.Fatalf("ResolveDataDir: %v", err)
	}
	if got != "/home/u/.local/share/shtrace" {
		t.Fatalf("ResolveDataDir = %q, want ~/.local/share/shtrace", got)
	}
}

func TestResolveDataDir_MacOS(t *testing.T) {
	env := map[string]string{
		"HOME": "/Users/u",
	}

	got, err := ResolveDataDir(env, "darwin")
	if err != nil {
		t.Fatalf("ResolveDataDir: %v", err)
	}
	if got != "/Users/u/Library/Application Support/shtrace" {
		t.Fatalf("ResolveDataDir = %q, want macOS Application Support path", got)
	}
}

func TestResolveDataDir_GitHubActionsAutoDetect(t *testing.T) {
	env := map[string]string{
		"GITHUB_WORKSPACE": "/runner/work/repo",
		"GITHUB_ACTIONS":   "true",
		"HOME":             "/home/runner",
	}

	got, err := ResolveDataDir(env, "linux")
	if err != nil {
		t.Fatalf("ResolveDataDir: %v", err)
	}
	if got != "/runner/work/repo/.shtrace" {
		t.Fatalf("ResolveDataDir = %q, want GitHub Actions workspace override", got)
	}
}

func TestResolveDataDir_GitHubActionsRespectsExplicitOverride(t *testing.T) {
	env := map[string]string{
		"SHTRACE_DATA_DIR": "/tmp/explicit",
		"GITHUB_WORKSPACE": "/runner/work/repo",
		"GITHUB_ACTIONS":   "true",
	}

	got, err := ResolveDataDir(env, "linux")
	if err != nil {
		t.Fatalf("ResolveDataDir: %v", err)
	}
	if got != "/tmp/explicit" {
		t.Fatalf("ResolveDataDir = %q, want explicit override to win", got)
	}
}

func TestResolveDataDir_NoHome_ReturnsError(t *testing.T) {
	if _, err := ResolveDataDir(map[string]string{}, "linux"); err == nil {
		t.Fatalf("ResolveDataDir: expected error when HOME and SHTRACE_DATA_DIR are missing")
	}
}
