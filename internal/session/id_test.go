package session

import (
	"regexp"
	"testing"
)

var uuidv7Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestDefaultIDGenerator_ProducesUUIDv7(t *testing.T) {
	gen := DefaultIDGenerator()

	sid, err := gen.NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	if !uuidv7Pattern.MatchString(sid) {
		t.Fatalf("session ID %q does not match UUIDv7 pattern", sid)
	}

	spid, err := gen.NewSpanID()
	if err != nil {
		t.Fatalf("NewSpanID: %v", err)
	}
	if !uuidv7Pattern.MatchString(spid) {
		t.Fatalf("span ID %q does not match UUIDv7 pattern", spid)
	}

	if sid == spid {
		t.Fatalf("session and span IDs collided: %q", sid)
	}
}

func TestDefaultIDGenerator_Unique(t *testing.T) {
	gen := DefaultIDGenerator()
	seen := map[string]struct{}{}
	for i := 0; i < 100; i++ {
		id, err := gen.NewSpanID()
		if err != nil {
			t.Fatalf("NewSpanID: %v", err)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id at iteration %d: %s", i, id)
		}
		seen[id] = struct{}{}
	}
}
