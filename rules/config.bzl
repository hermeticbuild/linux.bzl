"""Native Kconfig owns configuration values and all serialized output bytes."""

load("@autoconf.bzl//:autoconf.bzl", "AutoconfInfo", "check_toolchain", "read_results")
load("//execution_tools:defs.bzl", "EXECUTION_TOOLS_TYPE")
load(":compiler.bzl", "KernelCompilerInfo")
load(":source.bzl", "KernelSourceInfo")

KernelConfigInfo = provider(
    "Execution-time native outputs, their source, and exact selected compiler.",
    fields = ["directory", "source", "compiler", "host_compiler", "architecture", "pahole"],
)

# Fixed map_directory callback signature.
# buildifier: disable=unused-variable
def _with_probes(tctx, input_directories, output_directories, tools, additional_inputs, additional_params):
    measured = read_results(input_directories["probes"])
    output = tctx.declare_file("defaults.json", directory = output_directories["defaults"])
    tctx.run(
        executable = tools["python"],
        arguments = ["-B", tools["emit"].path, output.path, json.encode(measured)],
        inputs = depset([tools["emit"]], transitive = [additional_inputs["execution"]]),
        outputs = [output],
    )

def _impl(ctx):
    source = ctx.attr.source[KernelSourceInfo]
    compiler = ctx.attr.compiler[KernelCompilerInfo]
    host = ctx.attr.host_compiler[KernelCompilerInfo]
    execution = ctx.toolchains[EXECUTION_TOOLS_TYPE].execution
    architecture = None
    for cpu, arch, srcarch in [("x86_64", "x86_64", "x86"), ("aarch64", "arm64", "arm64")]:
        if ctx.target_platform_has_constraint(getattr(ctx.attr, "_" + cpu)[platform_common.ConstraintValueInfo]):
            architecture = struct(cpu = cpu, arch = arch, srcarch = srcarch, header_arch = srcarch, subarch = srcarch, bits = 64)
    if architecture == None:
        fail("Native configuration currently supports x86-64 and ARM64 target platforms")
    prefix = source.anchor.dirname + "/"
    logical = {file.path[len(prefix):]: file.path for file in source.files.to_list() if file.path.startswith(prefix)}
    output = ctx.actions.declare_directory(ctx.label.name)
    spec = ctx.actions.declare_file(ctx.label.name + ".json")
    ctx.actions.write(spec, json.encode(dict(
        sources = logical,
        source_label = str(ctx.attr.source.label),
        conf = source.conf.executable.path,
        compiler = compiler.context.spec,
        compiler_identity = compiler.context.id,
        host_compiler = host.context.spec,
        roles = compiler.roles,
        host_roles = host.roles,
        architecture = {"ARCH": architecture.arch},
        llvm_ias = "1" if "-fintegrated-as" in compiler.assembler_flags else "0",
        pahole = ctx.executable.pahole.path,
        tools = {name: file.path for name, file in execution.commands.items()},
        environment = execution.environment,
        path_environment = execution.path_environment,
        bin = execution.bin_dir,
        mode = ctx.attr.mode,
        fragments = [file.path for file in ctx.files.fragments],
        requested = ctx.attr.requested,
        extension = ctx.file.kconfig_extension.path if ctx.file.kconfig_extension else None,
    )))
    files = depset(
        [spec, ctx.file._driver, ctx.file._runtime, ctx.file._payload, ctx.file._spec_types] + ctx.files.fragments + ([ctx.file.kconfig_extension] if ctx.file.kconfig_extension else []),
        transitive = [source.files, compiler.files, host.files, execution.files, ctx.attr.pahole[DefaultInfo].files],
    )
    tools = execution.tools + compiler.tools + host.tools + [source.conf, ctx.attr.pahole[DefaultInfo].files_to_run]
    arguments = ["-B", ctx.file._driver.path, spec.path, output.path]
    if ctx.attr.probes:
        probes = ctx.attr.probes[AutoconfInfo]
        check_toolchain(probes, compiler.context)
        defaults = ctx.actions.declare_directory(ctx.label.name + ".defaults")
        ctx.actions.map_directory(
            input_directories = {"probes": probes.results},
            output_directories = {"defaults": defaults},
            tools = dict(python = execution.python, emit = ctx.file._emit),
            additional_inputs = {"execution": execution.python_files},
            implementation = _with_probes,
            mnemonic = "NativeKconfigDefaults",
        )
        arguments.extend(["--measured", defaults.path + "/defaults.json"])
        files = depset([defaults], transitive = [files])
    ctx.actions.run(
        executable = execution.python,
        arguments = arguments,
        inputs = files,
        tools = tools,
        outputs = [output],
        env = {"PYTHONPATH": ctx.file._runtime.dirname, "PYTHONDONTWRITEBYTECODE": "1"},
        mnemonic = "NativeKconfig",
    )
    return [
        KernelConfigInfo(directory = output, source = source, compiler = compiler, host_compiler = host, architecture = architecture, pahole = ctx.attr.pahole[DefaultInfo]),
        DefaultInfo(files = depset([output])),
    ]

kernel_config = rule(
    implementation = _impl,
    attrs = {
        "source": attr.label(mandatory = True, providers = [KernelSourceInfo]),
        "compiler": attr.label(default = "//native:compiler", providers = [KernelCompilerInfo]),
        "host_compiler": attr.label(default = "//native:compiler", cfg = "exec", providers = [KernelCompilerInfo]),
        "pahole": attr.label(executable = True, cfg = "exec", mandatory = True),
        "fragments": attr.label_list(allow_files = True),
        "requested": attr.string_dict(),
        "mode": attr.string(default = "allnoconfig", values = ["allnoconfig", "olddefconfig"]),
        "probes": attr.label(providers = [AutoconfInfo]),
        "kconfig_extension": attr.label(allow_single_file = True),
        "_driver": attr.label(default = "//native:configure.py", allow_single_file = True),
        "_runtime": attr.label(default = "//:probe_runtime.py", allow_single_file = True),
        "_payload": attr.label(default = "//:payload.py", allow_single_file = True),
        "_spec_types": attr.label(default = "//native:spec.py", allow_single_file = True),
        "_emit": attr.label(default = "//:emit.py", allow_single_file = True),
        "_x86_64": attr.label(default = "@platforms//cpu:x86_64"),
        "_aarch64": attr.label(default = "@platforms//cpu:aarch64"),
    },
    toolchains = [EXECUTION_TOOLS_TYPE],
)
