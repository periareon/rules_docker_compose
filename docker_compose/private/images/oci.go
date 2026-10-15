package images

import (
	"archive/tar"
	"bytes"

	// go-digest resolves hash implementations through the crypto registry, so
	// the algorithms an image may use have to be linked into the binary.
	_ "crypto/sha256"
	_ "crypto/sha512"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	// Docker's pre-OCI media types, still emitted by some tooling. The OCI
	// ones come from the image-spec module.
	mediaTypeDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"

	// attestationType marks BuildKit provenance manifests, which are not
	// runnable images and must never be selected.
	attestationType = "attestation-manifest"
)

// Identity is everything about an image that can be checked after a load.
type Identity struct {
	// ManifestDescriptor is the selected manifest's descriptor from the index,
	// with the platform filled in from the config when the index lacked it.
	ManifestDescriptor ocispec.Descriptor
	Manifest           ocispec.Manifest
	ConfigDigest       digest.Digest
	DiffIDs            []string
	Platform           ocispec.Platform
}

// Source provides an OCI index and the blobs it references.
type Source interface {
	Index() (*ocispec.Index, error)
	// Open returns a reader for a blob and its size.
	Open(d digest.Digest) (io.ReadCloser, int64, error)
}

// LayoutSource reads an OCI image layout directory.
type LayoutSource struct{ Dir string }

// Index implements Source.
func (s LayoutSource) Index() (*ocispec.Index, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir, ocispec.ImageIndexFile))
	if err != nil {
		return nil, fmt.Errorf("failed to read OCI index in %s: %w", s.Dir, err)
	}
	return parseIndex(data, s.Dir)
}

// Open implements Source.
func (s LayoutSource) Open(d digest.Digest) (io.ReadCloser, int64, error) {
	f, err := os.Open(filepath.Join(s.Dir, blobPath(d)))
	if err != nil {
		return nil, 0, fmt.Errorf("blob %s missing from OCI layout %s: %w", d, s.Dir, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

// TarSource reads an OCI layout packed as a tarball, such as the `tarball`
// output of a rules_img `image_load`.
type TarSource struct{ Path string }

// Index implements Source.
func (s TarSource) Index() (*ocispec.Index, error) {
	data, err := s.entry(ocispec.ImageIndexFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s from %s: %w", ocispec.ImageIndexFile, s.Path, err)
	}
	return parseIndex(data, s.Path)
}

// Open implements Source. Each call scans the archive from the start; tar
// readers skip entries by seeking, so that is cheap even for large layers.
func (s TarSource) Open(d digest.Digest) (io.ReadCloser, int64, error) {
	f, err := os.Open(s.Path)
	if err != nil {
		return nil, 0, err
	}
	tr := tar.NewReader(f)
	want := blobPath(d)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			f.Close()
			return nil, 0, fmt.Errorf("blob %s missing from %s", d, s.Path)
		}
		if err != nil {
			f.Close()
			return nil, 0, err
		}
		if path.Clean(hdr.Name) == want {
			return &tarEntry{Reader: tr, closer: f}, hdr.Size, nil
		}
	}
}

func (s TarSource) entry(name string) ([]byte, error) {
	f, err := os.Open(s.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s not found in archive", name)
		}
		if err != nil {
			return nil, err
		}
		if path.Clean(hdr.Name) == name {
			return io.ReadAll(tr)
		}
	}
}

type tarEntry struct {
	io.Reader
	closer io.Closer
}

func (t *tarEntry) Close() error { return t.closer.Close() }

func parseIndex(data []byte, where string) (*ocispec.Index, error) {
	var idx ocispec.Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("failed to parse OCI index in %s: %w", where, err)
	}
	return &idx, nil
}

func blobPath(d digest.Digest) string {
	return path.Join(ocispec.ImageBlobsDir, d.Algorithm().String(), d.Encoded())
}

// readBlob returns a blob's bytes after checking them against the digest.
func readBlob(src Source, d digest.Digest) ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("invalid digest %q: %w", d, err)
	}
	r, _, err := src.Open(d)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if got := d.Algorithm().FromBytes(data); got != d {
		return nil, fmt.Errorf("blob %s is corrupt: content hashes to %s", d, got)
	}
	return data, nil
}

