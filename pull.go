package container

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	// image is absent or when a pinned identity is not locally
	// addressable by the backend.
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

// WithAllowMutableImageTag permits Run to execute the caller's original
// mutable tag when the backend's image-inspect response contains no
// usable immutable identity, or when an Apple-generated synthetic index
// root cannot be addressed locally. It never downgrades a caller-supplied
// name@digest (including name:tag@digest), bare digest, or Docker
// image-ID-shaped value. This is an explicit compatibility escape hatch:
// it does not prevent a mutable tag from being replaced after inspection,
// so it is not an identity guarantee. Without this option Run fails closed
// with ErrImageIdentityUnavailable or ErrImageIdentityNotLocal.
func WithAllowMutableImageTag() Option {
	return func(c *config) error {
		c.allowMutableImageTag = true
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
	return pullWith(ctx, &cli.ExecRunner{Binary: eng.binary()}, eng, image)
}

// pullWith is the fake-runner-driven core of Pull: the flight shares one
// backend pull of the image, and failures go through Classify.
func pullWith(ctx context.Context, r cli.Runner, eng engine, image string) error {
	cfg := &config{runner: r, eng: eng}
	if err := eng.checkConfig(cfg); err != nil {
		return err
	}
	return doErr(ctx, &imageFlights, flightKey(eng, image, flightPull, cfg.platform), func() error {
		execCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
		defer cancel()
		return pullImage(execCtx, r, eng, image, cfg.platform)
	})
}

// flightKey identifies the pull being aggregated: one backend, one
// image, one operation kind, one platform. Same image with different
// platforms must not share a flight.
func flightKey(eng engine, image, op, platform string) string {
	return eng.name() + "\x00" + image + "\x00" + op + "\x00" + platform
}

// imageFlights aggregates concurrent image fetches across the process.
// Entries are always removed on completion, so a failed pull is
// retried on the next call.
var imageFlights flightGroup[struct{}]

// imageResolutionFlights shares the inspect/pull/identity transaction
// for PullMissing callers and the post-pull identity resolution for
// PullAlways callers. The pull itself remains in imageFlights so the
// public Pull API keeps sharing it with PullAlways.
var imageResolutionFlights flightGroup[imageIdentity]

// imageIdentityParser is an optional backend capability. Keeping it
// separate from engine lets older/injected engines retain the existence
// check while the default Run policy fails closed without an identity.
type imageIdentityParser interface {
	parseImageIdentity(data []byte, image, platform string) (imageIdentity, bool)
}

type localImageIdentityChecker interface {
	imageIdentityNeedsLocalCheck() bool
}

func imageIdentityNeedsLocalCheck(eng engine) bool {
	checker, ok := eng.(localImageIdentityChecker)
	return ok && checker.imageIdentityNeedsLocalCheck()
}

// imageIdentityNeedsLocalAddressCheck identifies pinned references that
// must be proven addressable before they are passed to a backend without
// a runtime pull-never switch. PullNever always checks. Apple has no
// run-time no-fetch switch, so every pinned Apple reference—not only an
// ID-derived one—is checked for PullMissing and PullAlways as well.
func imageIdentityNeedsLocalAddressCheck(eng engine, identity imageIdentity, policy PullPolicy) bool {
	if !identity.pinned || eng == nil {
		return false
	}
	if eng.name() == "apple" || imageIdentityNeedsLocalCheck(eng) {
		return true
	}
	return policy == PullNever
}

// ensureImage brings the image into the backend's local store according
// to the pull policy. It is retained as the error-only helper used by
// existing internal callers; Run uses ensureImageRef so the immutable
// identity is carried into the run command.
func (c *config) ensureImage(ctx context.Context, image string) error {
	_, err := c.ensureImageRef(ctx, image)
	return err
}

func (c *config) ensureImageRef(ctx context.Context, image string) (imageIdentity, error) {
	platform := c.platform
	switch c.pullPolicy {
	case PullNever:
		return c.resolveImage(ctx, image, platform)
	case PullAlways:
		if err := doErr(ctx, &imageFlights, flightKey(c.eng, image, flightPull, platform), func() error {
			execCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
			defer cancel()
			return pullImage(execCtx, c.runner, c.eng, image, platform)
		}); err != nil {
			return imageIdentity{}, err
		}
		resolveKey := flightKey(c.eng, image, flightPull, platform) + "\x00resolve"
		if c.allowMutableImageTag {
			resolveKey += "\x00allow-mutable-tag"
		}
		return imageResolutionFlights.do(ctx, resolveKey, func() (imageIdentity, error) {
			execCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
			defer cancel()
			return c.resolveImage(execCtx, image, platform)
		})
	default:
		key := flightKey(c.eng, image, flightMissing, platform)
		if c.allowMutableImageTag {
			// A fallback-enabled caller must not share a result with a
			// default fail-closed caller (or vice versa).
			key += "\x00allow-mutable-tag"
		}
		return imageResolutionFlights.do(ctx, key, func() (imageIdentity, error) {
			execCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
			defer cancel()
			identity, exists, err := inspectImage(execCtx, c.runner, c.eng, image, platform)
			if err != nil {
				return imageIdentity{}, err
			}
			if exists {
				// A record without a complete requested variant is not
				// proof that PullMissing already has the requested
				// platform. Pull explicitly when that policy permits;
				// PullNever still returns the typed not-local error below.
				if identity.notLocal && c.pullPolicy == PullMissing {
					if err := pullImage(execCtx, c.runner, c.eng, image, platform); err != nil {
						return imageIdentity{}, err
					}
					return c.resolveImage(execCtx, image, platform)
				}
				return c.resolveInspectedImage(execCtx, image, platform, identity)
			}
			if err := pullImage(execCtx, c.runner, c.eng, image, platform); err != nil {
				return imageIdentity{}, err
			}
			return c.resolveImage(execCtx, image, platform)
		})
	}
}

// resolveImage requires a successful inspect and turns the backend's
// reported identity into the argument that Run must use. A caller may
// explicitly opt into the old mutable-tag behavior, but the default is
// fail closed when the backend cannot identify the inspected image.
func (c *config) resolveImage(ctx context.Context, image, platform string) (imageIdentity, error) {
	identity, exists, err := inspectImage(ctx, c.runner, c.eng, image, platform)
	if err != nil {
		return imageIdentity{}, err
	}
	if !exists {
		if platform != "" {
			return imageIdentity{}, fmt.Errorf("%w: %s for platform %s", ErrImageNotFound, image, platform)
		}
		return imageIdentity{}, fmt.Errorf("%w: %s", ErrImageNotFound, image)
	}
	return c.resolveInspectedImage(ctx, image, platform, identity)
}

func (c *config) resolveInspectedImage(ctx context.Context, image, platform string, identity imageIdentity) (imageIdentity, error) {
	pinned, err := c.pinImage(image, identity)
	if err != nil {
		return imageIdentity{}, err
	}
	if !imageIdentityNeedsLocalAddressCheck(c.eng, pinned, c.pullPolicy) ||
		(pinned.reference == image && identity.pinned && !pinned.appleSynthetic) {
		return pinned, nil
	}

	// Apple Container resolves a missing reference during `container run`
	// because it has no --pull=never switch. Check the pinned reference
	// first, and explicitly pull it when the current policy permits a
	// fetch. This keeps a descriptor- or ID-derived reference from
	// silently becoming a registry lookup during create. Even when the
	// caller's reference already contains a digest, an identity-less
	// inspect must not be treated as proof of the descriptor identity.
	checked, pinnedExists, err := inspectImage(ctx, c.runner, c.eng, pinned.reference, platform)
	if err != nil {
		// A transport, permission, or cancellation failure is an
		// operational error, not evidence that identity is unavailable.
		// Preserve it for errors.Is/errors.As and never fall back to a
		// mutable tag on this path.
		return imageIdentity{}, fmt.Errorf("inspect locally addressable image %s: %w", pinned.reference, err)
	}
	if !pinnedExists && c.pullPolicy != PullNever {
		// Apple creates a local synthetic index around a single manifest.
		// Pulling that generated root by name@digest asks the registry for
		// an object it cannot name. Try the exact root only when it is
		// already addressable; otherwise require the caller's explicit
		// mutable-tag fallback (or fail with ErrImageIdentityNotLocal).
		if pinned.appleSynthetic {
			if c.canUseMutableFallback(image) {
				return mutableImageReference(image, pinned), nil
			}
			return imageIdentity{}, fmt.Errorf("%w: Apple synthetic index %s is not locally addressable", ErrImageIdentityNotLocal, pinned.reference)
		}
		pullErr := pullImage(ctx, c.runner, c.eng, pinned.reference, platform)
		if pullErr != nil {
			// A known not-found result means the exact reference is not
			// locally addressable and may use the explicit compatibility
			// escape hatch. Other failures (transport, permission, or
			// cancellation) must be returned unchanged and must not be
			// hidden by a mutable fallback.
			if c.canUseMutableFallback(image) && imageAddressMissing(c.eng, pullErr) {
				return mutableImageReference(image, identity), nil
			}
			if imageAddressMissing(c.eng, pullErr) {
				return imageIdentity{}, fmt.Errorf("%w: %s could not be made locally addressable: %w", ErrImageIdentityNotLocal, pinned.reference, pullErr)
			}
			return imageIdentity{}, fmt.Errorf("pull locally addressable image %s: %w", pinned.reference, pullErr)
		}
		checked, pinnedExists, err = inspectImage(ctx, c.runner, c.eng, pinned.reference, platform)
		if err != nil {
			return imageIdentity{}, fmt.Errorf("verify locally addressable image %s: %w", pinned.reference, err)
		}
	}
	if !pinnedExists {
		if c.canUseMutableFallback(image) {
			return mutableImageReference(image, pinned), nil
		}
		return imageIdentity{}, fmt.Errorf("%w: %s cannot run %s without fetching it", ErrImageIdentityNotLocal, c.eng.name(), pinned.reference)
	}
	if checked.notLocal {
		reason := checked.notLocalReason
		if reason == "" {
			reason = "the requested platform variant is not locally addressable"
		}
		return imageIdentity{}, fmt.Errorf("%w: pinned reference %s (%s)", ErrImageIdentityNotLocal, pinned.reference, reason)
	}
	if checked.mismatch {
		return imageIdentity{}, fmt.Errorf("%w: pinned reference %s resolves to another image", ErrImageIdentityMismatch, pinned.reference)
	}
	if !checked.pinned {
		if c.canUseMutableFallback(image) {
			return mutableImageReference(image, checked), nil
		}
		return imageIdentity{}, fmt.Errorf("%w: pinned reference %s did not report an immutable identity", ErrImageIdentityUnavailable, pinned.reference)
	}
	if !requestedImageIdentitiesCompatible(pinned, checked) {
		return imageIdentity{}, fmt.Errorf("%w: pinned reference %s changed before run", ErrImageIdentityMismatch, pinned.reference)
	}
	return pinned, nil
}

// verifyAppleCreatedImage closes the final Apple addressability window. A
// pinned image reference can disappear or be replaced after the last
// pre-create inspect; the container's own inspect must therefore report the
// same root identity and, when known, the selected platform variant. Any
// mismatch is returned before the new container is exposed to the caller;
// Run/reuse then roll it back.
func verifyAppleCreatedImage(ctx context.Context, c *Container, expected imageIdentity) error {
	if c == nil || c.eng == nil || c.eng.name() != "apple" || !expected.pinned {
		return nil
	}
	info, err := c.inspectFresh(ctx)
	if err != nil {
		return fmt.Errorf("verify Apple image after create: %w", err)
	}
	actual := imageFromInfo(info)
	if !actual.pinned || !imageRootsCompatible(expected, actual) {
		return fmt.Errorf("%w: Apple container created from a different image identity", ErrImageIdentityMismatch)
	}
	if expected.platform != "" {
		if actual.platform == "" || !applePlatformMetadataCompatible(expected.platform, actual.platform) {
			return fmt.Errorf("%w: Apple container created for a different platform", ErrImageIdentityMismatch)
		}
	}
	if expected.variantDigest != "" {
		variantDigest := actual.variantDigest
		if variantDigest == "" {
			imageReference := actual.reference
			if imageReference == "" {
				imageReference = expected.reference
			}
			checked, exists, inspectErr := inspectImage(ctx, c.runner, c.eng, imageReference, actual.platform)
			if inspectErr != nil {
				return fmt.Errorf("verify Apple image variant after create: %w", inspectErr)
			}
			if !exists || !checked.pinned || !imageRootsCompatible(expected, checked) {
				return fmt.Errorf("%w: Apple container did not retain the requested image variant", ErrImageIdentityMismatch)
			}
			variantDigest = checked.variantDigest
		}
		if !validImageDigest(variantDigest) || !strings.EqualFold(expected.variantDigest, variantDigest) {
			return fmt.Errorf("%w: Apple container created from a different image variant", ErrImageIdentityMismatch)
		}
	}
	return nil
}

func imageRootsCompatible(a, b imageIdentity) bool {
	if !a.pinned || !b.pinned {
		return false
	}
	aID, aHasID := canonicalDockerImageID(a.id)
	bID, bHasID := canonicalDockerImageID(b.id)
	if aHasID || bHasID {
		return aHasID && bHasID && strings.EqualFold(aID, bID)
	}
	aroot, broot := a.rootDigest, b.rootDigest
	if aroot == "" {
		aroot = a.digest
	}
	if broot == "" {
		broot = b.digest
	}
	if !validImageDigest(aroot) || !validImageDigest(broot) || !strings.EqualFold(aroot, broot) {
		return false
	}
	if a.repository != "" && b.repository != "" {
		return imageRepositoriesCompatible(a.repository, b.repository)
	}
	abase, bbase := imageReferenceBase(a.reference), imageReferenceBase(b.reference)
	return abase != "" && bbase != "" && imageRepositoryPathsCompatible(abase, bbase)
}

// imageAddressMissing reports only a backend-confirmed absence. Other
// failures must remain visible to the caller and must never authorize a
// mutable-tag fallback.
func imageAddressMissing(eng engine, err error) bool {
	return imageMissingError(eng, err)
}

// imageMissingError applies the backend's not-found matcher without
// allowing a broad "not found" substring to hide permission, transport,
// or cancellation failures.
func imageMissingError(eng engine, err error) bool {
	if eng == nil || err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrImageNotFound) {
		return true
	}
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		return false
	}
	// Delegate to the backend matcher, which is deliberately anchored to a
	// reason phrase. Do not search the whole stderr or CLIError.Error():
	// both include the requested image name, so a name such as
	// `permission-denied` or `not-found` must not change classification.
	return eng.imageMissing(err)
}

