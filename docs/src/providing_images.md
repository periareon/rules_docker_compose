# Providing images

Every image a compose file references must be provided through a loader target
listed in `images`. The loader is where the image's content is pinned; the
compose file only names it. How to declare loaders, the tag contract and the
supported providers are documented on the
[`docker_compose_yaml` rule](./docker_compose_yaml.md#images).
This page covers the module-level setup that goes with it.

| Provider | Pull (MODULE.bazel) | Loader (BUILD.bazel) |
| --- | --- | --- |
| [rules_oci](https://github.com/bazel-contrib/rules_oci) | `oci.pull` | `oci_load` |
| [rules_img](https://github.com/bazel-contrib/rules_img) | `img.pull` | `image_load` |

## rules_oci

```python
bazel_dep(name = "rules_oci", version = "2.3.0")

oci = use_extension("@rules_oci//oci:extensions.bzl", "oci")
oci.pull(
    name = "nginx",
    image = "index.docker.io/library/nginx",
    digest = "sha256:fb01117203ff38c2f9af91db1a7409459182a37c87cced5cb442d1d8fcc66d19",
    platforms = ["linux/amd64", "linux/arm64/v8"],
)
use_repo(oci, "nginx", "nginx_linux_amd64", "nginx_linux_arm64_v8")
```

### Which rules_oci version

Up to and including rules_oci 2.3.0, the BUILD file that `oci.pull` generates
loads from `@aspect_bazel_lib`, which under bzlmod is only visible if your root
module also declares it
([bazel-contrib/rules_oci#575](https://github.com/bazel-contrib/rules_oci/issues/575)).
Releases from 2.3.1 on fix this, but at the time of writing none of them is on
the Bazel Central Registry. Until one is, point the module at a release tarball
directly rather than adding `aspect_bazel_lib` to your own dependencies:

```python
archive_override(
    module_name = "rules_oci",
    integrity = "sha256-JD6sh+xB1o0dnm0hiEpQ7CjgMfcCsLn83H+Xw/PVOJo=",
    strip_prefix = "rules_oci-2.3.3",
    urls = ["https://github.com/bazel-contrib/rules_oci/releases/download/v2.3.3/rules_oci-v2.3.3.tar.gz"],
)
```

Overrides only apply in the root module, so this has to live in each consuming
workspace; the examples in this repository do the same. Once a 2.3.1+ release
is on the registry, delete the override and bump the `bazel_dep` version.

## rules_img

```python
bazel_dep(name = "rules_img", version = "0.3.9")

img = use_extension("@rules_img//img:extensions.bzl", "images")
img.pull(
    name = "nginx",
    registry = "index.docker.io",
    repository = "library/nginx",
    digest = "sha256:fb01117203ff38c2f9af91db1a7409459182a37c87cced5cb442d1d8fcc66d19",
    # Required: the default `shallow` handling leaves base layers in the
    # registry, and the rules need every layer to build a loadable tarball.
    layer_handling = "eager",
)
use_repo(img, "rules_img_images.bzl")
```

## Examples

- [`examples/rust`](https://github.com/periareon/rules_docker_compose/tree/main/examples/rust)
  and [`examples/python`](https://github.com/periareon/rules_docker_compose/tree/main/examples/python)
  are complete rules_oci workspaces.
- [`docker_compose/private/tests/image_deps_img`](https://github.com/periareon/rules_docker_compose/tree/main/docker_compose/private/tests/image_deps_img)
  and [`simple_test/img`](https://github.com/periareon/rules_docker_compose/tree/main/docker_compose/private/tests/simple_test/img)
  show the rules_img equivalents.
- [`docker_compose/private/tests/verification`](https://github.com/periareon/rules_docker_compose/tree/main/docker_compose/private/tests/verification)
  demonstrates the guarantees: it points a declared tag at an impostor image,
  re-points the unique tag between verification and startup, and asserts that
  the first is ignored and the second fails the test.