// HostPlatform is the container platform matching the machine running the
// build. Containers are always Linux, so macOS and Windows map to linux.
func HostPlatform() ocispec.Platform {
	return ocispec.Platform{OS: "linux", Architecture: runtime.GOARCH}
}

// PlatformString renders a platform as `os/arch[/variant]`, as Docker does.
func PlatformString(p ocispec.Platform) string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

func platformMatches(want ocispec.Platform, have *ocispec.Platform) bool {
	if have == nil || have.OS != want.OS || have.Architecture != want.Architecture {
		return false
	}
	return want.Variant == "" || have.Variant == "" || want.Variant == have.Variant
}

func isRunnable(d ocispec.Descriptor) bool {
	if d.Annotations["vnd.docker.reference.type"] == attestationType {
		return false
	}
	if d.Platform != nil && d.Platform.OS == "unknown" {
		return false
	}
	switch d.MediaType {
	case ocispec.MediaTypeImageIndex, ocispec.MediaTypeImageManifest, mediaTypeDockerList, mediaTypeDockerManifest, "":
		return true
	}
	return false
}

func isIndex(mediaType string) bool {
	return mediaType == ocispec.MediaTypeImageIndex || mediaType == mediaTypeDockerList
}

// selectManifest picks the single image manifest to load from an index,
// descending through nested indexes. An index with exactly one runnable entry
// is taken as-is; its platform is checked against the daemon at test time.
func selectManifest(src Source, idx *ocispec.Index, want ocispec.Platform) (ocispec.Descriptor, error) {
	var candidates []ocispec.Descriptor
	for _, d := range idx.Manifests {
		if isRunnable(d) {
			candidates = append(candidates, d)
		}
	}
	var chosen ocispec.Descriptor
	switch {
	case len(candidates) == 0:
		return ocispec.Descriptor{}, fmt.Errorf("image index contains no runnable manifests")
	case len(candidates) == 1:
		chosen = candidates[0]
	default:
		var available []string
		for _, d := range candidates {
			if platformMatches(want, d.Platform) {
				chosen = d
				break
			}
			if d.Platform != nil {
				available = append(available, PlatformString(*d.Platform))
			}
		}
		if chosen.Digest == "" {
			return ocispec.Descriptor{}, fmt.Errorf(
				"image index has no manifest for %s (available: %s)", PlatformString(want), strings.Join(available, ", "))
		}
	}
	if isIndex(chosen.MediaType) {
		data, err := readBlob(src, chosen.Digest)
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		nested, err := parseIndex(data, chosen.Digest.String())
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		return selectManifest(src, nested, want)
	}
	return chosen, nil
}

// Describe resolves the identity of the image in src for the wanted platform.
func Describe(src Source, want ocispec.Platform) (*Identity, error) {
	idx, err := src.Index()
	if err != nil {
		return nil, err
	}
	desc, err := selectManifest(src, idx, want)
	if err != nil {
		return nil, err
	}
	manifestBytes, err := readBlob(src, desc.Digest)
	if err != nil {
		return nil, err
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("failed to parse image manifest %s: %w", desc.Digest, err)
	}
	if manifest.Config.Digest == "" {
		return nil, fmt.Errorf("image manifest %s has no config", desc.Digest)
	}
	configBytes, err := readBlob(src, manifest.Config.Digest)
	if err != nil {
		return nil, err
	}
	var config ocispec.Image
	if err := json.Unmarshal(configBytes, &config); err != nil {
		return nil, fmt.Errorf("failed to parse image config %s: %w", manifest.Config.Digest, err)
	}
	if len(config.RootFS.DiffIDs) != len(manifest.Layers) {
		return nil, fmt.Errorf(
			"image manifest %s lists %d layers but its config has %d diff_ids",
			desc.Digest, len(manifest.Layers), len(config.RootFS.DiffIDs))
	}
	diffIDs := make([]string, 0, len(config.RootFS.DiffIDs))
	for _, d := range config.RootFS.DiffIDs {
		diffIDs = append(diffIDs, d.String())
	}
	platform := config.Platform
	if desc.Platform == nil {
		desc.Platform = &platform
	}
	desc.Size = int64(len(manifestBytes))
	if desc.MediaType == "" {
		desc.MediaType = manifest.MediaType
	}
	return &Identity{
		ManifestDescriptor: desc,
		Manifest:           manifest,
		ConfigDigest:       manifest.Config.Digest,
		DiffIDs:            diffIDs,
		Platform:           platform,
	}, nil
}