func (c *config) pinImage(image string, identity imageIdentity) (imageIdentity, error) {
	if identity.notLocal {
		reason := identity.notLocalReason
		if reason == "" {
			reason = "the requested platform variant is not locally addressable"
		}
		return imageIdentity{}, fmt.Errorf("%w: %s (%s)", ErrImageIdentityNotLocal, image, reason)
	}
	if identity.mismatch {
		return imageIdentity{}, fmt.Errorf("%w: %s", ErrImageIdentityMismatch, image)
	}
	requestedDigest := imageDigest(image)
	if identity.pinned {
		if !imageIdentityIsVerified(identity) {
			return imageIdentity{}, fmt.Errorf("%w: %s (%s returned an identity without repository provenance or a verified local ID)", ErrImageIdentityUnavailable, image, c.eng.name())
		}
		if validImageDigest(requestedDigest) && !identityHasDigest(identity, requestedDigest) {
			return imageIdentity{}, fmt.Errorf("%w: %q changed during resolution: requested %s, inspected %s", ErrImageIdentityMismatch, image, requestedDigest, identity.digest)
		}
		return identity, nil
	}
	// A successful identity-less Apple inspect cannot establish that a
	// caller-pinned digest is the local image. Mutable tags remain
	// available through the explicit compatibility option below; pinned
	// inputs are verified by resolveInspectedImage before they can run.
	immutableRequest := isBareImageReference(image) || isRepositoryDigestReference(image)
	if c.pullPolicy == PullNever && c.eng.name() == "apple" && immutableRequest {
		return imageIdentity{}, fmt.Errorf("%w: %s (Apple image inspect did not confirm the pinned identity)", ErrImageIdentityUnavailable, image)
	}
	// A caller-supplied digest is usable only with repository provenance.
	// A bare sha256:... is Docker's image-ID syntax, not proof that the
	// requested image exists; only a parser that inspected a matching
	// verified local ID may use that form (handled above).
	if requestedDigest := imageDigest(image); validImageDigest(requestedDigest) && imageReferenceBase(image) != "" {
		return imageIdentity{reference: image, digest: requestedDigest, pinned: true}, nil
	}
	if c.canUseMutableFallback(image) {
		return mutableImageReference(image, identity), nil
	}
	return imageIdentity{}, fmt.Errorf("%w: %s (%s image inspect did not report a usable digest or verified local ID)", ErrImageIdentityUnavailable, image, c.eng.name())
}

