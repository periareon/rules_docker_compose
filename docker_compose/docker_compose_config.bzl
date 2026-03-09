"""# docker_compose_config"""

load(
    "//docker_compose/private:docker_compose.bzl",
    _docker_compose_config = "docker_compose_config",
)

docker_compose_config = _docker_compose_config
