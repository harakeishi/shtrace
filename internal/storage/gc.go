package storage

import (
	"context"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

const (
	defaultTTLDays      = 30
	defaultMaxSizeBytes = int64(10 * 1024 * 1024 * 1024) // 10 GB
)

// GCConfig controls which sessions the garbage collector removes.
// Zero values fall back to the defaults.
type GCConfig struct {
	// TTLDays is the maximum session age in days. Default: 30.
	TTLDays int
	// MaxSizeBytes is the total output-storage cap in bytes. When exceeded,
	// the oldest sessions are removed until the total falls below the cap.
	// Default: 10 GB.
	MaxSizeBytes int64
}

// EffectiveTTLDays returns the configured value or the default.
func (c GCConfig) EffectiveTTLDays() int {
	if c.TTLDays > 0 {
		return c.TTLDays
	}
	return defaultTTLDays
}

// EffectiveMaxBytes returns the configured value or the default.
func (c GCConfig) EffectiveMaxBytes() int64 {
	if c.MaxSizeBytes > 0 {
		return c.MaxSizeBytes
	}
	return defaultMaxSizeBytes
}

// GCConfigFromEnv builds a GCConfig from SHTRACE_TTL_DAYS and
// SHTRACE_MAX_SIZE_BYTES environment variables. Unknown or invalid values are
// silently ignored and the defaults apply.
func GCConfigFromEnv(env map[string]string) GCConfig {
	var cfg GCConfig
	if v := env["SHTRACE_TTL_DAYS"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.TTLDays = n
		}
	}
	if v := env["SHTRACE_MAX_SIZE_BYTES"]; v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.MaxSizeBytes = n
		}
	}
	return cfg
}

// GCResult summarises what the GC run deleted (or would delete in dry-run mode).
type GCResult struct {
	// SessionsRemoved is the number of sessions removed (or that would be).
	SessionsRemoved int
	// BytesReclaimed is the total output-file bytes removed (or that would be).
	BytesReclaimed int64
	// Sessions lists the IDs of removed sessions, oldest first.
	Sessions []string
}

// RunGC removes sessions that exceed the TTL or push total output storage over
// the size cap defined in cfg.
//
// When dryRun is true the function computes what it would remove but makes no
// changes to the database or disk.  fts may be nil when no FTS index exists.
func RunGC(ctx context.Context, store *Store, fts *FTSStore, baseDir string, cfg GCConfig, dryRun bool) (GCResult, error) {
	// Fetch all sessions; math.MaxInt32 sidesteps the default limit of 50.
	sessions, err := store.ListSessions(ctx, math.MaxInt32, nil)
	if err != nil {
		return GCResult{}, fmt.Errorf("gc: list sessions: %w", err)
	}

	// Sort oldest-first so the size-cap pass removes the oldest data first.
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartedAt.Before(sessions[j].StartedAt)
	})

	cutoff := time.Now().UTC().AddDate(0, 0, -cfg.EffectiveTTLDays())
	toDelete := make(map[string]struct{})

	// TTL pass: mark sessions older than the cutoff.
	for _, s := range sessions {
		if s.StartedAt.Before(cutoff) {
			toDelete[s.ID] = struct{}{}
		}
	}

	// Size-cap pass.
	// Start from the current total, subtract the sizes that the TTL pass will
	// already reclaim, then keep removing oldest remaining sessions until the
	// projected total fits within the cap.
	total, err := gcTotalOutputSize(baseDir)
	if err != nil {
		return GCResult{}, fmt.Errorf("gc: measure output size: %w", err)
	}
	for id := range toDelete {
		sz, _ := gcSessionSize(baseDir, id)
		total -= sz
	}
	maxBytes := cfg.EffectiveMaxBytes()
	if total > maxBytes {
		for _, s := range sessions {
			if _, already := toDelete[s.ID]; already {
				continue
			}
			sz, _ := gcSessionSize(baseDir, s.ID)
			toDelete[s.ID] = struct{}{}
			total -= sz
			if total <= maxBytes {
				break
			}
		}
	}

	// Execute deletions (oldest first). In dry-run mode only report; do not
	// touch disk or DB.
	var result GCResult
	for _, s := range sessions {
		if _, ok := toDelete[s.ID]; !ok {
			continue
		}
		sz, _ := gcSessionSize(baseDir, s.ID)

		if dryRun {
			result.Sessions = append(result.Sessions, s.ID)
			result.BytesReclaimed += sz
			result.SessionsRemoved++
			continue
		}

		// Deletion order: authoritative DB first, then FTS (best-effort),
		// then disk. This ensures that if the DB delete fails the FTS index
		// and output files are left intact and consistent.
		if err := store.DeleteSession(ctx, s.ID); err != nil {
			return result, fmt.Errorf("gc: delete session %s: %w", s.ID, err)
		}
		if fts != nil {
			// FTS errors are non-fatal: the session is already gone from the
			// authoritative DB so stale index rows are harmless.
			_ = fts.DeleteSessionIndex(ctx, s.ID)
		}
		if err := os.RemoveAll(filepath.Join(baseDir, "outputs", s.ID)); err != nil {
			return result, fmt.Errorf("gc: remove outputs for session %s: %w", s.ID, err)
		}

		// Only count after every deletion step succeeds.
		result.Sessions = append(result.Sessions, s.ID)
		result.BytesReclaimed += sz
		result.SessionsRemoved++
	}
	return result, nil
}

// gcTotalOutputSize returns the total byte size of all files under
// baseDir/outputs/. Returns 0 without error when the directory does not exist.
func gcTotalOutputSize(baseDir string) (int64, error) {
	dir := filepath.Join(baseDir, "outputs")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return 0, nil
	}
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			info, infoErr := d.Info()
			if infoErr != nil {
				if os.IsNotExist(infoErr) {
					return nil // benign TOCTOU race: file removed between enumeration and stat
				}
				return infoErr
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// gcSessionSize returns the total byte size of output files for one session.
// Returns 0 without error when the session directory does not exist.
func gcSessionSize(baseDir, sessionID string) (int64, error) {
	dir := filepath.Join(baseDir, "outputs", sessionID)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return 0, nil
	}
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			info, infoErr := d.Info()
			if infoErr != nil {
				if os.IsNotExist(infoErr) {
					return nil // benign TOCTOU race
				}
				return infoErr
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}
