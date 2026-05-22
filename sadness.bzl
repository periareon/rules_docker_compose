"""Bazel macro for generating docker-compose JSON files."""

load("@rules_multirun//:defs.bzl", "multirun")
load("@rules_oci//oci:defs.bzl", "oci_tarball")
load("@rules_shell//shell:sh_binary.bzl", "sh_binary")

# TODO
def k2_docker_compose(
        name,
        services,
        networks = {},
        volumes = {},
        visibility = None):
    """Generate a docker-compose.json file from service definitions.

    Args:
        name: The name of the target. If empty, defaults to "docker_compose".
        services: Dictionary of service configurations matching docker-compose format.
                 Services can have "image" + "tag" fields where "tag" is a Bazel target
                 that resolves to an image reference.
        networks: Dictionary of network configurations (optional).
        volumes: Dictionary of volume configurations (optional).
        visibility: Target visibility.
    """

    target_name = name if name else "docker_compose"

    # Extract digest dependencies and create tarball targets for loading
    digest_deps_set = {}  # Use dict to track unique deps with their mappings
    image_load_deps_set = {}  # Use dict to track unique deps
    tag_to_digest_mapping = {}
    processed_tags = {}  # Track processed tags to avoid duplicate work

    for service_name, service_config in services.items():
        if "tag" in service_config:
            tag_value = service_config["tag"]

            # Skip if we've already processed this tag
            if tag_value in processed_tags:
                tag_to_digest_mapping[tag_value] = processed_tags[tag_value]
                continue

            # Handle different tag types
            if tag_value.startswith("@") and "//" in tag_value:
                # External targets: create oci_tarball from image
                load_target_name = target_name + "_load_" + service_name
                oci_tarball(
                    name = load_target_name,
                    image = tag_value,
                    repo_tags = [service_config["image"] + ":latest"],
                )
                image_load_deps_set[":" + load_target_name] = True

                # Use repo digest
                repo_part = tag_value.split("//")[0]
                digest_target = repo_part + "//:digest"
                digest_deps_set[digest_target] = True
                tag_to_digest_mapping[tag_value] = digest_target
                processed_tags[tag_value] = digest_target

            elif tag_value.startswith(":") or tag_value.startswith("//"):
                # Local targets
                # Tag is already a tarball, use it directly for loading
                image_load_deps_set[tag_value] = True

                # Get digest from corresponding image
                image_target = tag_value.replace("tarball", "image")
                digest_target = image_target + ".digest"
                digest_deps_set[digest_target] = True
                tag_to_digest_mapping[tag_value] = digest_target
                processed_tags[tag_value] = digest_target

    # Convert sets back to lists
    digest_deps = list(digest_deps_set.keys())
    image_load_deps = list(image_load_deps_set.keys())

    # Convert configurations to Python dict representations
    services_repr = repr(services)
    networks_repr = repr(networks) if networks else "{}"
    volumes_repr = repr(volumes) if volumes else "{}"
    tag_mapping_repr = repr(tag_to_digest_mapping)

    # Generate the docker-compose JSON file
    compose_file_target = target_name + "_file"
    native.genrule(
        name = compose_file_target,
        tools = ["@//build/oci/private/docker_compose:gen_docker_compose"] + digest_deps,
        outs = ["docker-compose.json"],
        cmd = "$(execpath @//build/oci/private/docker_compose:gen_docker_compose) '{}' '{}' '{}' '{}' {} > $@".format(
            services_repr.replace("'", "'\"'\"'"),  # Escape single quotes for shell
            networks_repr.replace("'", "'\"'\"'"),
            volumes_repr.replace("'", "'\"'\"'"),
            tag_mapping_repr.replace("'", "'\"'\"'"),
            " ".join(["$(execpath {})".format(dep) for dep in digest_deps]),
        ),
    )

    # Create shell script wrapper for docker compose
    wrapper_script_target = target_name + "_wrapper"

    # Generate different wrapper scripts depending on whether we have image dependencies
    if image_load_deps:
        # Script that loads images first, then runs docker-compose
        wrapper_script_content = """
cat > $@ << 'EOF'
#!/bin/bash
set -euo pipefail

# Get the directory where this script is located
SCRIPT_DIR="$$(cd "$$(dirname "$$0")" && pwd)"

# Load images before running docker-compose
echo "Loading required Docker images..."
if [[ -x "$$SCRIPT_DIR/{}_load_images.bash" ]]; then
    if ! "$$SCRIPT_DIR/{}_load_images.bash" > /dev/null 2>&1; then
        echo "Warning: Failed to load some images, but continuing with docker-compose..."
    fi
else
    echo "Warning: Load images script not found at $$SCRIPT_DIR/{}_load_images.bash"
fi

# The docker-compose.json file should be in the same directory as this script
COMPOSE_FILE="$$SCRIPT_DIR/docker-compose.json"

# Use Bazel-managed docker-compose binary from runfiles
DOCKER_COMPOSE_BIN=""
# Try to use Bazel's standard runfiles environment first
if [[ -v RUNFILES_DIR && -n "$$RUNFILES_DIR" && -x "$$RUNFILES_DIR/docker_compose_linux_x86_64/file/docker-compose" ]]; then
    DOCKER_COMPOSE_BIN="$$RUNFILES_DIR/docker_compose_linux_x86_64/file/docker-compose"
elif [[ -v RUNFILES_MANIFEST_FILE && -n "$$RUNFILES_MANIFEST_FILE" ]]; then
    # Use runfiles manifest to find the binary
    DOCKER_COMPOSE_BIN="$$(grep "docker_compose_linux_x86_64/file/docker-compose" "$$RUNFILES_MANIFEST_FILE" 2>/dev/null | cut -d' ' -f2 || true)"
    if [[ -n "$$DOCKER_COMPOSE_BIN" && ! -x "$$DOCKER_COMPOSE_BIN" ]]; then
        DOCKER_COMPOSE_BIN=""
    fi
else
    # Look for runfiles manifest next to the script
    SCRIPT_NAME="$$(basename "$$0")"
    MANIFEST_FILE="$$SCRIPT_DIR/$$SCRIPT_NAME.runfiles_manifest"
    if [[ -f "$$MANIFEST_FILE" ]]; then
        DOCKER_COMPOSE_BIN="$$(grep "docker_compose_linux_x86_64/file/docker-compose" "$$MANIFEST_FILE" 2>/dev/null | cut -d' ' -f2 || true)"
        if [[ -n "$$DOCKER_COMPOSE_BIN" && ! -x "$$DOCKER_COMPOSE_BIN" ]]; then
            DOCKER_COMPOSE_BIN=""
        fi
    fi
fi

# Fallback: search for the docker-compose binary in the runfiles tree
if [[ -z "$$DOCKER_COMPOSE_BIN" ]]; then
    for search_path in "$$SCRIPT_DIR" "$$SCRIPT_DIR/.." "$$SCRIPT_DIR/../.."; do
        if [[ -x "$$search_path/docker-compose" ]]; then
            DOCKER_COMPOSE_BIN="$$search_path/docker-compose"
            break
        fi
        # Also search in runfiles directory structure
        if [[ -x "$$search_path/docker_compose_linux_x86_64/file/docker-compose" ]]; then
            DOCKER_COMPOSE_BIN="$$search_path/docker_compose_linux_x86_64/file/docker-compose"
            break
        fi
    done
fi

# Check for runfiles directory next to script
if [[ -z "$$DOCKER_COMPOSE_BIN" ]]; then
    # Try different runfiles patterns
    for runfiles_pattern in "$$SCRIPT_DIR.runfiles"; do
        if [[ -d "$$runfiles_pattern" ]]; then
            if [[ -x "$$runfiles_pattern/docker_compose_linux_x86_64/file/docker-compose" ]]; then
                DOCKER_COMPOSE_BIN="$$runfiles_pattern/docker_compose_linux_x86_64/file/docker-compose"
                break
            fi
        fi
    done
fi

# Use find as last resort to locate the binary anywhere in runfiles
if [[ -z "$$DOCKER_COMPOSE_BIN" ]]; then
    # Find the runfiles directory
    RUNFILES_SEARCH_DIR="$$SCRIPT_DIR"
    while [[ "$$RUNFILES_SEARCH_DIR" != "/" && ! -f "$$RUNFILES_SEARCH_DIR/_repo_mapping" ]]; do
        RUNFILES_SEARCH_DIR="$$(dirname "$$RUNFILES_SEARCH_DIR")"
    done

    if [[ -f "$$RUNFILES_SEARCH_DIR/_repo_mapping" ]]; then
        DOCKER_COMPOSE_BIN="$$(find "$$RUNFILES_SEARCH_DIR" -name "docker-compose" -type f -executable 2>/dev/null | head -1)"
    fi
fi

if [[ -z "$$DOCKER_COMPOSE_BIN" || ! -x "$$DOCKER_COMPOSE_BIN" ]]; then
    echo "ERROR: Could not find docker-compose binary in runfiles" >&2
    echo "Please ensure the docker-compose dependency is included in the target data." >&2
    exit 1
fi

# Execute docker compose with the generated file and forward all arguments
exec "$$DOCKER_COMPOSE_BIN" -f "$$COMPOSE_FILE" "$$@"
EOF
chmod +x $@
        """.format(target_name, target_name, target_name)
    else:
        # Simple script without image loading
        wrapper_script_content = """
cat > $@ << 'EOF'
#!/bin/bash
set -euo pipefail

# Get the directory where this script is located
SCRIPT_DIR="$$(cd "$$(dirname "$$0")" && pwd)"

# The docker-compose.json file should be in the same directory as this script
COMPOSE_FILE="$$SCRIPT_DIR/docker-compose.json"

# Use Bazel-managed docker-compose binary from runfiles
DOCKER_COMPOSE_BIN=""
# Try to use Bazel's standard runfiles environment first
if [[ -v RUNFILES_DIR && -n "$$RUNFILES_DIR" && -x "$$RUNFILES_DIR/docker_compose_linux_x86_64/file/docker-compose" ]]; then
    DOCKER_COMPOSE_BIN="$$RUNFILES_DIR/docker_compose_linux_x86_64/file/docker-compose"
elif [[ -v RUNFILES_MANIFEST_FILE && -n "$$RUNFILES_MANIFEST_FILE" ]]; then
    # Use runfiles manifest to find the binary
    DOCKER_COMPOSE_BIN="$$(grep "docker_compose_linux_x86_64/file/docker-compose" "$$RUNFILES_MANIFEST_FILE" 2>/dev/null | cut -d' ' -f2 || true)"
    if [[ -n "$$DOCKER_COMPOSE_BIN" && ! -x "$$DOCKER_COMPOSE_BIN" ]]; then
        DOCKER_COMPOSE_BIN=""
    fi
else
    # Look for runfiles manifest next to the script
    SCRIPT_NAME="$$(basename "$$0")"
    MANIFEST_FILE="$$SCRIPT_DIR/$$SCRIPT_NAME.runfiles_manifest"
    if [[ -f "$$MANIFEST_FILE" ]]; then
        DOCKER_COMPOSE_BIN="$$(grep "docker_compose_linux_x86_64/file/docker-compose" "$$MANIFEST_FILE" 2>/dev/null | cut -d' ' -f2 || true)"
        if [[ -n "$$DOCKER_COMPOSE_BIN" && ! -x "$$DOCKER_COMPOSE_BIN" ]]; then
            DOCKER_COMPOSE_BIN=""
        fi
    fi
fi

# Fallback: search for the docker-compose binary in the runfiles tree
if [[ -z "$$DOCKER_COMPOSE_BIN" ]]; then
    for search_path in "$$SCRIPT_DIR" "$$SCRIPT_DIR/.." "$$SCRIPT_DIR/../.."; do
        if [[ -x "$$search_path/docker-compose" ]]; then
            DOCKER_COMPOSE_BIN="$$search_path/docker-compose"
            break
        fi
        # Also search in runfiles directory structure
        if [[ -x "$$search_path/docker_compose_linux_x86_64/file/docker-compose" ]]; then
            DOCKER_COMPOSE_BIN="$$search_path/docker_compose_linux_x86_64/file/docker-compose"
            break
        fi
    done
fi

# Check for runfiles directory next to script
if [[ -z "$$DOCKER_COMPOSE_BIN" ]]; then
    # Try different runfiles patterns
    for runfiles_pattern in "$$SCRIPT_DIR.runfiles"; do
        if [[ -d "$$runfiles_pattern" ]]; then
            if [[ -x "$$runfiles_pattern/docker_compose_linux_x86_64/file/docker-compose" ]]; then
                DOCKER_COMPOSE_BIN="$$runfiles_pattern/docker_compose_linux_x86_64/file/docker-compose"
                break
            fi
        fi
    done
fi

# Use find as last resort to locate the binary anywhere in runfiles
if [[ -z "$$DOCKER_COMPOSE_BIN" ]]; then
    # Find the runfiles directory
    RUNFILES_SEARCH_DIR="$$SCRIPT_DIR"
    while [[ "$$RUNFILES_SEARCH_DIR" != "/" && ! -f "$$RUNFILES_SEARCH_DIR/_repo_mapping" ]]; do
        RUNFILES_SEARCH_DIR="$$(dirname "$$RUNFILES_SEARCH_DIR")"
    done

    if [[ -f "$$RUNFILES_SEARCH_DIR/_repo_mapping" ]]; then
        DOCKER_COMPOSE_BIN="$$(find "$$RUNFILES_SEARCH_DIR" -name "docker-compose" -type f -executable 2>/dev/null | head -1)"
    fi
fi

if [[ -z "$$DOCKER_COMPOSE_BIN" || ! -x "$$DOCKER_COMPOSE_BIN" ]]; then
    echo "ERROR: Could not find docker-compose binary in runfiles" >&2
    echo "Please ensure the docker-compose dependency is included in the target data." >&2
    exit 1
fi

# Execute docker compose with the generated file and forward all arguments
exec "$$DOCKER_COMPOSE_BIN" -f "$$COMPOSE_FILE" "$$@"
EOF
chmod +x $@
        """

    native.genrule(
        name = wrapper_script_target,
        srcs = [":" + compose_file_target],
        outs = [target_name + "_wrapper.sh"],
        cmd = wrapper_script_content,
    )

    # Create separate targets for loading images and the main runnable target
    if image_load_deps:
        # Create multirun target for loading all images
        multirun(
            name = target_name + "_load_images",
            commands = image_load_deps,
            visibility = visibility,
        )

        # Create the main executable target
        sh_binary(
            name = target_name,
            srcs = [":" + wrapper_script_target],
            data = [":" + compose_file_target, ":" + target_name + "_load_images", "@rules_docker_compose//docker_compose"],
            visibility = visibility,
            tags = ["no-remote-exec"],
        )

        # Create alias for direct access to the compose file
        native.alias(
            name = target_name + "_json",
            actual = ":" + compose_file_target,
            visibility = visibility,
        )
    else:
        # If no image dependencies, create the executable target
        sh_binary(
            name = target_name,
            srcs = [":" + wrapper_script_target],
            data = [":" + compose_file_target, "@rules_docker_compose//docker_compose"],
            visibility = visibility,
            tags = ["no-remote-exec"],
        )

        # Create alias for direct access to the compose file
        native.alias(
            name = target_name + "_json",
            actual = ":" + compose_file_target,
            visibility = visibility,
        )
