"""Docker-Compose rules"""

load(":image_utils.bzl", "ImageLoadRepositoryInfo", "image_load_repository_aspect")
load(":toolchain.bzl", "TOOLCHAIN_TYPE")

def docker_compose_config(
        services = {},
        networks = {},
        volumes = {},
        configs = {},
        secrets = {}):
    """Encode a docker-compose configuration as a JSON string.

    Parameters mirror the top-level keys of the
    [Compose Specification](https://docs.docker.com/reference/compose-file/).
    The returned string is suitable for passing to the `config` attribute
    of `docker_compose_yaml`.

    Args:
        services: A dict mapping service names to their configuration dicts.
            Each value follows the Compose `services.<name>` schema (image,
            ports, environment, volumes, depends_on, etc.).
        networks: A dict mapping network names to their configuration dicts.
            Follows the Compose `networks.<name>` schema.
        volumes: A dict mapping volume names to their configuration dicts.
            Follows the Compose `volumes.<name>` schema.
        configs: A dict mapping config names to their configuration dicts.
            Follows the Compose `configs.<name>` schema.
        secrets: A dict mapping secret names to their configuration dicts.
            Follows the Compose `secrets.<name>` schema.

    Returns:
        A JSON-encoded string representing the docker-compose configuration.
    """
    compose = {}
    if services:
        compose["services"] = services
    if networks:
        compose["networks"] = networks
    if volumes:
        compose["volumes"] = volumes
    if configs:
        compose["configs"] = configs
    if secrets:
        compose["secrets"] = secrets
    return json.encode(compose)

DockerComposeYamlInfo = provider(
    doc = """\
Provides information about a resolved docker-compose YAML configuration.

This provider is returned by `docker_compose_yaml` and contains the merged
docker-compose configuration along with metadata about images and dependencies.
It can be consumed by `docker_compose_test` or other rules that need to work
with docker-compose configurations.
""",
    fields = {
        "data": "depset[File]: Additional data files referenced by the configuration (env_file, configs, secrets, bind-mount sources).",
        "image_tars": "depset[File]: A loadable tarball per loader-backed image, consumed by `docker_compose_test` and `docker_compose_binary`.",
        "images": "depset[File]: The image loader executables for each image referenced in the yaml.",
        "runtime_manifest": "File: A JSON manifest mapping each loader to its (original_tag, unique_tag) rewrites, consumed by the runner and launcher.",
        "yaml": "File: The merged docker-compose YAML file with original image tags preserved. This is the user-facing artifact, suitable for sharing outside Bazel.",
        "yaml_deps": "depset[File]: All files which contributed to the merged yaml (input yamls and image manifests).",
        "yaml_rewritten": "File: The merged docker-compose YAML with each loader-backed image rewritten to a content-derived unique tag. Used internally by `docker_compose_test` to isolate parallel test runs on a shared engine.",
    },
)

# Shared documentation for the `images` attribute of every rule.
_IMAGES_ATTR_DOC = (
    "Loader targets (`oci_load` from rules_oci or `image_load` from rules_img) providing the " +
    "container images the compose configuration references. Every `services.*.image` value must " +
    "equal a tag declared on one of these targets. See the rule documentation for details."
)

def _collect_yamls(*, yamls):
    direct = []
    transitive = []
    for target in yamls:
        if DockerComposeYamlInfo in target:
            transitive.append(target[DockerComposeYamlInfo].yaml_deps)
            continue
        if DefaultInfo in target:
            direct.append(target[DefaultInfo].files)
            continue
        fail("Unknown: {}".format(target))

    return depset(transitive = direct + transitive)

def _create_image_manifests(*, ctx, images, output_fmt):
    """Declare the per-image description and loadable tarball for each loader target.

    Args:
        ctx (ctx): The rule's context object.
        images (list): Loader targets carrying `ImageLoadRepositoryInfo`.
        output_fmt (str): A path template with `{name}` and `{image}` placeholders, ending in `.json`.

    Returns:
        tuple: The manifest files, the action inputs they reference, and the tarball outputs.
    """
    image_inputs = []
    single_image_manifests = []
    image_tars = []
    for image in images:
        info = image[ImageLoadRepositoryInfo]
        image_inputs.extend(info.tag_files)

        oci_layout_dir = None
        tarball = None
        if info.oci_layout:
            image_inputs.append(info.oci_layout)
            oci_layout_dir = info.oci_layout.path
        elif info.tarball:
            image_inputs.append(info.tarball)
            tarball = info.tarball.path
        else:
            fail("Unable to determine image content for {}".format(image.label))

        basename = output_fmt.replace(
            "{name}",
            ctx.label.name,
        ).replace(
            "{image}",
            str(image.label).strip("@").replace("/", "_").replace(":", "_"),
        )
        if not basename.endswith(".json"):
            fail("output_fmt must end in .json, got {}".format(output_fmt))
        single_image_manifest = ctx.actions.declare_file(basename)
        image_tar = ctx.actions.declare_file(basename[:-len(".json")] + ".tar")

        ctx.actions.write(
            output = single_image_manifest,
            content = json.encode_indent(
                struct(
                    label = str(image.label),
                    tag_file_paths = [f.path for f in info.tag_files],
                    oci_layout_dir = oci_layout_dir,
                    tarball = tarball,
                    tar_out = image_tar.path,
                    tar_rlocationpath = _rlocationpath(image_tar, ctx.workspace_name),
                ),
                indent = " " * 4,
            ),
        )
        single_image_manifests.append(single_image_manifest)
        image_tars.append(image_tar)

    return single_image_manifests, image_inputs, image_tars

