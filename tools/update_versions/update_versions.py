"""A script for fetching all available versions of docker-compose."""

import argparse
import base64
import binascii
import json
import logging
import os
import re
import time
import urllib.request
from pathlib import Path
from urllib.error import HTTPError
from urllib.parse import urlparse
from urllib.request import urlopen

DOCKER_COMPOSE_GITHUB_RELEASES_API_TEMPLATE = (
    "https://api.github.com/repos/docker/compose/releases?page={page}"
)

DOCKER_COMPOSE_ASSET_URL_TEMPLATE = (
    "https://github.com/docker/compose/releases/download/v{version}/{artifact}"
)

DOCKER_COMPOSE_RELEASE_NAME_REGEX = r"^v(\d+\.\d+\.\d+)$"

DOCKER_COMPOSE_PLATFORM_ARTIFACT_MAP = {
    "darwin-aarch64": ["darwin-aarch64"],
    "darwin-x86_64": ["darwin-x86_64"],
    "linux-aarch64": ["linux-aarch64"],
    "linux-armv7": ["linux-armv7"],
    "linux-ppc64le": ["linux-ppc64le"],
    "linux-riscv64": ["linux-riscv64"],
    "linux-s390x": ["linux-s390x"],
    "linux-x86_64": ["linux-x86_64"],
    "windows-aarch64": ["windows-aarch64.exe"],
    "windows-x86_64": ["windows-x86_64.exe"],
}

REQUEST_HEADERS = {"User-Agent": "curl/8.7.1"}  # Set the User-Agent header

BUILD_TEMPLATE = """\
\"\"\"Docker-Compose Versions

A mapping of platform to integrity of the archive for said platform for each version of Docker-Compose available.
\"\"\"

# AUTO-GENERATED: DO NOT MODIFY
#
# Update using the following command:
#
# ```
# bazel run //tools/update_versions
# ```

DOCKER_COMPOSE_VERSIONS = {versions}
"""


def _workspace_root() -> Path:
    if "BUILD_WORKSPACE_DIRECTORY" in os.environ:
        return Path(os.environ["BUILD_WORKSPACE_DIRECTORY"])

    return Path(__file__).parent.parent.parent


def parse_args() -> argparse.Namespace:
    """Parse command line arguments."""
    parser = argparse.ArgumentParser(description=__doc__)

    parser.add_argument(
        "--output",
        type=Path,
        default=_workspace_root() / "docker_compose/private/versions.bzl",
        help="The path in which to save results.",
    )
    parser.add_argument(
        "--verbose",
        action="store_true",
        help="Enable verbose logging",
    )

    return parser.parse_args()


def fetch_checksums(checksums_url: str) -> dict[str, str]:
    """Parse sha256 data from docker_compose checksums.txt file."""

    artifacts = {}
    req = urllib.request.Request(checksums_url, headers=REQUEST_HEADERS)
    logging.debug("Fetching checksums file: %s", checksums_url)
    with urlopen(req) as resp:
        data = resp.read()
        # Artifacts are uploaded with inconsistent encodings.
        try:
            content = data.decode("utf-8")
        except UnicodeDecodeError:
            content = data.decode("utf-16")

        for line in content.splitlines():
            line = line.strip()
            if not line:
                continue
            # Format: "sha256 *artifact-name" or "sha256  artifact-name"
            parts = line.split(None, 1)
            if len(parts) == 2:
                sha256, artifact = parts
                # Remove leading * if present
                artifact = artifact.lstrip("*").strip()
                artifacts[artifact] = sha256.strip()

    return artifacts


def integrity(hex_str: str) -> str:
    """Convert a sha256 hex value to a Bazel integrity value"""

    # Remove any whitespace and convert from hex to raw bytes
    try:
        raw_bytes = binascii.unhexlify(hex_str.strip())
    except binascii.Error as e:
        raise ValueError(f"Invalid hex input: {e}") from e

    # Convert to base64
    encoded = base64.b64encode(raw_bytes).decode("utf-8")
    return f"sha256-{encoded}"


def query_releases() -> dict[str, dict[str, str]]:
    page = 1
    releases_data = {}
    version_regex = re.compile(DOCKER_COMPOSE_RELEASE_NAME_REGEX)
    while True:
        url = urlparse(DOCKER_COMPOSE_GITHUB_RELEASES_API_TEMPLATE.format(page=page))
        req = urllib.request.Request(url.geturl(), headers=REQUEST_HEADERS)
        logging.debug("Releases url: %s", url.geturl())

        try:
            with urlopen(req) as data:
                json_data = json.loads(data.read())
                if not json_data:
                    break
                for release in json_data:
                    regex = version_regex.match(release["tag_name"])
                    if not regex:
                        continue
                    version = regex.group(1)
                    logging.debug("Processing %s", version)

                    checksums_url = None
                    for asset in release["assets"]:
                        if asset["name"] == "checksums.txt":
                            checksums_url = DOCKER_COMPOSE_ASSET_URL_TEMPLATE.format(
                                version=version,
                                artifact=asset["name"],
                            )
                            break

                    if not checksums_url:
                        logging.debug("No checksums.txt artifact.")
                        continue

                    sha256s = fetch_checksums(checksums_url)
                    if not sha256s:
                        logging.debug("No checksums collected.")
                        continue

                    artifacts = {}
                    for (
                        platform,
                        rank_suffix,
                    ) in DOCKER_COMPOSE_PLATFORM_ARTIFACT_MAP.items():
                        for suffix in rank_suffix:
                            if platform in artifacts:
                                break

                            for artifact, sha256 in sha256s.items():
                                if artifact.endswith(suffix):
                                    logging.debug(
                                        "Matched artifact for %s: %s",
                                        platform,
                                        artifact,
                                    )
                                    artifact_url = (
                                        DOCKER_COMPOSE_ASSET_URL_TEMPLATE.format(
                                            version=version,
                                            artifact=artifact,
                                        )
                                    )
                                    artifacts[platform] = {
                                        "url": artifact_url,
                                        "integrity": integrity(sha256),
                                    }
                                    break

                    logging.debug("Matched %s artifacts", len(artifacts))
                    releases_data[version] = artifacts

            page += 1
            time.sleep(0.5)
        except HTTPError as exc:
            if exc.code != 403:
                raise

            reset_time = exc.headers.get("x-ratelimit-reset")
            if not reset_time:
                raise

            sleep_duration = float(reset_time) - time.time()
            if sleep_duration < 0.0:
                continue

            logging.warning("%s", exc.msg)
            logging.debug("Waiting %ss for reset", sleep_duration)
            time.sleep(sleep_duration)

    return releases_data


def main() -> None:
    """The main entrypoint."""
    args = parse_args()

    if args.verbose:
        logging.basicConfig(level=logging.DEBUG)
    else:
        logging.basicConfig(level=logging.INFO)

    releases = query_releases()

    logging.debug("Writing to %s", args.output)
    # Convert the releases dict to a properly formatted Python dict string
    versions_str = json.dumps(releases, indent=4)
    args.output.write_text(BUILD_TEMPLATE.format(versions=versions_str))
    logging.info("Done")


if __name__ == "__main__":
    main()
