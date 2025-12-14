"""Docker-Compose rules"""

load(":image_utils.bzl", "ImageLoadRepositoryInfo", "image_load_repository_aspect")
load(":toolchain.bzl", "TOOLCHAIN_TYPE")

DockerComposeYamlInfo = provider(
    doc = """\
Provides information about a resolved docker-compose YAML configuration.

This provider is returned by `docker_compose_yaml` and contains the merged
docker-compose configuration along with metadata about images and dependencies.
It can be consumed by `docker_compose_test` or other rules that need to work
with docker-compose configurations.
""",
    fields = {
        "images": "depset[File]: The image loader executables for each image referenced in the yaml.",
        "yaml": "File: The merged docker-compose YAML file.",
        "yaml_deps": "depset[File]: All files which contributed to the merged yaml (input yamls and image manifests).",
        "yaml_lock": "File: A JSON lockfile mapping image tags to their content digests for verification.",
    },
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
    image_inputs = []
    single_image_manifests = []
    for image in images:
        # Add all tag_files
        tag_files = image[ImageLoadRepositoryInfo].tag_files
        if tag_files:
            image_inputs.extend(tag_files)

        # Add the appropriate image file based on format
        if image[ImageLoadRepositoryInfo].oci_layout:
            image_inputs.append(image[ImageLoadRepositoryInfo].oci_layout)
        elif image[ImageLoadRepositoryInfo].manifest_file:
            image_inputs.append(image[ImageLoadRepositoryInfo].manifest_file)

        single_image_manifest = ctx.actions.declare_file(output_fmt.replace(
            "{name}",
            ctx.label.name,
        ).replace(
            "{image}",
            str(image.label).strip("@").replace("/", "_").replace(":", "_"),
        ))

        # Set mutually exclusive fields based on image format
        oci_layout_dir = None
        manifest_file = None
        if image[ImageLoadRepositoryInfo].oci_layout:
            oci_layout_dir = image[ImageLoadRepositoryInfo].oci_layout.path
        elif image[ImageLoadRepositoryInfo].manifest_file:
            manifest_file = image[ImageLoadRepositoryInfo].manifest_file.path
        else:
            fail("Unable to determine repository info for {}".format(image.label))

        # Collect tag file paths
        tag_file_paths = []
        if tag_files:
            tag_file_paths = [tag_file.path for tag_file in tag_files]

        ctx.actions.write(
            output = single_image_manifest,
            content = json.encode_indent(
                struct(
                    label = str(image.label),
                    tag_file_paths = tag_file_paths,
                    oci_layout_dir = oci_layout_dir,
                    manifest_file = manifest_file,
                ),
                indent = " " * 4,
            ),
        )
        image_inputs.extend(image[DefaultInfo].default_runfiles.files.to_list())
        single_image_manifests.append(single_image_manifest)

    return single_image_manifests, image_inputs

def _docker_compose_yaml_action(
        *,
        ctx,
        yamls,
        output,
        out_lock,
        toolchain,
        image_manifests = [],
        image_inputs = []):
    # Sanitize label for project name: replace @ with AT, + with -, / with _, : with __
    project_name = str(ctx.label).replace("@", "0").replace("+", "-").replace("/", "_").replace(":", "__")

    args = ctx.actions.args()
    args.add("-docker-compose", toolchain.docker_compose)
    args.add("-output", output)
    args.add("-project-name", project_name)
    args.add("-output-lock", out_lock)
    args.add("-digest-mode", toolchain.digest_mode)
    for file in yamls:
        args.add("-file", file)

    for manifest in image_manifests:
        # Add the single_image_manifest to args
        args.add("-image_manifest", manifest)

    ctx.actions.run(
        mnemonic = "DockerComposeYamlConfig",
        executable = ctx.executable._merger,
        arguments = [args],
        inputs = depset(yamls + image_inputs + image_manifests),
        outputs = [output, out_lock],
        tools = toolchain.all_files,
    )

    return output, out_lock

def _collect_images(*, yamls, images):
    transitive_images = []
    for target in yamls:
        if DockerComposeYamlInfo in target:
            info = target[DockerComposeYamlInfo]
            transitive_images.extend(info.images)

    return depset(images + transitive_images).to_list()

def _docker_compose_yaml_impl(ctx):
    toolchain = ctx.toolchains[TOOLCHAIN_TYPE]

    yamls = _collect_yamls(yamls = ctx.attr.yamls)

    images = _collect_images(yamls = ctx.attr.yamls, images = ctx.attr.images)

    image_manifests, image_inputs = _create_image_manifests(
        ctx = ctx,
        images = images,
        output_fmt = "{name}_images/{image}.json",
    )

    output = ctx.outputs.out
    if not output:
        output = ctx.actions.declare_file("{}/docker-compose.yaml".format(ctx.label.name))

    lock = ctx.actions.declare_file("{}.lock.json".format(output.basename), sibling = output)

    _docker_compose_yaml_action(
        ctx = ctx,
        yamls = yamls.to_list(),
        output = output,
        out_lock = lock,
        toolchain = toolchain,
        image_manifests = image_manifests,
        image_inputs = image_inputs,
    )

    return [
        DefaultInfo(
            files = depset([output]),
            runfiles = ctx.runfiles(files = [output]),
        ),
        DockerComposeYamlInfo(
            yaml = output,
            yaml_lock = lock,
            yaml_deps = yamls,
            images = ctx.attr.images,
        ),
    ]

docker_compose_yaml = rule(
    doc = """\
Merges multiple docker-compose YAML files and validates image references.

This rule uses `docker-compose config` to merge one or more docker-compose YAML files
into a single, canonical configuration. It also validates that all images referenced
in the configuration have corresponding loader targets specified in the `images` attribute.

A lock file is generated containing a mapping of image tags to their content digests,
which can be used to verify that the correct images are loaded at runtime.

Supported image loader types:

| Loader | Source |
|--------|--------|
| [oci_load](https://github.com/bazel-contrib/rules_oci/blob/main/docs/load.md#oci_load) | rules_oci |
| [image_load](https://github.com/bazel-contrib/rules_img/blob/main/docs/load.md#image_load) | rules_img |

Example:
```python
load("@rules_docker_compose//docker_compose:docker_compose_yaml.bzl", "docker_compose_yaml")
load("@rules_oci//oci:defs.bzl", "oci_load")

oci_load(
    name = "my_image.load",
    image = ":my_image",
    repo_tags = ["my-app:latest"],
)

docker_compose_yaml(
    name = "compose",
    yamls = ["docker-compose.yaml"],
    images = [":my_image.load"],
)
```
""",
    implementation = _docker_compose_yaml_impl,
    attrs = {
        "images": attr.label_list(
            doc = "Image loader targets that provide the container images referenced in the YAML files. Each image referenced in the docker-compose YAML must have a corresponding loader target. See the rule documentation for supported loader types.",
            aspects = [image_load_repository_aspect],
        ),
        "out": attr.output(
            doc = "Optional output filename for the merged docker-compose YAML. Defaults to `{name}/docker-compose.yaml`.",
        ),
        "yamls": attr.label_list(
            doc = "One or more docker-compose YAML files to merge. Files are merged in order using `docker-compose config`.",
            allow_files = [".yaml", ".yml"],
            mandatory = True,
            allow_empty = False,
        ),
        "_merger": attr.label(
            cfg = "exec",
            executable = True,
            default = Label("//docker_compose/private/merger"),
        ),
    },
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

def _docker_compose_test_impl(ctx):
    toolchain = ctx.toolchains[TOOLCHAIN_TYPE]

    images = _collect_images(yamls = ctx.attr.yamls, images = ctx.attr.images)

    if len(ctx.attr.yamls) == 1 and DockerComposeYamlInfo in ctx.attr.yamls[0]:
        info = ctx.attr.yamls[0][DockerComposeYamlInfo]
        yaml = info.yaml
        lock = info.yaml_lock
    else:
        yamls = _collect_yamls(yamls = ctx.attr.yamls)

        image_manifests, image_inputs = _create_image_manifests(
            ctx = ctx,
            images = images,
            output_fmt = "{name}_images/{image}.json",
        )

        yaml, lock = _docker_compose_yaml_action(
            ctx = ctx,
            yamls = yamls.to_list(),
            output = ctx.actions.declare_file("{}-docker-compose.yaml".format(ctx.label.name)),
            out_lock = ctx.actions.declare_file("{}-docker-compose.yaml.lock.json".format(ctx.label.name)),
            toolchain = toolchain,
            image_manifests = image_manifests,
            image_inputs = image_inputs,
        )

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
    runfiles = ctx.runfiles(files = [args_file, yaml, lock], transitive_files = toolchain.all_files)

    args = ctx.actions.args()
    args.set_param_file_format("multiline")
    args.add("-docker-compose", _rlocationpath(toolchain.docker_compose, ctx.workspace_name))
    args.add("-yaml", _rlocationpath(yaml, ctx.workspace_name))
    args.add("-lock", _rlocationpath(lock, ctx.workspace_name))

    if ctx.attr.delay:
        args.add("-delay", ctx.attr.delay)

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

    for image in images:
        image_info = image[ImageLoadRepositoryInfo]
        runfiles = runfiles.merge(image_info.loader_runfiles)
        args.add("-loader", _rlocationpath(image_info.loader, ctx.workspace_name))

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
            env_inherit = ctx.attr.env_inherit,
            targets = ctx.attr.data,
            known_variables = known_variables,
        ),
        testing.ExecutionInfo(
            requirements = {
                "requires-network": "1",
            },
        ),
    ]

