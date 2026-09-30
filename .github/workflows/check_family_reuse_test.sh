#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
fixture_tmp=$(mktemp -d)
trap 'rm -r -- "${fixture_tmp}"' EXIT

make_report() {
  local mode=$1
  local output=$2
  jq -n --arg mode "${mode}" '
    def basis_points($numerator; $denominator):
      if $denominator == 0 then 0
      else (($numerator * 10000 / $denominator) | floor)
      end;
    def kind_counts:
      sort_by(.kind) | group_by(.kind)
      | map({kind: .[0].kind, nodes: length});
    def members($nodes; $variant):
      [$nodes[] | select((.memberships | index($variant)) != null)];

    ["base", "btf", "debug", "lz4"] as $variant_names
    | (if $mode == "all-shared" then
         [{node_id: ("0" * 64), kind: "compile", memberships: $variant_names, typed_compiler: true, precise_compile: true}]
       elif $mode == "higher-order-baseline" then
         [
           {node_id: ("0" * 64), kind: "compile", memberships: ["btf", "debug", "lz4"], typed_compiler: true, precise_compile: true},
           {node_id: ("1" * 64), kind: "compile", memberships: ["btf", "debug", "lz4"], typed_compiler: true, precise_compile: true},
           {node_id: ("2" * 64), kind: "compile", memberships: ["btf", "debug", "lz4"], typed_compiler: true, precise_compile: true},
           {node_id: ("3" * 64), kind: "compile", memberships: ["base", "btf"], typed_compiler: true, precise_compile: true},
           {node_id: ("4" * 64), kind: "compile", memberships: ["base", "debug"], typed_compiler: true, precise_compile: true},
           {node_id: ("5" * 64), kind: "compile", memberships: ["base", "lz4"], typed_compiler: true, precise_compile: true},
           {node_id: ("6" * 64), kind: "compile", memberships: ["btf"], typed_compiler: true, precise_compile: true},
           {node_id: ("7" * 64), kind: "compile", memberships: ["debug"], typed_compiler: true, precise_compile: true},
           {node_id: ("8" * 64), kind: "compile", memberships: ["lz4"], typed_compiler: true, precise_compile: true}
         ]
       elif $mode == "lost-precise-membership" then
         [
           {node_id: ("0" * 64), kind: "compile", memberships: ["base", "btf", "debug", "lz4"], typed_compiler: true, precise_compile: true},
           {node_id: ("1" * 64), kind: "compile", memberships: ["btf", "debug"], typed_compiler: true, precise_compile: true},
           {node_id: ("2" * 64), kind: "compile", memberships: ["btf", "debug"], typed_compiler: true, precise_compile: true},
           {node_id: ("3" * 64), kind: "compile", memberships: ["btf", "lz4"], typed_compiler: true, precise_compile: true},
           {node_id: ("4" * 64), kind: "compile", memberships: ["btf", "lz4"], typed_compiler: true, precise_compile: true},
           {node_id: ("5" * 64), kind: "compile", memberships: ["debug", "lz4"], typed_compiler: true, precise_compile: true},
           {node_id: ("6" * 64), kind: "compile", memberships: ["debug", "lz4"], typed_compiler: true, precise_compile: true},
           {node_id: ("7" * 64), kind: "compile", memberships: ["base"], typed_compiler: true, precise_compile: true},
           {node_id: ("8" * 64), kind: "compile", memberships: ["base"], typed_compiler: true, precise_compile: true}
         ]
       else
         (if $mode == "lost-reuse" then
            [
              {node_id: ("0" * 64), kind: "compile", memberships: ["base"], typed_compiler: true, precise_compile: true},
              {node_id: ("1" * 64), kind: "compile", memberships: ["btf"], typed_compiler: true, precise_compile: true},
              {node_id: ("2" * 64), kind: "compile", memberships: ["debug"], typed_compiler: true, precise_compile: true},
              {node_id: ("3" * 64), kind: "compile", memberships: ["lz4"], typed_compiler: true, precise_compile: true}
            ]
          else
            [
              {node_id: ("0" * 64), kind: "compile", memberships: ["base", "lz4"], typed_compiler: true, precise_compile: true},
              {node_id: ("1" * 64), kind: "compile", memberships: ["btf"], typed_compiler: true, precise_compile: true},
              {node_id: ("2" * 64), kind: "compile", memberships: ["debug"], typed_compiler: true, precise_compile: true}
            ]
          end)
         + (if $mode == "lost-typed-sharing" then
              [
                {node_id: ("4" * 64), kind: "compile", memberships: ["base"], typed_compiler: true, precise_compile: false},
                {node_id: ("5" * 64), kind: "compile", memberships: ["lz4"], typed_compiler: true, precise_compile: false},
                {node_id: ("6" * 64), kind: "generate", memberships: ["base", "lz4"], typed_compiler: false, precise_compile: false}
              ]
            else
              [
                {node_id: ("4" * 64), kind: "compile", memberships: ["base", "lz4"], typed_compiler: true, precise_compile: false},
                {node_id: ("5" * 64), kind: "generate", memberships: ["base"], typed_compiler: false, precise_compile: false},
                {node_id: ("6" * 64), kind: "generate", memberships: ["lz4"], typed_compiler: false, precise_compile: false}
              ]
            end)
         + (if $mode == "runaway-work" then
              [{node_id: ("7" * 64), kind: "generate", memberships: ["base"], typed_compiler: false, precise_compile: false}]
            else [] end)
       end
       | sort_by(.node_id)) as $nodes
    | ([
        $variant_names[] as $name
        | members($nodes; $name) as $variant_members
        | {
            name: $name,
            nodes: ($variant_members | length),
            shared_nodes: ([$variant_members[] | select((.memberships | length) > 1)] | length),
            exclusive_nodes: ([$variant_members[] | select((.memberships | length) == 1)] | length),
            typed_compiler_nodes: ([$variant_members[] | select(.typed_compiler)] | length),
            precise_compile_nodes: ([$variant_members[] | select(.precise_compile)] | length),
            precise_compiler_coverage_basis_points: basis_points(
              ([$variant_members[] | select(.precise_compile)] | length);
              ([$variant_members[] | select(.typed_compiler)] | length)
            ),
            kinds: ($variant_members | kind_counts)
          }
      ]) as $variants
    | ($variants | map({key: .name, value: .}) | from_entries) as $variant_map
    | ([
        range(0; $variant_names | length) as $left_index
        | range($left_index + 1; $variant_names | length) as $right_index
        | $variant_names[$left_index] as $left
        | $variant_names[$right_index] as $right
        | members($nodes; $left) as $left_members
        | members($nodes; $right) as $right_members
        | [$nodes[] | select(
            (.memberships | index($left)) != null
            and (.memberships | index($right)) != null
          )] as $shared
        | [$left_members[] | select(.kind == "compile" or .kind == "archive")] as $eligible_left
        | [$right_members[] | select(.kind == "compile" or .kind == "archive")] as $eligible_right
        | [$shared[] | select(.kind == "compile" or .kind == "archive")] as $eligible_shared
        | [$shared[] | select(.typed_compiler)] as $typed_shared
        | [$shared[] | select(.precise_compile)] as $precise_shared
        | {
            left: $left,
            right: $right,
            left_nodes: ($left_members | length),
            right_nodes: ($right_members | length),
            shared_nodes: ($shared | length),
            left_reuse_basis_points: basis_points(($shared | length); ($left_members | length)),
            right_reuse_basis_points: basis_points(($shared | length); ($right_members | length)),
            eligible_left_nodes: ($eligible_left | length),
            eligible_right_nodes: ($eligible_right | length),
            eligible_shared_nodes: ($eligible_shared | length),
            eligible_left_reuse_basis_points: basis_points(($eligible_shared | length); ($eligible_left | length)),
            eligible_right_reuse_basis_points: basis_points(($eligible_shared | length); ($eligible_right | length)),
            typed_left_compiler_nodes: $variant_map[$left].typed_compiler_nodes,
            typed_right_compiler_nodes: $variant_map[$right].typed_compiler_nodes,
            typed_shared_compiler_nodes: ($typed_shared | length),
            typed_left_reuse_basis_points: basis_points(($typed_shared | length); $variant_map[$left].typed_compiler_nodes),
            typed_right_reuse_basis_points: basis_points(($typed_shared | length); $variant_map[$right].typed_compiler_nodes),
            precise_left_compile_nodes: $variant_map[$left].precise_compile_nodes,
            precise_right_compile_nodes: $variant_map[$right].precise_compile_nodes,
            precise_shared_compile_nodes: ($precise_shared | length),
            precise_left_reuse_basis_points: basis_points(($precise_shared | length); $variant_map[$left].precise_compile_nodes),
            precise_right_reuse_basis_points: basis_points(($precise_shared | length); $variant_map[$right].precise_compile_nodes),
            effective_precise_left_reuse_basis_points: basis_points(($precise_shared | length); $variant_map[$left].typed_compiler_nodes),
            effective_precise_right_reuse_basis_points: basis_points(($precise_shared | length); $variant_map[$right].typed_compiler_nodes)
          }
      ]) as $pairs
    | {
        schema: "linux-kernel-family-reuse-report-v2",
        toolsets: {host: ("sha256-" + ("a" * 64)), target: ("sha256-" + ("b" * 64))},
        nodes: $nodes,
        variants: $variants,
        membership_groups: ($nodes
          | group_by(.memberships)
          | map({variants: .[0].memberships, nodes: length, kinds: kind_counts})
          | sort_by(.variants)),
        pairs: $pairs,
        variant_instances: ([$nodes[] | (.memberships | length)] | add),
        unique_nodes: ($nodes | length),
        shared_nodes: ([$nodes[] | select((.memberships | length) > 1)] | length),
        reused_instances: ([$nodes[] | ((.memberships | length) - 1)] | add),
        opaque_reasons: [],
        precise_compiles: [$nodes[] | select(.precise_compile) | . as $node
          | ($node.node_id[0:8] + ".o") as $path
          | {
              node_id: $node.node_id,
              outputs: ["objects:" + $path],
              output_details: [{
                slot: 0,
                tree: "objects",
                logical_path: $path,
                artifact_path: $path,
                store_path: ("nodes/" + $node.node_id + "/00000000")
              }],
              memberships: $node.memberships
            }]
      }
  ' >"${output}"
}

