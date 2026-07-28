"""Test support for the checked-in content-addressed compact graph."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load(
    "//internal:linux_objects.bzl",
    "LinuxGeneratedHeadersInfo",
    "LinuxImageInfo",
)

visibility("private")

def _fake_linux_generated_headers_impl(ctx):
    families = {}
    headers = []
    for name in sorted(ctx.attr.family_content_ids.keys()):
        header = ctx.actions.declare_file(ctx.label.name + "." + name + ".h")
        ctx.actions.write(header, "")
        headers.append(header)
        families[name] = struct(
            arch = "x86",
            cflags = None,
            content_id = ctx.attr.family_content_ids[name],
            files = depset([header]),
            include_dir_anchors = {},
            include_dirs = [],
            name = name,
            srcarch = "x86",
            vdsomunge = None,
        )
    return [
        DefaultInfo(files = depset(headers)),
        LinuxGeneratedHeadersInfo(
            arch = "x86",
            cflags = None,
            families = families,
            files = depset(headers),
            include_dir_anchors = {},
            include_dirs = [],
            srcarch = "x86",
            vdsomunge = None,
        ),
    ]

fake_linux_generated_headers = rule(
    implementation = _fake_linux_generated_headers_impl,
    attrs = {
        "family_content_ids": attr.string_dict(mandatory = True),
    },
)

def _config_payload_batch_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = analysistest.target_actions(env)
    payload_actions = [
        action
        for action in actions
        if action.mnemonic == "LinuxConfigPayloads"
    ]
    kernel_flag_actions = [
        action
        for action in actions
        if action.mnemonic == "LinuxKernelCFlags"
    ]
    asserts.equals(env, ctx.attr.expected_actions, len(payload_actions))
    asserts.equals(env, 0, len(kernel_flag_actions))
    output_count = 0
    for action in payload_actions:
        output_count += len(action.outputs.to_list())
    asserts.equals(
        env,
        ctx.attr.expected_outputs,
        output_count,
    )
    return analysistest.end(env)

_config_payload_batch_test = analysistest.make(
    _config_payload_batch_test_impl,
    attrs = {
        "expected_actions": attr.int(mandatory = True),
        "expected_outputs": attr.int(mandatory = True),
    },
)

def config_payload_batch_test(
        name,
        target,
        expected_actions,
        expected_outputs):
    """Checks the action and output counts for bucketed payload materialization."""
    _config_payload_batch_test(
        name = name,
        expected_actions = expected_actions,
        expected_outputs = expected_outputs,
        target_under_test = target,
    )

def _objects_by_path(image):
    return {
        obj.object: obj
        for obj in image.objects
    }

def _content_graph_image_test_impl(ctx):
    env = analysistest.begin(ctx)
    image = analysistest.target_under_test(env)[LinuxImageInfo]
    actual_objects = [obj.object for obj in image.objects]
    actual_modules = [obj.object for obj in image.module_objects]
    asserts.equals(env, ctx.attr.expected_objects, actual_objects)
    asserts.equals(env, [], actual_modules)

    if ctx.attr.base_image:
        base = ctx.attr.base_image[LinuxImageInfo]
        base_by_path = _objects_by_path(base)
        actual_by_path = _objects_by_path(image)
        for object_path in ctx.attr.shared_objects:
            in_both = object_path in base_by_path and object_path in actual_by_path
            asserts.true(env, in_both, "%s must exist in the base and overlay" % object_path)
            if in_both:
                asserts.equals(
                    env,
                    base_by_path[object_path].content_id,
                    actual_by_path[object_path].content_id,
                )
                asserts.equals(
                    env,
                    str(base_by_path[object_path].output),
                    str(actual_by_path[object_path].output),
                )
        for object_path in ctx.attr.different_objects:
            in_both = object_path in base_by_path and object_path in actual_by_path
            asserts.true(env, in_both, "%s must exist in the base and overlay" % object_path)
            if in_both:
                asserts.true(
                    env,
                    base_by_path[object_path].content_id != actual_by_path[object_path].content_id,
                    "%s must use a different content target" % object_path,
                )
                asserts.true(
                    env,
                    str(base_by_path[object_path].output) != str(actual_by_path[object_path].output),
                    "%s must use a different output target" % object_path,
                )
        if ctx.attr.expect_base_output:
            asserts.equals(env, str(base.output), str(image.output))

    return analysistest.end(env)

_content_graph_image_test = analysistest.make(
    _content_graph_image_test_impl,
    attrs = {
        "base_image": attr.label(providers = [LinuxImageInfo]),
        "different_objects": attr.string_list(),
        "expect_base_output": attr.bool(),
        "expected_objects": attr.string_list(),
        "shared_objects": attr.string_list(),
    },
)

def content_graph_analysis_test_suite(
        name,
        base_image,
        blocked_driver_image,
        debug_image,
        force_net_image):
    """Checks that content-equivalent image graph nodes are actually shared."""
    cases = [
        (
            "base",
            base_image,
            None,
            ["init.o"],
            [],
            [],
            False,
        ),
        (
            "blocked_driver",
            blocked_driver_image,
            base_image,
            ["init.o"],
            ["init.o"],
            [],
            True,
        ),
        (
            "debug",
            debug_image,
            base_image,
            ["init.o", "debug.o"],
            ["init.o"],
            [],
            False,
        ),
        (
            "force_net",
            force_net_image,
            base_image,
            ["init.o", "net/core.o", "drivers/foo.o"],
            [],
            ["init.o"],
            False,
        ),
    ]
    tests = []
    for case, target, base, expected, shared, different, expect_base_output in cases:
        test_name = name + "_" + case
        _content_graph_image_test(
            name = test_name,
            base_image = base,
            different_objects = different,
            expect_base_output = expect_base_output,
            expected_objects = expected,
            shared_objects = shared,
            target_under_test = target,
        )
        tests.append(":" + test_name)

    native.test_suite(
        name = name,
        tests = tests,
    )
