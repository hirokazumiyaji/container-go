package container

import "strings"

// maxAppleContainerizationDepth bounds cause recursion so a malformed or
// adversarial stderr line cannot exhaust the stack. The CLI nests at most a
// few wrappers in practice.
const maxAppleContainerizationDepth = 8

// appleContainerizationCodes is the complete set of ContainerizationError
// codes the CLI can print. Keeping it explicit rejects arbitrary
// "code: \"message\"" application output that happens to look structured.
var appleContainerizationCodes = map[string]struct{}{
	"unknown":         {},
	"invalidargument": {},
	"internalerror":   {},
	"exists":          {},
	"notfound":        {},
	"cancelled":       {},
	"invalidstate":    {},
	"empty":           {},
	"timeout":         {},
	"unsupported":     {},
	"interrupted":     {},
}

// appleContainerizationError is the small, stable portion of
// ContainerizationError's rendered form. The CLI prints codes and nested
// causes as quoted strings, in two spellings:
//
//	notFound: "container not found: web"
//	internalError: "failed to get container" (cause: "notFound: \"container not found: web\"")
//	internalError: "failed to get container" (cause: "notFound: "container not found: web"")
//
// The first two come from the escape-aware 1.3 renderer; the last keeps
// Swift's unescaped nested quotes. One parser supports both so classifiers
// cannot disagree about the same line.
type appleContainerizationError struct {
	code    string
	message string
	cause   *appleContainerizationError
}

func (e appleContainerizationError) walk(match func(code, message string) bool) bool {
	if match(e.code, e.message) {
		return true
	}
	return e.cause != nil && e.cause.walk(match)
}

// parseAppleContainerizationError parses one rendered ContainerizationError
// line. Messages retain their original case; callers that match wording
// lowercase them explicitly.
func parseAppleContainerizationError(line string) (appleContainerizationError, bool) {
	return parseAppleContainerizationErrorAt(line, 0)
}

func parseAppleContainerizationErrorAt(line string, depth int) (appleContainerizationError, bool) {
	if depth > maxAppleContainerizationDepth {
		return appleContainerizationError{}, false
	}
	line = trimAppleErrorPrefix(line)
	if line == "" || appleApplicationPrefixed(line) {
		return appleContainerizationError{}, false
	}
	code, rest, ok := strings.Cut(line, ":")
	if !ok {
		return appleContainerizationError{}, false
	}
	code = strings.ToLower(strings.TrimSpace(code))
	if _, ok := appleContainerizationCodes[code]; !ok {
		return appleContainerizationError{}, false
	}
	if envelope, ok := parseAppleEscapedEnvelope(code, rest, depth); ok {
		return envelope, true
	}
	return parseAppleUnescapedEnvelope(code, rest, depth)
}

func trimAppleErrorPrefix(line string) string {
	line = strings.TrimSpace(line)
	const prefix = "error:"
	if len(line) >= len(prefix) && strings.EqualFold(line[:len(prefix)], prefix) {
		line = strings.TrimSpace(line[len(prefix):])
	}
	return line
}

// parseAppleEscapedEnvelope handles the escape-aware spelling, where the
// nested cause is a properly quoted string.
func parseAppleEscapedEnvelope(code, rest string, depth int) (appleContainerizationError, bool) {
	message, tail, ok := parseAppleQuoted(rest)
	if !ok || appleApplicationPrefixed(message) {
		return appleContainerizationError{}, false
	}
	result := appleContainerizationError{code: code, message: message}
	tail = strings.TrimSpace(tail)
	if tail == "" {
		return result, true
	}
	const causePrefix = "(cause:"
	if !strings.HasPrefix(strings.ToLower(tail), causePrefix) {
		return appleContainerizationError{}, false
	}
	causeText, after, ok := parseAppleQuoted(strings.TrimSpace(tail[len(causePrefix):]))
	if !ok || strings.TrimSpace(after) != ")" {
		return appleContainerizationError{}, false
	}
	if cause, ok := parseAppleContainerizationErrorAt(causeText, depth+1); ok {
		result.cause = &cause
	}
	return result, true
}

