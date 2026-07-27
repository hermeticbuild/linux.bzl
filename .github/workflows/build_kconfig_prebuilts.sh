#!/usr/bin/env bash
set -euo pipefail

export LANG=C
export LC_ALL=C

BAZEL_STARTUP_ARGS=()
BAZEL_ARGS=("--lockfile_mode=error")
if [[ -n "${BUILDBUDDY_API_KEY:-}" ]]; then
  BAZEL_ARGS+=(
    "--remote_cache=grpcs://remote.buildbuddy.io"
    "--remote_executor=grpcs://remote.buildbuddy.io"
    "--remote_header=x-buildbuddy-api-key=${BUILDBUDDY_API_KEY}"
  )
else
  BAZEL_STARTUP_ARGS=("--ignore_all_rc_files")
fi

bazel "${BAZEL_STARTUP_ARGS[@]}" build "${BAZEL_ARGS[@]}" \
  //internal/cmd/kconfig/prebuilts:kconfig_prebuilts

rm -rf dist
mkdir -p dist

copy_out() {
  local label="$1"
  local src

  src="$(
    bazel "${BAZEL_STARTUP_ARGS[@]}" cquery "${BAZEL_ARGS[@]}" \
      --output=files "${label}"
  )"
  cp "${src}" "dist/$(basename "${src}")"
}

copy_out //internal/cmd/kconfig/prebuilts:kconfig-darwin-amd64
copy_out //internal/cmd/kconfig/prebuilts:kconfig-darwin-arm64
copy_out //internal/cmd/kconfig/prebuilts:kconfig-linux-amd64
copy_out //internal/cmd/kconfig/prebuilts:kconfig-linux-arm64
copy_out //internal/cmd/kconfig/prebuilts:kconfig-windows-amd64
copy_out //internal/cmd/kconfig/prebuilts:kconfig-windows-arm64

check_archive() {
  local archive="$1"
  shift
  local expected
  local listing

  listing="$(tar --zstd -tf "dist/${archive}")"
  expected="$(printf '%s\n' "$@")"
  if [[ "${listing}" != "${expected}" ]]; then
    echo "dist/${archive} has unexpected contents; wanted:" >&2
    printf '%s\n' "${expected}" >&2
    echo "got:" >&2
    printf '%s\n' "${listing}" >&2
    exit 1
  fi
}

check_archive kconfig-darwin-amd64.tar.zst kconfig kconfig_parse
check_archive kconfig-darwin-arm64.tar.zst kconfig kconfig_parse
check_archive kconfig-linux-amd64.tar.zst kconfig kconfig_parse
check_archive kconfig-linux-arm64.tar.zst kconfig kconfig_parse
check_archive kconfig-windows-amd64.tar.zst kconfig.exe kconfig_parse.exe
check_archive kconfig-windows-arm64.tar.zst kconfig.exe kconfig_parse.exe

(
  cd dist
  shasum -a 256 *.tar.zst > SHA256SUMS
  for archive in *.tar.zst; do
    integrity="sha256-$(openssl dgst -sha256 -binary "${archive}" | openssl base64 -A)"
    {
      echo "${archive}"
      echo "sha256=$(awk -v archive="${archive}" '$2 == archive {print $1}' SHA256SUMS)"
      echo "integrity=${integrity}"
    }
  done > kconfig_tool_releases.metadata
)
