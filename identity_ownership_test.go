package container

import (
	"errors"
	"strings"
	"testing"
)

const (
	identityTestCreation = "0123456789abcdef"
	identityTestOtherID  = "fedcba9876543210"
	identityTestUID      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	identityTestOtherUID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func identityInfo(creation, uid string) *engineInfo {
	return &engineInfo{uid: uid, labels: map[string]string{creationLabel: creation}}
}

// A missing expected generation or backend identity must never become a
// wildcard: the helper promises fail-closed identity comparison, and a future
// caller could otherwise delete or stop a container it never verified.
func TestSameContainerIdentityRejectsMissingExpectedIdentity(t *testing.T) {
	tests := []struct {
		name     string
		eng      engine
		expected *engineInfo
		fresh    *engineInfo
		want     string
	}{
		{
			name:     "nil expected",
			eng:      appleEngine{},
			expected: nil,
			fresh:    identityInfo(identityTestCreation, ""),
			want:     "inspect returned no container identity",
		},
		{
			name:     "nil fresh",
			eng:      appleEngine{},
			expected: identityInfo(identityTestCreation, ""),
			fresh:    nil,
			want:     "inspect returned no container identity",
		},
		{
			name:     "empty expected generation",
			eng:      appleEngine{},
			expected: identityInfo("", ""),
			fresh:    identityInfo(identityTestCreation, ""),
			want:     "creation generation is missing or invalid",
		},
		{
			name:     "malformed expected generation",
			eng:      appleEngine{},
			expected: identityInfo("not-a-generation", ""),
			fresh:    identityInfo(identityTestCreation, ""),
			want:     "creation generation is missing or invalid",
		},
		{
			name:     "empty expected Docker ID",
			eng:      dockerEngine{},
			expected: identityInfo(identityTestCreation, ""),
			fresh:    identityInfo(identityTestCreation, identityTestUID),
			want:     "docker inspect ID is missing or invalid",
		},
		{
			name:     "malformed expected Docker ID",
			eng:      dockerEngine{},
			expected: identityInfo(identityTestCreation, "not-an-immutable-id"),
			fresh:    identityInfo(identityTestCreation, identityTestUID),
			want:     "docker inspect ID is missing or invalid",
		},
		{
			name:     "name-addressed expected ID",
			eng:      appleEngine{},
			expected: identityInfo(identityTestCreation, identityTestUID),
			fresh:    identityInfo(identityTestCreation, ""),
			want:     "backend identity changed",
		},
		{
			name:     "matching Docker identity",
			eng:      dockerEngine{},
			expected: identityInfo(identityTestCreation, identityTestUID),
			fresh:    identityInfo(identityTestCreation, identityTestUID),
		},
		{
			name:     "matching name-addressed identity",
			eng:      appleEngine{},
			expected: identityInfo(identityTestCreation, ""),
			fresh:    identityInfo(identityTestCreation, ""),
		},
		{
			name:     "changed observed generation",
			eng:      appleEngine{},
			expected: identityInfo(identityTestCreation, ""),
			fresh:    identityInfo(identityTestOtherID, ""),
			want:     "creation generation changed",
		},
		{
			name:     "changed observed Docker ID",
			eng:      dockerEngine{},
			expected: identityInfo(identityTestCreation, identityTestUID),
			fresh:    identityInfo(identityTestCreation, identityTestOtherUID),
			want:     "immutable container ID changed",
		},
		{
			name:     "unexpected observed name-addressed ID",
			eng:      appleEngine{},
			expected: identityInfo(identityTestCreation, ""),
			fresh:    identityInfo(identityTestCreation, identityTestUID),
			want:     "backend identity changed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := sameContainerIdentity(tt.eng, tt.expected, tt.fresh)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("sameContainerIdentity() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("sameContainerIdentity() = %v, want %q", err, tt.want)
			}
			if !errors.Is(err, ErrGenerationReplaced) {
				t.Fatalf("sameContainerIdentity() = %v, want ErrGenerationReplaced", err)
			}
		})
	}
}
