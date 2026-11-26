package container

import "fmt"

// imageReferencesCompatible compares a requested reference with the reference
// inspect reported using Docker's repository and tag rules.
func (dockerEngine) imageReferencesCompatible(requested, actual string) bool {
	return imagesCompatible(requested, actual)
}

// parsePruneCandidates returns no list metadata: Docker prunes by immutable ID
// and needs none.
func (dockerEngine) parsePruneCandidates([]byte) (map[string]pruneCandidate, error) {
	return nil, nil
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
