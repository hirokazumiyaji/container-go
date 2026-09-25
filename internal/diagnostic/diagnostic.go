// Package diagnostic contains the formatting rules used for errors and
// other user-facing diagnostics. Diagnostics are deliberately rendered
// without terminal control sequences and with common secret assignments
// redacted.
package diagnostic

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const redacted = "[REDACTED]"

var (
	assignmentRE = regexp.MustCompile(`(?i)(\b(?:password|passwd|pass|token|secret|stderr|api[-_]?key|access[-_]?key|private[-_]?key|credential|credentials|authorization|auth|bearer)\b[ \t]*[:=][ \t]*)(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
	jsonSecretRE = regexp.MustCompile(`(?i)("(?:password|passwd|pass|token|secret|stderr|api[-_]?key|access[-_]?key|private[-_]?key|credential|credentials|authorization|auth|bearer)"\s*:\s*)("(?:\\.|[^"\\])*"|[^,}\s]+)`)
	flagRE       = regexp.MustCompile(`(?i)(--(?:password|passwd|pass|token|secret|stderr|api[-_]?key|access[-_]?key|private[-_]?key|credential|credentials|authorization|auth|bearer)(?:=|[ \t]+))("[^"]*"|'[^']*'|[^\s,;]+)`)
	secretWordRE = regexp.MustCompile(`(?i)(\b(?:password|passwd|pass|token|secret|stderr|api[-_]?key|access[-_]?key|private[-_]?key|credential|credentials|authorization|auth|bearer)\b[ \t]+)("[^"]*"|'[^']*'|[^\s,;]+)`)
	bearerRE     = regexp.MustCompile(`(?i)\bBearer[ \t]+[^\s,;]+`)
	urlSecretRE  = regexp.MustCompile(`(?i)(://[^:/\s@]+:)[^@\s/]+(@)`)
)

// Redactor removes known secret values and common key/value secret forms
// from diagnostic text. A nil Redactor is valid and applies only the
// structural rules.
type Redactor struct {
	values []string
}

type safeError struct {
	err      error
	redactor *Redactor
}

// Wrap returns an error whose Error method is safe for terminal and log
// output while retaining the original error in its unwrap chain.
func Wrap(err error) error {
	return WithRedactor(err, NewRedactor())
}

// WithRedactor is the configurable form of Wrap.
func WithRedactor(err error, r *Redactor) error {
	if err == nil || r == nil {
		return err
	}
	return &safeError{err: err, redactor: r}
}

func (e *safeError) Error() string { return e.redactor.Text(e.err.Error()) }

func (e *safeError) Unwrap() error { return e.err }

func (e *safeError) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprint(state, e.Error())
}

// NewRedactor returns a redactor for the supplied values. Values are
// copied and their common escaped forms are included so a secret does
// not reappear merely because a CLI quoted or URL-encoded it.
func NewRedactor(values ...string) *Redactor {
	seen := make(map[string]struct{})
	var all []string
	add := func(value string) {
		if value == "" || value == redacted || value == "/" || value == "\\" || value == "=" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		all = append(all, value)
	}
	for _, value := range values {
		add(value)
		if quoted := strconv.Quote(value); len(quoted) >= 2 {
			add(quoted[1 : len(quoted)-1])
		}
		add(url.QueryEscape(value))
		add(url.PathEscape(value))
	}
	sort.SliceStable(all, func(i, j int) bool { return len(all[i]) > len(all[j]) })
	return &Redactor{values: all}
}

// Text redacts known values, applies common secret-shaped assignments,
// and escapes control characters. The returned text is safe to write to
// a terminal or a CI log.
func (r *Redactor) Text(s string) string {
	s = bearerRE.ReplaceAllString(s, "Bearer "+redacted)
	s = jsonSecretRE.ReplaceAllString(s, `${1}"`+redacted+`"`)
	s = assignmentRE.ReplaceAllString(s, `${1}`+redacted)
	s = flagRE.ReplaceAllString(s, `${1}`+redacted)
	s = secretWordRE.ReplaceAllString(s, `${1}`+redacted)
	s = urlSecretRE.ReplaceAllString(s, `${1}`+redacted+`${2}`)
	if r != nil {
		for _, value := range r.values {
			s = strings.ReplaceAll(s, value, redacted)
		}
	}
	return Sanitize(s)
}

