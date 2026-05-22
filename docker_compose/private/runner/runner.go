package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
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

func checkContainersRunning(outputStr string) bool {
	outputStr = strings.TrimSpace(outputStr)
	debugLog("docker-compose ps output:\n%s", outputStr)

	if outputStr == "" {
		debugLog("No containers found")
		return false
	}

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

func waitForContainers(dockerCompose, yaml string, upProcess *os.Process, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	checkInterval := 500 * time.Millisecond

	for time.Now().Before(deadline) {
		if upProcess != nil {
			if err := upProcess.Signal(syscall.Signal(0)); err != nil {
				return fmt.Errorf("docker-compose up process (PID: %d) has exited - containers failed to start", upProcess.Pid)
			}
		}

		psCmd := exec.Command(dockerCompose, "-f", yaml, "ps", "--format", "json")
		output, err := psCmd.Output()
		if err != nil {
			debugLog("Error running docker-compose ps: %v", err)
			time.Sleep(checkInterval)
			continue
		}

		outputStr := string(output)
		if strings.TrimSpace(outputStr) == "" {
			debugLog("docker-compose ps returned empty output, waiting for containers to start")
			time.Sleep(checkInterval)
			continue
		}

		if checkContainersRunning(outputStr) {
			debugLog("All containers are running")
			return nil
		}
		time.Sleep(checkInterval)
	}

	return fmt.Errorf("timeout waiting for containers to be running after %v", timeout)
}

// TagRewrite mirrors the merger's sidecar JSON entry.
type TagRewrite struct {
	Original string `json:"original"`
	Unique   string `json:"unique"`
}

// LoaderEntry mirrors the merger's sidecar JSON entry.
type LoaderEntry struct {
	LoaderRlocation string       `json:"loader_rlocationpath"`
	Tags            []TagRewrite `json:"tags"`
}

// RuntimeManifest mirrors the merger's sidecar JSON.
type RuntimeManifest struct {
	Loaders []LoaderEntry `json:"loaders"`
}

func loadRuntimeManifest(path string) (*RuntimeManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read runtime manifest %s: %v", path, err)
	}
	var rm RuntimeManifest
	if err := json.Unmarshal(data, &rm); err != nil {
		return nil, fmt.Errorf("failed to parse runtime manifest %s: %v", path, err)
	}
	return &rm, nil
}

// engineBinary returns the container engine binary to invoke for
// `network create/inspect/rm` and `tag` operations. Defaults to "docker"
// on PATH; override with RULES_DOCKER_COMPOSE_ENGINE_BINARY (e.g. "podman").
func engineBinary() string {
	if v := os.Getenv("RULES_DOCKER_COMPOSE_ENGINE_BINARY"); v != "" {
		return v
	}
	return "docker"
}

// lockNameFor returns the daemon-level lock network name for a given
// original tag. The name is bounded to a safe Docker/Podman identifier.
func lockNameFor(originalTag string) string {
	h := sha256.Sum256([]byte(originalTag))
	return "rdc-loadlock-" + hex.EncodeToString(h[:])[:16]
}

const (
	lockTimeoutStaleMS = int64(120 * 1000)
	lockAcquireBudget  = 60 * time.Second
)

// processAlive returns true if the given PID (on this host) is running.
// On Linux this is `/proc/<pid>`; elsewhere we fall back to `kill -0`.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err == nil {
		return true
	} else if !os.IsNotExist(err) {
		// Not Linux or /proc not mounted; fall through.
	} else {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}

// inspectLockLabels runs `<engine> network inspect <name>` and returns the
// rdc-acquired-by PID and rdc-acquired-at unix-millis label values, if any.
func inspectLockLabels(engine, name string) (pid int, acquiredAtMs int64, found bool) {
	cmd := exec.Command(engine, "network", "inspect", name, "--format", "{{json .Labels}}")
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, false
	}
	var labels map[string]string
	if err := json.Unmarshal(out, &labels); err != nil {
		return 0, 0, false
	}
	if v, ok := labels["rdc-acquired-by"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			pid = n
		}
	}
	if v, ok := labels["rdc-acquired-at"]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			acquiredAtMs = n
		}
	}
	return pid, acquiredAtMs, true
}

// forceReleaseLock removes a stale lock network. Best-effort: errors are logged
// only, since the next acquire attempt will retry anyway.
func forceReleaseLock(engine, name string) {
	cmd := exec.Command(engine, "network", "rm", name)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		debugLog("force-release of lock %s failed (likely already gone): %v", name, err)
	} else {
		debugLog("Force-released stale lock: %s", name)
	}
}

