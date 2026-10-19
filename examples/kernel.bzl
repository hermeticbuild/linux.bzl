"""Examples select a platform/toolchain and pass fragments to the measured kernel rule."""

load("@bazel_skylib//lib:shell.bzl", "shell")
load("@rules_python//python:defs.bzl", "py_test")
load("//rules:config.bzl", "kernel_config")
load("//rules:kernel.bzl", "kernel_build")

# Fixed Bazel transition callback signature.
# buildifier: disable=unused-variable
def _platform_impl(settings, attr):
    return {
        "//command_line_option:platforms": str(attr.platform),
        "//command_line_option:extra_toolchains": [str(attr.cc_toolchain)],
    }

_platform = transition(
    implementation = _platform_impl,
    inputs = [],
    outputs = ["//command_line_option:platforms", "//command_line_option:extra_toolchains"],
)

def _configured_impl(ctx):
    return [DefaultInfo(files = ctx.attr.target[0][DefaultInfo].files)]

_configured = rule(implementation = _configured_impl, attrs = {
    "target": attr.label(mandatory = True, cfg = _platform),
    "platform": attr.label(mandatory = True),
    "cc_toolchain": attr.label(mandatory = True),
    "_allowlist_function_transition": attr.label(default = "@bazel_tools//tools/allowlists/function_transition_allowlist"),
})

def kernel_example(name, fragments, platform, cc_toolchain):
    kernel_config(
        name = name + "_configuration",
        source = "//native:source",
        fragments = fragments,
        pahole = "@pahole//:pahole",
        tags = ["manual"],
    )
    kernel_build(
        name = name + "_unconfigured",
        config = ":" + name + "_configuration",
        syntax = "@kernel_syntax//:program",
        tags = ["manual"],
    )
    for public, target in [(name + "_config", name + "_configuration"), (name, name + "_unconfigured")]:
        _configured(
            name = public,
            target = ":" + target,
            platform = platform,
            cc_toolchain = cc_toolchain,
            tags = ["manual"],
        )

def example_config_test(name, config, expected):
    py_test(
        name = name,
        srcs = ["//:test_config_values.py"],
        main = "//:test_config_values.py",
        args = ["$(location " + config + ")", shell.quote(json.encode(expected))],
        data = [config],
        tags = ["manual"],
    )

def _initramfs_impl(ctx):
    output = ctx.actions.declare_file(ctx.label.name + ".cpio")
    ctx.actions.run(
        executable = ctx.attr._writer[DefaultInfo].files_to_run,
        arguments = [ctx.file.motd.path, output.path],
        inputs = [ctx.file.motd],
        outputs = [output],
        mnemonic = "ExampleInitramfs",
    )
    return [DefaultInfo(files = depset([output]))]

example_initramfs = rule(implementation = _initramfs_impl, attrs = {
    "motd": attr.label(mandatory = True, allow_single_file = True),
    "_writer": attr.label(default = "//examples:initramfs_writer", cfg = "exec", executable = True),
})
