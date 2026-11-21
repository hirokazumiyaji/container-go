package container

import (
	"context"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// engineCapabilities is the declarative contract a backend publishes to the
// shared lifecycle layers. Shared policy must consult these flags instead of
// asserting a concrete engine type or comparing engine names, so a new
// backend supplies its own declaration rather than editing every branch.
//
// The flags carry invariants shared code relies on:
//   - immutableID and nameAddressed are mutually exclusive: a backend addresses
//     a container either by verified immutable ID or by logical name.
//   - pruneCandidateMetadata requires nameAddressed; a backend without it is
//     pruned by the immutable IDs its list output returned and therefore must
//     also declare immutableID.
type engineCapabilities struct {
	// immutableID requires an inspected immutable container ID for every
	// identity-bearing or destructive operation; a logical name is never an
	// acceptable fallback.
	immutableID bool
	// nameAddressed addresses containers by logical name, so create, verify,
	// prune, and delete serialize cooperating processes with the per-name
	// lock.
	nameAddressed bool
	// daemonTarget supports external daemon target overrides and implicit
	// target resolution.
	daemonTarget bool
	// defaultNetwork resolves the daemon's concrete default network for the
	// special "default" network mode.
	defaultNetwork bool
	// copyOutVersionCheck gates copy-out on the backend client/server version
	// and shares the result per target.
	copyOutVersionCheck bool
	// pruneCandidateMetadata exposes ownership and creation metadata from the
	// prune list, and deletes candidates by name under the name lock after a
	// fresh inspect.
	pruneCandidateMetadata bool
	// pruneLabelListFilter accepts label filters in the prune list command, so
	// candidates can be selected daemon-side. A backend without it filters
	// listed candidates from their inspected labels instead.
	pruneLabelListFilter bool
	// strictImageIdentity requires a pinned image reference to be proven
	// locally addressable with complete platform metadata before a container
	// may run, and verifies the created container's image afterwards.
	strictImageIdentity bool
	// healthcheck reports a service-level HEALTHCHECK status that readiness
	// waits can observe.
	healthcheck bool
}

// engineTargetOverride carries explicit client target settings. A nil field
// leaves that component unchanged.
type engineTargetOverride struct {
	host      *string
	context   *string
	configDir *string
	platform  *string
}

// empty reports whether no target component was requested.
func (o engineTargetOverride) empty() bool {
	return o.host == nil && o.context == nil && o.configDir == nil && o.platform == nil
}

// targetOverrideString returns a pointer to value, or nil when the caller did
// not set it. A pointer distinguishes "not requested" from an explicit empty
// value, which is how the public target options are defined.
func targetOverrideString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// configuredTarget applies explicit target settings. Only a backend that owns
// a daemon target replaces its engine, so a name-addressed engine (or a test
// wrapper embedding one) is returned untouched. No settings means no call at
// all, which also preserves a wrapper that embeds a targeted backend.
//
// A wrapper that embeds a targeted backend and overrides its target behavior
// loses those overrides when settings are applied: the promoted method returns
// the embedded value. Configure such a wrapper through the embedded engine
// before wrapping it.
func configuredTarget(eng engine, target engineTargetOverride) engine {
	if !eng.capabilities().daemonTarget || target.empty() {
		return eng
	}
	return eng.configureTarget(target)
}

// frozenTarget snapshots ambient target settings for a Client. A backend
// without an addressable target is returned untouched.
func frozenTarget(eng engine) engine {
	if !eng.capabilities().daemonTarget {
		return eng
	}
	return eng.freezeTargetEnvironment()
}

// resolvedTarget resolves the daemon target one operation addresses.
func resolvedTarget(ctx context.Context, eng engine, runner cli.Runner) (engine, error) {
	if !eng.capabilities().daemonTarget {
		return eng, nil
	}
	return eng.resolveTargetEngine(ctx, runner)
}

// preparedPruneTarget resolves the target a prune addresses, tolerating an
// implicit target the runner's own CLI can still resolve.
func preparedPruneTarget(ctx context.Context, eng engine, runner cli.Runner, timeout time.Duration) (engine, error) {
	if !eng.capabilities().daemonTarget {
		return eng, nil
	}
	return eng.preparePruneTarget(ctx, runner, timeout)
}

// capabilities implements the engine contract for Docker.
func (dockerEngine) capabilities() engineCapabilities {
	return engineCapabilities{
		immutableID:          true,
		daemonTarget:         true,
		defaultNetwork:       true,
		copyOutVersionCheck:  true,
		healthcheck:          true,
		pruneLabelListFilter: true,
	}
}

// capabilities implements the engine contract for Apple Container.
func (appleEngine) capabilities() engineCapabilities {
	return engineCapabilities{
		nameAddressed:          true,
		pruneCandidateMetadata: true,
		strictImageIdentity:    true,
	}
}
