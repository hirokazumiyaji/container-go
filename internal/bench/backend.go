package bench

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// Stable keys used in Env.CLIs. Docker client and server versions are
// intentionally separate values.
const (
	DockerClientVersionKey = "docker.client"
	DockerServerVersionKey = "docker.server"
	AppleClientVersionKey  = "apple.client"
	AppleServiceVersionKey = "apple.service"
)

// ErrImageNotFound is returned internally when an image inspection cannot
// find the requested reference. Backend.ImageExists translates this into
// (false, nil), while callers that need the distinction can inspect errors
// from Backend.ImageIdentity or Backend.ImageDigest.
var ErrImageNotFound = errors.New("benchmark image not found")

// ErrContainerNotFound is returned when a named benchmark container does
// not exist. It is distinct from daemon or CLI operational failures.
var ErrContainerNotFound = errors.New("benchmark container not found")

// ImageIdentity is the information needed to prove that an image reference
// and all of its known content aliases are gone. ContentID is a backend
// image ID when one is available; Digest is a repository digest. Tags are
// the mutable references that pointed at the same content at inspection
// time.
type ImageIdentity struct {
	Reference string
	ContentID string
	Digest    string
	Tags      []string
}

// ContainerIdentity is the small part of a backend container inspect result
// used to verify a testcontainers reaper. The reaper is deliberately
// inspected by its backend identity rather than inferred from the requested
// image tag.
type ContainerIdentity struct {
	ID             string
	Name           string
	ImageReference string
	ImageID        string
	Labels         map[string]string
}

// BackendProvenance identifies the effective backend selected by the CLI.
// Docker fields are populated only for Docker results; Apple results leave
// them empty so a mixed document cannot silently compare different daemons.
type BackendProvenance struct {
	Endpoint   string
	Context    string
	DaemonID   string
	DaemonOS   string
	DaemonArch string
}

// commandError preserves stderr from a command that failed. exec.Command's
// Output method only exposes stderr on some Go versions when Stderr is nil,
// so the benchmark wrapper records it explicitly for reliable
// classification.
type commandError struct {
	err    error
	stderr string
}

func (e *commandError) Error() string {
	if e.stderr == "" {
		return e.err.Error()
	}
	return fmt.Sprintf("%v: %s", e.err, e.stderr)
}

func (e *commandError) Unwrap() error { return e.err }

func runCommand(bin string, args ...string) ([]byte, error) {
	cmd := exec.Command(bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, &commandError{err: err, stderr: strings.TrimSpace(stderr.String())}
	}
	return out, nil
}

func commandStderr(err error) string {
	var commandErr *commandError
	if errors.As(err, &commandErr) {
		return commandErr.stderr
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return strings.TrimSpace(string(exitErr.Stderr))
	}
	return ""
}

// isImageNotFoundError recognizes backend-specific missing-image wording,
// but never treats a generic operational failure as an absent image.
func isImageNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrImageNotFound) {
		return true
	}
	text := strings.ToLower(commandStderr(err) + " " + err.Error())
	return strings.Contains(text, "no such image") ||
		strings.Contains(text, "image not found") ||
		(strings.Contains(text, "no such object") && strings.Contains(text, "image"))
}

func isContainerNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrContainerNotFound) {
		return true
	}
	text := strings.ToLower(commandStderr(err) + " " + err.Error())
	return strings.Contains(text, "no such container") ||
		(strings.Contains(text, "no such object") && strings.Contains(text, "container")) ||
		strings.Contains(text, "container not found")
}

func inspectImage(run func(...string) ([]byte, error), image string, args ...string) ([]byte, error) {
	out, err := run(args...)
	if err != nil {
		if isImageNotFoundError(err) {
			return nil, fmt.Errorf("%w: %s", ErrImageNotFound, image)
		}
		return nil, fmt.Errorf("inspect image %s: %w", image, err)
	}
	return out, nil
}

