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

build_metrics="$(
  jq -c \
    'select(.buildMetrics != null) | .buildMetrics' \
    "${bep}" 2>/dev/null | tail -n 1 || true
)"
if [[ -z "${build_metrics}" ]]; then
  write_missing_metrics
  exit 0
fi
metrics="$(jq -c '.actionSummary // {}' <<<"${build_metrics}")"

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

analysis_values="$(
  jq -r '
    def integer: (. // 0) | tonumber;
    [
      (.timingMetrics.analysisPhaseTimeInMs | integer),
      (.targetMetrics.targetsConfigured | integer),
      (.packageMetrics.packagesLoaded | integer),
      (.memoryMetrics.peakPostGcHeapSize | integer)
    ]
    | @tsv
  ' <<<"${build_metrics}"
)"
IFS=$'\t' read -r analysis_ms targets_configured packages_loaded peak_heap_bytes <<<"${analysis_values}"

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
    [.runnerCount[]? | select(.count != null) | "\(.name): \(.count)"]
    | join(", ")
  ' <<<"${metrics}"
)"
process_summary="$(
  grep -E '^INFO: .* processes:' "${log}" 2>/dev/null | tail -n 1 || true
)"
profile_values=""
if [[ -s "${profile}" ]]; then
  profile_values="$(
    gzip -cd "${profile}" 2>/dev/null |
      jq -r '
        [
          ([.traceEvents[]?
            | select(
                .cat == "Fetching repository"
                and ((.name // "") | contains("linux_image"))
              )
            | {
                name: .name,
                duration: (.dur // 0)
              }]
            | group_by(.name)
            | map([.[].duration] | max)
            | add // 0),
          ([.traceEvents[]?
            | select(.name == "Memory usage (Bazel)")
            | (.args.memory // 0)] | max // 0 | floor),
          ([.traceEvents[]?
            | select(.name == "_linux_object_impl")] | length),
          ([.traceEvents[]?
            | select(.name == "_linux_object_impl")
            | (.dur // 0)] | add // 0)
        ]
        | @tsv
      ' 2>/dev/null || true
  )"
fi
linux_image_fetch_us=0
profile_peak_memory_mib=0
linux_object_analyses=0
linux_object_analysis_us=0
if [[ -n "${profile_values}" ]]; then
  IFS=$'\t' read -r \
    linux_image_fetch_us \
    profile_peak_memory_mib \
    linux_object_analyses \
    linux_object_analysis_us <<<"${profile_values}"
fi
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
  if ((analysis_ms > 0)); then
    printf '| Analysis phase | %d.%03d s |\n' \
      "$((analysis_ms / 1000))" "$((analysis_ms % 1000))"
  fi
  if ((targets_configured > 0)); then
    printf '| Targets configured | %s |\n' "${targets_configured}"
  fi
  if ((packages_loaded > 0)); then
    printf '| Packages loaded | %s |\n' "${packages_loaded}"
  fi
  if ((peak_heap_bytes > 0)); then
    peak_heap_tenths=$((peak_heap_bytes * 10 / 1048576))
    printf '| Peak post-GC heap | %d.%d MiB |\n' \
      "$((peak_heap_tenths / 10))" "$((peak_heap_tenths % 10))"
  fi
  if ((profile_peak_memory_mib > 0)); then
    printf '| Peak Bazel memory (profile) | %s MiB |\n' "${profile_peak_memory_mib}"
  fi
  if ((linux_object_analyses > 0)); then
    printf '| Linux object analysis events (profile) | %s |\n' "${linux_object_analyses}"
    printf '| Linux object aggregate analysis duration (profile) | %d.%03d s |\n' \
      "$((linux_object_analysis_us / 1000000))" \
      "$((linux_object_analysis_us % 1000000 / 1000))"
  fi
  if ((linux_image_fetch_us > 0)); then
    printf '| Linux image repository generation | %d.%03d s |\n' \
      "$((linux_image_fetch_us / 1000000))" "$((linux_image_fetch_us % 1000000 / 1000))"
  fi
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
