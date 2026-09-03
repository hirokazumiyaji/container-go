package container

import (
	"context"
	"fmt"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// PullPolicy decides when Run fetches the image.
type PullPolicy int

const (
	// PullMissing fetches the image only when it is not in the
	// backend's local store. This is the default and mirrors the
	// implicit pull Run always did before.
	PullMissing PullPolicy = iota
	// PullAlways fetches the image on every Run.
	PullAlways
	// PullNever never fetches; Run fails before starting when the
	// image is absent.
	PullNever
)

// Flight operation kinds keep mandatory pulls from joining an
// inspect-only PullMissing flight for the same image.
const (
	flightPull    = "pull"
	flightMissing = "missing"
)

// WithPullPolicy sets when Run fetches the image. The default is
// PullMissing.
func WithPullPolicy(policy PullPolicy) Option {
	return func(c *config) error {
		if policy < PullMissing || policy > PullNever {
			return fmt.Errorf("invalid pull policy %d", policy)
		}
		c.pullPolicy = policy
		return nil
	}
}

// Pull fetches the image into the backend's local store. Concurrent
// callers in the same process share one pull of the same image; a
// caller whose context is cancelled stops waiting without affecting
// the others.
func Pull(ctx context.Context, image string) error {
	if !imageRE.MatchString(image) {
		return fmt.Errorf("invalid image reference %q", image)
	}
	eng, err := detectEngine()
	if err != nil {
		return err
	}
	r := &cli.ExecRunner{Binary: eng.binary()}
	return doErr(ctx, &imageFlights, flightKey(eng, image, flightPull), func() error {
		execCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
		defer cancel()
		_, _, err := r.Run(execCtx, eng.pullImageArgs(image)...)
		if err != nil {
			return cli.Classify(execCtx, r, err, eng.probe())
		}
		return nil
	})
}

// flightKey identifies the pull being aggregated: one backend, one
// image, one operation kind.
func flightKey(eng engine, image, op string) string {
	return eng.name() + "\x00" + image + "\x00" + op
}

// imageFlights aggregates concurrent image fetches across the process.
// Entries are always removed on completion, so a failed pull is
// retried on the next call.
var imageFlights flightGroup[struct{}]

// ensureImage brings the image into the backend's local store according
// to the pull policy. It runs once per concurrent set of Runs sharing
// the image and operation: the first caller inspects and/or pulls, the
// rest wait.
func (c *config) ensureImage(ctx context.Context, image string) error {
	switch c.pullPolicy {
	case PullNever:
		exists, err := imageExists(ctx, c.runner, c.eng, image)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: %s", ErrImageNotFound, image)
		}
		return nil
	case PullAlways:
		return doErr(ctx, &imageFlights, flightKey(c.eng, image, flightPull), func() error {
			execCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
			defer cancel()
			return pullImage(execCtx, c.runner, c.eng, image)
		})
	default:
		return doErr(ctx, &imageFlights, flightKey(c.eng, image, flightMissing), func() error {
			execCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
			defer cancel()
			exists, err := imageExists(execCtx, c.runner, c.eng, image)
			if err != nil || exists {
				return err
			}
			return pullImage(execCtx, c.runner, c.eng, image)
		})
	}
}

// imageExists reports whether the image is in the backend's store.
func imageExists(ctx context.Context, r cli.Runner, eng engine, image string) (bool, error) {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := r.Run(qCtx, eng.imageInspectArgs(image)...)
	if err != nil {
		if eng.imageMissing(err) {
			return false, nil
		}
		return false, cli.Classify(ctx, r, err, eng.probe())
	}
	return eng.parseImageExists(stdout), nil
}

// pullImage fetches the image through the backend CLI.
func pullImage(ctx context.Context, r cli.Runner, eng engine, image string) error {
	_, _, err := r.Run(ctx, eng.pullImageArgs(image)...)
	if err != nil {
		return cli.Classify(ctx, r, err, eng.probe())
	}
	return nil
}
