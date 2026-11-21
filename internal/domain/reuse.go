package domain

// ReuseStep is the next step the reuse state machine selects for one
// observation. The decisions that return it are pure: the imperative shell
// performs inspect, image resolution, image-identity verification, network
// setup, and deletion, then records only the results in the input structs.
type ReuseStep string

const (
	// ReuseCreateNew creates a new generation because no container exists.
	ReuseCreateNew ReuseStep = "create-new"
	// ReuseStartExisting waits for an existing generation that is already
	// starting to reach a final state.
	ReuseStartExisting ReuseStep = "start-existing"
	// ReuseAttachRunning attaches to a running generation.
	ReuseAttachRunning ReuseStep = "attach-running"
	// ReuseRecreateIncompatible replaces a generation that cannot satisfy the
	// request as it is.
	ReuseRecreateIncompatible ReuseStep = "recreate-incompatible"
	// ReuseFailMismatch fails because the observed container does not belong
	// to this request or cannot become ready.
	ReuseFailMismatch ReuseStep = "fail-mismatch"
)

// ReuseDecision is the pure outcome of one reuse assessment. Reason is a
// stable explanation used for diagnostics and table tests; the shell may
// replace it with a more specific error it recorded while collecting facts.
type ReuseDecision struct {
	Action ReuseStep
	Reason string
}

// ReuseInput is the pre-collected fact set for the ensure phase. The shell
// resolves the image and verifies its presence and digest identity before
// comparing the result with the inspected container.
type ReuseInput struct {
	// Exists reports whether inspect found the named container.
	Exists bool
	// State is the inspected lifecycle state. It is ignored when Exists is
	// false.
	State State
	// OwnershipVerified reports whether the ownership labels and reuse group
	// match this request.
	OwnershipVerified bool
	// ImageCompatible reports whether the requested image identity matches
	// the identity inspect reported.
	ImageCompatible bool
	// Recreated reports that this call already replaced a stopped generation.
	Recreated bool
}

// DecideReuse selects the ensure transition: create when the name is absent,
// attach when it is running, wait while it starts, replace a stopped
// generation, and fail when ownership, image, or state cannot be reconciled.
func DecideReuse(in ReuseInput) ReuseDecision {
	if !in.Exists {
		return ReuseDecision{Action: ReuseCreateNew, Reason: "no container exists for the name"}
	}
	if !in.OwnershipVerified {
		return ReuseDecision{Action: ReuseFailMismatch, Reason: "container ownership does not match"}
	}
	if !in.ImageCompatible {
		return ReuseDecision{Action: ReuseFailMismatch, Reason: "container image identity does not match"}
	}
	switch in.State {
	case StateRunning:
		return ReuseDecision{Action: ReuseAttachRunning, Reason: "container is running"}
	case StateCreated, StateRestarting, StateUnknown:
		return ReuseDecision{Action: ReuseStartExisting, Reason: "container is starting"}
	case StateStopped:
		if in.Recreated {
			return ReuseDecision{Action: ReuseFailMismatch, Reason: "container stayed stopped after recreate"}
		}
		return ReuseDecision{Action: ReuseRecreateIncompatible, Reason: "container is stopped"}
	case StateStopping:
		return ReuseDecision{Action: ReuseFailMismatch, Reason: "container is stopping and cannot become ready"}
	case StatePaused:
		return ReuseDecision{Action: ReuseFailMismatch, Reason: "container is paused and cannot become ready"}
	default:
		return ReuseDecision{Action: ReuseStartExisting, Reason: "container state is not final"}
	}
}

// ReuseCompatInput is the pre-collected fact set for a caller that already
// holds a shared generation handle. The shell inspects the handle, verifies
// its generation, ownership, and image identity, applies any default-network
// setup, and evaluates configuration and port compatibility before calling.
type ReuseCompatInput struct {
	// State is the inspected lifecycle state.
	State State
	// GenerationVerified reports whether the inspected creation label and
	// immutable ID still address the handle's generation.
	GenerationVerified bool
	// OwnershipVerified reports whether the ownership labels and reuse group
	// match this request.
	OwnershipVerified bool
	// ImageCompatible reports whether the requested image identity matches
	// the identity inspect reported.
	ImageCompatible bool
	// ConfigCompatible reports whether creation options, network, platform,
	// run options, and ports match. It is only assessed for running targets.
	ConfigCompatible bool
	// PortPending reports a requested port binding that another process may
	// still publish; the shell polls it instead of failing.
	PortPending bool
	// WaitForAddress reports a direct-IP backend that has requested ports but
	// no reported container address yet.
	WaitForAddress bool
}

// DecideReuseCompat selects the caller-phase transition: attach to a running
// compatible generation, keep polling while a generation is starting, a port
// binding is pending, or an address is still unreported, and fail on a
// replaced generation or an incompatible configuration. The shell handles a
// missing generation before calling; a not-found inspect is still a
// replacement, but it never reaches this decision.
func DecideReuseCompat(in ReuseCompatInput) ReuseDecision {
	if !in.GenerationVerified {
		return ReuseDecision{Action: ReuseFailMismatch, Reason: "container generation does not match"}
	}
	switch in.State {
	case StateRunning:
		if !in.OwnershipVerified {
			return ReuseDecision{Action: ReuseFailMismatch, Reason: "container ownership does not match"}
		}
		if !in.ImageCompatible {
			return ReuseDecision{Action: ReuseFailMismatch, Reason: "container image identity does not match"}
		}
		if !in.ConfigCompatible {
			if in.PortPending {
				return ReuseDecision{Action: ReuseStartExisting, Reason: "requested port binding is not published yet"}
			}
			return ReuseDecision{Action: ReuseFailMismatch, Reason: "container configuration does not match"}
		}
		if in.WaitForAddress {
			return ReuseDecision{Action: ReuseStartExisting, Reason: "container address is not reported yet"}
		}
		return ReuseDecision{Action: ReuseAttachRunning, Reason: "container is running"}
	case StateStopping:
		return ReuseDecision{Action: ReuseFailMismatch, Reason: "container is stopping and cannot become ready"}
	case StatePaused:
		return ReuseDecision{Action: ReuseFailMismatch, Reason: "container is paused and cannot become ready"}
	case StateStopped:
		return ReuseDecision{Action: ReuseFailMismatch, Reason: "container stopped before becoming ready"}
	default:
		return ReuseDecision{Action: ReuseStartExisting, Reason: "container state is not final"}
	}
}
