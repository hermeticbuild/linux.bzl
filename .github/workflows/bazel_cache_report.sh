#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 4 ]]; then
  echo "usage: $0 TITLE PROFILE BEP LOG" >&2
  exit 2
fi

title="$1"
profile="$2"
bep="$3"
log="$4"
summary="${GITHUB_STEP_SUMMARY:-/dev/stdout}"

write_missing_metrics() {
  {
    printf '### %s\n\n' "${title}"
    printf 'Bazel did not produce cache metrics. Check the build log artifact for the failure.\n'
  } >>"${summary}"
}

if [[ ! -s "${bep}" ]]; then
  write_missing_metrics
  exit 0
fi

metrics="$(
  jq -c \
    'select(.buildMetrics.actionSummary != null) | .buildMetrics.actionSummary' \
    "${bep}" 2>/dev/null | tail -n 1 || true
)"
if [[ -z "${metrics}" ]]; then
  write_missing_metrics
  exit 0
fi

values="$(
  jq -r '
    def integer: (. // 0) | tonumber;

    (.actionCacheStatistics.hits | integer) as $local_hits
    | (.actionCacheStatistics.misses | integer) as $local_misses
    | ([.runnerCount[]?
        | select((.name // "" | ascii_downcase) | contains("cache hit"))
        | (.count | integer)] | add // 0) as $runner_cache_hits
    | (.remoteCacheHits | integer) as $legacy_remote_hits
    | [
        $local_hits,
        $local_misses,
        ([$runner_cache_hits, $legacy_remote_hits] | max),
        (.actionsCreated | integer),
        (.actionsExecuted | integer)
      ]
    | @tsv
  ' <<<"${metrics}"
)"
IFS=$'\t' read -r local_hits local_misses shared_hits actions_created actions_executed <<<"${values}"

cache_lookups=$((local_hits + local_misses))
effective_hits=$((local_hits + shared_hits))
effective_misses=$((local_misses - shared_hits))
if ((effective_misses < 0)); then
  effective_misses=0
fi

if ((cache_lookups == 0)); then
  hit_rate="n/a"
else
  hit_rate="$(
    awk -v hits="${effective_hits}" -v lookups="${cache_lookups}" \
      'BEGIN { printf "%.1f%%", (100 * hits) / lookups }'
  )"
fi

miss_reasons="$(
  jq -r '
    [
      .actionCacheStatistics.missDetails[]?
      | select(((.count // 0) | tonumber) > 0)
      | "\(.reason // "DIFFERENT_ACTION_KEY"): \(.count)"
    ]
    | join(", ")
  ' <<<"${metrics}"
)"
runner_summary="$(
  jq -r '
    [.runnerCount[]? | "\(.name): \(.count)"]
    | join(", ")
  ' <<<"${metrics}"
)"
process_summary="$(
  grep -E '^INFO: .* processes:' "${log}" 2>/dev/null | tail -n 1 || true
)"
disk_cache_size="$(
  du -sh "${HOME}/.cache/bazel-disk" 2>/dev/null | awk '{ print $1 }' || true
)"
disk_available="$(
  df -h --output=avail "${GITHUB_WORKSPACE:-.}" 2>/dev/null |
    tail -n 1 |
    tr -d ' ' || true
)"

{
  printf '### %s\n\n' "${title}"
  printf '| Metric | Value |\n'
  printf '| --- | ---: |\n'
  printf '| Local action-cache hits | %s |\n' "${local_hits}"
  printf '| Disk/remote cache hits | %s |\n' "${shared_hits}"
  printf '| Cache misses after disk/remote lookup | %s |\n' "${effective_misses}"
  printf '| Effective action-cache hit rate | %s |\n' "${hit_rate}"
  printf '| Actions created | %s |\n' "${actions_created}"
  printf '| Actions executed | %s |\n' "${actions_executed}"
  if [[ -n "${disk_cache_size}" ]]; then
    printf '| Disk cache on runner | %s |\n' "${disk_cache_size}"
  fi
  if [[ -n "${disk_available}" ]]; then
    printf '| Runner disk available | %s |\n' "${disk_available}"
  fi
  if [[ -n "${miss_reasons}" ]]; then
    printf "\nLocal cache miss reasons: \`%s\`\n" "${miss_reasons}"
  fi
  if [[ -n "${runner_summary}" ]]; then
    printf "\nExecution runners: \`%s\`\n" "${runner_summary}"
  fi
  if [[ -n "${process_summary}" ]]; then
    printf "\nBazel process summary: \`%s\`\n" "${process_summary#INFO: }"
  fi
  if [[ -s "${profile}" ]]; then
    printf "\nTiming profile: \`%s\`\n" "$(basename "${profile}")"
  fi
} >>"${summary}"
