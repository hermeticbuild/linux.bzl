"""Reject syntax from a different source before registering kernel planning."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//rules:kernel.bzl", "kernel_build")
load("//rules:syntax.bzl", "KernelSyntaxInfo")

def _foreign_impl(ctx):
    return [KernelSyntaxInfo(anchor = ctx.file.anchor, expand = None)]

_foreign = rule(implementation = _foreign_impl, attrs = {"anchor": attr.label(allow_single_file = True)})

def _test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "does not match native configuration source")
    return analysistest.end(env)

_test = analysistest.make(
    _test_impl,
    expect_failure = True,
    config_settings = {
        "//command_line_option:platforms": str(Label("@llvm//platforms:linux_x86_64")),
        "//command_line_option:extra_execution_platforms": [str(Label("@llvm//platforms:linux_x86_64"))],
    },
)

def syntax_identity_test(name):
    _foreign(name = name + "_foreign", anchor = ":main.c", tags = ["manual"])
    kernel_build(
        name = name + "_kernel",
        config = "//native:config",
        syntax = ":" + name + "_foreign",
        tags = ["manual"],
    )
    _test(name = name, target_under_test = ":" + name + "_kernel")
