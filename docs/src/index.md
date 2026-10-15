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

- **Merge and validate docker-compose configurations** using the `docker_compose_yaml` rule, which combines multiple YAML files and/or inline Starlark configuration, and ensures all referenced images have corresponding loader targets
- **Define compose configurations in Starlark** using the `docker_compose_config` helper, which provides structured parameters mirroring the Compose Specification (services, networks, volumes, configs, secrets)
- **Run docker-compose interactively** with the `docker_compose_binary` rule, which loads images and forwards arguments to docker-compose (e.g. `bazel run :compose -- up -d`)
- **Run integration tests** with the `docker_compose_test` rule, which loads images, starts services, waits for them to be running and healthy, runs your test binary, and cleans up containers on every exit path
- **Verify image integrity** by recording each image's manifest digest, config digest and layer DiffIDs at build time, tagging loaded images by digest rather than by name, and checking both the images and the running containers against those records.

The rules integrate seamlessly with `rules_oci` and `rules_img`, allowing you to use container images built with Bazel in your docker-compose configurations. They handle the complete lifecycle of docker-compose services during testing: image loading and verification, service startup with health checks, container verification, and cleanup.

## How it works

The rules split the work into three stages, and the boundary between them is
the point of the design: everything that reaches a network happens first,
everything that is checked happens at build time, and the test only ever
confirms what the build already decided.

1. **Fetching** happens in repository rules only. Images enter the build through
   `oci.pull` or `img.pull`, pinned by digest, and the compose binary comes from
   the toolchain. Nothing contacts a registry once repository fetching is done.
2. **Resolution** happens in build actions. `docker_compose_yaml` merges the
   compose files, matches every `image:` reference to a loader target, records
   the identities of each image that survive a `docker load` (the OCI manifest
   digest, the config digest and the layer DiffIDs), and writes a loadable
   tarball per image. It emits two yamls: the merged file with your original
   tags, which is a plain compose file you can share, and a test copy where each
   loader-backed image is rewritten to a content-derived tag with
   `pull_policy: never`.
3. **Execution** happens in the test. The runner loads each tarball if its
   content is not already present, tags the image by digest with its unique
   tag, and verifies the result. It then starts compose, waits for every service
   to be running and healthy, verifies that each container runs the image ID it
   just checked, runs your test binary, captures the compose logs and tears the
   project down on every exit path.

## Philosophy

- **Tags are names, not identities.** A tag in a shared daemon can be
  re-pointed by any other test, developer or CI job. The rules never locate an
  image by the tag declared on its loader. They locate it by digest and then
  prove, twice, that what runs is what was built.
- **The compose file stays a compose file.** You write `image: postgres:16` as
  you would anywhere, and Bazel decides what `postgres:16` is through a pull and
  a loader that claim that name. The merged yaml with original tags works with
  plain `docker compose` outside Bazel.
- **A test cannot depend on the network.** Every image must come through a
  loader. A reference without one is a build error, unless it is pinned to a
  digest and the target opts in with `allow_registry_images`, which is
  deliberately the uncomfortable path.
- **Sharing a daemon is safe by construction.** Tests that use the same built
  image share one content-addressed tag and one image, which is harmless
  because the tag names immutable content and images are never removed. Tests
  that use different images never share a name, because the digest is in the
  name. Projects are named after the target, so containers and networks stay
  separate.
- **The environment is inherited, not guessed.** `DOCKER_HOST`,
  `DOCKER_CONTEXT`, `DOCKER_CONFIG` and the TLS variables pass from the shell
  running `bazel test` into the test, so contexts, rootless daemons and Docker
  Desktop on Linux behave the same under Bazel as in a terminal. Bazel tests
  run with a fixed `PATH`; a `docker` binary outside `/usr/bin` or
  `/usr/local/bin` needs `--action_env=PATH` or
  `RULES_DOCKER_COMPOSE_ENGINE_BINARY`.

See [Providing images](./providing_images.md) for how to declare images with
`rules_oci` and `rules_img`, and the [examples](https://github.com/periareon/rules_docker_compose/tree/main/examples)
for complete workspaces.

## Why not to use these rules

These rules are designed for integration testing scenarios where you need to test against real networked services. However, they come with trade-offs that may not be suitable for all use cases:

- **Requires network access**: Tests carry the `requires-network` and `no-remote-exec` execution requirements and cannot run in fully sandboxed or remote environments. This means tests may be slower and less reproducible than pure unit tests.

- **Docker dependency**: Tests require a Docker daemon reachable from the host machine. The compose binary is downloaded hermetically, but the daemon cannot be. The Docker endpoint variables (`DOCKER_HOST`, `DOCKER_CONTEXT`, `DOCKER_CONFIG` and friends) are inherited from the invoking shell so contexts, rootless daemons and Docker Desktop work under `bazel test`.

- **Resource tracking limitations**: Bazel cannot track all resources created by docker-compose (spawned containers, networks, volumes). Tests tear their project down on every exit path, but loaded images are deliberately kept, since other tests may share them. It's highly recommended to use resource tags to limit test execution and prevent resource exhaustion.

- **Host ports are assigned at test time**: The host side of every published port is left to the daemon when a test runs, and the resulting address is passed to the test through `RULES_DOCKER_COMPOSE_HOST_<SERVICE>_<PORT>`. Tests therefore cannot collide on ports, but a test must read its service addresses from the environment rather than hard-code them. The declared ports still apply to `bazel run`.

- **No registry access at test time**: Every image must be provided through an `oci_load` or `image_load` target so that all fetching happens in repository rules. Registry images are only permitted when pinned to a digest and explicitly opted into.

- **Platform-specific behavior**: Container behavior may vary across different platforms and Docker versions, potentially affecting test reproducibility.

Consider using these rules when you need to test interactions with real services, but prefer lighter-weight alternatives for fast, hermetic unit tests.
