package bench

import (
	"os"
	"path/filepath"
	"testing"
)

// readFixture loads a file from testdata/.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}