// parseAppleUnescapedEnvelope handles the older spelling, where Swift leaves
// the nested cause's quotes unescaped. The explicit "(cause: " marker keeps
// the unquoted portion bounded.
func parseAppleUnescapedEnvelope(code, rest string, depth int) (appleContainerizationError, bool) {
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, `"`) {
		return appleContainerizationError{}, false
	}
	const causeMarker = `" (cause: "`
	if marker := strings.Index(rest, causeMarker); marker >= 0 {
		message := rest[1:marker]
		if appleApplicationPrefixed(message) {
			return appleContainerizationError{}, false
		}
		causeText := rest[marker+len(causeMarker):]
		if !strings.HasSuffix(causeText, `")`) {
			return appleContainerizationError{}, false
		}
		causeText = strings.TrimSuffix(causeText, `")`)
		result := appleContainerizationError{code: code, message: message}
		if cause, ok := parseAppleContainerizationErrorAt(causeText, depth+1); ok {
			result.cause = &cause
		}
		return result, true
	}
	if !strings.HasSuffix(rest, `"`) {
		return appleContainerizationError{}, false
	}
	message := rest[1 : len(rest)-1]
	if appleApplicationPrefixed(message) {
		return appleContainerizationError{}, false
	}
	return appleContainerizationError{code: code, message: message}, true
}

// lowercaseAppleContainerizationError preserves the historical envelope
// parser contract: wording matches are case-insensitive.
func lowercaseAppleContainerizationError(e appleContainerizationError) appleContainerizationError {
	result := appleContainerizationError{code: strings.ToLower(e.code), message: strings.ToLower(e.message)}
	if e.cause != nil {
		cause := lowercaseAppleContainerizationError(*e.cause)
		result.cause = &cause
	}
	return result
}

func parseAppleQuoted(value string) (string, string, bool) {
	value = strings.TrimSpace(value)
	if len(value) < 2 || value[0] != '"' {
		return "", "", false
	}
	var decoded strings.Builder
	escaped := false
	for i := 1; i < len(value); i++ {
		ch := value[i]
		if escaped {
			if ch == '"' || ch == '\\' {
				decoded.WriteByte(ch)
			} else {
				decoded.WriteByte('\\')
				decoded.WriteByte(ch)
			}
			escaped = false
			continue
		}
		switch ch {
		case '\\':
			escaped = true
		case '"':
			return decoded.String(), value[i+1:], true
		default:
			decoded.WriteByte(ch)
		}
	}
	return "", "", false
}

func appleApplicationPrefixed(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(value, "application:") || strings.HasPrefix(value, "application ")
}

func appleTypedImageMissingLine(line, target string) bool {
	root, ok := parseAppleContainerizationError(line)
	if !ok {
		return false
	}
	for node := &root; node != nil; node = node.cause {
		if node.code != "notfound" && node.code != "internalerror" {
			continue
		}
		message := strings.TrimSpace(node.message)
		lower := strings.ToLower(message)
		if strings.HasPrefix(lower, "image not found:") {
			return cliTargetListExactMatches(message[len("image not found:"):], target)
		}
	}
	return false
}

func hasAppleTypedImageLine(branch cliErrorBranch, target string) bool {
	allowGenericError := genericErrorWrapperAllowed(branch.ctx.err)
	for _, raw := range splitCLIErrorLines(branch.stderr) {
		line := trimCLIErrorWrapper(raw, branch.ctx.err, allowGenericError)
		if appleTypedImageMissingLine(line, target) {
			return true
		}
	}
	return false
}

func appleTypedNameConflictLine(line, target string) bool {
	root, ok := parseAppleContainerizationError(line)
	if !ok {
		return false
	}
	for node := &root; node != nil; node = node.cause {
		if (node.code == "exists" || node.code == "internalerror") && appleNameConflictLine(strings.ToLower(node.message), target) {
			return true
		}
	}
	return false
}
