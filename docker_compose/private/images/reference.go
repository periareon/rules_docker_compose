package images

import (
	"strings"
)

// UniqueTagPrefix marks a tag as content-derived. The full manifest digest
// follows it, so two different images can never share a unique tag.
const UniqueTagPrefix = "rdc-"

// Repository strips the tag from an image reference, leaving `[registry/]repo`.
// A port in the registry component is not mistaken for a tag separator.
func Repository(ref string) string {
	if at := strings.Index(ref, "@"); at != -1 {
		ref = ref[:at]
	}
	slash := strings.LastIndex(ref, "/")
	last := ref[slash+1:]
	if colon := strings.LastIndex(last, ":"); colon != -1 {
		return ref[:slash+1] + last[:colon]
	}
	return ref
}

// UniqueTag derives the content-addressed name for an original tag.
func UniqueTag(originalTag, manifestDigest string) string {
	hex := strings.TrimPrefix(manifestDigest, "sha256:")
	return Repository(originalTag) + ":" + UniqueTagPrefix + hex
}

// QualifiedReference expands a reference to the fully qualified form Docker
// uses internally, e.g. `nginx:x` becomes `docker.io/library/nginx:x`. A
// reference stored in an OCI index annotation has to be in this form; a bare
// one loads, but the daemon then cannot resolve it by name.
func QualifiedReference(ref string) string {
	repo := Repository(ref)
	suffix := ref[len(repo):]
	first, rest, hasSlash := strings.Cut(repo, "/")
	isRegistry := hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost")
	switch {
	case isRegistry:
		return ref
	case !hasSlash:
		return "docker.io/library/" + repo + suffix
	default:
		return "docker.io/" + first + "/" + rest + suffix
	}
}

// NormalizedForms returns the spellings compose might use for a reference: as
// written, without a `docker.io/` prefix, and without `docker.io/library/`.
func NormalizedForms(ref string) []string {
	forms := []string{ref}
	if s := strings.TrimPrefix(ref, "docker.io/library/"); s != ref {
		forms = append(forms, s)
	}
	if s := strings.TrimPrefix(ref, "docker.io/"); s != ref {
		forms = append(forms, s)
	}
	return forms
}
