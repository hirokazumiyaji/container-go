// Package diagnostic contains the formatting rules used for errors and
// other user-facing diagnostics. Diagnostics are deliberately rendered
// without terminal control sequences and with common secret assignments
// redacted.
package diagnostic

import (
	"encoding/base64"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const (
	// Redacted is the stable replacement used in safe diagnostics.
	Redacted = "[REDACTED]"
	// Structural detectors use this lower bound for token-shaped values
	// whose presence is not tied to a named secret field. Explicit
	// values supplied to NewRedactor are still honored at any length.
	minimumTokenLength = 4
)

// Redactor removes explicitly supplied values and structurally recognizable
// secret values from diagnostic text. A nil Redactor is valid and applies
// only the structural rules.
//
// Redactor values are immutable. Add and Compose return a new redactor, which
// makes independently-created public-boundary redactors safe to combine.
type Redactor struct {
	source []string
	values []string
	hashes []valueHash
	index  *valueHashIndex
	force  bool
}

// NewRedactor returns a boundary-aware redactor for the supplied values.
// Values shorter than the structural minimum are left alone; use
// NewContextRedactor when a value came from an operation's explicit secret
// context. Values are copied and common escaped forms are included so a
// secret does not reappear merely because a CLI quoted, URL-encoded, or
// Basic-auth encoded it.
func NewRedactor(values ...string) *Redactor {
	return newRedactor(values, true, false)
}

// NewContextRedactor returns a redactor for values explicitly supplied by
// an operation's diagnostic context. Such values are replaced even when
// adjacent to other text, which prevents a short secret from surviving a
// truncation boundary. Structural-only redactors created with NewRedactor
// retain boundary-aware replacement.
func NewContextRedactor(values ...string) *Redactor {
	return newRedactor(values, true, true)
}

func newRedactor(values []string, expand, force bool) *Redactor {
	r := &Redactor{force: force}
	seen := make(map[string]struct{})
	for _, value := range values {
		r.addSource(value, seen)
	}
	if expand {
		r.values = r.expandedValues()
	} else {
		r.values = append([]string(nil), r.source...)
	}
	r.hashes = hashValues(r.values)
	r.index = newValueHashIndex(r.hashes)
	return r
}

func (r *Redactor) addSource(value string, seen map[string]struct{}) {
	if !usefulValue(value) {
		return
	}
	if _, ok := seen[value]; ok {
		return
	}
	seen[value] = struct{}{}
	r.source = append(r.source, value)
}

func usefulValue(value string) bool {
	if value == "" || value == Redacted {
		return false
	}
	// Replacing these punctuation-only values would destroy virtually every
	// URL and shell diagnostic without protecting a meaningful secret.
	switch value {
	case "/", "\\", "=", ",", ":", ";", " ":
		return false
	}
	return true
}

func (r *Redactor) expandedValues() []string {
	seen := make(map[string]struct{}, len(r.source)*5)
	values := make([]string, 0, len(r.source)*5)
	add := func(value string) {
		if !usefulValue(value) {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	for _, value := range r.source {
		add(value)
		if quoted := strconv.Quote(value); len(quoted) >= 2 {
			add(quoted[1 : len(quoted)-1])
		}
		if escaped := url.QueryEscape(value); escaped != value {
			add(escaped)
		}
		if escaped := url.PathEscape(value); escaped != value {
			add(escaped)
		}
		// Explicit values may be Basic-auth credentials. Including their
		// encodings also protects an error that only contains the header
		// value, not the original username/password fields.
		if len(value) >= minimumTokenLength {
			add(base64.StdEncoding.EncodeToString([]byte(value)))
			add(base64.RawStdEncoding.EncodeToString([]byte(value)))
		}
	}
	sort.SliceStable(values, func(i, j int) bool {
		return len(values[i]) > len(values[j])
	})
	return values
}

// WithValues returns a new redactor containing both r's values and values.
func (r *Redactor) WithValues(values ...string) *Redactor {
	if r == nil {
		return NewRedactor(values...)
	}
	combined := newRedactor(values, true, r.force)
	return newHashRedactor(mergeValueHashes(r.force, r.hashes, combined.hashes), r.force)
}

// WithBoundaryValues adds context values whose token boundaries remain significant
// even when this redactor also carries forced operation secrets.
func (r *Redactor) WithBoundaryValues(values ...string) *Redactor {
	added := newRedactor(values, true, false)
	for i := range added.hashes {
		added.hashes[i].boundary = true
	}
	return r.Compose(added)
}

// Add is an alias for WithValues, useful when composing independently-created
// redactors at a public boundary.
func (r *Redactor) Add(values ...string) *Redactor { return r.WithValues(values...) }

// WithForce returns a redactor that treats all of its explicit values as
// operation context, including short values adjacent to other text.
func (r *Redactor) WithForce() *Redactor {
	if r == nil {
		return NewContextRedactor()
	}
	return newHashRedactor(mergeValueHashes(true, r.hashes), true)
}

// Compose combines redactors without mutating any of them.
func (r *Redactor) Compose(others ...*Redactor) *Redactor {
	if r == nil && len(others) == 0 {
		return NewRedactor()
	}
	force := false
	var hashes []valueHash
	if r != nil {
		force = r.force
		hashes = append(hashes, r.hashes...)
	}
	for _, other := range others {
		if other != nil {
			force = force || other.force
			hashes = append(hashes, other.hashes...)
		}
	}
	return newHashRedactor(mergeValueHashes(force, hashes), force)
}

func newHashRedactor(hashes []valueHash, force bool) *Redactor {
	return &Redactor{hashes: hashes, index: newValueHashIndex(hashes), force: force}
}

// ComposeRedactors combines redactors in argument order.
func ComposeRedactors(redactors ...*Redactor) *Redactor {
	if len(redactors) == 0 {
		return NewRedactor()
	}
	return redactors[0].Compose(redactors[1:]...)
}

// Text redacts known values, applies structural secret rules, and escapes
// control characters. The returned text is safe to write to a terminal or a CI
// log.
func (r *Redactor) Text(s string) string {
	if s == "" {
		return s
	}
	if r == nil {
		r = NewRedactor()
	}

	// Do the structural passes before replacing explicitly supplied values.
	// This lets a field name (for example, "password") remain useful while
	// its value disappears, and also makes the result idempotent.
	s = redactPEM(s)
	s = redactCookieObjects(s)
	s = redactHeaders(s)
	s = redactJWT(s)
	s = redactAuth(s)
	s = redactQuotedAssignments(s)
	s = redactFlags(s)
	s = redactShortAttached(s)
	s = redactUnquotedAssignments(s)
	s = redactURLCredentials(s)
	s = redactSecretShaped(s)
	if len(r.hashes) > 0 {
		s = r.index.replace(s, r.force)
	}
	return Sanitize(s)
}

// Args returns a copy of args with sensitive flags, assignments, labels,
// env-file paths, volume/filter aliases, and run/exec payload values redacted
// before known values and control characters are filtered.
func (r *Redactor) Args(args []string) []string {
	if args == nil {
		return nil
	}
	if r == nil {
		r = NewRedactor()
	}
	out := make([]string, len(args))
	redactNext := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if redactNext {
			out[i] = Redacted
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
		if isOpaqueFlag(arg) {
			out[i] = r.Text(arg)
			redactNext = i+1 < len(args)
			continue
		}
		if key, _, ok := strings.Cut(arg, "="); ok {
			if isSecretName(key) || isOpaqueName(key) {
				out[i] = r.Text(key + "=" + Redacted)
				continue
			}
		}
		if flag, _, ok := attachedFlag(arg); ok && (isSensitiveFlag(flag) || isOpaqueFlag(flag)) {
			if strings.HasPrefix(flag, "--") {
				out[i] = r.Text(flag + "=" + Redacted)
			} else {
				out[i] = r.Text(flag + Redacted)
			}
			continue
		}
		if isSensitiveFlag(arg) {
			out[i] = r.Text(arg)
			redactNext = i+1 < len(args)
			continue
		}
		out[i] = r.Text(arg)
	}
	if start := commandPayloadStart(args); start >= 0 {
		for i := start; i < len(out); i++ {
			out[i] = Redacted
		}
	}
	return out
}

func commandPayloadStart(args []string) int {
	commandIndex := 0
	if len(args) > 0 && (args[0] == "container" || args[0] == "docker") {
		commandIndex = 1
	}
	if len(args) <= commandIndex+1 {
		return -1
	}
	var consumesNext map[string]bool
	switch args[commandIndex] {
	case "run", "create":
		consumesNext = map[string]bool{
			"--name": true, "--label": true, "-l": true, "--env-file": true, "--env": true, "-e": true,
			"--publish": true, "-p": true, "--mount": true, "--volume": true, "--volume-driver": true, "-v": true, "--filter": true, "--filter-file": true, "-f": true,
			"--cpus": true, "--memory": true, "--user": true, "--workdir": true, "--network": true, "--platform": true,
			"--network-alias": true,
			"--entrypoint":    true, "--pull": true,
			"--shm-size": true, "--cap-add": true, "--cap-drop": true, "--ulimit": true,
			"--add-host": true, "--hostname": true,
		}
	case "exec":
		consumesNext = map[string]bool{
			"--env-file": true, "--env": true, "-e": true, "--user": true, "--workdir": true, "-w": true,
		}
	default:
		return -1
	}
	for i := commandIndex + 1; i < len(args); {
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
		return i + 1
	}
	return -1
}

func attachedFlag(arg string) (flag, value string, ok bool) {
	if strings.HasPrefix(arg, "--") {
		if i := strings.IndexByte(arg, '='); i > 2 {
			return arg[:i], arg[i+1:], true
		}
		return "", "", false
	}
	if len(arg) > 2 && arg[0] == '-' && arg[1] != '-' {
		for _, flag := range []string{"-e", "-p", "-v", "-f", "-l"} {
			if strings.HasPrefix(arg, flag) && len(arg) > len(flag) {
				return flag, strings.TrimPrefix(arg[len(flag):], "="), true
			}
		}
	}
	return "", "", false
}
