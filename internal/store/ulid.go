package store

import (
	"crypto/rand"
	"time"

	"github.com/oklog/ulid/v2"
)

// NewID returns a new ULID string (IDs are ULIDs, time-sortable,
// timezone-independent).
func NewID() string {
	return NewIDAt(time.Now().UTC())
}

// NewIDAt returns a ULID string for the given instant (used by tests to
// verify sortability without racing the clock).
func NewIDAt(t time.Time) string {
	return ulid.MustNew(ulid.Timestamp(t.UTC()), rand.Reader).String()
}