docker_compose_test = rule(
    doc = """\
Runs a test with docker-compose services.

This rule starts docker-compose services, waits for them to be ready, runs a test binary,
and then tears down the services. The test lifecycle is:

1. Loading container images into Docker using the specified loader targets
2. Starting services with `docker-compose up`
3. Waiting for containers to be running (with health checks)
4. Verifying image digests match the expected values from the lock file
5. Running the test binary with optional arguments
6. Cleaning up with `docker-compose down`

The test requires network access and Docker to be available on the host.

Supported image loader types:

| Loader | Source |
|--------|--------|
| [oci_load](https://github.com/bazel-contrib/rules_oci/blob/main/docs/load.md#oci_load) | rules_oci |
| [image_load](https://github.com/bazel-contrib/rules_img/blob/main/docs/load.md#image_load) | rules_img |

Example:
```python
load("@rules_docker_compose//docker_compose:docker_compose_test.bzl", "docker_compose_test")
load("@rules_oci//oci:defs.bzl", "oci_load")

oci_load(
    name = "my_service.load",
    image = ":my_service",
    repo_tags = ["my-service:latest"],
)

docker_compose_test(
    name = "integration_test",
    yamls = ["docker-compose.yaml"],
    images = [":my_service.load"],
    test = ":my_test_binary",
    test_args = ["-host", "localhost:8080"],
    delay = 2,  # Wait 2 seconds after containers start
)
```
""",
    implementation = _docker_compose_test_impl,
    attrs = {
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
            doc = "Specifies additional environment variables to inherit from the external environment when the test is executed by `bazel test`.",
        ),
        "images": attr.label_list(
            doc = "Image loader targets that provide the container images for the docker-compose services. Each image will be loaded into Docker before `docker-compose up` is called. See the rule documentation for supported loader types.",
            aspects = [image_load_repository_aspect],
        ),
        "test": attr.label(
            doc = "The test binary to execute after containers are running. The binary will receive arguments from `test_args`.",
            cfg = "target",
            executable = True,
            aspects = [_args_collecter_aspect],
        ),
        "test_args": attr.string_list(
            doc = "Arguments to pass to the test binary.",
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
