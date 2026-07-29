#!/usr/bin/env bash

set -euo pipefail

readonly RUNFILES_ROOT="${TEST_SRCDIR}/${TEST_WORKSPACE}"
readonly SOURCE_FIXTURE="${RUNFILES_ROOT}/internal/tests/linux_image_extension_integration/fixture"
readonly WORK_ROOT="${TEST_TMPDIR}/linux_image_extension"
readonly MODULE_UNDER_TEST="${WORK_ROOT}/linux_bzl"
readonly BAZEL_BIN="${BAZEL:-bazel}"
readonly SUCCESS_OUTPUT="${TEST_TMPDIR}/success-output"
readonly MISSING_CC_PROFILE_OUTPUT="${TEST_TMPDIR}/missing-cc-profile-output"
readonly NON_ROOT_TAGS_OUTPUT="${TEST_TMPDIR}/non-root-tags-output"

fail() {
  echo "linux_image_extension_integration_test: $*" >&2
  exit 1
}

prepare_fixture() {
  rm -rf "${WORK_ROOT}"
  cp -R "${SOURCE_FIXTURE}" "${WORK_ROOT}"

  find "${WORK_ROOT}" -name BUILD.fixture -print0 |
    while IFS= read -r -d '' build_file; do
      mv "${build_file}" "${build_file%/BUILD.fixture}/BUILD.bazel"
    done

  cp "${RUNFILES_ROOT}/extensions.bzl" "${MODULE_UNDER_TEST}/extensions.bzl"
  mkdir -p "${MODULE_UNDER_TEST}/internal"
  for source in \
    compact_v7_repository.bzl \
    config_validation.bzl \
    kconfig_tool_filename.bzl \
    kconfig_tool_releases.bzl \
    linux_image_extension.bzl \
    linux_image_repository.bzl \
    repository_utils.bzl; do
    cp "${RUNFILES_ROOT}/internal/${source}" "${MODULE_UNDER_TEST}/internal/${source}"
  done
}

run_bazel() {
  local workspace="$1"
  local output_base="$2"
  shift 2
  (
    cd "${workspace}"
    "${BAZEL_BIN}" \
      --nohome_rc \
      --nosystem_rc \
      --noworkspace_rc \
      --output_base="${output_base}" \
      "$@" \
      --lockfile_mode=off \
      --color=no \
      --curses=no
  )
}

shutdown_bazel() {
  local output_base="$1"
  "${BAZEL_BIN}" \
    --nohome_rc \
    --nosystem_rc \
    --noworkspace_rc \
    --output_base="${output_base}" \
    shutdown >/dev/null 2>&1 || true
}

cleanup() {
  shutdown_bazel "${SUCCESS_OUTPUT}"
  shutdown_bazel "${MISSING_CC_PROFILE_OUTPUT}"
  shutdown_bazel "${NON_ROOT_TAGS_OUTPUT}"
}

expect_failure() {
  local workspace="$1"
  local output_base="$2"
  local expected="$3"
  shift 3
  local log="${output_base}.log"
  if run_bazel "${workspace}" "${output_base}" "$@" >"${log}" 2>&1; then
    fail "command unexpectedly succeeded: $*"
  fi
  if ! grep -F "${expected}" "${log}" >/dev/null; then
    cat "${log}" >&2
    fail "command did not report expected error: ${expected}"
  fi
}

prepare_fixture
command -v "${BAZEL_BIN}" >/dev/null || fail "bazel executable not found: ${BAZEL_BIN}"
trap cleanup EXIT

readonly SUCCESS="${WORK_ROOT}/success"
readonly QUERY_OUTPUT="${TEST_TMPDIR}/facade-query.txt"
run_bazel \
  "${SUCCESS}" \
  "${SUCCESS_OUTPUT}" \
  query \
  '@fixture_kernel//...' \
  --output=label >"${QUERY_OUTPUT}"

for target in \
  config \
  image \
  kernel \
  kernel_release \
  module_sdk \
  module_symvers \
  modules \
  modules_builtin \
  modules_builtin_modinfo \
  modules_order \
  system_map \
  vmlinux; do
  grep -E "/+:${target}$" "${QUERY_OUTPUT}" >/dev/null ||
    fail "base facade is missing ${target}"
  grep -E "/variants/debug:${target}$" "${QUERY_OUTPUT}" >/dev/null ||
    fail "debug variant facade is missing ${target}"
done
grep -E '/graph:metadata\.json$' "${QUERY_OUTPUT}" >/dev/null ||
  fail "facade is missing graph metadata"

readonly BUILD_OUTPUT="${TEST_TMPDIR}/facade-build-query.txt"
run_bazel \
  "${SUCCESS}" \
  "${SUCCESS_OUTPUT}" \
  query \
  '@fixture_kernel//:image + @fixture_kernel//variants/debug:image' \
  --output=build >"${BUILD_OUTPUT}"
grep -E 'fixture_kernel__linux_graph//:image",$' "${BUILD_OUTPUT}" >/dev/null ||
  fail "base alias does not use the hidden sibling graph repository"
grep -E 'fixture_kernel__linux_graph//variants/debug:image",$' "${BUILD_OUTPUT}" >/dev/null ||
  fail "variant alias does not use the hidden sibling graph repository"

readonly METADATA_BUILD_OUTPUT="${TEST_TMPDIR}/metadata-build-query.txt"
run_bazel \
  "${SUCCESS}" \
  "${SUCCESS_OUTPUT}" \
  query \
  '@fixture_kernel//graph:metadata.json' \
  --output=build >"${METADATA_BUILD_OUTPUT}"
grep -E 'fixture_kernel__linux_graph//graph:metadata.json",$' "${METADATA_BUILD_OUTPUT}" >/dev/null ||
  fail "metadata alias does not use the hidden sibling graph repository"

run_bazel \
  "${SUCCESS}" \
  "${SUCCESS_OUTPUT}" \
  query \
  '//:graph_metadata'

expect_failure \
  "${SUCCESS}" \
  "${SUCCESS_OUTPUT}" \
  "No repository visible as '@fixture_kernel__linux_graph'" \
  query \
  '@fixture_kernel__linux_graph//:image'

expect_failure \
  "${SUCCESS}" \
  "${SUCCESS_OUTPUT}" \
  "No repository visible as '@fixture_kernel__linux_graph'" \
  query \
  '@fixture_kernel__linux_graph//graph:metadata.json'

expect_failure \
  "${SUCCESS}" \
  "${SUCCESS_OUTPUT}" \
  "source must be in a dedicated external repository" \
  build \
  --nobuild \
  '@fixture_kernel//:image'

expect_failure \
  "${WORK_ROOT}/missing_cc_profile" \
  "${MISSING_CC_PROFILE_OUTPUT}" \
  "mandatory attribute" \
  query \
  '@fixture_kernel//:image'
grep -F "cc_profile" "${MISSING_CC_PROFILE_OUTPUT}.log" >/dev/null ||
  fail "missing mandatory attribute error did not identify cc_profile"

expect_failure \
  "${WORK_ROOT}/non_root_tags" \
  "${NON_ROOT_TAGS_OUTPUT}" \
  "linux_images tags are root-module application choices" \
  query \
  '@dependency_kernel//:image'
