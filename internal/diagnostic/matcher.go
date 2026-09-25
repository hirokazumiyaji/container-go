package diagnostic

import (
	"crypto/sha256"
	"strings"
)

type valueHash struct {
	length    int
	digest    [sha256.Size]byte
	prefix    uint32
	short     bool
	firstWord bool
	lastWord  bool
}

func hashValues(values []string) []valueHash {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[[sha256.Size]byte]struct{}, len(values))
	out := make([]valueHash, 0, len(values))
	for _, value := range values {
		if !usefulValue(value) || value == Redacted {
			continue
		}
		digest := sha256.Sum256([]byte(value))
		if _, ok := seen[digest]; ok {
			continue
		}
		seen[digest] = struct{}{}
		prefix := valuePrefix(value)
		if len(value) < 4 {
			prefix = fingerprint(value[:1])
		}
		out = append(out, valueHash{
			length:    len(value),
			digest:    digest,
			prefix:    prefix,
			short:     len(value) < 4,
			firstWord: wordByte(value[0]),
			lastWord:  wordByte(value[len(value)-1]),
		})
	}
	return out
}

func valuePrefix(value string) uint32 {
	if value == "" {
		return 0
	}
	n := len(value)
	if n > 4 {
		n = 4
	}
	return fingerprint(value[:n])
}

func valuePrefixAt(s string, start int) uint32 {
	if start >= len(s) {
		return 0
	}
	n := len(s) - start
	if n > 4 {
		n = 4
	}
	return fingerprint(s[start : start+n])
}

func shortPrefixAt(s string, start int) uint32 {
	if start >= len(s) {
		return 0
	}
	return fingerprint(s[start : start+1])
}

func fingerprint(value string) uint32 {
	// FNV-1a is only an index for the SHA-256 verifier below; no plaintext
	// prefix is retained in the matcher.
	const (
		offset = 2166136261
		prime  = 16777619
	)
	hash := uint32(offset)
	for i := 0; i < len(value); i++ {
		hash ^= uint32(value[i])
		hash *= prime
	}
	return hash
}

func mergeValueHashes(groups ...[]valueHash) []valueHash {
	seen := make(map[[sha256.Size]byte]struct{})
	var out []valueHash
	for _, group := range groups {
		for _, entry := range group {
			if _, ok := seen[entry.digest]; ok {
				continue
			}
			seen[entry.digest] = struct{}{}
			out = append(out, entry)
		}
	}
	return out
}

func longestHashMatch(s string, start int, entries []valueHash, force bool) int {
	matched := 0
	for _, entry := range entries {
		if (!force && entry.length < minimumTokenLength) || entry.length > len(s)-start {
			continue
		}
		boundarySafe := (!entry.firstWord || start == 0 || !wordByte(s[start-1])) &&
			(!entry.lastWord || start+entry.length == len(s) || !wordByte(s[start+entry.length]))
		if !force && !boundarySafe {
			continue
		}
		if sha256.Sum256([]byte(s[start:start+entry.length])) != entry.digest {
			continue
		}
		if entry.length > matched {
			matched = entry.length
		}
	}
	return matched
}

func replaceHashedValues(s string, hashes []valueHash, force bool, replacement string) string {
	if len(hashes) == 0 || s == "" {
		return s
	}
	byPrefix := make(map[uint32][]valueHash, len(hashes))
	byShort := make(map[uint32][]valueHash)
	for _, entry := range hashes {
		if entry.short {
			byShort[entry.prefix] = append(byShort[entry.prefix], entry)
		} else {
			byPrefix[entry.prefix] = append(byPrefix[entry.prefix], entry)
		}
	}

	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		matched := longestHashMatch(s, i, byShort[shortPrefixAt(s, i)], force)
		if len(s)-i >= 4 {
			if n := longestHashMatch(s, i, byPrefix[valuePrefixAt(s, i)], force); n > matched {
				matched = n
			}
		}
		if matched > 0 {
			b.WriteString(replacement)
			i += matched
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// NewHashedRedactor returns a redactor that does not retain caller-provided
// plaintext after construction. Values are expanded and hashed while the
// temporary strings are alive, then only lengths, digests, and non-reversible
// index fingerprints are retained.
// It is intended for bounded handle lifetimes where a closeable matcher is
// preferable to keeping credentials in a long-lived object.
func NewHashedRedactor(values ...string) *Redactor {
	r := newRedactor(values, true, false)
	r.source = nil
	r.values = nil
	return r
}

// NewHashedContextRedactor is the force-replacement variant of
// NewHashedRedactor for operation context.
func NewHashedContextRedactor(values ...string) *Redactor {
	r := newRedactor(values, true, true)
	r.source = nil
	r.values = nil
	return r
}

// Close releases the matcher's retained values and digests. The structural
// redactor remains usable after Close, but explicit values are no longer
// available. A closed redactor must not be closed again concurrently.
func (r *Redactor) Close() error {
	if r == nil {
		return nil
	}
	for i := range r.source {
		r.source[i] = ""
	}
	for i := range r.values {
		r.values[i] = ""
	}
	r.source = nil
	r.values = nil
	r.hashes = nil
	r.force = false
	return nil
}