// acquireLock atomically creates the lock network for the given original tag.
// Retries with stale-lock detection until success or the budget is exhausted.
func acquireLock(engine, originalTag string) (string, error) {
	name := lockNameFor(originalTag)
	deadline := time.Now().Add(lockAcquireBudget)
	myPid := os.Getpid()
	for {
		now := time.Now().UnixMilli()
		cmd := exec.Command(engine, "network", "create",
			"--label", fmt.Sprintf("rdc-acquired-at=%d", now),
			"--label", fmt.Sprintf("rdc-acquired-by=%d", myPid),
			"--label", fmt.Sprintf("rdc-original-tag=%s", originalTag),
			name,
		)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil {
			debugLog("Acquired lock %s for tag %s", name, originalTag)
			return name, nil
		} else {
			debugLog("Lock %s contended (err=%v, stderr=%s)", name, err, stderr.String())
		}

		// Inspect existing lock for staleness.
		if pid, atMs, ok := inspectLockLabels(engine, name); ok {
			stale := false
			if !processAlive(pid) {
				debugLog("Lock %s held by dead PID %d, considering stale", name, pid)
				stale = true
			} else if atMs > 0 && (time.Now().UnixMilli()-atMs) > lockTimeoutStaleMS {
				debugLog("Lock %s older than %dms, considering stale", name, lockTimeoutStaleMS)
				stale = true
			}
			if stale {
				forceReleaseLock(engine, name)
				continue
			}
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out acquiring lock for tag %s (network %s)", originalTag, name)
		}
		jitter := time.Duration(rand.Int63n(int64(50 * time.Millisecond)))
		time.Sleep(50*time.Millisecond + jitter)
	}
}

func releaseLock(engine, name string) {
	cmd := exec.Command(engine, "network", "rm", name)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to release lock %s: %v\n", name, err)
	} else {
		debugLog("Released lock %s", name)
	}
}

// retag invokes `<engine> tag <src> <dst>`. Fails loudly if src isn't loaded.
func retag(engine, src, dst string) error {
	cmd := exec.Command(engine, "tag", src, dst)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("`%s tag %s %s` failed: %v", engine, src, dst, err)
	}
	debugLog("Retagged %s -> %s", src, dst)
	return nil
}

// loadAndRetag runs the per-loader lock/load/retag/unlock cycle.
func loadAndRetag(engine string, loaderPath string, entry LoaderEntry, runfilesEnv []string) error {
	// Deduplicate original tags so we acquire each lock once.
	seen := make(map[string]bool)
	var originals []string
	for _, t := range entry.Tags {
		if seen[t.Original] {
			continue
		}
		seen[t.Original] = true
		originals = append(originals, t.Original)
	}

	var acquired []string
	defer func() {
		for _, name := range acquired {
			releaseLock(engine, name)
		}
	}()

	for _, original := range originals {
		name, err := acquireLock(engine, original)
		if err != nil {
			return fmt.Errorf("failed to acquire lock for tag %s: %v", original, err)
		}
		acquired = append(acquired, name)
	}

	debugLog("Running loader: %s", loaderPath)
	loaderCmd := exec.Command(loaderPath)
	loaderCmd.Stdout = os.Stderr
	loaderCmd.Stderr = os.Stderr
	loaderCmd.Env = append(os.Environ(), runfilesEnv...)
	if err := loaderCmd.Run(); err != nil {
		return fmt.Errorf("loader %s failed: %v", loaderPath, err)
	}
	debugLog("Loader %s completed", loaderPath)

	for _, t := range entry.Tags {
		if err := retag(engine, t.Original, t.Unique); err != nil {
			return err
		}
	}
	return nil
}

type Args struct {
	DockerCompose   string
	Yaml            string
	RuntimeManifest string
	Test            string
	TestArgs        []string
	Delay           time.Duration
}