// imageIdentityIsVerified rejects a digest-shaped identity that has no
// repository provenance and no verified backend image ID.
func imageIdentityIsVerified(identity imageIdentity) bool {
	if identity.mutableAlias || !identity.pinned || !imageRE.MatchString(identity.reference) {
		return false
	}
	if imageID, ok := canonicalDockerImageID(identity.id); ok {
		return strings.EqualFold(identity.reference, imageID) || imageReferenceBase(identity.reference) != ""
	}
	root := identity.rootDigest
	if root == "" {
		root = identity.digest
	}
	return validImageDigest(root) && imageReferenceBase(identity.reference) != ""
}

func identityHasDigest(identity imageIdentity, requested string) bool {
	if !validImageDigest(requested) {
		return true
	}
	for _, candidate := range []string{identity.digest, identity.rootDigest, identity.variantDigest} {
		if validImageDigest(candidate) && strings.EqualFold(candidate, requested) {
			return true
		}
	}
	return false
}

func (c *config) canUseMutableFallback(image string) bool {
	return c.allowMutableImageTag && !isBareImageReference(image) && !strings.Contains(image, "@")
}

func mutableImageReference(image string, identity imageIdentity) imageIdentity {
	identity.reference = image
	identity.pinned = false
	identity.mutableAlias = false
	identity.appleSynthetic = false
	return identity
}

