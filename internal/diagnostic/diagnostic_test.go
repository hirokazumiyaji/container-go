package diagnostic

import (
	"errors"
	"strings"
	"testing"
)

func TestRedactorHandlesCommonSecretForms(t *testing.T) {
	const (
		password = "password-value"
		token    = "token-value"
	)
	text := `password="` + password + `" Authorization: Bearer ` + token + ` {"api_key":"` + token + `"} https://user:` + password + `@example.test`
	got := NewRedactor().Text(text)
	for _, secret := range []string{password, token} {
		if strings.Contains(got, secret) {
			t.Errorf("Text() = %q, contains %q", got, secret)
		}
	}
	if !strings.Contains(got, Redacted) {
		t.Errorf("Text() = %q, want redaction marker", got)
	}
}

func TestRedactorHandlesStructuralSecretVariants(t *testing.T) {
	const (
		apiKey    = "api-key-value"
		client    = "client-secret-value"
		urlSecret = "url-password-value"
		basic     = "dXNlcjpiYXNpYy1zZWNyZXQ="
		jwt       = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.signature-value"
		cookie    = "cookie-session-value"
		signature = "signature-value"
	)
	text := strings.Join([]string{
		"api_key=" + apiKey,
		"client-secret=" + client,
		"https://:" + urlSecret + "@example.test",
		"Authorization: Basic " + basic,
		"Cookie: sid=" + cookie,
		"Set-Cookie: folded=" + cookie + "\n continued-cookie-value",
		"X-Amz-Signature=" + signature,
		"JWT=" + jwt,
	}, "\n")
	got := NewRedactor().Text(text)
	for _, secret := range []string{apiKey, client, urlSecret, basic, jwt, cookie, signature} {
		if strings.Contains(got, secret) {
			t.Errorf("Text() = %q, contains %q", got, secret)
		}
	}
	if !strings.Contains(got, Redacted) {
		t.Fatalf("Text() = %q, want marker", got)
	}
}

