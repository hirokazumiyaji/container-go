package bench

import (
	"errors"
	"strings"
	"testing"
)

func TestImageRemovalAccepted(t *testing.T) {
	t.Run("tolerates remove error when image is already absent", func(t *testing.T) {
		err := imageRemovalAccepted(
			func(string) (bool, error) { return false, nil },
			"redis:7-alpine",
			errors.New("no such image"),
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("rejects remove error when image remains", func(t *testing.T) {
		err := imageRemovalAccepted(
			func(string) (bool, error) { return true, nil },
			"redis:7-alpine",
			errors.New("image is in use"),
		)
		if err == nil {
			t.Fatal("want error when image is still present")
		}
		if !strings.Contains(err.Error(), "image still present") {
			t.Fatalf("error = %v, want image still present", err)
		}
	})

	t.Run("rejects when existence check fails", func(t *testing.T) {
		err := imageRemovalAccepted(
			func(string) (bool, error) { return false, errors.New("inspect failed") },
			"redis:7-alpine",
			errors.New("remove failed"),
		)
		if err == nil {
			t.Fatal("want error when existence check fails")
		}
		if !strings.Contains(err.Error(), "could not verify absence") {
			t.Fatalf("error = %v, want could not verify absence", err)
		}
	})
}
