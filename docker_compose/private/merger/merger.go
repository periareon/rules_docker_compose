package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/periareon/rules_docker_compose/docker_compose/private/digest"
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

type Args struct {
	DockerCompose  string
	Output         string
	OutputLock     string
	ProjectName    string
	DigestMode     string // "oci" or "docker"
	Files          []string
	ImageManifests []string
}

type ImageManifest struct {
	Label        string   `json:"label"`
	TagFilePaths []string `json:"tag_file_paths"`
	OCILayoutDir string   `json:"oci_layout_dir"`
	ManifestFile string   `json:"manifest_file"`
}

type ImageInfo struct {
	Repository string
}

type ComposeService struct {
	Image string      `yaml:"image"`
	Build interface{} `yaml:"build"`
}

type ComposeFile struct {
	Services map[string]ComposeService `yaml:"services"`
	Version  string                    `yaml:"version"`
}

func parseArgs() (*Args, error) {
	args := &Args{}
	flag.StringVar(&args.DockerCompose, "docker-compose", "", "Path to docker-compose binary")
	flag.StringVar(&args.Output, "output", "", "Path to output merged yaml file")
	flag.StringVar(&args.OutputLock, "output-lock", "", "Path to output lock file (JSON mapping image tags to digests)")
	flag.StringVar(&args.ProjectName, "project-name", "", "Project name for docker-compose")
	flag.StringVar(&args.DigestMode, "digest-mode", "oci", "Digest mode: 'oci' uses manifest digest, 'docker' uses config digest (for docker load compatibility)")
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
	if args.DigestMode != "oci" && args.DigestMode != "docker-legacy" && args.DigestMode != "docker-containerd" {
		return nil, fmt.Errorf("invalid -digest-mode: %s (must be 'oci', 'docker-legacy', or 'docker-containerd')", args.DigestMode)
	}

	args.Files = files
	args.ImageManifests = imageManifests
	return args, nil
}

// flagArray is a custom flag type that allows multiple values
type flagArray []string

func (f *flagArray) String() string {
	return fmt.Sprintf("%v", []string(*f))
}

func (f *flagArray) Set(value string) error {
	*f = append(*f, value)
	return nil
}

// readRepositoryFromTagFiles reads the repository from the first tag across all tag files
// Tag format: repository:tag (one per line)
func readRepositoryFromTagFiles(tagFilePaths []string) (string, error) {
	for _, tagFilePath := range tagFilePaths {
		data, err := os.ReadFile(tagFilePath)
		if err != nil {
			return "", fmt.Errorf("failed to read tag file %s: %v", tagFilePath, err)
		}

		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			// Extract repository from first tag (format: repository:tag)
			if strings.Contains(line, ":") {
				return strings.SplitN(line, ":", 2)[0], nil
			}
			// If no colon, the whole line is the tag (might be just repository)
			return line, nil
		}
	}

	return "", fmt.Errorf("all tag files are empty or contain no valid tags")
}

// loadImageManifests loads all image manifests and returns a map of normalized repository to image info
func loadImageManifests(manifestPaths []string) (map[string]ImageInfo, error) {
	images := make(map[string]ImageInfo)

	for _, manifestPath := range manifestPaths {
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read image manifest %s: %v", manifestPath, err)
		}

		var manifest ImageManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return nil, fmt.Errorf("failed to parse image manifest %s: %v", manifestPath, err)
		}

		if len(manifest.TagFilePaths) == 0 {
			return nil, fmt.Errorf("image manifest %s missing tag_file_paths", manifestPath)
		}

		repository, err := readRepositoryFromTagFiles(manifest.TagFilePaths)
		if err != nil {
			return nil, err
		}

		images[repository] = ImageInfo{
			Repository: repository,
		}
	}

	return images, nil
}

// readDigestFromManifestFile reads the manifest digest from a rules_img manifest file
func readDigestFromManifestFile(manifestFile string) (string, error) {
	data, err := os.ReadFile(manifestFile)
	if err != nil {
		return "", fmt.Errorf("failed to read manifest file %s: %v", manifestFile, err)
	}

	var ociManifest struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(data, &ociManifest); err == nil {
		if len(ociManifest.Manifests) > 0 {
			dgst := ociManifest.Manifests[0].Digest
			if strings.HasPrefix(dgst, "sha256:") {
				return dgst, nil
			}
		}
		if ociManifest.Config.Digest != "" {
			dgst := ociManifest.Config.Digest
			if strings.HasPrefix(dgst, "sha256:") {
				return dgst, nil
			}
		}
	}

	var dockerManifest struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(data, &dockerManifest); err == nil && dockerManifest.Config.Digest != "" {
		dgst := dockerManifest.Config.Digest
		if strings.HasPrefix(dgst, "sha256:") {
			return dgst, nil
		}
	}

	return "", fmt.Errorf("no valid digest found in manifest file")
}

