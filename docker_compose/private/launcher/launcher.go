package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

func debugLog(format string, args ...interface{}) {
	if os.Getenv("RULES_DOCKER_COMPOSE_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[DEBUG] "+format+"\n", args...)
	}
}

// TagRewrite mirrors the merger's sidecar JSON entry. The launcher ignores
// the Unique field — interactive `bazel run :compose -- ...` deliberately
// leaves the user-facing tags alone so users see the names they declared.
type TagRewrite struct {
	Original string `json:"original"`
	Unique   string `json:"unique"`
}

type LoaderEntry struct {
	LoaderRlocation string       `json:"loader_rlocationpath"`
	Tags            []TagRewrite `json:"tags"`
}

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

type Args struct {
	DockerCompose   string
	Yaml            string
	RuntimeManifest string
}

func parseArgs() (*Args, error) {
	argsFile := os.Getenv("RULES_DOCKER_COMPOSE_ARGS_FILE")
	if argsFile == "" {
		return nil, fmt.Errorf("RULES_DOCKER_COMPOSE_ARGS_FILE environment variable is not set")
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

	fs := flag.NewFlagSet("launcher", flag.ContinueOnError)
	args := &Args{}

	fs.StringVar(&args.DockerCompose, "docker-compose", "", "Path to docker-compose binary")
	fs.StringVar(&args.Yaml, "yaml", "", "Path to docker-compose yaml file")
	fs.StringVar(&args.RuntimeManifest, "runtime-manifest", "", "Path to runtime manifest JSON")

	if err := fs.Parse(argLines); err != nil {
		return nil, fmt.Errorf("failed to parse args: %v", err)
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

	if args.DockerCompose == "" {
		return nil, fmt.Errorf("missing required flag: -docker-compose")
	}
	if args.Yaml == "" {
		return nil, fmt.Errorf("missing required flag: -yaml")
	}

	return args, nil
}

func main() {
	debugLog("Starting launcher")
	args, err := parseArgs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing args: %v\n", err)
		os.Exit(1)
	}
	debugLog("Parsed args: docker-compose=%s, yaml=%s, runtime-manifest=%s",
		args.DockerCompose, args.Yaml, args.RuntimeManifest)

	// Run loaders so the user-declared original tags resolve in the engine.
	// No retag / no daemon lock: bazel run is interactive, and the YAML
	// references original tags so users see familiar names in `docker ps`.
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
		debugLog("Running %d image loader(s)", len(loaders))
		for i, entry := range loaders {
			loaderPath, err := rf.Rlocation(entry.LoaderRlocation)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error resolving loader %s: %v\n", entry.LoaderRlocation, err)
				os.Exit(1)
			}
			debugLog("Loader %d: %s", i+1, loaderPath)
			loaderCmd := exec.Command(loaderPath)
			loaderCmd.Stdout = os.Stderr
			loaderCmd.Stderr = os.Stderr
			loaderCmd.Env = append(os.Environ(), rf.Env()...)
			if err := loaderCmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "Error running loader %s: %v\n", entry.LoaderRlocation, err)
				os.Exit(1)
			}
			debugLog("Loader %d completed", i+1)
		}
	} else {
		debugLog("No image loaders specified")
	}

	execArgs := []string{args.DockerCompose, "-f", args.Yaml}
	execArgs = append(execArgs, os.Args[1:]...)

	debugLog("Exec: %v", execArgs)
	if err := syscall.Exec(args.DockerCompose, execArgs, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "Error exec'ing docker-compose: %v\n", err)
		os.Exit(1)
	}
}
