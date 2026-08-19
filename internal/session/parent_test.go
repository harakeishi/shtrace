package session

import (
	"os"
	"strings"
	"testing"
)

func TestParentKey_ResolvesForCurrentProcess(t *testing.T) {
	key, err := ParentKey()
	if err != nil {
		t.Fatalf("ParentKey() error = %v (the test process always has a parent)", err)
	}
	if key == "" {
		t.Fatal("ParentKey() returned an empty key")
	}
	// The key pairs the pid with the parent's start time so a recycled pid
	// cannot silently inherit an unrelated session.
	if !strings.Contains(key, ":") {
		t.Fatalf("ParentKey() = %q, want a pid:starttime pair", key)
	}
	ppid := os.Getppid()
	if !strings.HasPrefix(key, itoa(ppid)+":") {
		t.Fatalf("ParentKey() = %q, want it to start with ppid %d", key, ppid)
	}
}

func TestParentKey_IsStableAcrossCalls(t *testing.T) {
	first, err := ParentKey()
	if err != nil {
		t.Fatalf("first ParentKey: %v", err)
	}
	second, err := ParentKey()
	if err != nil {
		t.Fatalf("second ParentKey: %v", err)
	}
	if first != second {
		t.Fatalf("ParentKey not stable: %q then %q", first, second)
	}
}

func TestProcessKey_FailsForNonexistentPID(t *testing.T) {
	// A pid that cannot exist: callers must fall back to a fresh session
	// rather than grouping on a bogus key.
	if _, err := processKey(-1); err == nil {
		t.Fatal("processKey(-1) succeeded, want an error so grouping is skipped")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
