package container

import (
	"context"
	"slices"
	"testing"
)

const inspectExtensionJSON = `[
  {
    "Id": "` + dockerFixtureID + `",
    "Name": "/myctr",
    "Platform": "linux/arm64",
    "State": {
      "Status": "running",
      "Health": {"Status": "unhealthy", "FailingStreak": 3}
    },
    "Config": {
      "Image": "redis:7-alpine@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      "Labels": {"com.github.hirokazumiyaji.container-go": "true"}
    },
    "HostConfig": {"NetworkMode": "appnet"},
    "NetworkSettings": {
      "IPAddress": "10.0.0.2",
      "Networks": {
        "appnet": {
          "IPAddress": "10.0.0.2",
          "Aliases": ["alias-a"],
          "DNSNames": ["dns-a"]
        },
        "othernet": {
          "IPAddress": "10.0.0.3",
          "Aliases": ["alias-b"]
        }
      }
    }
  }
]`

func TestInspectInfoExposesParsedDockerMetadata(t *testing.T) {
	d := &dockerRunner{fakeRunner: newTestRunner()}
	ctr := runDockerTestContainer(t, d)
	d.inspectJSON = []byte(inspectExtensionJSON)

	info, err := ctr.Inspect(context.Background())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got := info.ImageDigest(); got != "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("ImageDigest() = %q", got)
	}
	status, streak := info.Health()
	if status != "unhealthy" || streak != 3 {
		t.Errorf("Health() = (%q, %d), want (unhealthy, 3)", status, streak)
	}
	if got := info.NetworkMode(); got != "appnet" {
		t.Errorf("NetworkMode() = %q, want appnet", got)
	}
	if got := info.Networks(); !slices.Equal(got, []string{"appnet", "othernet"}) {
		t.Errorf("Networks() = %v, want [appnet othernet]", got)
	}
	aliases := info.NetworkAliases()
	if got := aliases["appnet"]; !slices.Equal(got, []string{"alias-a", "dns-a"}) {
		t.Errorf("NetworkAliases()[appnet] = %v, want [alias-a dns-a]", got)
	}
	if got := aliases["othernet"]; !slices.Equal(got, []string{"alias-b"}) {
		t.Errorf("NetworkAliases()[othernet] = %v, want [alias-b]", got)
	}
	if got := info.Platform(); got != "linux/arm64" {
		t.Errorf("Platform() = %q, want linux/arm64", got)
	}
}

func TestInspectInfoGetterCopies(t *testing.T) {
	info := &InspectInfo{
		networks:       []string{"appnet"},
		networkAliases: map[string][]string{"appnet": {"alias-a"}},
	}
	networks := info.Networks()
	networks[0] = "changed"
	if got := info.Networks(); !slices.Equal(got, []string{"appnet"}) {
		t.Errorf("Networks() after caller mutation = %v", got)
	}
	aliases := info.NetworkAliases()
	aliases["appnet"][0] = "changed"
	delete(aliases, "appnet")
	if got := info.NetworkAliases()["appnet"]; !slices.Equal(got, []string{"alias-a"}) {
		t.Errorf("NetworkAliases() after caller mutation = %v", got)
	}
}

func TestInspectInfoAppleReportsNetworksWithoutAliases(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)

	info, err := ctr.Inspect(context.Background())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got := info.Networks(); !slices.Equal(got, []string{"default"}) {
		t.Errorf("Networks() = %v, want [default]", got)
	}
	if got := info.NetworkMode(); got != "" {
		t.Errorf("NetworkMode() = %q, want empty for Apple Container", got)
	}
	if got := info.NetworkAliases(); got != nil {
		t.Errorf("NetworkAliases() = %v, want nil for Apple Container", got)
	}
	status, streak := info.Health()
	if status != "" || streak != 0 {
		t.Errorf("Health() = (%q, %d), want empty values for Apple Container", status, streak)
	}
}