// loadImageManifestsWithDigests loads image manifests and extracts digests
// digestMode can be:
// - "oci": uses OCI manifest digest (for OCI-compatible tools)
// - "docker-legacy": uses config digest (for Docker without containerd storage)
// - "docker-containerd": uses Docker V2 manifest digest (for Docker with containerd storage)
func loadImageManifestsWithDigests(manifestPaths []string, digestMode string) (map[string]string, error) {
	// Map of repository to digest
	imageDigests := make(map[string]string)

	for _, manifestPath := range manifestPaths {
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read image manifest %s: %v", manifestPath, err)
		}

		var manifest ImageManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return nil, fmt.Errorf("failed to parse image manifest %s: %v", manifestPath, err)
		}

		if len(manifest.TagFilePaths) == 0 {
			return nil, fmt.Errorf("image manifest %s missing tag_file_paths", manifestPath)
		}

		// Read all tags from tag files (not just repository)
		var allTags []string
		for _, tagFilePath := range manifest.TagFilePaths {
			tagData, err := os.ReadFile(tagFilePath)
			if err != nil {
				return nil, fmt.Errorf("failed to read tag file %s: %v", tagFilePath, err)
			}
			lines := strings.Split(string(tagData), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line != "" {
					allTags = append(allTags, line)
				}
			}
		}

		var dgst string
		if manifest.OCILayoutDir != "" {
			switch digestMode {
			case "docker-legacy":
				// Config digest for Docker without containerd storage
				dgst, err = digest.ReadConfigDigestFromOCILayout(manifest.OCILayoutDir)
			case "docker-containerd":
				// Docker V2 manifest digest for Docker with containerd storage
				dgst, err = digest.ReadDockerContainerdImageIDFromOCILayout(manifest.OCILayoutDir)
			default: // "oci" or unspecified
				// OCI manifest digest
				dgst, err = digest.ReadOCIManifestDigest(manifest.OCILayoutDir)
			}
			if err != nil {
				return nil, err
			}
		} else if manifest.ManifestFile != "" {
			switch digestMode {
			case "docker-legacy", "docker-containerd":
				// Use config digest for docker load compatibility (rules_img)
				dgst, err = digest.ReadConfigDigestFromManifestFile(manifest.ManifestFile)
			default: // "oci" or unspecified
				// Use manifest digest for OCI mode
				dgst, err = readDigestFromManifestFile(manifest.ManifestFile)
			}
			if err != nil {
				return nil, err
			}
		} else {
			return nil, fmt.Errorf("image manifest %s has neither oci_layout_dir nor manifest_file", manifestPath)
		}

		// Map all tags to the same digest
		for _, tag := range allTags {
			imageDigests[tag] = dgst
			debugLog("Mapped tag to digest (%s mode): %s -> %s", digestMode, tag, dgst)
		}
	}

	return imageDigests, nil
}

