"""Analysis tests for the compact generator's strict content-graph contract."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load(
    "//internal:compact_generator.bzl",
    "LinuxCompactV7Info",
    "linux_compact_buildfiles",
    "linux_compact_v7_metadata",
)

visibility("private")

_CC_PROFILE_ID = "9c5a251ef14dfa9a0fa4db464d32db226f09b0ab7cfe36e4f18f08385768cb22"

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
    asserts.equals(env, _CC_PROFILE_ID, info.toolchain_profile_id)
    asserts.equals(env, "linux.bzl/test/x86", info.compile_environment_abi)
    asserts.true(env, info.metadata.basename.endswith(".metadata.json"))

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
            "compact-v7-lazy-action-graph",
            _argument_after(argv, "-compact_protocol"),
        )
        asserts.equals(
            env,
            _CC_PROFILE_ID,
            _argument_after(argv, "-toolchain_profile_id"),
        )
        cc_profile = _argument_after(argv, "-cc_profile")
        asserts.true(
            env,
            cc_profile.endswith("internal/tests/cc_profile.json"),
            "unexpected -cc_profile path %r" % cc_profile,
        )
        asserts.true(
            env,
            "cc_profile.json" in [file.basename for file in actions[0].inputs.to_list()],
            "checked-in CC profile must be an action input",
        )
        asserts.equals(
            env,
            "linux.bzl/test/x86",
            _argument_after(argv, "-compile_environment_abi"),
        )
        asserts.true(env, "-compact_metadata_out" in argv)
        asserts.false(env, "-compact_buildfile_out" in argv)
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
        asserts.true(env, "-cc_profile" in actions[0].argv)
    return analysistest.end(env)

_v7_derived_profile_id_test = analysistest.make(_v7_derived_profile_id_test_impl)

def _subject(
        name,
        configs,
        compact_base_config = "base",
        compile_environment_abi = "linux.bzl/test/x86",
        generated_headers_by_config = {
            "base": "//internal/tests:base_generated_headers",
        },
        source_label_package = "//tests/compact",
        source_root_label = "//tests/compact:content_graph.Kconfig"):
    linux_compact_buildfiles(
        name = name,
        compact_base_config = compact_base_config,
        compile_environment_abi = compile_environment_abi,
        configs = configs,
        generated_headers_by_config = generated_headers_by_config,
        kbuild = "//tests/compact:Kbuild",
        root = "//tests/compact:content_graph.Kconfig",
        source_label_package = source_label_package,
        source_root_label = source_root_label,
        tags = ["manual"],
    )

def _v7_subject(name, toolchain_profile_id):
    linux_compact_v7_metadata(
        name = name,
        cc_profile = ":cc_profile.json",
        compile_environment_abi = "linux.bzl/test/x86",
        configs = {
            "//tests/compact:base.config": "base",
        },
        generated_headers_by_config = {
            "base": "//internal/tests:base_generated_headers",
        },
        kbuild = "//tests/compact:Kbuild",
        root = "//tests/compact:content_graph.Kconfig",
        tags = ["manual"],
        toolchain_profile_id = toolchain_profile_id,
    )

def compact_generator_test_suite(name):
    cases = [
        struct(
            configs = {},
            expected_error = "configs must contain at least one compact config",
            generated_headers_by_config = {},
            name = "empty_configs",
        ),
        struct(
            compact_base_config = "base",
            configs = {
                "//tests/compact:base.config": "other",
            },
            expected_error = "is not present in configs",
            generated_headers_by_config = {
                "other": "//internal/tests:other_generated_headers",
            },
            name = "missing_base",
        ),
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
        struct(
            configs = {
                "//tests/compact:base.config": "base",
            },
            expected_error = "source_label_package must be non-empty",
            generated_headers_by_config = {
                "base": "//internal/tests:base_generated_headers",
            },
            name = "empty_source_package",
            source_label_package = "",
        ),
        struct(
            configs = {
                "//tests/compact:base.config": "base",
            },
            expected_error = "source_root_label must be non-empty",
            generated_headers_by_config = {
                "base": "//internal/tests:base_generated_headers",
            },
            name = "empty_source_root",
            source_root_label = "",
        ),
    ]

    tests = []
    for case in cases:
        subject = "%s_%s_subject" % (name, case.name)
        _subject(
            name = subject,
            compact_base_config = getattr(case, "compact_base_config", "base"),
            compile_environment_abi = getattr(case, "compile_environment_abi", "linux.bzl/test/x86"),
            configs = case.configs,
            generated_headers_by_config = case.generated_headers_by_config,
            source_label_package = getattr(case, "source_label_package", "//tests/compact"),
            source_root_label = getattr(case, "source_root_label", "//tests/compact:content_graph.Kconfig"),
        )
        test = "%s_%s_test" % (name, case.name)
        _failure_test(
            name = test,
            expected_error = case.expected_error,
            target_under_test = ":" + subject + "_generated",
        )
        tests.append(":" + test)

    v7_subject = name + "_v7_subject"
    _v7_subject(
        name = v7_subject,
        toolchain_profile_id = _CC_PROFILE_ID,
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
