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

def _config_validation_supported_test_impl(ctx):
    env = unittest.begin(ctx)
    for config in [
        {"CONFIG_KCSAN": "y"},
        {"CONFIG_MODVERSIONS": "y"},
        {
            "CONFIG_BASIC_MODVERSIONS": "y",
            "CONFIG_MODVERSIONS": "y",
        },
        {
            "CONFIG_SYSTEM_TRUSTED_KEYRING": "y",
            "CONFIG_SYSTEM_TRUSTED_KEYS": "\"\"",
        },
        {"CONFIG_TRUSTED_KEYS": "y"},
    ]:
        validate_config_features(config, str(ctx.label))
    return unittest.end(env)

config_validation_supported_test = unittest.make(_config_validation_supported_test_impl)

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
        "ima_modsigning": (
            {"CONFIG_IMA_APPRAISE_MODSIG": "y"},
            "certificate or signing",
        ),
        "module_signing": (
            {"CONFIG_MODULE_SIG": "y"},
            "certificate or signing",
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
        "trusted_certificates": (
            {"CONFIG_SYSTEM_TRUSTED_KEYS": "\"certs/trusted.pem\""},
            "unsupported certificate input",
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

    supported_test = name + "_supported_test"
    config_validation_supported_test(name = supported_test)
    tests.append(":" + supported_test)

    native.test_suite(
        name = name,
        tests = tests,
    )
