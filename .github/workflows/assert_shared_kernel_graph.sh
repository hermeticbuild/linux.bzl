#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 5 ]]; then
  echo "usage: $0 BEP METADATA EXPECTED_ACTIONS EXPECTED_MEMBERSHIPS MAX_CONFIGURED_TARGETS" >&2
  exit 2
fi

bep="$1"
metadata="$2"
expected_actions="$3"
expected_memberships="$4"
max_configured_targets="$5"

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
expect base_count "${base_raw}/${base_count}" "1156/1156"
expect lz4_count "${lz4_raw}/${lz4_count}" "1156/1156"
expect debug_count "${debug_raw}/${debug_count}" "1186/1186"
expect btf_count "${btf_raw}/${btf_count}" "1229/1229"
expect base_lz4_exact "${base_lz4_equal}" "true"
expect base_debug_shared "${base_debug}" "38"
expect base_btf_shared "${base_btf}" "0"
expect lz4_debug_shared "${lz4_debug}" "38"
expect lz4_btf_shared "${lz4_btf}" "0"
expect debug_btf_shared "${debug_btf}" "0"
expect union "${union_count}" "${expected_actions}"
expect memberships "${membership_count}" "${expected_memberships}"

if ! action_group_stats="$(
  jq -er '
    . as $document
    | ($document.action_groups // error("missing action_groups")) as $groups
    | ([ $document.configs[].name ] | sort | unique) as $config_names
    | [
        $document.configs[] as $config
        | (($config.object_targets // []) + ($config.module_object_targets // []))[]
        | {config: $config.name, target: .}
      ]
      | sort_by(.target, .config)
      | group_by(.target)
      | map({
          configs: (map(.config) | sort | unique),
          target: .[0].target
        }) as $expected
    | [
        $groups[] as $group
        | $group.object_targets[]
        | {
            configs: $group.reachable_configs,
            group_id: $group.id,
            recipe_id: $group.recipe_id,
            target: .
          }
      ] as $owners
    | ([ $document.object_variants[].target ] | sort | unique) as $variant_targets
    | if ($groups | type) != "array" or ($groups | length) == 0 then
        error("action_groups must be a non-empty array")
      elif any($groups[]; (keys | sort) != ["id", "object_targets", "reachable_configs", "recipe_id"]) then
        error("action group has unsupported or missing fields")
      elif any($groups[]; (.id | test("^[0-9a-f]{64}$")) | not) then
        error("action group has invalid ID")
      elif any($groups[]; (.recipe_id | test("^[0-9a-f]{64}$")) | not) then
        error("action group has invalid recipe ID")
      elif ([ $groups[].id ] != ([ $groups[].id ] | sort)) then
        error("action groups are not sorted by ID")
      elif ([ $groups[].id ] | length) != ([ $groups[].id ] | unique | length) then
        error("action group IDs are not unique")
      elif any(
        $groups[];
        (.reachable_configs | length) == 0 or
        .reachable_configs != (.reachable_configs | sort | unique) or
        (.object_targets | length) == 0 or
        .object_targets != (.object_targets | sort | unique)
      ) then
        error("action group members are empty, duplicated, or unsorted")
      elif any($groups[].reachable_configs[]; . as $name | ($config_names | index($name)) == null) then
        error("action group references an unknown config")
      elif ([ $groups[] | [.recipe_id, .reachable_configs] | tojson ] | length) !=
           ([ $groups[] | [.recipe_id, .reachable_configs] | tojson ] | unique | length) then
        error("action groups repeat a recipe/reachability pair")
      elif any($owners[]; .target as $target | ($variant_targets | index($target)) == null) then
        error("action group references an unknown object target")
      elif ($owners | length) != ($owners | map(.target) | unique | length) then
        error("an object target has multiple action-group owners")
      elif (
        $owners
        | sort_by(.target)
        | map({configs, target})
      ) != $expected then
        error("action-group ownership or reachability does not match the concrete graph")
      elif any(
        $groups[];
        ((.reachable_configs | index("x86_64")) != null) !=
        ((.reachable_configs | index("lz4")) != null)
      ) then
        error("base and LZ4 do not share exact action-group ownership")
      else
        [
          ($groups | length),
          ([ $groups[].recipe_id ] | unique | length),
          ([ $groups[].reachable_configs | tojson ] | unique | length),
          ($owners | length),
          ([ $groups[].object_targets | length ] | max),
          ([ $groups[] | select((.reachable_configs | length) > 1) ] | length)
        ]
        | @tsv
      end
  ' "${metadata}"
)"; then
  echo "invalid concrete action-group metadata: ${metadata}" >&2
  exit 1
fi
IFS=$'\t' read -r \
  action_group_count recipe_count reachability_count \
  owned_object_count largest_action_group shared_action_groups <<<"${action_group_stats}"

if (( action_group_count > 200 )); then
  echo "action groups: got ${action_group_count}, maximum 200" >&2
  exit 1
fi
expect action_group_ownership "${owned_object_count}" "${union_count}"
if (( recipe_count == 0 || reachability_count == 0 || largest_action_group < 2 || shared_action_groups == 0 )); then
  echo "action groups did not demonstrate concrete recipe/reachability sharing" >&2
  exit 1
fi

if ! header_families="$(
  jq -er '
    def family_names($labels):
      [
        .generated_header_families[]
        | select(.labels == $labels)
        | .name
      ]
      | sort
      | join(",");
    [
      (.generated_header_families | length),
      (
        [.generated_header_families[].labels | join(",")]
        | unique
        | length
      ),
      family_names([
        "//:_base_x86_generated_headers",
        "//:_variant_btf_x86_generated_headers",
        "//:_variant_debug_x86_generated_headers",
        "//:_variant_lz4_x86_generated_headers"
      ]),
      family_names([
        "//:_base_x86_generated_headers",
        "//:_variant_btf_x86_generated_headers",
        "//:_variant_lz4_x86_generated_headers"
      ]),
      family_names([
        "//:_base_x86_generated_headers",
        "//:_variant_lz4_x86_generated_headers"
      ]),
      family_names(["//:_variant_btf_x86_generated_headers"]),
      family_names(["//:_variant_debug_x86_generated_headers"])
    ]
    | @tsv
  ' "${metadata}"
)"; then
  echo "invalid generated-header family metadata: ${metadata}" >&2
  exit 1
fi
IFS=$'\t' read -r \
  family_count family_partitions \
  shared_families base_btf_lz4_families \
  base_lz4_families btf_families debug_families <<<"${header_families}"

expect generated_header_families "${family_count}" "23"
expect generated_header_family_partitions "${family_partitions}" "5"
expect shared_generated_header_families \
  "${shared_families}" \
  "compile,cpufeatures,static,timeconst,utsversion,version"
expect base_btf_lz4_generated_header_families \
  "${base_btf_lz4_families}" \
  "utsrelease"
expect base_lz4_generated_header_families \
  "${base_lz4_families}" \
  "all,asm_offsets,bounds,kvm_offsets,rq_offsets"
expect btf_generated_header_families \
  "${btf_families}" \
  "all,asm_offsets,bounds,kvm_offsets,rq_offsets"
expect debug_generated_header_families \
  "${debug_families}" \
  "all,asm_offsets,bounds,kvm_offsets,rq_offsets,utsrelease"

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

if ! generated_header_actions="$(
  jq -ser '
    . as $events
    |
    def created($mnemonic):
      [
        $events[]?
        | .buildMetrics.actionSummary.actionData[]?
        | select(.mnemonic == $mnemonic)
        | .actionsCreated
      ]
      | if length != 1 then
          error("expected exactly one \($mnemonic) metric")
        else
          .[0]
        end;
    [
      "LinuxCPUFeatureMasks",
      "LinuxCompileHeader",
      "LinuxModuleOffsetsAsm",
      "LinuxModuleOffsetsHeader",
      "LinuxORCHash",
      "LinuxOffsetsAsm",
      "LinuxOffsetsHeader",
      "LinuxSyscallHeader",
      "LinuxSyscallTableHeader",
      "LinuxTimeconstHeader",
      "LinuxUTSReleaseHeader",
      "LinuxUTSVersionHeader",
      "LinuxVersionHeader",
      "LinuxXenHypercalls"
    ]
    | map("\(.)=\(created(.))")
    | join(",")
  ' "${bep}"
)"; then
  echo "invalid generated-header action metrics: ${bep}" >&2
  exit 1
