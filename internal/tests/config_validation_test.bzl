"""Analysis tests for supported Linux config validation."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts", "unittest")
load("//internal:config_validation.bzl", "validate_config_features")

visibility("private")

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

def _config_validation_kcsan_test_impl(ctx):
    env = unittest.begin(ctx)
    validate_config_features(
        {"CONFIG_KCSAN": "y"},
        str(ctx.label),
    )
    return unittest.end(env)

config_validation_kcsan_test = unittest.make(_config_validation_kcsan_test_impl)

def config_validation_test_suite(name):
    cases = {
        "cfi_modules": (
            {"CONFIG_CFI_CLANG": "y"},
            "module metadata instrumentation",
        ),
        "extended_modversions": (
            {"CONFIG_EXTENDED_MODVERSIONS": "y"},
            "module versioning",
        ),
        "gcov_modules": (
            {"CONFIG_GCOV_KERNEL": "y"},
            "module metadata instrumentation",
        ),
        "gendwarfksyms": (
            {"CONFIG_GENDWARFKSYMS": "y"},
            "module versioning",
        ),
        "modversions": (
            {"CONFIG_MODVERSIONS": "y"},
            "module versioning",
        ),
        "native_cpu": (
            {"CONFIG_X86_NATIVE_CPU": "y"},
            "CONFIG_X86_NATIVE_CPU",
        ),
        "srcversion": (
            {"CONFIG_MODULE_SRCVERSION_ALL": "y"},
            "module versioning",
        ),
        "trim_unused_ksyms": (
            {"CONFIG_TRIM_UNUSED_KSYMS": "y"},
            "module versioning",
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

    kcsan_test = name + "_kcsan_test"
    config_validation_kcsan_test(name = kcsan_test)
    tests.append(":" + kcsan_test)

    native.test_suite(
        name = name,
        tests = tests,
    )
