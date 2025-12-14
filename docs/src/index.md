# rules_docker_compose

Bazel rules for running integration tests which use [Docker-Compose](https://docs.docker.com/compose/) to setup
networked infrastructure.

## Setup

```python
bazel_dep(name = "rules_docker_compose", version = "{version}")

docker_compose = use_extension("@rules_docker_compose//docker_compose:extensions.bzl", "docker_compose")
docker_compose.toolchain(
    name = "docker_compose_toolchains",
    version = "2.24.0",
)
use_repo(docker_compose, "docker_compose_toolchains")

register_toolchains(
    "@docker_compose_toolchains//:all",
)
```

## Overview

`rules_docker_compose` provides Bazel rules for integrating Docker-Compose into your build and test workflows. These rules enable you to:

- **Merge and validate docker-compose configurations** using the `docker_compose_yaml` rule, which combines multiple YAML files and ensures all referenced images have corresponding loader targets
- **Run integration tests** with the `docker_compose_test` rule, which automatically starts services, waits for them to be ready, runs your test binary, and cleans up containers
- **Verify image integrity** through automatically generated lock files that map image tags to content digests, ensuring the correct images are loaded at runtime

The rules integrate seamlessly with `rules_oci` and `rules_img`, allowing you to use container images built with Bazel in your docker-compose configurations. They handle the complete lifecycle of docker-compose services during testing, including image loading, service startup, health checking, and cleanup.

## Why not to use these rules

These rules are designed for integration testing scenarios where you need to test against real networked services. However, they come with trade-offs that may not be suitable for all use cases:

- **Requires network access**: Tests must be marked with `requires-network` and cannot run in fully sandboxed environments. This means tests may be slower and less reproducible than pure unit tests.

- **Docker dependency**: Tests require Docker (or compatible container runtime) to be available on the host machine. This adds an external dependency and may not work in all CI/CD environments.

- **Resource tracking limitations**: Bazel cannot track all resources created by docker-compose (spawned containers, networks, volumes). It's highly recommended to use resource tags to limit test execution and prevent resource exhaustion.

- **Port collisions**: Tests that bind to the same host ports cannot run in parallel. If multiple tests use the same port mappings (e.g., `"8080:80"`), they will conflict when Bazel attempts to run them concurrently. Use unique port mappings per test or ensure tests are tagged to run serially.

- **Platform-specific behavior**: Container behavior may vary across different platforms and Docker versions, potentially affecting test reproducibility.

Consider using these rules when you need to test interactions with real services, but prefer lighter-weight alternatives for fast, hermetic unit tests.
