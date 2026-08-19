package cli

import "testing"

func TestCommandLabel_ExtractsShellCommandString(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{
			name: "bash -c",
			argv: []string{"/opt/homebrew/bin/bash", "-c", "go test ./..."},
			want: "go test ./...",
		},
		{
			name: "login shell before -c",
			argv: []string{"/bin/zsh", "-l", "-c", "npm run build"},
			want: "npm run build",
		},
		{
			name: "sh -c",
			argv: []string{"sh", "-c", "echo hi"},
			want: "echo hi",
		},
		{
			// Not a shell: the -c belongs to the program, so joining is right.
			name: "non-shell argv with -c",
			argv: []string{"docker", "-c", "ctx", "ps"},
			want: "docker -c ctx ps",
		},
		{
			name: "plain command",
			argv: []string{"go", "test", "./..."},
			want: "go test ./...",
		},
		{
			name: "shell without -c",
			argv: []string{"/bin/bash", "script.sh"},
			want: "/bin/bash script.sh",
		},
		{
			// -c present but no command string after it.
			name: "dangling -c",
			argv: []string{"/bin/bash", "-c"},
			want: "/bin/bash -c",
		},
		{
			name: "empty argv falls back to command",
			argv: nil,
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandLabel(tc.argv); got != tc.want {
				t.Fatalf("commandLabel(%q) = %q, want %q", tc.argv, got, tc.want)
			}
		})
	}
}

func TestCommandLabel_CollapsesNewlinesForSingleLineDisplay(t *testing.T) {
	got := commandLabel([]string{"/bin/bash", "-c", "echo one\necho two"})
	want := "echo one echo two"
	if got != want {
		t.Fatalf("commandLabel = %q, want %q (newlines must not break table layout)", got, want)
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		limit int
		want  string
	}{
		{name: "under limit is unchanged", in: "go test", limit: 20, want: "go test"},
		{name: "exactly at limit is unchanged", in: "abcde", limit: 5, want: "abcde"},
		{name: "over limit gets ellipsis", in: "abcdefghij", limit: 6, want: "abc..."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncate(tc.in, tc.limit); got != tc.want {
				t.Fatalf("truncate(%q, %d) = %q, want %q", tc.in, tc.limit, got, tc.want)
			}
		})
	}
}

// Truncation must not split a multi-byte rune, which would emit invalid UTF-8.
func TestTruncate_IsRuneSafe(t *testing.T) {
	got := truncate("日本語テキスト", 5)
	for _, r := range got {
		if r == '�' {
			t.Fatalf("truncate produced an invalid rune: %q", got)
		}
	}
	if len([]rune(got)) > 5 {
		t.Fatalf("truncate returned %d runes, want <= 5: %q", len([]rune(got)), got)
	}
}
