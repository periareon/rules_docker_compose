// The runner executes a docker_compose_test.
//
// Lifecycle:
//
//  1. Preflight the container engine.
//  2. For every loader-backed image: load its tarball if the content is not
//     already present, tag it by digest with its unique tag, and verify the
//     result against the identity recorded at build time.
//  3. Start compose detached from the test yaml, which references only unique
//     tags and pins pull_policy to never, and wait for every service to be
//     running (and healthy, when it declares a healthcheck).
//  4. Verify that each container runs the image ID verified in step 2.
//  5. Run the test binary.
//  6. Capture compose logs and tear everything down, on every exit path.
package main

import (
	"bufio"
	"crypto/rand"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/periareon/rules_docker_compose/docker_compose/private/images"
)

// EnvPreUpHook names a shell command the runner executes after images are
// verified and before compose starts. It exists so the rules' own tests can
// tamper with the daemon in that window and prove the container verification
// catches it. The unique tags are passed in RULES_DOCKER_COMPOSE_IMAGE_TAGS.
const EnvPreUpHook = "RULES_DOCKER_COMPOSE_PRE_UP_HOOK"

func debugLog(format string, args ...interface{}) {
	images.Debugf(format, args...)
}

type Args struct {
	DockerCompose   string
	Yaml            string
	RuntimeManifest string
	Test            string
	TestArgs        []string
	TestEnv         []string
	Delay           time.Duration
	StartupTimeout  time.Duration
}

func parseArgs(rf *runfiles.Runfiles) (*Args, error) {
	argsFile := os.Getenv("RULES_DOCKER_COMPOSE_TEST_ARGS_FILE")
	if argsFile == "" {
		return nil, fmt.Errorf("RULES_DOCKER_COMPOSE_TEST_ARGS_FILE environment variable is not set")
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

	var delayStr, startupStr string
	fs.StringVar(&delayStr, "delay", "", "Delay before running test (integer seconds or duration like '5s')")
	fs.StringVar(&startupStr, "startup-timeout", "60s", "How long to wait for services to be running and healthy")

	var testArgs, testEnv []string
	fs.Func("test-arg", "Test argument (can be specified multiple times)", func(value string) error {
		testArgs = append(testArgs, value)
		return nil
	})
	fs.Func("test-env", "KEY=VALUE set for the test process (can be specified multiple times)", func(value string) error {
		if !strings.Contains(value, "=") {
			return fmt.Errorf("expected KEY=VALUE, got %q", value)
		}
		testEnv = append(testEnv, value)
		return nil
	})

	if err := fs.Parse(argLines); err != nil {
		return nil, fmt.Errorf("failed to parse args: %v", err)
	}

	if args.Delay, err = parseSeconds(delayStr, 0); err != nil {
		return nil, fmt.Errorf("invalid delay value %s: %v", delayStr, err)
	}
	if args.StartupTimeout, err = parseSeconds(startupStr, 60*time.Second); err != nil {
		return nil, fmt.Errorf("invalid startup-timeout value %s: %v", startupStr, err)
	}

	for name, field := range map[string]*string{
		"docker-compose":   &args.DockerCompose,
		"yaml":             &args.Yaml,
		"runtime-manifest": &args.RuntimeManifest,
		"test":             &args.Test,
	} {
		if *field == "" {
			continue
		}
		resolved, err := rf.Rlocation(*field)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve %s path %s: %v", name, *field, err)
		}
		*field = resolved
	}
	args.TestArgs = testArgs
	args.TestEnv = testEnv

	if args.DockerCompose == "" {
		return nil, fmt.Errorf("missing required flag: -docker-compose")
	}
	if args.Yaml == "" {
		return nil, fmt.Errorf("missing required flag: -yaml")
	}
	if args.RuntimeManifest == "" {
		return nil, fmt.Errorf("missing required flag: -runtime-manifest")
	}
	return args, nil
}

// parseSeconds accepts a Go duration or a bare number of seconds.
func parseSeconds(value string, def time.Duration) (time.Duration, error) {
	if value == "" {
		return def, nil
	}
	if d, err := time.ParseDuration(value); err == nil {
		return d, nil
	}
	seconds, err := strconv.Atoi(value)
	if err != nil {
		return 0, err
	}
	return time.Duration(seconds) * time.Second, nil
}

