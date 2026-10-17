package domain

import (
	"bytes"
	"testing"
)

func TestLastNBytes(t *testing.T) {
	data := []byte("hello world")
	if got := LastNBytes(data, 5); !bytes.Equal(got, []byte("world")) {
		t.Errorf("LastNBytes(5) = %q, want world", got)
	}
	if got := LastNBytes(data, 20); !bytes.Equal(got, data) {
		t.Errorf("LastNBytes(20) = %q, want %q", got, data)
	}
	if got := LastNBytes(data, 0); got != nil {
		t.Errorf("LastNBytes(0) = %q, want nil", got)
	}
}
