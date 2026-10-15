package images

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// EnvEngineBinary overrides the container engine CLI, which defaults to
// `docker` on PATH. It is the hook for pointing the rules at a wrapper script.
const EnvEngineBinary = "RULES_DOCKER_COMPOSE_ENGINE_BINARY"

// EnvDebug enables verbose logging to stderr.
const EnvDebug = "RULES_DOCKER_COMPOSE_DEBUG"

// EndpointEnv lists the variables the Docker CLI and compose use to pick a
// daemon. docker_compose_test inherits them from the invoking shell so a
// context, a rootless daemon or Docker Desktop on Linux behaves the same under
// `bazel test` as it does in a terminal.
var EndpointEnv = []string{
	"DOCKER_HOST",
	"DOCKER_CONTEXT",
	"DOCKER_CONFIG",
	"DOCKER_CERT_PATH",
	"DOCKER_TLS_VERIFY",
	"DOCKER_API_VERSION",
}

// Debugf logs to stderr when EnvDebug is set.
func Debugf(format string, args ...any) {
	if os.Getenv(EnvDebug) != "" {
		fmt.Fprintf(os.Stderr, "[DEBUG] "+format+"\n", args...)
	}
}

// Engine drives the container engine CLI.
type Engine struct {
	Binary string
}

// NewEngine resolves the engine binary from the environment.
func NewEngine() *Engine {
	binary := os.Getenv(EnvEngineBinary)
	if binary == "" {
		binary = "docker"
	}
	return &Engine{Binary: binary}
}

// Command builds an engine invocation.
func (e *Engine) Command(args ...string) *exec.Cmd {
	return exec.Command(e.Binary, args...)
}

