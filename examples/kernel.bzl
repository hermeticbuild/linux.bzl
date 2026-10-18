"""Examples select a platform/toolchain and pass fragments to the measured kernel rule."""

load("@bazel_skylib//lib:shell.bzl", "shell")
load("@rules_python//python:defs.bzl", "py_test")
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

def kernel_example(name, requested, platform, cc_toolchain, overrides = {}):
    requested = dict(requested, **overrides)
    for suffix, config_only in [("_configuration", True), ("_unconfigured", False)]:
        kernel_build(
            name = name + suffix,
            config_only = config_only,
            mode = "qualification",
            requested = requested,
            rounds = 0 if config_only else 64,
            tags = ["manual"],
        )
        _configured(
            name = name + ("_config" if config_only else ""),
            target = ":" + name + suffix,
            platform = platform,
            cc_toolchain = cc_toolchain,
            tags = ["manual"] if not config_only else [],
        )

def example_config_test(name, config, expected):
    py_test(
        name = name,
        srcs = ["//:test_config_values.py"],
        main = "//:test_config_values.py",
        args = ["$(location " + config + ")", shell.quote(json.encode(expected))],
        data = [config],
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
