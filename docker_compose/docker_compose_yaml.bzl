"""# docker_compose_yaml"""

load(
    "//docker_compose/private:docker_compose.bzl",
    _docker_compose_yaml = "docker_compose_yaml",
)

docker_compose_yaml = _docker_compose_yaml
