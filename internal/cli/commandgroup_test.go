package cli

import "testing"

func TestCommandGroup(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{nil, ""},
		{[]string{}, ""},
		{[]string{"pytest"}, "pytest"},
		{[]string{"pytest", "-q", "tests/"}, "pytest"},
		{[]string{"pytest", "tests/x", "-x"}, "pytest"},
		{[]string{"go", "test", "./...", "-race"}, "go test"},
		{[]string{"go", "build", "./..."}, "go build"},
		{[]string{"go"}, "go"},
		{[]string{"go", "-h"}, "go"},
		{[]string{"docker", "build", "-t", "x", "."}, "docker build"},
		{[]string{"git", "push", "origin", "main"}, "git push"},
		{[]string{"npm", "run", "build"}, "npm run"},
		{[]string{"make", "ci"}, "make ci"},
		{[]string{"make"}, "make"},
		{[]string{"cargo", "test"}, "cargo test"},
		{[]string{"yarn", "install"}, "yarn install"},
		{[]string{"pnpm", "-w", "build"}, "pnpm build"},
		{[]string{"terraform", "plan", "-out=tfplan"}, "terraform plan"},
		{[]string{"kubectl", "rollout", "status"}, "kubectl rollout"},
		// leading flags are skipped; a flag's separate value is not
		// distinguishable from a subcommand and wins.
		{[]string{"go", "-C", "sub", "test"}, "go sub"},
		// path prefixes collapse to the leaf name
		{[]string{"/usr/local/bin/go", "vet", "./..."}, "go vet"},
		{[]string{"./scripts/deploy.sh", "staging"}, "deploy.sh"},
		{[]string{"curl", "-sf", "http://localhost"}, "curl"},
		{[]string{""}, ""},
	}
	for _, tc := range cases {
		if got := commandGroup(tc.argv); got != tc.want {
			t.Errorf("commandGroup(%q) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

func TestCommandBase(t *testing.T) {
	cases := map[string]string{
		"go":                  "go",
		"/usr/bin/go":         "go",
		"./scripts/deploy.sh": "deploy.sh",
		`C:\tools\go.exe`:     "go.exe",
		"":                    "",
	}
	for in, want := range cases {
		if got := commandBase(in); got != want {
			t.Errorf("commandBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSessionLabel(t *testing.T) {
	roots := map[string][]string{
		"sess-a": {"make", "ci"},
		"sess-b": {},
	}
	cases := []struct{ id, want string }{
		{"sess-a", "make ci"},
		{"sess-b", ""},
		{"sess-missing", ""},
	}
	for _, tc := range cases {
		if got := sessionLabel(roots, tc.id); got != tc.want {
			t.Errorf("sessionLabel(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}
