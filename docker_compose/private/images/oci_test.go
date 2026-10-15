package images

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// fakeLayout writes a minimal OCI layout and returns its directory plus the
// digests of the manifest and config it contains.
type fakeImage struct {
	dir            string
	manifestDigest digest.Digest
	configDigest   digest.Digest
	diffIDs        []string
	layerDigest    digest.Digest
}

func writeBlob(t *testing.T, dir string, data []byte) digest.Digest {
	t.Helper()
	d := digest.FromBytes(data)
	p := filepath.Join(dir, blobPath(d))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

// fakeLayout writes a minimal OCI layout with one manifest per platform and
// returns the first image's identities plus every manifest descriptor.
func fakeLayout(t *testing.T, platforms ...ocispec.Platform) (fakeImage, []ocispec.Descriptor) {
	t.Helper()
	dir := t.TempDir()
	layer := []byte("not really a layer")
	layerDigest := writeBlob(t, dir, layer)
	diffID := digest.Digest("sha256:" + strings.Repeat("ab", 32))

	var descs []ocispec.Descriptor
	var first fakeImage
	for i, p := range platforms {
		config, _ := json.Marshal(map[string]any{
			"architecture": p.Architecture,
			"os":           p.OS,
			"rootfs":       map[string]any{"type": "layers", "diff_ids": []digest.Digest{diffID}},
			"author":       fmt.Sprintf("platform-%d", i), // keeps config digests distinct per platform
		})
		configDigest := writeBlob(t, dir, config)
		manifest, _ := json.Marshal(ocispec.Manifest{
			Versioned: specs.Versioned{SchemaVersion: 2},
			MediaType: ocispec.MediaTypeImageManifest,
			Config:    ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: configDigest, Size: int64(len(config))},
			Layers:    []ocispec.Descriptor{{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: layerDigest, Size: int64(len(layer))}},
		})
		manifestDigest := writeBlob(t, dir, manifest)
		pl := p
		descs = append(descs, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: manifestDigest, Size: int64(len(manifest)), Platform: &pl})
		if i == 0 {
			first = fakeImage{dir: dir, manifestDigest: manifestDigest, configDigest: configDigest, diffIDs: []string{diffID.String()}, layerDigest: layerDigest}
		}
	}
	idx, _ := json.Marshal(ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex, Manifests: descs})
	if err := os.WriteFile(filepath.Join(dir, ocispec.ImageIndexFile), idx, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ocispec.ImageLayoutFile), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return first, descs
}

func TestDescribeSinglePlatform(t *testing.T) {
	img, _ := fakeLayout(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	// A single-manifest index is taken even when the wanted platform differs;
	// the mismatch is reported at test time against the real daemon.
	id, err := Describe(LayoutSource{Dir: img.dir}, ocispec.Platform{OS: "linux", Architecture: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if id.ManifestDescriptor.Digest != img.manifestDigest || id.ConfigDigest != img.configDigest {
		t.Errorf("unexpected identity %+v", id)
	}
	if !reflect.DeepEqual(id.DiffIDs, img.diffIDs) {
		t.Errorf("diff ids = %v, want %v", id.DiffIDs, img.diffIDs)
	}
	if id.Platform.Architecture != "amd64" {
		t.Errorf("platform = %s", id.Platform)
	}
}

func TestDescribeSelectsPlatform(t *testing.T) {
	img, descs := fakeLayout(t,
		ocispec.Platform{OS: "linux", Architecture: "amd64"},
		ocispec.Platform{OS: "linux", Architecture: "arm64"},
	)
	id, err := Describe(LayoutSource{Dir: img.dir}, ocispec.Platform{OS: "linux", Architecture: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if id.ManifestDescriptor.Digest != descs[1].Digest {
		t.Errorf("selected %s, want arm64 manifest %s", id.ManifestDescriptor.Digest, descs[1].Digest)
	}
	if _, err := Describe(LayoutSource{Dir: img.dir}, ocispec.Platform{OS: "linux", Architecture: "s390x"}); err == nil || !strings.Contains(err.Error(), "no manifest for linux/s390x") {
		t.Errorf("expected a platform error, got %v", err)
	}
}

func TestAttestationManifestsAreSkipped(t *testing.T) {
	img, descs := fakeLayout(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	descs = append(descs, ocispec.Descriptor{
		MediaType:   ocispec.MediaTypeImageManifest,
		Digest:      digest.Digest("sha256:" + strings.Repeat("ee", 32)),
		Platform:    &ocispec.Platform{OS: "unknown", Architecture: "unknown"},
		Annotations: map[string]string{"vnd.docker.reference.type": attestationType},
	})
	idx, _ := json.Marshal(ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: descs})
	if err := os.WriteFile(filepath.Join(img.dir, ocispec.ImageIndexFile), idx, 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := Describe(LayoutSource{Dir: img.dir}, ocispec.Platform{OS: "linux", Architecture: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if id.ManifestDescriptor.Digest != img.manifestDigest {
		t.Errorf("selected %s, want the runnable manifest %s", id.ManifestDescriptor.Digest, img.manifestDigest)
	}
}

func TestWriteDualTarRoundTrip(t *testing.T) {
	img, _ := fakeLayout(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	src := LayoutSource{Dir: img.dir}
	id, err := Describe(src, HostPlatform())
	if err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(t.TempDir(), "image.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	ref := "docker.io/example/app:rdc-" + img.manifestDigest.Encoded()
	if err := WriteDualTar(f, src, id, ref); err != nil {
		t.Fatal(err)
	}
	f.Close()

	idx, entries, err := ReadDualTar(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Manifests) != 1 || idx.Manifests[0].Digest != img.manifestDigest {
		t.Errorf("index = %+v", idx)
	}
	if got := idx.Manifests[0].Annotations[ocispec.AnnotationRefName]; got != ref {
		t.Errorf("ref.name = %q, want %q", got, ref)
	}
	if len(entries) != 1 || entries[0].Config != blobPath(img.configDigest) || entries[0].RepoTags[0] != ref {
		t.Errorf("manifest.json = %+v", entries)
	}
	if !reflect.DeepEqual(entries[0].Layers, []string{blobPath(img.layerDigest)}) {
		t.Errorf("layers = %v", entries[0].Layers)
	}

	// The archive is itself a valid source, and re-describing it yields the
	// same identity: the blobs were copied byte for byte.
	again, err := Describe(TarSource{Path: tarPath}, HostPlatform())
	if err != nil {
		t.Fatal(err)
	}
	if again.ManifestDescriptor.Digest != id.ManifestDescriptor.Digest || again.ConfigDigest != id.ConfigDigest {
		t.Errorf("identity changed through the tar: %+v vs %+v", again, id)
	}
}

func TestDescribeRejectsCorruptBlob(t *testing.T) {
	img, _ := fakeLayout(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	p := filepath.Join(img.dir, blobPath(img.configDigest))
	if err := os.WriteFile(p, []byte(`{"os":"linux","architecture":"amd64","rootfs":{"diff_ids":[]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Describe(LayoutSource{Dir: img.dir}, HostPlatform()); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("expected a corruption error, got %v", err)
	}
}
