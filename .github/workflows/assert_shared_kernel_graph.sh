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

if ! metadata_protocol="$(jq -er '.protocol' "${metadata}")"; then
  echo "invalid metadata protocol: ${metadata}" >&2
  exit 1
fi
expect metadata_protocol "${metadata_protocol}" "compact-v7-lazy-action-graph"

if ! group_sets="$(
  jq -er '
      def config_names($document; $reachability):
        [
          $document.reachability_signatures[]
          | select(.id == $reachability)
          | .configs
        ][0];
      def groups($document; $name):
        [
          $document.action_recipe_groups[]
          | select((config_names($document; .reachability) | index($name)) != null)
          | .id
        ]
        | sort;
      def shared($left; $right):
        ($right | reduce .[] as $group ({}; .[$group] = true)) as $right_set
        | [$left[] | select($right_set[.] == true)]
        | length;
      . as $document
      | groups($document; "x86_64") as $base
      | groups($document; "lz4") as $lz4
      | groups($document; "debug") as $debug
      | groups($document; "btf") as $btf
      | [
          ($base | length),
          ($lz4 | length),
          ($debug | length),
          ($btf | length),
          ($base == $lz4),
          shared($base; $debug),
          shared($base; $btf),
          shared($debug; $btf),
          (($base + $lz4 + $debug + $btf) | unique | length),
          (
            [
              $document.action_recipe_groups[].objects[]
            ]
            | length
          )
      ]
      | @tsv
  ' "${metadata}"
)"; then
  echo "invalid lazy action-group metadata: ${metadata}" >&2
  exit 1
fi
IFS=$'\t' read -r \
  base_groups lz4_groups debug_groups btf_groups \
  base_lz4_groups_equal \
  base_debug_groups base_btf_groups debug_btf_groups \
  group_union group_objects <<<"${group_sets}"

expect base_groups "${base_groups}" "63"
expect lz4_groups "${lz4_groups}" "63"
expect debug_groups "${debug_groups}" "67"
expect btf_groups "${btf_groups}" "65"
expect base_lz4_group_identity "${base_lz4_groups_equal}" "true"
expect base_debug_shared_groups "${base_debug_groups}" "4"
expect base_btf_shared_groups "${base_btf_groups}" "0"
expect debug_btf_shared_groups "${debug_btf_groups}" "0"
expect group_union "${group_union}" "191"
expect group_object_variants "${group_objects}" "${expected_actions}"

