"""Raw source inputs and source-selected native configuration tools."""

load("//execution_tools:defs.bzl", "EXECUTION_TOOLS_TYPE")
load(":compiler.bzl", "KernelCompilerInfo")

KernelSourceInfo = provider(
    "Raw source files and the native conf built from those same sources.",
    fields = ["files", "anchor", "conf"],
)

def _native_conf_impl(ctx):
    execution = ctx.toolchains[EXECUTION_TOOLS_TYPE].execution
    compiler = ctx.attr.compiler[KernelCompilerInfo]
    output = ctx.actions.declare_file(ctx.label.name)
    spec = ctx.actions.declare_file(ctx.label.name + ".json")
    prefix = ctx.file.anchor.dirname + "/"
    sources = [
        file
        for file in ctx.files.sources
        if file.path.startswith(prefix + "scripts/kconfig/") or file.path.startswith(prefix + "scripts/include/")
    ]
    ctx.actions.write(spec, json.encode(dict(
        source = ctx.file.anchor.dirname,
        tools = {name: file.path for name, file in execution.bootstrap_commands.items()},
        bin = execution.bin_dir,
        environment = execution.bootstrap_environment,
        path_environment = execution.bootstrap_path_environment,
        compiler = compiler.context.spec,
        main = ctx.file._main.path,
        process = ctx.file._process.path,
    )))
    ctx.actions.run(
        executable = execution.python,
        arguments = ["-B", ctx.file._builder.path, spec.path, output.path],
        inputs = depset(sources + [spec, ctx.file._builder, ctx.file._runtime, ctx.file._payload, ctx.file._spec_types, ctx.file._main, ctx.file._process], transitive = [compiler.files, execution.bootstrap_files]),
        tools = compiler.tools + execution.bootstrap_tools,
        outputs = [output],
        env = {"PYTHONPATH": ctx.file._runtime.dirname, "PYTHONDONTWRITEBYTECODE": "1"},
        mnemonic = "NativeKconfigBootstrap",
    )
    return [DefaultInfo(executable = output, files = depset([output]))]

_native_conf = rule(
    implementation = _native_conf_impl,
    executable = True,
    attrs = {
        "sources": attr.label(mandatory = True),
        "anchor": attr.label(allow_single_file = True, mandatory = True),
        "compiler": attr.label(default = "//native:compiler", cfg = "exec", providers = [KernelCompilerInfo]),
        "_builder": attr.label(default = "//native:build_conf.py", allow_single_file = True),
        "_runtime": attr.label(default = "//:probe_runtime.py", allow_single_file = True),
        "_payload": attr.label(default = "//:payload.py", allow_single_file = True),
        "_spec_types": attr.label(default = "//native:spec.py", allow_single_file = True),
        "_main": attr.label(default = "//native:conf_main.c", allow_single_file = True),
        "_process": attr.label(default = "//native:conf_process.c", allow_single_file = True),
    },
    toolchains = [EXECUTION_TOOLS_TYPE],
)

def _source_impl(ctx):
    return [KernelSourceInfo(
        files = ctx.attr.sources[DefaultInfo].files,
        anchor = ctx.file.anchor,
        conf = ctx.attr.conf[DefaultInfo].files_to_run,
    ), DefaultInfo(files = ctx.attr.sources[DefaultInfo].files)]

_kernel_source = rule(
    implementation = _source_impl,
    attrs = {
        "sources": attr.label(mandatory = True),
        "anchor": attr.label(allow_single_file = True, mandatory = True),
        "conf": attr.label(executable = True, cfg = "exec", mandatory = True),
    },
)

def kernel_source(name, sources, anchor, **kwargs):
    """Bind one source tree and its native tools without a global repository name."""
    _native_conf(name = name + "_conf", sources = sources, anchor = anchor, tags = ["manual"])
    _kernel_source(name = name, sources = sources, anchor = anchor, conf = ":" + name + "_conf", **kwargs)
