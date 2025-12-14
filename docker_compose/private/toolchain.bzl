"""Docker-Compose toolchain"""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")

TOOLCHAIN_TYPE = str(Label("//docker_compose:toolchain_type"))

def _rlocationpath(file, workspace_name):
    if file.short_path.startswith("../"):
        return file.short_path[len("../"):]
    return "{}/{}".format(workspace_name, file.short_path)

def _docker_compose_toolchain_impl(ctx):
    all_files = []
    if DefaultInfo in ctx.attr.docker_compose:
        all_files.extend([
            ctx.attr.docker_compose[DefaultInfo].files,
            ctx.attr.docker_compose[DefaultInfo].default_runfiles.files,
        ])

    digest_mode = ctx.attr.digest_mode
    if not digest_mode:
        digest_mode = ctx.attr._default_digest_mode[BuildSettingInfo].value

    make_variable_info = platform_common.TemplateVariableInfo({
        "DOCKER_COMPOSE": ctx.executable.docker_compose.path,
        "DOCKER_COMPOSE_RLOCATIONPATH": _rlocationpath(ctx.executable.docker_compose, ctx.workspace_name),
    })

    return [
        platform_common.ToolchainInfo(
            docker_compose = ctx.executable.docker_compose,
            digest_mode = digest_mode,
            all_files = depset(transitive = all_files),
            make_variable_info = make_variable_info,
        ),
    ]

docker_compose_toolchain = rule(
    doc = """\
A toolchain for providing Docker-Compose to Bazel rules.

The `digest_mode` attribute controls how image digests are computed for lock files:

| Mode | Description |
|------|-------------|
| `oci` | Uses the OCI manifest digest directly from the image's index.json. Use this when images are pushed to an OCI-compliant registry. |
| `docker-legacy` | Uses the config blob digest from the OCI manifest. This is the image ID that Docker reports after `docker load` when using legacy storage (without containerd). Use this for Linux CI runners like GitHub Actions. |
| `docker-containerd` | Converts the OCI manifest to Docker V2 Schema 2 format and computes the manifest digest. This is the image ID that Docker reports after `docker load` when using containerd storage. Use this for Docker Desktop or Docker Engine with containerd enabled. |

See the [OCI Image Manifest spec](https://github.com/opencontainers/image-spec/blob/main/manifest.md)
and the [Docker V2 Schema 2 spec](https://docs.docker.com/registry/spec/manifest-v2-2/).

Example:

```python
load("@rules_docker_compose//docker_compose:docker_compose_toolchain.bzl", "docker_compose_toolchain")

filegroup(
    name = "docker_compose_bin",
    srcs = ["docker_compose/docker_compose.exe"],
    # Note that additional runfiles associated with a hermetic archive
    # of docker_compose should be associated with the target passed to the
    # `docker_compose` attribute.
    data = glob(["docker_compose/**"]),
)

docker_compose_toolchain(
    name = "docker_compose_toolchain",
    docker_compose = ":docker_compose_bin",
    visibility = ["//visibility:public"],
)
```

For users looking to use a system install of Docker-Compose, a shell/batch script
should be added that points to the system install.

Example or non-hermetic toolchain:

`docker_compose.sh`
```bash
#!/usr/bin/env bash
set -euo pipefail
exec /usr/bin/docker_compose $@
```

`docker_compose.bat`
```batch
@ECHO OFF
C:\\Program Files\\Docker\\Docker\\resources\\docker-compose.exe %*
set EXITCODE=%ERRORLEVEL%
exit /b %EXITCODE%
```

```python
load("@rules_docker_compose//docker_compose:docker_compose_toolchain.bzl", "docker_compose_toolchain")

filegroup(
    name = "docker_compose_bin",
    srcs = select({
        "@platforms//os:windows": ["docker_compose.bat"],
        "//conditions:default": ["docker_compose.sh"],
    }),
)

docker_compose_toolchain(
    name = "docker_compose_toolchain",
    docker_compose = ":docker_compose_bin",
    visibility = ["//visibility:public"],
)
```
""",
    implementation = _docker_compose_toolchain_impl,
    attrs = {
        "digest_mode": attr.string(
            doc = "Controls how image digests are computed for lock files. See the rule documentation for details on available modes. Defaults to the value of `--@rules_docker_compose//docker_compose/settings:toolchain_default_digest_mode`.",
            values = [
                "oci",
                "docker-legacy",
                "docker-containerd",
            ],
        ),
        "docker_compose": attr.label(
            doc = "The docker-compose executable.",
            cfg = "exec",
            executable = True,
            mandatory = True,
            allow_files = True,
        ),
        "version": attr.string(
            doc = "The version of docker-compose.",
            mandatory = True,
        ),
        "_default_digest_mode": attr.label(
            default = Label("//docker_compose/settings:toolchain_default_digest_mode"),
        ),
    },
)

def _current_docker_compose_toolchain_impl(ctx):
    toolchain = ctx.toolchains[TOOLCHAIN_TYPE]

    return [
        DefaultInfo(
            files = toolchain.all_files,
            runfiles = ctx.runfiles(transitive_files = toolchain.all_files),
        ),
        toolchain,
        toolchain.make_variable_info,
    ]

current_docker_compose_toolchain = rule(
    doc = "Access the `docker_compose_toolchain` for the current configuration.",
    implementation = _current_docker_compose_toolchain_impl,
    toolchains = [TOOLCHAIN_TYPE],
)
