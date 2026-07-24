"""Analysis tests for the private Linux platform transition gateway."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//internal:providers.bzl", "LinuxKernelInfo")

visibility("private")

def _graph_fixture_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name + ".txt")
    ctx.actions.write(
        output = out,
        content = "%s %s\n" % (ctx.attr.selected_os, ctx.attr.selected_arch),
    )
    kernel = LinuxKernelInfo(
        arch = ctx.attr.selected_arch,
        version = "test",
        kernel_release = out,
        image = out,
        vmlinux = out,
        config = out,
        system_map = out,
    )
    return [
        DefaultInfo(files = depset([out])),
        kernel,
        OutputGroupInfo(probe = depset([out])),
    ]

platform_graph_fixture = rule(
    implementation = _graph_fixture_impl,
    attrs = {
        "selected_arch": attr.string(mandatory = True),
        "selected_os": attr.string(mandatory = True),
    },
)

def _gateway_forwarding_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)

    asserts.true(env, LinuxKernelInfo in target)
    asserts.true(env, OutputGroupInfo in target)
    kernel = target[LinuxKernelInfo]
    asserts.equals(env, ctx.attr.expected_arch, kernel.arch)
    asserts.equals(env, 1, len(target[DefaultInfo].files.to_list()))
    asserts.equals(env, 1, len(target[OutputGroupInfo].probe.to_list()))
    asserts.equals(
        env,
        target[DefaultInfo].files.to_list()[0],
        target[OutputGroupInfo].probe.to_list()[0],
    )
    return analysistest.end(env)

gateway_forwarding_test = analysistest.make(
    _gateway_forwarding_test_impl,
    attrs = {
        "expected_arch": attr.string(mandatory = True),
    },
)

def _gateway_failure_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(
        env,
        "requires a target platform with @platforms//os:linux",
    )
    return analysistest.end(env)

gateway_failure_test = analysistest.make(
    _gateway_failure_test_impl,
    expect_failure = True,
)