// inspectImage returns both existence and the identity reported by the
// backend. A non-empty inspect array can be present without an identity;
// callers must not interpret that as safe to execute a mutable tag.
func inspectImage(ctx context.Context, r cli.Runner, eng engine, image, platform string) (imageIdentity, bool, error) {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := r.Run(qCtx, eng.imageInspectArgs(image, platform)...)
	if err != nil {
		if imageMissingError(eng, err) {
			return imageIdentity{}, false, nil
		}
		return imageIdentity{}, false, cli.Classify(ctx, r, err, eng.probe())
	}
	if parser, ok := eng.(imageIdentityParser); ok {
		identity, exists := parser.parseImageIdentity(stdout, image, platform)
		return identity, exists, nil
	}
	return imageIdentity{}, eng.parseImageExists(stdout, platform), nil
}

// imageExists reports whether the image (and requested platform
// variant, when set) is in the backend's store.
func imageExists(ctx context.Context, r cli.Runner, eng engine, image, platform string) (bool, error) {
	identity, exists, err := inspectImage(ctx, r, eng, image, platform)
	return exists && !identity.notLocal, err
}

// pullImage fetches the image (and requested platform variant, when
// set) through the backend CLI.
func pullImage(ctx context.Context, r cli.Runner, eng engine, image, platform string) error {
	_, _, err := r.Run(ctx, eng.pullImageArgs(image, platform)...)
	if err != nil {
		return cli.Classify(ctx, r, err, eng.probe())
	}
	return nil
}
