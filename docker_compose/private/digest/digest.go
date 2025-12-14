// Package digest provides utilities for reading container image digests.
//
// This package handles reading digests from OCI (Open Container Initiative) image
// layouts and manifest files, supporting both OCI manifest digests and Docker
// image IDs (manifest digests after OCI-to-Docker conversion).
//
// # OCI Image Manifest Specification
//
// The OCI Image Manifest specification defines the format for OCI container images:
// https://github.com/opencontainers/image-spec/blob/main/manifest.md
//
// # OCI Image Layout
//
// The OCI Image Layout specifies how OCI image content is stored on disk:
// https://github.com/opencontainers/image-spec/blob/main/image-layout.md
//
// # Docker Image ID
//
// When Docker loads an OCI image with `docker load`, it converts the OCI manifest
// to Docker V2 Schema 2 format and uses the SHA256 digest of the converted manifest
// as the image ID. This is visible in `docker images` output and `docker inspect`.
//
// See: https://docs.docker.com/registry/spec/manifest-v2-2/
package digest

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ociToDockerMediaTypes maps OCI media types to their Docker V2 equivalents.
// See: https://github.com/opencontainers/image-spec/blob/main/media-types.md
// See: https://docs.docker.com/registry/spec/manifest-v2-2/#media-types
var ociToDockerMediaTypes = map[string]string{
	"application/vnd.oci.image.manifest.v1+json":  "application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.oci.image.config.v1+json":    "application/vnd.docker.container.image.v1+json",
	"application/vnd.oci.image.layer.v1.tar+gzip": "application/vnd.docker.image.rootfs.diff.tar.gzip",
	"application/vnd.oci.image.layer.v1.tar":      "application/vnd.docker.image.rootfs.diff.tar",
	"application/vnd.oci.image.layer.v1.tar+zstd": "application/vnd.docker.image.rootfs.diff.tar+zstd",
}

// convertOCIToDockerMediaType converts an OCI media type to its Docker V2 equivalent.
func convertOCIToDockerMediaType(ociMediaType string) string {
	if dockerType, ok := ociToDockerMediaTypes[ociMediaType]; ok {
		return dockerType
	}
	return ociMediaType
}

// dockerManifest represents a Docker V2 Schema 2 manifest.
// Field order matches Docker's serialization for digest computation.
// See: https://docs.docker.com/registry/spec/manifest-v2-2/#image-manifest-field-descriptions
type dockerManifest struct {
	SchemaVersion int                        `json:"schemaVersion"`
	MediaType     string                     `json:"mediaType"`
	Config        dockerManifestDescriptor   `json:"config"`
	Layers        []dockerManifestDescriptor `json:"layers"`
}

// dockerManifestDescriptor represents a content descriptor in a Docker V2 manifest.
type dockerManifestDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// OCIManifest represents an OCI image manifest.
// See: https://github.com/opencontainers/image-spec/blob/main/manifest.md
type OCIManifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	MediaType     string `json:"mediaType"`
	Config        struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	} `json:"config"`
	Layers []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	} `json:"layers"`
}

// OCIIndex represents an OCI image index (index.json).
// See: https://github.com/opencontainers/image-spec/blob/main/image-index.md
type OCIIndex struct {
	Manifests []struct {
		Digest    string `json:"digest"`
		MediaType string `json:"mediaType"`
	} `json:"manifests"`
}

// ReadOCIManifestDigest reads the manifest digest from an OCI layout directory.
// This reads the first manifest digest from index.json.
//
// OCI Image Layout spec: https://github.com/opencontainers/image-spec/blob/main/image-layout.md
func ReadOCIManifestDigest(ociLayoutDir string) (string, error) {
	indexPath := filepath.Join(ociLayoutDir, "index.json")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return "", fmt.Errorf("failed to read OCI index.json from %s: %v", ociLayoutDir, err)
	}

	var index OCIIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return "", fmt.Errorf("failed to parse OCI index.json: %v", err)
	}

	if len(index.Manifests) == 0 {
		return "", fmt.Errorf("no manifests found in OCI layout")
	}

	digest := index.Manifests[0].Digest
	if !strings.HasPrefix(digest, "sha256:") {
		return "", fmt.Errorf("invalid digest format: %s (expected sha256:...)", digest)
	}

	return digest, nil
}