func parseArgs() (*Args, error) {
	argsFile := os.Getenv("RULES_DOCKER_COMPOSE_TEST_ARGS_FILE")
	if argsFile == "" {
		return nil, fmt.Errorf("RULES_DOCKER_COMPOSE_TEST_ARGS_FILE environment variable is not set")
	}

	rf, err := runfiles.New()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize runfiles: %v", err)
	}

	resolvedArgsFile, err := rf.Rlocation(argsFile)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve runfiles path %s: %v", argsFile, err)
	}

	file, err := os.Open(resolvedArgsFile)
	if err != nil {
		return nil, fmt.Errorf("failed to open args file %s: %v", resolvedArgsFile, err)
	}
	defer file.Close()

	var argLines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
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

	fs := flag.NewFlagSet("runner", flag.ContinueOnError)
	args := &Args{}

	fs.StringVar(&args.DockerCompose, "docker-compose", "", "Path to docker-compose binary")
	fs.StringVar(&args.Yaml, "yaml", "", "Path to docker-compose yaml file")
	fs.StringVar(&args.RuntimeManifest, "runtime-manifest", "", "Path to runtime manifest JSON")
	fs.StringVar(&args.Test, "test", "", "Path to test binary")

	var delayStr string
	fs.StringVar(&delayStr, "delay", "", "Delay before running test (integer seconds or duration like '5s')")

	var testArgs []string
	fs.Func("test-arg", "Test argument (can be specified multiple times)", func(value string) error {
		testArgs = append(testArgs, value)
		return nil
	})

	if err := fs.Parse(argLines); err != nil {
		return nil, fmt.Errorf("failed to parse args: %v", err)
	}

	if delayStr != "" {
		if delay, err := time.ParseDuration(delayStr); err == nil {
			args.Delay = delay
		} else {
			if seconds, err := strconv.Atoi(delayStr); err == nil {
				args.Delay = time.Duration(seconds) * time.Second
			} else {
				return nil, fmt.Errorf("invalid delay value %s: %v", delayStr, err)
			}
		}
	}

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
	if args.RuntimeManifest != "" {
		resolved, err := rf.Rlocation(args.RuntimeManifest)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve runtime manifest path %s: %v", args.RuntimeManifest, err)
		}
		args.RuntimeManifest = resolved
	}

	if args.Test != "" {
		resolved, err := rf.Rlocation(args.Test)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve test path %s: %v", args.Test, err)
		}
		args.Test = resolved
	}

	args.TestArgs = testArgs

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
	debugLog("Parsed args: docker-compose=%s, yaml=%s, runtime-manifest=%s, test=%s, test-args=%v, delay=%v",
		args.DockerCompose, args.Yaml, args.RuntimeManifest, args.Test, args.TestArgs, args.Delay)

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

	var upProcess *os.Process

	cleanupCalled := false
	cleanupFunc := func() {
		if cleanupCalled {
			return
		}
		cleanupCalled = true

		if upProcess != nil {
			debugLog("Sending SIGINT to docker-compose up process (PID: %d)", upProcess.Pid)
			if err := upProcess.Signal(os.Interrupt); err != nil {
				debugLog("Error sending SIGINT to docker-compose up: %v", err)
			} else {
				_, err := upProcess.Wait()
				if err != nil {
					debugLog("docker-compose up process exited with error: %v", err)
				} else {
					debugLog("docker-compose up process exited successfully")
				}
			}
		}

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

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		cleanupFunc()
		os.Exit(1)
	}()

	defer cleanupFunc()

	// Run loaders under the daemon-level lock + retag flow.
	var loaders []LoaderEntry
	if args.RuntimeManifest != "" {
		rm, err := loadRuntimeManifest(args.RuntimeManifest)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading runtime manifest: %v\n", err)
			os.Exit(1)
		}
		loaders = rm.Loaders
	}
	if len(loaders) > 0 {
		rf, err := runfiles.New()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to load runfiles: %v\n", err)
			os.Exit(1)
		}
		engine := engineBinary()
		debugLog("Running %d image loader(s) with engine=%s", len(loaders), engine)
		for i, entry := range loaders {
			loaderPath, err := rf.Rlocation(entry.LoaderRlocation)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error resolving loader %s: %v\n", entry.LoaderRlocation, err)
				os.Exit(1)
			}
			debugLog("Loader %d: %s", i+1, loaderPath)
			if err := loadAndRetag(engine, loaderPath, entry, rf.Env()); err != nil {
				fmt.Fprintf(os.Stderr, "Error running loader %s: %v\n", entry.LoaderRlocation, err)
				os.Exit(1)
			}
		}
	} else {
		debugLog("No image loaders specified")
	}

	debugLog("Spawning docker-compose up: %s -f %s up", args.DockerCompose, args.Yaml)
	upCmd := exec.Command(args.DockerCompose, "-f", args.Yaml, "up", "--timeout", "60", "--no-build")
	upCmd.Stdout = logFileHandle
	upCmd.Stderr = logFileHandle
	if err := upCmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting docker-compose up: %v\n", err)
		os.Exit(1)
	}
	upProcess = upCmd.Process
	debugLog("docker-compose up process started (PID: %d)", upProcess.Pid)

	if err := waitForContainers(args.DockerCompose, args.Yaml, upProcess, 10*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "Error: containers failed to start: %v\n", err)
		if upProcess != nil {
			upProcess.Signal(os.Interrupt)
			upProcess.Wait()
		}
		os.Exit(1)
	}
	debugLog("All containers are running and ready")

	if args.Delay > 0 {
		debugLog("Sleeping for delay: %v", args.Delay)
		time.Sleep(args.Delay)
		debugLog("Delay completed")
	}

	var testExitCode int
	if args.Test != "" {
		debugLog("Running test: %s with args: %v", args.Test, args.TestArgs)
		rf, err := runfiles.New()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to load runfiles: %v\n", err)
			os.Exit(1)
		}
		testCmd := exec.Command(args.Test, args.TestArgs...)
		testCmd.Stdout = os.Stdout
		testCmd.Stderr = os.Stderr
		testCmd.Env = append(os.Environ(), rf.Env()...)
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

	if upProcess != nil {
		debugLog("Sending SIGINT to docker-compose up process (PID: %d)", upProcess.Pid)
		if err := upProcess.Signal(os.Interrupt); err != nil {
			fmt.Fprintf(os.Stderr, "Error sending SIGINT to docker-compose up: %v\n", err)
		} else {
			debugLog("Waiting for docker-compose up process to finish")
			_, err := upProcess.Wait()
			if err != nil {
				debugLog("docker-compose up process exited with error: %v", err)
			} else {
				debugLog("docker-compose up process exited successfully")
			}
		}
		upProcess = nil
	}

	debugLog("Exiting with code: %d", testExitCode)
	os.Exit(testExitCode)
}
