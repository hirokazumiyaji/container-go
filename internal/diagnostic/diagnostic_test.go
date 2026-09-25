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

func TestRedactorHandlesQuotedMultilineAndPEMValues(t *testing.T) {
	const secret = "multiline-secret-value"
	text := "password=\"line-one\nline-two\"\n" +
		"token=line-three\nline-four\n\n" +
		"-----BEGIN PRIVATE KEY-----\n" + secret + "\n-----END PRIVATE KEY-----"
	got := NewRedactor().Text(text)
	if strings.Contains(got, secret) || strings.Contains(got, "line-one") || strings.Contains(got, "line-two") || strings.Contains(got, "line-three") || strings.Contains(got, "line-four") {
		t.Fatalf("Text() = %q, contains multiline secret", got)
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
	stream := r.NewStream(1024 * 1024)
	first := strings.Repeat("x", DefaultStreamOverlap-8) + "password=" + secret[:len(secret)/2]
	second := secret[len(secret)/2:] + strings.Repeat("y", DefaultStreamOverlap)
	if _, err := stream.Write([]byte(first)); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Write([]byte(second)); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	got := stream.String()
	if strings.Contains(got, secret) || strings.Contains(got, secret[:len(secret)/2]) || strings.Contains(got, secret[len(secret)/2:]) {
		t.Fatalf("stream = %q, contains split secret", got)
	}
	if !strings.Contains(got, Redacted) {
		t.Fatalf("stream = %q, want redaction marker", got)
	}
}

func TestStreamRedactorDoesNotSplitLongStructuralValue(t *testing.T) {
	secret := strings.Repeat("b", DefaultStreamOverlap+1024)
	stream := NewRedactor().NewStream(2 * 1024 * 1024)
	_, _ = stream.Write([]byte("Authorization: Basic " + secret[:len(secret)/2]))
	_, _ = stream.Write([]byte(secret[len(secret)/2:] + "\n"))
	_ = stream.Close()
	got := stream.String()
	if strings.Contains(got, secret) || strings.Contains(got, secret[:len(secret)/2]) || strings.Contains(got, secret[len(secret)/2:]) || !strings.Contains(got, Redacted) {
		t.Fatalf("stream leaked a long structural value: len=%d", len(got))
	}
}

func TestStreamRedactorBoundsOutputAndLargeValues(t *testing.T) {
	secret := strings.Repeat("s", 4096)
	stream := NewContextRedactor(secret).NewStream(32)
	_, _ = stream.Write([]byte("password=" + secret + strings.Repeat("z", 128*1024)))
	_ = stream.Close()
	if got := stream.String(); len(got) > 32 || strings.Contains(got, secret) {
		t.Fatalf("bounded stream = %q (len %d), want <=32 without secret", got, len(got))
	}
}

func TestStreamRedactorRejectsValuesBeyondBoundedWindow(t *testing.T) {
	secret := strings.Repeat("s", MaxStreamOverlap+1)
	stream := NewContextRedactor(secret).NewStream(2 * MaxStreamOverlap)
	if n, err := stream.Write([]byte("before" + secret + "after")); err == nil || n != 0 {
		t.Fatalf("Write() = (%d, %v), want rejected oversized value", n, err)
	}
	if err := stream.Close(); err == nil {
		t.Fatal("Close() = nil, want oversized value error")
	}
	if got := stream.String(); got != "" {
		t.Fatalf("stream = %q, want no output for an unsafe configuration", got)
	}
}

func TestStreamRedactorDropsOversizedMultilineStructuralValue(t *testing.T) {
	first := "password=\"" + strings.Repeat("x", MaxStreamOverlap-32) + "\n"
	second := strings.Repeat("x", streamChunkSize) + "\"\n"
	stream := NewRedactor().NewStream(2 * MaxStreamOverlap)
	if _, err := stream.Write([]byte(first)); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Write([]byte(second)); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if got := stream.String(); strings.Contains(got, strings.Repeat("x", 1024)) {
		t.Fatalf("stream leaked an oversized multiline structural value: len=%d", len(got))
	}
}

func TestStreamRedactorDropsOversizedStructuralLine(t *testing.T) {
	secret := strings.Repeat("s", MaxStreamOverlap+streamChunkSize+1)
	stream := NewRedactor().NewStream(2 * MaxStreamOverlap)
	if n, err := stream.Write([]byte("password=\"" + secret + "\"\nvisible\n")); err != nil || n == 0 {
		t.Fatalf("Write() = (%d, %v)", n, err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	got := stream.String()
	if strings.Contains(got, secret) || strings.Contains(got, secret[:32]) || strings.Contains(got, secret[len(secret)-32:]) {
		t.Fatalf("stream leaked an oversized structural value: len=%d", len(got))
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

func TestExplicitValuesRedactBeforeStructuralParsing(t *testing.T) {
	tests := []struct {
		name   string
		secret string
		text   string
	}{
		{
			name:   "assignment delimiters",
			secret: `semi;colon, space [bracket] "quoted"`,
			text:   "password=semi;colon, space [bracket] \"quoted\"",
		},
		{
			name:   "cookie form",
			secret: "session=abc; theme=dark",
			text:   "cookie session=abc; theme=dark",
		},
		{
			name:   "public marker injection",
			secret: "[REDACTED] forged suffix",
			text:   "token=[REDACTED] forged suffix",
		},
		{
			name:   "single quoted JSON",
			secret: "single quoted, [value]",
			text:   "{'password':'single quoted, [value]'}",
		},
		{
			name:   "escaped single quoted JSON",
			secret: "apostrophe's [value]",
			text:   `{'password':'apostrophe\'s [value]'}`,
		},
		{
			name:   "single quoted cookie JSON",
			secret: "cookie-value with, [brackets]",
			text:   "{'cookie':{'session':'cookie-value with, [brackets]'}}",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NewContextRedactor(tc.secret).Text(tc.text)
			if strings.Contains(got, tc.secret) {
				t.Fatalf("Text() = %q, contains exact configured value %q", got, tc.secret)
			}
		})
	}
}

func TestStructuralRedactionDoesNotTrustPublicRedactionMarker(t *testing.T) {
	const injected = "token=[REDACTED]injected-secret"
	got := NewRedactor().Text(injected)
	if strings.Contains(got, "injected-secret") {
		t.Fatalf("Text() = %q, trusted an input-provided public marker", got)
	}
	if !strings.Contains(got, Redacted) {
		t.Fatalf("Text() = %q, want a redaction", got)
	}
}

func TestRedactorHandlesSingleQuotedJSONAndCookieObjects(t *testing.T) {
	const secret = "single-quoted-json-secret"
	text := `{'password':'` + secret + `','cookie':{'session':'cookie {value} secret'}}`
	got := NewRedactor().Text(text)
	if strings.Contains(got, secret) || strings.Contains(got, "cookie {value} secret") {
		t.Fatalf("Text() = %q, contains single-quoted JSON secret", got)
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
	const (
		token = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.c2lnbmF0dXJl"
		jwe   = "eyJhbGciOiJkaXIiLCJlbmMiOiJBMjU2Q0JDLUhTMjU2In0..aXY.Y2lwaGVy.dGFn"
	)
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
		"foo.bar.baz",
	} {
		if got := NewRedactor().Text(text); got != text {
			t.Fatalf("Text() = %q, want maximal dotted run preserved as %q", got, text)
		}
	}
	if got, want := NewRedactor().Text("trace."+token+".suffix"), "trace."+Redacted+".suffix"; got != want {
		t.Fatalf("Text() = %q, want embedded token redacted as %q", got, want)
	}
	if got, want := NewRedactor().Text("trace."+jwe+".suffix"), "trace."+Redacted+".suffix"; got != want {
		t.Fatalf("Text() = %q, want embedded JWE redacted as %q", got, want)
	}
}

func TestRedactJWTScansRepeatedDottedCandidatesOnce(t *testing.T) {
	const candidates = 10000
	input := strings.Repeat("eyJ.", candidates)
	if got := NewRedactor().Text(input); got != input {
		t.Fatalf("Text() changed %d invalid dotted candidates", candidates)
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
