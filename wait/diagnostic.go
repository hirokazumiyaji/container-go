package wait

import (
	"encoding/base64"
	"net/url"
	"sort"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// DiagnosticContext is an optional interface implemented by readiness
// strategies that have values which may be echoed by a container backend or
// its logs. It is deliberately separate from Strategy so existing custom
// strategies remain source-compatible.
type DiagnosticContext interface {
	DiagnosticValues() []string
}

// DiagnosticProvider is the original name for DiagnosticContext.
type DiagnosticProvider = DiagnosticContext

// DiagnosticSecretProvider is accepted as an alternative spelling for
// custom strategies. The built-in strategies implement both interfaces.
type DiagnosticSecretProvider interface {
	DiagnosticSecrets() []string
}

// DiagnosticValues returns a snapshot of all caller-provided values that can
// appear in a wait failure. Composite strategies recurse into their children.
// The result is deduplicated and sorted to make diagnostics deterministic.
func DiagnosticValues(strategy Strategy) []string {
	seen := make(map[string]struct{})
	var values []string
	add := func(value string) {
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	var collect func(Strategy)
	collect = func(s Strategy) {
		if s == nil {
			return
		}
		if provider, ok := s.(DiagnosticProvider); ok {
			for _, value := range provider.DiagnosticValues() {
				add(value)
			}
		} else if provider, ok := s.(DiagnosticSecretProvider); ok {
			for _, value := range provider.DiagnosticSecrets() {
				add(value)
			}
		}
		switch composite := s.(type) {
		case *AllStrategy:
			for _, child := range composite.strategies {
				collect(child)
			}
		case *AnyStrategy:
			for _, child := range composite.strategies {
				collect(child)
			}
		}
	}
	collect(strategy)
	sort.Strings(values)
	return values
}

// DiagnosticSecrets is a descriptive alias for DiagnosticValues.
func DiagnosticSecrets(strategy Strategy) []string { return DiagnosticValues(strategy) }

// SecretValues is a short alias for callers that prefer secret-oriented
// terminology.
func SecretValues(strategy Strategy) []string { return DiagnosticValues(strategy) }

func safeDiagnosticError(err error, values ...string) error {
	return cli.WithRedactor(err, cli.NewHashedContextRedactor(values...))
}

func basicAuthValues(username, password string) []string {
	joined := username + ":" + password
	return []string{
		joined,
		base64.StdEncoding.EncodeToString([]byte(joined)),
		base64.RawStdEncoding.EncodeToString([]byte(joined)),
	}
}

// cookieDiagnosticValues registers each cookie component as well as common
// encoded spellings. Header-level redaction protects the complete header, but
// backend logs often render a JSON/map field with only one cookie component.
func cookieDiagnosticValues(value string) []string {
	seen := make(map[string]struct{})
	var values []string
	add := func(v string) {
		if v == "" {
			return
		}
		if _, ok := seen[v]; ok {
			return
		}
		seen[v] = struct{}{}
		values = append(values, v)
	}
	addEncoded := func(v string) {
		add(v)
		add(url.QueryEscape(v))
		add(url.PathEscape(v))
		if v != "" {
			add(base64.StdEncoding.EncodeToString([]byte(v)))
			add(base64.RawStdEncoding.EncodeToString([]byte(v)))
			add(base64.RawURLEncoding.EncodeToString([]byte(v)))
			add(base64.URLEncoding.EncodeToString([]byte(v)))
		}
	}
	for _, part := range splitCookieParts(value) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, rawCookieValue, ok := strings.Cut(part, "=")
		name = strings.TrimSpace(name)
		rawCookieValue = strings.TrimSpace(rawCookieValue)
		cookieValue := unquoteCookieValue(rawCookieValue)
		if !ok {
			addEncoded(name)
			continue
		}
		addEncoded(name)
		addEncoded(cookieValue)
		addEncoded(name + "=" + cookieValue)
		addEncoded(name + "=" + rawCookieValue)
	}
	return values
}

func splitCookieParts(value string) []string {
	var parts []string
	start := 0
	var quote byte
	escaped := false
	for i := 0; i < len(value); i++ {
		c := value[i]
		if quote != 0 {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case ';', ',':
			parts = append(parts, value[start:i])
			start = i + 1
		}
	}
	return append(parts, value[start:])
}

func unquoteCookieValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 2 || (value[0] != '"' && value[0] != '\'') || value[len(value)-1] != value[0] {
		return value
	}
	inner := value[1 : len(value)-1]
	if !strings.Contains(inner, `\`) {
		return inner
	}
	var b strings.Builder
	b.Grow(len(inner))
	for i := 0; i < len(inner); i++ {
		if inner[i] == '\\' && i+1 < len(inner) {
			i++
		}
		b.WriteByte(inner[i])
	}
	return b.String()
}
