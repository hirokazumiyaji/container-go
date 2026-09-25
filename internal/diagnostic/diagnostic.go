// Package diagnostic contains the formatting rules used for errors and
// other user-facing diagnostics. Diagnostics are deliberately rendered
// without terminal control sequences and with common secret assignments
// redacted.
package diagnostic

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// Redacted is the stable replacement used in safe diagnostics.
	Redacted = "[REDACTED]"
	// Structural detectors use this lower bound for token-shaped values
	// whose presence is not tied to a named secret field. Explicit
	// values supplied to NewRedactor are still honored at any length.
	minimumTokenLength = 4
)

var (
	// These expressions deliberately use ASCII name characters rather
	// than \b: underscore is a word character in the secret names this
	// package needs to recognize (for example, api_key and x_auth_token).
	doubleQuotedRE = regexp.MustCompile(`(?i)("[A-Za-z][A-Za-z0-9_.-]*"\s*:\s*")((?:\\.|[^"\\])*)(")`)
	singleQuotedRE = regexp.MustCompile(`(?i)('[A-Za-z][A-Za-z0-9_.-]*'\s*:\s*')((?:\\.|[^'\\])*)(')`)
	doubleValueRE  = regexp.MustCompile(`(?i)([A-Za-z][A-Za-z0-9_.-]*\s*(?:=|:)\s*)"((?:\\.|[^"\\])*)(")`)
	singleValueRE  = regexp.MustCompile(`(?i)([A-Za-z][A-Za-z0-9_.-]*\s*(?:=|:)\s*)'((?:\\.|[^'\\])*)(')`)

	flagValueRE     = regexp.MustCompile(`(?i)(^|[ \t])(--[A-Za-z][A-Za-z0-9_-]*[ \t]*(?:=|[ \t]+))("[^"]*"|'[^']*'|[^\s,;]+)`)
	shortAttachedRE = regexp.MustCompile(`(^|[ \t])(-[evpfl])([A-Za-z0-9_./=:-]+)`)
	shortSeparateRE = regexp.MustCompile(`(^|[ \t])(-[evpfl])([ \t]+)("[^"]*"|'[^']*'|[^\s,;]+)`)
	headerBlockRE   = regexp.MustCompile(`(?im)(^|[ \t])([A-Za-z][A-Za-z0-9-]*[ \t]*:[ \t]*)([^\r\n]*(?:(?:\r?\n)[ \t]+[^\r\n]*)*)`)
	headerRE        = regexp.MustCompile(`(?im)(^|[ \t])([A-Za-z][A-Za-z0-9-]*[ \t]*:[ \t]*)([^\r\n]*)`)
	basicRE         = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9])(Basic|Bearer)([ \t]+)([A-Za-z0-9+/=_-]+)`)
	secretShapedRE  = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_-])[A-Za-z0-9_-]*(?:password|passwd|secret|token|api[-_.]?key|access[-_.]?key|private[-_.]?key|client[-_.]?secret|credential|authorization|signature|jwt)[A-Za-z0-9_-]*($|[^A-Za-z0-9_-])`)
	jwtRE           = regexp.MustCompile(`(^|[^A-Za-z0-9_-])eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*($|[^A-Za-z0-9_-])`)
	pemRE           = regexp.MustCompile(`(?s)-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----`)
	urlCredentialRE = regexp.MustCompile(`(?i)((?:[a-z][a-z0-9+.-]*://|//))([^/\s@]*)(@)`)
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
	return newRedactor(append(append([]string(nil), r.source...), values...), true, r.force)
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
	return newRedactor(r.source, true, true)
}

// Compose combines redactors without mutating any of them.
func (r *Redactor) Compose(others ...*Redactor) *Redactor {
	if r == nil && len(others) == 0 {
		return NewRedactor()
	}
	var values []string
	force := false
	if r != nil {
		values = append(values, r.source...)
		force = r.force
	}
	for _, other := range others {
		if other != nil {
			values = append(values, other.source...)
			force = force || other.force
		}
	}
	return newRedactor(values, true, force)
}