// generateLockFileFromManifests generates a lock file by mapping images from docker-compose config to digests from image manifests
// digestMode can be "oci", "docker", or "docker-desktop"
func generateLockFileFromManifests(dockerCompose string, cmdArgs []string, manifestPaths []string, lockPath string, digestMode string) error {
	// Get list of images from docker-compose config --images
	imagesArgs := append(cmdArgs, "config", "--images")
	debugLog("Running docker-compose config --images for lock file: %s %v", dockerCompose, imagesArgs)
	imagesCmd := exec.Command(dockerCompose, imagesArgs...)
	imagesCmd.Stderr = os.Stderr
	imagesOutput, err := imagesCmd.Output()
	if err != nil {
		return fmt.Errorf("error running docker-compose config --images: %v", err)
	}

	// Parse images from output (one per line)
	imageLines := strings.Split(strings.TrimSpace(string(imagesOutput)), "\n")

	// Load image manifests to get digests
	imageDigests, err := loadImageManifestsWithDigests(manifestPaths, digestMode)
	if err != nil {
		return fmt.Errorf("error loading image manifests with digests: %v", err)
	}

	// Map of image tag to digest
	lockData := make(map[string]string)

	for _, imageLine := range imageLines {
		imageLine = strings.TrimSpace(imageLine)
		if imageLine == "" {
			continue
		}

		// Find matching digest in imageDigests
		dgst, found := imageDigests[imageLine]
		if !found {
			// Try to match by repository (without tag) or tag prefix matching
			for tag, d := range imageDigests {
				// Check if imageLine matches tag or starts with repository part of tag
				if imageLine == tag || strings.HasPrefix(imageLine, tag+":") || strings.HasPrefix(tag, imageLine+":") {
					dgst = d
					found = true
					break
				}
			}
		}

		if found {
			lockData[imageLine] = dgst
			debugLog("Added lock entry: %s -> %s", imageLine, dgst)

			// Add sanitized entry without docker.io/ prefix if present
			if strings.HasPrefix(imageLine, "docker.io/") {
				sanitized := imageLine[len("docker.io/"):]
				lockData[sanitized] = dgst
				debugLog("Added sanitized lock entry: %s -> %s", sanitized, dgst)
			}
		} else {
			return fmt.Errorf("image '%s' from docker-compose config --images does not have a digest in image manifests", imageLine)
		}
	}

	// Create lock file structure with mode and digests
	lockFile := struct {
		Mode    string            `json:"mode"`
		Digests map[string]string `json:"digests"`
	}{
		Mode:    digestMode,
		Digests: lockData,
	}

	// Write lock file as JSON
	lockJSON, err := json.MarshalIndent(lockFile, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal lock data: %v", err)
	}

	// Ensure output directory exists
	lockDir := filepath.Dir(lockPath)
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		return fmt.Errorf("failed to create lock file directory: %v", err)
	}

	if err := os.WriteFile(lockPath, lockJSON, 0644); err != nil {
		return fmt.Errorf("failed to write lock file: %v", err)
	}

	return nil
}

func main() {
	debugLog("Starting merger")
	args, err := parseArgs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing args: %v\n", err)
		os.Exit(1)
	}
	debugLog("Parsed args: docker-compose=%s, output=%s, digest-mode=%s, files=%v, image_manifests=%v",
		args.DockerCompose, args.Output, args.DigestMode, args.Files, args.ImageManifests)

	// Build docker-compose config command
	// The -f flags must come before the subcommand
	// Make file paths absolute to ensure docker-compose can find them
	var cmdArgs []string
	for _, file := range args.Files {
		// Check if file exists
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

	// Add project name if provided
	if args.ProjectName != "" {
		cmdArgs = append(cmdArgs, "--project-name", args.ProjectName)
		debugLog("Using project name: %s", args.ProjectName)
	}

	// Run docker-compose config to get merged yaml
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

	// Generate lock file if requested
	if args.OutputLock != "" {
		if len(args.ImageManifests) == 0 {
			// No images - create an empty lock file with mode
			debugLog("No image manifests provided, creating empty lock file")
			emptyLockData := struct {
				Mode    string            `json:"mode"`
				Digests map[string]string `json:"digests"`
			}{
				Mode:    args.DigestMode,
				Digests: map[string]string{},
			}
			emptyLock, _ := json.MarshalIndent(emptyLockData, "", "  ")
			if err := os.WriteFile(args.OutputLock, emptyLock, 0644); err != nil {
				fmt.Fprintf(os.Stderr, "Error writing empty lock file: %v\n", err)
				os.Exit(1)
			}
		} else {
			if err := generateLockFileFromManifests(args.DockerCompose, cmdArgs, args.ImageManifests, args.OutputLock, args.DigestMode); err != nil {
				fmt.Fprintf(os.Stderr, "Error generating lock file: %v\n", err)
				os.Exit(1)
			}
		}
		debugLog("Lock file generated (%s mode): %s", args.DigestMode, args.OutputLock)
		// Log lock file contents for debugging
		data, readErr := os.ReadFile(args.OutputLock)
		if readErr == nil {
			var lockData map[string]string
			if jsonErr := json.Unmarshal(data, &lockData); jsonErr == nil {
				debugLog("Lock file contains %d entries", len(lockData))
				for tag, dgst := range lockData {
					debugLog("  Lock entry: %s -> %s", tag, dgst)
				}
			}
		}
	}

	// Validate that all images have loaders if image manifests are provided
	if len(args.ImageManifests) > 0 {
		debugLog("Loading %d image manifest(s) for validation", len(args.ImageManifests))
		imageMap, err := loadImageManifests(args.ImageManifests)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading image manifests: %v\n", err)
			os.Exit(1)
		}
		debugLog("Loaded %d image(s) from manifests", len(imageMap))
		for repo := range imageMap {
			debugLog("  Image: %s", repo)
		}

		// Run docker-compose config --images to get list of images
		// cmdArgs already includes project name if provided
		imagesArgs := append(cmdArgs, "config", "--images")
		debugLog("Running docker-compose config --images: %s %v", args.DockerCompose, imagesArgs)
		imagesCmd := exec.Command(args.DockerCompose, imagesArgs...)
		imagesCmd.Stderr = os.Stderr
		imagesOutput, err := imagesCmd.Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error running docker-compose config --images: %v\n", err)
			os.Exit(1)
		}

		// Parse images from output (one per line)
		imageLines := strings.Split(strings.TrimSpace(string(imagesOutput)), "\n")
		debugLog("Found %d image(s) in docker-compose file", len(imageLines))

		// Validate each image has a loader
		for _, imageLine := range imageLines {
			imageLine = strings.TrimSpace(imageLine)
			if imageLine == "" {
				continue
			}
			debugLog("Validating image: %s", imageLine)
			// Check if image matches any repository in the map
			found := false
			for repo := range imageMap {
				// Check if the image line starts with the repository (handles tags and digests)
				if imageLine == repo || strings.HasPrefix(imageLine, repo+":") || strings.HasPrefix(imageLine, repo+"@") {
					found = true
					break
				}
			}
			if !found {
				fmt.Fprintf(os.Stderr, "Error: image '%s' does not have a loader associated with it. Available images with loaders: %v\n",
					imageLine, getAvailableRepositories(imageMap))
				os.Exit(1)
			}
			debugLog("  Image %s has a loader", imageLine)
		}
	}

	// Normalize line endings for consistent output across platforms (CRLF -> LF)
	finalOutput := normalizeLineEndings(output)
	debugLog("Final YAML size: %d bytes", len(finalOutput))

	// Ensure output directory exists
	outputDir := filepath.Dir(args.Output)
	debugLog("Creating output directory: %s", outputDir)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating output directory: %v\n", err)
		os.Exit(1)
	}

	// Write merged yaml to output file
	debugLog("Writing output to: %s", args.Output)
	if err := os.WriteFile(args.Output, finalOutput, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing output file: %v\n", err)
		os.Exit(1)
	}
	debugLog("Merger completed successfully")
}

