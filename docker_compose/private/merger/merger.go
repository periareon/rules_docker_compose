package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/periareon/rules_docker_compose/docker_compose/private/images"
	"gopkg.in/yaml.v3"
)

// debugLog logs a message to stderr if RULES_DOCKER_COMPOSE_DEBUG is set
func debugLog(format string, args ...interface{}) {
	images.Debugf(format, args...)
}

// normalizeLineEndings converts CRLF to LF for consistent output across platforms
func normalizeLineEndings(data []byte) []byte {
	return []byte(strings.ReplaceAll(string(data), "\r\n", "\n"))
}

// relativizePaths rewrites absolute execroot paths in the merged YAML to paths
// relative to the output file's runfiles location. It uses a manifest that maps
// each data file's execpath to its rlocationpath, ensuring correctness across the
// execroot-to-runfiles boundary (where bazel-out/<config>/bin/ is stripped).
//
// Path separators are normalized to forward slashes so the regex and manifest
// lookups work consistently on both Unix and Windows, where docker-compose may
// emit backslash-separated absolute paths.
func relativizePaths(yamlContent []byte, execrootPrefix string,
	dataManifest map[string]string, outputRlocationDir string) []byte {

	if execrootPrefix == "" || len(dataManifest) == 0 {
		return yamlContent
	}
	execrootPrefix = filepath.ToSlash(filepath.Clean(execrootPrefix))
	if !strings.HasSuffix(execrootPrefix, "/") {
		execrootPrefix += "/"
	}
	content := string(yamlContent)
	escapedPrefix := regexp.QuoteMeta(execrootPrefix)
	escapedPrefix = strings.ReplaceAll(escapedPrefix, "/", `[/\\]`)
	re := regexp.MustCompile(escapedPrefix + `([^:\s"'\n]*)`)
	return []byte(re.ReplaceAllStringFunc(content, func(match string) string {
		execpath := strings.TrimPrefix(filepath.ToSlash(match), execrootPrefix)

		rlocationpath, ok := dataManifest[execpath]
		if !ok {
			debugLog("Path not in data manifest, leaving unchanged: %s", execpath)
			return match
		}

		relPath, err := filepath.Rel(filepath.FromSlash(outputRlocationDir), filepath.FromSlash(rlocationpath))
		if err != nil {
			debugLog("Failed to relativize path %s: %v", execpath, err)
			return match
		}
		relPath = filepath.ToSlash(relPath)
		debugLog("Relativized path: %s -> %s (rlocation: %s)", execpath, relPath, rlocationpath)
		return relPath
	}))
}

func loadDataManifest(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read data manifest %s: %v", path, err)
	}
	var manifest map[string]string
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("failed to parse data manifest %s: %v", path, err)
	}
	return manifest, nil
}

type Args struct {
	DockerCompose       string
	Output              string
	OutputRewritten     string
	OutputRuntime       string
	ProjectName         string
	DataManifest        string
	OutputRlocationpath string
	NoInterpolate       bool
	AllowRegistryImages bool
	RequireLoaders      bool
	KeepPublishedPorts  bool
	Files               []string
	ImageManifests      []string
	Env                 []string
}

// ImageManifest is the per-image description emitted by the bzl rule. It
// points at the image content (an OCI layout directory from rules_oci, or an
// OCI layout tarball from rules_img), the files listing the tags the user
// declared, and where to write the loadable tarball.
type ImageManifest struct {
	Label        string   `json:"label"`
	TagFilePaths []string `json:"tag_file_paths"`
	OCILayoutDir string   `json:"oci_layout_dir"`
	Tarball      string   `json:"tarball"`
	TarOut       string   `json:"tar_out"`
	TarRlocation string   `json:"tar_rlocationpath"`
}