// Args returns a copy of args with sensitive flags, assignments, labels,
// env-file paths, and mount arguments redacted before known values and
// control characters are filtered.
func (r *Redactor) Args(args []string) []string {
	if args == nil {
		return nil
	}
	out := make([]string, len(args))
	redactNext := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if redactNext {
			out[i] = r.Text(redacted)
			redactNext = false
			continue
		}

		if arg == "--label" || arg == "-l" {
			out[i] = r.Text(arg)
			if i+1 < len(args) {
				out[i+1] = r.Text(redactLabel(args[i+1]))
				i++
			}
			continue
		}
		if arg == "--env-file" || arg == "--env" || arg == "-e" || arg == "--mount" || arg == "--entrypoint" {
			out[i] = r.Text(arg)
			redactNext = i+1 < len(args)
			continue
		}
		if strings.HasPrefix(arg, "-p") && !strings.HasPrefix(arg, "--") && len(arg) > 2 {
			out[i] = r.Text("-p" + redacted)
			continue
		}

		if key, value, ok := strings.Cut(arg, "="); ok {
			if key == "--label" || key == "-l" {
				out[i] = r.Text(redactLabel(key + "=" + value))
				continue
			}
			if isSensitiveFlag(key) || isSensitiveName(key) {
				out[i] = r.Text(key + "=" + redacted)
				continue
			}
			if isOpaqueFlag(key) {
				out[i] = r.Text(key + "=" + redacted)
				continue
			}
		}
		if isSensitiveFlag(arg) || isOpaqueFlag(arg) {
			out[i] = r.Text(arg)
			redactNext = i+1 < len(args)
			continue
		}
		out[i] = r.Text(arg)
	}
	if start := commandPayloadStart(args); start >= 0 {
		for i := start + 1; i < len(out); i++ {
			out[i] = r.Text(redacted)
		}
	}
	return out
}

func commandPayloadStart(args []string) int {
	if len(args) < 2 {
		return -1
	}
	var consumesNext map[string]bool
	switch args[0] {
	case "run":
		consumesNext = map[string]bool{
			"--name": true, "--label": true, "-l": true, "--env-file": true, "--env": true, "-e": true,
			"--publish": true, "-p": true, "--mount": true, "--cpus": true, "--memory": true,
			"--user": true, "--workdir": true, "--network": true, "--platform": true, "--entrypoint": true,
			"--pull": true, "--volume": true, "-v": true,
		}
	case "exec":
		consumesNext = map[string]bool{
			"--env-file": true, "--env": true, "-e": true, "--user": true, "--workdir": true, "-w": true,
		}
	default:
		return -1
	}
	for i := 1; i < len(args); {
		arg := args[i]
		if arg == "--" {
			return i + 1
		}
		if strings.HasPrefix(arg, "-") {
			if strings.Contains(arg, "=") || !consumesNext[arg] {
				i++
				continue
			}
			i += 2
			continue
		}
		return i
	}
	return -1
}

// Sanitize escapes terminal control sequences and all other control or
// format runes. It intentionally renders line breaks visibly instead of
// returning a multi-line diagnostic.
func Sanitize(s string) string {
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

func redactLabel(value string) string {
	key, _, ok := strings.Cut(value, "=")
	if !ok {
		return redacted
	}
	return key + "=" + redacted
}

func isOpaqueFlag(flag string) bool {
	switch strings.ToLower(flag) {
	case "--env-file", "--env", "-e", "-p", "--mount", "--entrypoint", "--password-file", "--token-file", "--secret-file":
		return true
	default:
		return false
	}
}

func isSensitiveFlag(flag string) bool {
	return isSensitiveName(strings.TrimLeft(flag, "-"))
}

func isSensitiveName(name string) bool {
	normalized := strings.ToLower(name)
	normalized = strings.NewReplacer("-", "", "_", "", ".", "", "/", "", " ", "").Replace(normalized)
	for _, marker := range []string{
		"password", "passwd", "token", "secret", "credential", "authorization", "bearer", "apikey", "accesskey", "privatekey", "jwt",
	} {
		if strings.Contains(normalized, marker) {
			return true
		}
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
