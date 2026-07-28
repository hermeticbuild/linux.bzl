#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 dist/kconfig-linux-amd64.tar.zst" >&2
  exit 2
fi

root="$(git rev-parse --show-toplevel)"
archive="$(realpath "$1")"
if [[ ! -f "${archive}" ]]; then
  echo "candidate archive does not exist: ${archive}" >&2
  exit 1
fi
if [[ -n "$(git -C "${root}" status --porcelain --untracked-files=normal)" ]]; then
  echo "schema comparison requires a clean checkout with no untracked source files" >&2
  exit 1
fi
if [[ "$(grep -c '^_REPOSITORY_COMPACT_SCHEMA = "v0.0.12"$' "${root}/internal/linux_image_repository.bzl")" -ne 1 ]]; then
  echo "expected the released repository schema to remain v0.0.12" >&2
  exit 1
fi
if [[ "$(grep -c '^KCONFIG_TOOL_VERSION = "v0.0.12"$' "${root}/internal/kconfig_tool_releases.bzl")" -ne 1 ]]; then
  echo "expected the released Kconfig tool version to remain v0.0.12" >&2
  exit 1
fi

work_root="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/linux-bzl-schema-diff"
report_dir="${root}/dist/kconfig-schema-diff"
rm -rf "${work_root}" "${report_dir}"
mkdir -p "${work_root}" "${report_dir}"

candidate_sha256="$(sha256sum "${archive}" | awk '{print $1}')"
candidate_integrity="sha256-$(openssl dgst -sha256 -binary "${archive}" | openssl base64 -A)"
candidate_version="v0.0.14-candidate"
candidate_dist="$(dirname "${archive}")"
repository_cache="${work_root}/repository-cache"
mkdir -p "${repository_cache}"

{
  printf 'archive=%s\n' "$(basename "${archive}")"
  printf 'sha256=%s\n' "${candidate_sha256}"
  printf 'integrity=%s\n' "${candidate_integrity}"
  tar --zstd -tf "${archive}"
} > "${report_dir}/candidate.txt"

copy_checkout() {
  local destination="$1"
  mkdir -p "${destination}"
  git -C "${root}" archive HEAD | tar -x -C "${destination}"
}

write_candidate_release() {
  local checkout="$1"
  cat > "${checkout}/internal/kconfig_tool_releases.bzl" <<EOF
"""Candidate release metadata used only by kconfig_schema_diff.sh."""

visibility("//...")

KCONFIG_TOOL_VERSION = "${candidate_version}"

KCONFIG_TOOL_RELEASES = {
    "linux_amd64": struct(
        integrity = "${candidate_integrity}",
        urls = ["https://linux.bzl.invalid/kconfig-linux-amd64.tar.zst"],
    ),
}
EOF
}