# Resolve every configured target through its content-addressed object variant.
# The graph key mirrors the variant fields that define action-graph identity,
# so equal graph nodes with different target names or content IDs fail here.
if ! graph_identities="$(
  jq -er '
    def graph_key:
      [
        .object,
        .recipe,
        (.compile_environment // ""),
        (.action_source_group // ""),
        (.deps // []),
        (.members // [])
      ]
      | tojson;
    def targets($document; $name):
      [
        $document.configs[]
        | select(.name == $name)
        | ((.object_targets // []) + (.module_object_targets // []))[]
      ];
    def shared($left; $right):
      ($right | reduce .[] as $id ({}; .[$id] = true)) as $right_set
      | [$left[] | select($right_set[.] == true)]
      | length;
    . as $document
    | (
        reduce $document.object_variants[] as $variant
          ({};
            if has($variant.target) then
              error("duplicate object target " + $variant.target)
            else
              .[$variant.target] = $variant
            end
          )
      ) as $by_target
    | def content_ids($name):
        [
          targets($document; $name)[] as $target
          | ($by_target[$target] // error("missing object variant for " + $target))
          | .content_id
        ]
        | sort;
      ($document.object_variants | length) as $variants
    | ([$document.object_variants[].target] | unique | length) as $targets
    | ([$document.object_variants[].content_id] | unique | length) as $contents
    | ([$document.object_variants[] | graph_key] | unique | length) as $graph_keys
    | ([
        $document.configs[]
        | ((.object_targets // []) + (.module_object_targets // []))[]
      ]) as $memberships
    | content_ids("x86_64") as $base
    | content_ids("lz4") as $lz4
    | content_ids("debug") as $debug
    | content_ids("btf") as $btf
    | [
        $variants,
        $targets,
        $contents,
        $graph_keys,
        ($memberships | length),
        ($memberships | unique | length),
        ($base | length),
        ($lz4 | length),
        ($debug | length),
        ($btf | length),
        ($base == $lz4),
        shared($base; $debug),
        shared($base; $btf),
        shared($debug; $btf),
        (($base + $lz4 + $debug + $btf) | unique | length)
      ]
    | @tsv
  ' "${metadata}"
)"; then
  echo "invalid configured graph-target identities: ${metadata}" >&2
  exit 1
fi
IFS=$'\t' read -r \
  variant_count target_identity_count content_identity_count graph_key_count \
  identity_memberships identity_union \
  base_identities lz4_identities debug_identities btf_identities \
  base_lz4_content_equal \
  base_debug_content base_btf_content debug_btf_content \
  content_union <<<"${graph_identities}"

expect object_variant_identities \
  "${variant_count}/${target_identity_count}/${content_identity_count}/${graph_key_count}" \
  "${expected_actions}/${expected_actions}/${expected_actions}/${expected_actions}"
expect configured_graph_memberships \
  "${identity_memberships}/${identity_union}" \
  "${expected_memberships}/${expected_actions}"
expect base_configured_graph_targets "${base_identities}" "1152"
expect lz4_configured_graph_targets "${lz4_identities}" "1152"
expect debug_configured_graph_targets "${debug_identities}" "1182"
expect btf_configured_graph_targets "${btf_identities}" "1225"
expect base_lz4_graph_content_identity "${base_lz4_content_equal}" "true"
expect base_debug_shared_graph_content "${base_debug_content}" "40"
expect base_btf_shared_graph_content "${base_btf_content}" "0"
expect debug_btf_shared_graph_content "${debug_btf_content}" "0"
expect configured_graph_content_union "${content_union}" "${expected_actions}"

# Check both sides of interning: semantic payloads are unique in each table,
# while references from nodes and configured object recipes reuse those IDs.
if ! flag_dag="$(
  jq -er '
    def terminal_key:
      [(.argv // [])]
      | tojson;
    def probe_key:
      [
        .kind,
        (.candidate_argv // []),
        (.context_program // ""),
        (.language // ""),
        (.srcarch // "")
      ]
      | tojson;
    def node_key:
      [
        .kind,
        (.children // []),
        (.probe // ""),
        (.when_true // ""),
        (.when_false // "")
      ]
      | tojson;
    def program_key:
      [.root, (.effects // [])]
      | tojson;
    . as $document
    | (
        reduce ($document.flag_terminals[], $document.flag_nodes[]) as $value
          ({}; .[$value.id] = true)
      ) as $values
    | (
        reduce $document.flag_programs[] as $program
          ({}; .[$program.id] = true)
      ) as $programs
    | (
        reduce $document.kbuild_probes[] as $probe
          ({}; .[$probe.id] = true)
      ) as $probes
    | (
        reduce $document.action_recipes[] as $recipe
          ({}; .[$recipe.id] = $recipe)
      ) as $recipes
    | ([
        $document.flag_nodes[] as $node
        | ($node.children // [])[],
          ($node.when_true // empty),
          ($node.when_false // empty)
      ]) as $node_value_refs
    | ([$document.flag_programs[].root] + $node_value_refs) as $value_refs
    | ([$document.flag_nodes[] | .probe // empty]) as $probe_refs
    | ([
        $document.action_recipes[]
        | .flag_program,
          .remove_flag_program
      ] + [
        $document.kbuild_probes[]
        | .context_program // empty
      ]) as $declared_program_refs
    | ([
        $document.object_variants[]
        | ($recipes[.recipe] // error("missing action recipe " + .recipe))
        | .flag_program,
          .remove_flag_program
      ] + [
        $document.kbuild_probes[]
        | .context_program // empty
      ]) as $configured_program_refs
    | [
        ($document.kbuild_probes | length),
        ([$document.kbuild_probes[].id] | unique | length),
        ([$document.kbuild_probes[] | probe_key] | unique | length),
        ($document.flag_terminals | length),
        ([$document.flag_terminals[].id] | unique | length),
        ([$document.flag_terminals[] | terminal_key] | unique | length),
        ($document.flag_nodes | length),
        ([$document.flag_nodes[].id] | unique | length),
        ([$document.flag_nodes[] | node_key] | unique | length),
        ($document.flag_programs | length),
        ([$document.flag_programs[].id] | unique | length),
        ([$document.flag_programs[] | program_key] | unique | length),
        ($document.action_recipes | length),
        ([$value_refs[] | select($values[.] != true)] | length),
        ([$probe_refs[] | select($probes[.] != true)] | length),
        ([$declared_program_refs[] | select($programs[.] != true)] | length),
        ($value_refs | length),
        ($value_refs | unique | length),
        ($value_refs | group_by(.) | map(select(length > 1)) | length),
        ($configured_program_refs | length),
        ($configured_program_refs | unique | length),
        ($configured_program_refs | group_by(.) | map(select(length > 1)) | length)
      ]
    | @tsv
  ' "${metadata}"
)"; then
  echo "invalid lazy flag DAG metadata: ${metadata}" >&2
  exit 1
fi
IFS=$'\t' read -r \
  probe_count probe_id_count probe_payload_count \
  terminal_count terminal_id_count terminal_payload_count \
  node_count node_id_count node_payload_count \
  program_count program_id_count program_payload_count \
  recipe_count \
  missing_value_refs missing_probe_refs missing_program_refs \
  value_ref_count unique_value_refs reused_value_ids \
  configured_program_ref_count unique_configured_program_refs reused_program_ids \
  <<<"${flag_dag}"

expect lazy_probe_interning \
  "${probe_count}/${probe_id_count}/${probe_payload_count}" \
  "2/2/2"
expect lazy_terminal_interning \
  "${terminal_count}/${terminal_id_count}/${terminal_payload_count}" \
  "24/24/24"
expect lazy_node_interning \
  "${node_count}/${node_id_count}/${node_payload_count}" \
  "7/7/7"
expect lazy_program_interning \
  "${program_count}/${program_id_count}/${program_payload_count}" \
  "25/25/25"
expect lazy_action_recipes "${recipe_count}" "65"
expect lazy_dag_missing_references \
  "${missing_value_refs}/${missing_probe_refs}/${missing_program_refs}" \
  "0/0/0"
expect lazy_value_reference_reuse \
  "${value_ref_count}/${unique_value_refs}/${reused_value_ids}" \
  "42/31/6"
expect configured_flag_program_reuse \
  "${configured_program_ref_count}/${unique_configured_program_refs}/${reused_program_ids}" \
  "7040/25/25"

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

if ! flag_actions="$(
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
    [created("LinuxFlagSelect"), created("LinuxFlagConcat")]
    | @tsv
  ' "${bep}"
)"; then
  echo "invalid lazy flag action metrics: ${bep}" >&2
  exit 1
fi
IFS=$'\t' read -r flag_select_actions flag_concat_actions <<<"${flag_actions}"
expect flag_select_actions "${flag_select_actions}" "2"
expect flag_concat_actions "${flag_concat_actions}" "5"

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

printf '%s memberships deduplicated to %s LinuxObjectCompile actions (%s executed); %s configured targets\n' \
  "${membership_count}" "${union_count}" "${actions_executed}" "${configured_targets}"