def _docker_compose_yaml_action(
        *,
        ctx,
        yamls,
        output,
        out_rewritten,
        out_runtime_manifest,
        toolchain,
        image_manifests = [],
        image_inputs = [],
        image_tars = [],
        data_inputs = [],
        config_file = None,
        interpolate = True,
        interpolation_env = {},
        allow_registry_images = False,
        require_loaders = False,
        ephemeral_ports = True):
    # Sanitize label for project name: replace @ with AT, + with -, / with _, : with __
    project_name = str(ctx.label).replace("@", "0").replace("+", "-").replace("/", "_").replace(":", "__")

    args = ctx.actions.args()
    args.add("-docker-compose", toolchain.docker_compose)
    args.add("-output", output)
    args.add("-output-rewritten", out_rewritten)
    args.add("-project-name", project_name)
    args.add("-output-runtime", out_runtime_manifest)
    for file in yamls:
        args.add("-file", file)

    if not interpolate:
        args.add("-no-interpolate")

    if allow_registry_images:
        args.add("-allow-registry-images")

    if require_loaders:
        args.add("-require-loaders")

    if not ephemeral_ports:
        args.add("-keep-published-ports")

    # Sorted so the action key doesn't depend on declaration order.
    for key in sorted(interpolation_env):
        args.add("-env", "{}={}".format(key, interpolation_env[key]))

    for manifest in image_manifests:
        # Add the single_image_manifest to args
        args.add("-image_manifest", manifest)

    extra_inputs = []
    if data_inputs:
        data_manifest = {}
        for f in data_inputs:
            f_rlocation = _rlocationpath(f, ctx.workspace_name)
            data_manifest[f.path] = f_rlocation

            if config_file:
                bin_prefix = config_file.path[:-len(config_file.short_path)]
                config_relative = bin_prefix + f.short_path
                if config_relative != f.path:
                    data_manifest[config_relative] = f_rlocation

        data_manifest_file = ctx.actions.declare_file("{}_data_manifest.json".format(ctx.label.name))
        ctx.actions.write(
            output = data_manifest_file,
            content = json.encode(data_manifest),
        )
        args.add("-data-manifest", data_manifest_file)
        args.add("-output-rlocationpath", _rlocationpath(output, ctx.workspace_name))
        extra_inputs.append(data_manifest_file)

    ctx.actions.run(
        mnemonic = "DockerComposeYamlConfig",
        executable = ctx.executable._merger,
        arguments = [args],
        inputs = depset(yamls + image_inputs + image_manifests + data_inputs + extra_inputs),
        outputs = [output, out_rewritten, out_runtime_manifest] + image_tars,
        tools = toolchain.all_files,
        # The compose binary comes from the toolchain, so the action has to run
        # on the platform that toolchain was resolved for.
        toolchain = TOOLCHAIN_TYPE,
    )

    return output, out_rewritten, out_runtime_manifest

def _collect_images(*, yamls, images):
    transitive_images = []
    for target in yamls:
        if DockerComposeYamlInfo in target:
            info = target[DockerComposeYamlInfo]
            transitive_images.extend(info.images)

    return depset(images + transitive_images).to_list()

def _docker_compose_yaml_impl(ctx):
    toolchain = ctx.toolchains[TOOLCHAIN_TYPE]

    if not ctx.attr.yamls and not ctx.attr.config:
        fail("At least one of `yamls` or `config` must be provided")

    yamls = _collect_yamls(yamls = ctx.attr.yamls)

    extra_yaml_files = []
    config_file = None
    if ctx.attr.config:
        config_file = ctx.actions.declare_file("{}_config.json".format(ctx.label.name))
        ctx.actions.write(
            output = config_file,
            content = ctx.attr.config,
        )
        extra_yaml_files.append(config_file)

    data_files = []
    for target in ctx.attr.data:
        data_files.extend(target[DefaultInfo].files.to_list())

    images = _collect_images(yamls = ctx.attr.yamls, images = ctx.attr.images)

    image_manifests, image_inputs, image_tars = _create_image_manifests(
        ctx = ctx,
        images = images,
        output_fmt = "{name}_images/{image}.json",
    )

    output = ctx.outputs.out
    if not output:
        output = ctx.actions.declare_file("{}/docker-compose.yaml".format(ctx.label.name))

    rewritten = ctx.actions.declare_file(
        "{}.rewritten.yaml".format(output.basename),
        sibling = output,
    )
    runtime_manifest = ctx.actions.declare_file(
        "{}.runtime.json".format(output.basename),
        sibling = output,
    )

    _docker_compose_yaml_action(
        ctx = ctx,
        yamls = yamls.to_list() + extra_yaml_files,
        output = output,
        out_rewritten = rewritten,
        out_runtime_manifest = runtime_manifest,
        toolchain = toolchain,
        image_manifests = image_manifests,
        image_inputs = image_inputs,
        image_tars = image_tars,
        data_inputs = data_files,
        config_file = config_file,
        interpolate = ctx.attr.interpolate,
        interpolation_env = ctx.attr.interpolation_env,
        allow_registry_images = ctx.attr.allow_registry_images,
        ephemeral_ports = ctx.attr.ephemeral_ports,
    )

    return [
        DefaultInfo(
            files = depset([output]),
            runfiles = ctx.runfiles(files = [output] + data_files),
        ),
        DockerComposeYamlInfo(
            yaml = output,
            yaml_rewritten = rewritten,
            runtime_manifest = runtime_manifest,
            yaml_deps = yamls,
            images = ctx.attr.images,
            image_tars = depset(image_tars),
            data = depset(data_files),
        ),
    ]