func parseArgs() (*Args, error) {
	args := &Args{}
	flag.StringVar(&args.DockerCompose, "docker-compose", "", "Path to docker-compose binary")
	flag.StringVar(&args.Output, "output", "", "Path to output merged yaml file (original tags preserved; suitable for sharing outside Bazel)")
	flag.StringVar(&args.OutputRewritten, "output-rewritten", "", "Optional path to write the merged yaml with services.*.image rewritten to content-derived unique tags (consumed by the test runner)")
	flag.StringVar(&args.OutputRuntime, "output-runtime", "", "Path to output runtime manifest JSON consumed by runner/launcher")
	flag.StringVar(&args.ProjectName, "project-name", "", "Project name for docker-compose")
	flag.StringVar(&args.DataManifest, "data-manifest", "", "Path to JSON manifest mapping data file execpaths to rlocationpaths")
	flag.StringVar(&args.OutputRlocationpath, "output-rlocationpath", "", "Rlocationpath of the output YAML file (for computing relative paths)")
	flag.BoolVar(&args.NoInterpolate, "no-interpolate", false, "Leave ${VAR} references in the merged yaml unresolved so docker-compose interpolates them at runtime")
	flag.BoolVar(&args.AllowRegistryImages, "allow-registry-images", false, "Permit digest-pinned image references that have no loader and are pulled from a registry at runtime")
	flag.BoolVar(&args.RequireLoaders, "require-loaders", false, "Fail at build time for any image reference without a loader, even when no loaders are declared (set for merges owned by a test)")
	flag.BoolVar(&args.KeepPublishedPorts, "keep-published-ports", false, "Keep the declared host ports in the test yaml instead of letting the daemon pick free ones")
	var files flagArray
	flag.Var(&files, "file", "Docker compose yaml file to merge (can be specified multiple times)")
	var imageManifests flagArray
	flag.Var(&imageManifests, "image_manifest", "Image manifest file (can be specified multiple times)")
	var env flagArray
	flag.Var(&env, "env", "KEY=VALUE pair added to the docker-compose environment for interpolation (can be specified multiple times)")
	flag.Parse()

	if args.DockerCompose == "" {
		return nil, fmt.Errorf("missing required flag: -docker-compose")
	}
	if args.Output == "" {
		return nil, fmt.Errorf("missing required flag: -output")
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("at least one -file flag is required")
	}

	for _, pair := range env {
		if !strings.Contains(pair, "=") {
			return nil, fmt.Errorf("invalid -env value %q: expected KEY=VALUE", pair)
		}
	}

	args.Files = files
	args.ImageManifests = imageManifests
	args.Env = env
	return args, nil
}

type flagArray []string

func (f *flagArray) String() string {
	return fmt.Sprintf("%v", []string(*f))
}

func (f *flagArray) Set(value string) error {
	*f = append(*f, value)
	return nil
}

// readTagsFromTagFiles reads all tag lines from the given files.
func readTagsFromTagFiles(tagFilePaths []string) ([]string, error) {
	var tags []string
	for _, tagFilePath := range tagFilePaths {
		data, err := os.ReadFile(tagFilePath)
		if err != nil {
			return nil, fmt.Errorf("failed to read tag file %s: %v", tagFilePath, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				tags = append(tags, line)
			}
		}
	}
	if len(tags) == 0 {
		return nil, fmt.Errorf("all tag files are empty or contain no valid tags")
	}
	return tags, nil
}

