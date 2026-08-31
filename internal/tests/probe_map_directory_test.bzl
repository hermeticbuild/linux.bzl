"""Focused tests for path-only symbolic probe map expansion."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts", "unittest")
load(
    "//internal:probe_map_directory.bzl",
    "linux_probe_map_directory_params",
    "linux_probe_map_directory_tools",
    "linux_test_index_probe_source_paths",
    "linux_test_parse_probe_marker_paths",
    "linux_test_probe_execution_root_marker",
    "linux_test_probe_topological_order",
    "linux_test_render_probe_action_value",
    "linux_test_resolve_probe_source_paths",
    "linux_test_select_prior_host_result_paths",
    "linux_test_validate_probe_additional_input_names",
)

visibility("private")

_SENTINEL = "__LINUX_BZL_KBUILD_ARGS_V1__"
_HOST = "a" * 64
_TARGET = "b" * 64
_FINAL = "c" * 64
_HOST_REQUEST = "d" * 64
_TARGET_REQUEST = "e" * 64
_FINAL_REQUEST = "f" * 64

_ProbeActionPathInfo = provider(
    doc = "Configured and source path values captured by the probe expansion fixture.",
    fields = {
        "configured_output": "Configured output artifact.",
        "configured_output_value": "Rendered configured output path.",
        "source_directory": "Declared probe source directory.",
        "source_directory_value": "Rendered probe source directory path.",
    },
)

def _valid_paths():
    return [
        "schema/linux-probe-plan-v2",
        "toolsets/host/sha256-%s" % ("1" * 64),
        "toolsets/target/sha256-%s" % ("2" * 64),
        "requests/%s.json" % _HOST_REQUEST,
        "requests/%s.json" % _TARGET_REQUEST,
        "requests/%s.json" % _FINAL_REQUEST,
        "nodes/%s/scope/host" % _HOST,
        "nodes/%s/request/%s" % (_HOST, _HOST_REQUEST),
        "nodes/%s/tool/cc" % _HOST,
        "nodes/%s/scope/target" % _TARGET,
        "nodes/%s/request/%s" % (_TARGET, _TARGET_REQUEST),
        "nodes/%s/tool/cc" % _TARGET,
        "nodes/%s/source/+Kconfig/path" % _TARGET,
        "nodes/%s/source/+scripts/+probe.sh/path" % _TARGET,
        "nodes/%s/source/+scripts/+probe.sh/+path/path" % _TARGET,
        "nodes/%s/source_root/linux" % _TARGET,
        "nodes/%s/in/00000000/%s" % (_TARGET, _HOST),
        "nodes/%s/scope/target" % _FINAL,
        "nodes/%s/request/%s" % (_FINAL, _FINAL_REQUEST),
        "nodes/%s/tool/ld" % _FINAL,
        "nodes/%s/source/+external/+rust-src/+library/+core/+src/+lib.rs/path" % _FINAL,
        "nodes/%s/source_root/rust" % _FINAL,
        "nodes/%s/in/00000000/%s" % (_FINAL, _TARGET),
        "terminal/%s" % _FINAL,
    ]

def _target_only_paths():
    paths = _valid_paths()
    for path in [
        "toolsets/host/sha256-%s" % ("1" * 64),
        "requests/%s.json" % _HOST_REQUEST,
        "nodes/%s/scope/host" % _HOST,
        "nodes/%s/request/%s" % (_HOST, _HOST_REQUEST),
        "nodes/%s/tool/cc" % _HOST,
        "nodes/%s/in/00000000/%s" % (_TARGET, _HOST),
    ]:
        paths.remove(path)
    return paths

def _rust_root_only_paths():
    paths = _valid_paths()
    paths.remove("nodes/%s/source/+external/+rust-src/+library/+core/+src/+lib.rs/path" % _FINAL)
    return paths

def _probe_map_directory_test_impl(ctx):
    env = unittest.begin(ctx)
    parsed = linux_test_parse_probe_marker_paths(_valid_paths())
    asserts.equals(env, [_HOST, _TARGET, _FINAL], parsed.order)
    asserts.equals(env, "host", parsed.nodes[_HOST]["scope"])
    asserts.equals(env, _HOST, parsed.nodes[_TARGET]["inputs"]["00000000"])
    asserts.equals(env, True, "Kconfig" in parsed.nodes[_TARGET]["sources"])
    asserts.equals(env, True, "scripts/probe.sh" in parsed.nodes[_TARGET]["sources"])
    asserts.equals(env, True, "scripts/probe.sh/path" in parsed.nodes[_TARGET]["sources"])
    asserts.equals(env, True, "linux" in parsed.nodes[_TARGET]["source_roots"])
    asserts.equals(env, True, "rust" in parsed.nodes[_FINAL]["source_roots"])
    asserts.equals(env, True, _FINAL in parsed.terminals)
    asserts.equals(
        env,
        [_HOST],
        linux_test_select_prior_host_result_paths("target", parsed, ["results/%s.json" % _HOST]),
    )

    # The staged Kbuild topology always gives its target map the host result
    # tree. A plan with no host probes must accept that tree only while empty.
    target_only = linux_test_parse_probe_marker_paths(_target_only_paths())
    asserts.equals(env, [], linux_test_select_prior_host_result_paths("target", target_only, []))

    params = linux_probe_map_directory_params(
        "target",
        {
            "cc": ["prefix", _SENTINEL, "suffix"],
            "pahole": [],
        },
        {
            "cc": {"ZED": "last", "ALPHA": "first"},
            "pahole": {},
        },
        source_prefix = "../linux-source",
        rust_source_root = "external/rust-src/library",
    )
    asserts.equals(env, "target", params["scope"])
    asserts.equals(env, "../linux-source", params["source_prefix"])
    asserts.equals(env, "external/rust-src/library", params["rust_source_root"])
    asserts.equals(env, "3", params["action_arg_count_cc"])
    asserts.equals(env, _SENTINEL, params["action_arg_cc_1"])
    asserts.equals(env, "ALPHA=first", params["action_env_cc_0"])
    asserts.equals(env, "ZED=last", params["action_env_cc_1"])
    bootstrap_params = linux_probe_map_directory_params("host", {}, {})
    asserts.equals(env, "", bootstrap_params["source_prefix"])
    asserts.equals(env, "", bootstrap_params["rust_source_root"])
    bootstrap_sources = linux_test_validate_probe_additional_input_names([])
    asserts.equals(env, {}, bootstrap_sources.files)

    tools = linux_probe_map_directory_tools(
        "exact-runner",
        {"cc": "exact-cc", "ld": "exact-ld"},
        "exact-toolchain-closure",
        "exact-toolset-manifest",
        {"root-00000000": "exact-toolset-anchor"},
    )
    asserts.equals(env, "exact-runner", tools["probe_runner"])
    asserts.equals(env, "exact-toolchain-closure", tools["toolchain_files"])
    asserts.equals(env, "exact-toolset-manifest", tools["toolset_manifest"])
    asserts.equals(env, "exact-toolset-anchor", tools["toolset_anchor_root-00000000"])
    asserts.equals(env, "exact-cc", tools["probe_role_cc"])
    asserts.equals(env, "exact-ld", tools["probe_role_ld"])

    indexed = linux_test_index_probe_source_paths(
        [
            "repo/Kconfig",
            "repo/scripts/probe.sh",
            "repo/scripts/probe.sh/path",
        ],
        "repo",
        rust_paths = ["../rust-src/library/core/src/lib.rs"],
        rust_source_root = "external/rust-src/library",
    )
    asserts.equals(env, [
        "Kconfig",
        "external/rust-src/library/core/src/lib.rs",
        "scripts/probe.sh",
        "scripts/probe.sh/path",
    ], indexed)
    target_sources = linux_test_resolve_probe_source_paths(
        parsed,
        _TARGET,
        [
            "repo/Kconfig",
            "repo/scripts/probe.sh",
            "repo/scripts/probe.sh/path",
        ],
        "repo",
        rust_paths = ["../rust-src/library/core/src/lib.rs"],
        rust_source_root = "external/rust-src/library",
    )
    asserts.equals(env, "repo/Kconfig", target_sources.linux_anchor)
    asserts.equals(env, "repo/Kconfig", target_sources.sources["Kconfig"])
    asserts.equals(env, "repo/scripts/probe.sh", target_sources.sources["scripts/probe.sh"])
    asserts.equals(env, "repo/scripts/probe.sh/path", target_sources.sources["scripts/probe.sh/path"])
    final_sources = linux_test_resolve_probe_source_paths(
        parsed,
        _FINAL,
        ["repo/Kconfig"],
        "repo",
        rust_paths = ["../rust-src/library/core/src/lib.rs"],
        rust_source_root = "external/rust-src/library",
    )
    asserts.equals(env, "external/rust-src/library", final_sources.rust_root)
    asserts.equals(env, "../rust-src/library/core/src/lib.rs", final_sources.rust_witness)
    asserts.equals(
        env,
        "../rust-src/library/core/src/lib.rs",
        final_sources.sources["external/rust-src/library/core/src/lib.rs"],
    )
    root_only = linux_test_resolve_probe_source_paths(
        linux_test_parse_probe_marker_paths(_rust_root_only_paths()),
        _FINAL,
        ["repo/Kconfig"],
        "repo",
        rust_paths = [
            "../rust-src/library/std/src/lib.rs",
            "../rust-src/library/core/src/lib.rs",
        ],
        rust_source_root = "external/rust-src/library",
    )
    asserts.equals(env, {}, root_only.sources)
    asserts.equals(env, "../rust-src/library/core/src/lib.rs", root_only.rust_witness)

    # Keep the callback scheduler linear for the adversarial ordering where
    # every consumer sorts before its producer. The old fixed-point scan made
    # this graph perform one full node scan per dependency level.
    chain_size = 2048
    chain_ids = ["0" * (64 - len(str(index))) + str(index) for index in range(chain_size)]
    chain = {}
    for index, node_id in enumerate(chain_ids):
        chain[node_id] = {
            "inputs": {} if index + 1 == chain_size else {"00000000": chain_ids[index + 1]},
        }
    chain_order = linux_test_probe_topological_order(chain)
    asserts.equals(env, chain_size, len(chain_order))
    asserts.equals(env, chain_ids[-1], chain_order[0])
    asserts.equals(env, chain_ids[0], chain_order[-1])
    return unittest.end(env)

_probe_map_directory_test = unittest.make(_probe_map_directory_test_impl)

def probe_map_directory_test(name):
    _probe_map_directory_test(name = name)

def _probe_action_path_subject_impl(ctx):
    configured_output = ctx.actions.declare_file(ctx.label.name + ".cfg-tool")
    ctx.actions.write(configured_output, "configured output\n")
    source_directory = ctx.file.source.path.rsplit("/", 1)[0]
    artifacts = [configured_output, ctx.file.source]
    return [
        DefaultInfo(files = depset([configured_output])),
        _ProbeActionPathInfo(
            configured_output = configured_output,
            configured_output_value = linux_test_render_probe_action_value(
                "--configured-tool=" + configured_output.path,
                artifacts,
            ),
            source_directory = source_directory,
            source_directory_value = linux_test_render_probe_action_value(
                "-I" + source_directory,
                artifacts,
            ),
        ),
    ]

_probe_action_path_subject = rule(
    implementation = _probe_action_path_subject_impl,
    attrs = {
        "source": attr.label(
            allow_single_file = True,
            mandatory = True,
        ),
    },
)

def _probe_action_path_rendering_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[_ProbeActionPathInfo]
    marker = linux_test_probe_execution_root_marker() + "/"
    asserts.false(env, info.configured_output.is_source)
    asserts.true(env, info.configured_output.path.startswith("bazel-out/"))
    asserts.equals(env, [
        "--configured-tool=",
        marker,
        info.configured_output,
    ], info.configured_output_value.fragments)
    asserts.equals(env, [
        "-I",
        marker,
        info.source_directory,
    ], info.source_directory_value.fragments)
    return analysistest.end(env)

_probe_action_path_rendering_test = analysistest.make(_probe_action_path_rendering_test_impl)

def probe_map_directory_path_rendering_test(name):
    subject = name + "_subject"
    _probe_action_path_subject(
        name = subject,
        source = ":toolchain_resource/lib/clang/22/include/stddef.h",
        tags = ["manual"],
    )
    _probe_action_path_rendering_test(
        name = name,
        target_under_test = ":" + subject,
    )

def _invalid_probe_plan_impl(ctx):
    paths = _valid_paths()
    if ctx.attr.case == "unknown_marker":
        paths.append("metadata/not-allowed")
    elif ctx.attr.case == "sparse_ordinal":
        paths.remove("nodes/%s/in/00000000/%s" % (_FINAL, _TARGET))
        paths.append("nodes/%s/in/00000001/%s" % (_FINAL, _TARGET))
    elif ctx.attr.case == "host_depends_target":
        paths.append("nodes/%s/in/00000000/%s" % (_HOST, _TARGET))
    elif ctx.attr.case == "cycle":
        paths.append("nodes/%s/in/00000001/%s" % (_TARGET, _FINAL))
    elif ctx.attr.case == "unknown_request":
        paths.remove("requests/%s.json" % _FINAL_REQUEST)
    elif ctx.attr.case == "unexpected_empty_plan_host_result":
        parsed = linux_test_parse_probe_marker_paths(_target_only_paths())
        linux_test_select_prior_host_result_paths("target", parsed, ["results/%s.json" % _HOST])
        return []
    elif ctx.attr.case == "malformed_source_marker":
        paths.remove("nodes/%s/source/+scripts/+probe.sh/path" % _TARGET)
        paths.append("nodes/%s/source/scripts/+probe.sh/path" % _TARGET)
    elif ctx.attr.case == "missing_source":
        parsed = linux_test_parse_probe_marker_paths(paths)
        linux_test_resolve_probe_source_paths(
            parsed,
            _TARGET,
            ["repo/Kconfig", "repo/scripts/probe.sh"],
            "repo",
            rust_paths = ["../rust-src/library/core/src/lib.rs"],
            rust_source_root = "external/rust-src/library",
        )
        return []
    elif ctx.attr.case == "ambiguous_source":
        linux_test_index_probe_source_paths(
            ["repo/Kconfig", "repo/scripts/probe.sh", "repo/scripts/probe.sh"],
            "repo",
        )
        return []
    elif ctx.attr.case == "outside_source_prefix":
        linux_test_index_probe_source_paths(["other/Kconfig"], "repo")
        return []
    elif ctx.attr.case == "unexpected_additional_input":
        linux_test_validate_probe_additional_input_names(["source_files", "source_root", "mystery"])
        return []
    linux_test_parse_probe_marker_paths(paths)
    return []

_invalid_probe_plan = rule(
    implementation = _invalid_probe_plan_impl,
    attrs = {"case": attr.string(mandatory = True)},
)

def _probe_map_directory_failure_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, ctx.attr.expected)
    return analysistest.end(env)

_probe_map_directory_failure_test = analysistest.make(
    _probe_map_directory_failure_test_impl,
    attrs = {"expected": attr.string(mandatory = True)},
    expect_failure = True,
)

def probe_map_directory_validation_test(name):
    cases = {
        "cycle": "contains a dependency cycle",
        "host_depends_target": "depends on target node",
        "malformed_source_marker": "has invalid source marker",
        "missing_source": "requests unavailable source",
        "ambiguous_source": "is provided by distinct artifacts",
        "outside_source_prefix": "is outside repo",
        "sparse_ordinal": "non-contiguous dependency ordinals",
        "unexpected_additional_input": "unexpected additional inputs",
        "unknown_marker": "contains unknown marker",
        "unknown_request": "references unknown request",
        "unexpected_empty_plan_host_result": "received host results for a plan with no host nodes",
    }
    tests = []
    for case, expected in cases.items():
        subject = name + "_" + case + "_subject"
        test = name + "_" + case
        _invalid_probe_plan(
            name = subject,
            case = case,
            tags = ["manual"],
        )
        _probe_map_directory_failure_test(
            name = test,
            expected = expected,
            target_under_test = ":" + subject,
        )
        tests.append(test)
    native.test_suite(name = name, tests = tests)