// ComposeRedactors combines redactors in argument order.
func ComposeRedactors(redactors ...*Redactor) *Redactor {
	if len(redactors) == 0 {
		return NewRedactor()
	}
	return redactors[0].Compose(redactors[1:]...)
}

// Compose is a short alias for ComposeRedactors.
func Compose(redactors ...*Redactor) *Redactor { return ComposeRedactors(redactors...) }

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
	s = redactHeaders(s)
	s = redactJWT(s)
	s = redactAuth(s)
	s = redactQuotedAssignments(s)
	s = redactFlags(s)
	s = redactShortAttached(s)
	s = redactUnquotedAssignments(s)
	s = redactURLCredentials(s)
	s = redactSecretShaped(s)
	for _, value := range r.values {
		s = replaceKnownValue(s, value, r.force)
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
			"--entrypoint": true, "--pull": true,
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

func redactPEM(s string) string {
	return pemRE.ReplaceAllString(s, Redacted)
}

func redactHeaders(s string) string {
	redact := func(re *regexp.Regexp) string {
		return re.ReplaceAllStringFunc(s, func(match string) string {
			parts := re.FindStringSubmatch(match)
			if len(parts) != 4 || !isSecretHeader(parts[2]) || strings.TrimSpace(parts[3]) == "" {
				return match
			}
			return parts[1] + parts[2] + Redacted
		})
	}
	s = redact(headerBlockRE)
	return headerRE.ReplaceAllStringFunc(s, func(match string) string {
		parts := headerRE.FindStringSubmatch(match)
		if len(parts) != 4 || !isSecretHeader(parts[2]) || strings.TrimSpace(parts[3]) == "" {
			return match
		}
		return parts[1] + parts[2] + Redacted
	})
}

func redactAuth(s string) string {
	return basicRE.ReplaceAllStringFunc(s, func(match string) string {
		parts := basicRE.FindStringSubmatch(match)
		if len(parts) != 5 || len(parts[4]) < minimumTokenLength {
			return match
		}
		return parts[1] + parts[2] + parts[3] + Redacted
	})
}

func redactQuotedAssignments(s string) string {
	s = doubleQuotedRE.ReplaceAllStringFunc(s, func(match string) string {
		parts := doubleQuotedRE.FindStringSubmatch(match)
		if len(parts) != 4 || !nameFromAssignment(parts[1]) || parts[2] == "" || parts[2] == Redacted {
			return match
		}
		return parts[1] + Redacted + parts[3]
	})
	s = singleQuotedRE.ReplaceAllStringFunc(s, func(match string) string {
		parts := singleQuotedRE.FindStringSubmatch(match)
		if len(parts) != 4 || !nameFromAssignment(parts[1]) || parts[2] == "" || parts[2] == Redacted {
			return match
		}
		return parts[1] + Redacted + parts[3]
	})
	s = doubleValueRE.ReplaceAllStringFunc(s, func(match string) string {
		parts := doubleValueRE.FindStringSubmatch(match)
		if len(parts) != 4 || !nameFromAssignment(parts[1]) || parts[2] == "" || parts[2] == Redacted {
			return match
		}
		return parts[1] + `"` + Redacted + parts[3]
	})
	return singleValueRE.ReplaceAllStringFunc(s, func(match string) string {
		parts := singleValueRE.FindStringSubmatch(match)
		if len(parts) != 4 || !nameFromAssignment(parts[1]) || parts[2] == "" || parts[2] == Redacted {
			return match
		}
		return parts[1] + `'` + Redacted + parts[3]
	})
}

func redactShortAttached(s string) string {
	s = shortAttachedRE.ReplaceAllStringFunc(s, func(match string) string {
		parts := shortAttachedRE.FindStringSubmatch(match)
		if len(parts) != 4 || parts[3] == "" {
			return match
		}
		return parts[1] + parts[2] + Redacted
	})
	return shortSeparateRE.ReplaceAllStringFunc(s, func(match string) string {
		parts := shortSeparateRE.FindStringSubmatch(match)
		if len(parts) != 4 || parts[3] == "" {
			return match
		}
		return parts[1] + parts[2] + parts[3] + Redacted
	})
}

func redactFlags(s string) string {
	return flagValueRE.ReplaceAllStringFunc(s, func(match string) string {
		parts := flagValueRE.FindStringSubmatch(match)
		if len(parts) != 4 || !isSensitiveFlag(strings.TrimSpace(parts[2])) && !isOpaqueFlag(strings.TrimSpace(parts[2])) {
			return match
		}
		return parts[1] + parts[2] + Redacted
	})
}

func redactUnquotedAssignments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for i := 0; i < len(s); {
		if !isNameStart(s, i) || (i > 0 && isNameByte(s[i-1])) {
			i++
			continue
		}
		start := i
		i++
		for i < len(s) && isNameByte(s[i]) {
			i++
		}
		name := s[start:i]
		sep := i
		for sep < len(s) && (s[sep] == ' ' || s[sep] == '\t' || s[sep] == '\r' || s[sep] == '\n') {
			sep++
		}
		if sep >= len(s) || (s[sep] != '=' && s[sep] != ':') {
			continue
		}
		valueStart := sep + 1
		for valueStart < len(s) && (s[valueStart] == ' ' || s[valueStart] == '\t' || s[valueStart] == '\r' || s[valueStart] == '\n') {
			valueStart++
		}
		if valueStart >= len(s) || s[valueStart] == '\r' || s[valueStart] == '\n' || s[valueStart] == '"' || s[valueStart] == '\'' {
			continue
		}
		if strings.HasPrefix(s[valueStart:], Redacted) {
			i = valueStart + len(Redacted)
			continue
		}
		valueEnd := valueStart
		for valueEnd < len(s) {
			c := s[valueEnd]
			if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == ',' || c == ';' || c == '}' || c == ']' || c == ')' || c == '"' || c == '\'' {
				break
			}
			valueEnd++
		}
		if valueEnd == valueStart {
			continue
		}
		if (isSecretName(name) || isOpaqueName(name)) && s[valueStart:valueEnd] != Redacted {
			if isSecretName(name) {
				valueEnd = extendMultilineSecret(s, valueEnd)
			}
			b.WriteString(s[last:valueStart])
			b.WriteString(Redacted)
			last = valueEnd
			i = valueEnd
			continue
		}
		// A non-sensitive outer assignment (for example, "log:") must
		// not hide a nested sensitive assignment in its value. Resume at
		// the value rather than consuming it.
		i = valueStart
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

func extendMultilineSecret(s string, end int) int {
	pos := end
	for lines := 0; lines < 64 && pos < len(s); lines++ {
		if s[pos] != '\r' && s[pos] != '\n' {
			break
		}
		for pos < len(s) && (s[pos] == '\r' || s[pos] == '\n') {
			pos++
		}
		lineEnd := pos
		for lineEnd < len(s) && s[lineEnd] != '\r' && s[lineEnd] != '\n' {
			lineEnd++
		}
		line := strings.TrimSpace(s[pos:lineEnd])
		if line == "" || looksLikeAssignmentLine(line) {
			break
		}
		end = lineEnd
		pos = lineEnd
	}
	return end
}

func looksLikeAssignmentLine(line string) bool {
	if strings.HasPrefix(line, "=") {
		return false
	}
	key, _, ok := strings.Cut(line, "=")
	if !ok {
		return false
	}
	key = strings.TrimSpace(key)
	return isSecretName(key) || isOpaqueName(key)
}

func isNameStart(s string, i int) bool {
	c := s[i]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isNameByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.'
}

func redactJWT(s string) string {
	return jwtRE.ReplaceAllStringFunc(s, func(match string) string {
		parts := jwtRE.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		return parts[1] + Redacted + parts[2]
	})
}

func redactURLCredentials(s string) string {
	return urlCredentialRE.ReplaceAllStringFunc(s, func(match string) string {
		parts := urlCredentialRE.FindStringSubmatch(match)
		if len(parts) != 4 {
			return match
		}
		userinfo := parts[2]
		colon := strings.LastIndexByte(userinfo, ':')
		if colon < 0 || colon == len(userinfo)-1 {
			return match
		}
		// An empty username is valid URL syntax and still carries a
		// password, so do not require a non-empty user component.
		return parts[1] + userinfo[:colon] + ":" + Redacted + parts[3]
	})
}

func redactSecretShaped(s string) string {
	return secretShapedRE.ReplaceAllStringFunc(s, func(match string) string {
		parts := secretShapedRE.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		value := strings.TrimSuffix(strings.TrimPrefix(parts[0], parts[1]), parts[2])
		if parts[2] == "=" || parts[2] == ":" {
			return match
		}
		lowerValue := strings.ToLower(value)
		hasSeparator := strings.ContainsAny(value, "-_.")
		hasSuffixMarker := strings.HasSuffix(lowerValue, "secret") || strings.HasSuffix(lowerValue, "token") || strings.HasSuffix(lowerValue, "password") || strings.HasSuffix(lowerValue, "signature")
		if !hasSeparator && !hasSuffixMarker {
			return match
		}
		switch strings.ToLower(value) {
		case "authorization", "password", "token", "secret", "signature", "cookie", "credentials", "bearer", "jwt":
			return match
		}
		if len(value) < minimumTokenLength || (len(value) < 10 && !strings.ContainsAny(value, "-_.")) {
			return match
		}
		return parts[1] + Redacted + parts[2]
	})
}

func nameFromAssignment(fragment string) bool {
	name := strings.TrimSpace(fragment)
	name = strings.TrimRight(name, ":=")
	name = strings.TrimSpace(name)
	name = strings.Trim(name, "\"'")
	return isSecretName(name) || isOpaqueName(name)
}

func isSecretHeader(name string) bool {
	name = strings.TrimSpace(strings.TrimSuffix(name, ":"))
	lower := strings.ToLower(name)
	return lower == "authorization" || lower == "proxy-authorization" || lower == "cookie" || lower == "set-cookie" || isSecretName(name)
}

func isSensitiveFlag(flag string) bool {
	name := strings.TrimLeft(strings.TrimSpace(flag), "-")
	return isSecretName(name) || isOpaqueName(name)
}

func isOpaqueFlag(flag string) bool {
	switch strings.ToLower(strings.TrimSpace(flag)) {
	case "--env", "-e", "--env-file", "--label", "-l", "--mount", "--volume", "--volume-driver", "-v", "--filter", "--filter-file", "-f", "--publish", "-p", "--entrypoint", "--password-file", "--token-file", "--secret-file":
		return true
	default:
		return false
	}
}

func isOpaqueName(name string) bool {
	switch normalizeName(name) {
	case "env", "envfile", "label", "mount", "volume", "volumedriver", "filter", "filters", "publish", "entrypoint":
		return true
	default:
		return false
	}
}

func isSecretName(name string) bool {
	normalized := normalizeName(name)
	if normalized == "" {
		return false
	}
	// Match complete components and common prefixed/suffixed spellings,
	// including api_key, x_signature, and client-secret.
	markers := []string{
		"password", "passwd", "pwd", "passphrase", "secret", "token", "apikey", "accesskey", "privatekey",
		"credential", "credentials", "oauth", "auth", "authorization", "bearer", "cookie", "session", "sessionid", "connectsid", "jsessionid", "sid", "jwt",
		"signature", "sig", "hmac", "clientsecret", "refreshtoken", "accesstoken", "idtoken", "csrftoken", "pass", "stderr", "stdout",
	}
	for _, marker := range markers {
		if normalized == marker || strings.HasPrefix(normalized, marker) || strings.HasSuffix(normalized, marker) {
			return true
		}
	}
	return false
}

func normalizeName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func redactLabel(value string) string {
	key, _, ok := strings.Cut(value, "=")
	if !ok {
		return Redacted
	}
	if isSecretName(key) || isOpaqueName(key) {
		return Redacted
	}
	return key + "=" + Redacted
}

func replaceKnownValue(s, value string, force bool) string {
	if !usefulValue(value) || value == Redacted || (!force && len(value) < minimumTokenLength) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for start := 0; start < len(s); {
		i := strings.Index(s[start:], value)
		if i < 0 {
			b.WriteString(s[start:])
			break
		}
		i += start
		end := i + len(value)
		if marker := strings.Index(s[start:], Redacted); marker >= 0 {
			marker += start
			if i < marker+len(Redacted) && end > marker {
				b.WriteString(s[start : marker+len(Redacted)])
				start = marker + len(Redacted)
				continue
			}
		}
		boundarySafe := (!wordByte(value[0]) || i == 0 || !wordByte(s[i-1])) &&
			(!wordByte(value[len(value)-1]) || end == len(s) || !wordByte(s[end]))
		if force || boundarySafe {
			b.WriteString(s[start:i])
			b.WriteString(Redacted)
			start = end
		} else {
			b.WriteString(s[start : i+1])
			start = i + 1
		}
	}
	return b.String()
}

func wordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

// Sanitize escapes terminal control sequences and all other control or
// format runes. It intentionally renders line breaks visibly instead of
// returning a multi-line diagnostic. U+2028 and U+2029 are escaped as well;
// although they are Unicode separators rather than control characters, many
// log viewers treat them as line breaks.
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

// safeError is kept in this package for the small generic wrapper used by
// callers that only need structural redaction. CLI-specific wrappers live in
// internal/cli so they can intercept errors.As for *CLIError.
type safeError struct {
	err      error
	redactor *Redactor
}

// DiagnosticRedactor exposes the context to higher-level safe wrappers so
// they can compose instead of dropping an earlier wait or public boundary.
func (e *safeError) DiagnosticRedactor() *Redactor { return e.redactor }

// Wrap returns an error whose Error method is safe for terminal and log
// output while retaining the original error in its unwrap chain.
func Wrap(err error) error { return WithRedactor(err, NewRedactor()) }

// WithRedactor is the configurable form of Wrap.
func WithRedactor(err error, r *Redactor) error {
	if err == nil {
		return nil
	}
	if r == nil {
		r = NewRedactor()
	}
	r = r.WithForce()
	if existing, ok := err.(*safeError); ok {
		return &safeError{err: existing.err, redactor: existing.redactor.Compose(r)}
	}
	combined := r
	for _, existing := range redactorsIn(err) {
		combined = existing.Compose(combined)
	}
	return &safeError{err: err, redactor: combined}
}

func redactorsIn(err error) []*Redactor {
	var redactors []*Redactor
	var walk func(error)
	walk = func(current error) {
		if current == nil {
			return
		}
		if provider, ok := current.(interface{ DiagnosticRedactor() *Redactor }); ok {
			redactors = append(redactors, provider.DiagnosticRedactor())
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				walk(child)
			}
			return
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			walk(wrapped.Unwrap())
		}
	}
	walk(err)
	return redactors
}

func (e *safeError) Error() string { return e.redactor.Text(e.err.Error()) }
func (e *safeError) Unwrap() error { return e.err }

func (e *safeError) As(target any) bool { return e.as(target, e.redactor) }

func (e *safeError) AsRedacted(target any, r *Redactor) bool {
	return e.as(target, e.redactor.Compose(r))
}

func (e *safeError) as(target any, r *Redactor) bool {
	var walk func(error) bool
	walk = func(current error) bool {
		if current == nil {
			return false
		}
		if provider, ok := current.(interface {
			AsRedacted(any, *Redactor) bool
		}); ok && provider.AsRedacted(target, r) {
			return true
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				if walk(child) {
					return true
				}
			}
			return false
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			return walk(wrapped.Unwrap())
		}
		return false
	}
	return walk(e.err)
}

func (e *safeError) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprint(state, e.Error())
}