// output runs the engine and returns trimmed stdout, folding stderr into the
// error so callers can surface the daemon's own message.
func (e *Engine) output(args ...string) (string, error) {
	cmd := e.Command(args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	Debugf("Running: %s %s", e.Binary, strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("`%s %s` failed: %s", e.Binary, strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// EndpointDescription names the daemon the CLI will talk to, for messages.
func (e *Engine) EndpointDescription() string {
	var parts []string
	for _, key := range []string{"DOCKER_HOST", "DOCKER_CONTEXT"} {
		if v := os.Getenv(key); v != "" {
			parts = append(parts, key+"="+v)
		}
	}
	if ctx, err := e.output("context", "show"); err == nil && ctx != "" {
		parts = append(parts, "context="+ctx)
	}
	if len(parts) == 0 {
		return "the default endpoint"
	}
	return strings.Join(parts, " ")
}

// Preflight confirms the daemon is reachable and returns an actionable error
// when it is not. The underlying failure is otherwise a bare connection error
// from deep inside compose or a loader.
func (e *Engine) Preflight() error {
	if _, err := exec.LookPath(e.Binary); err != nil {
		return fmt.Errorf(
			"container engine binary %q was not found on PATH (%s).\n"+
				"Bazel tests run with a fixed PATH; pass --action_env=PATH or set %s to the full path of the engine.",
			e.Binary, os.Getenv("PATH"), EnvEngineBinary)
	}
	if _, err := e.output("version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf(
			"could not reach the container engine via %s.\n"+
				"The test inherits DOCKER_HOST, DOCKER_CONTEXT and DOCKER_CONFIG from the invoking shell; "+
				"if your daemon is selected another way, export one of those or pass it with --test_env.\n"+
				"Underlying error: %v",
			e.EndpointDescription(), err)
	}
	return nil
}

// ImageInfo is the subset of `docker image inspect` the verification needs.
type ImageInfo struct {
	ID           string   `json:"Id"`
	RepoTags     []string `json:"RepoTags"`
	OS           string   `json:"Os"`
	Architecture string   `json:"Architecture"`
	Variant      string   `json:"Variant"`
	RootFS       struct {
		Layers []string `json:"Layers"`
	} `json:"RootFS"`
}

// InspectImage returns the daemon's view of an image reference or ID.
func (e *Engine) InspectImage(ref string) (*ImageInfo, error) {
	out, err := e.output("image", "inspect", ref, "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	var info ImageInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return nil, fmt.Errorf("failed to parse `image inspect %s` output: %w", ref, err)
	}
	return &info, nil
}

// ResolveID finds an image by any of the given digests and returns the ID the
// daemon uses for it. Which digest resolves depends on the image store.
func (e *Engine) ResolveID(digests ...string) (string, bool) {
	for _, d := range digests {
		if d == "" {
			continue
		}
		if id, err := e.output("image", "inspect", d, "--format", "{{.Id}}"); err == nil && id != "" {
			return id, true
		}
	}
	return "", false
}

// Load imports a tarball into the daemon.
func (e *Engine) Load(tarPath string) error {
	out, err := e.output("load", "--input", tarPath)
	if err != nil {
		return err
	}
	Debugf("%s", out)
	return nil
}

// Tag points ref at the image with the given ID.
func (e *Engine) Tag(id, ref string) error {
	_, err := e.output("tag", id, ref)
	return err
}

// Verify checks that what the daemon holds under a reference is the image the
// merger recorded. The ID must be one of the two digests a load can produce,
// and the layers and platform must match exactly.
func Verify(img *Image, info *ImageInfo) error {
	var problems []string
	if info.ID != img.ManifestDigest && info.ID != img.ConfigDigest {
		problems = append(problems, fmt.Sprintf(
			"image ID %s is neither the manifest digest %s nor the config digest %s",
			info.ID, img.ManifestDigest, img.ConfigDigest))
	}
	if !slices.Equal(info.RootFS.Layers, img.DiffIDs) {
		problems = append(problems, fmt.Sprintf(
			"layers differ: daemon has %d (%s), build produced %d (%s)",
			len(info.RootFS.Layers), summarize(info.RootFS.Layers), len(img.DiffIDs), summarize(img.DiffIDs)))
	}
	if info.OS != img.Platform.OS || info.Architecture != img.Platform.Architecture {
		problems = append(problems, fmt.Sprintf(
			"platform differs: daemon has %s/%s, build produced %s",
			info.OS, info.Architecture, PlatformString(img.Platform)))
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("image %s does not match the build output:\n  %s", img.Label, strings.Join(problems, "\n  "))
}

func summarize(digests []string) string {
	if len(digests) == 0 {
		return "none"
	}
	short := make([]string, 0, len(digests))
	for _, d := range digests {
		d = strings.TrimPrefix(d, "sha256:")
		if len(d) > 12 {
			d = d[:12]
		}
		short = append(short, d)
	}
	return strings.Join(short, ",")
}

// Ensure makes an image available under its unique tags and any extra tags,
// and returns the daemon's ID for it.
//
// The image is located by digest, never by a user-visible name, so nothing
// another test or developer did to a tag can leak in. Loading is skipped when
// the content is already present. Every tag is then re-pointed at the ID and
// the result is verified against the recorded identity.
func (e *Engine) Ensure(img *Image, tarPath string, extraTags ...string) (string, error) {
	if len(img.UniqueTags) == 0 {
		return "", fmt.Errorf("image %s has no unique tags", img.Label)
	}
	id, found := e.ResolveID(img.ManifestDigest, img.ConfigDigest)
	if found {
		Debugf("Image %s already present as %s", img.Label, id)
	} else {
		Debugf("Loading %s from %s", img.Label, tarPath)
		if err := e.Load(tarPath); err != nil {
			return "", fmt.Errorf("failed to load image %s: %w", img.Label, err)
		}
		id, found = e.ResolveID(img.ManifestDigest, img.ConfigDigest)
		if !found {
			return "", fmt.Errorf(
				"loaded %s into %s, but neither its manifest digest %s nor its config digest %s resolves afterwards. "+
					"The load and the lookup may be reaching different daemons",
				img.Label, e.EndpointDescription(), img.ManifestDigest, img.ConfigDigest)
		}
	}
	for _, tag := range slices.Concat(img.UniqueTags, extraTags) {
		if err := e.Tag(id, tag); err != nil {
			return "", fmt.Errorf("failed to tag %s as %s: %w", img.Label, tag, err)
		}
	}
	info, err := e.InspectImage(img.UniqueTags[0])
	if err != nil {
		return "", fmt.Errorf("failed to inspect %s after tagging: %w", img.UniqueTags[0], err)
	}
	if err := Verify(img, info); err != nil {
		return "", err
	}
	if info.ID != id {
		return "", fmt.Errorf("tag %s resolved to %s immediately after being pointed at %s", img.UniqueTags[0], info.ID, id)
	}
	return id, nil
}

// Container is one entry of `docker compose ps --format json`.
type Container struct {
	ID         string      `json:"ID"`
	Name       string      `json:"Name"`
	Service    string      `json:"Service"`
	State      string      `json:"State"`
	Health     string      `json:"Health"`
	ExitCode   int         `json:"ExitCode"`
	Image      string      `json:"Image"`
	Publishers []Publisher `json:"Publishers"`
}

// Publisher is one published port of a container as reported by compose.
type Publisher struct {
	URL           string `json:"URL"`
	TargetPort    int    `json:"TargetPort"`
	PublishedPort int    `json:"PublishedPort"`
	Protocol      string `json:"Protocol"`
}

// ContainerImage is one entry of `docker compose images --format json`.
type ContainerImage struct {
	ID            string `json:"ID"`
	ContainerName string `json:"ContainerName"`
	Repository    string `json:"Repository"`
	Tag           string `json:"Tag"`
}

// ParseComposeImages decodes `docker compose images --format json`.
func ParseComposeImages(out []byte) ([]ContainerImage, error) {
	var entries []ContainerImage
	if err := json.Unmarshal(bytes.TrimSpace(out), &entries); err != nil {
		return nil, fmt.Errorf("failed to parse compose images: %w", err)
	}
	return entries, nil
}

// ParseComposePS decodes `docker compose ps --format json`. Compose emitted a
// JSON array before 2.21.0 and one object per line since, so both are accepted.
func ParseComposePS(out []byte) ([]Container, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var containers []Container
		if err := json.Unmarshal(trimmed, &containers); err != nil {
			return nil, fmt.Errorf("failed to parse compose ps array: %w", err)
		}
		return containers, nil
	}
	var containers []Container
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	for {
		var c Container
		err := dec.Decode(&c)
		if errors.Is(err, io.EOF) {
			return containers, nil
		}
		if err != nil {
			return nil, fmt.Errorf("failed to parse compose ps line: %w", err)
		}
		containers = append(containers, c)
	}
}
