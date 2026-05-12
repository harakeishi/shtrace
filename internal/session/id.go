package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// DefaultIDGenerator returns an IDGenerator that yields UUIDv7 strings.
// UUIDv7 is chosen because IDs sort lexicographically by creation time, which
// is convenient for "list latest sessions" queries (RFC 9562).
func DefaultIDGenerator() IDGenerator {
	return IDGenerator{
		NewSessionID: newUUIDv7,
		NewSpanID:    newUUIDv7,
	}
}

func newUUIDv7() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}

	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)

	// version 7
	b[6] = (b[6] & 0x0f) | 0x70
	// IETF variant
	b[8] = (b[8] & 0x3f) | 0x80

	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32], nil
}
