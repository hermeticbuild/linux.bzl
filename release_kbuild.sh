#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${VERSION:-}" ]]; then
  echo "usage: VERSION=v0.0.1 ./release_kbuild.sh" >&2
  exit 1
fi

tag="kconfig-${VERSION}"

git tag -a "${tag}" -m "${tag}"
git push origin "refs/tags/${tag}"
