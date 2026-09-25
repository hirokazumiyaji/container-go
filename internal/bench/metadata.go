package bench

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
)

// SourceMetadata identifies the source used by a benchmark run. Tree is
// the Git tree object for Commit. Dirty covers tracked, untracked, and
// submodule changes; ignored build and result files are not source changes.
type SourceMetadata struct {
	Commit string
	Tree   string
	Dirty  bool
}

// Source is a shorter compatibility alias for SourceMetadata.
type Source = SourceMetadata

// CurrentSource resolves source provenance without hiding a dirty worktree.
// It returns Dirty=true so callers can record the condition; benchmark runs
// must use RequireCleanSource before measuring.
func CurrentSource() (SourceMetadata, error) {
	override, err := benchmarkCommitOverride()
	if err != nil {
		return SourceMetadata{}, err
	}

	if _, err := exec.LookPath("git"); err == nil {
		head, headErr := gitOutput("rev-parse", "--verify", "HEAD^{commit}")
		if headErr == nil {
			head = strings.TrimSpace(head)
			if !validGitObjectID(head) {
				return SourceMetadata{}, fmt.Errorf("resolve benchmark commit: git returned invalid object ID %q", head)
			}
			if override != "" && override != head {
				return SourceMetadata{}, fmt.Errorf("CONTAINERGO_BENCH_COMMIT=%q does not match checked-out HEAD %q", override, head)
			}
			if override == "" {
				override = head
			}

			tree, treeErr := gitOutput("rev-parse", "--verify", override+"^{tree}")
			if treeErr != nil {
				return SourceMetadata{}, fmt.Errorf("resolve benchmark tree for %s: %w", override, treeErr)
			}
			tree = strings.TrimSpace(tree)
			if !validGitObjectID(tree) {
				return SourceMetadata{}, fmt.Errorf("resolve benchmark tree: git returned invalid object ID %q", tree)
			}
			status, statusErr := gitOutput("status", "--porcelain=v1", "--untracked-files=normal")
			if statusErr != nil {
				return SourceMetadata{}, fmt.Errorf("resolve benchmark source status: %w", statusErr)
			}
			return SourceMetadata{Commit: override, Tree: tree, Dirty: status != ""}, nil
		}
	}

	// An exported source tree can retain a full commit override, but without
	// Git metadata its tree object and cleanliness cannot be established.
	// RequireCleanSource rejects that incomplete provenance.
	if override != "" {
		return SourceMetadata{Commit: override}, nil
	}

	if info, ok := debug.ReadBuildInfo(); ok {
		var commit string
		dirty := false
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				commit = strings.TrimSpace(setting.Value)
			case "vcs.modified":
				dirty = strings.EqualFold(strings.TrimSpace(setting.Value), "true")
			}
		}
		if commit != "" {
			if !validGitObjectID(commit) {
				return SourceMetadata{}, fmt.Errorf("resolve benchmark commit: build metadata has invalid object ID %q", commit)
			}
			return SourceMetadata{Commit: commit, Dirty: dirty}, nil
		}
	}
	return SourceMetadata{}, errors.New("resolve benchmark source: no verified Git checkout or vcs.revision")
}

// RequireCleanSource returns source metadata suitable for a strict result
// document. In particular, a valid commit override is not enough without a
// Git tree hash, and any dirty source is rejected.
func RequireCleanSource() (SourceMetadata, error) {
	source, err := CurrentSource()
	if err != nil {
		return SourceMetadata{}, err
	}
	if source.Dirty {
		return SourceMetadata{}, fmt.Errorf("benchmark source is dirty at commit %s; commit or stash all tracked, untracked, and submodule changes", source.Commit)
	}
	if !validGitObjectID(source.Tree) {
		return SourceMetadata{}, fmt.Errorf("benchmark source tree is unavailable for commit %s; run from a verified Git checkout", source.Commit)
	}
	return source, nil
}

// CurrentCommit returns the verified source revision and rejects a dirty
// Git worktree. It retains the legacy ability to return a full commit
// override for an exported tree, but benchmark recording should use
// RequireCleanSource because an exported tree has no verifiable tree hash.
func CurrentCommit() (string, error) {
	source, err := CurrentSource()
	if err != nil {
		return "", err
	}
	if source.Dirty {
		return "", fmt.Errorf("benchmark source is dirty at commit %s", source.Commit)
	}
	return source.Commit, nil
}

func benchmarkCommitOverride() (string, error) {
	value, set := os.LookupEnv("CONTAINERGO_BENCH_COMMIT")
	if !set {
		return "", nil
	}
	if value != strings.TrimSpace(value) {
		return "", fmt.Errorf("CONTAINERGO_BENCH_COMMIT must not contain surrounding whitespace")
	}
	if value == "" {
		return "", nil
	}
	if !validGitObjectID(value) {
		return "", fmt.Errorf("CONTAINERGO_BENCH_COMMIT=%q is not a full Git object ID", value)
	}
	return value, nil
}

func validGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func gitOutput(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	if err == nil {
		return string(out), nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(exitErr.Stderr)))
	}
	return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
}
