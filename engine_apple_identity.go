package container

import "fmt"

// imageIdentityComplete reports whether the parsed identity carries every
// platform field Apple requires for a pinned reference.
func (appleEngine) imageIdentityComplete(identity imageIdentity, platform string) bool {
	return appleImageIdentityComplete(identity, platform)
}

// verifyPinnedImageIdentity rejects a re-inspected pinned identity that cannot
// be proven complete and platform-correct from Apple's descriptor metadata.
func (appleEngine) verifyPinnedImageIdentity(pinned, checked imageIdentity, platform string) error {
	if !appleImageIdentityMetadataPresent(checked, platform) {
		return fmt.Errorf("%w: pinned reference %s did not return complete platform metadata", ErrImageIdentityUnavailable, pinned.reference)
	}
	if platform != "" && !platformMetadataCompatible(platform, checked.platform) {
		return fmt.Errorf("%w: pinned reference %s resolved for a different platform", ErrImageIdentityMismatch, pinned.reference)
	}
	if !appleImageIdentityComplete(checked, platform) {
		return fmt.Errorf("%w: pinned reference %s did not return a complete platform identity", ErrImageIdentityUnavailable, pinned.reference)
	}
	return nil
}

// reconcileImageIdentity merges the authoritative re-inspected identity back
// into the caller's pinned spelling.
func (appleEngine) reconcileImageIdentity(pinned, checked imageIdentity) imageIdentity {
	reconciled := checked
	// The exact inspect is the authoritative identity observation, but the
	// caller-selected reference is the stable run argument. Keep that spelling
	// while carrying the verified root, repository, platform, variant, and
	// synthetic metadata forward.
	if pinned.reference != "" {
		reconciled.reference = pinned.reference
	}
	if reconciled.rootDigest == "" {
		reconciled.rootDigest = reconciled.digest
	}
	reconciled.appleSynthetic = pinned.appleSynthetic || reconciled.appleSynthetic
	return reconciled
}

// imageReferencesCompatible compares a requested reference with the reference
// Apple's inspect reported, tolerating its canonical default-registry
// spelling.
func (appleEngine) imageReferencesCompatible(requested, actual string) bool {
	return appleImageReferencesCompatible(requested, actual)
}

