package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestHasVariableRef(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  bool
	}{
		{"nginx:latest", false},
		{"nginx:${TAG}", true},
		{"nginx:$TAG", true},
		{"keep $$this literal", false},
		{"$$$TAG", true}, // escaped "$" followed by a real reference
		{"$$$$", false},  // two escaped "$"
		{"/home/${USER}/x", true},
	} {
		if got := hasVariableRef(tc.input); got != tc.want {
			t.Errorf("hasVariableRef(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestComposeVarRefs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  []string
	}{
		{"none", "nginx:latest", nil},
		{"braced", "nginx:${TAG}", []string{"TAG"}},
		{"bare", "nginx:$TAG", []string{"TAG"}},
		{"multiple", "${REGISTRY}/app:${TAG}", []string{"REGISTRY", "TAG"}},

		// Forms carrying a default or alternate resolve without the
		// environment, so they are not reported.
		{"colon dash default", "nginx:${TAG:-latest}", nil},
		{"dash default", "nginx:${TAG-latest}", nil},
		{"empty default", "nginx:${TAG-}", nil},
		{"colon plus alternate", "nginx:${TAG:+alt}", nil},
		{"plus alternate", "nginx:${TAG+alt}", nil},

		// docker-compose reports these itself with a better message.
		{"required", "nginx:${TAG:?must be set}", nil},
		{"required no colon", "nginx:${TAG?must be set}", nil},

		{"escaped dollar", "keep $$this literal", nil},
		{"escaped then real", "$$literal-${REAL}", []string{"REAL"}},
		{"unterminated brace", "nginx:${TAG", nil},
		{"empty braces", "nginx:${}", nil},
		{"trailing dollar", "nginx:latest$", nil},
		{"underscores and digits", "${MY_VAR2}", []string{"MY_VAR2"}},
		{"bare stops at punctuation", "$TAG/suffix", []string{"TAG"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := composeVarRefs(tc.input)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("composeVarRefs(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

const deferredYAML = `services:
  resolvable:
    image: nginx:${TAG}
    environment:
      KEEP: /home/${USER}
  concrete:
    image: postgres:16
  undefined:
    image: myapp:${NOPE}
`

func TestResolveServiceImagesSubstitutesAndDefers(t *testing.T) {
	out, unresolved, err := resolveServiceImages(
		[]byte(deferredYAML),
		map[string]string{"resolvable": "nginx:1.2.3", "concrete": "postgres:16", "undefined": "myapp:"},
		map[string]struct{}{"TAG": {}},
	)
	if err != nil {
		t.Fatalf("resolveServiceImages: %v", err)
	}

	// An unset variable interpolates to the empty string rather than
	// failing, so "myapp:${NOPE}" must be reported, not silently accepted
	// as the image reference "myapp:".
	if len(unresolved) != 1 || !strings.Contains(unresolved[0], "NOPE") {
		t.Errorf("unresolved = %v, want one entry naming NOPE", unresolved)
	}

	got := string(out)
	if !strings.Contains(got, "image: nginx:1.2.3") {
		t.Errorf("declared image variable was not resolved:\n%s", got)
	}
	if !strings.Contains(got, "image: postgres:16") {
		t.Errorf("concrete image was altered:\n%s", got)
	}
	// Everything outside services.*.image stays deferred to runtime.
	if !strings.Contains(got, "${USER}") {
		t.Errorf("non-image variable was resolved:\n%s", got)
	}
}

func TestResolveServiceImagesLeavesConcreteYAMLUntouched(t *testing.T) {
	const concrete = "services:\n  app:\n    image: nginx:latest\n"
	out, unresolved, err := resolveServiceImages(
		[]byte(concrete), map[string]string{"app": "nginx:latest"}, map[string]struct{}{},
	)
	if err != nil {
		t.Fatalf("resolveServiceImages: %v", err)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %v, want none", unresolved)
	}
	// No image needed substituting, so the yaml is passed through verbatim
	// rather than round-tripped through the encoder.
	if string(out) != concrete {
		t.Errorf("yaml was rewritten unnecessarily:\ngot  %q\nwant %q", string(out), concrete)
	}
}