make_report shared "${fixture_tmp}/shared.json"
make_report all-shared "${fixture_tmp}/all-shared.json"
make_report lost-reuse "${fixture_tmp}/lost-reuse.json"
make_report runaway-work "${fixture_tmp}/runaway-work.json"
make_report lost-typed-sharing "${fixture_tmp}/lost-typed-sharing.json"
make_report higher-order-baseline "${fixture_tmp}/higher-order-baseline.json"
make_report lost-precise-membership "${fixture_tmp}/lost-precise-membership.json"
jq '.precise_compiles[0].output_details[0].artifact_path = "../escape.o"' \
  "${fixture_tmp}/shared.json" >"${fixture_tmp}/invalid-artifact-path.json"
jq '.precise_compiles[0].node_id as $node_id
    | .precise_compiles[0].output_details[0].store_path = ("nodes/" + $node_id + "/00000001")' \
  "${fixture_tmp}/shared.json" >"${fixture_tmp}/invalid-store-path.json"

make_baseline() {
  local report=$1
  local output=$2
  jq '
    def expected_pairs:
      [
        "base/btf",
        "base/debug",
        "base/lz4",
        "btf/debug",
        "btf/lz4",
        "debug/lz4"
      ];
    def expected_shared_memberships:
      expected_pairs + [
        "base/btf/debug",
        "base/btf/lz4",
        "base/debug/lz4",
        "btf/debug/lz4",
        "base/btf/debug/lz4"
      ];
    def compiler_membership_nodes($report; $membership; $field):
      ($membership | split("/")) as $members
      | ([$report.nodes[]
          | select(.[$field])
          | . as $node
          | select(all($members[]; . as $member |
              ($node.memberships | index($member)) != null
            ))] | length);
    def membership_floors($report; $field):
      expected_shared_memberships
      | map(. as $membership
          | compiler_membership_nodes($report; $membership; $field) as $nodes
          | {key: $membership, value: $nodes})
      | from_entries;

    . as $report
    | {
      schema: "linux-kernel-family-reuse-baseline-v1",
      measurement_status: "measured",
      measured_bazel_versions: ["9.1.0", "9.2.0"],
      # Deliberately distinct fixture identities verify that equal graph
      # populations cannot mask a wrong compiler, version, or host toolset.
      measured_toolsets: {
        clang: {
          "9.1.0": {host: ("sha256-" + ("c" * 64)), target: ("sha256-" + ("d" * 64))},
          "9.2.0": $report.toolsets
        },
        gcc: {
          "9.1.0": {host: ("sha256-" + ("0" * 64)), target: ("sha256-" + ("1" * 64))},
          "9.2.0": {host: ("sha256-" + ("e" * 64)), target: ("sha256-" + ("f" * 64))}
        }
      },
      tolerances: {
        work_count_basis_points: 0,
        work_count_absolute_nodes: 0,
        reuse_rate_drop_basis_points: 0
      },
      compilers: {}
    }
    | . as $document
    | ($document + {
        compilers: {
          clang: {
            unique_nodes: $report.unique_nodes,
            variants: ($report.variants | map({
              key: .name,
              value: {
                nodes,
                typed_compiler_nodes,
                precise_compile_nodes
              }
            }) | from_entries),
            shared_typed_compiles: membership_floors($report; "typed_compiler"),
            shared_precise_compiles: membership_floors($report; "precise_compile")
          }
        }
      })
    | .compilers.gcc = .compilers.clang
  ' "${report}" >"${output}"
}

