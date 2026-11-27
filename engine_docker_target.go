package container

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// target returns the Docker daemon target scope.
func (e dockerEngine) target() engineTarget { return e }

// targetEnv lists the configured target components as they must appear in the
// child environment.
func (e dockerEngine) targetEnv() []string {
	var env []string
	if e.explicitHost && !e.explicitContext {
		if host := e.effectiveHost(); host != "" {
			env = append(env, "DOCKER_HOST="+host)
		}
	} else if ctx := e.effectiveContext(); ctx != "" {
		env = append(env, "DOCKER_CONTEXT="+ctx)
	} else if host := e.effectiveHost(); host != "" {
		env = append(env, "DOCKER_HOST="+host)
	}
	if cfg := e.effectiveConfigDir(); cfg != "" {
		env = append(env, "DOCKER_CONFIG="+cfg)
	}
	if plat := e.effectiveDefaultPlatform(); plat != "" {
		env = append(env, "DOCKER_DEFAULT_PLATFORM="+plat)
	}
	return env
}

// pinnedTargetEnv returns the complete child environment for this target:
// every component is either its configured value or a bare key that removes
// the inherited entry. targetEnv only lists configured components, so an
// unset one would let a variable exported after the target was frozen
// repoint the child CLI at another daemon.
//
// Connection modifiers (DOCKER_TLS_VERIFY, DOCKER_CERT_PATH,
// DOCKER_API_VERSION) are intentionally left inherited: they change how the
// pinned connection is negotiated, not which daemon it reaches, and
// stripping them would break a daemon that requires TLS.
func (e dockerEngine) pinnedTargetEnv() []string {
	env := e.targetEnv()
	present := make(map[string]struct{}, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		present[key] = struct{}{}
	}
	for _, key := range []string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_DEFAULT_PLATFORM"} {
		if _, ok := present[key]; !ok {
			env = append(env, key)
		}
	}
	return env
}

// configureTarget applies explicit client target settings to a Docker target.
// The returned engine addresses the configured daemon.
func (e dockerEngine) configureTarget(target engineTargetOverride) engine {
	if target.host != nil {
		e.host = *target.host
		e.explicitHost = true
		e.explicitTarget = true
	}
	if target.context != nil {
		e.context = *target.context
		e.explicitContext = true
		e.explicitTarget = true
	}
	if target.configDir != nil {
		e.configDir = *target.configDir
		e.explicitConfigDir = true
		e.explicitTarget = true
	}
	if target.platform != nil {
		e.defaultPlatform = *target.platform
		e.explicitDefaultPlatform = true
		e.explicitTarget = true
	}
	return e
}

// freezeTargetEnvironment snapshots the ambient Docker target settings so a
// Client keeps addressing the same daemon after process environment changes.
func (e dockerEngine) freezeTargetEnvironment() engine {
	e.explicitTarget = true
	if e.host == "" && !e.explicitHost {
		e.host = os.Getenv("DOCKER_HOST")
	}
	if e.context == "" && !e.explicitContext {
		e.context = os.Getenv("DOCKER_CONTEXT")
	}
	if e.configDir == "" && !e.explicitConfigDir {
		e.configDir = os.Getenv("DOCKER_CONFIG")
	}
	if e.defaultPlatform == "" && !e.explicitDefaultPlatform {
		e.defaultPlatform = os.Getenv("DOCKER_DEFAULT_PLATFORM")
	}
	return e
}

// resolveTargetEngine resolves the Docker daemon target this operation will
// address. The concrete resolveTarget keeps returning dockerEngine for its
// internal callers and tests.
func (e dockerEngine) resolveTargetEngine(ctx context.Context, runner cli.Runner) (engine, error) {
	return e.resolveTarget(ctx, runner)
}