// prepareImage resolves the identity of one loader-backed image, writes its
// loadable tarball, and returns the runtime manifest entry describing it.
//
// The unique tag is derived from the manifest digest, which is what the
// containerd image store reports as the image ID after a load. The classic
// store reports the config digest instead; both are recorded so the runner can
// recognise either.
func prepareImage(manifest ImageManifest, tags []string) (*images.Image, error) {
	var src images.Source
	switch {
	case manifest.OCILayoutDir != "":
		src = images.LayoutSource{Dir: manifest.OCILayoutDir}
	case manifest.Tarball != "":
		src = images.TarSource{Path: manifest.Tarball}
	default:
		return nil, fmt.Errorf("image manifest %s has neither oci_layout_dir nor tarball", manifest.Label)
	}
	if manifest.TarOut == "" {
		return nil, fmt.Errorf("image manifest %s has no tar_out", manifest.Label)
	}

	identity, err := images.Describe(src, images.HostPlatform())
	if err != nil {
		return nil, fmt.Errorf("failed to describe image %s: %v", manifest.Label, err)
	}

	img := &images.Image{
		Label:          manifest.Label,
		TarRlocation:   manifest.TarRlocation,
		OriginalTags:   tags,
		ManifestDigest: identity.ManifestDescriptor.Digest.String(),
		ConfigDigest:   identity.ConfigDigest.String(),
		DiffIDs:        identity.DiffIDs,
		Platform:       identity.Platform,
	}
	for _, tag := range tags {
		unique := images.UniqueTag(tag, img.ManifestDigest)
		if !slices.Contains(img.UniqueTags, unique) {
			img.UniqueTags = append(img.UniqueTags, unique)
		}
	}

	if err := os.MkdirAll(filepath.Dir(manifest.TarOut), 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(manifest.TarOut)
	if err != nil {
		return nil, fmt.Errorf("failed to create image tarball %s: %v", manifest.TarOut, err)
	}
	defer f.Close()
	ref := images.QualifiedReference(img.UniqueTags[0])
	if err := images.WriteDualTar(f, src, identity, ref); err != nil {
		return nil, fmt.Errorf("failed to write image tarball for %s: %v", manifest.Label, err)
	}
	debugLog("Image %s: manifest=%s config=%s platform=%s unique=%v",
		manifest.Label, img.ManifestDigest, img.ConfigDigest, images.PlatformString(img.Platform), img.UniqueTags)
	return img, nil
}

// normalizedTagForms returns the spellings compose might use for a reference.
func normalizedTagForms(tag string) []string {
	return images.NormalizedForms(tag)
}

// forEachService walks `services.<name>` in a parsed compose document,
// invoking fn with the service name and its mapping node. Documents that don't
// have the expected shape simply yield no calls.
func forEachService(root *yaml.Node, fn func(service string, serviceNode *yaml.Node)) {
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(top.Content); i += 2 {
		key := top.Content[i]
		val := top.Content[i+1]
		if key.Value != "services" || val.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(val.Content); j += 2 {
			if service := val.Content[j+1]; service.Kind == yaml.MappingNode {
				fn(val.Content[j].Value, service)
			}
		}
	}
}

// mappingValue returns the value node for key in a mapping node, or nil.
func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// forEachServiceImage walks `services.<name>.image`, invoking fn with the
// service name, the service's mapping node and the mutable image scalar node.
func forEachServiceImage(root *yaml.Node, fn func(service string, serviceNode *yaml.Node, image *yaml.Node)) {
	forEachService(root, func(name string, service *yaml.Node) {
		if image := mappingValue(service, "image"); image != nil && image.Kind == yaml.ScalarNode {
			fn(name, service, image)
		}
	})
}

// collectPorts records every published container port in the document. When
// strip is set, the host side of each mapping is removed from the test yaml so
// the daemon assigns a free port at startup; the runner then discovers it and
// passes it to the test. Fixed host ports are what make two tests, or a test
// and anything else on the host, collide.
//
// `docker-compose config` always emits the long port syntax, so entries are
// mappings with `target`, `published` and `protocol` keys.
func collectPorts(root *yaml.Node, strip bool) []images.PortMapping {
	var ports []images.PortMapping
	forEachService(root, func(name string, service *yaml.Node) {
		list := mappingValue(service, "ports")
		if list == nil || list.Kind != yaml.SequenceNode {
			return
		}
		for _, entry := range list.Content {
			if entry.Kind != yaml.MappingNode {
				continue
			}
			target := mappingValue(entry, "target")
			if target == nil {
				continue
			}
			var targetPort int
			if _, err := fmt.Sscanf(target.Value, "%d", &targetPort); err != nil {
				continue
			}
			mapping := images.PortMapping{Service: name, Target: targetPort, Protocol: "tcp"}
			if protocol := mappingValue(entry, "protocol"); protocol != nil {
				mapping.Protocol = protocol.Value
			}
			if published := mappingValue(entry, "published"); published != nil {
				mapping.Published = published.Value
				if strip {
					for i := 0; i+1 < len(entry.Content); i += 2 {
						if entry.Content[i].Value == "published" {
							entry.Content = append(entry.Content[:i], entry.Content[i+2:]...)
							break
						}
					}
					debugLog("Releasing host port for services.%s port %d/%s (declared %s)", name, targetPort, mapping.Protocol, mapping.Published)
				}
			}
			ports = append(ports, mapping)
		}
	})
	return ports
}

// setMappingValue sets key to a scalar value in a mapping node, replacing an
// existing entry or appending a new one.
func setMappingValue(mapping *yaml.Node, key, value string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Value: value}
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: value},
	)
}

