#!/usr/bin/env bash
set -euo pipefail

export LANG=C
export LC_ALL=C

if [[ -z "${BUILDBUDDY_API_KEY:-}" ]]; then
  echo "BUILDBUDDY_API_KEY GitHub secret is required for kconfig prebuilt builds." >&2
  exit 1
fi

BAZEL_ARGS=(
  "--remote_cache=grpcs://remote.buildbuddy.io"
  "--remote_executor=grpcs://remote.buildbuddy.io"
  "--remote_header=x-buildbuddy-api-key=${BUILDBUDDY_API_KEY}"
)

bazel build "${BAZEL_ARGS[@]}" //internal/cmd/kconfig/prebuilts:kconfig_prebuilts

rm -rf dist
mkdir -p dist

copy_out() {
  local label="$1"
  local src

  src="$(bazel cquery "${BAZEL_ARGS[@]}" --output=files "${label}")"
  cp "${src}" "dist/$(basename "${src}")"
}

copy_out //internal/cmd/kconfig/prebuilts:kconfig-darwin-amd64
copy_out //internal/cmd/kconfig/prebuilts:kconfig-darwin-arm64
copy_out //internal/cmd/kconfig/prebuilts:kconfig-linux-amd64
copy_out //internal/cmd/kconfig/prebuilts:kconfig-linux-arm64
copy_out //internal/cmd/kconfig/prebuilts:kconfig-windows-amd64
copy_out //internal/cmd/kconfig/prebuilts:kconfig-windows-arm64

(
  cd dist
  shasum -a 256 *.tar.gz > SHA256SUMS
  for archive in *.tar.gz; do
    integrity="sha256-$(openssl dgst -sha256 -binary "${archive}" | openssl base64 -A)"
    {
      echo "${archive}"
      echo "sha256=$(awk -v archive="${archive}" '$2 == archive {print $1}' SHA256SUMS)"
      echo "integrity=${integrity}"
    }
  done > kconfig_tool_releases.metadata
)
