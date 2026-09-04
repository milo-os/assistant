// Package usage builds and emits the assistant's billing usage as CloudEvents.
//
// The wire format is BYTE-COMPATIBLE with the TypeScript service's usage
// emitter (src/usage): the same CloudEvents envelope fields, the same ULID id
// format, the same meter names, int64-string values, dimensions, subject
// (projects/{name}), a 100-event batch cap, an optional x-api-key header, a 5s
// timeout, and never-throw semantics. A recorded golden of the TS wire is
// diffed against this package's output — do not "improve" the shape.
package usage

import (
	"crypto/rand"
	"regexp"
)

// crockford is the Crockford base32 alphabet used by ULID.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const (
	ulidTimeLen = 10
	ulidRandLen = 16
)

// NewULID generates a 26-character ULID (10 timestamp chars + 16 randomness
// chars) for the given unix-millisecond timestamp. It mirrors the TS ulid():
// lexicographic ordering and same-millisecond monotonicity are not required
// (each usage event is an independent dedup key), so the simple form is used.
func NewULID(nowMillis int64) string {
	var randBytes [10]byte
	// crypto/rand.Read never returns a short read; ignore err to match the
	// TS never-throw posture (a failed read would still yield zero bytes,
	// which is a valid — if non-random — ULID).
	_, _ = rand.Read(randBytes[:])
	return encodeTime(nowMillis) + encodeRandom(randBytes[:])
}

func encodeTime(now int64) string {
	out := make([]byte, ulidTimeLen)
	t := now
	for i := ulidTimeLen - 1; i >= 0; i-- {
		out[i] = crockford[t%32]
		t /= 32
	}
	return string(out)
}

func encodeRandom(randBytes []byte) string {
	out := make([]byte, 0, ulidRandLen)
	var bitBuf uint
	var bitCount uint
	i := 0
	for len(out) < ulidRandLen {
		if bitCount < 5 {
			bitBuf = (bitBuf << 8) | uint(randBytes[i])
			i++
			bitCount += 8
		}
		idx := (bitBuf >> (bitCount - 5)) & 0x1f
		bitCount -= 5
		out = append(out, crockford[idx])
	}
	return string(out)
}

var ulidRE = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// IsULID reports whether value is a syntactically valid 26-char Crockford ULID.
func IsULID(value string) bool {
	return ulidRE.MatchString(value)
}
