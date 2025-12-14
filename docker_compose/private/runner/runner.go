package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

// debugLog logs a message to stderr if RULES_DOCKER_COMPOSE_DEBUG is set
func debugLog(format string, args ...interface{}) {
	if os.Getenv("RULES_DOCKER_COMPOSE_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[DEBUG] "+format+"\n", args...)
	}
}

// ContainerStatus represents a container status from docker-compose ps --format json
type ContainerStatus struct {
	ID     string `json:"ID"`
	Name   string `json:"Name"`
	State  string `json:"State"`
	Status string `json:"Status"`
}

// checkContainersRunning checks if docker-compose containers are running based on JSON output
// Returns true if all containers are running, false otherwise
func checkContainersRunning(outputStr string) bool {
	outputStr = strings.TrimSpace(outputStr)
	debugLog("docker-compose ps output:\n%s", outputStr)

	// Check if output is empty or contains no containers
	if outputStr == "" {
		debugLog("No containers found")
		return false
	}

	// Parse JSON output - each line is a JSON object
	lines := strings.Split(outputStr, "\n")
	allRunning := true
	hasContainers := false

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var status ContainerStatus
		if err := json.Unmarshal([]byte(line), &status); err != nil {
			debugLog("Error parsing container status JSON: %v, line: %s", err, line)
			continue
		}

		hasContainers = true
		// Check if container is running (State should be "running")
		if status.State != "running" {
			debugLog("Container %s is not running (State: %s, Status: %s)", status.Name, status.State, status.Status)
			allRunning = false
		} else {
			debugLog("Container %s is running", status.Name)
		}
	}

	if !hasContainers {
		debugLog("No containers found in JSON output")
		return false
	}

	return allRunning
}

// waitForContainers waits for containers to be running with a timeout
func waitForContainers(dockerCompose, yaml string, upProcess *os.Process, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	checkInterval := 500 * time.Millisecond

	for time.Now().Before(deadline) {
		// First, check if the docker-compose up process is still running
		// If it has died, there's no point waiting - something went wrong
		if upProcess != nil {
			if err := upProcess.Signal(syscall.Signal(0)); err != nil {
				// Process is not running - this indicates docker-compose up failed
				return fmt.Errorf("docker-compose up process (PID: %d) has exited - containers failed to start", upProcess.Pid)
			}
		}

		// Check if docker-compose ps returns empty output (indicates failure)
		psCmd := exec.Command(dockerCompose, "-f", yaml, "ps", "--format", "json")
		output, err := psCmd.Output()
		if err != nil {
			debugLog("Error running docker-compose ps: %v", err)
			time.Sleep(checkInterval)
			continue
		}

		outputStr := string(output)
		// If output is empty, containers haven't started yet
		if strings.TrimSpace(outputStr) == "" {
			debugLog("docker-compose ps returned empty output, waiting for containers to start")
			time.Sleep(checkInterval)
			continue
		}

		// Check if containers are running using the output we just got
		if checkContainersRunning(outputStr) {
			debugLog("All containers are running")
			return nil
		}
		time.Sleep(checkInterval)
	}

	return fmt.Errorf("timeout waiting for containers to be running after %v", timeout)
}

// ImageInfo represents expected image information including digest
type ImageInfo struct {
	Repository string
	Digest     string
}

// LockFile represents the lock file structure
type LockFile struct {
	Mode    string            `json:"mode"`
	Digests map[string]string `json:"digests"`
}

// loadLockFile loads the lock file and returns the parsed structure
func loadLockFile(lockPath string) (*LockFile, error) {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read lock file %s: %v", lockPath, err)
	}

	var lockFile LockFile
	if err := json.Unmarshal(data, &lockFile); err != nil {
		return nil, fmt.Errorf("failed to parse lock file %s: %v", lockPath, err)
	}

	return &lockFile, nil
}

