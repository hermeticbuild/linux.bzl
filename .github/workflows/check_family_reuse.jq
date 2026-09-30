# Validate both the reuse report's independently reconstructible evidence and
# the compact compiler-specific baseline supplied through --slurpfile baseline.
# The report describes one planned family graph; it intentionally contains no
# runtime cache-hit data.

def expected_variants:
  ["base", "btf", "debug", "lz4"];

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

def nonnegative_integer:
  type == "number" and . >= 0 and . == floor;

def positive_integer:
  nonnegative_integer and . > 0;

def canonical_toolset_identities($toolsets):
  ($toolsets | type) == "object"
  and ($toolsets | keys | sort) == ["host", "target"]
  and all($toolsets[];
    (type == "string") and test("^sha256-[0-9a-f]{64}$")
  );

def fields_are_nonnegative_integers($object; $fields):
  all($fields[]; . as $field | ($object[$field] | nonnegative_integer));

def basis_points($numerator; $denominator):
  if $denominator == 0 then
    0
  else
    (($numerator * 10000 / $denominator) | floor)
  end;

def ceil_div($numerator; $denominator):
  (($numerator + $denominator - 1) / $denominator | floor);

def count_slack($baseline; $tolerances):
  [
    $tolerances.work_count_absolute_nodes,
    ceil_div(
      $baseline * $tolerances.work_count_basis_points;
      10000
    )
  ] | max;

def within_count_tolerance($actual; $baseline; $tolerances):
  count_slack($baseline; $tolerances) as $slack
  | $actual >= ($baseline - $slack)
    and $actual <= ($baseline + $slack);

def floor_with_tolerance($baseline; $tolerance):
  [0, $baseline - $tolerance] | max;

def violation($condition; $message):
  if $condition then [] else [$message] end;

def kind_counts:
  group_by(.kind) | map({kind: .[0].kind, nodes: length});

