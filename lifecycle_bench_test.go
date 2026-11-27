package container

import (
	"context"
	"testing"
	"time"
)

// BenchmarkHealthStatusCachedSnapshot measures the 10 Hz poll path: a fresh
// lifecycle snapshot supplies the health field without cloning labels, ports,
// capabilities, or aliases.
func BenchmarkHealthStatusCachedSnapshot(b *testing.B) {
	now := time.Unix(1_700_000_000, 0)
	ctr := &Container{sync: &lockPack{now: func() time.Time { return now }}}
	ctr.pack().dynamic = &engineInfo{
		observedAt: now,
		health:     "healthy",
		labels: map[string]string{
			managedLabel:  "true",
			creationLabel: "aaaaaaaaaaaaaaaa",
			sessionLabel:  "0123456789abcdef",
			reuseLabel:    "true",
		},
		networkNames:   []string{"bridge"},
		networkAliases: []string{"alias-one", "alias-two"},
		bound:          []boundPort{{containerPort: 8080, proto: "tcp", hostPort: 8080}},
		runOptions: engineRunOptions{
			capAdd:  []string{"NET_ADMIN"},
			capDrop: []string{"ALL"},
			ulimits: []ulimitSpec{{name: "nofile", soft: 1024, hard: 2048}},
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		health, err := ctr.healthStatus(context.Background())
		if err != nil || health != "healthy" {
			b.Fatalf("healthStatus = (%q, %v), want the cached healthy status", health, err)
		}
	}
}