make_baseline "${fixture_tmp}/shared.json" "${fixture_tmp}/baseline.json"
make_baseline "${fixture_tmp}/all-shared.json" "${fixture_tmp}/all-shared-baseline.json"
make_baseline "${fixture_tmp}/higher-order-baseline.json" "${fixture_tmp}/higher-order-baseline-document.json"

validate() {
  validate_against "$1" "${fixture_tmp}/baseline.json"
}

validate_against() {
  local report=$1
  local baseline=$2
  local fixture_compiler=${3:-clang}
  local fixture_bazel_version=${4:-9.2.0}
  jq -e \
    --arg compiler "${fixture_compiler}" \
    --arg bazel_version "${fixture_bazel_version}" \
    --slurpfile baseline "${baseline}" \
    -f "${repo_root}/.github/workflows/check_family_reuse.jq" \
    "${report}" >/dev/null
}

expect_baseline_failure() {
  local report=$1
  local expected_metric=$2
  local baseline=${3:-${fixture_tmp}/baseline.json}
  local output
  if output=$(validate_against "${report}" "${baseline}" 2>&1); then
    echo "${expected_metric} regression unexpectedly passed the family-reuse baseline" >&2
    exit 1
  fi
  if [[ "${output}" != *"family-reuse baseline violations"* || "${output}" != *"${expected_metric}"* ]]; then
    echo "${expected_metric} regression failed for the wrong reason:" >&2
    echo "${output}" >&2
    exit 1
  fi
}