docker_compose_yaml = rule(
    doc = """\
Merges docker-compose configurations and validates image references.

This rule uses `docker-compose config` to merge one or more docker-compose YAML files
(and/or an inline Starlark configuration) into a single, canonical configuration. It
also validates that all images referenced in the configuration have corresponding loader
targets specified in the `images` attribute.

At least one of `yamls` or `config` must be provided. When both are given, they are
merged together using `docker-compose config` (the `config` JSON is applied on top of
the YAML files).

The merged YAML preserves the original image tags declared in your inputs — it is
diff-reviewable and safe to share outside Bazel (e.g. check it into version control,
hand off to a teammate's `docker compose -f` invocation, etc.).

For each image with a registered loader, the merger ALSO records the identities that
survive a `docker load` (the OCI manifest digest, the image config digest and the layer
DiffIDs), writes a loadable tarball of the image, and emits a second YAML where
`services.*.image` references are rewritten to the content-derived tag
`<repo>:rdc-<manifest digest>` with `pull_policy: never`. That rewritten YAML is consumed
only by `docker_compose_test`, which loads the tarball, tags the image by digest and
verifies it before and after the services start. `docker_compose_binary` (and the
default output of this rule) use the original tags.

### Images

Every `services.*.image` value must equal a tag declared on a loader target listed in
`images`; that string is the whole contract between the compose file and Bazel. The loader
is where the image's content is pinned, through an `oci.pull` or `img.pull` by digest, while
the compose file only names it. So `image: postgres:16` keeps working unchanged outside
Bazel and, inside Bazel, resolves to whatever digest the pull declares.

| Loader | Source | Tag attribute |
|--------|--------|---------------|
| [oci_load](https://github.com/bazel-contrib/rules_oci/blob/main/docs/load.md#oci_load) | rules_oci | `repo_tags` |
| [image_load](https://github.com/bazel-contrib/rules_img/blob/main/docs/load.md#image_load) | rules_img | `tag`, `tag_list` or `tag_file` |

Only the image content behind a loader is used: the OCI layout of an `oci_load`, or the
`tarball` output group of an `image_load`. The loader's own script is never run. For
`image_load`, the base image's `img.pull` must therefore use `layer_handling = "eager"`
(or `"lazy"`) rather than the default `"shallow"`, so every layer is available to build
the tarball.

A referenced image without a loader is an error, unless it is pinned to a digest
(`<repo>@sha256:<digest>`) and `allow_registry_images` is set.

Example with rules_oci:
```python
load("@rules_docker_compose//docker_compose:docker_compose_yaml.bzl", "docker_compose_yaml")
load("@rules_oci//oci:defs.bzl", "oci_load")

# `@my_image` comes from an `oci.pull` in MODULE.bazel, pinned by digest, or from an
# `oci_image` built in this repository.
oci_load(
    name = "my_image.load",
    image = "@my_image",
    repo_tags = ["my-app:latest"],
)

docker_compose_yaml(
    name = "compose",
    # References `image: my-app:latest`.
    yamls = ["docker-compose.yaml"],
    images = [":my_image.load"],
)
```

Example with rules_img:
```python
load("@rules_docker_compose//docker_compose:docker_compose_yaml.bzl", "docker_compose_yaml")
load("@rules_img//img:image.bzl", "image_manifest")
load("@rules_img//img:load.bzl", "image_load")
load("@rules_img_images.bzl", "image")

# The `img.pull` behind `image("my_base")` must set `layer_handling = "eager"`.
image_manifest(
    name = "my_image",
    base = image("my_base"),
)

image_load(
    name = "my_image.load",
    image = ":my_image",
    tag = "my-app:latest",
)

docker_compose_yaml(
    name = "compose",
    yamls = ["docker-compose.yaml"],
    images = [":my_image.load"],
)
```

Public images need no change to the compose file. Pull them by digest and let the loader
claim the name the yaml already uses:
```python
oci_load(
    name = "postgres.load",
    image = "@postgres",  # oci.pull(..., digest = "sha256:...")
    repo_tags = ["postgres:16"],
)
```

Example with inline Starlark config:
```python
load("@rules_docker_compose//docker_compose:docker_compose_config.bzl", "docker_compose_config")
load("@rules_docker_compose//docker_compose:docker_compose_yaml.bzl", "docker_compose_yaml")
load("@rules_oci//oci:defs.bzl", "oci_load")

oci_load(
    name = "nginx.load",
    image = ":nginx",
    repo_tags = ["my-app/nginx:latest"],
)

docker_compose_yaml(
    name = "compose",
    config = docker_compose_config(
        services = {
            "web": {
                "image": "my-app/nginx:latest",
                "ports": ["8080:80"],
            },
        },
    ),
    images = [":nginx.load"],
)
```
""",
    implementation = _docker_compose_yaml_impl,
    attrs = {
        "allow_registry_images": attr.bool(
            doc = """\
Permit image references that have no loader target and are pinned to a digest
(`<repo>@sha256:<digest>`). Such images are pulled from their registry when the
services start, so the test depends on network access and registry credentials
at runtime. Unpinned references without a loader are always an error.
""",
            default = False,
        ),
        "config": attr.string(
            doc = "A JSON-encoded docker-compose configuration string. Use `docker_compose_config()` to produce this value from structured Starlark dicts. Can be used alone or combined with `yamls`.",
        ),
        "data": attr.label_list(
            doc = "Additional files referenced by the docker-compose configuration (e.g. env_file, config files, secret files, bind-mount sources). These are made available during the merge action and propagated to consuming rules for runtime availability.",
            allow_files = True,
        ),
        "ephemeral_ports": attr.bool(
            doc = """\
Let the daemon choose the host port for every published port when the configuration is
used by a `docker_compose_test`, instead of the host port declared in the compose file.
The runner reads the assigned ports after startup and passes them to the test as
`RULES_DOCKER_COMPOSE_HOST_<SERVICE>_<PORT>` and `RULES_DOCKER_COMPOSE_PORT_<SERVICE>_<PORT>`.
The merged yaml and `docker_compose_binary` keep the declared ports. Set to `False` only
when a test genuinely needs a fixed host port, accepting that it then cannot run alongside
anything else using it.
""",
            default = True,
        ),
        "images": attr.label_list(
            doc = _IMAGES_ATTR_DOC,
            aspects = [image_load_repository_aspect],
        ),
        "interpolate": attr.bool(
            doc = """\
Whether to resolve `${VAR}` references when merging.

When `True` (the default), `docker-compose config` interpolates every variable
at build time. Bazel actions run with a sanitized environment, so a variable
that isn't declared in `interpolation_env` resolves to an empty string.

Set to `False` to keep `${VAR}` references verbatim in the merged YAML so
docker-compose resolves them at runtime instead — the right choice for
per-developer or per-machine values like `${USER}`. `services.*.image` is
always resolved at build time regardless, because each image reference must be
matched to a loader target; declare any variables it uses in
`interpolation_env`.
""",
            default = True,
        ),
        "interpolation_env": attr.string_dict(
            doc = """\
Environment variables used to interpolate the compose files at build time.

These are visible only to the merge action, not to the containers. Because the
values are declared in the BUILD file rather than read from the invoking shell,
they keep the merged YAML hermetic and correctly cached.

When `interpolate = False` this applies to `services.*.image` only, since that
is the only field still resolved at build time; every other reference is left
for docker-compose to resolve at runtime.

To set variables the containers see at runtime, use the compose file's own
`environment`/`env_file` keys. To let a variable come from the host at runtime,
set `interpolate = False` and (for `bazel test`) list it in the
`env_inherit` attribute of `docker_compose_test`.
""",
            default = {},
        ),
        "out": attr.output(
            doc = "Optional output filename for the merged docker-compose YAML. Defaults to `{name}/docker-compose.yaml`.",
        ),
        "yamls": attr.label_list(
            doc = "One or more docker-compose YAML files to merge. Files are merged in order using `docker-compose config`. At least one of `yamls` or `config` must be provided.",
            allow_files = [".yaml", ".yml"],
        ),
        "_merger": attr.label(
            cfg = "exec",
            executable = True,
            default = Label("//docker_compose/private/merger"),
        ),
    },
    toolchains = [TOOLCHAIN_TYPE],
)

