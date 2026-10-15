package images

import (
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestParseComposePS(t *testing.T) {
	array := `[{"ID":"a1","Name":"p-web-1","Service":"web","State":"running","Health":"","ExitCode":0,"Image":"x:1"}]`
	ndjson := "{\"ID\":\"a1\",\"Service\":\"web\",\"State\":\"running\"}\n{\"ID\":\"b2\",\"Service\":\"db\",\"State\":\"exited\",\"ExitCode\":3}\n"

	got, err := ParseComposePS([]byte(array))
	if err != nil || len(got) != 1 || got[0].ID != "a1" || got[0].Service != "web" {
		t.Fatalf("array form: got %+v, err %v", got, err)
	}
	got, err = ParseComposePS([]byte(ndjson))
	if err != nil || len(got) != 2 || got[1].ExitCode != 3 || got[1].State != "exited" {
		t.Fatalf("ndjson form: got %+v, err %v", got, err)
	}
	got, err = ParseComposePS([]byte("  \n"))
	if err != nil || got != nil {
		t.Fatalf("empty form: got %+v, err %v", got, err)
	}
	if _, err := ParseComposePS([]byte("not json")); err == nil {
		t.Fatalf("expected an error for garbage input")
	}
}

func TestParseComposeImages(t *testing.T) {
	out := `[{"ID":"sha256:abc","ContainerName":"p-web-1","Repository":"x","Tag":"1","Platform":"linux/amd64","Size":1}]`
	got, err := ParseComposeImages([]byte(out))
	if err != nil || len(got) != 1 || got[0].ID != "sha256:abc" || got[0].ContainerName != "p-web-1" {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

func TestVerify(t *testing.T) {
	img := &Image{
		Label:          "//x:img",
		ManifestDigest: "sha256:mmm",
		ConfigDigest:   "sha256:ccc",
		DiffIDs:        []string{"sha256:l1", "sha256:l2"},
		Platform:       ocispec.Platform{OS: "linux", Architecture: "amd64"},
	}
	ok := func() *ImageInfo {
		info := &ImageInfo{ID: "sha256:mmm", OS: "linux", Architecture: "amd64"}
		info.RootFS.Layers = []string{"sha256:l1", "sha256:l2"}
		return info
	}

	if err := Verify(img, ok()); err != nil {
		t.Errorf("containerd-store identity should verify: %v", err)
	}
	classic := ok()
	classic.ID = "sha256:ccc"
	if err := Verify(img, classic); err != nil {
		t.Errorf("classic-store identity should verify: %v", err)
	}

	wrongID := ok()
	wrongID.ID = "sha256:other"
	if err := Verify(img, wrongID); err == nil || !strings.Contains(err.Error(), "neither the manifest digest") {
		t.Errorf("unexpected ID should fail, got %v", err)
	}
	wrongLayers := ok()
	wrongLayers.RootFS.Layers = []string{"sha256:l1"}
	if err := Verify(img, wrongLayers); err == nil || !strings.Contains(err.Error(), "layers differ") {
		t.Errorf("layer mismatch should fail, got %v", err)
	}
	wrongPlatform := ok()
	wrongPlatform.Architecture = "arm64"
	if err := Verify(img, wrongPlatform); err == nil || !strings.Contains(err.Error(), "platform differs") {
		t.Errorf("platform mismatch should fail, got %v", err)
	}
}
