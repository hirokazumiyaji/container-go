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
		if isNilStrategy(s) {
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
		} else {
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
	}
	collect(strategy)
	sort.Strings(values)
	return values
}

// diagnosticBoundaryProvider is implemented by strategies whose values are
// worth hiding only as whole tokens, such as non-secret query values, so
// a short value like "json" does not erase every occurrence inside words.
type diagnosticBoundaryProvider interface {
	diagnosticBoundaryValues() []string
}

// DiagnosticBoundaryValues returns the caller-provided values of strategy
// that are redacted only at token boundaries. Composite strategies recurse
// into their children.
func DiagnosticBoundaryValues(strategy Strategy) []string {
	if isNilStrategy(strategy) {
		return nil
	}
	var values []string
	switch s := strategy.(type) {
	case diagnosticBoundaryProvider:
		values = s.diagnosticBoundaryValues()
	case *AllStrategy:
		for _, child := range s.strategies {
			values = append(values, DiagnosticBoundaryValues(child)...)
		}
	case *AnyStrategy:
		for _, child := range s.strategies {
			values = append(values, DiagnosticBoundaryValues(child)...)
		}
	}
	return values
}

func safeStrategyError(err error, strategy Strategy) error {
	if isNilStrategy(strategy) {
		return err
	}
	r := cli.NewHashedContextRedactor(DiagnosticValues(strategy)...).WithBoundaryValues(DiagnosticBoundaryValues(strategy)...)
	return cli.WithRedactor(err, r)
}

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

// authorizationDiagnosticValues registers the credential forms carried by a
// Bearer or Basic Authorization header. The complete header value is
// registered separately; this adds the bare Bearer token and, for Basic, the
// decoded user:password form so a backend that echoes only the token or the
// decoded credentials is still covered.
func authorizationDiagnosticValues(value string) []string {
	scheme, credential, ok := strings.Cut(strings.TrimSpace(value), " ")
	if !ok {
		return nil
	}
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return nil
	}
	if strings.EqualFold(scheme, "bearer") {
		return []string{credential}
	}
	if !strings.EqualFold(scheme, "basic") {
		return nil
	}
	values := []string{credential}
	decoded, err := base64.StdEncoding.DecodeString(credential)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(credential)
	}
	if err != nil {
		return values
	}
	values = append(values, string(decoded))
	if _, password, ok := strings.Cut(string(decoded), ":"); ok && password != "" {
		values = append(values, password)
	}
	return values
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
