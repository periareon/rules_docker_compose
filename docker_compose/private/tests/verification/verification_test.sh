#!/usr/bin/env bash
# Proves the image guarantees of docker_compose_test against a live daemon:
#
#   1. A stale original tag is ignored: the test runs the built image even when
#      the tag it declared points at something else, and leaves that tag alone.
#   2. A clobbered unique tag is caught: if something re-points the unique tag
#      between image verification and compose up, the container check fails.
#
# The inner docker_compose_test is executed directly, with the environment the
# rules would normally provide, so this script can tamper with the daemon in
# between runs.
set -euo pipefail

# --- begin runfiles.bash initialization v3 ---
# Copy-pasted from the Bazel Bash runfiles library v3.
set -uo pipefail; set +e; f=bazel_tools/tools/bash/runfiles/runfiles.bash
# shellcheck disable=SC1090
source "${RUNFILES_DIR:-/dev/null}/$f" 2>/dev/null || \
  source "$(grep -sm1 "^$f " "${RUNFILES_MANIFEST_FILE:-/dev/null}" | cut -f2- -d' ')" 2>/dev/null || \
  source "$0.runfiles/$f" 2>/dev/null || \
  source "$(grep -sm1 "^$f " "$0.runfiles_manifest" | cut -f2- -d' ')" 2>/dev/null || \
  source "$(grep -sm1 "^$f " "$0.exe.runfiles_manifest" | cut -f2- -d' ')" 2>/dev/null || \
  { echo>&2 "ERROR: cannot find $f"; exit 1; }; f=; set -e
# --- end runfiles.bash initialization v3 ---

runfiles_export_envvars

DOCKER="${RULES_DOCKER_COMPOSE_ENGINE_BINARY:-docker}"
INNER="$(rlocation "${INNER_BINARY}")"
MANIFEST="$(rlocation "${TEST_WORKSPACE}/${INNER_RUNTIME_MANIFEST}")"
ORIGINAL_TAG="rules_docker_compose/verification/nginx:latest"
BOGUS_TAG="rules_docker_compose/verification/bogus:latest"

log() { echo "[verification_test] $*" >&2; }

# run_inner [ENV=VALUE...]: runs the inner test with the rules' environment
# contract. Output is captured in $INNER_OUTPUT; the exit code is returned.
run_inner() {
    set +e
    INNER_OUTPUT="$(env \
        REPOSITORY_NAME="${TEST_WORKSPACE}" \
        RULES_DOCKER_COMPOSE_TEST_ARGS_FILE="${TEST_WORKSPACE}/${INNER_ARGS_FILE}" \
        "$@" "${INNER}" 2>&1)"
    local code=$?
    set -e
    echo "${INNER_OUTPUT}" >&2
    return "${code}"
}

image_id() { "${DOCKER}" image inspect --format '{{.Id}}' "$1"; }

cleanup() {
    # Drop the impostor and, if the declared tag was left pointing at it, that
    # tag too, so the host is as it was before the test.
    if [[ -n "${BOGUS_ID:-}" && "$(image_id "${ORIGINAL_TAG}" 2>/dev/null)" == "${BOGUS_ID}" ]]; then
        "${DOCKER}" rmi "${ORIGINAL_TAG}" >/dev/null 2>&1 || true
    fi
    "${DOCKER}" rmi -f "${BOGUS_TAG}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

UNIQUE_TAG="$(sed -n 's/.*"\(rules_docker_compose\/verification\/nginx:rdc-[0-9a-f]*\)".*/\1/p' "${MANIFEST}" | head -n 1)"
[[ -n "${UNIQUE_TAG}" ]] || { log "could not find the unique tag in ${MANIFEST}"; exit 1; }
log "unique tag: ${UNIQUE_TAG}"

# Phase 0: a plain run loads the image and establishes the baseline.
log "phase 0: baseline run"
run_inner
VERIFIED_ID="$(image_id "${UNIQUE_TAG}")"
log "verified image id: ${VERIFIED_ID}"
grep -q "Verified 1 container(s)" <<<"${INNER_OUTPUT}" || { log "baseline run did not report container verification"; exit 1; }

# Build a runnable impostor: the same filesystem flattened into a single layer
# with a plain nginx command, so compose can start it but every identity
# differs from the built image.
log "creating impostor image"
CONTAINER="$("${DOCKER}" create "${UNIQUE_TAG}")"
"${DOCKER}" export "${CONTAINER}" | "${DOCKER}" import \
    --change 'CMD ["nginx", "-g", "daemon off;"]' \
    --change 'EXPOSE 80' \
    - "${BOGUS_TAG}" >/dev/null
"${DOCKER}" rm "${CONTAINER}" >/dev/null
BOGUS_ID="$(image_id "${BOGUS_TAG}")"
[[ "${BOGUS_ID}" != "${VERIFIED_ID}" ]] || { log "impostor has the same id as the real image"; exit 1; }

# Phase 1: point the declared tag at the impostor. The test must still run the
# built image and must not touch the declared tag.
log "phase 1: stale original tag"
"${DOCKER}" tag "${BOGUS_TAG}" "${ORIGINAL_TAG}"
run_inner
grep -q "Verified 1 container(s)" <<<"${INNER_OUTPUT}" || { log "stale-tag run did not report container verification"; exit 1; }
[[ "$(image_id "${ORIGINAL_TAG}")" == "${BOGUS_ID}" ]] || { log "the runner modified the declared tag"; exit 1; }
[[ "$(image_id "${UNIQUE_TAG}")" == "${VERIFIED_ID}" ]] || { log "unique tag no longer points at the verified image"; exit 1; }
log "phase 1 ok: declared tag ignored and left untouched"

# Phase 2: re-point the unique tag at the impostor after image verification
# and before compose up. The container check must fail the test.
log "phase 2: clobbered unique tag"
HOOK='for t in ${RULES_DOCKER_COMPOSE_IMAGE_TAGS}; do '"${DOCKER}"' tag '"${BOGUS_TAG}"' "$t"; done'
if run_inner RULES_DOCKER_COMPOSE_PRE_UP_HOOK="${HOOK}"; then
    log "clobbered run unexpectedly passed"
    exit 1
fi
grep -q "containers do not run the images this test was built with" <<<"${INNER_OUTPUT}" || {
    log "clobbered run failed for a different reason than container verification"
    exit 1
}
# Teardown must have run despite the failure.
if [[ -n "$("${DOCKER}" ps -aq --filter "label=com.docker.compose.project" --filter "name=verification")" ]]; then
    log "containers were left behind after the failed run"
    exit 1
fi
log "phase 2 ok: clobbered tag detected and everything torn down"

# Phase 3: a following plain run heals the unique tag and passes again.
log "phase 3: recovery run"
run_inner
[[ "$(image_id "${UNIQUE_TAG}")" == "${VERIFIED_ID}" ]] || { log "unique tag was not re-pointed at the verified image"; exit 1; }
log "all phases passed"
