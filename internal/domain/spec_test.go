package domain

import (
	"reflect"
	"testing"
)

func TestSpecClone(t *testing.T) {
	orig := Spec{
		Image: "redis:7-alpine",
		Name:  "myctr",
		Cmd:   []string{"redis-server", "--port", "6379"},
		Env: map[string]string{
			"FOO": "bar",
		},
		Labels: map[string]string{
			"app": "redis",
		},
		Exposed: []PortSpec{
			{Port: 6379, Proto: "tcp"},
		},
		Published: []PublishSpec{
			{HostPort: 16379, ContainerPort: 6379, Proto: "tcp"},
		},
		Reuse:      true,
		ReuseGroup: "grp",
		Creation:   "gen-1",
	}

	clone := orig.Clone()
	if !reflect.DeepEqual(orig, clone) {
		t.Fatalf("Clone() = %+v, want %+v", clone, orig)
	}

	// Verify deep copy of map and slice
	clone.Env["FOO"] = "baz"
	if orig.Env["FOO"] != "bar" {
		t.Errorf("modifying clone modified orig.Env")
	}

	clone.Cmd[0] = "other"
	if orig.Cmd[0] != "redis-server" {
		t.Errorf("modifying clone modified orig.Cmd")
	}
}