func parseImagePresent(data []byte) (bool, error) {
	var images []json.RawMessage
	if err := json.Unmarshal(data, &images); err != nil {
		return false, fmt.Errorf("decode image inspect output: %w", err)
	}
	if images == nil {
		return false, fmt.Errorf("decode image inspect output: expected an array, got null")
	}
	return len(images) > 0, nil
}

func parseDockerImageIdentity(data []byte, image string) (ImageIdentity, error) {
	var records []struct {
		ID          string   `json:"Id"`
		RepoTags    []string `json:"RepoTags"`
		RepoDigests []string `json:"RepoDigests"`
	}
	if err := json.Unmarshal(data, &records); err != nil {
		return ImageIdentity{}, fmt.Errorf("decode Docker image identity for %s: %w", image, err)
	}
	if len(records) != 1 {
		return ImageIdentity{}, fmt.Errorf("docker image identity for %s returned %d records", image, len(records))
	}
	record := records[0]
	identity := ImageIdentity{
		Reference: image,
		ContentID: record.ID,
	}
	for _, tag := range record.RepoTags {
		if tag != "" && !strings.Contains(tag, "<none>") {
			identity.Tags = append(identity.Tags, tag)
		}
	}
	if len(record.RepoDigests) > 0 {
		encoded, err := json.Marshal(record.RepoDigests)
		if err == nil {
			identity.Digest, _ = dockerRepoDigest(image, encoded)
		}
	}
	if identity.ContentID == "" && identity.Digest == "" {
		return ImageIdentity{}, fmt.Errorf("docker image identity for %s has no content ID or repository digest", image)
	}
	return identity, nil
}

