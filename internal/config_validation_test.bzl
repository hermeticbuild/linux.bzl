"""Analysis tests for supported Linux config validation."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load(":config_validation.bzl", "validate_config_features")

def _config_validation_fixture_impl(ctx):
    validate_config_features(ctx.attr.config, str(ctx.label))
    return []

config_validation_fixture = rule(
    implementation = _config_validation_fixture_impl,
    attrs = {
        "config": attr.string_dict(),
    },
)

def _config_validation_failure_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, ctx.attr.expected_error)
    return analysistest.end(env)

config_validation_failure_test = analysistest.make(
    _config_validation_failure_test_impl,
    attrs = {
        "expected_error": attr.string(mandatory = True),
    },
    expect_failure = True,
)

def config_validation_test_suite(name):
    cases = {
        "bpf": (
            {"CONFIG_BPF_SYSCALL": "y"},
            "CONFIG_BPF_SYSCALL",
        ),
        "module": (
            {"CONFIG_TEST_DRIVER": "m"},
            "loadable-module selections",
        ),
        "modules": (
            {"CONFIG_MODULES": "y"},
            "CONFIG_MODULES",
        ),
        "native_cpu": (
            {"CONFIG_X86_NATIVE_CPU": "y"},
            "CONFIG_X86_NATIVE_CPU",
        ),
    }
    tests = []
    for case, values in cases.items():
        fixture = name + "_" + case
        config_validation_fixture(
            name = fixture,
            config = values[0],
            tags = ["manual"],
        )
        test = fixture + "_test"
        config_validation_failure_test(
            name = test,
            expected_error = values[1],
            target_under_test = ":" + fixture,
        )
        tests.append(":" + test)

    native.test_suite(
        name = name,
        tests = tests,
    )