fi
expect generated_header_actions \
  "${generated_header_actions}" \
  "LinuxCPUFeatureMasks=1,LinuxCompileHeader=1,LinuxModuleOffsetsAsm=3,LinuxModuleOffsetsHeader=3,LinuxORCHash=1,LinuxOffsetsAsm=12,LinuxOffsetsHeader=12,LinuxSyscallHeader=5,LinuxSyscallTableHeader=3,LinuxTimeconstHeader=1,LinuxUTSReleaseHeader=2,LinuxUTSVersionHeader=1,LinuxVersionHeader=1,LinuxXenHypercalls=1"

if ! configured_targets="$(
  jq -ser '
    [
      .[]?
      | .buildMetrics?
      | select(. != null)
      | .targetMetrics.targetsConfiguredNotIncludingAspects
    ]
    | if length != 1 then
        error("expected exactly one configured-target metric")
      else
        .[0]
      end
  ' "${bep}"
)"; then
  echo "invalid configured-target metrics: ${bep}" >&2
  exit 1
fi
if (( configured_targets > max_configured_targets )); then
  echo "configured targets: got ${configured_targets}, maximum ${max_configured_targets}" >&2
  exit 1
fi

printf '%s memberships deduplicated to %s LinuxObjectCompile actions (%s executed) in %s action groups; %s configured targets\n' \
  "${membership_count}" "${union_count}" "${actions_executed}" "${action_group_count}" "${configured_targets}"
