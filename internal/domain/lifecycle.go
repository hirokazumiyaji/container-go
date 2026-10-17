package domain

import "fmt"

// State represents the lifecycle state of a container.
type State string

const (
	StateRunning    State = "running"
	StateStopped    State = "stopped"
	StateStopping   State = "stopping"
	StateCreated    State = "created"
	StateRestarting State = "restarting"
	StatePaused     State = "paused"
	StateUnknown    State = "unknown"
)

// Action defines the action to take based on container state during reuse resolution.
type Action int

const (
	ActionUnknown Action = iota
	ActionWait
	ActionAttach
	ActionRecreate
)

// ReuseAction determines the next step to take when attaching to an existing container.
func ReuseAction(state State, recreated bool) (Action, error) {
	switch state {
	case StateRunning:
		return ActionAttach, nil
	case StateCreated, StateRestarting, StateUnknown:
		return ActionWait, nil
	case StateStopping:
		return ActionUnknown, fmt.Errorf("container is stopping and cannot become ready")
	case StatePaused:
		return ActionUnknown, fmt.Errorf("container is paused and cannot become ready")
	case StateStopped:
		if recreated {
			return ActionUnknown, fmt.Errorf("container stayed stopped after recreate")
		}
		return ActionRecreate, nil
	default:
		return ActionWait, nil
	}
}
