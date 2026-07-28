#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 4 ]]; then
  echo "usage: $0 BEP METADATA EXPECTED_ACTIONS EXPECTED_MEMBERSHIPS" >&2
  exit 2
fi

bep="$1"
metadata="$2"
expected_actions="$3"
expected_memberships="$4"

expect() {
  local label="$1"
  local actual="$2"
  local expected="$3"
  if [[ "${actual}" != "${expected}" ]]; then
    echo "${label}: got ${actual}, expected ${expected}" >&2
    exit 1
  fi
}

if [[ ! -f "${metadata}" ]]; then
  echo "missing shared graph metadata: ${metadata}" >&2
  exit 1
fi

schema="$(jq -er '.schema' "${metadata}")"
if [[ "${schema}" != "v0.0.13" ]]; then
  echo "generated compact schema ${schema}, expected v0.0.13" >&2
  exit 1
fi

if ! graph_sets="$(
  jq -er '
    def raw_targets($doc; $name):
      [
        $doc.configs[]
        | select(.name == $name)
        | ((.object_targets // []) + (.module_object_targets // []))[]
      ];
    def set($targets):
      $targets | sort | unique;
    def shared($left; $right):
      ($right | reduce .[] as $target ({}; .[$target] = true)) as $right_set
      | [$left[] | select($right_set[.] == true)]
      | length;
    . as $document
    | ([$document.configs[].name] | sort | join(",")) as $names
    | raw_targets($document; "x86_64") as $base_raw
    | raw_targets($document; "lz4") as $lz4_raw
    | raw_targets($document; "debug") as $debug_raw
    | raw_targets($document; "btf") as $btf_raw
    | set($base_raw) as $base
    | set($lz4_raw) as $lz4
    | set($debug_raw) as $debug
    | set($btf_raw) as $btf
    | [
        $names,
        ($base_raw | length),
        ($base | length),
        ($lz4_raw | length),
        ($lz4 | length),
        ($debug_raw | length),
        ($debug | length),
        ($btf_raw | length),
        ($btf | length),
        ($base == $lz4),
        shared($base; $debug),
        shared($base; $btf),
        shared($lz4; $debug),
        shared($lz4; $btf),
        shared($debug; $btf),
        (($base + $lz4 + $debug + $btf) | unique | length),
        (
          ($base_raw | length) +
          ($lz4_raw | length) +
          ($debug_raw | length) +
          ($btf_raw | length)
        )
      ]
    | @tsv
  ' "${metadata}"
)"; then
  echo "invalid shared graph metadata: ${metadata}" >&2
  exit 1
fi
IFS=$'\t' read -r \
  config_names \
  base_raw base_count \
  lz4_raw lz4_count \
  debug_raw debug_count \
  btf_raw btf_count \
  base_lz4_equal \
  base_debug base_btf \
  lz4_debug lz4_btf \
  debug_btf \
  union_count membership_count <<<"${graph_sets}"

expect configs "${config_names}" "btf,debug,lz4,x86_64"
expect base_count "${base_raw}/${base_count}" "1152/1152"
expect lz4_count "${lz4_raw}/${lz4_count}" "1152/1152"
expect debug_count "${debug_raw}/${debug_count}" "1182/1182"
expect btf_count "${btf_raw}/${btf_count}" "1225/1225"
expect base_lz4_exact "${base_lz4_equal}" "true"
expect base_debug_shared "${base_debug}" "40"
expect base_btf_shared "${base_btf}" "0"
expect lz4_debug_shared "${lz4_debug}" "40"
expect lz4_btf_shared "${lz4_btf}" "0"
expect debug_btf_shared "${debug_btf}" "0"
expect union "${union_count}" "${expected_actions}"
expect memberships "${membership_count}" "${expected_memberships}"

if ! action_counts="$(
  jq -ser '
    [
      .[]?
      | .buildMetrics.actionSummary.actionData[]?
      | select(.mnemonic == "LinuxObjectCompile")
    ]
    | if length != 1 then
        error("expected exactly one LinuxObjectCompile metric")
      else
        [.[0].actionsCreated, .[0].actionsExecuted] | @tsv
      end
  ' "${bep}"
)"; then
  echo "invalid LinuxObjectCompile metrics: ${bep}" >&2
  exit 1
fi
IFS=$'\t' read -r actions_created actions_executed <<<"${action_counts}"
expect actions_created "${actions_created}" "${union_count}"

printf '%s memberships deduplicated to %s LinuxObjectCompile actions (%s executed)\n' \
  "${membership_count}" "${union_count}" "${actions_executed}"
