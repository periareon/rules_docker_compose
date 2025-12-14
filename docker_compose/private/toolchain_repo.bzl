"""Docker-Compose toolchain repository configuration"""

load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_file")
load("//docker_compose/private:versions.bzl", _DOCKER_COMPOSE_VERSIONS = "DOCKER_COMPOSE_VERSIONS")

PLATFORM_TO_CONSTRAINTS = {
    "darwin-aarch64": ["@platforms//os:macos", "@platforms//cpu:aarch64"],
    "darwin-x86_64": ["@platforms//os:macos", "@platforms//cpu:x86_64"],
    "linux-aarch64": ["@platforms//os:linux", "@platforms//cpu:aarch64"],
    "linux-armv7": ["@platforms//os:linux", "@platforms//cpu:armv7"],
    "linux-ppc64le": ["@platforms//os:linux", "@platforms//cpu:ppc64le"],
    "linux-riscv64": ["@platforms//os:linux", "@platforms//cpu:riscv64"],
    "linux-s390x": ["@platforms//os:linux", "@platforms//cpu:i386"],
    "linux-x86_64": ["@platforms//os:linux", "@platforms//cpu:x86_64"],
    "windows-aarch64": ["@platforms//os:windows", "@platforms//cpu:aarch64"],
    "windows-x86_64": ["@platforms//os:windows", "@platforms//cpu:x86_64"],
}

DOCKER_COMPOSE_DEFAULT_VERSION = "5.0.0"

DOCKER_COMPOSE_VERSIONS = _DOCKER_COMPOSE_VERSIONS

_DOCKER_COMPOSE_TOOLCHAIN_BUILD_FILE_CONTENT = """\
load("@rules_docker_compose//docker_compose/private:toolchain.bzl", "docker_compose_toolchain")

docker_compose_toolchain(
    name = "toolchain",
    docker_compose = "{docker_compose}",
    version = "{version}",
    visibility = ["//visibility:public"],
)

alias(
    name = "{name}",
    actual = ":toolchain",
    visibility = ["//visibility:public"],
)
"""

def _docker_compose_toolchain_repository_impl(repository_ctx):
    docker_compose, _, _ = str(repository_ctx.attr.docker_compose).rpartition(":")
    repository_ctx.file("BUILD.bazel", _DOCKER_COMPOSE_TOOLCHAIN_BUILD_FILE_CONTENT.format(
        name = repository_ctx.original_name,
        docker_compose = docker_compose,
        version = repository_ctx.attr.version,
    ))
    repository_ctx.file("WORKSPACE.bazel", """workspace(name = "{}")""".format(
        repository_ctx.name,
    ))

docker_compose_toolchain_repository = repository_rule(
    doc = "A rule for defining docker-compose toolchains.",
    implementation = _docker_compose_toolchain_repository_impl,
    attrs = {
        "docker_compose": attr.label(
            doc = "The docker-compose binary.",
            allow_files = True,
            mandatory = True,
        ),
        "version": attr.string(
            doc = "The version of docker-compose.",
            mandatory = True,
        ),
    },
)

def docker_compose_tools_repository(*, name, version, platform, urls, integrity):
    """Download a version of Docker-Compose and instantiate targets for it.

    Args:
        name (str): The name of the repository to create.
        version (str): The version of Docker-Compose (e.g., "2.24.0").
        platform (str): The target platform (e.g., "linux-x86_64", "darwin-aarch64").
        urls (list): The URLs to fetch Docker-Compose from.
        integrity (str): The integrity checksum of the Docker-Compose archive.

    Returns:
        str: Return `name` for convenience.
    """

    # Determine binary path: Windows uses .exe, others don't
    if platform.startswith("windows-"):
        docker_compose_bin = "docker-compose.exe"
    else:
        docker_compose_bin = "docker-compose"

    bin_name = name + "_bin"
    http_file(
        name = bin_name,
        urls = urls,
        integrity = integrity,
        downloaded_file_path = docker_compose_bin,
        executable = True,
    )

    docker_compose_toolchain_repository(
        name = name,
        docker_compose = "@{}//file:{}".format(
            bin_name,
            docker_compose_bin,
        ),
        version = version,
    )

    return name

_BUILD_FILE_FOR_TOOLCHAIN_HUB_TEMPLATE = """
toolchain(
    name = "{name}",
    exec_compatible_with = {exec_constraint_sets_serialized},
    target_compatible_with = {target_constraint_sets_serialized},
    toolchain = "{toolchain}",
    toolchain_type = "@rules_docker_compose//docker_compose:toolchain_type",
    visibility = ["//visibility:public"],
)
"""

def _BUILD_for_toolchain_hub(
        toolchain_names,
        toolchain_labels,
        target_compatible_with,
        exec_compatible_with):
    return "\n".join([_BUILD_FILE_FOR_TOOLCHAIN_HUB_TEMPLATE.format(
        name = toolchain_name,
        exec_constraint_sets_serialized = json.encode(exec_compatible_with.get(toolchain_name, [])),
        target_constraint_sets_serialized = json.encode(target_compatible_with.get(toolchain_name, [])),
        toolchain = toolchain_labels[toolchain_name],
    ) for toolchain_name in toolchain_names])

def _docker_compose_toolchain_repository_hub_impl(repository_ctx):
    repository_ctx.file("WORKSPACE.bazel", """workspace(name = "{}")""".format(
        repository_ctx.name,
    ))

    repository_ctx.file("BUILD.bazel", _BUILD_for_toolchain_hub(
        toolchain_names = repository_ctx.attr.toolchain_names,
        toolchain_labels = repository_ctx.attr.toolchain_labels,
        target_compatible_with = repository_ctx.attr.target_compatible_with,
        exec_compatible_with = repository_ctx.attr.exec_compatible_with,
    ))

docker_compose_toolchain_repository_hub = repository_rule(
    doc = (
        "Generates a toolchain-bearing repository that declares a set of Docker-Compose toolchains from other " +
        "repositories. This exists to allow registering a set of toolchains in one go with the `:all` target."
    ),
    attrs = {
        "exec_compatible_with": attr.string_list_dict(
            doc = "A list of constraints for the execution platform for this toolchain, keyed by toolchain name.",
            mandatory = True,
        ),
        "target_compatible_with": attr.string_list_dict(
            doc = "A list of constraints for the target platform for this toolchain, keyed by toolchain name.",
            mandatory = True,
        ),
        "toolchain_labels": attr.string_dict(
            doc = "The name of the toolchain implementation target, keyed by toolchain name.",
            mandatory = True,
        ),
        "toolchain_names": attr.string_list(
            mandatory = True,
        ),
    },
    implementation = _docker_compose_toolchain_repository_hub_impl,
)