def _docker_compose_binary_impl(ctx):
    toolchain = ctx.toolchains[TOOLCHAIN_TYPE]

    info = ctx.attr.yaml[DockerComposeYamlInfo]
    yaml = info.yaml
    runtime_manifest = info.runtime_manifest

    is_windows = ctx.executable._launcher.basename.endswith(".exe")
    executable = ctx.actions.declare_file("{}{}".format(
        ctx.label.name,
        ".exe" if is_windows else "",
    ))

    ctx.actions.symlink(
        output = executable,
        target_file = ctx.executable._launcher,
        is_executable = True,
    )

    args_file = ctx.actions.declare_file("{0}_data/{0}.args.txt".format(ctx.label.name))
    runfiles = ctx.runfiles(
        files = [args_file, yaml, runtime_manifest],
        transitive_files = depset(transitive = [toolchain.all_files, info.image_tars]),
    )

    if info.data:
        runfiles = runfiles.merge(ctx.runfiles(transitive_files = info.data))

    args = ctx.actions.args()
    args.set_param_file_format("multiline")
    args.add("-docker-compose", _rlocationpath(toolchain.docker_compose, ctx.workspace_name))
    args.add("-yaml", _rlocationpath(yaml, ctx.workspace_name))
    args.add("-runtime-manifest", _rlocationpath(runtime_manifest, ctx.workspace_name))

    ctx.actions.write(
        output = args_file,
        content = args,
    )

    workspace_name = ctx.label.workspace_name
    if not workspace_name:
        workspace_name = ctx.workspace_name

    return [
        DefaultInfo(
            files = depset([executable]),
            runfiles = runfiles,
            executable = executable,
        ),
        RunEnvironmentInfo(
            environment = {
                "REPOSITORY_NAME": workspace_name,
                "RULES_DOCKER_COMPOSE_ARGS_FILE": _rlocationpath(args_file, workspace_name),
            },
        ),
    ]

