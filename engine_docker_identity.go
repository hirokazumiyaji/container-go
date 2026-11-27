package container

import "fmt"

// imageIdentityComplete reports that Docker's identity metadata is complete
// whenever inspect returned it: a pinned Docker reference is proven by its
// repo digest or verified local image ID alone.
func (dockerEngine) imageIdentityComplete(imageIdentity, string) bool { return true }

// verifyPinnedImageIdentity has no additional Docker completeness rule to
// enforce.
func (dockerEngine) verifyPinnedImageIdentity(imageIdentity, imageIdentity, string) error {
	return nil
}

// reconcileImageIdentity keeps the pinned identity unchanged: Docker's inspect
// is the authoritative observation and the pinned spelling is already the run
// argument.
func (dockerEngine) reconcileImageIdentity(pinned imageIdentity, _ imageIdentity) imageIdentity {
	return pinned
}

// imageReferencesCompatible compares a requested reference with the reference
// inspect reported using Docker's repository and tag rules.
func (dockerEngine) imageReferencesCompatible(requested, actual string) bool {
	return imagesCompatible(requested, actual)
}

// parsePruneListed returns no list metadata: Docker prunes by immutable ID
// from a line-oriented list and needs none.
func (dockerEngine) parsePruneListed(data []byte, _ string) ([]string, map[string]pruneCandidate, error) {
	ids, err := parseDockerPruneIDs(data)
	return ids, nil, err
}

// checkReuseNetworkCompat rejects an existing container whose Docker network
// configuration cannot satisfy the requested reuse options.
func (e dockerEngine) checkReuseNetworkCompat(info *engineInfo, cfg *config) error {
	requested := cfg.network
	if !cfg.networkExplicit {
		requested = ""
	}
	if err := dockerNetworkModeErrorDefault(requested, info.networkMode, info.networkNames, info.defaultNetwork); err != nil {
		return fmt.Errorf("%w: reuse %s: %w", ErrReuseMismatch, cfg.name, err)
	}
	if err := dockerReuseNetworkAliasesError(cfg, info); err != nil {
		return fmt.Errorf("%w: reuse %s: %w", ErrReuseMismatch, cfg.name, err)
	}
	if len(cfg.exposed) > 0 || len(cfg.published) > 0 {
		if err := dockerNetworkEndpointError(info.networkMode); err != nil {
			return fmt.Errorf("%w: reuse %s: %w", ErrReuseMismatch, cfg.name, err)
		}
	}
	return nil
}