func encodeYAML(root *yaml.Node) ([]byte, error) {
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("failed to re-marshal YAML: %v", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("failed to close YAML encoder: %v", err)
	}
	return []byte(buf.String()), nil
}

// hasVariableRef reports whether s contains an un-escaped compose variable
// reference ($VAR or ${VAR}). "$$" is compose's escape for a literal "$" and
// does not count.
func hasVariableRef(s string) bool {
	return strings.Contains(strings.ReplaceAll(s, "$$", ""), "$")
}

// composeVarRef matches one compose interpolation token. The alternatives are
// ordered so "$$" (the escape for a literal "$") is consumed before it can be
// mistaken for a reference; the remaining two capture the braced and bare
// forms respectively.
var composeVarRef = regexp.MustCompile(`\$\$|\$\{([^}]*)\}|\$([A-Za-z0-9_]+)`)

// composeVarRefs returns the variable names in s that have to come from the
// environment. References carrying a default or alternate (`${VAR:-x}`,
// `${VAR-x}`, `${VAR:+x}`, `${VAR+x}`) are excluded because compose resolves
// them on its own, as are `${VAR:?err}` forms, which compose reports itself.
//
// This only identifies names; docker-compose still performs the substitution.
// It exists because an unset variable interpolates to the empty string rather
// than failing, which would otherwise turn "myapp:${TAG}" into the mystifying
// image reference "myapp:".
func composeVarRefs(s string) []string {
	var names []string
	for _, match := range composeVarRef.FindAllStringSubmatch(s, -1) {
		// Exactly one of the two groups is populated; "$$" leaves both empty.
		name := match[1] + match[2]
		if name == "" || strings.ContainsAny(name, ":-+?") {
			continue
		}
		names = append(names, name)
	}
	return names
}

// serviceImages maps each service name to its `image` value.
func serviceImages(yamlContent []byte) (map[string]string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(yamlContent, &root); err != nil {
		return nil, fmt.Errorf("failed to parse compose config: %v", err)
	}
	images := make(map[string]string)
	forEachServiceImage(&root, func(service string, _ *yaml.Node, image *yaml.Node) {
		images[service] = image.Value
	})
	return images, nil
}

// resolveServiceImages substitutes `services.<name>.image` values from an
// interpolated compose config into a yaml produced with --no-interpolate.
//
// Image references are the one field that must be concrete at build time,
// because each is matched against an image loader target and rewritten to a
// content-derived unique tag for test isolation. Everything else keeps its
// ${VAR} references so docker-compose resolves them at runtime.
//
// It returns the updated yaml plus a list of references that stayed
// unresolved, for the caller to report.
func resolveServiceImages(yamlContent []byte, resolved map[string]string, available map[string]struct{}) ([]byte, []string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(yamlContent, &root); err != nil {
		return nil, nil, fmt.Errorf("failed to parse merged YAML for image resolution: %v", err)
	}
	var unresolved []string
	changed := false
	forEachServiceImage(&root, func(service string, _ *yaml.Node, image *yaml.Node) {
		value, found := resolved[service]
		if found && value == image.Value {
			return // already concrete; nothing to substitute
		}
		var undefined []string
		for _, name := range composeVarRefs(image.Value) {
			if _, defined := available[name]; !defined {
				undefined = append(undefined, name)
			}
		}
		if !found || len(undefined) > 0 {
			detail := ""
			if len(undefined) > 0 {
				detail = fmt.Sprintf(" (undefined: %s)", strings.Join(undefined, ", "))
			}
			unresolved = append(unresolved, fmt.Sprintf("services.%s.image: %s%s", service, image.Value, detail))
			return
		}
		debugLog("Resolved service image: services.%s.image %s -> %s", service, image.Value, value)
		image.Value = value
		changed = true
	})
	if !changed {
		return yamlContent, unresolved, nil
	}
	out, err := encodeYAML(&root)
	if err != nil {
		return nil, nil, err
	}
	return out, unresolved, nil
}