// verifyImageDigests verifies that all running images exist in the lockfile
func verifyImageDigests(dockerCompose, yaml string, expectedImages map[string]ImageInfo) error {
	// Get actual running images from docker compose images --format json
	imagesCmd := exec.Command(dockerCompose, "-f", yaml, "images", "--format", "json")
	imagesCmd.Stderr = os.Stderr
	imagesOutput, err := imagesCmd.Output()
	if err != nil {
		return fmt.Errorf("error running docker compose images --format json: %v", err)
	}

	// Parse JSON output
	type DockerComposeImage struct {
		ID         string `json:"ID"`
		Repository string `json:"Repository"`
		Tag        string `json:"Tag"`
		Name       string `json:"Name"` // Fallback if Repository is empty
		Digest     string `json:"Digest"`
	}

	actualImages := make(map[string]string) // image ref -> digest
	outputStr := strings.TrimSpace(string(imagesOutput))

	// Try to parse as JSON array first
	var imagesArray []DockerComposeImage
	if err := json.Unmarshal([]byte(outputStr), &imagesArray); err == nil {
		debugLog("Parsed docker compose images output as JSON array")
		for _, img := range imagesArray {
			imageRef := img.Repository
			if imageRef == "" {
				imageRef = img.Name
			}
			if img.Tag != "" && img.Tag != "<none>" {
				imageRef = fmt.Sprintf("%s:%s", imageRef, img.Tag)
			}

			// Get digest from Digest field or ID field (as fallback)
			digest := img.Digest
			if digest == "" && strings.HasPrefix(img.ID, "sha256:") {
				digest = img.ID
			}

			if digest != "" {
				actualImages[imageRef] = digest
				debugLog("Found running image: %s -> %s", imageRef, digest)
			} else {
				debugLog("Image %s has no digest in docker compose images output", imageRef)
			}
		}
	} else {
		// Try to parse as JSON lines (one object per line)
		debugLog("Failed to parse docker compose images output as JSON array (%v), attempting JSON lines", err)
		lines := strings.Split(outputStr, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var img DockerComposeImage
			if err := json.Unmarshal([]byte(line), &img); err != nil {
				return fmt.Errorf("error parsing docker compose images JSON: %v, line: %s", err, line)
			}

			imageRef := img.Repository
			if imageRef == "" {
				imageRef = img.Name
			}
			if img.Tag != "" && img.Tag != "<none>" {
				imageRef = fmt.Sprintf("%s:%s", imageRef, img.Tag)
			}

			// Get digest from Digest field or ID field (as fallback)
			digest := img.Digest
			if digest == "" && strings.HasPrefix(img.ID, "sha256:") {
				digest = img.ID
			}

			if digest != "" {
				actualImages[imageRef] = digest
				debugLog("Found running image: %s -> %s", imageRef, digest)
			} else {
				debugLog("Image %s has no digest in docker compose images output", imageRef)
			}
		}
	}

	// Verify each running image exists in the lockfile
	if len(actualImages) == 0 {
		return fmt.Errorf("no images found in docker compose images output")
	}

	for imageRef, actualDigest := range actualImages {
		// Find matching entry in lockfile
		var expectedInfo ImageInfo
		hasExpected := false
		for lockKey, info := range expectedImages {
			// Match if they're exactly equal, or if imageRef matches the lock key (with or without tag)
			if imageRef == lockKey || strings.HasPrefix(imageRef, lockKey+":") || strings.HasPrefix(imageRef, lockKey+"@") {
				expectedInfo = info
				hasExpected = true
				break
			}
			// Also check reverse - if lockKey starts with imageRef
			if strings.HasPrefix(lockKey, imageRef+":") || strings.HasPrefix(lockKey, imageRef+"@") {
				expectedInfo = info
				hasExpected = true
				break
			}
		}

		if !hasExpected {
			availableTags := make([]string, 0, len(expectedImages))
			for tag := range expectedImages {
				availableTags = append(availableTags, tag)
			}
			return fmt.Errorf("image '%s' (digest: %s) does not have an entry in the lock file. Available entries: %v", imageRef, actualDigest, availableTags)
		}

		// Compare digests (handle both full sha256:... and short format)
		expectedDigest := expectedInfo.Digest
		actualDigestFull := actualDigest
		if !strings.HasPrefix(actualDigest, "sha256:") {
			actualDigestFull = "sha256:" + actualDigest
		}

		// Normalize expected digest format
		expectedDigestNormalized := expectedDigest
		if !strings.HasPrefix(expectedDigestNormalized, "sha256:") {
			expectedDigestNormalized = "sha256:" + expectedDigestNormalized
		}

		if expectedDigestNormalized != actualDigestFull {
			// Try comparing just the hash part (after sha256:)
			expectedHash := expectedDigestNormalized[7:]
			actualHash := actualDigestFull[7:]

			// Compare full hashes
			if len(expectedHash) > len(actualHash) {
				// Compare prefix if actual is shorter (short format)
				if expectedHash[:len(actualHash)] != actualHash {
					return fmt.Errorf("digest mismatch for image '%s': expected `%s` (from lockfile), got `%s` (from running container)", imageRef, expectedDigest, actualDigest)
				}
			} else if len(actualHash) > len(expectedHash) {
				if actualHash[:len(expectedHash)] != expectedHash {
					return fmt.Errorf("digest mismatch for image '%s': expected `%s` (from lockfile), got `%s` (from running container)", imageRef, expectedDigest, actualDigest)
				}
			} else {
				if expectedHash != actualHash {
					return fmt.Errorf("digest mismatch for image '%s': expected `%s` (from lockfile), got `%s` (from running container)", imageRef, expectedDigest, actualDigest)
				}
			}
		}

		debugLog("Verified digest for running image %s: %s", imageRef, expectedDigest)
	}

	return nil
}