docker_compose_binary = rule(
    doc = """\
Creates a runnable target for invoking docker-compose with a generated configuration.

This rule consumes a `docker_compose_yaml` target and produces an executable that:

1. Loads the container images recorded by the `docker_compose_yaml` target, tags each by
   digest with the names declared on its loader, and verifies them against the build output
2. Runs `docker-compose -f <yaml> <args...>` forwarding all command-line arguments

Use this rule when you need to interactively manage docker-compose services
(e.g. `bazel run :compose -- up -d`, `bazel run :compose -- down`).

Example:
```python
load("@rules_docker_compose//docker_compose:defs.bzl", "docker_compose_binary", "docker_compose_yaml")
load("@rules_oci//oci:defs.bzl", "oci_load")

oci_load(
    name = "my_service.load",
    image = ":my_service",
    repo_tags = ["my-service:latest"],
)

docker_compose_yaml(
    name = "compose_yaml",
    yamls = ["docker-compose.yaml"],
    images = [":my_service.load"],
)

docker_compose_binary(
    name = "compose",
    yaml = ":compose_yaml",
    images = [":my_service.load"],
)
```

Then run with:
```bash
bazel run :compose -- up -d
bazel run :compose -- down
bazel run :compose -- ps
```
""",
    implementation = _docker_compose_binary_impl,
    attrs = {
        "images": attr.label_list(
            doc = (
                "Additional loader targets. Images are normally declared on the `docker_compose_yaml` target " +
                "passed as `yaml`, which already carries everything needed to load and verify them. " +
                "See `docker_compose_yaml.images`."
            ),
            aspects = [image_load_repository_aspect],
        ),
        "yaml": attr.label(
            doc = "A `docker_compose_yaml` target providing the merged docker-compose configuration.",
            mandatory = True,
            providers = [DockerComposeYamlInfo],
        ),
        "_launcher": attr.label(
            cfg = "exec",
            executable = True,
            default = Label("//docker_compose/private/launcher"),
        ),
    },
    executable = True,
    toolchains = [TOOLCHAIN_TYPE],
)

def _expand_args(ctx, args, targets, known_variables):
    expanded = []
    for arg in args:
        expanded.append(ctx.expand_make_variables(
            arg,
            ctx.expand_location(arg, targets),
            known_variables,
        ))

    return expanded

_ArgsInfo = provider(
    doc = "Arguments collected from a target.",
    fields = {
        "args": "list[str]: Expanded command line arguments.",
    },
)

def _args_collecter_aspect_impl(target, ctx):
    if not hasattr(ctx.rule.attr, "args"):
        return []

    known_variables = {}
    for target in getattr(ctx.rule.attr, "toolchains", []):
        if platform_common.TemplateVariableInfo in target:
            variables = getattr(target[platform_common.TemplateVariableInfo], "variables", {})
            known_variables.update(variables)

    args = ctx.rule.attr.args
    data = getattr(ctx.rule.attr, "data", [])

    expanded = _expand_args(ctx, args, data, known_variables)

    return [_ArgsInfo(
        args = expanded,
    )]

_args_collecter_aspect = aspect(
    doc = "An aspect for collecting `args` from `docker_compose_test.test` targets.",
    implementation = _args_collecter_aspect_impl,
)