// compose wraps docker-compose invocations against the test yaml.
type compose struct {
	binary  string
	yaml    string
	project string
	log     *os.File
}

func (c *compose) cmd(args ...string) *exec.Cmd {
	full := append([]string{"-f", c.yaml, "--project-name", c.project}, args...)
	debugLog("Running: %s %s", c.binary, strings.Join(full, " "))
	return exec.Command(c.binary, full...)
}

// run executes a compose command, sending its output to the log file and to
// stderr on failure.
func (c *compose) run(args ...string) error {
	cmd := c.cmd(args...)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	fmt.Fprintf(c.log, "$ docker-compose %s\n%s\n", strings.Join(args, " "), out.String())
	if err != nil {
		return fmt.Errorf("docker-compose %s failed: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return nil
}

func (c *compose) ps() ([]images.Container, error) {
	out, err := c.cmd("ps", "--all", "--format", "json").Output()
	if err != nil {
		return nil, err
	}
	return images.ParseComposePS(out)
}

// images returns the image ID each container of the project was created from.
func (c *compose) images() (map[string]string, error) {
	out, err := c.cmd("images", "--format", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("docker-compose images failed: %v", err)
	}
	entries, err := images.ParseComposeImages(out)
	if err != nil {
		return nil, err
	}
	byContainer := map[string]string{}
	for _, e := range entries {
		byContainer[e.ContainerName] = e.ID
	}
	return byContainer, nil
}

// waitForServices polls until every container of the project is running and
// healthy. A container that exits with a non-zero code fails the wait
// immediately rather than after the timeout; one that exits cleanly is treated
// as a completed one-shot service. Every service backed by a loader must have
// a container before the wait can succeed.
func (c *compose) waitForServices(required []string, timeout time.Duration) ([]images.Container, error) {
	deadline := time.Now().Add(timeout)
	var last []images.Container
	for {
		containers, err := c.ps()
		if err != nil {
			debugLog("compose ps failed, retrying: %v", err)
		} else {
			last = containers
			ready, failure := assessContainers(containers, required)
			if failure != nil {
				return containers, failure
			}
			if ready {
				return containers, nil
			}
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("timed out after %s waiting for services to be running and healthy:\n%s",
				timeout, describeContainers(last))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// assessContainers reports whether the project is ready, or a failure that
// cannot resolve itself by waiting.
func assessContainers(containers []images.Container, required []string) (bool, error) {
	if len(containers) == 0 {
		return false, nil
	}
	present := map[string]bool{}
	for _, ctr := range containers {
		present[ctr.Service] = true
		switch ctr.State {
		case "running":
			if ctr.Health != "" && ctr.Health != "healthy" {
				if ctr.Health == "unhealthy" {
					return false, fmt.Errorf("service %s is unhealthy:\n%s", ctr.Service, describeContainers(containers))
				}
				return false, nil
			}
		case "exited":
			if ctr.ExitCode != 0 {
				return false, fmt.Errorf("service %s exited with code %d:\n%s", ctr.Service, ctr.ExitCode, describeContainers(containers))
			}
		case "dead", "removing":
			return false, fmt.Errorf("service %s is %s:\n%s", ctr.Service, ctr.State, describeContainers(containers))
		default:
			return false, nil
		}
	}
	for _, service := range required {
		if !present[service] {
			return false, nil
		}
	}
	return true, nil
}

func describeContainers(containers []images.Container) string {
	if len(containers) == 0 {
		return "  (no containers)"
	}
	var lines []string
	for _, ctr := range containers {
		health := ""
		if ctr.Health != "" {
			health = " health=" + ctr.Health
		}
		lines = append(lines, fmt.Sprintf("  %s: state=%s%s exit=%d image=%s", ctr.Service, ctr.State, health, ctr.ExitCode, ctr.Image))
	}
	return strings.Join(lines, "\n")
}

// verifyContainers checks that every loader-backed service runs the image ID
// that was verified after loading, using the IDs compose reports per
// container. This holds even if something re-pointed a tag between the load
// and compose up.
func verifyContainers(containerImages map[string]string, containers []images.Container, services map[string]string, verifiedIDs map[string]string) error {
	var problems []string
	seen := map[string]bool{}
	for _, ctr := range containers {
		unique, backed := services[ctr.Service]
		if !backed {
			continue
		}
		seen[ctr.Service] = true
		want := verifiedIDs[unique]
		got, ok := containerImages[ctr.Name]
		if !ok {
			problems = append(problems, fmt.Sprintf("service %s: compose reported no image for container %s", ctr.Service, ctr.Name))
			continue
		}
		if got != want {
			problems = append(problems, fmt.Sprintf(
				"service %s runs image %s but the verified image for %s is %s", ctr.Service, got, unique, want))
		}
	}
	for service := range services {
		if !seen[service] {
			problems = append(problems, fmt.Sprintf("service %s has no container", service))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("containers do not run the images this test was built with:\n  %s", strings.Join(problems, "\n  "))
}

// discoverPorts reads the host address of every published container port
// from what compose reported and returns the environment variables describing
// them.
func discoverPorts(containers []images.Container, ports []images.PortMapping) (map[string]string, error) {
	env := map[string]string{}
	if len(ports) == 0 {
		return env, nil
	}
	byService := map[string]images.Container{}
	for _, ctr := range containers {
		byService[ctr.Service] = ctr
	}
	for _, p := range ports {
		ctr, ok := byService[p.Service]
		if !ok {
			return nil, fmt.Errorf("service %s publishes port %d but has no container", p.Service, p.Target)
		}
		host, port, err := images.PublishedPort(ctr.Publishers, p.Target, p.Protocol)
		if err != nil {
			return nil, fmt.Errorf("service %s: %v", p.Service, err)
		}
		env[images.PortEnvName(p.Service, p.Target)] = port
		env[images.HostEnvName(p.Service, p.Target)] = host + ":" + port
	}
	return env, nil
}

// uniqueProjectName appends a random suffix to the build-time project name.
// Compose project names are lower-case alphanumerics, `-` and `_`.
func uniqueProjectName(base string) string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s-%d", base, os.Getpid())
	}
	return fmt.Sprintf("%s-%x", base, b)
}

func runHook(command string, tags []string) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", command)
	} else {
		cmd = exec.Command("/bin/sh", "-c", command)
	}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "RULES_DOCKER_COMPOSE_IMAGE_TAGS="+strings.Join(tags, " "))
	return cmd.Run()
}

func main() {
	os.Exit(run())
}

func run() int {
	debugLog("Starting runner")
	rf, err := runfiles.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to initialize runfiles: %v\n", err)
		return 1
	}
	args, err := parseArgs(rf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing args: %v\n", err)
		return 1
	}
	debugLog("Parsed args: %+v", args)

	outputDir := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR")
	if outputDir == "" {
		outputDir = os.TempDir()
	}
	logFile, err := os.Create(filepath.Join(outputDir, "docker_compose.log"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating log file: %v\n", err)
		return 1
	}
	defer logFile.Close()
	debugLog("Compose log: %s", logFile.Name())

	engine := images.NewEngine()
	if err := engine.Preflight(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	manifest, err := images.ReadRuntimeManifest(args.RuntimeManifest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	if len(manifest.RegistryImages) > 0 && !manifest.AllowRegistryImages {
		fmt.Fprintf(os.Stderr,
			"Error: this test would pull images from a registry at runtime, which is not permitted by default:\n  %s\n"+
				"Provide them through `oci_load`/`image_load` targets, or pin them to a digest and set "+
				"`allow_registry_images = True` on the docker_compose_yaml target.\n",
			strings.Join(manifest.RegistryImages, "\n  "))
		return 1
	}

	// Make every image available under its unique tags and remember the ID
	// the daemon settled on, keyed by unique tag.
	verifiedIDs := map[string]string{}
	var allTags []string
	for i := range manifest.Images {
		img := &manifest.Images[i]
		tarPath, err := rf.Rlocation(img.TarRlocation)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: failed to resolve image tarball %s: %v\n", img.TarRlocation, err)
			return 1
		}
		id, err := engine.Ensure(img, tarPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		for _, tag := range img.UniqueTags {
			verifiedIDs[tag] = id
		}
		allTags = append(allTags, img.UniqueTags...)
		fmt.Fprintf(os.Stderr, "Verified image %s as %s\n", img.Label, id)
	}

	if hook := os.Getenv(EnvPreUpHook); hook != "" {
		debugLog("Running pre-up hook: %s", hook)
		if err := runHook(hook, allTags); err != nil {
			fmt.Fprintf(os.Stderr, "Error: pre-up hook failed: %v\n", err)
			return 1
		}
	}

	// A per-run project name keeps this run's containers and networks apart
	// from any other run of the same target, concurrent or crashed.
	project := uniqueProjectName(manifest.Project)
	fmt.Fprintf(os.Stderr, "Compose project: %s\n", project)
	c := &compose{binary: args.DockerCompose, yaml: args.Yaml, project: project, log: logFile}

	// Teardown runs exactly once, on normal exit and on signals alike.
	var once sync.Once
	teardown := func() {
		once.Do(func() {
			if err := c.run("logs", "--no-color", "--timestamps"); err != nil {
				debugLog("Failed to capture compose logs: %v", err)
			}
			if err := c.run("down", "--remove-orphans", "--volumes", "--timeout", "30"); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
			} else {
				debugLog("docker-compose down completed")
			}
		})
	}
	defer teardown()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		fmt.Fprintf(os.Stderr, "Received %s, tearing down\n", sig)
		teardown()
		os.Exit(1)
	}()

	if err := c.run("up", "--detach", "--no-build"); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	var required []string
	for service := range manifest.Services {
		required = append(required, service)
	}
	containers, err := c.waitForServices(required, args.StartupTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: services failed to start: %v\n", err)
		return 1
	}
	debugLog("All services are running:\n%s", describeContainers(containers))

	containerImages, err := c.images()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	if err := verifyContainers(containerImages, containers, manifest.Services, verifiedIDs); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "Verified %d container(s) against the built images\n", len(manifest.Services))

	// Tell the test where its services ended up. Host ports are assigned by
	// the daemon unless the target opted out, so they are only knowable now.
	portEnv, err := discoverPorts(containers, manifest.Ports)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	for _, line := range images.FormatEnv(portEnv) {
		if strings.HasPrefix(line, images.EnvPrefix+"HOST_") {
			fmt.Fprintf(os.Stderr, "%s\n", line)
		}
	}

	if args.Delay > 0 {
		debugLog("Sleeping for delay: %v", args.Delay)
		time.Sleep(args.Delay)
	}

	if args.Test == "" {
		debugLog("No test specified")
		return 0
	}

	// The port variables are visible to `${NAME}` references in the test's
	// arguments and declared environment, and to the test itself.
	testArgs := make([]string, 0, len(args.TestArgs))
	for _, arg := range args.TestArgs {
		testArgs = append(testArgs, images.ExpandRefs(arg, portEnv))
	}
	testEnv := make([]string, 0, len(args.TestEnv))
	for _, pair := range args.TestEnv {
		key, value, _ := strings.Cut(pair, "=")
		testEnv = append(testEnv, key+"="+images.ExpandRefs(value, portEnv))
	}

	debugLog("Running test: %s with args: %v", args.Test, testArgs)
	testCmd := exec.Command(args.Test, testArgs...)
	testCmd.Stdout = os.Stdout
	testCmd.Stderr = os.Stderr
	// Later entries win in os/exec, so the test target's own environment
	// overrides anything inherited.
	testCmd.Env = slices.Concat(os.Environ(), rf.Env(), images.FormatEnv(portEnv), testEnv)
	if err := testCmd.Run(); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			debugLog("Test exited with code: %d", exitError.ExitCode())
			return exitError.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "Error running test: %v\n", err)
		return 1
	}
	debugLog("Test completed successfully")
	return 0
}
