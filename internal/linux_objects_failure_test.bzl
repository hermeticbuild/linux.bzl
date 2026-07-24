"""Analysis tests for Linux rules that intentionally fail closed."""

load(
    "@bazel_skylib//lib:unittest.bzl",
    "analysistest",
    "asserts",
)
load(
    ":linux_objects.bzl",
    "LinuxImageInfo",
    "linux_compact_image",
    "linux_compressed_image",
    "linux_object",
)

visibility("//...")

def _fake_linux_image_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name + ".vmlinux")
    ctx.actions.write(out, "")
    return [
        DefaultInfo(files = depset([out])),
        LinuxImageInfo(
            archives = [],
            objects = [],
            output = out,
        ),
    ]

_fake_linux_image = rule(implementation = _fake_linux_image_impl)

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

def _image_output_groups_test_impl(ctx):
    env = analysistest.begin(ctx)
    output_groups = analysistest.target_under_test(env)[OutputGroupInfo]
    asserts.true(env, hasattr(output_groups, "image"))
    asserts.false(env, hasattr(output_groups, "modinfo"))
    asserts.false(env, hasattr(output_groups, "modules"))
    return analysistest.end(env)

_image_output_groups_test = analysistest.make(_image_output_groups_test_impl)

def linux_objects_fail_closed_test_suite(name):
    """Instantiates analysis tests for supported Linux object/image rules."""
    image = name + "_input_image"
    fixture_tags = ["manual"]

    _fake_linux_image(
        name = image,
        tags = fixture_tags,
    )
    empty_image = name + "_empty_image"
    linux_compact_image(
        name = empty_image,
        objects = [],
        tags = fixture_tags,
    )

    certificate_object = name + "_certificate_object"
    linux_object(
        name = certificate_object,
        src = "linux_objects_test_fixture.c",
        mode = "y",
        object = "certs/system_certificates.o",
        tags = fixture_tags,
    )

    failure_cases = [
        (empty_image, "requires at least one compiled object"),
        (certificate_object, "hermetic certificate embedding and signing are not implemented"),
    ]
    tests = []
    for target, expected_error in failure_cases:
        test_name = target + "_test"
        _failure_test(
            name = test_name,
            expected_error = expected_error,
            target_under_test = ":" + target,
        )
        tests.append(":" + test_name)

    real_arm64_image = name + "_real_arm64_image"
    linux_compressed_image(
        name = real_arm64_image,
        arch = "arm64",
        format = "arm64_image",
        image = ":" + image,
        tags = fixture_tags,
    )
    output_groups_test = real_arm64_image + "_output_groups_test"
    _image_output_groups_test(
        name = output_groups_test,
        target_under_test = ":" + real_arm64_image,
    )
    tests.append(":" + output_groups_test)

    native.test_suite(
        name = name,
        tests = tests,
    )