def _create_run_environment_info(ctx, env, env_inherit, targets, known_variables):
    """Create an environment info provider

    This macro performs location expansions.

    Args:
        ctx (ctx): The rule's context object.
        env (dict): Environment variables to set.
        env_inherit (list): Environment variables to inehrit from the host.
        targets (List[Target]): Targets to use in location expansion.
        known_variables (dict): A mapping of expansion keys to values.

    Returns:
        RunEnvironmentInfo: The provider.
    """
    expanded_env = {}
    for key, value in env.items():
        expanded_env[key] = ctx.expand_make_variables(
            key,
            ctx.expand_location(value, targets),
            known_variables,
        )

    workspace_name = ctx.label.workspace_name
    if not workspace_name:
        workspace_name = ctx.workspace_name

    # Needed for bzlmod-aware runfiles resolution.
    expanded_env["REPOSITORY_NAME"] = workspace_name

    return RunEnvironmentInfo(
        environment = expanded_env,
        inherited_environment = env_inherit,
    )

def _rlocationpath(file, workspace_name):
    if file.short_path.startswith("../"):
        return file.short_path[len("../"):]

    return "{}/{}".format(workspace_name, file.short_path)

# Variables the Docker CLI and compose use to select a daemon. They are always
# inherited so a context, a rootless daemon or Docker Desktop on Linux behaves
# the same under `bazel test` as in the invoking shell.
_DOCKER_ENDPOINT_ENV = [
    "DOCKER_HOST",
    "DOCKER_CONTEXT",
    "DOCKER_CONFIG",
    "DOCKER_CERT_PATH",
    "DOCKER_TLS_VERIFY",
    "DOCKER_API_VERSION",
]

def _docker_compose_test_impl(ctx):
    toolchain = ctx.toolchains[TOOLCHAIN_TYPE]

    images = _collect_images(yamls = ctx.attr.yamls, images = ctx.attr.images)

    data_from_yaml = None
    if len(ctx.attr.yamls) == 1 and DockerComposeYamlInfo in ctx.attr.yamls[0]:
        # The merge already happened in the `docker_compose_yaml` target, so
        # interpolation is configured there. Fail rather than silently drop it.
        if not ctx.attr.interpolate or ctx.attr.interpolation_env or ctx.attr.allow_registry_images or not ctx.attr.ephemeral_ports:
            fail((
                "`interpolate`, `interpolation_env`, `allow_registry_images` and `ephemeral_ports` have no " +
                "effect when `yamls` is a single `docker_compose_yaml` target. Set them on {} instead."
            ).format(ctx.attr.yamls[0].label))

        info = ctx.attr.yamls[0][DockerComposeYamlInfo]
        yaml = info.yaml_rewritten
        runtime_manifest = info.runtime_manifest
        data_from_yaml = info.data
        image_tars = info.image_tars
    else:
        yamls = _collect_yamls(yamls = ctx.attr.yamls)

        image_manifests, image_inputs, image_tars = _create_image_manifests(
            ctx = ctx,
            images = images,
            output_fmt = "{name}_images/{image}.json",
        )

        _, yaml, runtime_manifest = _docker_compose_yaml_action(
            ctx = ctx,
            yamls = yamls.to_list(),
            output = ctx.actions.declare_file("{}-docker-compose.yaml".format(ctx.label.name)),
            out_rewritten = ctx.actions.declare_file("{}-docker-compose.rewritten.yaml".format(ctx.label.name)),
            out_runtime_manifest = ctx.actions.declare_file("{}-docker-compose.yaml.runtime.json".format(ctx.label.name)),
            toolchain = toolchain,
            image_manifests = image_manifests,
            image_inputs = image_inputs,
            image_tars = image_tars,
            interpolate = ctx.attr.interpolate,
            interpolation_env = ctx.attr.interpolation_env,
            allow_registry_images = ctx.attr.allow_registry_images,
            # This merge only ever feeds the test, so an unbacked image is a
            # build error rather than something deferred to the runner.
            require_loaders = True,
            ephemeral_ports = ctx.attr.ephemeral_ports,
        )
        image_tars = depset(image_tars)

    is_windows = ctx.executable._test_runner.basename.endswith(".exe")
    executable = ctx.actions.declare_file("{}{}".format(
        ctx.label.name,
        ".exe" if is_windows else "",
    ))

    ctx.actions.symlink(
        output = executable,
        target_file = ctx.executable._test_runner,
        is_executable = True,
    )

    known_variables = {}
    for target in ctx.attr.toolchains:
        if platform_common.TemplateVariableInfo in target:
            variables = getattr(target[platform_common.TemplateVariableInfo], "variables", {})
            known_variables.update(variables)

    args_file = ctx.actions.declare_file("{0}_data/{0}.args.txt".format(ctx.label.name))
    runfiles = ctx.runfiles(
        files = [args_file, yaml, runtime_manifest],
        transitive_files = depset(transitive = [toolchain.all_files, image_tars]),
    )

    if data_from_yaml:
        runfiles = runfiles.merge(ctx.runfiles(transitive_files = data_from_yaml))

    args = ctx.actions.args()
    args.set_param_file_format("multiline")
    args.add("-docker-compose", _rlocationpath(toolchain.docker_compose, ctx.workspace_name))
    args.add("-yaml", _rlocationpath(yaml, ctx.workspace_name))
    args.add("-runtime-manifest", _rlocationpath(runtime_manifest, ctx.workspace_name))

    if ctx.attr.delay:
        args.add("-delay", ctx.attr.delay)
    args.add("-startup-timeout", "{}s".format(ctx.attr.startup_timeout))

    test_env_inherit = []
    if ctx.attr.test:
        runfiles = runfiles.merge(ctx.attr.test[DefaultInfo].default_runfiles)
        args.add("-test", _rlocationpath(ctx.executable.test, ctx.workspace_name))

        # Add test arguments
        if ctx.attr.test_args:
            for arg in _expand_args(ctx, ctx.attr.test_args, ctx.attr.data, known_variables):
                args.add("-test-arg", arg)
        else:
            for arg in ctx.attr.test[_ArgsInfo].args:
                args.add("-test-arg", arg)

        # A `*_test` target declares its environment through RunEnvironmentInfo,
        # which Bazel only applies when that target is the one being run. Carry
        # it across: fixed values go to the runner, inherited names are added to
        # this test's own inherited set so they reach the inner process.
        if RunEnvironmentInfo in ctx.attr.test:
            test_env_info = ctx.attr.test[RunEnvironmentInfo]
            for key, value in test_env_info.environment.items():
                args.add("-test-env", "{}={}".format(key, value))
            test_env_inherit = test_env_info.inherited_environment

    ctx.actions.write(
        output = args_file,
        content = args,
    )

    return [
        DefaultInfo(
            files = depset(),
            runfiles = runfiles,
            executable = executable,
        ),
        _create_run_environment_info(
            ctx,
            env = ctx.attr.env | {
                "RULES_DOCKER_COMPOSE_TEST_ARGS_FILE": _rlocationpath(args_file, ctx.workspace_name),
            },
            env_inherit = _DOCKER_ENDPOINT_ENV + [
                name
                for name in ctx.attr.env_inherit + test_env_inherit
                if name not in _DOCKER_ENDPOINT_ENV
            ],
            targets = ctx.attr.data,
            known_variables = known_variables,
        ),
        testing.ExecutionInfo(
            requirements = {
                # The test needs the host network to reach the daemon and the
                # published ports, and a daemon only exists on the local host.
                "no-remote-exec": "1",
                "requires-network": "1",
            },
        ),
    ]