func getAvailableRepositories(imageMap map[string]ImageInfo) []string {
	repos := make([]string, 0, len(imageMap))
	for repo := range imageMap {
		repos = append(repos, repo)
	}
	return repos
}

// generateLockFile parses the docker-compose YAML and generates a lock file mapping image tags to digests
func generateLockFile(yamlContent []byte, lockPath string) error {
	var compose ComposeFile
	if err := yaml.Unmarshal(yamlContent, &compose); err != nil {
		return fmt.Errorf("failed to parse docker-compose YAML: %v", err)
	}

	// Map of image tag (repository:tag) to digest
	lockData := make(map[string]string)

	if compose.Services != nil {
		for _, service := range compose.Services {
			if service.Image == "" {
				continue
			}

			// Parse image reference to extract tag and digest
			// Format can be: repository:tag, repository@sha256:digest, or repository:tag@sha256:digest
			imageRef := service.Image

			// Check if image has a digest (@sha256:...)
			if idx := strings.LastIndex(imageRef, "@"); idx != -1 {
				dgst := imageRef[idx+1:]
				if strings.HasPrefix(dgst, "sha256:") {
					// Extract the tag part (everything before @)
					tagPart := imageRef[:idx]
					lockData[tagPart] = dgst
					debugLog("Found image with digest: %s -> %s", tagPart, dgst)
				}
			} else {
				// No digest in the image reference, skip it
				debugLog("Skipping image without digest: %s", imageRef)
			}
		}
	}

	// Write lock file as JSON
	lockJSON, err := json.MarshalIndent(lockData, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal lock data: %v", err)
	}

	// Ensure output directory exists
	lockDir := filepath.Dir(lockPath)
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		return fmt.Errorf("failed to create lock file directory: %v", err)
	}

	if err := os.WriteFile(lockPath, lockJSON, 0644); err != nil {
		return fmt.Errorf("failed to write lock file: %v", err)
	}

	return nil
}
