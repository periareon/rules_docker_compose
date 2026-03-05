"""# Docker-Compose settings

Definitions for all `@rules_docker_compose//docker_compose` settings
"""

load(
    "@bazel_skylib//rules:common_settings.bzl",
    "string_flag",
)
load("//docker_compose/private:versions.bzl", "DOCKER_COMPOSE_VERSIONS")

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

def version(name = "version"):
    """The target version of docker-compose"""
    string_flag(
        name = name,
        values = DOCKER_COMPOSE_VERSIONS.keys(),
        build_setting_default = "5.1.0",
    )

    for ver in DOCKER_COMPOSE_VERSIONS.keys():
        native.config_setting(
            name = "{}_{}".format(name, ver),
            flag_values = {str(Label("//docker_compose/settings:{}".format(name))): ver},
        )
