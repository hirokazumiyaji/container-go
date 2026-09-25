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
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("Text() = %q, want redaction marker", got)
	}
}

func TestSanitizeEscapesTerminalSequencesAndLineBreaks(t *testing.T) {
	got := Sanitize("before\x1b[31mred\x1b[0m\r\nafter")
	if strings.ContainsAny(got, "\x1b\r\n") {
		t.Fatalf("Sanitize() = %q, contains terminal controls", got)
	}
	if !strings.Contains(got, `\x1b`) || !strings.Contains(got, `\r\n`) {
		t.Fatalf("Sanitize() = %q, want visible escapes", got)
	}
}
