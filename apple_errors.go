package container

import "strings"

// appleTypedError is the small, stable portion of ContainerizationError's
// rendered form. Apple Container 1.3 prints codes and nested causes as
// quoted strings, for example:
//
//	notFound: "container not found: web"
//	internalError: "failed to get container" (cause: "notFound: \"container not found: web\"")
type appleTypedError struct {
	code    string
	message string
	cause   *appleTypedError
}

func parseAppleTypedErrorLine(line string) (appleTypedError, bool) {
	line = strings.TrimSpace(line)
	if line == "" || appleApplicationPrefixed(line) {
		return appleTypedError{}, false
	}
	code, rest, ok := strings.Cut(line, ":")
	if !ok {
		return appleTypedError{}, false
	}
	code = strings.ToLower(strings.TrimSpace(code))
	switch code {
	case "notfound", "internalerror", "exists":
	default:
		return appleTypedError{}, false
	}
	message, rest, ok := parseAppleQuoted(rest)
	if !ok || appleApplicationPrefixed(message) {
		return appleTypedError{}, false
	}
	result := appleTypedError{code: code, message: message}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return result, true
	}
	if !strings.HasPrefix(strings.ToLower(rest), "(cause:") {
		return appleTypedError{}, false
	}
	causeText, tail, ok := parseAppleQuoted(strings.TrimSpace(rest[len("(cause:"):]))
	if !ok || strings.TrimSpace(tail) != ")" {
		return appleTypedError{}, false
	}
	if cause, ok := parseAppleTypedErrorLine(causeText); ok {
		result.cause = &cause
	}
	return result, true
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
	root, ok := parseAppleTypedErrorLine(line)
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
	for _, raw := range strings.Split(branch.stderr, "\n") {
		line := trimCLIErrorWrapper(raw, allowGenericError)
		if appleTypedImageMissingLine(line, target) {
			return true
		}
	}
	return false
}

func appleTypedNameConflictLine(line, target string) bool {
	root, ok := parseAppleTypedErrorLine(line)
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
