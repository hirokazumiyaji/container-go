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

// CanTransition reports whether a transition from state 'from' to state 'to' is valid.
func CanTransition(from, to State) bool {
	if from == to {
		return true
	}
	switch from {
	case StateCreated:
		return to == StateRunning || to == StateStopped || to == StateStopping || to == StateUnknown
	case StateRunning:
		return to == StateStopping || to == StateStopped || to == StateRestarting || to == StatePaused || to == StateUnknown
	case StateRestarting:
		return to == StateRunning || to == StateStopped || to == StateStopping || to == StateUnknown
	case StatePaused:
		return to == StateRunning || to == StateStopping || to == StateStopped || to == StateUnknown
	case StateStopping:
		return to == StateStopped || to == StateUnknown
	case StateStopped:
		return to == StateRunning || to == StateCreated || to == StateUnknown
	case StateUnknown:
		return true
	default:
		return false
	}
}
