package diagnostic

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Sanitize escapes terminal control sequences and all other control or
// format runes. It intentionally renders line breaks visibly instead of
// returning a multi-line diagnostic. U+2028 and U+2029 are escaped as well;
// although they are Unicode separators rather than control characters, many
// log viewers treat them as line breaks.
func Sanitize(s string) string {
	if !sanitizeNeeded(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			b.WriteString(`\x1b`)
			i += ansiSequenceLen(s[i:])
			continue
		}
		if s[i] < 0x20 || s[i] == 0x7f {
			b.WriteString(escapeByte(s[i]))
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteString(escapeByte(s[i]))
			i++
			continue
		}
		if r == '\u2028' || r == '\u2029' {
			if r <= 0xffff {
				b.WriteString(`\u`)
				b.WriteString(hex4(uint16(r)))
			}
			i += size
			continue
		}
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			if r <= 0xffff {
				b.WriteString(`\u`)
				b.WriteString(hex4(uint16(r)))
			} else {
				b.WriteString(`\U`)
				b.WriteString(hex8(uint32(r)))
			}
			i += size
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}

// sanitizeNeeded reports whether Sanitize would rewrite any byte or rune in
// s. Diagnostics are usually clean, and returning the original string in that
// case avoids building a copy that only repeats the input.
func sanitizeNeeded(s string) bool {
	for i := 0; i < len(s); {
		c := s[i]
		if c >= 0x20 && c < 0x7f {
			i++
			continue
		}
		if c < 0x20 || c == 0x7f {
			return true
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			return true
		}
		if r == '\u2028' || r == '\u2029' || unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return true
		}
		i += size
	}
	return false
}

func ansiSequenceLen(s string) int {
	if len(s) < 2 {
		return len(s)
	}
	switch s[1] {
	case '[':
		i := 2
		for i < len(s) && s[i] >= 0x20 && s[i] <= 0x3f {
			i++
		}
		if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7e {
			i++
		}
		return i
	case ']', 'P', 'X', '^', '_':
		i := 2
		for i < len(s) {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
			i++
		}
		return i
	default:
		i := 1
		for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
			i++
		}
		if i < len(s) {
			i++
		}
		return i
	}
}

func escapeByte(b byte) string {
	const digits = "0123456789abcdef"
	if b == '\n' {
		return `\n`
	}
	if b == '\r' {
		return `\r`
	}
	if b == '\t' {
		return `\t`
	}
	return string([]byte{'\\', 'x', digits[b>>4], digits[b&0x0f]})
}

func hex4(v uint16) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[v>>12], digits[(v>>8)&0xf], digits[(v>>4)&0xf], digits[v&0xf]})
}

func hex8(v uint32) string {
	const digits = "0123456789abcdef"
	return string([]byte{
		digits[(v>>28)&0xf], digits[(v>>24)&0xf], digits[(v>>20)&0xf], digits[(v>>16)&0xf],
		digits[(v>>12)&0xf], digits[(v>>8)&0xf], digits[(v>>4)&0xf], digits[v&0xf],
	})
}
