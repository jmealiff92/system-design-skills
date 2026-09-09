// Package uuid provides a minimal, dependency-free UUIDv4 generator.
//
// Deliberately hand-rolled instead of pulling in google/uuid: this repo's
// two binaries otherwise have zero third-party dependencies (see the
// itrs-notify and event-relay package docs for why that matters across 80
// heterogeneous Gateway hosts), and a UUIDv4 is ~10 lines over crypto/rand.
package uuid

import (
	"crypto/rand"
	"fmt"
)

// V4 returns a random (version 4, variant 1) UUID string, e.g.
// "b6e5c3a2-1f4d-4e2a-9c3b-8a1e2f3d4c5b". Used as event_id — the value
// that later becomes the EMS Idempotency-Key, so it only needs to be
// unique, not sortable or otherwise structured.
func V4() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing means the OS entropy source is broken; there
		// is no sane fallback that keeps the uniqueness guarantee, so this
		// is one of the few places in the notify/relay path it's correct
		// to panic rather than limp on with a degraded event_id.
		panic("uuid: crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
