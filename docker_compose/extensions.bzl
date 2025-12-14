"""Docker-Compose bzlmod extensions"""

load(
    "//docker_compose/private:toolchain_repo.bzl",
    "DOCKER_COMPOSE_DEFAULT_VERSION",
    "DOCKER_COMPOSE_VERSIONS",
    "PLATFORM_TO_CONSTRAINTS",
    "docker_compose_toolchain_repository_hub",
    "docker_compose_tools_repository",
)

def _docker_compose_impl(module_ctx):
    reproducible = True

    # Process all modules, not just the root
    for mod in module_ctx.modules:
        for attrs in mod.tags.toolchain:
            if attrs.version not in DOCKER_COMPOSE_VERSIONS:
                fail("Docker-Compose toolchain hub `{}` was given unsupported version `{}`. Try: {}".format(
                    attrs.name,
                    attrs.version,
                    DOCKER_COMPOSE_VERSIONS.keys(),
                ))
            available = DOCKER_COMPOSE_VERSIONS[attrs.version]
            toolchain_names = []
            toolchain_labels = {}
            exec_compatible_with = {}
            for platform, artifact_info in available.items():
                tool_name = docker_compose_tools_repository(
                    name = "{}__{}".format(attrs.name, platform),
                    version = attrs.version,
                    platform = platform,
                    urls = [artifact_info["url"]],
                    integrity = artifact_info["integrity"],
                )

                toolchain_names.append(tool_name)
                toolchain_labels[tool_name] = "@{}".format(tool_name)
                exec_compatible_with[tool_name] = PLATFORM_TO_CONSTRAINTS[platform]

            docker_compose_toolchain_repository_hub(
                name = attrs.name,
                toolchain_labels = toolchain_labels,
                toolchain_names = toolchain_names,
                exec_compatible_with = exec_compatible_with,
                target_compatible_with = {},
            )

    return module_ctx.extension_metadata(
        reproducible = reproducible,
    )

_TOOLCHAIN_TAG = tag_class(
    doc = """\
An extension for defining a `docker_compose_toolchain` from a download archive.

An example of defining and registering toolchains:

```python
docker_compose = use_extension("//docker_compose:extensions.bzl", "docker_compose", dev_dependency = True)
docker_compose.toolchain(
    name = "docker_compose_toolchains",
    version = "2.24.0",
)
use_repo(docker_compose, "docker_compose_toolchains")

register_toolchains(
    "@docker_compose_toolchains//:all",
    dev_dependency = True,
)
```
""",
    attrs = {
        "name": attr.string(
            doc = "The name of the toolchain.",
            mandatory = True,
        ),
        "version": attr.string(
            doc = "The version of Docker-Compose to download.",
            default = DOCKER_COMPOSE_DEFAULT_VERSION,
        ),
    },
)

docker_compose = module_extension(
    doc = "Bzlmod extensions for Docker-Compose",
    implementation = _docker_compose_impl,
    tag_classes = {
        "toolchain": _TOOLCHAIN_TAG,
    },
)
