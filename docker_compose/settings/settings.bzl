"""# Docker-Compose settings

Definitions for all `@rules_docker_compose//docker_compose` settings
"""

load(
    "@bazel_skylib//rules:common_settings.bzl",
    "string_flag",
)

def toolchain_default_digest_mode():
    """The default value of `docker_compose_toolchain.digest_mode`
    """
    string_flag(
        name = "toolchain_default_digest_mode",
        values = [
            "oci",
            "docker-legacy",
            "docker-containerd",
        ],
        build_setting_default = "docker-containerd",
    )