type Args struct {
	DockerCompose string
	Yaml          string
	Loaders       []string
	Lock          string
	Test          string
	TestArgs      []string
	Delay         time.Duration
}

func parseArgs() (*Args, error) {
	argsFile := os.Getenv("RULES_DOCKER_COMPOSE_TEST_ARGS_FILE")
	if argsFile == "" {
		return nil, fmt.Errorf("RULES_DOCKER_COMPOSE_TEST_ARGS_FILE environment variable is not set")
	}

	// Resolve runfiles path
	rf, err := runfiles.New()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize runfiles: %v", err)
	}

	resolvedArgsFile, err := rf.Rlocation(argsFile)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve runfiles path %s: %v", argsFile, err)
	}

	// Read args from file (one per line)
	file, err := os.Open(resolvedArgsFile)
	if err != nil {
		return nil, fmt.Errorf("failed to open args file %s: %v", resolvedArgsFile, err)
	}
	defer file.Close()

	// Collect all lines into a slice of strings
	var argLines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// Split line into flag and value, preserving quoted values
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			argLines = append(argLines, parts[0], strings.Join(parts[1:], " "))
		} else {
			argLines = append(argLines, parts[0])
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading args file: %v", err)
	}

	// Create a FlagSet and define flags
	fs := flag.NewFlagSet("runner", flag.ContinueOnError)
	args := &Args{}

	fs.StringVar(&args.DockerCompose, "docker-compose", "", "Path to docker-compose binary")
	fs.StringVar(&args.Yaml, "yaml", "", "Path to docker-compose yaml file")
	fs.StringVar(&args.Test, "test", "", "Path to test binary")

	// For delay, handle both integer (seconds) and duration string formats
	var delayStr string
	fs.StringVar(&delayStr, "delay", "", "Delay before running test (integer seconds or duration like '5s')")

	// For loader, we need to collect multiple values
	var loaders []string
	fs.Func("loader", "Image loader tool to call before docker-compose up (can be specified multiple times)", func(value string) error {
		loaders = append(loaders, value)
		return nil
	})

	// For test-arg, we need to collect multiple values
	var testArgs []string
	fs.Func("test-arg", "Test argument (can be specified multiple times)", func(value string) error {
		testArgs = append(testArgs, value)
		return nil
	})

	var lock string
	fs.StringVar(&lock, "lock", "", "Lock file (JSON mapping image tags to digests)")

	// Parse from the slice of strings
	if err := fs.Parse(argLines); err != nil {
		return nil, fmt.Errorf("failed to parse args: %v", err)
	}

	// Parse delay (handle both integer seconds and duration strings)
	if delayStr != "" {
		if delay, err := time.ParseDuration(delayStr); err == nil {
			args.Delay = delay
		} else {
			// Try parsing as integer seconds (backward compatibility)
			if seconds, err := strconv.Atoi(delayStr); err == nil {
				args.Delay = time.Duration(seconds) * time.Second
			} else {
				return nil, fmt.Errorf("invalid delay value %s: %v", delayStr, err)
			}
		}
	}

	// Resolve runfiles paths
	if args.DockerCompose != "" {
		resolved, err := rf.Rlocation(args.DockerCompose)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve docker-compose path %s: %v", args.DockerCompose, err)
		}
		args.DockerCompose = resolved
	}
	if args.Yaml != "" {
		resolved, err := rf.Rlocation(args.Yaml)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve yaml path %s: %v", args.Yaml, err)
		}
		args.Yaml = resolved
	}
	// Resolve loader paths
	for i, loader := range loaders {
		resolved, err := rf.Rlocation(loader)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve loader path %s: %v", loader, err)
		}
		loaders[i] = resolved
	}
	args.Loaders = loaders

	// Resolve lock file path if provided
	if lock != "" {
		resolved, err := rf.Rlocation(lock)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve lock file path %s: %v", lock, err)
		}
		args.Lock = resolved
	}

	if args.Test != "" {
		resolved, err := rf.Rlocation(args.Test)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve test path %s: %v", args.Test, err)
		}
		args.Test = resolved
	}

	args.TestArgs = testArgs

	// Validate required flags
	if args.DockerCompose == "" {
		return nil, fmt.Errorf("missing required flag: -docker-compose")
	}
	if args.Yaml == "" {
		return nil, fmt.Errorf("missing required flag: -yaml")
	}

	return args, nil
}