func parseAppleImageIdentity(data []byte, image string) (ImageIdentity, error) {
	var records []struct {
		ID         string `json:"id"`
		Reference  string `json:"reference"`
		Descriptor struct {
			Digest string `json:"digest"`
		} `json:"descriptor"`
		Configuration struct {
			Image struct {
				Reference  string `json:"reference"`
				Descriptor struct {
					Digest string `json:"digest"`
				} `json:"descriptor"`
			} `json:"image"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal(data, &records); err != nil {
		return ImageIdentity{}, fmt.Errorf("decode Apple image identity for %s: %w", image, err)
	}
	if len(records) != 1 {
		return ImageIdentity{}, fmt.Errorf("apple image identity for %s returned %d records", image, len(records))
	}
	record := records[0]
	identity := ImageIdentity{Reference: image, ContentID: record.ID, Digest: record.Descriptor.Digest}
	if identity.Digest == "" {
		identity.Digest = record.Configuration.Image.Descriptor.Digest
	}
	if record.Reference != "" {
		identity.Reference = record.Reference
	} else if record.Configuration.Image.Reference != "" {
		identity.Reference = record.Configuration.Image.Reference
	}
	if identity.ContentID == "" && identity.Digest == "" {
		return ImageIdentity{}, fmt.Errorf("apple image identity for %s has no content ID or descriptor digest", image)
	}
	return identity, nil
}

func parseDockerContainerIdentity(data []byte, requested string) (ContainerIdentity, error) {
	var records []struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Image  string `json:"Image"`
		Config struct {
			Image  string            `json:"Image"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(data, &records); err != nil {
		return ContainerIdentity{}, fmt.Errorf("decode Docker container identity for %s: %w", requested, err)
	}
	if len(records) != 1 {
		return ContainerIdentity{}, fmt.Errorf("docker container identity for %s returned %d records", requested, len(records))
	}
	record := records[0]
	return ContainerIdentity{
		ID:             record.ID,
		Name:           strings.TrimPrefix(record.Name, "/"),
		ImageReference: record.Config.Image,
		ImageID:        record.Image,
		Labels:         record.Config.Labels,
	}, nil
}

// Backend wraps the engine-specific operations the scenarios need
// outside the timed region: probing the service, and inspecting,
// pulling, and removing images.
type Backend struct {
	// Name is the backend identifier: "apple" or "docker".
	Name string
	// Bin is the CLI binary of the backend.
	Bin string
	// VersionArgs prints the backend client version.
	VersionArgs []string
	// ServerVersionArgs prints the Docker server version. It is empty for
	// backends that do not expose an independent server version.
	ServerVersionArgs []string
	// ServiceVersionArgs prints an optional backend service version.
	ServiceVersionArgs []string
	// ServiceVersionOptional permits older Apple CLIs that predate
	// "container system version", but not operational failures.
	ServiceVersionOptional bool
	// Probe reports whether the backend's service answers.
	Probe func() error
	// ImageExists reports whether the image is in the local store. A
	// missing image is (false, nil); all other failures are returned.
	ImageExists func(image string) (bool, error)
	// ImageIdentity returns the content and tag aliases for an image.
	// It returns ErrImageNotFound when the reference is absent.
	ImageIdentity func(image string) (ImageIdentity, error)
	// ImageID returns the backend's immutable content ID for a reference.
	ImageID func(image string) (string, error)
	// RemoveImage drops the image from the local store.
	RemoveImage func(image string) error
	// PullImage fetches the image into the local store.
	PullImage func(image string) error
	// ImageDigest returns the repository digest currently resolved for an
	// image. It is used to verify locally cached mutable tags.
	ImageDigest func(image string) (string, error)
	// TagImage points target at the already-present source image.
	TagImage func(source, target string) error
	// ContainerInspect returns an identity for a named container. It is
	// used only by the Docker/testcontainers benchmark.
	ContainerInspect func(name string) (ContainerIdentity, error)
	// Provenance records the effective endpoint and daemon identity.
	Provenance func() (BackendProvenance, error)
}

// CaptureProvenance returns the backend-specific effective provenance.
// Backends without a provenance operation return an empty value, except for
// Docker where omitting provenance would make a strict result unverifiable.
func (b Backend) CaptureProvenance() (BackendProvenance, error) {
	if b.Provenance == nil {
		if b.Name == "docker" {
			return BackendProvenance{}, fmt.Errorf("docker provenance operation is unavailable")
		}
		return BackendProvenance{}, nil
	}
	return b.Provenance()
}

// ObserveImage returns the backend-observed content identity for image. The
// benchmark uses this after the timed region instead of copying the policy
// digest into the result.
func (b Backend) ObserveImage(image string) (ImageIdentity, error) {
	return b.imageIdentity(image)
}

// DockerBackend returns the harness for the docker CLI.
func DockerBackend() Backend {
	run := func(args ...string) ([]byte, error) { return runCommand("docker", args...) }
	inspect := func(image string) ([]byte, error) {
		return inspectImage(run, image, "image", "inspect", image)
	}
	imageIdentity := func(image string) (ImageIdentity, error) {
		out, err := inspect(image)
		if err != nil {
			return ImageIdentity{}, err
		}
		return parseDockerImageIdentity(out, image)
	}
	return Backend{
		Name:              "docker",
		Bin:               "docker",
		VersionArgs:       []string{"version", "--format", "{{.Client.Version}}"},
		ServerVersionArgs: []string{"version", "--format", "{{.Server.Version}}"},
		Probe: func() error {
			_, err := run("version")
			return err
		},
		ImageExists: func(image string) (bool, error) {
			out, err := inspect(image)
			if err != nil {
				if errors.Is(err, ErrImageNotFound) {
					return false, nil
				}
				return false, err
			}
			return parseImagePresent(out)
		},
		ImageIdentity: imageIdentity,
		ImageID: func(image string) (string, error) {
			out, err := inspect(image)
			if err != nil {
				return "", err
			}
			var records []struct {
				ID string `json:"Id"`
			}
			if err := json.Unmarshal(out, &records); err != nil || len(records) != 1 || records[0].ID == "" {
				if err != nil {
					return "", fmt.Errorf("decode Docker image ID for %s: %w", image, err)
				}
				return "", fmt.Errorf("docker image ID for %s is empty or ambiguous", image)
			}
			return records[0].ID, nil
		},
		RemoveImage: func(image string) error {
			_, err := run("rmi", "-f", image)
			return err
		},
		PullImage: func(image string) error {
			_, err := run("pull", "--quiet", image)
			return err
		},
		ImageDigest: func(image string) (string, error) {
			identity, err := imageIdentity(image)
			if err != nil {
				return "", err
			}
			if identity.Digest == "" {
				return "", fmt.Errorf("image %s has no repository digest", image)
			}
			return identity.Digest, nil
		},
		TagImage: func(source, target string) error {
			_, err := run("tag", source, target)
			return err
		},
		ContainerInspect: func(name string) (ContainerIdentity, error) {
			out, err := run("inspect", "--type", "container", name)
			if err != nil {
				if isContainerNotFoundError(err) {
					return ContainerIdentity{}, fmt.Errorf("%w: %s", ErrContainerNotFound, name)
				}
				return ContainerIdentity{}, fmt.Errorf("inspect Docker container %s: %w", name, err)
			}
			return parseDockerContainerIdentity(out, name)
		},
		Provenance: func() (BackendProvenance, error) {
			return captureDockerProvenance(run)
		},
	}
}

// AppleBackend returns the harness for the Apple container CLI.
func AppleBackend() Backend {
	run := func(args ...string) ([]byte, error) { return runCommand("container", args...) }
	inspect := func(image string) ([]byte, error) {
		return inspectImage(run, image, "image", "inspect", image)
	}
	return Backend{
		Name:                   "apple",
		Bin:                    "container",
		VersionArgs:            []string{"--version"},
		ServiceVersionArgs:     []string{"system", "version", "--format", "json"},
		ServiceVersionOptional: true,
		Probe: func() error {
			_, err := run("system", "status")
			return err
		},
		ImageExists: func(image string) (bool, error) {
			out, err := inspect(image)
			if err != nil {
				if errors.Is(err, ErrImageNotFound) {
					return false, nil
				}
				return false, err
			}
			return parseImagePresent(out)
		},
		ImageIdentity: func(image string) (ImageIdentity, error) {
			out, err := inspect(image)
			if err != nil {
				return ImageIdentity{}, err
			}
			return parseAppleImageIdentity(out, image)
		},
		ImageID: func(image string) (string, error) {
			out, err := inspect(image)
			if err != nil {
				return "", err
			}
			identity, err := parseAppleImageIdentity(out, image)
			if err != nil {
				return "", err
			}
			if identity.ContentID == "" {
				return "", fmt.Errorf("apple image ID for %s is unavailable", image)
			}
			return identity.ContentID, nil
		},
		RemoveImage: func(image string) error {
			_, err := run("image", "delete", image)
			return err
		},
		PullImage: func(image string) error {
			_, err := run("image", "pull", image)
			return err
		},
	}
}

// Versions records independently identifiable client and service/daemon
// versions under stable Env.CLIs keys.
func (b Backend) Versions() (map[string]string, error) {
	versions := make(map[string]string)
	run := func(args ...string) (string, error) {
		out, err := runCommand(b.Bin, args...)
		if err != nil {
			return "", err
		}
		value := normalizeVersionOutput(out)
		if value == "" {
			return "", fmt.Errorf("%s %s returned an empty version", b.Bin, strings.Join(args, " "))
		}
		return value, nil
	}

	switch b.Name {
	case "docker":
		client, err := run(b.VersionArgs...)
		if err != nil {
			return nil, fmt.Errorf("record Docker client version: %w", err)
		}
		server, err := run(b.ServerVersionArgs...)
		if err != nil {
			return nil, fmt.Errorf("record Docker server version: %w", err)
		}
		versions[DockerClientVersionKey] = client
		versions[DockerServerVersionKey] = server
	case "apple":
		client, err := run(b.VersionArgs...)
		if err != nil {
			return nil, fmt.Errorf("record Apple client version: %w", err)
		}
		versions[AppleClientVersionKey] = client
		if len(b.ServiceVersionArgs) > 0 {
			out, serviceErr := runCommand(b.Bin, b.ServiceVersionArgs...)
			if serviceErr != nil {
				if b.ServiceVersionOptional && isUnsupportedServiceVersionError(serviceErr) {
					break
				}
				return nil, fmt.Errorf("record Apple service version: %w", serviceErr)
			}
			service, err := parseAppleServiceVersion(out)
			if err != nil {
				return nil, err
			}
			versions[AppleServiceVersionKey] = service
		}
	default:
		return nil, fmt.Errorf("record versions: unsupported backend %q", b.Name)
	}
	return versions, nil
}

func normalizeVersionOutput(out []byte) string {
	value := strings.TrimSpace(string(out))
	if value == "" {
		return ""
	}
	if json.Valid([]byte(value)) {
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(value)); err == nil {
			return compact.String()
		}
	}
	return strings.Join(strings.Fields(value), " ")
}

func isUnsupportedServiceVersionError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(commandStderr(err) + " " + err.Error())
	for _, phrase := range []string{
		"unknown command",
		"unrecognized command",
		"unknown flag",
		"unrecognized flag",
		"command not found",
		"does not support",
		"not supported",
		"unsupported command",
		"unsupported subcommand",
		"unsupported flag",
	} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func dockerRepoDigest(image string, output []byte) (string, error) {
	var references []string
	if err := json.Unmarshal(output, &references); err != nil {
		return "", fmt.Errorf("decode repository digests for %s: %w", image, err)
	}
	repository := imageRepository(image)
	prefix := repository + "@"
	for _, reference := range references {
		if strings.HasPrefix(reference, prefix) {
			digest := strings.TrimPrefix(reference, prefix)
			if validSHA256Digest(digest) {
				return digest, nil
			}
		}
	}
	return "", fmt.Errorf("image %s has no valid %s digest", image, repository)
}

func imageRepository(image string) string {
	repository := image
	if before, _, ok := strings.Cut(repository, "@"); ok {
		repository = before
	} else if slash := strings.LastIndex(repository, "/"); slash >= 0 {
		if colon := strings.LastIndex(repository, ":"); colon > slash {
			repository = repository[:colon]
		}
	} else if colon := strings.LastIndex(repository, ":"); colon >= 0 {
		repository = repository[:colon]
	}
	return repository
}

// Available skips the test when the backend's CLI or service is
// missing, or when CONTAINERGO_BACKEND selects a different backend.
func (b Backend) Available(tb testing.TB) {
	tb.Helper()
	selected := os.Getenv("CONTAINERGO_BACKEND")
	if err := validateBackendSelection(selected, b.Name); err != nil {
		tb.Fatal(err)
	}
	if selected != "" && selected != b.Name {
		tb.Skipf("CONTAINERGO_BACKEND=%s; skipping %s", selected, b.Name)
	}
	if _, err := exec.LookPath(b.Bin); err != nil {
		tb.Skipf("%s CLI not installed", b.Name)
	}
	if err := b.Probe(); err != nil {
		tb.Skipf("%s backend service not running: %v", b.Name, err)
	}
}

func validateBackendSelection(selected, backend string) error {
	switch selected {
	case "", backend:
		return nil
	case "apple", "docker":
		return nil // caller should skip the non-selected backend
	default:
		return fmt.Errorf("invalid CONTAINERGO_BACKEND=%q: valid values are \"apple\" and \"docker\"", selected)
	}
}

// EnsureImage guarantees the image is present before a timed
// iteration; preparation never lands in the measurement.
func (b Backend) EnsureImage(tb testing.TB, image string) {
	tb.Helper()
	if b.ImageExists == nil {
		tb.Fatalf("image existence operation is unavailable for %s", b.Name)
	}
	ok, err := b.ImageExists(image)
	if err != nil {
		tb.Fatalf("inspect image %s: %v", image, err)
	}
	if ok {
		return
	}
	if b.PullImage == nil {
		tb.Fatalf("image pull operation is unavailable for %s", b.Name)
	}
	if err := b.PullImage(image); err != nil {
		tb.Fatalf("pull %s: %v", image, err)
	}
	ok, err = b.ImageExists(image)
	if err != nil {
		tb.Fatalf("verify pulled image %s: %v", image, err)
	}
	if !ok {
		tb.Fatalf("pull %s reported success but image is still absent", image)
	}
}

// EnsureImageAbsent removes the image before a cold iteration and proves
// that the requested reference, its content ID/digest, and every tag known
// before removal are absent. A remove error is tolerated only after that
// verification succeeds.
func (b Backend) EnsureImageAbsent(tb testing.TB, image string) {
	tb.Helper()
	identity, identityErr := b.imageIdentity(image)
	if identityErr != nil && !errors.Is(identityErr, ErrImageNotFound) {
		tb.Fatalf("inspect image identity %s: %v", image, identityErr)
	}
	if errors.Is(identityErr, ErrImageNotFound) {
		identity = ImageIdentity{}
	}
	if b.RemoveImage == nil {
		tb.Fatal("image removal operation is unavailable")
	}
	removeErr := b.RemoveImage(image)
	verifyErr := b.verifyImageAbsent(image, identity)
	if verifyErr != nil {
		if removeErr != nil {
			tb.Fatalf("remove %s: %v; verify absence: %v", image, removeErr, verifyErr)
		}
		tb.Fatal(verifyErr)
	}
	if removeErr != nil {
		if !isImageNotFoundError(removeErr) {
			tb.Fatalf("remove %s: %v", image, removeErr)
		}
		tb.Logf("remove %s reported a not-found error and image content and tags are absent: %v", image, removeErr)
	}
}

func (b Backend) imageIdentity(image string) (ImageIdentity, error) {
	if b.ImageIdentity != nil {
		return b.ImageIdentity(image)
	}
	if b.ImageDigest != nil {
		digest, err := b.ImageDigest(image)
		if err != nil {
			return ImageIdentity{}, err
		}
		if !validSHA256Digest(digest) {
			return ImageIdentity{}, fmt.Errorf("image %s has invalid repository digest %q", image, digest)
		}
		return ImageIdentity{Reference: image, Digest: digest}, nil
	}
	return ImageIdentity{}, fmt.Errorf("backend %s cannot establish image content identity", b.Name)
}

func (b Backend) verifyImageAbsent(image string, identity ImageIdentity) error {
	if b.ImageExists == nil {
		return fmt.Errorf("cannot verify image %s absence: image existence operation is unavailable", image)
	}
	refs := map[string]struct{}{image: {}}
	if identity.ContentID != "" {
		refs[identity.ContentID] = struct{}{}
	}
	if identity.Digest != "" {
		if !validSHA256Digest(identity.Digest) {
			return fmt.Errorf("image %s has invalid content digest %q", image, identity.Digest)
		}
		reference := identity.Reference
		if reference == "" {
			reference = image
		}
		refs[imageRepository(reference)+"@"+identity.Digest] = struct{}{}
	}
	for _, tag := range identity.Tags {
		if tag != "" && !strings.Contains(tag, "<none>") {
			refs[tag] = struct{}{}
		}
	}
	if identity.Reference != "" && identity.ContentID == "" && identity.Digest == "" {
		return fmt.Errorf("cannot verify content identity for image %s", image)
	}
	references := make([]string, 0, len(refs))
	for reference := range refs {
		references = append(references, reference)
	}
	sort.Strings(references)
	for _, reference := range references {
		exists, err := b.ImageExists(reference)
		if err != nil {
			return fmt.Errorf("verify image %s absence: %w", reference, err)
		}
		if exists {
			return fmt.Errorf("image %s is still present after removal", reference)
		}
	}
	return nil
}

// imageRemovalAccepted reports whether a RemoveImage failure is
// tolerable because the image is already absent. It remains as a small
// compatibility helper for callers that only have an existence probe.
func imageRemovalAccepted(exists func(string) (bool, error), image string, removeErr error) error {
	ok, checkErr := exists(image)
	if checkErr != nil {
		if isImageNotFoundError(checkErr) || errors.Is(checkErr, ErrImageNotFound) {
			return nil
		}
		return fmt.Errorf("remove %s: %w (and could not verify absence: %v)", image, removeErr, checkErr)
	}
	if ok {
		return fmt.Errorf("remove %s: %w (image still present)", image, removeErr)
	}
	return nil
}
