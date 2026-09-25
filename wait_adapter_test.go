package container

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/hirokazumiyaji/container-go/wait"
)

type waitRunnerFunc func(context.Context, ...string) ([]byte, []byte, error)

func (f waitRunnerFunc) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	return f(ctx, args...)
}

func TestWaitTargetBackendStatePolicy(t *testing.T) {
	cases := []struct {
		name   string
		engine engine
		status string
		want   wait.State
	}{
		{name: "Apple created", engine: appleEngine{}, status: "created", want: wait.StateCreated},
		{name: "Apple stopping is transient", engine: appleEngine{}, status: "stopping", want: wait.StateStopping},
		{name: "Apple unknown", engine: appleEngine{}, status: "future-state", want: wait.StateUnknown},
		{name: "Docker restarting is transient", engine: dockerEngine{}, status: "restarting", want: wait.StateRestarting},
		{name: "Docker paused is terminal", engine: dockerEngine{}, status: "paused", want: wait.StatePaused},
		{name: "Docker unknown", engine: dockerEngine{}, status: "future-state", want: wait.StateUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := waitRunnerFunc(func(_ context.Context, _ ...string) ([]byte, []byte, error) {
				if tc.engine.name() == "apple" {
					return []byte(fmt.Sprintf(`[{"id":"myctr","status":{"state":%q}}]`, tc.status)), nil, nil
				}
				return []byte(fmt.Sprintf(`[{"Id":"myctr","State":{"Status":%q}}]`, tc.status)), nil, nil
			})
			target := waitTarget{c: &Container{id: "myctr", runner: runner, eng: tc.engine}}

			got, err := target.State(context.Background())
			if err != nil {
				t.Fatalf("State: %v", err)
			}
			if got != tc.want {
				t.Errorf("State = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWaitTargetReturnsUnknownWithInspectError(t *testing.T) {
	inspectErr := errors.New("temporary inspect failure")
	runner := waitRunnerFunc(func(context.Context, ...string) ([]byte, []byte, error) {
		return nil, nil, inspectErr
	})
	target := waitTarget{c: &Container{id: "myctr", runner: runner, eng: appleEngine{}}}

	state, err := target.State(context.Background())
	if state != wait.StateUnknown {
		t.Errorf("State = %q, want %q", state, wait.StateUnknown)
	}
	if !errors.Is(err, inspectErr) {
		t.Errorf("State error = %v, want %v", err, inspectErr)
	}
}
