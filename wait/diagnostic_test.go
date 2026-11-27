package wait

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestForLogDiagnosticRedactsSecretsAndControlCharacters(t *testing.T) {
	const (
		password = "wait-password-value"
		token    = "wait-token-value"
	)
	for _, pattern := range []string{
		"password=" + password + "\x1b[31m\r\n",
		"token=" + token + "\x1b[0m\r\n",
	} {
		t.Run(pattern, func(t *testing.T) {
			target := newFakeTarget()
			err := ForLog(pattern).WithStartupTimeout(50*time.Millisecond).WithPollInterval(5*time.Millisecond).WaitUntilReady(context.Background(), target)
			if err == nil {
				t.Fatal("want timeout error")
			}
			got := err.Error()
			if strings.Contains(got, password) || strings.Contains(got, token) {
				t.Errorf("error = %q, contains a secret", got)
			}
			if strings.ContainsAny(got, "\x1b\r\n") {
				t.Errorf("error = %q, contains terminal controls", got)
			}
		})
	}
}

func TestForExecDiagnosticRedactsBareCommandSecrets(t *testing.T) {
	const secret = "bare-command-token-value"
	target := newFakeTarget()
	target.execErr = errors.New("backend rejected command")
	err := ForExec([]string{"tool", secret}).WithStartupTimeout(50*time.Millisecond).WithPollInterval(5*time.Millisecond).WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want exec timeout error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %q, contains command secret", err)
	}
}

func TestForLogDiagnosticSanitizesFollowError(t *testing.T) {
	target := &errorTarget{err: errors.New("stderr=raw-secret\x1b[31m\r\n")}
	err := ForLog("ready").WithStartupTimeout(time.Second).WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want follow error")
	}
	if strings.Contains(err.Error(), "raw-secret") || strings.ContainsAny(err.Error(), "\x1b\r\n") {
		t.Fatalf("error = %q, want sanitized follow error", err)
	}
}

type errorTarget struct {
	err error
}

func (t *errorTarget) Endpoint(context.Context, string) (string, error) { return "", t.err }
func (t *errorTarget) Running(context.Context) (bool, error)            { return true, t.err }
func (t *errorTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return nil, t.err
}
func (t *errorTarget) ExecCommand(context.Context, []string) (int, error) { return 0, t.err }

func TestDiagnosticValuesCarryCompleteStrategyContext(t *testing.T) {
	const (
		query    = "private-query-token"
		port     = "8443/tcp"
		header   = "private-header-value"
		user     = "private-user"
		password = "private-password"
		pattern  = "private-log-pattern"
		command  = "private-command-value"
	)
	strategy := ForAll(
		ForHTTP("/private-health?token="+query).WithPort(port).WithHeader("X-Private", header).WithBasicAuth(user, password),
		ForLog(pattern),
		ForExec([]string{"tool", command}),
		ForListeningPort("9443/tcp"),
	)
	values := DiagnosticValues(strategy)
	joined := strings.Join(values, "\x00")
	for _, value := range []string{query, header, user, password, pattern, command, "9443/tcp", "cHJpdmF0ZS11c2VyOnByaXZhdGUtcGFzc3dvcmQ"} {
		if !strings.Contains(joined, value) {
			t.Errorf("DiagnosticValues() = %v, missing %q", values, value)
		}
	}
	// The HTTP path, method, and port are request metadata, not secrets.
	for _, value := range []string{"/private-health", port, http.MethodGet} {
		if strings.Contains(joined, value) {
			t.Errorf("DiagnosticValues() = %v, contains request metadata %q", values, value)
		}
	}
}

func TestHTTPDiagnosticValuesDecomposeAuthorization(t *testing.T) {
	const (
		bearerToken = "bearer-token-117"
		basicUser   = "basic-user-117"
		basicPass   = "basic-password-117"
	)
	encoded := base64.StdEncoding.EncodeToString([]byte(basicUser + ":" + basicPass))
	strategy := ForHTTP("/health").
		WithHeader("Authorization", "Bearer "+bearerToken).
		WithHeader("Proxy-Authorization", "Basic "+encoded)
	values := DiagnosticValues(strategy)
	for _, want := range []string{
		"Bearer " + bearerToken,
		bearerToken,
		"Basic " + encoded,
		encoded,
		basicUser + ":" + basicPass,
		basicPass,
	} {
		if !slices.Contains(values, want) {
			t.Errorf("DiagnosticValues() = %v, missing %q", values, want)
		}
	}
}

func TestHTTPWaitErrorRedactsBareAuthorizationToken(t *testing.T) {
	const token = "bare-bearer-token-117"
	target := &errorTarget{err: errors.New("proxy rejected " + token)}
	err := ForHTTP("/health").
		WithHeader("Authorization", "Bearer "+token).
		WithStartupTimeout(20*time.Millisecond).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want HTTP wait error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error = %q, contains the bare bearer token", err)
	}
}

