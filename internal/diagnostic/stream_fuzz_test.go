package diagnostic

import (
	"strings"
	"testing"
)

const (
	fuzzStreamSecret = "fuzz-stream-secret-value"
	fuzzStreamToken  = "fuzz-stream-token-value"
)

var fuzzStreamSecrets = []string{fuzzStreamSecret, fuzzStreamToken}

type streamRedactorSeed struct {
	name       string
	input      string
	structural []string
}

// streamRedactorFuzzSeeds covers known values, structural secret forms, and
// malformed input (truncated PEM, unterminated quotes, invalid UTF-8, control
// bytes). The fuzz target runs these under ordinary `go test`; a timed fuzz
// job is not part of CI.
var streamRedactorFuzzSeeds = []streamRedactorSeed{
	{name: "empty"},
	{name: "ordinary line", input: "creating container...\n"},
	{name: "known value", input: "prefix " + fuzzStreamSecret + " suffix\n"},
	{
		name:  "known value across chunk boundary",
		input: strings.Repeat("x", 4093) + fuzzStreamSecret + "\n",
	},
	{
		name:       "password assignment",
		input:      `password="` + fuzzStreamSecret + `"` + "\n",
		structural: []string{fuzzStreamSecret},
	},
	{
		name:       "bearer token",
		input:      "Authorization: Bearer " + fuzzStreamToken + "\n",
		structural: []string{fuzzStreamToken},
	},
	{
		name:       "url credentials",
		input:      "https://user:" + fuzzStreamSecret + "@example.test/path\n",
		structural: []string{fuzzStreamSecret},
	},
	{
		name:       "cookie object",
		input:      `{"cookie":{"session":"` + fuzzStreamSecret + `"}}`,
		structural: []string{fuzzStreamSecret},
	},
	{
		name:       "json api key",
		input:      `{"api_key":"` + fuzzStreamSecret + `","note":"keep"}`,
		structural: []string{fuzzStreamSecret},
	},
	{
		name:       "private key",
		input:      "-----BEGIN PRIVATE KEY-----\n" + fuzzStreamSecret + "\n-----END PRIVATE KEY-----\n",
		structural: []string{fuzzStreamSecret},
	},
	{name: "truncated PEM", input: "-----BEGIN PRIVATE KEY-----\n" + fuzzStreamSecret + "\n"},
	{name: "unterminated quote", input: `password="` + fuzzStreamSecret},
	{
		name:  "invalid UTF-8 and controls",
		input: string([]byte{0xff, 0x00, 0x1b, '[', '3', '1', 'm', '\r', '\n'}) + fuzzStreamSecret,
	},
	{
		name:       "large structural line",
		input:      "Authorization: Basic " + strings.Repeat("b", 8192) + "\n",
		structural: []string{strings.Repeat("b", 256)},
	},
}

// TestStreamRedactorSeedCorpus makes the fuzz seeds named table tests. The
// known-value subtest proves an explicit secret disappears even when it is
// embedded in other text; the structural subtest proves the regex/state
// machine rules redact the shapes they claim to cover.
func TestStreamRedactorSeedCorpus(t *testing.T) {
	for _, seed := range streamRedactorFuzzSeeds {
		t.Run(seed.name, func(t *testing.T) {
			t.Run("known values", func(t *testing.T) {
				r := NewContextRedactor(fuzzStreamSecrets...)
				checkStreamRedactorInvariants(t, r, seed.input, fuzzStreamSecrets)
			})
			t.Run("structural", func(t *testing.T) {
				checkStreamRedactorInvariants(t, NewRedactor(), seed.input, seed.structural)
			})
		})
	}
}

func FuzzStreamRedactor(f *testing.F) {
	for _, seed := range streamRedactorFuzzSeeds {
		f.Add(seed.input)
	}
	f.Fuzz(func(t *testing.T, input string) {
		checkStreamRedactorInvariants(t, NewContextRedactor(fuzzStreamSecrets...), input, fuzzStreamSecrets)
		checkStreamRedactorInvariants(t, NewRedactor(), input, nil)
	})
}

// checkStreamRedactorInvariants compares the incremental redactor with the
// one-shot path, checks that requested values never survive, and requires a
// second pass to remain safe. The one-shot path returns the whole input when
// it fits in the streaming window, so any difference means a value was split
// across writes or a structural value was cut during streaming.
func checkStreamRedactorInvariants(t *testing.T, r *Redactor, input string, mustRedact []string) {
	t.Helper()
	if len(input) > MaxStreamOverlap {
		t.Skip("input exceeds the streaming window")
	}

	want := r.Text(input)
	stream := r.NewStream(0)
	const chunkSize = 4096
	for start := 0; start < len(input); start += chunkSize {
		end := start + chunkSize
		if end > len(input) {
			end = len(input)
		}
		if _, err := stream.Write([]byte(input[start:end])); err != nil {
			t.Fatalf("stream.Write: %v", err)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("stream.Close: %v", err)
	}
	got := stream.String()
	if got != want {
		t.Fatalf("streamed redaction = %q, want one-shot Text() = %q", got, want)
	}
	assertRedacted(t, got, mustRedact)
	if strings.ContainsAny(got, "\x00\r\n\x1b") {
		t.Errorf("streamed redaction left terminal controls in %q", got)
	}

	// A second pass must not reveal a requested value or a raw control even
	// when byte-for-byte idempotence does not apply to the sanitized text.
	again := r.Text(got)
	assertRedacted(t, again, mustRedact)
	if strings.ContainsAny(again, "\x00\r\n\x1b") {
		t.Errorf("second redaction pass left terminal controls in %q", again)
	}

	bounded := r.RedactBounded(input, 256)
	if len(bounded) > 256 {
		t.Errorf("RedactBounded length = %d, want <= 256", len(bounded))
	}
	assertRedacted(t, bounded, mustRedact)
}

// TestStreamRedactorIdempotentSeeds pins the forms where repeating Text must
// be a no-op. They deliberately have no trailing line break: Sanitize renders
// a line break as the literal text \n, and a second structural pass may treat
// that escape as part of a following value. The fuzz target above therefore
// asserts second-pass safety, not byte equality, for arbitrary input.
func TestStreamRedactorIdempotentSeeds(t *testing.T) {
	inputs := []string{
		"password=" + fuzzStreamSecret,
		`password="` + fuzzStreamSecret + `"`,
		"Authorization: Bearer " + fuzzStreamToken,
		"https://user:" + fuzzStreamSecret + "@example.test/path",
		`{"api_key":"` + fuzzStreamSecret + `"}`,
		`{"cookie":{"session":"` + fuzzStreamSecret + `"}}`,
		"-----BEGIN PRIVATE KEY-----\n" + fuzzStreamSecret + "\n-----END PRIVATE KEY-----",
		"prefix " + fuzzStreamSecret + " suffix",
	}
	redactors := []*Redactor{NewRedactor(), NewContextRedactor(fuzzStreamSecrets...)}
	for _, input := range inputs {
		for _, r := range redactors {
			got := r.Text(input)
			if again := r.Text(got); again != got {
				t.Errorf("redaction is not idempotent: %q then %q", got, again)
			}
		}
	}
}

func assertRedacted(t *testing.T, got string, mustRedact []string) {
	t.Helper()
	for _, secret := range mustRedact {
		if strings.Contains(got, secret) {
			t.Errorf("redaction output %q contains %q", got, secret)
		}
	}
}