func main() {
	debugLog("Starting runner")
	args, err := parseArgs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing args: %v\n", err)
		os.Exit(1)
	}
	debugLog("Parsed args: docker-compose=%s, yaml=%s, loaders=%v, lock=%s, test=%s, test-args=%v, delay=%v",
		args.DockerCompose, args.Yaml, args.Loaders, args.Lock, args.Test, args.TestArgs, args.Delay)

	// Get output directory for logs
	outputDir := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR")
	if outputDir == "" {
		outputDir = os.TempDir()
	}
	debugLog("Output directory: %s", outputDir)

	logFile := filepath.Join(outputDir, "docker_compose.log")
	debugLog("Log file: %s", logFile)
	logFileHandle, err := os.Create(logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating log file: %v\n", err)
		os.Exit(1)
	}
	defer logFileHandle.Close()

	// Track docker-compose up process
	var upProcess *os.Process

	// Cleanup function to stop docker-compose and run down
	cleanupCalled := false
	cleanupFunc := func() {
		if cleanupCalled {
			return
		}
		cleanupCalled = true

		// Send SIGINT to docker-compose up process if it's still running
		if upProcess != nil {
			debugLog("Sending SIGINT to docker-compose up process (PID: %d)", upProcess.Pid)
			if err := upProcess.Signal(os.Interrupt); err != nil {
				debugLog("Error sending SIGINT to docker-compose up: %v", err)
			} else {
				// Wait for the process to finish
				_, err := upProcess.Wait()
				if err != nil {
					debugLog("docker-compose up process exited with error: %v", err)
				} else {
					debugLog("docker-compose up process exited successfully")
				}
			}
		}

		// Run docker-compose down for cleanup
		debugLog("Running docker-compose down")
		cmd := exec.Command(args.DockerCompose, "-f", args.Yaml, "down")
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Error running docker-compose down: %v\n", err)
		} else {
			debugLog("docker-compose down completed")
		}
	}

	// Handle signals to ensure cleanup
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		cleanupFunc()
		os.Exit(1)
	}()

	// Ensure cleanup on exit
	defer cleanupFunc()

	// Run image loader tools if provided (in order)
	if len(args.Loaders) > 0 {
		debugLog("Running %d image loader(s)", len(args.Loaders))
		for i, loader := range args.Loaders {
			debugLog("Running loader %d: %s", i+1, loader)
			loaderCmd := exec.Command(loader)
			loaderCmd.Stdout = os.Stderr
			loaderCmd.Stderr = os.Stderr
			if err := loaderCmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "Error running image loader %s: %v\n", loader, err)
				os.Exit(1)
			}
			debugLog("Loader %d completed successfully", i+1)
		}
	} else {
		debugLog("No image loaders specified")
	}

	// Load lock file data if provided (will be used for validation after containers start)
	var expectedImages map[string]ImageInfo
	if args.Lock != "" {
		debugLog("Loading lock file for digest verification: %s", args.Lock)
		lockFile, err := loadLockFile(args.Lock)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading lock file: %v\n", err)
			os.Exit(1)
		}
		debugLog("Loaded lock file with mode=%s, %d image(s)", lockFile.Mode, len(lockFile.Digests))
		if len(lockFile.Digests) == 0 {
			fmt.Fprintf(os.Stderr, "Warning: lock file %s has no digests. Make sure images in docker-compose yaml have digests.\n", args.Lock)
		}
		for tag, digest := range lockFile.Digests {
			debugLog("  Lock entry: %s -> %s", tag, digest)
		}

		// Convert lock data to expectedImages format
		expectedImages = make(map[string]ImageInfo)
		for tag, digest := range lockFile.Digests {
			expectedImages[tag] = ImageInfo{
				Repository: tag, // tag includes repository
				Digest:     digest,
			}
		}
	}

	// Spawn docker-compose up as a background process (without -d to capture logs)
	debugLog("Spawning docker-compose up: %s -f %s up", args.DockerCompose, args.Yaml)
	upCmd := exec.Command(args.DockerCompose, "-f", args.Yaml, "up", "--timeout", "60", "--no-build")
	// Write both stdout and stderr to the log file only (container logs will be in docker_compose.log)
	upCmd.Stdout = logFileHandle
	upCmd.Stderr = logFileHandle
	if err := upCmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting docker-compose up: %v\n", err)
		os.Exit(1)
	}
	upProcess = upCmd.Process
	debugLog("docker-compose up process started (PID: %d)", upProcess.Pid)

	// Wait for containers to be running (with timeout)
	if err := waitForContainers(args.DockerCompose, args.Yaml, upProcess, 10*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "Error: containers failed to start: %v\n", err)
		// Send SIGINT to stop docker-compose
		if upProcess != nil {
			upProcess.Signal(os.Interrupt)
			upProcess.Wait()
		}
		os.Exit(1)
	}
	debugLog("All containers are running and ready")

	// Verify image digests from lock file now that containers are running
	if args.Lock != "" && len(expectedImages) > 0 {
		debugLog("Verifying image digests from lock file")
		if err := verifyImageDigests(args.DockerCompose, args.Yaml, expectedImages); err != nil {
			fmt.Fprintf(os.Stderr, "Error verifying image digests: %v\n", err)
			// Send SIGINT to stop docker-compose
			if upProcess != nil {
				upProcess.Signal(os.Interrupt)
				upProcess.Wait()
			}
			os.Exit(1)
		}
		debugLog("All image digests verified successfully")
	}

	// Sleep for optional delay
	if args.Delay > 0 {
		debugLog("Sleeping for delay: %v", args.Delay)
		time.Sleep(args.Delay)
		debugLog("Delay completed")
	}

	// Run the test binary if provided
	var testExitCode int
	if args.Test != "" {
		debugLog("Running test: %s with args: %v", args.Test, args.TestArgs)
		testCmd := exec.Command(args.Test, args.TestArgs...)
		testCmd.Stdout = os.Stdout
		testCmd.Stderr = os.Stderr
		if err := testCmd.Run(); err != nil {
			if exitError, ok := err.(*exec.ExitError); ok {
				testExitCode = exitError.ExitCode()
				debugLog("Test exited with code: %d", testExitCode)
			} else {
				fmt.Fprintf(os.Stderr, "Error running test: %v\n", err)
				testExitCode = 1
				debugLog("Test failed with error: %v", err)
			}
		} else {
			debugLog("Test completed successfully")
		}
	} else {
		debugLog("No test specified")
	}

	// Send SIGINT to docker-compose up process to stop it gracefully
	// This will cause it to stop containers and write all logs
	if upProcess != nil {
		debugLog("Sending SIGINT to docker-compose up process (PID: %d)", upProcess.Pid)
		if err := upProcess.Signal(os.Interrupt); err != nil {
			fmt.Fprintf(os.Stderr, "Error sending SIGINT to docker-compose up: %v\n", err)
		} else {
			// Wait for the process to finish
			debugLog("Waiting for docker-compose up process to finish")
			_, err := upProcess.Wait()
			if err != nil {
				debugLog("docker-compose up process exited with error: %v", err)
			} else {
				debugLog("docker-compose up process exited successfully")
			}
		}
		upProcess = nil // Mark as cleaned up so defer doesn't try again
	}

	// Exit with test exit code
	debugLog("Exiting with code: %d", testExitCode)
	os.Exit(testExitCode)
}