func TestHTTPWaitErrorRedactsCLIErrorFromErrorsAs(t *testing.T) {
	const secret = "wait-cli-error-secret"
	raw := &cli.CLIError{Args: []string{"probe", secret}, Stderr: secret, ExitCode: 1}
	err := ForHTTP("/health").WithStartupTimeout(20*time.Millisecond).WithPollInterval(time.Millisecond).WaitUntilReady(context.Background(), &errorTarget{err: raw})
	if err == nil {
		t.Fatal("want wait error")
	}
	var got *cli.CLIError
	if !errors.As(err, &got) {
		t.Fatal("errors.As did not find *CLIError")
	}
	if strings.Contains(got.Error(), secret) || strings.Contains(got.Stderr, secret) {
		t.Fatalf("errors.As CLIError is unsafe: %q", got.Error())
	}
	if !strings.Contains(raw.RawError(), secret) {
		t.Fatalf("original RawError() = %q, want raw value", raw.RawError())
	}
}

func TestDiagnosticValuesRegisterCookieComponentsAndEncodings(t *testing.T) {
	const (
		name  = "session"
		value = "cookie-value-117"
	)
	strategy := ForHTTP("/").WithHeader("Cookie", name+"="+value+"; theme=dark")
	values := strings.Join(DiagnosticValues(strategy), "\x00")
	for _, want := range []string{
		name,
		value,
		name + "=" + value,
		"cookie-value-117",
		"Y29va2llLXZhbHVlLTExNw==",
	} {
		if !strings.Contains(values, want) {
			t.Errorf("DiagnosticValues() = %v, missing %q", values, want)
		}
	}
}

func TestHTTPDiagnosticValuesRegisterQueryValuesOnly(t *testing.T) {
	const (
		querySecret = "query-secret-117"
		headerValue = "application/json"
	)
	strategy := ForHTTP("/healthz?token="+querySecret).
		WithPort("8080/tcp").
		WithMethod(http.MethodGet).
		WithHeader("Accept", headerValue)
	values := DiagnosticValues(strategy)
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	for _, value := range []string{querySecret, headerValue} {
		if _, ok := set[value]; !ok {
			t.Errorf("DiagnosticValues() = %v, missing %q", values, value)
		}
	}
	for _, metadata := range []string{http.MethodGet, "/healthz", "8080/tcp", "Accept"} {
		if _, ok := set[metadata]; ok {
			t.Errorf("DiagnosticValues() = %v, contains request metadata %q", values, metadata)
		}
	}
}

func TestHTTPWaitErrorRedactsQueryValueButKeepsRequestMetadata(t *testing.T) {
	const querySecret = "query-secret-117"
	target := newFakeTarget()
	target.endpoint = "bad endpoint"
	err := ForHTTP("/healthz?token="+querySecret).
		WithStartupTimeout(20*time.Millisecond).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want HTTP wait error")
	}
	got := err.Error()
	if strings.Contains(got, querySecret) {
		t.Errorf("error = %q, contains query secret", got)
	}
	if !strings.Contains(got, http.MethodGet) || !strings.Contains(got, "/healthz?") {
		t.Errorf("error = %q, want request method and path", got)
	}
}

