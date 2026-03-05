"""Docker-Compose bzlmod extensions"""

load(
    "//docker_compose/private:toolchain_repo.bzl",
    "DOCKER_COMPOSE_VERSIONS",
    "PLATFORM_TO_CONSTRAINTS",
    "docker_compose_toolchain_repository_hub",
    "docker_compose_tools_repository",
)

def _docker_compose_impl(module_ctx):
    reproducible = True

    toolchain_names = []
    toolchain_labels = {}
    exec_compatible_with = {}
    target_settings = {}
    for version, info in DOCKER_COMPOSE_VERSIONS.items():
        for platform, artifact_info in info.items():
            tool_name = docker_compose_tools_repository(
                name = "docker_compose__{}_{}".format(version, platform),
                version = version,
                platform = platform,
                urls = [artifact_info["url"]],
                integrity = artifact_info["integrity"],
            )

            toolchain_names.append(tool_name)
            toolchain_labels[tool_name] = "@{}".format(tool_name)
            exec_compatible_with[tool_name] = PLATFORM_TO_CONSTRAINTS[platform]
            target_settings[tool_name] = ["@rules_docker_compose//docker_compose/settings:version_{}".format(version)]

    docker_compose_toolchain_repository_hub(
        name = "docker_compose_toolchains",
        toolchain_labels = toolchain_labels,
        toolchain_names = toolchain_names,
        exec_compatible_with = exec_compatible_with,
        target_compatible_with = {},
        target_settings = target_settings,
    )

    return module_ctx.extension_metadata(
        reproducible = reproducible,
    )

docker_compose = module_extension(
    doc = "Bzlmod extensions for Docker-Compose",
    implementation = _docker_compose_impl,
)