// ReadConfigDigestFromOCILayout reads the config digest from an OCI layout.
// This is the digest that Docker Engine (without containerd) uses as the image ID.
//
// OCI Image Layout spec: https://github.com/opencontainers/image-spec/blob/main/image-layout.md
func ReadConfigDigestFromOCILayout(ociLayoutDir string) (string, error) {
	// Read the OCI index to get the manifest digest
	ociManifestDigest, err := ReadOCIManifestDigest(ociLayoutDir)
	if err != nil {
		return "", err
	}

	// Read the OCI manifest blob
	manifestPath := filepath.Join(ociLayoutDir, "blobs", "sha256", ociManifestDigest[7:])
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", fmt.Errorf("failed to read manifest blob %s: %v", manifestPath, err)
	}

	// Parse OCI manifest to get the config digest
	var ociManifest OCIManifest
	if err := json.Unmarshal(manifestData, &ociManifest); err != nil {
		return "", fmt.Errorf("failed to parse OCI manifest: %v", err)
	}

	if ociManifest.Config.Digest == "" {
		return "", fmt.Errorf("no config digest found in OCI manifest")
	}

	return ociManifest.Config.Digest, nil
}

// ReadDockerContainerdImageIDFromOCILayout reads an OCI layout, converts the manifest
// to Docker V2 format, and returns the Docker manifest digest.
//
// This is the digest that Docker (with containerd storage) uses as the image ID
// after `docker load`. The conversion involves:
// 1. Reading the OCI index.json to find the manifest digest
// 2. Reading the OCI manifest blob
// 3. Converting media types from OCI to Docker format
// 4. Computing the SHA256 digest of the resulting Docker manifest
//
// OCI Image Layout spec: https://github.com/opencontainers/image-spec/blob/main/image-layout.md
// Docker V2 Schema 2 spec: https://docs.docker.com/registry/spec/manifest-v2-2/
func ReadDockerContainerdImageIDFromOCILayout(ociLayoutDir string) (string, error) {
	// Read the OCI index to get the manifest digest
	ociManifestDigest, err := ReadOCIManifestDigest(ociLayoutDir)
	if err != nil {
		return "", err
	}

	// Read the OCI manifest blob
	manifestPath := filepath.Join(ociLayoutDir, "blobs", "sha256", ociManifestDigest[7:])
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", fmt.Errorf("failed to read manifest blob %s: %v", manifestPath, err)
	}

	// Parse OCI manifest
	var ociManifest OCIManifest
	if err := json.Unmarshal(manifestData, &ociManifest); err != nil {
		return "", fmt.Errorf("failed to parse OCI manifest: %v", err)
	}

	// Convert to Docker V2 manifest
	dockerMfst := dockerManifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.docker.distribution.manifest.v2+json",
		Config: dockerManifestDescriptor{
			MediaType: convertOCIToDockerMediaType(ociManifest.Config.MediaType),
			Digest:    ociManifest.Config.Digest,
			Size:      ociManifest.Config.Size,
		},
		Layers: make([]dockerManifestDescriptor, len(ociManifest.Layers)),
	}

	for i, layer := range ociManifest.Layers {
		dockerMfst.Layers[i] = dockerManifestDescriptor{
			MediaType: convertOCIToDockerMediaType(layer.MediaType),
			Digest:    layer.Digest,
			Size:      layer.Size,
		}
	}

	// Serialize to compact JSON (no whitespace) to match Docker's format
	manifestJSON, err := json.Marshal(dockerMfst)
	if err != nil {
		return "", fmt.Errorf("failed to marshal Docker manifest: %v", err)
	}

	// Compute and return the Docker manifest digest
	hash := sha256.Sum256(manifestJSON)
	return fmt.Sprintf("sha256:%x", hash), nil
}

// ReadConfigDigestFromManifestFile reads the config digest from a manifest JSON file.
// This is used for rules_img images where the manifest file is provided directly.
func ReadConfigDigestFromManifestFile(manifestFile string) (string, error) {
	data, err := os.ReadFile(manifestFile)
	if err != nil {
		return "", fmt.Errorf("failed to read manifest file %s: %v", manifestFile, err)
	}

	var manifest struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", fmt.Errorf("failed to parse manifest file %s: %v", manifestFile, err)
	}

	if manifest.Config.Digest == "" {
		return "", fmt.Errorf("no config digest found in manifest file %s", manifestFile)
	}
	if !strings.HasPrefix(manifest.Config.Digest, "sha256:") {
		return "", fmt.Errorf("invalid config digest format in %s: %s (expected sha256:...)", manifestFile, manifest.Config.Digest)
	}

	return manifest.Config.Digest, nil
}