expect_schema_failure() {
  local report=$1
  local description=$2
  local output
  if output=$(validate "${report}" 2>&1); then
    echo "${description} unexpectedly passed family-reuse schema validation" >&2
    exit 1
  fi
  if [[ "${output}" != *"family-reuse precise compiler records are invalid"* ]]; then
    echo "${description} failed for the wrong reason:" >&2
    echo "${output}" >&2
    exit 1
  fi
}

expect_baseline_schema_failure() {
  local report=$1
  local baseline=$2
  local description=$3
  local output
  if output=$(validate_against "${report}" "${baseline}" 2>&1); then
    echo "${description} unexpectedly passed family-reuse baseline validation" >&2
    exit 1
  fi
  if [[ "${output}" != *"compiler family-reuse baseline is incomplete or invalid"* ]]; then
    echo "${description} failed for the wrong reason:" >&2
    echo "${output}" >&2
    exit 1
  fi
}

# Exact equality passes, including the explicitly recorded zero intersections.
jq -e 'all(.compilers[];
  all(.shared_typed_compiles, .shared_precise_compiles;
    length == 11 and any(.[]; . == 0) and any(.[]; . > 0)
  ))' "${fixture_tmp}/baseline.json" >/dev/null
validate "${fixture_tmp}/shared.json"
# Superset memberships contribute to every contained pair and triple. Assert
# actual numbers independently of the extractor/checker's shared algorithm.
jq -e 'all(.compilers[];
  all(.shared_typed_compiles, .shared_precise_compiles;
    length == 11 and all(.[]; . == 1)
  ))' "${fixture_tmp}/all-shared-baseline.json" >/dev/null
