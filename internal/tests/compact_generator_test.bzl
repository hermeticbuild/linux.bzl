"""Analysis tests for the compact generator's strict content-graph contract."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//internal:compact_generator.bzl", "linux_compact_buildfiles")

visibility("private")

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

    native.test_suite(
        name = name,
        tests = tests,
    )
