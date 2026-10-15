// The launcher backs `bazel run` of a docker_compose_binary. It makes every
// loader-backed image available under the tags the user declared, verifies
// each against the identity recorded at build time, then replaces itself with
// docker-compose so the user gets compose's own signal handling and terminal
// behaviour.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/periareon/rules_docker_compose/docker_compose/private/images"
)

func debugLog(format string, args ...interface{}) {
	images.Debugf(format, args...)
}

type Args struct {
	DockerCompose   string
	Yaml            string
	RuntimeManifest string
}

func parseArgs(rf *runfiles.Runfiles) (*Args, error) {
	argsFile := os.Getenv("RULES_DOCKER_COMPOSE_ARGS_FILE")
	if argsFile == "" {
		return nil, fmt.Errorf("RULES_DOCKER_COMPOSE_ARGS_FILE environment variable is not set")
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

	for name, field := range map[string]*string{
		"docker-compose":   &args.DockerCompose,
		"yaml":             &args.Yaml,
		"runtime-manifest": &args.RuntimeManifest,
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
	rf, err := runfiles.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to initialize runfiles: %v\n", err)
		os.Exit(1)
	}
	args, err := parseArgs(rf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing args: %v\n", err)
		os.Exit(1)
	}
	debugLog("Parsed args: %+v", args)

	if args.RuntimeManifest != "" {
		manifest, err := images.ReadRuntimeManifest(args.RuntimeManifest)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if len(manifest.Images) > 0 {
			engine := images.NewEngine()
			if err := engine.Preflight(); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			// The user-facing yaml references the declared tags, so those are
			// applied here on top of the unique ones. They are still pointed
			// at the image by digest, never copied from another name.
			for i := range manifest.Images {
				img := &manifest.Images[i]
				tarPath, err := rf.Rlocation(img.TarRlocation)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error: failed to resolve image tarball %s: %v\n", img.TarRlocation, err)
					os.Exit(1)
				}
				id, err := engine.Ensure(img, tarPath, img.OriginalTags...)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error: %v\n", err)
					os.Exit(1)
				}
				fmt.Fprintf(os.Stderr, "Verified image %s as %s (%s)\n", img.Label, id, strings.Join(img.OriginalTags, ", "))
			}
		}
	}

	execArgs := []string{args.DockerCompose, "-f", args.Yaml}
	execArgs = append(execArgs, os.Args[1:]...)
	debugLog("Exec: %v", execArgs)
	if err := syscall.Exec(args.DockerCompose, execArgs, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "Error exec'ing docker-compose: %v\n", err)
		os.Exit(1)
	}
}
