"""Small real-compiler graph exercising generated headers and source pruning."""

load(":compiler.bzl", "KernelCompilerInfo")
load(":runtime.bzl", "context")
load(":schedule.bzl", "schedule", "write_payload")

# Fixed map_directory callback signature.
# buildifier: disable=unused-variable
def _plan(tctx, input_directories, output_directories, tools, additional_inputs, additional_params):
    originals = {f.basename: f for f in additional_inputs["originals"].to_list()}
    runtime_tools = dict(tools, host_cc = additional_params["cc"], host_cxx = additional_params["cc"], target_cc = additional_params["cc"], commands = {f.basename: f for f in tools["execution_files"].to_list()})
    params = struct(python = tools["python"], executor = tools["executor"], materialize = tools["emit"], link_script = tools["emit"])
    jobs = []
    ctx = context(tctx, output_directories, runtime_tools, params, recorded = jobs)
    header = ctx.actions.declare_file("generated.h")
    ctx.actions.run(executable = tools["python"], arguments = [tools["codegen"].path, header.path], inputs = [tools["codegen"]], outputs = [header])
    for source in ["main.c", "other.c"]:
        output = ctx.actions.declare_file(source + ".o")
        ctx.actions.run(
            executable = additional_params["cc"],
            arguments = ["-I", header.dirname, "-c", originals[source].path, "-o", output.path],
            inputs = depset([tools["source_root"], header], transitive = [tools["compiler_files"]]),
            outputs = [output],
            mnemonic = "FixtureCompile",
            progress_message = "Compile fixture " + source,
        )
    write_payload(tctx, tools["python"], tools["emit"], output_directories["schedule"], jobs)

def _impl(ctx):
    compiler = ctx.attr.compiler[KernelCompilerInfo]
    execution_files = ctx.attr._execution[DefaultInfo].files
    commands = {f.basename: f for f in execution_files.to_list()}
    sources = ctx.actions.declare_directory(ctx.label.name + ".sources")
    manifest = ctx.actions.declare_file(ctx.label.name + ".sources.json")
    ctx.actions.write(manifest, json.encode(dict(sources = {f.basename: f.path for f in ctx.files.srcs})))
    ctx.actions.run(
        executable = commands["python3"],
        arguments = [ctx.file._prepare.path, manifest.path, sources.path],
        inputs = ctx.files.srcs + [manifest, ctx.file._prepare],
        outputs = [sources],
    )
    literals = ctx.actions.declare_directory(ctx.label.name + ".graph")
    metadata = ctx.actions.declare_directory(ctx.label.name + ".schedule")
    scratch = ctx.actions.declare_directory(ctx.label.name + ".scratch")
    tools = dict(python = commands["python3"], emit = ctx.file._emit, executor = ctx.file._executor, runner = ctx.file._runner, codegen = ctx.file._codegen)
    ctx.actions.map_directory(
        input_directories = {"sources": sources},
        output_directories = {"kernel": literals, "schedule": metadata, "scratch": scratch},
        additional_inputs = {"originals": depset(ctx.files.srcs)},
        tools = dict(tools, source_root = sources, compiler_files = compiler.files, execution_files = execution_files),
        additional_params = dict(cc = compiler.roles["CC"]),
        implementation = _plan,
    )
    result = schedule(ctx, literals, metadata, sources, depset(ctx.files.srcs), ctx.files.srcs[0].dirname, tools, compiler.files, execution_files, steps = 3)
    return [DefaultInfo(files = depset([result]))]

schedule_fixture = rule(implementation = _impl, attrs = {
    "compiler": attr.label(default = "//:compiler", providers = [KernelCompilerInfo]),
    "srcs": attr.label_list(allow_files = True),
    "_execution": attr.label(default = "@host_tools//:tools"),
    "_prepare": attr.label(default = "//rules:prepare_source.py", allow_single_file = True),
    "_emit": attr.label(default = "//:emit.py", allow_single_file = True),
    "_executor": attr.label(default = "//:execute.py", allow_single_file = True),
    "_codegen": attr.label(default = "//tests:generate.py", allow_single_file = True),
    "_runner": attr.label(default = "//:scheduler.py", allow_single_file = True),
})
