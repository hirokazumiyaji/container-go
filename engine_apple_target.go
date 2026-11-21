package container

import (
	"context"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// pinnedTargetEnv returns no pinning environment: Apple Container has no
// externally addressable target.
func (appleEngine) pinnedTargetEnv() []string { return nil }

// configureTarget ignores target settings: Apple Container has one local
// service and no addressable daemon target.
func (e appleEngine) configureTarget(engineTargetOverride) engine { return e }

// freezeTargetEnvironment has no ambient target to freeze.
func (e appleEngine) freezeTargetEnvironment() engine { return e }

// resolveTargetEngine returns the local Apple service.
func (e appleEngine) resolveTargetEngine(context.Context, cli.Runner) (engine, error) {
	return e, nil
}

// preparePruneTarget has no implicit target to resolve.
func (e appleEngine) preparePruneTarget(context.Context, cli.Runner, time.Duration) (engine, error) {
	return e, nil
}

func (appleEngine) targetHost() string      { return "" }
func (appleEngine) targetContext() string   { return "" }
func (appleEngine) targetConfigDir() string { return "" }

func (appleEngine) diagnosticTargetValues() []string { return nil }

func (appleEngine) diagnosticBoundaryValues() []string { return nil }

func (appleEngine) runnerDiagnosticValues(cli.Runner) (values, boundary []string) {
	return nil, nil
}

// defaultRunPlatform reports Apple Container's host-architecture Linux
// platform, which the run command applies when the caller selects none.
func (appleEngine) defaultRunPlatform() string { return appleHostPlatform() }
