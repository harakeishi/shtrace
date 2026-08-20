package cli

import (
	"path/filepath"
	"strings"
)

// displayShells are the shell basenames whose `-c <string>` argument carries
// the command the user actually cares about.
var displayShells = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
}

// commandLabel renders argv for humans. Auto-wrapped commands arrive as
// [<shell path>, -c, "<command>"], where argv[0] alone ("/opt/homebrew/bin/zsh")
// says nothing about what ran — so for a shell invoked with -c, the command
// string is the label. Anything else is the argv joined.
//
// No attempt is made to strip agent-specific boilerplate from the command
// string: such heuristics break as soon as the agent changes its wrapper.
func commandLabel(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	if cmd, ok := shellCommandString(argv); ok {
		return collapseWhitespace(cmd)
	}
	return collapseWhitespace(strings.Join(argv, " "))
}

// shellCommandString returns the argument following -c when argv is a shell
// invoked to run a command string.
func shellCommandString(argv []string) (string, bool) {
	if len(argv) < 3 {
		return "", false
	}
	if !displayShells[filepath.Base(argv[0])] {
		return "", false
	}
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		if arg == "-c" {
			if i+1 < len(argv) {
				return argv[i+1], true
			}
			return "", false
		}
		// Only option words may precede -c; a bare word means this is a script
		// invocation rather than a command string.
		if !strings.HasPrefix(arg, "-") {
			return "", false
		}
	}
	return "", false
}

// collapseWhitespace flattens a multi-line command into one display line so a
// heredoc or multi-command string cannot break the table layout.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncate shortens s to at most limit runes, marking elision with "...".
// It counts runes rather than bytes so multi-byte text is never cut mid-rune.
func truncate(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	if limit <= 3 {
		return string(runes[:limit])
	}
	return string(runes[:limit-3]) + "..."
}