docker_compose_test = rule(
    doc = """\
Runs a test with docker-compose services.

This rule starts docker-compose services, waits for them to be ready, runs a test binary,
and then tears down the services. The test lifecycle is:

1. Loading each image's tarball (skipped when the content is already present), tagging it
   by digest with a content-derived unique tag, and verifying the result against the
   identity recorded at build time
2. Starting services with `docker-compose up` against a YAML that references only those
   unique tags with `pull_policy: never`
3. Waiting for every service to be running, and healthy when it declares a healthcheck
4. Verifying that every container runs the image ID verified in step 1
5. Running the test binary with optional arguments
6. Capturing compose logs and running `docker-compose down`, on every exit path

Images are never located by the tags declared on loaders, so a tag that something else
re-pointed cannot leak into a test. Images that other tests share are safe because the
unique tag names immutable content and images are never removed.

### Ports

Host ports are the other shared resource on a machine running many tests. By default the
test yaml drops the host side of every published port, the daemon assigns a free one at
startup, and the runner hands the result to the test through the environment:

| Variable | Value |
|----------|-------|
| `RULES_DOCKER_COMPOSE_HOST_<SERVICE>_<PORT>` | `host:port` the container port is reachable at, e.g. `127.0.0.1:49153` |
| `RULES_DOCKER_COMPOSE_PORT_<SERVICE>_<PORT>` | the host port alone |

`<SERVICE>` is the compose service name upper-cased with anything but letters and digits
replaced by `_`, and `<PORT>` is the container port. `${NAME}` references in `test_args`
and in the test target's `env` are expanded against these at test time, so a test can be
told `-host $${RULES_DOCKER_COMPOSE_HOST_NGINX_80}` without knowing the port in advance.
Two tests publishing the same declared port therefore no longer conflict, and the declared
port keeps working for `bazel run`. Set `ephemeral_ports = False` to opt out.

Each run also uses its own compose project name, the target-derived name plus a random
suffix, so concurrent runs of one target, or a run overlapping a crashed predecessor, never
share containers or networks. Network and volume names derived from the project follow
suit; names set explicitly in the compose file, like `container_name`, are kept as written
and remain the user's responsibility.

The test requires network access and a Docker daemon reachable from the host. The Docker
endpoint variables are inherited from the invoking shell.

Images are declared through `oci_load` or `image_load` targets in `images`, and every
`services.*.image` value must equal a tag declared on one of them. The `docker_compose_yaml`
rule documentation describes the contract, the supported loaders and the rules_img
requirements in full; passing a single `docker_compose_yaml` target as `yamls` is the
simplest way to share one set of images between a yaml, a test and a binary.

The `test` is normally an ordinary `*_test` target written with the language's test
framework and tagged `manual`, so `bazel test //...` runs it only through this rule, with
the services up. Its `args` and `env` are honored, which keeps the service address next to
the test that reads it.

Example:
```python
load("@rules_docker_compose//docker_compose:docker_compose_test.bzl", "docker_compose_test")
load("@rules_oci//oci:defs.bzl", "oci_load")
load("@rules_python//python:defs.bzl", "py_test")

oci_load(
    name = "my_service.load",
    image = ":my_service",
    repo_tags = ["my-service:latest"],
)

py_test(
    name = "my_service_test",
    srcs = ["my_service_test.py"],
    env = {"SERVICE_HOST": "localhost:8080"},
    # Only meaningful with the services running, so only run via :integration_test.
    tags = ["manual"],
)

docker_compose_test(
    name = "integration_test",
    yamls = ["docker-compose.yaml"],
    images = [":my_service.load"],
    test = ":my_service_test",
    delay = 2,  # Wait 2 seconds after containers start
)
```
""",
    implementation = _docker_compose_test_impl,
    attrs = {
        "allow_registry_images": attr.bool(
            doc = """\
Permit digest-pinned image references that have no loader when merging raw
`yamls`. See the `docker_compose_yaml` rule for details. Setting this is an
error when `yamls` is a single `docker_compose_yaml` target.
""",
            default = False,
        ),
        "data": attr.label_list(
            doc = "Additional runtime dependencies for the test.",
        ),
        "delay": attr.int(
            doc = "Seconds to wait after containers are running before executing the test. Useful for services that need initialization time beyond their health checks.",
            default = 1,
        ),
        "env": attr.string_dict(
            doc = "Dictionary of strings; values are subject to `$(location)` and \"Make variable\" substitution",
            default = {},
        ),
        "env_inherit": attr.string_list(
            doc = (
                "Specifies additional environment variables to inherit from the external environment when the " +
                "test is executed by `bazel test`. The Docker endpoint variables (`DOCKER_HOST`, `DOCKER_CONTEXT`, " +
                "`DOCKER_CONFIG`, `DOCKER_CERT_PATH`, `DOCKER_TLS_VERIFY`, `DOCKER_API_VERSION`) are always inherited."
            ),
        ),
        "ephemeral_ports": attr.bool(
            doc = """\
Let the daemon choose host ports when merging raw `yamls`. See the `docker_compose_yaml`
rule for details. Setting this is an error when `yamls` is a single `docker_compose_yaml`
target.
""",
            default = True,
        ),
        "images": attr.label_list(
            doc = _IMAGES_ATTR_DOC,
            aspects = [image_load_repository_aspect],
        ),
        "interpolate": attr.bool(
            doc = """\
Whether to resolve `${VAR}` references when merging raw `yamls` at build time.
See the `docker_compose_yaml` rule for details.

Setting this is an error when `yamls` is a single `docker_compose_yaml` target,
which carries its own `interpolate` setting.

Note that `bazel test` runs with a sanitized environment, so a deferred variable
also needs to be listed in `env_inherit` (or set in `env`) to reach
docker-compose at runtime.
""",
            default = True,
        ),
        "interpolation_env": attr.string_dict(
            doc = """\
Environment variables used to interpolate raw `yamls` at build time. See the
`docker_compose_yaml` rule for details.

This is distinct from `env`, which sets the environment of the test process
itself. Setting this is an error when `yamls` is a single `docker_compose_yaml`
target; declare it on that target instead.
""",
            default = {},
        ),
        "startup_timeout": attr.int(
            doc = (
                "Seconds to wait for every service to be running, and healthy when it declares a healthcheck, " +
                "before the test is considered failed."
            ),
            default = 60,
        ),
        "test": attr.label(
            doc = (
                "The executable to run once the services are up. This is usually a `*_test` target " +
                "(`py_test`, `rust_test`, `go_test`, ...) tagged `manual` so it only runs here; its `args` " +
                "and `env` are honored. A plain binary works too. Arguments from `test_args` take precedence."
            ),
            cfg = "target",
            executable = True,
            aspects = [_args_collecter_aspect],
        ),
        "test_args": attr.string_list(
            doc = (
                "Arguments to pass to the test binary. `${NAME}` references are expanded at test time against " +
                "the test's environment, including the port variables; write them as `$${NAME}` in BUILD files."
            ),
            default = [],
        ),
        "yamls": attr.label_list(
            doc = "One or more docker-compose YAML files defining the services to run. Files are merged using `docker-compose config`.",
            allow_files = [".yaml", ".yml"],
        ),
        "_merger": attr.label(
            cfg = "exec",
            executable = True,
            default = Label("//docker_compose/private/merger"),
        ),
        "_test_runner": attr.label(
            cfg = "exec",
            executable = True,
            default = Label("//docker_compose/private/runner"),
        ),
    },
    test = True,
    toolchains = [TOOLCHAIN_TYPE],
)
