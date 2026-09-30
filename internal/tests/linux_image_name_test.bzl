"""Tests portable Linux image overlay names."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//internal:image_name_validation.bzl", "validate_linux_overlay_name")

visibility("private")

def _overlay_names_impl(ctx):
    for name in ctx.attr.names:
        validate_linux_overlay_name(name)
    return []

_overlay_names = rule(
    implementation = _overlay_names_impl,
    attrs = {"names": attr.string_list(mandatory = True)},
)

def _overlay_name_success_test_impl(ctx):
    env = analysistest.begin(ctx)
    return analysistest.end(env)

_overlay_name_success_test = analysistest.make(_overlay_name_success_test_impl)

def _overlay_name_failure_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, ctx.attr.expected)
    return analysistest.end(env)

_overlay_name_failure_test = analysistest.make(
    _overlay_name_failure_test_impl,
    attrs = {"expected": attr.string(mandatory = True)},
    expect_failure = True,
)

def linux_image_name_test(name):
    tests = []

    valid_subject = name + "_valid_subject"
    _overlay_names(
        name = valid_subject,
        names = ["debug", "debug-2", "debug_2", "com0", "com10", "lpt0"],
        tags = ["manual"],
    )
    valid_test = name + "_valid"
    _overlay_name_success_test(
        name = valid_test,
        target_under_test = ":" + valid_subject,
    )
    tests.append(valid_test)

    failures = {"base": "must not be base"}
    for reserved in ["aux", "con", "nul", "prn", "com1", "com9", "lpt1", "lpt9"]:
        failures[reserved] = "reserved on Windows"
    for invalid_name, expected in failures.items():
        subject = "%s_%s_subject" % (name, invalid_name)
        _overlay_names(
            name = subject,
            names = [invalid_name],
            tags = ["manual"],
        )
        test = "%s_%s" % (name, invalid_name)
        _overlay_name_failure_test(
            name = test,
            expected = expected,
            target_under_test = ":" + subject,
        )
        tests.append(test)

    native.test_suite(
        name = name,
        tests = tests,
    )
