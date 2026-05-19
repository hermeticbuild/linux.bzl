#!/usr/bin/env bash
set -euo pipefail

qemu="$1"
kernel="$2"
timeout_seconds="$3"
expect="$4"
kernel_args="$5"
shift 5

log="$(mktemp "${TMPDIR:-/tmp}/linux-qemu-boot.XXXXXX.log")"
pid=""

cleanup() {
  if [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null; then
    kill "${pid}" 2>/dev/null || true
    wait "${pid}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

"${qemu}" \
  -nographic \
  -no-reboot \
  -no-shutdown \
  -kernel "${kernel}" \
  -append "${kernel_args}" \
  "$@" >"${log}" 2>&1 &
pid="$!"

deadline=$((SECONDS + timeout_seconds))
while kill -0 "${pid}" 2>/dev/null; do
  if grep -Fq "${expect}" "${log}"; then
    exit 0
  fi
  if ((SECONDS >= deadline)); then
    echo "timed out after ${timeout_seconds}s waiting for ${expect}" >&2
    cat "${log}" >&2
    exit 1
  fi
  sleep 1
done

if grep -Fq "${expect}" "${log}"; then
  exit 0
fi

echo "qemu exited before expected boot output appeared: ${expect}" >&2
cat "${log}" >&2
exit 1