validate_against "${fixture_tmp}/all-shared.json" "${fixture_tmp}/all-shared-baseline.json"
jq -e '.compilers.clang.shared_typed_compiles["btf/debug"] == 3
    and .compilers.clang.shared_precise_compiles["btf/debug"] == 3' \
  "${fixture_tmp}/higher-order-baseline-document.json" >/dev/null
validate_against "${fixture_tmp}/higher-order-baseline.json" \
  "${fixture_tmp}/higher-order-baseline-document.json"
expect_baseline_failure "${fixture_tmp}/lost-reuse.json" "unique_nodes"
expect_baseline_failure "${fixture_tmp}/runaway-work.json" "variant_instances"
expect_baseline_failure "${fixture_tmp}/lost-typed-sharing.json" "shared_typed_compiles[base/lz4]"
expect_baseline_failure \
  "${fixture_tmp}/lost-precise-membership.json" \
  "shared_precise_compiles[btf/debug/lz4]" \
  "${fixture_tmp}/higher-order-baseline-document.json"
expect_schema_failure "${fixture_tmp}/invalid-artifact-path.json" "noncanonical artifact path"
expect_schema_failure "${fixture_tmp}/invalid-store-path.json" "store path for the wrong output slot"

for fixture_floor in shared_typed_compiles shared_precise_compiles; do
  while IFS= read -r fixture_membership; do
    jq --arg field "${fixture_floor}" --arg membership "${fixture_membership}" \
      'del(.compilers.clang[$field][$membership])' \
      "${fixture_tmp}/baseline.json" >"${fixture_tmp}/missing-membership-floor.json"
    expect_baseline_schema_failure "${fixture_tmp}/shared.json" \
      "${fixture_tmp}/missing-membership-floor.json" "missing ${fixture_floor}[${fixture_membership}]"
  done < <(jq -r --arg field "${fixture_floor}" \
    '.compilers.clang[$field] | keys[]' "${fixture_tmp}/baseline.json")

  for fixture_value in -1 0.5 '"1"' null true 99; do
    jq --arg field "${fixture_floor}" --argjson value "${fixture_value}" \
      '.compilers.clang[$field]["base/btf"] = $value' \
      "${fixture_tmp}/baseline.json" >"${fixture_tmp}/invalid-membership-floor.json"
    expect_baseline_schema_failure "${fixture_tmp}/shared.json" \
      "${fixture_tmp}/invalid-membership-floor.json" "${fixture_floor} value ${fixture_value}"
  done

  jq --arg field "${fixture_floor}" '.compilers.clang[$field]["lz4/base"] = 1' \
    "${fixture_tmp}/baseline.json" >"${fixture_tmp}/extra-membership-floor.json"
  expect_baseline_schema_failure "${fixture_tmp}/shared.json" \
    "${fixture_tmp}/extra-membership-floor.json" "noncanonical extra ${fixture_floor} key"

  jq --arg field "${fixture_floor}" '.compilers.clang[$field] |= with_entries(.value = 0)' \
    "${fixture_tmp}/baseline.json" >"${fixture_tmp}/all-zero-membership-floors.json"
  expect_baseline_schema_failure "${fixture_tmp}/shared.json" \
    "${fixture_tmp}/all-zero-membership-floors.json" "all-zero ${fixture_floor}"

  # A zero floor is valid when other intersections retain positive reuse.
  jq --arg field "${fixture_floor}" '.compilers.clang[$field]["base/lz4"] = 0' \
    "${fixture_tmp}/higher-order-baseline-document.json" >"${fixture_tmp}/zero-membership-floor.json"
  validate_against "${fixture_tmp}/higher-order-baseline.json" \
    "${fixture_tmp}/zero-membership-floor.json"
done

