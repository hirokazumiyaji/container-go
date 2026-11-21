package domain

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
