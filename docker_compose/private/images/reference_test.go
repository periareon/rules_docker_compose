package images

import (
	"reflect"
	"testing"
)

func TestRepository(t *testing.T) {
	for in, want := range map[string]string{
		"nginx":                          "nginx",
		"nginx:latest":                   "nginx",
		"docker.io/library/nginx:1.25":   "docker.io/library/nginx",
		"localhost:5000/app:v1":          "localhost:5000/app",
		"localhost:5000/app":             "localhost:5000/app",
		"registry.example.com/a/b:tag":   "registry.example.com/a/b",
		"nginx@sha256:abc":               "nginx",
		"registry:5000/x:1@sha256:abc":   "registry:5000/x",
		"rules_docker_compose/oci/nginx": "rules_docker_compose/oci/nginx",
	} {
		if got := Repository(in); got != want {
			t.Errorf("Repository(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUniqueTag(t *testing.T) {
	digest := "sha256:3043d61c6f7aff9351c6a7283c2300dc40a5364aa56325675d279502bd2d2085"
	got := UniqueTag("localhost:5000/app:v1", digest)
	want := "localhost:5000/app:rdc-3043d61c6f7aff9351c6a7283c2300dc40a5364aa56325675d279502bd2d2085"
	if got != want {
		t.Errorf("UniqueTag = %q, want %q", got, want)
	}
	if len(got[len("localhost:5000/app:"):]) > 128 {
		t.Errorf("tag component exceeds docker's 128 character limit")
	}
}

func TestQualifiedReference(t *testing.T) {
	for in, want := range map[string]string{
		"nginx:x":                          "docker.io/library/nginx:x",
		"nginx":                            "docker.io/library/nginx",
		"user/app:x":                       "docker.io/user/app:x",
		"rules_docker_compose/oci/n:rdc-1": "docker.io/rules_docker_compose/oci/n:rdc-1",
		"docker.io/user/app:x":             "docker.io/user/app:x",
		"localhost/app:x":                  "localhost/app:x",
		"localhost:5000/app:x":             "localhost:5000/app:x",
		"ghcr.io/org/app:x":                "ghcr.io/org/app:x",
	} {
		if got := QualifiedReference(in); got != want {
			t.Errorf("QualifiedReference(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizedForms(t *testing.T) {
	got := NormalizedForms("docker.io/library/nginx:1")
	want := []string{"docker.io/library/nginx:1", "nginx:1", "library/nginx:1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NormalizedForms = %v, want %v", got, want)
	}
	if got := NormalizedForms("ghcr.io/a/b:1"); !reflect.DeepEqual(got, []string{"ghcr.io/a/b:1"}) {
		t.Errorf("NormalizedForms(ghcr) = %v", got)
	}
}