// rewriteComposeYAML produces the yaml the test runner consumes. Every
// `services.*.image` matching an original loader tag is replaced with its
// unique tag and pinned to `pull_policy: never`, so compose can only ever use
// the image the runner loaded, and unless keepPorts is set the host side of
// every published port is released so the daemon assigns a free one.
//
// It returns the rewritten yaml, a map of rewritten service names to the
// unique tag each now references, and the published ports.
func rewriteComposeYAML(yamlContent []byte, mapping map[string]string, keepPorts bool, project string) ([]byte, map[string]string, []images.PortMapping, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(yamlContent, &root); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to parse merged YAML for rewriting: %v", err)
	}
	services := map[string]string{}
	forEachServiceImage(&root, func(service string, serviceNode *yaml.Node, image *yaml.Node) {
		for _, form := range normalizedTagForms(image.Value) {
			unique, found := mapping[form]
			if !found {
				continue
			}
			debugLog("Rewriting services.%s.image: %s -> %s", service, image.Value, unique)
			image.Value = unique
			setMappingValue(serviceNode, "pull_policy", "never")
			services[service] = unique
			return
		}
	})
	ports := collectPorts(&root, !keepPorts)
	releaseProjectNames(&root, project)
	out, err := encodeYAML(&root)
	return out, services, ports, err
}

// releaseProjectNames removes the names `docker-compose config` derived from
// the project name, so that the project name the runner passes at test time
// takes effect: the top-level `name`, and `networks.<n>.name` /
// `volumes.<n>.name` values equal to `<project>_<n>`. Names the user chose
// explicitly do not match that pattern and are kept.
func releaseProjectNames(root *yaml.Node, project string) {
	if project == "" || root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(top.Content); i += 2 {
		key, val := top.Content[i], top.Content[i+1]
		switch key.Value {
		case "name":
			if val.Value == project {
				top.Content = append(top.Content[:i], top.Content[i+2:]...)
				i -= 2
			}
		case "networks", "volumes":
			if val.Kind != yaml.MappingNode {
				continue
			}
			for j := 0; j+1 < len(val.Content); j += 2 {
				entry := val.Content[j+1]
				if entry.Kind != yaml.MappingNode {
					continue
				}
				derived := project + "_" + val.Content[j].Value
				for k := 0; k+1 < len(entry.Content); k += 2 {
					if entry.Content[k].Value == "name" && entry.Content[k+1].Value == derived {
						entry.Content = append(entry.Content[:k], entry.Content[k+2:]...)
						break
					}
				}
			}
		}
	}
}

// composeArgs returns a fresh slice. The base slice is shared by every compose
// invocation, so appending to it in place would let one invocation clobber
// another's arguments.
func composeArgs(base []string, extra ...string) []string {
	return slices.Concat(base, extra)
}

