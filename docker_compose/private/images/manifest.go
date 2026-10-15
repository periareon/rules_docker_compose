// Package images is the shared contract between the build-time merger and the
// test-time runner and launcher for loader-backed container images.
//
// At build time the merger derives, for every image a compose file references,
// the identities that survive a `docker load`: the OCI manifest digest, the
// image config digest and the layer DiffIDs. It writes them to a runtime
// manifest alongside a dual-format tarball of the image. At test time the
// runner loads that tarball, tags the resulting image by digest rather than by
// any user-visible name, and verifies both the image and the containers started
// from it against the recorded identities.
package images

import (
	"encoding/json"
	"fmt"
	"os"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Image is one loader-backed image as recorded by the merger.
type Image struct {
	// Label of the loader target the image came from, for messages.
	Label string `json:"label"`
	// TarRlocation is the runfiles path of the dual-format tarball.
	TarRlocation string `json:"tar_rlocationpath"`
	// UniqueTags are the content-derived names the test yaml references. There
	// is one per original tag, since original tags may live in different
	// repositories. The first one is also baked into the tarball.
	UniqueTags []string `json:"unique_tags"`
	// OriginalTags are the names the user declared on the loader target. The
	// runner never reads them; the launcher applies them for `bazel run`.
	OriginalTags []string `json:"original_tags"`
	// ManifestDigest is the digest of the OCI image manifest. Docker's
	// containerd image store reports it as the image ID after a load.
	ManifestDigest string `json:"manifest_digest"`
	// ConfigDigest is the digest of the image config blob. Docker's classic
	// graph-driver store reports it as the image ID after a load.
	ConfigDigest string `json:"config_digest"`
	// DiffIDs are the uncompressed layer digests, in order. Every store
	// preserves them, so they are the cross-check that does not depend on
	// which ID the daemon chose.
	DiffIDs []string `json:"diff_ids"`
	// Platform the manifest was selected for.
	Platform ocispec.Platform `json:"platform"`
}

// RuntimeManifest is written by the merger and read by the runner and launcher.
type RuntimeManifest struct {
	// Project is the compose project name derived from the target label. The
	// runner appends a per-run suffix so concurrent runs of one target, or a
	// run overlapping a crashed predecessor, never share containers or
	// networks.
	Project string  `json:"project"`
	Images  []Image `json:"images"`
	// Services maps each compose service backed by a loader to the unique tag
	// its `image:` was rewritten to in the test yaml.
	Services map[string]string `json:"services"`
	// Ports lists every container port the compose file publishes. In the
	// test yaml the host side is left to the daemon unless the target opted
	// out, so the runner looks the real host port up after startup and hands
	// it to the test through the environment.
	Ports []PortMapping `json:"ports,omitempty"`
	// RegistryImages are references compose would pull from a registry because
	// no loader provides them. A yaml-only target may carry these; a test may
	// run with them only when AllowRegistryImages is set.
	RegistryImages []string `json:"registry_images,omitempty"`
	// AllowRegistryImages records the `allow_registry_images` attribute.
	AllowRegistryImages bool `json:"allow_registry_images,omitempty"`
}

// ReadRuntimeManifest parses a runtime manifest from disk.
func ReadRuntimeManifest(path string) (*RuntimeManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read runtime manifest %s: %w", path, err)
	}
	var m RuntimeManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("failed to parse runtime manifest %s: %w", path, err)
	}
	if m.Services == nil {
		m.Services = map[string]string{}
	}
	return &m, nil
}

// ImageForTag finds the image that owns a unique tag.
func (m *RuntimeManifest) ImageForTag(tag string) (*Image, bool) {
	for i := range m.Images {
		for _, t := range m.Images[i].UniqueTags {
			if t == tag {
				return &m.Images[i], true
			}
		}
	}
	return nil, false
}