jq '.compilers.clang.shared_typed_compiles |= with_entries(.value = 0)
    | .compilers.clang.shared_precise_compiles |= with_entries(.value = 0)' \
  "${fixture_tmp}/baseline.json" >"${fixture_tmp}/all-zero-compiler-floors.json"
expect_baseline_schema_failure "${fixture_tmp}/shared.json" \
  "${fixture_tmp}/all-zero-compiler-floors.json" "all-zero compiler sharing floors"

# This report preserves pairwise counts but loses the positive triple floor.
# Deleting both triple keys used to bypass its only remaining regression gate.
jq 'del(.compilers.clang.shared_typed_compiles["btf/debug/lz4"],
        .compilers.clang.shared_precise_compiles["btf/debug/lz4"])' \
  "${fixture_tmp}/higher-order-baseline-document.json" >"${fixture_tmp}/deleted-triple-floors.json"
expect_baseline_schema_failure "${fixture_tmp}/lost-precise-membership.json" \
  "${fixture_tmp}/deleted-triple-floors.json" "deleted positive higher-order floors"

expect_toolset_failure() {
  local report=$1
  local baseline=$2
  local fixture_compiler=$3
  local fixture_bazel_version=$4
  local expected_message=$5
  local output
  if output=$(validate_against "${report}" "${baseline}" "${fixture_compiler}" "${fixture_bazel_version}" 2>&1); then
    echo "${expected_message} unexpectedly passed the toolset identity check" >&2
    exit 1
  fi
  if [[ "${output}" != *"${expected_message}"* ]]; then
    echo "toolset identity fixture failed for the wrong reason:" >&2
    echo "${output}" >&2
    exit 1
  fi
}

for fixture_compiler in clang gcc; do
  for fixture_bazel_version in 9.1.0 9.2.0; do
    jq \
      --arg compiler "${fixture_compiler}" \
      --arg bazel_version "${fixture_bazel_version}" \
      --slurpfile baseline "${fixture_tmp}/baseline.json" \
      '.toolsets = $baseline[0].measured_toolsets[$compiler][$bazel_version]' \
      "${fixture_tmp}/shared.json" >"${fixture_tmp}/selected-toolsets.json"
    validate_against "${fixture_tmp}/selected-toolsets.json" "${fixture_tmp}/baseline.json" \
      "${fixture_compiler}" "${fixture_bazel_version}"
  done
done

# The populations remain identical to the baseline throughout these cases.
expect_toolset_failure "${fixture_tmp}/shared.json" "${fixture_tmp}/baseline.json" gcc 9.2.0 \
  "toolset identities do not match measured gcc/Bazel 9.2.0"
expect_toolset_failure "${fixture_tmp}/shared.json" "${fixture_tmp}/baseline.json" clang 9.1.0 \
  "toolset identities do not match measured clang/Bazel 9.1.0"
for fixture_scope in host target; do
  jq --arg scope "${fixture_scope}" '.toolsets[$scope] = ("sha256-" + ("9" * 64))' \
    "${fixture_tmp}/shared.json" >"${fixture_tmp}/wrong-scope-identity.json"
  expect_toolset_failure "${fixture_tmp}/wrong-scope-identity.json" "${fixture_tmp}/baseline.json" clang 9.2.0 \
    "toolset identities do not match measured clang/Bazel 9.2.0"
done

for fixture_mutation in \
  'del(.measured_toolsets)' \
  'del(.measured_toolsets.gcc)' \
  'del(.measured_toolsets.gcc["9.1.0"])' \
  'del(.measured_toolsets.gcc["9.1.0"].host)' \
  '.measured_toolsets.gcc["9.1.0"].host = "not-a-digest"' \
  '.measured_toolsets.gcc["9.1.0"].extra = ("sha256-" + ("9" * 64))'; do
  jq "${fixture_mutation}" "${fixture_tmp}/baseline.json" >"${fixture_tmp}/invalid-measured-toolsets.json"
  expect_toolset_failure "${fixture_tmp}/shared.json" "${fixture_tmp}/invalid-measured-toolsets.json" clang 9.2.0 \
    "baseline must contain canonical measured toolsets for every compiler and Bazel version"
done

echo "family-reuse validator fixtures passed"