func runCompose(dockerCompose string, env []string, args ...string) ([]byte, error) {
	debugLog("Running docker-compose: %s %v", dockerCompose, args)
	cmd := exec.Command(dockerCompose, args...)
	cmd.Env = env
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

// resolveImagesAtBuildTime fills in `services.*.image` in a merged yaml that
// was produced with --no-interpolate, by re-running config with interpolation
// enabled and copying just the image values across.
//
// Image references are the one field that has to be concrete at build time,
// because each is matched against an image loader target and rewritten to a
// content-derived unique tag. When requireResolved is set, a reference whose
// variables cannot be interpolated is a hard error rather than a silently
// empty string.
func resolveImagesAtBuildTime(dockerCompose string, cmdArgs, env []string, merged []byte, requireResolved bool) ([]byte, error) {
	raw, err := serviceImages(merged)
	if err != nil {
		return nil, fmt.Errorf("failed to read service images: %v", err)
	}
	// Interpolating costs a second compose invocation, so only pay for it when
	// an image actually references a variable.
	needsResolution := false
	for _, image := range raw {
		if hasVariableRef(image) {
			needsResolution = true
			break
		}
	}
	if !needsResolution {
		return merged, nil
	}

	interpolated, err := runCompose(dockerCompose, env, composeArgs(cmdArgs, "config", "--format=yaml")...)
	if err != nil {
		return nil, fmt.Errorf("failed to run docker-compose config for image resolution: %v", err)
	}
	resolved, err := serviceImages(interpolated)
	if err != nil {
		return nil, fmt.Errorf("failed to read interpolated service images: %v", err)
	}

	// The names compose can interpolate from, used to tell a genuinely
	// resolved image reference from one that silently became empty.
	available := make(map[string]struct{}, len(env))
	for _, pair := range env {
		if key, _, found := strings.Cut(pair, "="); found {
			available[key] = struct{}{}
		}
	}

	out, unresolved, err := resolveServiceImages(merged, resolved, available)
	if err != nil {
		return nil, err
	}
	if len(unresolved) > 0 && requireResolved {
		return nil, fmt.Errorf(
			"image references must resolve at build time so they can be matched to an image loader, "+
				"but these are still unresolved:\n  %s\n"+
				"Declare the variables in the `interpolation_env` attribute, or give them defaults "+
				"(e.g. ${TAG:-latest}) in the compose file",
			strings.Join(unresolved, "\n  "))
	}
	return out, nil
}

// validateImageReferences checks every image compose will try to use against
// the loader-backed set and returns the references that would be pulled from
// a registry at runtime.
//
// Registry references are an error when the target has opted in but failed to
// pin them, or when the merge is known to feed a test (`strict`): either
// loaders are declared, or a docker_compose_test owns the merge. A yaml-only
// target with no loaders is allowed to carry them; the runner refuses to start
// a test from such a yaml unless `allow_registry_images` is set.
func validateImageReferences(imageLines []string, knownOriginals map[string]struct{}, allowRegistry, strict bool) ([]string, error) {
	var avail []string
	for k := range knownOriginals {
		avail = append(avail, k)
	}
	sort.Strings(avail)

	var registry []string
	for _, imageLine := range imageLines {
		imageLine = strings.TrimSpace(imageLine)
		if imageLine == "" {
			continue
		}
		matched := false
		for _, form := range normalizedTagForms(imageLine) {
			if _, ok := knownOriginals[form]; ok {
				matched = true
				break
			}
		}
		if matched {
			debugLog("Validated image has loader: %s", imageLine)
			continue
		}
		pinned := strings.Contains(imageLine, "@sha256:")
		switch {
		case pinned && allowRegistry:
			debugLog("Allowing digest-pinned registry image: %s", imageLine)
		case allowRegistry:
			return nil, fmt.Errorf(
				"image '%s' has no loader and is not pinned to a digest. `allow_registry_images` only permits "+
					"references of the form `<repo>@sha256:<digest>`. Images with loaders: %v",
				imageLine, avail)
		case pinned && strict:
			return nil, fmt.Errorf(
				"image '%s' has no loader and would be pulled from a registry at test time. "+
					"Set `allow_registry_images = True` on the target to permit digest-pinned registry images, "+
					"or provide the image through an `oci_load`/`image_load` target. Images with loaders: %v",
				imageLine, avail)
		case strict:
			return nil, fmt.Errorf(
				"image '%s' does not have a loader associated with it and is not pinned to a digest. "+
					"Provide it through an `oci_load`/`image_load` target in `images`, or pin it as "+
					"`<repo>@sha256:<digest>` and set `allow_registry_images = True`. Images with loaders: %v",
				imageLine, avail)
		default:
			debugLog("Recording registry image for runtime enforcement: %s", imageLine)
		}
		registry = append(registry, imageLine)
	}
	sort.Strings(registry)
	return registry, nil
}

func main() {
	debugLog("Starting merger")
	args, err := parseArgs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing args: %v\n", err)
		os.Exit(1)
	}
	debugLog("Parsed args: docker-compose=%s, output=%s, files=%v, image_manifests=%v",
		args.DockerCompose, args.Output, args.Files, args.ImageManifests)

	// Build docker-compose config command. -f flags come before the subcommand.
	var cmdArgs []string
	for _, file := range args.Files {
		if _, err := os.Stat(file); err != nil {
			fmt.Fprintf(os.Stderr, "Error: file does not exist or is not readable: %s\n", file)
			os.Exit(1)
		}
		absPath, err := filepath.Abs(file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error making file path absolute %s: %v\n", file, err)
			os.Exit(1)
		}
		cmdArgs = append(cmdArgs, "-f", absPath)
		debugLog("Added compose file: %s (absolute: %s)", file, absPath)
	}

	if args.ProjectName != "" {
		cmdArgs = append(cmdArgs, "--project-name", args.ProjectName)
		debugLog("Using project name: %s", args.ProjectName)
	}

	// Declared values win over anything inherited from the action environment:
	// os/exec keeps the last occurrence of a duplicated key.
	composeEnv := append(os.Environ(), args.Env...)
	debugLog("Declared interpolation environment: %v", args.Env)

	configArgs := []string{"config", "--format=yaml"}
	if args.NoInterpolate {
		configArgs = append(configArgs, "--no-interpolate")
	}
	output, err := runCompose(args.DockerCompose, composeEnv, composeArgs(cmdArgs, configArgs...)...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error running docker-compose config: %v\n", err)
		os.Exit(1)
	}
	debugLog("docker-compose config completed, output size: %d bytes", len(output))

	if args.DataManifest != "" && args.OutputRlocationpath != "" {
		dataManifest, err := loadDataManifest(args.DataManifest)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading data manifest: %v\n", err)
			os.Exit(1)
		}
		debugLog("Loaded data manifest with %d entries", len(dataManifest))

		execrootPrefix := ""
		if cwd, err := os.Getwd(); err == nil {
			execrootPrefix = filepath.Clean(cwd) + string(filepath.Separator)
		}
		outputRlocationDir := filepath.Dir(args.OutputRlocationpath)
		output = relativizePaths(output, execrootPrefix, dataManifest, outputRlocationDir)
	}

	// Describe every loader-backed image, write its loadable tarball and
	// derive its unique tags.
	var runtimeImages []images.Image
	// Map original tag (and normalized forms) -> unique tag, for YAML rewriting.
	tagToUnique := make(map[string]string)
	// Set of original tags in all normalized forms, for the validation pass.
	knownOriginals := make(map[string]struct{})

	for _, manifestPath := range args.ImageManifests {
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading image manifest %s: %v\n", manifestPath, err)
			os.Exit(1)
		}
		var manifest ImageManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing image manifest %s: %v\n", manifestPath, err)
			os.Exit(1)
		}
		if len(manifest.TagFilePaths) == 0 {
			fmt.Fprintf(os.Stderr, "Error: image manifest %s missing tag_file_paths\n", manifestPath)
			os.Exit(1)
		}
		tags, err := readTagsFromTagFiles(manifest.TagFilePaths)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading tag files for %s: %v\n", manifestPath, err)
			os.Exit(1)
		}
		img, err := prepareImage(manifest, tags)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		for _, tag := range tags {
			unique := images.UniqueTag(tag, img.ManifestDigest)
			for _, form := range normalizedTagForms(tag) {
				if existing, dup := tagToUnique[form]; dup && existing != unique {
					fmt.Fprintf(os.Stderr,
						"Error: tag %s is declared by two different images (%s and another loader); "+
							"a tag can only map to one image\n", tag, manifest.Label)
					os.Exit(1)
				}
				tagToUnique[form] = unique
				knownOriginals[form] = struct{}{}
			}
		}
		runtimeImages = append(runtimeImages, *img)
	}

	// Sort for deterministic output.
	sort.Slice(runtimeImages, func(i, j int) bool {
		return runtimeImages[i].Label < runtimeImages[j].Label
	})

	// Under --no-interpolate the image fields came through with their ${VAR}
	// references intact, but those have to be concrete to match a loader.
	if args.NoInterpolate && (len(runtimeImages) > 0 || args.OutputRuntime != "") {
		output, err = resolveImagesAtBuildTime(
			args.DockerCompose, cmdArgs, composeEnv, output, len(runtimeImages) > 0)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}

	// Classify every image compose will use, so that nothing is fetched from
	// a registry at test time without an explicit opt-in.
	var registryImages []string
	if len(runtimeImages) > 0 || args.OutputRuntime != "" {
		imagesOutput, err := runCompose(args.DockerCompose, composeEnv,
			composeArgs(cmdArgs, "config", "--images")...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error running docker-compose config --images: %v\n", err)
			os.Exit(1)
		}
		imageLines := strings.Split(strings.TrimSpace(string(imagesOutput)), "\n")
		strict := args.RequireLoaders || len(runtimeImages) > 0
		registryImages, err = validateImageReferences(imageLines, knownOriginals, args.AllowRegistryImages, strict)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}

	// Write the original (non-rewritten) merged YAML. This is the user-facing
	// artifact — diff-reviewable, shareable outside Bazel.
	originalYAML := normalizeLineEndings(output)
	debugLog("Original YAML size: %d bytes", len(originalYAML))

	outputDir := filepath.Dir(args.Output)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating output directory: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(args.Output, originalYAML, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing output file: %v\n", err)
		os.Exit(1)
	}

	// Write the test yaml: every loader-backed image replaced with its unique
	// tag and pinned to pull_policy never, and host ports released. Only the
	// test runner consumes it.
	services := map[string]string{}
	var ports []images.PortMapping
	if args.OutputRewritten != "" {
		rewrittenSrc, s, p, err := rewriteComposeYAML(output, tagToUnique, args.KeepPublishedPorts, args.ProjectName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error rewriting YAML: %v\n", err)
			os.Exit(1)
		}
		services, ports = s, p
		rewrittenOutput := normalizeLineEndings(rewrittenSrc)
		debugLog("Rewritten YAML size: %d bytes", len(rewrittenOutput))
		rewrittenDir := filepath.Dir(args.OutputRewritten)
		if err := os.MkdirAll(rewrittenDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "Error creating rewritten output dir: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(args.OutputRewritten, rewrittenOutput, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing rewritten output file: %v\n", err)
			os.Exit(1)
		}
	}

	if args.OutputRuntime != "" {
		runtime := images.RuntimeManifest{
			Project:             args.ProjectName,
			Images:              runtimeImages,
			Services:            services,
			Ports:               ports,
			RegistryImages:      registryImages,
			AllowRegistryImages: args.AllowRegistryImages,
		}
		runtimeJSON, err := json.MarshalIndent(runtime, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error marshaling runtime manifest: %v\n", err)
			os.Exit(1)
		}
		runtimeDir := filepath.Dir(args.OutputRuntime)
		if err := os.MkdirAll(runtimeDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "Error creating runtime manifest dir: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(args.OutputRuntime, runtimeJSON, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing runtime manifest: %v\n", err)
			os.Exit(1)
		}
		debugLog("Wrote runtime manifest: %s", args.OutputRuntime)
	}

	debugLog("Merger completed successfully")
}
