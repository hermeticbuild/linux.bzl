"""Compare generated configuration with pristine native conf using selected tools."""

load(":compiler.bzl", "KernelCompilerInfo")

def _impl(ctx):
    tree = ctx.attr.kernel[DefaultInfo].files.to_list()
    if len(tree) != 1:
        fail("Expected one kernel configuration directory")
    compiler = ctx.attr.compiler[KernelCompilerInfo]
    host = ctx.attr.host_compiler[KernelCompilerInfo]
    commands = {f.basename: f for f in ctx.attr._tools[DefaultInfo].files.to_list()}
    report = ctx.actions.declare_directory(ctx.label.name + ".report")
    ctx.actions.run(
        executable = commands["python3"],
        arguments = [
            ctx.file._compare.path,
            "--tree",
            tree[0].path,
            "--linux",
            ctx.file._anchor.dirname,
            "--execution-root",
            ".",
            "--conf",
            ctx.file._conf.path,
            "--values",
            ctx.file._values.path,
            "--out",
            report.path,
            "--report-only",
        ],
        inputs = depset(
            tree + [ctx.file._compare, ctx.file._runtime, ctx.file._conf, ctx.file._values],
            transitive = [compiler.files, host.files, ctx.attr._source[DefaultInfo].files, ctx.attr._tools[DefaultInfo].files],
        ),
        outputs = [report],
        tools = compiler.tools + host.tools,
        env = {"PATH": "/usr/bin:/bin"},
        mnemonic = "KernelNativeParity",
    )
    return [DefaultInfo(files = depset([report]))]

kernel_parity = rule(implementation = _impl, attrs = {
    "kernel": attr.label(mandatory = True),
    "compiler": attr.label(default = "//:compiler", providers = [KernelCompilerInfo]),
    "host_compiler": attr.label(default = "//:compiler", cfg = "exec", providers = [KernelCompilerInfo]),
    "_tools": attr.label(default = "@host_tools//:tools"),
    "_source": attr.label(default = "@host_tools//:linux"),
    "_anchor": attr.label(default = "@host_tools//:linux/Makefile", allow_single_file = True),
    "_compare": attr.label(default = "//:compare_upstream.py", allow_single_file = True),
    "_runtime": attr.label(default = "//:probe_runtime.py", allow_single_file = True),
    "_conf": attr.label(default = "@kernel_inputs//native:upstream/conf", allow_single_file = True),
    "_values": attr.label(default = "@kernel_inputs//native:upstream/values", allow_single_file = True),
})

# Fixed transition callback signature.
# buildifier: disable=unused-variable
def _platform_transition_impl(settings, attr):
    return {"//command_line_option:platforms": str(attr.platform)}

_platform_transition = transition(
    implementation = _platform_transition_impl,
    inputs = [],
    outputs = ["//command_line_option:platforms"],
)

def _configured_impl(ctx):
    return [DefaultInfo(files = ctx.attr.report[0][DefaultInfo].files)]

configured_report = rule(implementation = _configured_impl, attrs = {
    "report": attr.label(mandatory = True, cfg = _platform_transition),
    "platform": attr.label(mandatory = True),
    "_allowlist_function_transition": attr.label(default = "@bazel_tools//tools/allowlists/function_transition_allowlist"),
})
