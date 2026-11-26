package diagnostic

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
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
	basicRE         = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9])(Basic|Bearer)([ \t]+)([A-Za-z0-9+/=._-]+)`)
	secretShapedRE  = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_-])[A-Za-z0-9_-]*(?:password|passwd|secret|token|api[-_.]?key|access[-_.]?key|private[-_.]?key|client[-_.]?secret|credential|authorization|signature|jwt)[A-Za-z0-9_-]*($|[^A-Za-z0-9_-])`)
	pemRE           = regexp.MustCompile(`(?s)-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----`)
	urlCredentialRE = regexp.MustCompile(`(?i)((?:[a-z][a-z0-9+.-]*://|//))([^/\s@]*)(@)`)
)

func redactCookieObjects(s string) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(s); {
		if !isCookieKeyAt(s, i) {
			i++
			continue
		}
		keyEnd := i + len("cookie")
		if keyEnd < len(s) && (s[keyEnd] == 's' || s[keyEnd] == 'S') {
			keyEnd++
		}
		if keyEnd < len(s) && (s[keyEnd] == '\'' || s[keyEnd] == '"') {
			keyEnd++
		}
		j := keyEnd
		for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\r' || s[j] == '\n') {
			j++
		}
		if j >= len(s) || s[j] != ':' {
			i = keyEnd
			continue
		}
		j++
		for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\r' || s[j] == '\n') {
			j++
		}
		if j >= len(s) || s[j] != '{' {
			i = keyEnd
			continue
		}
		end := matchingBrace(s, j)
		if end < 0 {
			i = keyEnd
			continue
		}
		b.WriteString(s[last : j+1])
		b.WriteString(Redacted)
		b.WriteByte('}')
		last = end + 1
		i = last
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

func isCookieKeyAt(s string, i int) bool {
	if i > 0 && isNameByte(s[i-1]) {
		return false
	}
	if i+len("cookie") > len(s) || !strings.EqualFold(s[i:i+len("cookie")], "cookie") {
		return false
	}
	return true
}

