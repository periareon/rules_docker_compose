"""# Docker Compose"""

load(
    ":docker_compose_test.bzl",
    _docker_compose_test = "docker_compose_test",
)
load(
    ":docker_compose_toolchain.bzl",
    _docker_compose_toolchain = "docker_compose_toolchain",
)
load(
    ":docker_compose_yaml.bzl",
    _docker_compose_yaml = "docker_compose_yaml",
)

docker_compose_toolchain = _docker_compose_toolchain
docker_compose_test = _docker_compose_test
docker_compose_yaml = _docker_compose_yaml