func TestSecretNameMatchingUsesIdentifierComponents(t *testing.T) {
	for _, name := range []string{
		"password", "passwd", "passphrase", "passcode", "pass",
		"secret", "client_secret", "clientSecret", "CLIENT_SECRET",
		"token", "access_token", "refreshToken", "id-token", "csrf_token",
		"api_key", "x-api-key", "apikey", "access_key", "private_key",
		"credential", "credentials", "oauth", "auth", "authentication",
		"authorization", "proxy-authorization", "bearer", "cookie", "set-cookie",
		"session", "sessionId", "JSESSIONID", "connect.sid", "sid",
		"jwt", "signature", "X-Amz-Signature", "sig", "hmac",
		"PGPASSWORD", "DBPASSWORD", "dbpassword", "SECRETKEY", "githubtoken",
		"authtoken", "x_signature", "MYSQL_PWD", "db-pwd", "adminPwd",
		"secrets", "tokens", "passwords", "client_secrets", "access_tokens",
		"AWS_SECRETS", "PHPSESSID", "cookies",
	} {
		if !isSecretName(name) {
			t.Errorf("isSecretName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{
		"signal", "signup", "significant", "passed", "passthrough", "passive",
		"bypass", "compass", "side", "sidebar", "path", "pattern", "patch",
		"pwd", "PWD", "OLDPWD", "stdout", "stderr", "author", "authority", "tokenizer",
		"sessionless", "design", "certificate", "certain", "key", "public_key",
	} {
		if isSecretName(name) {
			t.Errorf("isSecretName(%q) = true, want false", name)
		}
	}
}

func TestRedactorPreservesOrdinaryLogLines(t *testing.T) {
	const input = "signal=SIGTERM\n" +
		"passed=true\n" +
		"stdout: starting server...\n" +
		"stderr: listening on :8080\n" +
		"sidebar=open\n" +
		"path=/app\n" +
		"pwd=/usr/src/app\n" +
		"pattern=*.go\n" +
		"goroutine 1 [running]:\n" +
		"main.main()\n" +
		"\t/app/main.go:12 +0x20\n"
	got := NewRedactor().Text(input)
	for _, want := range []string{
		"signal=SIGTERM",
		"passed=true",
		"stdout: starting server...",
		"stderr: listening on :8080",
		"sidebar=open",
		"path=/app",
		"pwd=/usr/src/app",
		"pattern=*.go",
		"goroutine 1 [running]:",
		"main.main()",
		"/app/main.go:12",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Text() = %q, missing ordinary log text %q", got, want)
		}
	}
}

func TestRedactorMasksUnseparatedSecretNames(t *testing.T) {
	const secret = "hunter2-value"
	for _, name := range []string{"PGPASSWORD", "SECRETKEY", "githubtoken", "MYSQL_PWD"} {
		got := NewRedactor().Text("env " + name + "=" + secret + " ready")
		if strings.Contains(got, secret) {
			t.Errorf("Text(%s=...) = %q, leaked the value", name, got)
		}
	}
}

func TestMultilineSecretExtensionKeepsFollowingLogLines(t *testing.T) {
	const trace = "goroutine 1 [running]:\nmain.main()\n\t/app/main.go:12 +0x20\n"
	for _, line := range []string{"token=abc", "password=abcd", "sid=abcd", "sig=abcd", "auth=abcd", "session=abcd", "stdout=abcd"} {
		got := NewRedactor().Text(line + "\n" + trace)
		for _, want := range []string{"goroutine 1 [running]:", "main.main()", "/app/main.go:12"} {
			if !strings.Contains(got, want) {
				t.Errorf("Text(%q) = %q, swallowed %q", line, got, want)
			}
		}
	}
	got := NewRedactor().Text("token=abc\nlevel=info msg=ready\nDone.\n")
	if !strings.Contains(got, "level=info msg=ready") || !strings.Contains(got, "Done.") {
		t.Errorf("Text() = %q, swallowed a following log line", got)
	}
	for _, input := range []string{
		"password=abcd\nDone\nexiting\n",
		"token=abcd\n--------------------\nok\n",
		"token=abc\ntoken_endpoint=https://example.com/oauth\ngoroutine 1 [running]:\n",
	} {
		got := NewRedactor().Text(input)
		for _, want := range []string{"Done", "exiting", "--------------------", "ok", "token_endpoint=", "goroutine 1 [running]:"} {
			if strings.Contains(input, want) && !strings.Contains(got, want) {
				t.Errorf("Text(%q) = %q, swallowed %q", input, got, want)
			}
		}
	}
}

func TestMultilineSecretExtensionCoversEncodedBody(t *testing.T) {
	const (
		first  = "MIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Ppy1tPf9Cnzj4p4WGeKLs1Pt8Qu"
		second = "KUpRKfFLfRYC9AIKjbJTWit+CqvjWYzvQwECAwEAAQ=="
		next   = "server listening on :8080"
	)
	got := NewRedactor().Text("private_key=" + first + "\n" + second + "\n" + next + "\n")
	if strings.Contains(got, first) || strings.Contains(got, second) {
		t.Errorf("Text() = %q, leaked a wrapped key line", got)
	}
	if !strings.Contains(got, next) {
		t.Errorf("Text() = %q, swallowed the following log line", got)
	}

	const tail = "AQAB0dQ=="
	for _, prefix := range []string{
		"private_key=",
		"private_key: ",
		"level=info private_key=",
	} {
		got = NewRedactor().Text(prefix + first + "\n" + tail + "\n" + next + "\n")
		if strings.Contains(got, tail) {
			t.Errorf("Text(%q...) = %q, leaked the short padded final line", prefix, got)
		}
		if !strings.Contains(got, next) {
			t.Errorf("Text(%q...) = %q, swallowed the following log line", prefix, got)
		}
	}
}

func TestRedactorMasksDottedBearerTokens(t *testing.T) {
	const token = "ya29.a0AfH6SMBxQ9K2mN7pR4sT1vW8"
	got := NewRedactor().Text("probe failed: Bearer " + token + ".")
	want := "probe failed: Bearer " + Redacted + "."
	if got != want {
		t.Fatalf("Text() = %q, want %q", got, want)
	}
}

func TestRedactorHandlesQuotedMultilineAndPEMValues(t *testing.T) {
	const secret = "multiline-secret-value"
	text := "password=\"line-one\nline-two\"\n" +
		"token=line-three\nline-four\n\n" +
		"-----BEGIN PRIVATE KEY-----\n" + secret + "\n-----END PRIVATE KEY-----"
	got := NewRedactor().Text(text)
	if strings.Contains(got, secret) || strings.Contains(got, "line-one") || strings.Contains(got, "line-two") || strings.Contains(got, "line-three") {
		t.Fatalf("Text() = %q, contains multiline secret", got)
	}
	// A plain word after an unquoted credential is a following log line,
	// not a wrapped encoded body.
	if !strings.Contains(got, "line-four") {
		t.Fatalf("Text() = %q, swallowed a plain continuation line", got)
	}
	if !strings.Contains(got, `\n`) {
		t.Fatalf("Text() = %q, want escaped line break", got)
	}
}

func TestRedactorArgsCoversAttachedAndAliasFlags(t *testing.T) {
	args := []string{
		"run", "--env-file", "/tmp/private.env", "--filter", "label=token=filter-secret",
		"--volume", "/private/volume", "-ePASSWORD=attached-secret", "image", "command-secret",
	}
	got := strings.Join(NewRedactor().Args(args), " ")
	for _, secret := range []string{"private.env", "filter-secret", "/private/volume", "attached-secret", "command-secret"} {
		if strings.Contains(got, secret) {
			t.Errorf("Args() = %q, contains %q", got, secret)
		}
	}
	if !strings.Contains(got, Redacted) {
		t.Errorf("Args() = %q, want marker", got)
	}
}

func TestRedactorUsesBoundariesForKnownValues(t *testing.T) {
	got := NewRedactor("prod").Text("production prod productionprod")
	if !strings.Contains(got, "productionprod") {
		t.Fatalf("Text() = %q, replaced a value inside a larger word", got)
	}
	if word := NewRedactor("password").Text("passwordless"); word != "passwordless" {
		t.Fatalf("Text() = %q, replaced a long value inside a larger word", word)
	}
	if short := NewRedactor("abc").Text("abc"); short != "abc" {
		t.Fatalf("Text() = %q, replaced a short boundary value", short)
	}
	if !strings.Contains(got, Redacted) {
		t.Fatalf("Text() = %q, want standalone value redacted", got)
	}
	if context := NewContextRedactor("abc").Text("xxabcxx"); strings.Contains(context, "abc") {
		t.Fatalf("context Text() = %q, want adjacent explicit value redacted", context)
	}
	if ordinary := NewRedactor().Text("tokenizer and production"); ordinary != "tokenizer and production" {
		t.Fatalf("structural Text() = %q, want ordinary words preserved", ordinary)
	}
	if shaped := NewRedactor().Text("api-key"); shaped != Redacted {
		t.Fatalf("structural Text() = %q, want short structured secret redacted", shaped)
	}
}

func TestRedactorsComposeWithoutMutation(t *testing.T) {
	first := NewRedactor("first-secret")
	second := NewRedactor("second-secret")
	combined := first.Compose(second)
	got := combined.Text("first-secret second-secret")
	if strings.Contains(got, "first-secret") || strings.Contains(got, "second-secret") {
		t.Fatalf("composed Text() = %q", got)
	}
	if strings.Contains(first.Text("first-secret"), "first-secret") {
		t.Fatal("Compose mutated the first redactor")
	}
	if strings.Contains(second.Text("second-secret"), "second-secret") {
		t.Fatal("Compose mutated the second redactor")
	}
}

func TestSanitizeEscapesTerminalSequencesAndLineBreaks(t *testing.T) {
	got := Sanitize("before\x1b[31mred\x1b[0m\r\nafter\u2028paragraph\u2029end")
	if strings.ContainsAny(got, "\x1b\r\n\u2028\u2029") {
		t.Fatalf("Sanitize() = %q, contains terminal controls", got)
	}
	if !strings.Contains(got, `\x1b`) || !strings.Contains(got, `\r\n`) || !strings.Contains(got, `\u2028`) || !strings.Contains(got, `\u2029`) {
		t.Fatalf("Sanitize() = %q, want visible escapes", got)
	}
}

func TestStreamRedactorCarriesSecretAcrossChunks(t *testing.T) {
	const secret = "stream-boundary-secret-value"
	r := NewContextRedactor(secret)
	stream := newStreamProcessor(r, 1024*1024, nil)
	first := strings.Repeat("x", DefaultStreamOverlap-8) + "password=" + secret[:len(secret)/2]
	second := secret[len(secret)/2:] + strings.Repeat("y", DefaultStreamOverlap)
	if err := stream.write([]byte(first)); err != nil {
		t.Fatal(err)
	}
	if err := stream.write([]byte(second)); err != nil {
		t.Fatal(err)
	}
	if err := stream.close(); err != nil {
		t.Fatal(err)
	}
	got := string(stream.output)
	if strings.Contains(got, secret) || strings.Contains(got, secret[:len(secret)/2]) || strings.Contains(got, secret[len(secret)/2:]) {
		t.Fatalf("stream = %q, contains split secret", got)
	}
	if !strings.Contains(got, Redacted) {
		t.Fatalf("stream = %q, want redaction marker", got)
	}
}

func TestStreamRedactorDoesNotSplitLongStructuralValue(t *testing.T) {
	secret := strings.Repeat("b", DefaultStreamOverlap+1024)
	stream := newStreamProcessor(NewRedactor(), 2*1024*1024, nil)
	_ = stream.write([]byte("Authorization: Basic " + secret[:len(secret)/2]))
	_ = stream.write([]byte(secret[len(secret)/2:] + "\n"))
	_ = stream.close()
	got := string(stream.output)
	if strings.Contains(got, secret) || strings.Contains(got, secret[:len(secret)/2]) || strings.Contains(got, secret[len(secret)/2:]) || !strings.Contains(got, Redacted) {
		t.Fatalf("stream leaked a long structural value: len=%d", len(got))
	}
}

func TestStreamRedactorBoundsOutputAndLargeValues(t *testing.T) {
	secret := strings.Repeat("s", 4096)
	stream := newStreamProcessor(NewContextRedactor(secret), 32, nil)
	_ = stream.write([]byte("password=" + secret + strings.Repeat("z", 128*1024)))
	_ = stream.close()
	if got := string(stream.output); len(got) > 32 || strings.Contains(got, secret) {
		t.Fatalf("bounded stream = %q (len %d), want <=32 without secret", got, len(got))
	}
}

func TestRedactTailIsBoundedAndPreservesReadError(t *testing.T) {
	wantErr := errors.New("terminal read error")
	input := &errorAfterReader{data: []byte(strings.Repeat("a", 4096)), err: wantErr}
	got, err := NewRedactor().RedactTail(input, 64)
	if !errors.Is(err, wantErr) {
		t.Fatalf("RedactTail error = %v, want %v", err, wantErr)
	}
	if len(got) > 64 || !strings.Contains(got, "a") {
		t.Fatalf("tail = %q, want bounded terminal data", got)
	}
}

func TestRedactorHandlesCookieMapsAndCompactTokens(t *testing.T) {
	const (
		cookieName  = "session"
		cookieValue = "json-cookie-secret"
		jws         = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.c2lnbmF0dXJl"
		jwe         = "eyJhbGciOiJkaXIiLCJlbmMiOiJBMjU2Q0JDLUhTMjU2In0..aXY.Y2lwaGVy.dGFn"
	)
	text := `prefix {"cookie":{"` + cookieName + `":"` + cookieValue + `"},"cookies":{"theme":"dark"}} ` + jws + ` ` + jwe + ` foo.bar.baz suffix`
	got := NewRedactor().Text(text)
	for _, secret := range []string{cookieName, cookieValue, jws, jwe} {
		if strings.Contains(got, secret) {
			t.Errorf("Text() = %q, contains %q", got, secret)
		}
	}
	if !strings.Contains(got, "foo.bar.baz") || !strings.Contains(got, "prefix") || !strings.Contains(got, "suffix") {
		t.Fatalf("Text() = %q, want non-token context preserved", got)
	}
}

func TestRedactorDoesNotPartiallyMatchCompactTokens(t *testing.T) {
	const token = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.c2lnbmF0dXJl"
	for _, text := range []string{
		"prefix " + token + " suffix",
		"prefix " + token + ".",
		"prefix eyJhbGciOiJub25lIn0.. suffix",
	} {
		got := NewRedactor().Text(text)
		if strings.Contains(got, token) {
			t.Fatalf("Text() = %q, contains complete token", got)
		}
	}
	for _, text := range []string{
		"prefix" + token + "suffix",
		"prefix." + token + ".suffix",
		"foo.bar.baz",
	} {
		if got := NewRedactor().Text(text); got != text {
			t.Fatalf("Text() = %q, want maximal dotted run preserved as %q", got, text)
		}
	}
}

func TestHashedRedactorDoesNotRetainPlaintext(t *testing.T) {
	const secret = "hashed-secret-value"
	r := NewHashedContextRedactor(secret)
	if len(r.source) != 0 || len(r.values) != 0 || len(r.hashes) == 0 {
		t.Fatalf("hashed redactor retained plaintext fields: source=%v values=%v hashes=%d", r.source, r.values, len(r.hashes))
	}
	if got := r.Text("prefix" + secret + "suffix"); strings.Contains(got, secret) {
		t.Fatalf("Text() = %q, contains secret", got)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if got := r.Text("prefixplaintextsuffix"); got != "prefixplaintextsuffix" {
		t.Fatalf("closed Text() = %q, want structural-only behavior", got)
	}
}

type errorAfterReader struct {
	data []byte
	err  error
	done bool
}

func (r *errorAfterReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.data), nil
	}
	return 0, r.err
}

