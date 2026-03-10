package main

import (
	"bufio"
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

type Args struct {
	DockerCompose string
	Yaml          string
	Loaders       []string
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

	var loaders []string
	fs.Func("loader", "Image loader to run before docker-compose (can be specified multiple times)", func(value string) error {
		loaders = append(loaders, value)
		return nil
	})

	if err := fs.Parse(argLines); err != nil {
		return nil, fmt.Errorf("failed to parse args: %v", err)
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
	for i, loader := range loaders {
		resolved, err := rf.Rlocation(loader)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve loader path %s: %v", loader, err)
		}
		loaders[i] = resolved
	}
	args.Loaders = loaders

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
	debugLog("Parsed args: docker-compose=%s, yaml=%s, loaders=%v", args.DockerCompose, args.Yaml, args.Loaders)

	// Load images
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
	}

	// Build the docker-compose command: docker-compose -f <yaml> <user args...>
	execArgs := []string{args.DockerCompose, "-f", args.Yaml}
	execArgs = append(execArgs, os.Args[1:]...)

	debugLog("Exec: %v", execArgs)
	if err := syscall.Exec(args.DockerCompose, execArgs, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "Error exec'ing docker-compose: %v\n", err)
		os.Exit(1)
	}
}
