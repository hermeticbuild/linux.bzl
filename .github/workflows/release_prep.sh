#!/usr/bin/env bash

set -o errexit -o nounset -o pipefail

TAG=$1
PREFIX="linux.bzl-${TAG:1}"
ARCHIVE="linux.bzl-$TAG.tar.gz"

git archive --format=tar --prefix="${PREFIX}/" "${TAG}" | gzip > "${ARCHIVE}"

cat << EOF
## Add to your \`MODULE.bazel\` file:

\`\`\`starlark
bazel_dep(name = "linux.bzl", version = "${TAG:1}")
\`\`\`

EOF
