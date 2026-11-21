package domain

import (
	"testing"
)

const (
	testCreationID = "0123456789abcdef"
	testOtherID    = "fedcba9876543210"
	testUID        = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testOtherUID   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestVerifyOwnership(t *testing.T) {
	owned := func(overrides map[string]string) map[string]string {
		labels := map[string]string{
			LabelManaged:    "true",
			LabelSession:    "session-a",
			LabelCreation:   testCreationID,
			LabelReuse:      "true",
			LabelReuseGroup: "group-a",
		}
		for key, value := range overrides {
			labels[key] = value
		}
		return labels
	}

	manageOnly := OwnershipExpected{RequireManaged: true}
	reuseOnly := OwnershipExpected{RequireManaged: true, RequireReuse: true}
	full := OwnershipExpected{
		RequireManaged: true,
		RequireReuse:   true,
		ReuseGroup:     "group-a",
		Session:        "session-a",
		Creation:       testCreationID,
	}

	tests := []struct {
		name     string
		expected OwnershipExpected
		observed OwnershipObserved
		want     OwnershipFailure
	}{
		{
			name:     "managed",
			expected: manageOnly,
			observed: OwnershipObserved{Labels: owned(nil)},
		},
		{
			name:     "managed marker missing",
			expected: manageOnly,
			observed: OwnershipObserved{Labels: owned(map[string]string{LabelManaged: "false"})},
			want:     OwnershipFailureManaged,
		},
		{
			name:     "reuse marker missing",
			expected: reuseOnly,
			observed: OwnershipObserved{Labels: owned(map[string]string{LabelReuse: ""})},
			want:     OwnershipFailureReuse,
		},
		{
			name:     "reuse group mismatch",
			expected: full,
			observed: OwnershipObserved{Labels: owned(map[string]string{LabelReuseGroup: "group-b"})},
			want:     OwnershipFailureReuseGroup,
		},
		{
			name:     "reuse group missing",
			expected: full,
			observed: OwnershipObserved{Labels: owned(map[string]string{LabelReuseGroup: ""})},
			want:     OwnershipFailureReuseGroup,
		},
		{
			name:     "session mismatch",
			expected: full,
			observed: OwnershipObserved{Labels: owned(map[string]string{LabelSession: "session-b"})},
			want:     OwnershipFailureSession,
		},
		{
			name:     "session missing",
			expected: full,
			observed: OwnershipObserved{Labels: owned(map[string]string{LabelSession: ""})},
			want:     OwnershipFailureSession,
		},
		{
			name:     "creation mismatch",
			expected: full,
			observed: OwnershipObserved{Labels: owned(map[string]string{LabelCreation: testOtherID})},
			want:     OwnershipFailureCreation,
		},
		{
			name:     "creation missing",
			expected: full,
			observed: OwnershipObserved{Labels: owned(map[string]string{LabelCreation: ""})},
			want:     OwnershipFailureCreation,
		},
		{
			name:     "creation malformed",
			expected: full,
			observed: OwnershipObserved{Labels: owned(map[string]string{LabelCreation: "not-a-generation"})},
			want:     OwnershipFailureCreation,
		},
		{
			name:     "creation valid when only presence required",
			expected: OwnershipExpected{RequireCreation: true},
			observed: OwnershipObserved{Labels: owned(map[string]string{LabelCreation: testOtherID})},
		},
		{
			name:     "creation required but absent",
			expected: OwnershipExpected{RequireCreation: true},
			observed: OwnershipObserved{Labels: owned(map[string]string{LabelCreation: ""})},
			want:     OwnershipFailureCreation,
		},
		{
			name:     "identity exact",
			expected: OwnershipExpected{UID: testUID, RequireImmutableID: true},
			observed: OwnershipObserved{UID: testUID, Labels: owned(nil)},
		},
		{
			name:     "identity mismatch",
			expected: OwnershipExpected{UID: testUID, RequireImmutableID: true},
			observed: OwnershipObserved{UID: testOtherUID, Labels: owned(nil)},
			want:     OwnershipFailureIdentity,
		},
		{
			name:     "identity missing under immutable requirement",
			expected: OwnershipExpected{RequireImmutableID: true},
			observed: OwnershipObserved{Labels: owned(nil)},
			want:     OwnershipFailureIdentity,
		},
		{
			name:     "identity malformed under immutable requirement",
			expected: OwnershipExpected{RequireImmutableID: true},
			observed: OwnershipObserved{UID: "not-an-immutable-id", Labels: owned(nil)},
			want:     OwnershipFailureIdentity,
		},
		{
			name:     "identity rejected for name-addressed target",
			expected: OwnershipExpected{RequireNoImmutableID: true},
			observed: OwnershipObserved{UID: testUID, Labels: owned(nil)},
			want:     OwnershipFailureIdentity,
		},
		{
			name:     "nil labels are not owned",
			expected: manageOnly,
			observed: OwnershipObserved{},
			want:     OwnershipFailureManaged,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := VerifyOwnership(tt.expected, tt.observed); got != tt.want {
				t.Fatalf("VerifyOwnership() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Verification order decides which diagnostic a caller sees when several
// facts are wrong. Ownership markers must be reported before generation or
// identity mismatches, and the order is part of the policy.
func TestVerifyOwnershipReportsFirstFailure(t *testing.T) {
	failure := VerifyOwnership(OwnershipExpected{
		RequireManaged: true,
		RequireReuse:   true,
		Session:        "session-a",
		Creation:       testCreationID,
		UID:            testUID,
	}, OwnershipObserved{
		UID: testOtherUID,
		Labels: map[string]string{
			LabelManaged:  "false",
			LabelReuse:    "false",
			LabelCreation: testCreationID,
		},
	})
	if failure != OwnershipFailureManaged {
		t.Fatalf("first failure = %q, want managed", failure)
	}
}

func TestSameOwnershipLabels(t *testing.T) {
	base := map[string]string{
		LabelManaged:    "true",
		LabelSession:    "session-a",
		LabelCreation:   testCreationID,
		LabelReuse:      "true",
		LabelReuseGroup: "group-a",
		"custom":        "value-one",
	}
	same := map[string]string{
		LabelManaged:    "true",
		LabelSession:    "session-a",
		LabelCreation:   testCreationID,
		LabelReuse:      "true",
		LabelReuseGroup: "group-a",
		"custom":        "value-two",
	}
	if !SameOwnershipLabels(base, same) {
		t.Fatal("custom label differences must not affect ownership identity")
	}

	for _, key := range []string{
		LabelManaged,
		LabelSession,
		LabelCreation,
		LabelReuse,
		LabelReuseGroup,
	} {
		changed := make(map[string]string, len(base))
		for k, v := range base {
			changed[k] = v
		}
		changed[key] = "changed"
		if SameOwnershipLabels(base, changed) {
			t.Errorf("SameOwnershipLabels() ignored a change to %s", key)
		}
	}
}

func TestCreationAndContainerIDForms(t *testing.T) {
	if !IsCreationID(testCreationID) {
		t.Fatal("IsCreationID rejected a valid generation")
	}
	for _, generation := range []string{"", "0123456789abcde", "0123456789abcdef0", "0123456789ABCDEF", "zzzzzzzzzzzzzzzz"} {
		if IsCreationID(generation) {
			t.Errorf("IsCreationID(%q) = true, want false", generation)
		}
	}
	if !IsImmutableContainerID(testUID) {
		t.Fatal("IsImmutableContainerID rejected a valid ID")
	}
	for _, id := range []string{"", "abc", testUID + "a", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		if IsImmutableContainerID(id) {
			t.Errorf("IsImmutableContainerID(%q) = true, want false", id)
		}
	}
}
