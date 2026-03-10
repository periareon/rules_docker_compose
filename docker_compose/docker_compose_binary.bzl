"""# docker_compose_binary"""

load(
    "//docker_compose/private:docker_compose.bzl",
    _docker_compose_binary = "docker_compose_binary",
)

docker_compose_binary = _docker_compose_binary
