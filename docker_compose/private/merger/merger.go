package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// debugLog logs a message to stderr if RULES_DOCKER_COMPOSE_DEBUG is set
func debugLog(format string, args ...interface{}) {
	if os.Getenv("RULES_DOCKER_COMPOSE_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[DEBUG] "+format+"\n", args...)
	}
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
	Files               []string
	ImageManifests      []string
}

// ImageManifest is the per-loader manifest emitted by the bzl rule and
// consumed by the merger. It identifies the loader, the file used to
// compute the content digest, and the list of tags the loader writes to.
type ImageManifest struct {
	Label           string   `json:"label"`
	LoaderRlocation string   `json:"loader_rlocationpath"`
	TagFilePaths    []string `json:"tag_file_paths"`
	OCILayoutDir    string   `json:"oci_layout_dir"`
	ManifestFile    string   `json:"manifest_file"`
}

// TagRewrite records a single original->unique tag mapping for one loader.
type TagRewrite struct {
	Original string `json:"original"`
	Unique   string `json:"unique"`
}

// LoaderEntry is one loader plus all the tag rewrites it produces.
type LoaderEntry struct {
	LoaderRlocation string       `json:"loader_rlocationpath"`
	Tags            []TagRewrite `json:"tags"`
}

// RuntimeManifest is the sidecar JSON written by the merger and consumed
// at runtime by the runner/launcher. It tells them which loaders to run
// and what retag mappings to apply under the daemon-level lock.
type RuntimeManifest struct {
	Loaders []LoaderEntry `json:"loaders"`
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
	var files flagArray
	flag.Var(&files, "file", "Docker compose yaml file to merge (can be specified multiple times)")
	var imageManifests flagArray
	flag.Var(&imageManifests, "image_manifest", "Image manifest file (can be specified multiple times)")
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

	args.Files = files
	args.ImageManifests = imageManifests
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

// computeContentDigest computes sha256 of the file (or, for an OCI layout dir,
// the index.json inside it). The result captures all image content because both
// OCI manifests and rules_img manifests reference their content by digest.
func computeContentDigest(manifest ImageManifest) (string, error) {
	var target string
	if manifest.OCILayoutDir != "" {
		target = filepath.Join(manifest.OCILayoutDir, "index.json")
	} else if manifest.ManifestFile != "" {
		target = manifest.ManifestFile
	} else {
		return "", fmt.Errorf("image manifest %s has neither oci_layout_dir nor manifest_file", manifest.Label)
	}

	f, err := os.Open(target)
	if err != nil {
		return "", fmt.Errorf("failed to open digest source %s: %v", target, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("failed to hash %s: %v", target, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// uniqueTagFor takes an original tag (repo[:tag] or repo) and returns
// "<repo>:rdc-<digestPrefix>". The original tag part is replaced; everything
// up to the last ":" is treated as the repository.
func uniqueTagFor(originalTag, digestHex string) string {
	prefix := digestHex
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	repo := originalTag
	// Strip ":tag" suffix if present, but be careful with "host:port/path:tag"
	// — the repo's last segment may itself contain a colon (port). We split on
	// "/" first to isolate the final path segment, then strip any ":tag" from it.
	slash := strings.LastIndex(originalTag, "/")
	last := originalTag
	if slash != -1 {
		last = originalTag[slash+1:]
	}
	if colon := strings.LastIndex(last, ":"); colon != -1 {
		// repo == everything except the trailing ":tag"
		if slash != -1 {
			repo = originalTag[:slash+1] + last[:colon]
		} else {
			repo = last[:colon]
		}
	}
	return repo + ":rdc-" + prefix
}

// normalizedTags returns the input tag plus an entry without a leading
// "docker.io/" prefix (if present). Compose's `--images` output sometimes
// includes the docker.io/ prefix and sometimes does not, so we match both.
func normalizedTagForms(tag string) []string {
	forms := []string{tag}
	if stripped := strings.TrimPrefix(tag, "docker.io/"); stripped != tag {
		forms = append(forms, stripped)
	}
	return forms
}

// rewriteComposeYAML walks `services.*.image` in the merged compose YAML and
// replaces any reference matching an original loader tag with its unique tag.
// External references (no loader registered) are left untouched.
func rewriteComposeYAML(yamlContent []byte, mapping map[string]string) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(yamlContent, &root); err != nil {
		return nil, fmt.Errorf("failed to parse merged YAML for rewriting: %v", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return yamlContent, nil
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		return yamlContent, nil
	}
	for i := 0; i+1 < len(top.Content); i += 2 {
		key := top.Content[i]
		val := top.Content[i+1]
		if key.Value != "services" || val.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(val.Content); j += 2 {
			service := val.Content[j+1]
			if service.Kind != yaml.MappingNode {
				continue
			}
			for k := 0; k+1 < len(service.Content); k += 2 {
				skey := service.Content[k]
				sval := service.Content[k+1]
				if skey.Value != "image" || sval.Kind != yaml.ScalarNode {
					continue
				}
				original := sval.Value
				if unique, ok := mapping[original]; ok {
					debugLog("Rewriting service image: %s -> %s", original, unique)
					sval.Value = unique
				} else if stripped := strings.TrimPrefix(original, "docker.io/"); stripped != original {
					if unique, ok := mapping[stripped]; ok {
						debugLog("Rewriting service image (stripped docker.io): %s -> %s", original, unique)
						sval.Value = unique
					}
				}
			}
		}
	}
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return nil, fmt.Errorf("failed to re-marshal rewritten YAML: %v", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("failed to close YAML encoder: %v", err)
	}
	return []byte(buf.String()), nil
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

	configArgs := append(cmdArgs, "config", "--format=yaml")
	debugLog("Running docker-compose config: %s %v", args.DockerCompose, configArgs)
	configCmd := exec.Command(args.DockerCompose, configArgs...)
	configCmd.Stderr = os.Stderr
	output, err := configCmd.Output()
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

	// Load image manifests, compute content digests, derive unique tags.
	loaderEntries := make([]LoaderEntry, 0, len(args.ImageManifests))
	// Map original tag (and normalized form) -> unique tag, for YAML rewriting.
	tagToUnique := make(map[string]string)
	// Map repo[:tag] -> "" used for the loader-exists validation pass.
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
		dgst, err := computeContentDigest(manifest)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error computing content digest for %s: %v\n", manifest.Label, err)
			os.Exit(1)
		}
		debugLog("Content digest for %s: %s", manifest.Label, dgst)

		entry := LoaderEntry{LoaderRlocation: manifest.LoaderRlocation}
		for _, tag := range tags {
			unique := uniqueTagFor(tag, dgst)
			entry.Tags = append(entry.Tags, TagRewrite{Original: tag, Unique: unique})
			for _, form := range normalizedTagForms(tag) {
				tagToUnique[form] = unique
				knownOriginals[form] = struct{}{}
			}
			debugLog("Tag mapping: %s -> %s", tag, unique)
		}
		loaderEntries = append(loaderEntries, entry)
	}

	// Sort loader entries by loader rlocationpath for deterministic output.
	sort.Slice(loaderEntries, func(i, j int) bool {
		return loaderEntries[i].LoaderRlocation < loaderEntries[j].LoaderRlocation
	})

	// Validate every image referenced in the compose config has a loader,
	// unless it's pinned via "@sha256:..." (user-managed external pin).
	if len(loaderEntries) > 0 || args.OutputRuntime != "" {
		imagesArgs := append(cmdArgs, "config", "--images")
		debugLog("Running docker-compose config --images: %s %v", args.DockerCompose, imagesArgs)
		imagesCmd := exec.Command(args.DockerCompose, imagesArgs...)
		imagesCmd.Stderr = os.Stderr
		imagesOutput, err := imagesCmd.Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error running docker-compose config --images: %v\n", err)
			os.Exit(1)
		}
		imageLines := strings.Split(strings.TrimSpace(string(imagesOutput)), "\n")
		for _, imageLine := range imageLines {
			imageLine = strings.TrimSpace(imageLine)
			if imageLine == "" {
				continue
			}
			if strings.Contains(imageLine, "@sha256:") {
				continue
			}
			matched := false
			for _, form := range normalizedTagForms(imageLine) {
				if _, ok := knownOriginals[form]; ok {
					matched = true
					break
				}
			}
			if !matched && len(loaderEntries) > 0 {
				avail := make([]string, 0, len(knownOriginals))
				for k := range knownOriginals {
					avail = append(avail, k)
				}
				sort.Strings(avail)
				fmt.Fprintf(os.Stderr,
					"Error: image '%s' does not have a loader associated with it. Available images with loaders: %v\n",
					imageLine, avail)
				os.Exit(1)
			}
			debugLog("Validated image has loader: %s", imageLine)
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

	// Optionally write a rewritten copy of the YAML where each service image
	// is replaced with its content-derived unique tag. Only the test runner
	// consumes this file; the daemon-lock + retag flow only makes sense for
	// parallel-test isolation.
	if args.OutputRewritten != "" {
		rewrittenSrc := output
		if len(tagToUnique) > 0 {
			r, err := rewriteComposeYAML(output, tagToUnique)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error rewriting YAML: %v\n", err)
				os.Exit(1)
			}
			rewrittenSrc = r
		}
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
		runtime := RuntimeManifest{Loaders: loaderEntries}
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
