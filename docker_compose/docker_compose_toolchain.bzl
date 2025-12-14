"""# docker_compose_toolchain"""

load(
    "//docker_compose/private:toolchain.bzl",
    _docker_compose_toolchain = "docker_compose_toolchain",
)

docker_compose_toolchain = _docker_compose_toolchain
