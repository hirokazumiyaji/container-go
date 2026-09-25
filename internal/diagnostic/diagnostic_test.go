package diagnostic

import (
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
