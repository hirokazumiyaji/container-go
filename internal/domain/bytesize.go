package domain

import (
	"fmt"
	"regexp"
	"strconv"
)

// MaxByteSize is the portable, backend-neutral ceiling for a parsed byte
// size. A backend may enforce a smaller capability limit.
const MaxByteSize uint64 = 1 << 50 // 1 PiB

// byteSizeRE accepts sizes like "512M" or "1G". A unit beyond this syntax is
// a backend capability and is checked separately.
var byteSizeRE = regexp.MustCompile(`^[0-9]+[KMGTP]?$`)

// ByteSize is a parsed positive byte size such as "64M".
//
// It keeps the caller's spelling so a backend argument can reproduce it, and
// the normalized byte count so comparisons and fingerprints do not depend on
// how the same size was written. The zero value means no size was requested.
type ByteSize struct {
	raw   string
	bytes uint64
}

// ParseByteSize parses a decimal integer with an optional K, M, G, T, or P
// suffix. Units are binary (1K = 1024 bytes), matching the container CLIs.
// Zero and sizes above MaxByteSize are rejected, as are suffixes outside that
// set, because the public options reject the same values before any backend
// work.
func ParseByteSize(size string) (ByteSize, error) {
	if !byteSizeRE.MatchString(size) {
		return ByteSize{}, fmt.Errorf("invalid size %q: want an integer with an optional K, M, G, T, or P suffix", size)
	}

	digits := size
	unit := uint64(1)
	if last := size[len(size)-1]; last < '0' || last > '9' {
		digits = size[:len(size)-1]
		switch last {
		case 'K':
			unit = 1 << 10
		case 'M':
			unit = 1 << 20
		case 'G':
			unit = 1 << 30
		case 'T':
			unit = 1 << 40
		case 'P':
			unit = 1 << 50
		}
	}
	value, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return ByteSize{}, fmt.Errorf("invalid or overflowing size %q: %w", size, err)
	}
	if value == 0 {
		return ByteSize{}, fmt.Errorf("size must be greater than zero: %q", size)
	}
	if value > MaxByteSize/unit {
		return ByteSize{}, fmt.Errorf("size overflows the %d-byte maximum: %q", MaxByteSize, size)
	}
	return ByteSize{raw: size, bytes: value * unit}, nil
}

// Bytes returns the size in bytes. The zero value returns 0.
func (b ByteSize) Bytes() uint64 { return b.bytes }

// String returns the spelling the size was parsed from, or "" when unset.
// Backends receive this spelling so the caller's unit notation is preserved.
func (b ByteSize) String() string { return b.raw }

// IsZero reports whether no size was requested.
func (b ByteSize) IsZero() bool { return b.bytes == 0 }
