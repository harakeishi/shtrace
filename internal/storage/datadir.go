// Package storage owns the on-disk layout: base data dir resolution,
// SQLite metadata, JSON Lines output files, and FTS5 indexing.
package storage

import (
	"errors"
	"path/filepath"
)

// ResolveDataDir picks the base directory where shtrace stores its state. The
// order matches the plan: explicit SHTRACE_DATA_DIR > GitHub Actions
// workspace > XDG/macOS conventions.
//
// goos must be the runtime.GOOS-style string ("linux", "darwin"); callers
// thread it through so this stays testable.
func ResolveDataDir(env map[string]string, goos string) (string, error) {
	if v := env["SHTRACE_DATA_DIR"]; v != "" {
		return v, nil
	}
	if env["GITHUB_ACTIONS"] == "true" {
		if ws := env["GITHUB_WORKSPACE"]; ws != "" {
			return filepath.Join(ws, ".shtrace"), nil
		}
	}

	switch goos {
	case "darwin":
		home := env["HOME"]
		if home == "" {
			return "", errors.New("shtrace: HOME is required to resolve data dir on darwin")
		}
		return filepath.Join(home, "Library", "Application Support", "shtrace"), nil
	default:
		if xdg := env["XDG_DATA_HOME"]; xdg != "" {
			return filepath.Join(xdg, "shtrace"), nil
		}
		home := env["HOME"]
		if home == "" {
			return "", errors.New("shtrace: neither SHTRACE_DATA_DIR, XDG_DATA_HOME, nor HOME is set")
		}
		return filepath.Join(home, ".local", "share", "shtrace"), nil
	}
}

// OutputPath returns the JSON Lines log path for the given session/span pair
// underneath baseDir.
func OutputPath(baseDir, sessionID, spanID string) string {
	return filepath.Join(baseDir, "outputs", sessionID, spanID+".log")
}