def eligible_nodes($variant):
  ([$variant.kinds[]
    | select(.kind == "compile" or .kind == "archive")
    | .nodes] | add // 0);

def evidence_for_variant($report; $variant):
  [$report.nodes[] | select((.memberships | index($variant)) != null)];

def evidence_for_pair($report; $left; $right):
  [$report.nodes[]
    | select(
        (.memberships | index($left)) != null
        and (.memberships | index($right)) != null
      )];

def pair_shared_nodes($report; $left; $right):
  (evidence_for_pair($report; $left; $right) | length);

def pair_eligible_shared_nodes($report; $left; $right):
  ([evidence_for_pair($report; $left; $right)[]
    | select(.kind == "compile" or .kind == "archive")] | length);

def pair_typed_shared_nodes($report; $left; $right):
  ([evidence_for_pair($report; $left; $right)[]
    | select(.typed_compiler)] | length);

def pair_precise_shared_nodes($report; $left; $right):
  ([evidence_for_pair($report; $left; $right)[]
    | select(.precise_compile)] | length);

def compiler_membership_nodes($report; $membership; $field):
  ($membership | split("/")) as $members
  | ([$report.nodes[]
      | select(.[$field])
      | . as $node
      | select(all($members[]; . as $member |
          ($node.memberships | index($member)) != null
        ))] | length);

def canonical_compiler_membership_floors($floors):
  ($floors | type) == "object"
  and ($floors | keys) == (expected_shared_memberships | sort)
  and all($floors[]; nonnegative_integer)
  # Zero records an explicitly measured empty intersection; omission must not
  # silently disable its gate. Each map still needs meaningful positive reuse.
  and any($floors[]; positive_integer);

def canonical_variant_names($names):
  ($names | type) == "array"
  and ($names | length) > 0
  and $names == ($names | sort | unique)
  and all($names[]; . as $name | (expected_variants | index($name)) != null);

def canonical_kinds($kinds):
  ($kinds | type) == "array"
  and ($kinds | map(.kind)) == ($kinds | map(.kind) | sort | unique)
  and all($kinds[];
    ((. | keys | sort) == ["kind", "nodes"])
    and (.kind | type) == "string"
    and (.kind | length) > 0
    and (.nodes | positive_integer)
  );

def canonical_node_evidence($nodes):
  ($nodes | type) == "array"
  and ($nodes | map(.node_id)) == ($nodes | map(.node_id) | sort | unique)
  and all($nodes[];
    ((. | keys | sort) == ["kind", "memberships", "node_id", "precise_compile", "typed_compiler"])
    and (.node_id | type) == "string"
    and (.node_id | test("^[0-9a-f]{64}$"))
    and (.kind | type) == "string"
    and (.kind | length) > 0
    and canonical_variant_names(.memberships)
    and (.typed_compiler | type) == "boolean"
    and (.precise_compile | type) == "boolean"
    and ((.typed_compiler | not) or .kind == "compile")
    and ((.precise_compile | not) or .typed_compiler)
  );

def canonical_relative_path:
  type == "string"
  and length > 0
  and (startswith("/") | not)
  and (contains("\\") | not)
  and (contains("\u0000") | not)
  and (split("/") | all(. != "" and . != "." and . != ".."));

def plan_ordinal($slot):
  ($slot | tostring) as $value
  | "00000000"[0:([0, 8 - ($value | length)] | max)] + $value;

def canonical_precise_compile($record):
  (($record | keys | sort) == ["memberships", "node_id", "output_details", "outputs"])
  and ($record.node_id | type) == "string"
  and ($record.node_id | test("^[0-9a-f]{64}$"))
  and canonical_variant_names($record.memberships)
  and ($record.outputs | type) == "array"
  and ($record.output_details | type) == "array"
  and ($record.outputs | length) > 0
  and ($record.outputs | length) == ($record.output_details | length)
  and all($record.outputs[]; (type == "string") and length > 0)
  and all($record.output_details[];
    ((. | keys | sort) == ["artifact_path", "logical_path", "slot", "store_path", "tree"])
    and (.slot | nonnegative_integer)
    and (.tree | type) == "string" and (.tree | length) > 0
    and (.logical_path | canonical_relative_path)
    and (.artifact_path | canonical_relative_path)
    and (.store_path | canonical_relative_path)
    and .store_path == ("nodes/" + $record.node_id + "/" + plan_ordinal(.slot))
  )
  and ($record.output_details | map(.slot))
    == [range(0; ($record.output_details | length))]
  and $record.outputs
    == [$record.output_details[] | .tree + ":" + .logical_path];

def canonical_opaque_reasons($report):
  ($report.opaque_reasons | type) == "array"
  and all($report.opaque_reasons[];
    ((. | keys | sort) == ["kind", "nodes", "reason", "variant"])
    and (.variant | type) == "string"
    and (.variant as $variant | (expected_variants | index($variant)) != null)
    and (.kind | type) == "string" and (.kind | length) > 0
    and (.reason | type) == "string" and (.reason | length) > 0
    and (.nodes | positive_integer)
    and (. as $reason
      | ([evidence_for_variant($report; $reason.variant)[]
          | select(.kind == $reason.kind)] | length) >= $reason.nodes)
  )
  and ($report.opaque_reasons | map([.variant, .kind, .reason]))
    == ($report.opaque_reasons | map([.variant, .kind, .reason]) | sort | unique);

def require($condition; $message):
  if $condition then . else error($message) end;

def top_count_fields:
  [
    "variant_instances",
    "unique_nodes",
    "shared_nodes",
    "reused_instances"
  ];

def variant_count_fields:
  [
    "nodes",
    "shared_nodes",
    "exclusive_nodes",
    "typed_compiler_nodes",
    "precise_compile_nodes",
    "precise_compiler_coverage_basis_points"
  ];

def pair_count_fields:
  [
    "left_nodes",
    "right_nodes",
    "shared_nodes",
    "left_reuse_basis_points",
    "right_reuse_basis_points",
    "eligible_left_nodes",
    "eligible_right_nodes",
    "eligible_shared_nodes",
    "eligible_left_reuse_basis_points",
    "eligible_right_reuse_basis_points",
    "typed_left_compiler_nodes",
    "typed_right_compiler_nodes",
    "typed_shared_compiler_nodes",
    "typed_left_reuse_basis_points",
    "typed_right_reuse_basis_points",
    "precise_left_compile_nodes",
    "precise_right_compile_nodes",
    "precise_shared_compile_nodes",
    "precise_left_reuse_basis_points",
    "precise_right_reuse_basis_points",
    "effective_precise_left_reuse_basis_points",
    "effective_precise_right_reuse_basis_points"
  ];

if ($baseline | length) != 1 then
  error("expected exactly one family-reuse baseline document")
else
  .
end
| . as $report
| $baseline[0] as $baseline_document
| require(
    $report.schema == "linux-kernel-family-reuse-report-v2";
    "unexpected family-reuse report schema"
  )
| require(
    canonical_toolset_identities($report.toolsets);
    "family-reuse report must identify canonical host and target toolsets"
  )
| require(
    ($compiler == "clang" or $compiler == "gcc");
    "compiler must be clang or gcc"
  )
| require(
    ($bazel_version | type) == "string" and ($bazel_version | length) > 0;
    "Bazel version must be supplied"
  )
| require(
    $baseline_document.schema == "linux-kernel-family-reuse-baseline-v1";
    "unexpected family-reuse baseline schema"
  )
| require(
    $baseline_document.measurement_status == "measured";
    "family-reuse baseline must contain fresh measurements"
  )
| require(
    $baseline_document.measured_bazel_versions == ["9.1.0", "9.2.0"]
    and ($baseline_document.measured_bazel_versions | index($bazel_version)) != null;
    "family-reuse baseline does not cover this Bazel version"
  )
| require(
    ($baseline_document.tolerances | type) == "object"
    and ($baseline_document.tolerances | keys | sort) == [
      "reuse_rate_drop_basis_points",
      "work_count_absolute_nodes",
      "work_count_basis_points"
    ]
    and ($baseline_document.tolerances.work_count_basis_points | nonnegative_integer)
    and $baseline_document.tolerances.work_count_basis_points <= 10000
    and ($baseline_document.tolerances.work_count_absolute_nodes | nonnegative_integer)
    and ($baseline_document.tolerances.reuse_rate_drop_basis_points | nonnegative_integer)
    and $baseline_document.tolerances.reuse_rate_drop_basis_points <= 10000;
    "family-reuse baseline tolerances are invalid"
  )
| require(
    ($baseline_document.compilers | type) == "object"
    and ($baseline_document.compilers | keys | sort) == ["clang", "gcc"];
    "family-reuse baseline must contain exactly clang and gcc"
  )
| require(
    ($baseline_document.measured_toolsets | type) == "object"
    and ($baseline_document.measured_toolsets | keys | sort) == ["clang", "gcc"]
    and all($baseline_document.measured_toolsets[];
      (type == "object")
      and (keys | sort) == $baseline_document.measured_bazel_versions
      and all(.[]; canonical_toolset_identities(.))
    );
    "family-reuse baseline must contain canonical measured toolsets for every compiler and Bazel version"
  )
| require(
    $report.toolsets == $baseline_document.measured_toolsets[$compiler][$bazel_version];
    "family-reuse report toolset identities do not match measured \($compiler)/Bazel \($bazel_version)"
  )
| require(
    fields_are_nonnegative_integers($report; top_count_fields);
    "family-reuse top-level counts must be nonnegative integers"
  )
| require(
    ($report.variants | type) == "array"
    and ($report.pairs | type) == "array"
    and ($report.membership_groups | type) == "array"
    and ($report.precise_compiles | type) == "array"
    and ($report.opaque_reasons | type) == "array"
    and canonical_node_evidence($report.nodes);
    "family-reuse report collections or node evidence are invalid"
  )
| require(
    [$report.variants[].name] == expected_variants;
    "family-reuse report has an unexpected variant set or order"
  )
| require(
    [$report.pairs[] | .left + "/" + .right] == expected_pairs;
    "family-reuse report has an unexpected pair set or order"
  )
| require(
    all($report.variants[];
      fields_are_nonnegative_integers(.; variant_count_fields)
      and canonical_kinds(.kinds)
      and .nodes == ([.kinds[].nodes] | add // 0)
    );
    "family-reuse variant counts or kind partitions are invalid"
  )
| require(
    all($report.pairs[]; fields_are_nonnegative_integers(.; pair_count_fields));
    "family-reuse pair counts must be nonnegative integers"
  )
| require(
    all($report.membership_groups[];
      ((. | keys | sort) == ["kinds", "nodes", "variants"])
      and (.nodes | positive_integer)
      and canonical_variant_names(.variants)
      and canonical_kinds(.kinds)
      and .nodes == ([.kinds[].nodes] | add // 0)
    );
    "family-reuse membership groups are invalid"
  )
| require(
    ($report.membership_groups | map(.variants))
      == ($report.nodes | map(.memberships) | sort | unique)
    and all($report.membership_groups[]; . as $group |
      ([ $report.nodes[] | select(.memberships == $group.variants) ]) as $members
      | $group.nodes == ($members | length)
        and $group.kinds == ($members | kind_counts)
    );
    "family-reuse membership groups do not match node evidence"
  )
| require(
    all($report.precise_compiles[]; canonical_precise_compile(.));
    "family-reuse precise compiler records are invalid"
  )
| require(
    ($report.precise_compiles | map(.node_id))
      == ($report.precise_compiles | map(.node_id) | sort | unique)
    and ($report.precise_compiles | length)
      == ([$report.nodes[] | select(.precise_compile)] | length)
    and all($report.precise_compiles[]; . as $precise |
      ([$report.nodes[]
        | select(
            .node_id == $precise.node_id
            and .typed_compiler
            and .precise_compile
            and .memberships == $precise.memberships
          )] | length) == 1
    );
    "family-reuse precise compiler records do not match node evidence"
  )
| require(
    canonical_opaque_reasons($report);
    "family-reuse opaque reasons are invalid or not canonical"
  )
| require(
    $report.variant_instances
      == ([$report.nodes[] | (.memberships | length)] | add // 0)
    and $report.unique_nodes == ($report.nodes | length)
    and $report.shared_nodes
      == ([$report.nodes[] | select((.memberships | length) > 1)] | length)
    and $report.reused_instances
      == ([$report.nodes[] | ((.memberships | length) - 1)] | add // 0);
    "family-reuse top-level accounting does not match node evidence"
  )
| ($report.variants | map({key: .name, value: .}) | from_entries) as $variants
| require(
    all($report.variants[]; . as $variant |
      evidence_for_variant($report; $variant.name) as $members
      | $variant.nodes == ($members | length)
        and $variant.shared_nodes
          == ([$members[] | select((.memberships | length) > 1)] | length)
        and $variant.exclusive_nodes
          == ([$members[] | select((.memberships | length) == 1)] | length)
        and $variant.nodes == ($variant.shared_nodes + $variant.exclusive_nodes)
        and $variant.kinds == ($members | kind_counts)
        and $variant.typed_compiler_nodes
          == ([$members[] | select(.typed_compiler)] | length)
        and $variant.precise_compile_nodes
          == ([$members[] | select(.precise_compile)] | length)
        and $variant.precise_compile_nodes <= $variant.typed_compiler_nodes
        and $variant.precise_compiler_coverage_basis_points
          == basis_points($variant.precise_compile_nodes; $variant.typed_compiler_nodes)
    );
    "family-reuse per-variant accounting does not match node evidence"
  )
| require(
    all($report.pairs[]; . as $pair |
      $pair.left_nodes == $variants[$pair.left].nodes
      and $pair.right_nodes == $variants[$pair.right].nodes
      and $pair.shared_nodes == pair_shared_nodes($report; $pair.left; $pair.right)
      and $pair.left_reuse_basis_points
        == basis_points($pair.shared_nodes; $pair.left_nodes)
      and $pair.right_reuse_basis_points
        == basis_points($pair.shared_nodes; $pair.right_nodes)
      and $pair.eligible_left_nodes == eligible_nodes($variants[$pair.left])
      and $pair.eligible_right_nodes == eligible_nodes($variants[$pair.right])
      and $pair.eligible_shared_nodes
        == pair_eligible_shared_nodes($report; $pair.left; $pair.right)
      and $pair.eligible_left_reuse_basis_points
        == basis_points($pair.eligible_shared_nodes; $pair.eligible_left_nodes)
      and $pair.eligible_right_reuse_basis_points
        == basis_points($pair.eligible_shared_nodes; $pair.eligible_right_nodes)
      and $pair.typed_left_compiler_nodes == $variants[$pair.left].typed_compiler_nodes
      and $pair.typed_right_compiler_nodes == $variants[$pair.right].typed_compiler_nodes
      and $pair.typed_shared_compiler_nodes
        == pair_typed_shared_nodes($report; $pair.left; $pair.right)
      and $pair.typed_left_reuse_basis_points
        == basis_points($pair.typed_shared_compiler_nodes; $pair.typed_left_compiler_nodes)
      and $pair.typed_right_reuse_basis_points
        == basis_points($pair.typed_shared_compiler_nodes; $pair.typed_right_compiler_nodes)
      and $pair.precise_left_compile_nodes == $variants[$pair.left].precise_compile_nodes
      and $pair.precise_right_compile_nodes == $variants[$pair.right].precise_compile_nodes
      and $pair.precise_shared_compile_nodes
        == pair_precise_shared_nodes($report; $pair.left; $pair.right)
      and $pair.precise_left_reuse_basis_points
        == basis_points($pair.precise_shared_compile_nodes; $pair.precise_left_compile_nodes)
      and $pair.precise_right_reuse_basis_points
        == basis_points($pair.precise_shared_compile_nodes; $pair.precise_right_compile_nodes)
      and $pair.effective_precise_left_reuse_basis_points
        == basis_points($pair.precise_shared_compile_nodes; $pair.typed_left_compiler_nodes)
      and $pair.effective_precise_right_reuse_basis_points
        == basis_points($pair.precise_shared_compile_nodes; $pair.typed_right_compiler_nodes)
    );
    "family-reuse pair accounting does not match node evidence"
  )
| ($report.pairs
    | map({key: (.left + "/" + .right), value: .})
    | from_entries) as $pairs
| ($baseline_document.compilers[$compiler]) as $reference
| ($baseline_document.tolerances) as $tolerances
| require(
    ($reference | type) == "object"
    and ($reference | keys | sort) == [
      "shared_precise_compiles",
      "shared_typed_compiles",
      "unique_nodes",
      "variants"
    ]
    and ($reference.unique_nodes | positive_integer)
    and ($reference.variants | type) == "object"
    and ($reference.variants | keys | sort) == expected_variants
    and all($reference.variants[];
      (keys | sort) == ["nodes", "precise_compile_nodes", "typed_compiler_nodes"]
      and (.nodes | positive_integer)
      and (.typed_compiler_nodes | positive_integer)
      and (.precise_compile_nodes | positive_integer)
      and .precise_compile_nodes <= .typed_compiler_nodes
      and .typed_compiler_nodes <= .nodes
    )
    and canonical_compiler_membership_floors($reference.shared_typed_compiles)
    and all($reference.shared_typed_compiles | to_entries[]; . as $entry |
      ($entry.key | split("/")) as $members
      | $entry.value <= ($members | map(
          . as $member | $reference.variants[$member].typed_compiler_nodes
        ) | min)
    )
    and canonical_compiler_membership_floors($reference.shared_precise_compiles)
    and all($reference.shared_precise_compiles | to_entries[]; . as $entry |
      ($entry.key | split("/")) as $members
      | $entry.value <= ($members | map(
          . as $member | $reference.variants[$member].precise_compile_nodes
        ) | min)
    );
    "compiler family-reuse baseline is incomplete or invalid"
  )
| ([$reference.variants[].nodes] | add) as $baseline_variant_instances
| ($baseline_variant_instances - $reference.unique_nodes) as $baseline_reused_instances
| require(
    $baseline_reused_instances > 0;
    "compiler family-reuse baseline contains no reuse"
  )
| (violation(
    within_count_tolerance(
      $report.variant_instances;
      $baseline_variant_instances;
      $tolerances
    );
    "variant_instances: got \($report.variant_instances), baseline \($baseline_variant_instances)"
  )
  + violation(
    $report.unique_nodes <= (
      $reference.unique_nodes + count_slack($reference.unique_nodes; $tolerances)
    );
    "unique_nodes: got \($report.unique_nodes), baseline \($reference.unique_nodes)"
  )
  + violation(
    $report.reused_instances >= floor_with_tolerance(
      $baseline_reused_instances;
      count_slack($baseline_reused_instances; $tolerances)
    );
    "reused_instances: got \($report.reused_instances), baseline \($baseline_reused_instances)"
  )
  + violation(
    basis_points($report.reused_instances; $report.variant_instances)
      >= floor_with_tolerance(
        basis_points($baseline_reused_instances; $baseline_variant_instances);
        $tolerances.reuse_rate_drop_basis_points
      );
    "global reuse rate: got \(basis_points($report.reused_instances; $report.variant_instances)) basis points, baseline \(basis_points($baseline_reused_instances; $baseline_variant_instances))"
  )) as $top_violations
| ([
    expected_variants[] as $variant_name
    | ($variants[$variant_name]) as $actual
    | ($reference.variants[$variant_name]) as $expected
    | violation(
        within_count_tolerance($actual.nodes; $expected.nodes; $tolerances);
        "\($variant_name).nodes: got \($actual.nodes), baseline \($expected.nodes)"
      )
      + violation(
        within_count_tolerance(
          $actual.typed_compiler_nodes;
          $expected.typed_compiler_nodes;
          $tolerances
        );
        "\($variant_name).typed_compiler_nodes: got \($actual.typed_compiler_nodes), baseline \($expected.typed_compiler_nodes)"
      )
      + violation(
        $actual.precise_compile_nodes >= $expected.precise_compile_nodes;
        "\($variant_name).precise_compile_nodes: got \($actual.precise_compile_nodes), floor \($expected.precise_compile_nodes)"
      )
      + violation(
        $actual.precise_compiler_coverage_basis_points
          >= floor_with_tolerance(
            basis_points(
              $expected.precise_compile_nodes;
              $expected.typed_compiler_nodes
            );
            $tolerances.reuse_rate_drop_basis_points
          );
        "\($variant_name).precise compiler coverage: got \($actual.precise_compiler_coverage_basis_points) basis points, baseline \(basis_points($expected.precise_compile_nodes; $expected.typed_compiler_nodes))"
      )
  ] | add // []) as $variant_violations
| ([
    ($reference.shared_typed_compiles | to_entries[]) as $entry
    | compiler_membership_nodes($report; $entry.key; "typed_compiler") as $actual
    | violation(
        $actual >= $entry.value;
        "shared_typed_compiles[\($entry.key)]: got \($actual), floor \($entry.value)"
      )
  ] | add // []) as $typed_membership_violations
| ([
    ($reference.shared_precise_compiles | to_entries[]) as $entry
    | compiler_membership_nodes($report; $entry.key; "precise_compile") as $actual
    | violation(
        $actual >= $entry.value;
        "shared_precise_compiles[\($entry.key)]: got \($actual), floor \($entry.value)"
      )
  ] | add // []) as $precise_membership_violations
| ([
    ($reference.shared_precise_compiles | to_entries[]) as $entry
    | select(($entry.key | split("/") | length) == 2)
    | ($pairs[$entry.key]) as $actual
    | ($entry.key | split("/")) as $members
    | ($reference.variants[$members[0]]) as $left
    | ($reference.variants[$members[1]]) as $right
    | violation(
        $actual.effective_precise_left_reuse_basis_points
          >= floor_with_tolerance(
            basis_points($entry.value; $left.typed_compiler_nodes);
            $tolerances.reuse_rate_drop_basis_points
          );
        "\($entry.key).effective left reuse: got \($actual.effective_precise_left_reuse_basis_points) basis points, baseline \(basis_points($entry.value; $left.typed_compiler_nodes))"
      )
      + violation(
        $actual.effective_precise_right_reuse_basis_points
          >= floor_with_tolerance(
            basis_points($entry.value; $right.typed_compiler_nodes);
            $tolerances.reuse_rate_drop_basis_points
          );
        "\($entry.key).effective right reuse: got \($actual.effective_precise_right_reuse_basis_points) basis points, baseline \(basis_points($entry.value; $right.typed_compiler_nodes))"
      )
  ] | add // []) as $pair_violations
| ($top_violations
    + $variant_violations
    + $typed_membership_violations
    + $precise_membership_violations
    + $pair_violations) as $violations
| require(
    ($violations | length) == 0;
    "family-reuse baseline violations:\n" + ($violations | join("\n"))
  )
| true
