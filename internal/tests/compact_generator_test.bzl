"""Analysis tests for the compact generator's strict content-graph contract."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load(
    "//internal:compact_generator.bzl",
    "LinuxCompactV7Info",
    "linux_compact_v7_metadata",
)

visibility("private")

_GRAPH_PROJECTION_ID = "90a7f57f22e5bef11e9aa283e6d0b54e098bbc1a800b28e23bdbd2c23f53eeb0"

def _failure_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, ctx.attr.expected_error)
    return analysistest.end(env)

_failure_test = analysistest.make(
    _failure_test_impl,
    attrs = {
        "expected_error": attr.string(mandatory = True),
    },
    expect_failure = True,
)

def _argument_after(argv, flag):
    for index in range(len(argv) - 1):
        if argv[index] == flag:
            return argv[index + 1]
    return ""

def _v7_action_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[LinuxCompactV7Info]
    asserts.equals(env, "compact-v7-lazy-action-graph", info.protocol)
    asserts.equals(env, _GRAPH_PROJECTION_ID, info.toolchain_profile_id)
    asserts.equals(env, "linux.bzl/test/x86", info.compile_environment_abi)
    asserts.true(env, info.metadata.basename.endswith(".metadata.json"))
    asserts.true(env, info.graph_projection.basename.endswith(".graph_profile_projection.json"))

    actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "LinuxCompactV7Kconfig"
    ]
    asserts.equals(env, 1, len(actions))
    if actions:
        argv = actions[0].argv
        asserts.equals(
            env,
            _GRAPH_PROJECTION_ID,
            _argument_after(argv, "-toolchain_profile_id"),
        )
        graph_profile = _argument_after(argv, "-graph_profile")
        asserts.true(
            env,
            graph_profile.endswith("internal/tests/graph_profile.json"),
            "unexpected -graph_profile path %r" % graph_profile,
        )
        asserts.true(
            env,
            "graph_profile.json" in [file.basename for file in actions[0].inputs.to_list()],
            "checked-in graph profile must be an action input",
        )
        asserts.true(env, "-graph_profile_projection_out" in argv)
        asserts.equals(
            env,
            "linux.bzl/test/x86",
            _argument_after(argv, "-compile_environment_abi"),
        )
        asserts.true(env, "-compact_metadata_out" in argv)
    return analysistest.end(env)

_v7_action_test = analysistest.make(_v7_action_test_impl)

def _v7_derived_profile_id_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[LinuxCompactV7Info]
    asserts.equals(env, "", info.toolchain_profile_id)
    actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "LinuxCompactV7Kconfig"
    ]
    asserts.equals(env, 1, len(actions))
    if actions:
        asserts.false(env, "-toolchain_profile_id" in actions[0].argv)
        asserts.true(env, "-graph_profile" in actions[0].argv)
    return analysistest.end(env)

_v7_derived_profile_id_test = analysistest.make(_v7_derived_profile_id_test_impl)

def _subject(
        name,
        configs,
        compile_environment_abi = "linux.bzl/test/x86",
        generated_headers_by_config = {
            "base": "//internal/tests:base_generated_headers",
        }):
    linux_compact_v7_metadata(
        name = name,
        compile_environment_abi = compile_environment_abi,
        configs = configs,
        graph_profile = ":graph_profile.json",
        generated_headers_by_config = generated_headers_by_config,
        kbuild = "//tests/compact:Kbuild",
        root = "//tests/compact:content_graph.Kconfig",
        srcs = [
            "//tests/compact:arch/x86/kernel/vmlinux.lds.S",
            "//tests/compact:include/linux/compiler-version.h",
            "//tests/compact:include/linux/compiler_types.h",
            "//tests/compact:include/linux/kconfig.h",
            "//tests/compact:init.c",
        ],
        tags = ["manual"],
    )

def _v7_subject(name, toolchain_profile_id):
    linux_compact_v7_metadata(
        name = name,
        graph_profile = ":graph_profile.json",
        compile_environment_abi = "linux.bzl/test/x86",
        configs = {
            "//tests/compact:base.config": "base",
        },
        generated_headers_by_config = {
            "base": "//internal/tests:base_generated_headers",
        },
        kbuild = "//tests/compact:Kbuild",
        root = "//tests/compact:content_graph.Kconfig",
        srcs = [
            "//tests/compact:arch/x86/kernel/vmlinux.lds.S",
            "//tests/compact:include/linux/compiler-version.h",
            "//tests/compact:include/linux/compiler_types.h",
            "//tests/compact:include/linux/kconfig.h",
            "//tests/compact:init.c",
        ],
        tags = ["manual"],
        toolchain_profile_id = toolchain_profile_id,
        vars = {
            "ARCH": "x86",
            "SRCARCH": "x86",
        },
    )

def compact_generator_test_suite(name):
    cases = [
        struct(
            configs = {
                "//tests/compact:base.config": "base",
            },
            expected_error = "do not match compact config names",
            generated_headers_by_config = {
                "debug": "//internal/tests:debug_generated_headers",
            },
            name = "header_key_mismatch",
        ),
        struct(
            configs = {
                "//tests/compact:base.config": "base",
                "//tests/compact:debug.config": "base",
            },
            expected_error = "duplicate compact config name",
            generated_headers_by_config = {
                "base": "//internal/tests:base_generated_headers",
            },
            name = "duplicate_config_name",
        ),
        struct(
            configs = {
                "//tests/compact:base.config": "",
            },
            expected_error = "compact config names must be non-empty",
            generated_headers_by_config = {},
            name = "empty_config_name",
        ),
        struct(
            configs = {
                "//tests/compact:base.config": "base",
            },
            expected_error = "generated_headers_by_config",
            generated_headers_by_config = {
                "base": "",
            },
            name = "empty_header_label",
        ),
        struct(
            compile_environment_abi = "",
            configs = {
                "//tests/compact:base.config": "base",
            },
            expected_error = "compile_environment_abi must be non-empty",
            generated_headers_by_config = {
                "base": "//internal/tests:base_generated_headers",
            },
            name = "empty_abi",
        ),
    ]

    tests = []
    for case in cases:
        subject = "%s_%s_subject" % (name, case.name)
        _subject(
            name = subject,
            compile_environment_abi = getattr(case, "compile_environment_abi", "linux.bzl/test/x86"),
            configs = case.configs,
            generated_headers_by_config = case.generated_headers_by_config,
        )
        test = "%s_%s_test" % (name, case.name)
        _failure_test(
            name = test,
            expected_error = case.expected_error,
            target_under_test = ":" + subject,
        )
        tests.append(":" + test)

    v7_subject = name + "_v7_subject"
    _v7_subject(
        name = v7_subject,
        toolchain_profile_id = _GRAPH_PROJECTION_ID,
    )
    v7_test = name + "_v7_action_test"
    _v7_action_test(
        name = v7_test,
        target_under_test = ":" + v7_subject,
    )
    tests.append(":" + v7_test)

    derived_v7_profile_subject = name + "_derived_v7_profile_subject"
    _v7_subject(
        name = derived_v7_profile_subject,
        toolchain_profile_id = "",
    )
    derived_v7_profile_test = name + "_derived_v7_profile_test"
    _v7_derived_profile_id_test(
        name = derived_v7_profile_test,
        target_under_test = ":" + derived_v7_profile_subject,
    )
    tests.append(":" + derived_v7_profile_test)

    native.test_suite(
        name = name,
        tests = tests,
    )