// preparePruneTarget resolves the target a prune will address. Prune has no
// --host/--context flag, so it follows the child's Docker environment: an
// explicit runner host overrides an ambient context. An implicit target that
// cannot be discovered is left for the runner's own CLI to resolve, matching
// prune's best-effort target discovery.
func (e dockerEngine) preparePruneTarget(ctx context.Context, runner cli.Runner, timeout time.Duration) (engine, error) {
	original := e
	implicitContext := !e.explicitContext
	if entry, ok := runnerEnvEntry(runner, "DOCKER_CONTEXT"); ok && implicitContext {
		if _, _, assigned := strings.Cut(entry, "="); !assigned {
			e.context, e.explicitContext = "", true
		}
	}
	// Package prune follows the child's Docker environment, where a host
	// overrides the ambient context; discovery must not retarget the runner.
	if host, _, _, _ := e.targetSelection(runner); host != "" {
		e.host, e.explicitHost = host, true
		e.explicitContext = false
	}
	queryCtx, cancel := context.WithTimeout(ctx, min(timeout, queryTimeout))
	defer cancel()
	resolved, err := e.resolveTarget(queryCtx, runner)
	if err != nil {
		host, contextName, _, _ := e.targetSelection(runner)
		implicitTarget := implicitContext && host == ""
		var unavailable *dockerContextUnavailableError
		if ctx.Err() == nil && implicitTarget && (contextName == "" || errors.As(err, &unavailable)) {
			return original, nil
		}
		return nil, err
	}
	return resolved, nil
}

// targetHost reports the configured Docker host for diagnostics and
// validation.
func (e dockerEngine) targetHost() string { return e.effectiveHost() }

// targetContext reports the configured Docker context.
func (e dockerEngine) targetContext() string { return e.effectiveContext() }

// targetConfigDir reports the configured Docker config directory.
func (e dockerEngine) targetConfigDir() string { return e.effectiveConfigDir() }

// diagnosticTargetValues returns the Docker target components that must be
// redacted from diagnostics.
func (e dockerEngine) diagnosticTargetValues() []string {
	values := []string{e.effectiveHost(), e.effectiveConfigDir()}
	if context := e.effectiveContext(); context != "default" {
		values = append(values, context)
	}
	if e.resolvedTarget != nil && e.resolvedTarget.isRemote {
		values = append(values, e.resolvedTarget.rawHost)
	}
	return values
}

// diagnosticBoundaryValues returns Docker endpoint values that may straddle a
// bounded streaming window and therefore need boundary-aware redaction.
func (e dockerEngine) diagnosticBoundaryValues() []string {
	var values []string
	if endpoint, err := parseDockerEndpoint(e.effectiveHost()); err == nil && endpoint.isRemote {
		values = append(values, dockerEndpointBoundaryValues(endpoint)...)
	}
	if e.resolvedTarget != nil && e.resolvedTarget.isRemote {
		values = append(values, dockerEndpointBoundaryValues(*e.resolvedTarget)...)
	}
	return values
}

// runnerDiagnosticValues returns target components that a caller-supplied
// runner sets in its own environment. The engine does not see those values
// until target resolution runs, so a failed operation quotes them before a
// resolved target exists.
func (e dockerEngine) runnerDiagnosticValues(runner cli.Runner) (values, boundary []string) {
	for _, key := range []string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG"} {
		value, ok := runnerEnvLookup(runner, key)
		if !ok || value == "" || (key == "DOCKER_CONTEXT" && value == "default") {
			continue
		}
		values = append(values, value)
		if key == "DOCKER_HOST" {
			if endpoint, err := parseDockerEndpoint(value); err == nil && endpoint.isRemote {
				boundary = append(boundary, dockerEndpointBoundaryValues(endpoint)...)
			}
		}
	}
	return values, boundary
}

// defaultRunPlatform reports the platform the backend selects when the caller
// chose none. Docker resolves its own default, so an empty selector is left in
// place.
func (dockerEngine) defaultRunPlatform() string { return "" }