func matchingBrace(s string, start int) int {
	depth := 0
	quoted := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if quoted {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		switch c {
		case '"':
			quoted = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
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
		if len(parts) != 5 {
			return match
		}
		// A trailing dot is usually sentence punctuation, not part of the
		// token. Keep it outside the replacement, and apply the minimum
		// length to the token itself.
		token := strings.TrimRight(parts[4], ".")
		if len(token) < minimumTokenLength {
			return match
		}
		return parts[1] + parts[2] + parts[3] + Redacted + parts[4][len(token):]
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
	last := 0
	replaced := false
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
			if isMultilineSecretName(name) {
				valueEnd = extendMultilineSecret(s, valueStart, valueEnd)
			}
			if !replaced {
				b.Grow(len(s))
				replaced = true
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
	if !replaced {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// extendMultilineSecret continues a credential value only across lines that
// look like a wrapped base64, base64url, or PEM body, so a following log message or stack
// trace stays visible.
func extendMultilineSecret(s string, valueStart, end int) int {
	prevEncoded := isEncodedSecretLine(strings.TrimSpace(s[valueStart:end]))
	pos := end
	for lines := 0; lines < 64 && pos < len(s); lines++ {
		switch {
		case strings.HasPrefix(s[pos:], "\r\n"):
			pos += 2
		case s[pos] == '\r' || s[pos] == '\n':
			pos++
		default:
			return end
		}
		lineEnd := pos
		for lineEnd < len(s) && s[lineEnd] != '\r' && s[lineEnd] != '\n' {
			lineEnd++
		}
		line := strings.TrimSpace(s[pos:lineEnd])
		if !isEncodedSecretLine(line) {
			// A wrapped body commonly ends with a short padded fragment;
			// accept one only directly after an encoded line.
			if prevEncoded && isFinalEncodedSecretLine(line) {
				end = lineEnd
			}
			break
		}
		end = lineEnd
		pos = lineEnd
		prevEncoded = true
	}
	return end
}

// isFinalEncodedSecretLine recognizes the short last line of a wrapped
// base64 body. Padding keeps ordinary words such as Done and ok visible.
func isFinalEncodedSecretLine(line string) bool {
	if line == "" || len(line) >= 16 || !strings.HasSuffix(line, "=") {
		return false
	}
	body := strings.TrimRight(line, "=")
	if body == "" {
		return false
	}
	for i := 0; i < len(body); i++ {
		if c := body[i]; !isBase64URLByte(c) && c != '+' && c != '/' {
			return false
		}
	}
	return true
}

func isEncodedSecretLine(line string) bool {
	if line == "" {
		return false
	}
	// PEM headers are unambiguous. Other lines must be long enough to be a
	// wrapped body; a single ordinary word such as "Done" or "exiting" is
	// not a continuation.
	if strings.HasPrefix(line, "-----BEGIN ") || strings.HasPrefix(line, "-----END ") {
		return true
	}
	if len(line) < 16 {
		return false
	}
	body := strings.TrimRight(line, "=")
	if body == "" || len(line)-len(body) > 2 {
		return false
	}
	hasAlphaNumeric := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		if !isBase64URLByte(c) && c != '+' && c != '/' {
			return false
		}
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			hasAlphaNumeric = true
		}
	}
	return hasAlphaNumeric
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

// redactJWT replaces complete compact JWS/JWE tokens. It scans maximal
// base64url/dot runs instead of matching an eyJ prefix, so a valid token is
// never partially matched and an arbitrary dotted word is not overmatched.
func redactJWT(s string) string {
	var b strings.Builder
	last := 0
	replaced := false
	for i := 0; i < len(s); {
		if !isCompactTokenByte(s[i]) {
			i++
			continue
		}
		start := i
		for i < len(s) && isCompactTokenByte(s[i]) {
			i++
		}
		token := s[start:i]
		replacement := Redacted
		if !isCompactJWSOrJWE(token) {
			cut, ok := compactTokenBeforeTrailingDots(token)
			if !ok {
				continue
			}
			replacement += token[cut:]
		}
		if !replaced {
			b.Grow(len(s))
			replaced = true
		}
		b.WriteString(s[last:start])
		b.WriteString(replacement)
		last = i
	}
	if !replaced {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

func isCompactTokenByte(c byte) bool {
	return isBase64URLByte(c) || c == '.'
}

func isBase64URLByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') || c == '_' || c == '-'
}

func compactTokenBeforeTrailingDots(token string) (int, bool) {
	for cut := len(token) - 1; cut > 0; cut-- {
		if token[cut] != '.' {
			continue
		}
		if isCompactJWSOrJWE(token[:cut]) {
			return cut, true
		}
	}
	return 0, false
}

func isCompactJWSOrJWE(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 && len(parts) != 5 {
		return false
	}
	for i, part := range parts {
		if part == "" {
			// A JWS may have an empty payload or an unsecured empty
			// signature. A JWE may have an empty encrypted-key segment;
			// all of its other segments are required to be present.
			if (len(parts) == 3 && i != 1 && i != 2) || (len(parts) == 5 && i != 1) {
				return false
			}
			continue
		}
		for i := 0; i < len(part); i++ {
			if !isBase64URLByte(part[i]) {
				return false
			}
		}
	}
	header, ok := decodeCompactJSON(parts[0])
	if !ok {
		return false
	}
	if len(parts) == 3 {
		_, ok = header["alg"]
		return ok
	}
	_, hasAlg := header["alg"]
	_, hasEnc := header["enc"]
	return hasAlg && hasEnc
}

func decodeCompactJSON(segment string) (map[string]any, bool) {
	if segment == "" {
		return nil, false
	}
	data, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		data, err = base64.URLEncoding.DecodeString(segment)
		if err != nil {
			return nil, false
		}
	}
	var header map[string]any
	if err := json.Unmarshal(data, &header); err != nil || header == nil {
		return nil, false
	}
	return header, true
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

// strongSecretMarkers are long enough to be matched as a suffix of an
// identifier component, which keeps unseparated names such as PGPASSWORD,
// SECRETKEY, and githubtoken secret without matching words like tokenizer.
var strongSecretMarkers = []string{
	"password", "passwords", "passwd", "passwds", "passphrase", "passcode",
	"secret", "secrets", "secretkey", "token", "apikey", "accesskey",
	"privatekey", "credential", "credentials", "signature", "signatures",
}

// componentSecretMarkers are short or common words that only mark a secret as
// a complete component, so signal, passed, sidebar, author, and sessionless
// stay ordinary.
var componentSecretMarkers = map[string]struct{}{
	"pass": {}, "oauth": {}, "auth": {}, "authentication": {}, "authorization": {},
	"bearer": {}, "cookie": {}, "cookies": {}, "session": {}, "sessionid": {},
	"connectsid": {}, "jsessionid": {}, "phpsessid": {}, "sid": {}, "jwt": {},
	"tokens": {}, "sig": {}, "hmac": {},
}

// IsSecretName reports whether a field name marks its value as a secret.
func IsSecretName(name string) bool { return isSecretName(name) }

func isSecretName(name string) bool {
	tokens := identifierTokens(name)
	// A bare pwd is the working directory; a qualified one such as
	// MYSQL_PWD is a password.
	if len(tokens) > 1 && tokens[len(tokens)-1] == "pwd" {
		return true
	}
	for _, window := range identifierWindows(tokens) {
		if _, ok := componentSecretMarkers[window]; ok || hasStrongSecretSuffix(window) {
			return true
		}
	}
	return false
}

// isMultilineSecretName limits value continuation to credential-shaped keys;
// broad markers such as auth, sid, and session never consume following lines.
func isMultilineSecretName(name string) bool {
	for _, window := range identifierWindows(identifierTokens(name)) {
		if hasStrongSecretSuffix(window) {
			return true
		}
	}
	return false
}

func hasStrongSecretSuffix(window string) bool {
	for _, marker := range strongSecretMarkers {
		if strings.HasSuffix(window, marker) {
			return true
		}
	}
	return false
}

// identifierWindows joins up to three adjacent components without their
// separator, so api_key, x-api-key, and apikey all reach the same marker.
func identifierWindows(tokens []string) []string {
	var windows []string
	for start := range tokens {
		for end := start + 1; end <= len(tokens) && end-start <= 3; end++ {
			windows = append(windows, strings.Join(tokens[start:end], ""))
		}
	}
	return windows
}

// identifierTokens splits a field name into lower-case word components.
// Separators other than letters and digits end a component, and camelCase
// and digit boundaries start a new one.
func identifierTokens(name string) []string {
	var tokens []string
	for i := 0; i < len(name); {
		if !isIdentifierByte(name[i]) {
			i++
			continue
		}
		start := i
		i++
		for i < len(name) && isIdentifierByte(name[i]) {
			current, previous := name[i], name[i-1]
			boundary := isDigitIdentifierByte(current) != isDigitIdentifierByte(previous) ||
				isUpperIdentifierByte(current) && (isLowerIdentifierByte(previous) ||
					isDigitIdentifierByte(previous) ||
					isUpperIdentifierByte(previous) && i+1 < len(name) && isLowerIdentifierByte(name[i+1]))
			if boundary {
				break
			}
			i++
		}
		tokens = append(tokens, strings.ToLower(name[start:i]))
	}
	return tokens
}

func isIdentifierByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || isDigitIdentifierByte(c)
}

func isLowerIdentifierByte(c byte) bool { return c >= 'a' && c <= 'z' }
func isUpperIdentifierByte(c byte) bool { return c >= 'A' && c <= 'Z' }
func isDigitIdentifierByte(c byte) bool { return c >= '0' && c <= '9' }

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

func wordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}
