"""Producer-owned acceptance check of measured flags and linked products.

This development fixture uses the source-built Make exporter; it is not part of
the downstream source/config/build/artifact interface.
"""

load("//execution_tools:defs.bzl", "EXECUTION_TOOLS_TYPE")
load(":kernel.bzl", "KernelBuildInfo")

def _impl(ctx):
    kernel = ctx.attr.kernel[KernelBuildInfo]
    config = kernel.config
    execution = ctx.toolchains[EXECUTION_TOOLS_TYPE].execution
    spec = ctx.actions.declare_file(ctx.label.name + ".json")
    output = ctx.actions.declare_directory(ctx.label.name)
    prefix = config.source.anchor.dirname + "/"
    ctx.actions.write(spec, json.encode(dict(
        kernel = kernel.directory.path,
        config = config.directory.path,
        sources = {f.path[len(prefix):]: f.path for f in config.source.files.to_list() if f.path.startswith(prefix)},
        compiler = config.compiler.context.spec,
        host_compiler = config.host_compiler.context.spec,
        roles = config.compiler.roles,
        host_roles = config.host_compiler.roles,
        architecture = config.architecture.arch,
        pahole = config.pahole.files_to_run.executable.path,
        tools = {name: file.path for name, file in execution.build_commands.items()},
        environment = execution.bootstrap_environment,
        path_environment = execution.bootstrap_path_environment,
        bin = execution.bin_dir,
        make = ctx.executable._make.path,
        configure = ctx.file._configure.path,
    )))
    ctx.actions.run(
        executable = execution.python,
        arguments = ["-B", ctx.file._checker.path, spec.path, output.path],
        inputs = depset([spec, kernel.directory, config.directory, ctx.file._checker, ctx.file._configure, ctx.file._spec_types, ctx.file._runtime], transitive = [config.source.files, config.compiler.files, config.host_compiler.files, config.pahole.files, execution.build_files]),
        tools = config.compiler.tools + config.host_compiler.tools + [config.pahole.files_to_run, ctx.attr._make[DefaultInfo].files_to_run] + execution.build_tools,
        outputs = [output],
        env = {"PYTHONPATH": ctx.file._runtime.dirname, "PYTHONDONTWRITEBYTECODE": "1"},
        mnemonic = "KernelNativeValidation",
    )
    return [DefaultInfo(files = depset([output]))]

kernel_validation = rule(
    implementation = _impl,
    toolchains = [EXECUTION_TOOLS_TYPE],
    attrs = {
        "kernel": attr.label(mandatory = True, providers = [KernelBuildInfo]),
        "_make": attr.label(default = "//native:make_ast", cfg = "exec", executable = True),
        "_checker": attr.label(default = "//:compare_kbuild.py", allow_single_file = True),
        "_configure": attr.label(default = "//native:configure.py", allow_single_file = True),
        "_spec_types": attr.label(default = "//native:spec.py", allow_single_file = True),
        "_runtime": attr.label(default = "//:probe_runtime.py", allow_single_file = True),
    },
)
