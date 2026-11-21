package domain

import (
	"strings"
	"testing"
)

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		input string
		bytes uint64
	}{
		{"1", 1},
		{"512", 512},
		{"1K", 1 << 10},
		{"64M", 64 << 20},
		{"1G", 1 << 30},
		{"2T", 2 << 40},
		{"1P", MaxByteSize},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			size, err := ParseByteSize(tt.input)
			if err != nil {
				t.Fatalf("ParseByteSize(%q): %v", tt.input, err)
			}
			if size.Bytes() != tt.bytes {
				t.Errorf("Bytes() = %d, want %d", size.Bytes(), tt.bytes)
			}
			if size.String() != tt.input {
				t.Errorf("String() = %q, want %q", size.String(), tt.input)
			}
			if size.IsZero() {
				t.Error("parsed size reports zero")
			}
		})
	}
}

func TestParseByteSizeRejects(t *testing.T) {
	for _, input := range []string{
		"", "0", "-1", "64MB", "1.5G", "1g", "1G\n", "1\x00G", "P", "1P2",
		"18446744073709551615", "18014398509481983K", "2P", strings.Repeat("9", 100),
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := ParseByteSize(input); err == nil {
				t.Fatalf("ParseByteSize(%q) = nil error", input)
			}
		})
	}
}

func TestByteSizeZeroValue(t *testing.T) {
	var size ByteSize
	if !size.IsZero() {
		t.Error("zero ByteSize is not zero")
	}
	if size.Bytes() != 0 || size.String() != "" {
		t.Errorf("zero ByteSize = %d bytes, %q", size.Bytes(), size.String())
	}
}
