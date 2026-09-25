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
	for _, part := range strings.Split(value, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, cookieValue, ok := strings.Cut(part, "=")
		name = strings.TrimSpace(name)
		cookieValue = strings.Trim(strings.TrimSpace(cookieValue), "\"")
		if !ok {
			addEncoded(name)
			continue
		}
		addEncoded(name)
		addEncoded(cookieValue)
		addEncoded(name + "=" + cookieValue)
	}
	return values
}
