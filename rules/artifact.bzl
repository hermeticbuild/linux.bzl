"""Expose one native product from a measured kernel build."""

load("//execution_tools:defs.bzl", "EXECUTION_TOOLS_TYPE")
load(":kernel.bzl", "KernelBuildInfo")

def _impl(ctx):
    path = ctx.attr.path
    if not path or path.startswith("/") or any([part in ["", ".", ".."] for part in path.split("/")]):
        fail("Kernel artifact must be a normalized native relative path: " + path)
    kernel = ctx.attr.kernel[KernelBuildInfo]
    native_config = path in [".config", "include/config/auto.conf", "include/generated/autoconf.h"]
    directory = kernel.config.directory if native_config else kernel.directory
    source = directory.path + "/" + ("" if native_config else "kernel.artifacts/") + path
    output = ctx.actions.declare_file(ctx.label.name)
    execution = ctx.toolchains[EXECUTION_TOOLS_TYPE].execution
    ctx.actions.run(
        executable = execution.python,
        arguments = ["-B", ctx.file._runner.path, "copy", source, output.path],
        inputs = depset([directory, ctx.file._runner, ctx.file._executor, ctx.file._payload], transitive = [execution.python_files]),
        outputs = [output],
        mnemonic = "KernelArtifactSelect",
    )
    return [DefaultInfo(files = depset([output]))]

kernel_artifact = rule(
    implementation = _impl,
    toolchains = [EXECUTION_TOOLS_TYPE],
    attrs = {
        "kernel": attr.label(mandatory = True, providers = [KernelBuildInfo]),
        "path": attr.string(mandatory = True),
        "_runner": attr.label(default = "//:scheduler.py", allow_single_file = True),
        "_executor": attr.label(default = "//:execute.py", allow_single_file = True),
        "_payload": attr.label(default = "//:payload.py", allow_single_file = True),
    },
)