for schema in v0.0.12 v0.0.13; do
  checkout="${work_root}/${schema}"
  copy_checkout "${checkout}"
  write_candidate_release "${checkout}"
  if [[ "${schema}" == "v0.0.13" ]]; then
    sed -i \
      's/^_REPOSITORY_COMPACT_SCHEMA = "v0.0.12"$/_REPOSITORY_COMPACT_SCHEMA = "v0.0.13"/' \
      "${checkout}/internal/linux_image_repository.bzl"
  fi
  if [[ "$(grep -c "^_REPOSITORY_COMPACT_SCHEMA = \"${schema}\"$" "${checkout}/internal/linux_image_repository.bzl")" -ne 1 ]]; then
    echo "failed to select repository schema ${schema}" >&2
    exit 1
  fi
done

fields=(
  config
  image
  kernel_release
  module_symvers
  modules
  modules_builtin
  modules_builtin_modinfo
  modules_order
  system_map
  vmlinux
)

examples_specs=(
  "x86_64/base|@example_x86_64//:"
  "x86_64/debug|@example_x86_64//variants/debug:"
  "x86_64/btf|@example_x86_64//variants/btf:"
  "x86_64/lz4|@example_x86_64//variants/lz4:"
  "aarch64/base|@example_aarch64//:"
)

e2e_specs=(
  "e2e/x86_64|@e2e_x86_64//:"
  "e2e/aarch64|@e2e_aarch64//:"
)

bazel_startup_args=(
  --ignore_all_rc_files
  --host_jvm_args=-Xmx5g
)

bazel_common_args=(
  --check_direct_dependencies=off
  --distdir="${candidate_dist}"
  --enable_platform_specific_config
  --experimental_output_paths=strip
  --incompatible_enforce_starlark_utf8=error
  --incompatible_modify_execution_info_additive
  --jobs=4
  --lockfile_mode=error
  --module_mirrors=https://bcr.cloudflaremirrors.com
  --nobuild_runfile_links
  --remote_download_outputs=all
  --repo_env=BAZEL_DO_NOT_DETECT_CPP_TOOLCHAIN=1
  --repo_env=BAZEL_NO_APPLE_CPP_TOOLCHAIN=1
  --repository_cache="${repository_cache}"
)

output_bases=()
output_workspaces=()

run_bazel() {
  local workspace="$1"
  local output_base="$2"
  shift 2
  (
    cd "${workspace}"
    bazel \
      "${bazel_startup_args[@]}" \
      --output_base="${output_base}" \
      "$@" \
      "${bazel_common_args[@]}"
  )
}

collect_failure_diagnostics() {
  local output_base="$1"
  local external="${output_base}/external"
  local destination
  local file
  local relative
  local repository
  local -a repositories=("${external}"/+linux_image+*)

  destination="${report_dir}/failed-graphs/$(basename "${output_base}")"
  if [[ ! -d "${external}" ]]; then
    return
  fi
  for repository in "${repositories[@]}"; do
    if [[ ! -d "${repository}" ]]; then
      continue
    fi
    while IFS= read -r -d '' file; do
      relative="${file#"${external}/"}"
      mkdir -p "${destination}/$(dirname "${relative}")"
      cp -L "${file}" "${destination}/${relative}"
    done < <(
      find -L "${repository}" -type f \
        \( \
          -name .linux-bzl-generator.json -o \
          \( -path "*/graph/*" \
            \( -name BUILD.bazel -o -name metadata.json \) \) \
        \) \
        -print0 2>/dev/null
    )
  done
}

shutdown_output_base() {
  local workspace="$1"
  local output_base="$2"

  if [[ ! -d "${output_base}" ]]; then
    return
  fi
  (
    cd "${workspace}"
    bazel \
      "${bazel_startup_args[@]}" \
      --output_base="${output_base}" \
      shutdown >/dev/null 2>&1
  ) || true
  rm -rf "${output_base}"
}

cleanup() {
  local status=$?
  local index
  local output_base

  trap - EXIT
  if ((status != 0)); then
    for output_base in "${output_bases[@]}"; do
      if ! collect_failure_diagnostics "${output_base}"; then
        echo "warning: failed to preserve diagnostics from ${output_base}" >&2
      fi
    done
  fi
  for index in "${!output_bases[@]}"; do
    shutdown_output_base \
      "${output_workspaces[index]}" \
      "${output_bases[index]}" || true
  done
  exit "${status}"
}
trap cleanup EXIT

# The archive build immediately preceding this gate uses the checkout's default
# output base. Do not retain that Bazel server alongside the isolated builds.
(
  cd "${root}"
  bazel shutdown >/dev/null 2>&1
) || true

assert_generator_marker() {
  local output_base="$1"
  local repository="$2"
  local schema="$3"
  local workspace_name="$4"
  local marker
  marker="$(
    find -L "${output_base}/external" \
      -path "*/+linux_image+${repository}/.linux-bzl-generator.json" \
      -type f -print -quit
  )"
  if [[ -z "${marker}" ]]; then
    echo "missing generator marker for ${repository}" >&2
    exit 1
  fi
  jq -e \
    --arg schema "${schema}" \
    --arg tool_version "${candidate_version}" \
    '.compact_schema == $schema and .tool_version == $tool_version' \
    "${marker}" >/dev/null
  cp "${marker}" \
    "${report_dir}/${schema}-${workspace_name}-${repository}-generator.json"
}