// dockerManifestEntry is one element of a docker-save `manifest.json`.
type dockerManifestEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// WriteDualTar writes an archive that both of Docker's image stores accept and
// that yields a predictable image ID in each.
//
// The archive carries an OCI layout (oci-layout, index.json, blobs) whose
// index names the image `ref`, and a docker-save manifest.json pointing at the
// same blobs. The containerd store imports the OCI side verbatim, so the image
// ID is the manifest digest; the classic store reads manifest.json, so the
// image ID is the config digest. Blobs are copied byte for byte, which is what
// keeps those digests stable.
func WriteDualTar(w io.Writer, src Source, id *Identity, ref string) error {
	tw := tar.NewWriter(w)

	desc := id.ManifestDescriptor
	annotations := map[string]string{ocispec.AnnotationRefName: ref}
	for k, v := range desc.Annotations {
		if k != ocispec.AnnotationRefName {
			annotations[k] = v
		}
	}
	desc.Annotations = annotations
	indexJSON, err := json.Marshal(ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{desc},
	})
	if err != nil {
		return err
	}

	layers := make([]string, 0, len(id.Manifest.Layers))
	for _, l := range id.Manifest.Layers {
		layers = append(layers, blobPath(l.Digest))
	}
	dockerManifest, err := json.Marshal([]dockerManifestEntry{{
		Config:   blobPath(id.ConfigDigest),
		RepoTags: []string{ref},
		Layers:   layers,
	}})
	if err != nil {
		return err
	}
	layoutJSON, err := json.Marshal(ocispec.ImageLayout{Version: ocispec.ImageLayoutVersion})
	if err != nil {
		return err
	}

	for name, data := range map[string][]byte{
		ocispec.ImageLayoutFile: layoutJSON,
		ocispec.ImageIndexFile:  indexJSON,
		"manifest.json":         dockerManifest,
	} {
		if err := writeTarFile(tw, name, data); err != nil {
			return err
		}
	}
	for _, dir := range []string{ocispec.ImageBlobsDir + "/", ocispec.ImageBlobsDir + "/" + digest.Canonical.String() + "/"} {
		if err := tw.WriteHeader(&tar.Header{Name: dir, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			return err
		}
	}
	blobs := []digest.Digest{desc.Digest, id.ConfigDigest}
	for _, l := range id.Manifest.Layers {
		blobs = append(blobs, l.Digest)
	}
	seen := map[digest.Digest]bool{}
	for _, d := range blobs {
		if seen[d] {
			continue
		}
		seen[d] = true
		if err := copyBlob(tw, src, d); err != nil {
			return err
		}
	}
	return tw.Close()
}

func writeTarFile(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// copyBlob streams a blob into the archive, verifying its digest on the way.
func copyBlob(tw *tar.Writer, src Source, d digest.Digest) error {
	if err := d.Validate(); err != nil {
		return fmt.Errorf("invalid digest %q: %w", d, err)
	}
	r, size, err := src.Open(d)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := tw.WriteHeader(&tar.Header{Name: blobPath(d), Mode: 0o644, Size: size, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	verifier := d.Verifier()
	n, err := io.Copy(io.MultiWriter(tw, verifier), r)
	if err != nil {
		return fmt.Errorf("failed to copy blob %s: %w", d, err)
	}
	if n != size {
		return fmt.Errorf("blob %s: expected %d bytes, copied %d", d, size, n)
	}
	if !verifier.Verified() {
		return fmt.Errorf("blob %s is corrupt: content does not match its digest", d)
	}
	return nil
}

// ReadDualTar parses the index.json and manifest.json of an archive written by
// WriteDualTar. It exists for tests and diagnostics.
func ReadDualTar(path string) (*ocispec.Index, []dockerManifestEntry, error) {
	src := TarSource{Path: path}
	idx, err := src.Index()
	if err != nil {
		return nil, nil, err
	}
	data, err := src.entry("manifest.json")
	if err != nil {
		return nil, nil, err
	}
	var entries []dockerManifestEntry
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&entries); err != nil {
		return nil, nil, err
	}
	return idx, entries, nil
}