func TestHTTPDiagnosticValuesSplitQueryBySecretName(t *testing.T) {
	strategy := ForHTTP("/healthz?token=tok-secret-1&api_key=k%2Fsecret&mode=standalone&verbose=1&ready=true;sig=abc%zz#frag")
	secrets := DiagnosticValues(ForAll(strategy))
	boundary := DiagnosticBoundaryValues(ForAll(strategy))
	contains := func(values []string, want string) bool {
		for _, value := range values {
			if value == want {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"tok-secret-1", "k%2Fsecret", "k/secret", "abc%zz"} {
		if !contains(secrets, want) {
			t.Errorf("DiagnosticValues() = %v, missing secret query value %q", secrets, want)
		}
	}
	for _, unwanted := range []string{"standalone", "1", "true", "frag"} {
		if contains(secrets, unwanted) {
			t.Errorf("DiagnosticValues() = %v, contains non-secret query value %q", secrets, unwanted)
		}
	}
	if !contains(boundary, "standalone") {
		t.Errorf("DiagnosticBoundaryValues() = %v, missing %q", boundary, "standalone")
	}
	for _, unwanted := range []string{"1", "true", "tok-secret-1"} {
		if contains(boundary, unwanted) {
			t.Errorf("DiagnosticBoundaryValues() = %v, contains %q", boundary, unwanted)
		}
	}
}

func TestHTTPWaitErrorRedactsNonSecretQueryValuesAtBoundaries(t *testing.T) {
	target := newFakeTarget()
	target.endpoint = "bad endpoint"
	err := ForHTTP("/healthz?verbose=1&mode=standalone").
		WithStartupTimeout(20*time.Millisecond).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want HTTP wait error")
	}
	got := err.Error()
	if strings.Contains(got, "standalone") {
		t.Errorf("error = %q, contains boundary query value", got)
	}
	if !strings.Contains(got, "verbose=1") || !strings.Contains(got, http.MethodGet) {
		t.Errorf("error = %q, want short query value and method visible", got)
	}
	r := cli.NewHashedContextRedactor().WithBoundaryValues(DiagnosticBoundaryValues(ForHTTP("/healthz?mode=standalone"))...)
	if got := r.Text("standalones at 127.0.0.1"); got != "standalones at 127.0.0.1" {
		t.Errorf("Text() = %q, boundary value replaced inside a word", got)
	}
}

func TestHTTPWaitErrorUsesHeaderAndBasicAuthContext(t *testing.T) {
	const (
		path     = "/private-health"
		header   = "private-header-value"
		password = "private-password"
	)
	target := newFakeTarget()
	target.endpoint = "bad endpoint"
	err := ForHTTP(path).
		WithHeader("X-Private", header).
		WithBasicAuth("user", password).
		WithStartupTimeout(20*time.Millisecond).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want HTTP wait error")
	}
	got := err.Error()
	for _, secret := range []string{header, password} {
		if strings.Contains(got, secret) {
			t.Errorf("error = %q, contains %q", got, secret)
		}
	}
	// The request metadata stays visible so the failure identifies the probe.
	if !strings.Contains(got, http.MethodGet) || !strings.Contains(got, path) {
		t.Errorf("error = %q, want request method and path", got)
	}
}

func TestCompositeWaitRejectsTypedNilStrategyWithoutPanic(t *testing.T) {
	var typedNil *LogStrategy
	for name, strategy := range map[string]Strategy{
		"all": ForAll(typedNil),
		"any": ForAny(typedNil),
	} {
		t.Run(name, func(t *testing.T) {
			err := strategy.WaitUntilReady(context.Background(), newFakeTarget())
			if err == nil || !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("error = %v, want ErrInvalidConfiguration", err)
			}
		})
	}
}

func TestDiagnosticHelpersIgnoreTypedNilStrategy(t *testing.T) {
	var (
		httpTypedNil        *HTTPStrategy
		healthcheckTypedNil *HealthcheckStrategy
	)
	if values := DiagnosticValues(httpTypedNil); len(values) != 0 {
		t.Errorf("DiagnosticValues = %v, want none for a typed-nil strategy", values)
	}
	if values := DiagnosticValues(ForAll(ForAny(httpTypedNil))); len(values) != 0 {
		t.Errorf("nested DiagnosticValues = %v, want none for a typed-nil strategy", values)
	}
	if values := DiagnosticBoundaryValues(httpTypedNil); len(values) != 0 {
		t.Errorf("DiagnosticBoundaryValues = %v, want none for a typed-nil strategy", values)
	}
	if values := DiagnosticBoundaryValues(ForAny(httpTypedNil)); len(values) != 0 {
		t.Errorf("nested DiagnosticBoundaryValues = %v, want none for a typed-nil strategy", values)
	}
	if RequiresHealthcheck(httpTypedNil) || RequiresHealthcheck(healthcheckTypedNil) {
		t.Error("typed-nil strategy must not require a healthcheck")
	}
	cause := errors.New("wait failed")
	if err := safeStrategyError(cause, httpTypedNil); err != cause { //nolint:errorlint // identity of the original cause
		t.Errorf("safeStrategyError = %v, want the original cause", err)
	}
}

// panicDiagnosticStrategy fails the test if diagnostics are collected on a
// successful wait.
type panicDiagnosticStrategy struct{}

func (panicDiagnosticStrategy) WaitUntilReady(context.Context, Target) error { return nil }
func (panicDiagnosticStrategy) DiagnosticValues() []string {
	panic("diagnostics collected on the success path")
}

// Every successful WaitUntilReady call, and every successful child of a
// composite strategy, used to collect diagnostic values and build a redactor
// only to wrap a nil error.
func TestSafeErrorsSkipDiagnosticsOnSuccess(t *testing.T) {
	if err := safeStrategyError(nil, panicDiagnosticStrategy{}); err != nil {
		t.Fatalf("safeStrategyError(nil) = %v, want nil", err)
	}
	if err := safeDiagnosticError(nil, "secret"); err != nil {
		t.Fatalf("safeDiagnosticError(nil) = %v, want nil", err)
	}
}
