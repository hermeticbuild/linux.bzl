"""Analysis regressions for execution-configured optional Rust toolchains."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//internal:execution_platform.bzl", "linux_execution_platform_attr", "linux_execution_platform_label")
load(
    "//internal:rust_toolchain.bzl",
    "ExecutionOptionalToolchainInfo",
    "execution_bindgen_toolchain",
    "execution_bindgen_toolchain_attr",
    "execution_rust_source_toolchain",
    "execution_rust_source_toolchain_attr",
    "execution_rust_toolchain",
    "execution_rust_toolchain_attr",
    "execution_rust_toolchain_test_filter",
)

visibility("private")

_PIN_TOOLCHAIN_TYPE = str(Label("//internal/tests:rust_guard_pin_toolchain_type"))
_SECOND_PIN_TOOLCHAIN_TYPE = str(Label("//internal/tests:rust_guard_second_pin_toolchain_type"))
_FIRST_EXECUTION_PLATFORM = Label("//internal/tests:rust_guard_first_platform")
_SECOND_EXECUTION_PLATFORM = Label("//internal/tests:rust_guard_second_platform")
_FALLBACK_EXECUTION_PLATFORM = Label("//internal/tests:rust_guard_fallback_platform")

_OptionalToolchainGuardInfo = provider(
    doc = "The independently bridged optional toolchains for one pinned execution group.",
    fields = {
        "bindgen_execution_platform": "The execution platform selected by the bindgen resolver.",
        "bindgen_marker": "The accepted fake bindgen marker, or None.",
        "host_execution_platform": "The execution platform selected by the parent host group.",
        "rust_execution_platform": "The execution platform selected by the Rust compiler resolver.",
        "rust_marker": "The accepted fake Rust compiler marker, or None.",
        "rust_source_execution_platform": "The execution platform selected by the Rust source resolver.",
        "rust_source_marker": "The accepted fake Rust source marker, or None.",
        "synthetic_mismatch_marker": "The result of filtering a deliberately mismatched marked provider.",
    },
)

def _fake_rust_guard_toolchain_impl(ctx):
    return [platform_common.ToolchainInfo(marker = ctx.attr.marker)]

fake_rust_guard_toolchain = rule(
    implementation = _fake_rust_guard_toolchain_impl,
    attrs = {
        "marker": attr.string(mandatory = True),
    },
)

def _marker(toolchain):
    return getattr(toolchain, "marker", None) if toolchain != None else None

def _rust_toolchain_guard_probe_impl(ctx):
    host_execution_platform = linux_execution_platform_label(ctx.attr._host_execution_platform)
    rust_bridge = ctx.attr._host_rust_toolchain[ExecutionOptionalToolchainInfo]
    rust_source_bridge = ctx.attr._host_rust_source_toolchain[ExecutionOptionalToolchainInfo]
    bindgen_bridge = ctx.attr._host_bindgen_toolchain[ExecutionOptionalToolchainInfo]
    rust = execution_rust_toolchain(ctx, "_host_rust_toolchain", host_execution_platform)
    rust_source = execution_rust_source_toolchain(ctx, "_host_rust_source_toolchain", host_execution_platform)
    bindgen = execution_bindgen_toolchain(ctx, "_host_bindgen_toolchain", host_execution_platform)
    mismatched_platform = _SECOND_EXECUTION_PLATFORM if host_execution_platform == _FIRST_EXECUTION_PLATFORM else _FIRST_EXECUTION_PLATFORM
    mismatched = execution_rust_toolchain_test_filter(
        ExecutionOptionalToolchainInfo(
            execution_platform = mismatched_platform,
            toolchain = platform_common.ToolchainInfo(marker = "must-not-leak"),
        ),
        host_execution_platform,
    )
    return [_OptionalToolchainGuardInfo(
        bindgen_execution_platform = bindgen_bridge.execution_platform,
        bindgen_marker = _marker(bindgen),
        host_execution_platform = host_execution_platform,
        rust_execution_platform = rust_bridge.execution_platform,
        rust_marker = _marker(rust),
        rust_source_execution_platform = rust_source_bridge.execution_platform,
        rust_source_marker = _marker(rust_source),
        synthetic_mismatch_marker = _marker(mismatched),
    )]

def _rust_toolchain_guard_rule(host_pin_toolchain_type):
    return rule(
        implementation = _rust_toolchain_guard_probe_impl,
        attrs = {
            "_host_bindgen_toolchain": execution_bindgen_toolchain_attr(exec_group = "host_cc"),
            "_host_execution_platform": linux_execution_platform_attr(exec_group = "host_cc"),
            "_host_rust_source_toolchain": execution_rust_source_toolchain_attr(exec_group = "host_cc"),
            "_host_rust_toolchain": execution_rust_toolchain_attr(exec_group = "host_cc"),
        },
        # The parent chooses P from mandatory anchors alone. Each optional
        # toolchain is resolved independently only after entering config.exec(P).
        exec_groups = {
            "host_cc": exec_group(toolchains = [host_pin_toolchain_type]),
        },
    )

_rust_toolchain_first_probe = _rust_toolchain_guard_rule(_PIN_TOOLCHAIN_TYPE)
_rust_toolchain_second_probe = _rust_toolchain_guard_rule(_SECOND_PIN_TOOLCHAIN_TYPE)

_PIN_TOOLCHAINS = [
    str(Label("//internal/tests:rust_guard_pin_toolchain")),
    str(Label("//internal/tests:rust_guard_second_pin_toolchain")),
]

_RUST_CHANNEL_SETTING = str(Label("@rules_rust//rust/toolchain/channel:channel"))

_FIRST_CONFIG_SETTINGS = {
    _RUST_CHANNEL_SETTING: "nightly",
    "//command_line_option:extra_execution_platforms": [
        str(_FIRST_EXECUTION_PLATFORM),
        str(_SECOND_EXECUTION_PLATFORM),
    ],
    "//command_line_option:extra_toolchains": _PIN_TOOLCHAINS + [
        str(Label("//internal/tests:rust_guard_first_rust_toolchain")),
        str(Label("//internal/tests:rust_guard_first_rust_source_toolchain")),
        str(Label("//internal/tests:rust_guard_first_bindgen_toolchain")),
    ],
    "//command_line_option:platforms": str(_FIRST_EXECUTION_PLATFORM),
}

_COEXISTENCE_CONFIG_SETTINGS = {
    _RUST_CHANNEL_SETTING: "nightly",
    "//command_line_option:extra_execution_platforms": [
        str(_FIRST_EXECUTION_PLATFORM),
        str(_SECOND_EXECUTION_PLATFORM),
    ],
    "//command_line_option:extra_toolchains": _PIN_TOOLCHAINS + [
        str(Label("//internal/tests:rust_guard_first_rust_toolchain")),
        str(Label("//internal/tests:rust_guard_cross_rust_toolchain")),
        str(Label("//internal/tests:rust_guard_rust_toolchain")),
        str(Label("//internal/tests:rust_guard_cross_rust_source_toolchain")),
        str(Label("//internal/tests:rust_guard_rust_source_toolchain")),
        str(Label("//internal/tests:rust_guard_first_bindgen_toolchain")),
        str(Label("//internal/tests:rust_guard_bindgen_toolchain")),
    ],
    "//command_line_option:host_platform": str(_FALLBACK_EXECUTION_PLATFORM),
    "//command_line_option:platforms": str(_FIRST_EXECUTION_PLATFORM),
}

_REJECTION_CONFIG_SETTINGS = {
    _RUST_CHANNEL_SETTING: "nightly",
    "//command_line_option:extra_execution_platforms": [
        str(_FIRST_EXECUTION_PLATFORM),
        str(_SECOND_EXECUTION_PLATFORM),
    ],
    "//command_line_option:extra_toolchains": _PIN_TOOLCHAINS + [
        str(Label("//internal/tests:rust_guard_first_rust_toolchain")),
        str(Label("//internal/tests:rust_guard_rust_source_toolchain")),
        str(Label("//internal/tests:rust_guard_bindgen_toolchain")),
    ],
    "//command_line_option:platforms": str(_FIRST_EXECUTION_PLATFORM),
}

_CROSS_ONLY_CONFIG_SETTINGS = {
    _RUST_CHANNEL_SETTING: "nightly",
    "//command_line_option:extra_execution_platforms": [
        str(_FIRST_EXECUTION_PLATFORM),
        str(_SECOND_EXECUTION_PLATFORM),
    ],
    "//command_line_option:extra_toolchains": _PIN_TOOLCHAINS + [
        str(Label("//internal/tests:rust_guard_cross_rust_toolchain")),
        str(Label("//internal/tests:rust_guard_rust_source_toolchain")),
        str(Label("//internal/tests:rust_guard_cross_bindgen_toolchain")),
    ],
    "//command_line_option:host_platform": str(_FALLBACK_EXECUTION_PLATFORM),
    "//command_line_option:platforms": str(_FIRST_EXECUTION_PLATFORM),
}

def _assert_execution_platforms(env, info, rust, rust_source, bindgen):
    asserts.equals(env, rust, info.rust_execution_platform, "Rust compiler resolver execution platform")
    asserts.equals(env, rust_source, info.rust_source_execution_platform, "Rust source resolver execution platform")
    asserts.equals(env, bindgen, info.bindgen_execution_platform, "bindgen resolver execution platform")

def _rust_toolchain_first_execution_platform_test_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[_OptionalToolchainGuardInfo]
    asserts.equals(env, _FIRST_EXECUTION_PLATFORM, info.host_execution_platform)
    _assert_execution_platforms(env, info, _FIRST_EXECUTION_PLATFORM, _FIRST_EXECUTION_PLATFORM, _FIRST_EXECUTION_PLATFORM)
    asserts.equals(env, "first", info.rust_marker)
    asserts.equals(env, "first-source", info.rust_source_marker)
    asserts.equals(env, "first-bindgen", info.bindgen_marker)
    asserts.equals(env, None, info.synthetic_mismatch_marker)
    return analysistest.end(env)

_rust_toolchain_first_execution_platform_test = analysistest.make(
    _rust_toolchain_first_execution_platform_test_impl,
    config_settings = _FIRST_CONFIG_SETTINGS,
)

def _rust_toolchain_execution_platform_coexistence_test_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[_OptionalToolchainGuardInfo]
    asserts.equals(env, _SECOND_EXECUTION_PLATFORM, info.host_execution_platform)
    _assert_execution_platforms(env, info, _SECOND_EXECUTION_PLATFORM, _SECOND_EXECUTION_PLATFORM, _SECOND_EXECUTION_PLATFORM)
    asserts.equals(env, "second", info.rust_marker)
    asserts.equals(env, "second-source", info.rust_source_marker)
    asserts.equals(env, "second-bindgen", info.bindgen_marker)
    asserts.equals(env, None, info.synthetic_mismatch_marker)
    return analysistest.end(env)

_rust_toolchain_execution_platform_coexistence_test = analysistest.make(
    _rust_toolchain_execution_platform_coexistence_test_impl,
    config_settings = _COEXISTENCE_CONFIG_SETTINGS,
)

def _rust_toolchain_execution_platform_rejection_test_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[_OptionalToolchainGuardInfo]
    asserts.equals(env, _SECOND_EXECUTION_PLATFORM, info.host_execution_platform)
    _assert_execution_platforms(env, info, _SECOND_EXECUTION_PLATFORM, _SECOND_EXECUTION_PLATFORM, _SECOND_EXECUTION_PLATFORM)
    asserts.equals(env, None, info.rust_marker, "an unavailable Rust compiler must not move or fail the parent")
    asserts.equals(env, "second-source", info.rust_source_marker)
    asserts.equals(env, "second-bindgen", info.bindgen_marker)
    return analysistest.end(env)

_rust_toolchain_execution_platform_rejection_test = analysistest.make(
    _rust_toolchain_execution_platform_rejection_test_impl,
    config_settings = _REJECTION_CONFIG_SETTINGS,
)

def _rust_toolchain_cross_platform_rejection_test_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[_OptionalToolchainGuardInfo]
    asserts.equals(env, _SECOND_EXECUTION_PLATFORM, info.host_execution_platform)
    _assert_execution_platforms(env, info, _FALLBACK_EXECUTION_PLATFORM, _SECOND_EXECUTION_PLATFORM, _SECOND_EXECUTION_PLATFORM)
    asserts.equals(env, None, info.rust_marker, "a cross-platform Rust compiler must not leak through its bridge")
    asserts.equals(env, "second-source", info.rust_source_marker, "the analyzer resolver must remain independent")
    asserts.equals(env, None, info.bindgen_marker, "cross-platform bindgen must not leak through its bridge")
    return analysistest.end(env)

_rust_toolchain_cross_platform_rejection_test = analysistest.make(
    _rust_toolchain_cross_platform_rejection_test_impl,
    config_settings = _CROSS_ONLY_CONFIG_SETTINGS,
)

def rust_toolchain_execution_platform_guard_test(name):
    first_subject = name + "_first_subject"
    _rust_toolchain_first_probe(
        name = first_subject,
        tags = ["manual"],
    )
    first_test = name + "_first"
    _rust_toolchain_first_execution_platform_test(
        name = first_test,
        target_under_test = ":" + first_subject,
    )

    coexistence_subject = name + "_coexistence_subject"
    _rust_toolchain_second_probe(
        name = coexistence_subject,
        tags = ["manual"],
    )
    coexistence_test = name + "_coexistence"
    _rust_toolchain_execution_platform_coexistence_test(
        name = coexistence_test,
        target_under_test = ":" + coexistence_subject,
    )

    rejection_subject = name + "_rejection_subject"
    _rust_toolchain_second_probe(
        name = rejection_subject,
        tags = ["manual"],
    )
    rejection_test = name + "_rejection"
    _rust_toolchain_execution_platform_rejection_test(
        name = rejection_test,
        target_under_test = ":" + rejection_subject,
    )

    cross_subject = name + "_cross_subject"
    _rust_toolchain_second_probe(
        name = cross_subject,
        tags = ["manual"],
    )
    cross_test = name + "_cross"
    _rust_toolchain_cross_platform_rejection_test(
        name = cross_test,
        target_under_test = ":" + cross_subject,
    )

    native.test_suite(
        name = name,
        tests = [
            ":" + first_test,
            ":" + coexistence_test,
            ":" + rejection_test,
            ":" + cross_test,
        ],
    )