stage_projection() {
  local workspace="$1"
  local output_base="$2"
  local schema="$3"
  local logical_name="$4"
  local field="$5"
  local label="$6"
  local destination="${work_root}/staged/${schema}/${logical_name}/${field}"
  local file
  local source
  local relative
  local query_output
  local -a files=()

  if ! query_output="$(
    run_bazel "${workspace}" "${output_base}" cquery "${label}" --output=files
  )"; then
    echo "failed to resolve outputs for ${label}" >&2
    exit 1
  fi
  if [[ -n "${query_output}" ]]; then
    mapfile -t files <<<"${query_output}"
  fi
  mkdir -p "${destination}"
  if [[ "${field}" != "modules" && ${#files[@]} -ne 1 ]]; then
    echo "${label} produced ${#files[@]} files; expected exactly one" >&2
    exit 1
  fi
  for file in "${files[@]}"; do
    source="${file}"
    if [[ "${source}" != /* ]]; then
      source="${workspace}/${source}"
    fi
    if [[ ! -f "${source}" ]]; then
      echo "${label} output was not materialized: ${source}" >&2
      exit 1
    fi
    if [[ "${field}" == "modules" ]]; then
      relative="${file#*.modules/}"
      if [[ "${relative}" == "${file}" ]]; then
        relative="$(basename "${file}")"
      fi
    else
      relative="artifact"
    fi
    if [[ -e "${destination}/${relative}" ]]; then
      echo "${label} maps multiple outputs to ${relative}" >&2
      exit 1
    fi
    mkdir -p "$(dirname "${destination}/${relative}")"
    cp "${source}" "${destination}/${relative}"
  done
}

run_workspace() {
  local schema="$1"
  local workspace_name="$2"
  local workspace="$3"
  local output_base="$4"
  shift 4
  local -a specs=("$@")
  local -a targets=()
  local spec
  local logical_name
  local prefix
  local field
  local repository

  for spec in "${specs[@]}"; do
    logical_name="${spec%%|*}"
    prefix="${spec#*|}"
    for field in "${fields[@]}"; do
      targets+=("${prefix}${field}")
    done
  done

  run_bazel "${workspace}" "${output_base}" build \
    "${targets[@]}" \
    --build_event_json_file="${report_dir}/${schema}-${workspace_name}.bep.json" \
    --profile="${report_dir}/${schema}-${workspace_name}.profile.gz" \
    2>&1 | tee "${report_dir}/${schema}-${workspace_name}.log"

  for spec in "${specs[@]}"; do
    logical_name="${spec%%|*}"
    prefix="${spec#*|}"
    repository="${prefix#@}"
    repository="${repository%%//*}"
    assert_generator_marker \
      "${output_base}" "${repository}" "${schema}" "${workspace_name}"
    for field in "${fields[@]}"; do
      stage_projection \
        "${workspace}" \
        "${output_base}" \
        "${schema}" \
        "${logical_name}" \
        "${field}" \
        "${prefix}${field}"
    done
    if [[ "${workspace_name}" == "e2e" ]] &&
      [[ -z "$(
        find \
          "${work_root}/staged/${schema}/${logical_name}/modules" \
          -type f -name test_module.ko -print -quit
      )" ]]; then
      echo "${prefix}modules did not contain test_module.ko" >&2
      exit 1
    fi
  done
}

for schema in v0.0.12 v0.0.13; do
  examples_workspace="${work_root}/${schema}/examples"
  e2e_workspace="${work_root}/${schema}/e2e"
  examples_output_base="${work_root}/output-${schema}-examples"
  e2e_output_base="${work_root}/output-${schema}-e2e"
  output_bases+=("${examples_output_base}" "${e2e_output_base}")
  output_workspaces+=("${examples_workspace}" "${e2e_workspace}")
  run_workspace \
    "${schema}" \
    examples \
    "${examples_workspace}" \
    "${examples_output_base}" \
    "${examples_specs[@]}"
  shutdown_output_base "${examples_workspace}" "${examples_output_base}"
  run_workspace \
    "${schema}" \
    e2e \
    "${e2e_workspace}" \
    "${e2e_output_base}" \
    "${e2e_specs[@]}"
  shutdown_output_base "${e2e_workspace}" "${e2e_output_base}"
done

write_manifest() {
  local schema="$1"
  local stage="${work_root}/staged/${schema}"
  (
    cd "${stage}"
    while IFS= read -r -d '' path; do
      printf '%s  %s\n' \
        "$(sha256sum "${path}" | awk '{print $1}')" \
        "${path}"
    done < <(find . -type f -print0 | sort -z)
  ) > "${report_dir}/${schema}.sha256"
}

write_manifest v0.0.12
write_manifest v0.0.13
diff -ru \
  "${work_root}/staged/v0.0.12" \
  "${work_root}/staged/v0.0.13" \
  > "${report_dir}/outputs.diff"
diff -u \
  "${report_dir}/v0.0.12.sha256" \
  "${report_dir}/v0.0.13.sha256" \
  > "${report_dir}/manifests.diff"
printf '%s\n' \
  "v0.0.12 and v0.0.13 public kernel outputs are byte-for-byte identical." \
  | tee "${report_dir}/result.txt"
