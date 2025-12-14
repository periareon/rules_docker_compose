"""# docker_compose_test"""

load(
    "//docker_compose/private:docker_compose.bzl",
    _docker_compose_test = "docker_compose_test",
)

docker_compose_test = _docker_compose_test