func TestComposedRedactorsProtectTextAndStreams(t *testing.T) {
	base := NewHashedContextRedactor("xy", "abcdef")
	redactors := []*Redactor{
		base.WithValues("abcdeg"),
		base.Compose(NewHashedRedactor("abcdeg")),
		NewHashedRedactor("xy", "abcdef", "abcdeg").WithForce(),
	}
	const input = "(xy) abcdef abcdeg"
	const want = "([REDACTED]) [REDACTED] [REDACTED]"
	for _, r := range redactors {
		for range 2 {
			if got := r.Text(input); got != want {
				t.Fatalf("Text() = %q, want %q", got, want)
			}
			stream := newStreamProcessor(r, 1024, nil)
			for _, chunk := range []string{"(x", "y) abc", "def abcd", "eg"} {
				if err := stream.write([]byte(chunk)); err != nil {
					t.Fatal(err)
				}
			}
			if err := stream.close(); err != nil {
				t.Fatal(err)
			}
			if got := string(stream.output); got != want {
				t.Fatalf("stream = %q, want %q", got, want)
			}
		}
	}
	if err := base.Close(); err != nil {
		t.Fatal(err)
	}
	if got := base.Text(input); got != input {
		t.Fatalf("closed Text() = %q, want %q", got, input)
	}
}

func TestStreamRetainsExplicitValuesAfterSourceClose(t *testing.T) {
	const secret = "explicit-stream-context"
	r := NewHashedContextRedactor(secret)
	stream := newStreamProcessor(r, 0, nil)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stream.write([]byte(secret)); err != nil {
		t.Fatal(err)
	}
	if err := stream.close(); err != nil {
		t.Fatal(err)
	}
	if got := string(stream.output); got != Redacted {
		t.Fatalf("stream = %q, want %q", got, Redacted)
	}
}

func TestHashedWithValuesDoesNotRetainPlaintext(t *testing.T) {
	r := NewHashedContextRedactor("first").WithValues("second")
	if len(r.source) != 0 || len(r.values) != 0 {
		t.Fatal("hashed composition retained plaintext")
	}
	if got := r.Text("first second"); got != Redacted+" "+Redacted {
		t.Fatalf("Text = %q", got)
	}
}
