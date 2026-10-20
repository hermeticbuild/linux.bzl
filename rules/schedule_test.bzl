"""Small real-compiler graph exercising generated headers and source pruning."""

load("//execution_tools:defs.bzl", "EXECUTION_TOOLS_TYPE")
load(":commands.bzl", "compile_action", "native_argument")
load(":compiler.bzl", "KernelCompilerInfo")
load(":image.bzl", "compiler_prerequisites")
load(":runtime.bzl", "context")
load(":schedule.bzl", "schedule", "write_schedule")

# Fixed map_directory callback signature.
# buildifier: disable=unused-variable
def _plan(tctx, input_directories, output_directories, tools, additional_inputs, additional_params):
    originals = {f.basename: f for f in additional_inputs["originals"].to_list()}
    environment = json.decode(additional_params["environment"])
    runtime_tools = dict(tools, host_cc = additional_params["cc"], host_cxx = additional_params["cc"], target_cc = additional_params["cc"], host_roles = {}, host_spec = {}, backend_environment = environment, target_compiler_files = tools["compiler_files"], commands = {name.removeprefix("cmd_"): file for name, file in tools.items() if name.startswith("cmd_")}, environment = environment, path_environment = json.decode(additional_params["path_environment"]))
    params = struct(python = tools["python"], executor = tools["executor"], materialize = tools["emit"], link_script = tools["emit"], native_recipe = tools["emit"], link_script_text = "")
    jobs = []
    ctx = context(tctx, output_directories, runtime_tools, params, recorded = jobs)
    header = ctx.actions.declare_file("generated.h")
    ctx.actions.run(executable = tools["python"], arguments = [tools["codegen"].path, header.path], inputs = [tools["codegen"]], outputs = [header])
    compilers, artifacts, inventory = {}, {}, {}
    parser = ctx.actions.declare_file("parser.c")
    ctx.actions.run(
        executable = tools["cmd_bison"],
        arguments = ["--output", parser.path, originals["parser.y"].path],
        inputs = [originals["parser.y"]],
        outputs = [parser],
        env = json.decode(additional_params["execution_environment"]),
        progress_message = "Generate parser through declared Bison runfiles",
    )
    artifacts["parser.c"] = parser.path
    for source in ["main.c", "other.c"]:
        bitcode = ctx.actions.declare_file(source + ".bc")
        output = ctx.actions.declare_file(source + ".o")
        flags = json.decode(additional_params["flags"])
        ctx.actions.run(
            executable = additional_params["cc"],
            arguments = flags + ["-I", header.dirname, "-emit-llvm", "-Xclang", "-disable-llvm-passes", "-c", originals[source].path, "-o", bitcode.path],
            inputs = depset([tools["source_root"], header], transitive = [tools["compiler_files"]]),
            outputs = [bitcode],
            mnemonic = "FixtureCompile",
            progress_message = "Frontend fixture " + source,
            compiler = dict(phase = "frontend", target = source + ".o", kind = "c", source = originals[source].path),
        )
        anchors = {}
        for file in tools["compiler_files"].to_list():
            anchors[file.path] = {"file": file.path, "parent": 0}
            if not file.is_directory:
                anchors.setdefault(file.dirname, {"file": file.path, "parent": 1})
        arguments = [native_argument(flag, anchors) for flag in flags]
        arguments.extend(["-I", {"parts": [{"file": header.path, "parent": 1}]}, {"parts": ["-ffile-prefix-map=", {"file": header.path, "parent": 1}, "=fixture"]}, "-Wno-unused-command-line-argument", "-c", {"parts": [{"file": bitcode.path, "parent": 0}]}, "-o", {"parts": [{"file": output.path, "parent": 0}]}])
        ctx.actions.run(executable = additional_params["cc"], inputs = [bitcode, originals[source]], outputs = [output], compiler = dict(phase = "backend", target = source + ".o", source = originals[source].path, kind = "c", executable = additional_params["cc"], arguments = arguments, environment = environment))
        product = ctx.actions.declare_file(source + ".product")
        ctx.actions.run(executable = tools["python"], arguments = [tools["runner"].path, "copy", output.path, product.path], inputs = [output, tools["runner"], tools["executor"], tools["payload"]], outputs = [product])
        compilers[source + ".o"] = dict(kind = "c", source = originals[source].path, object = output.path, frontend = bitcode.path, bitcode = bitcode.path)
        inventory[source + ".o"] = dict(kind = "c", source = source, source_file = originals[source].path, directory = ".")
        artifacts[source + ".product"] = product.path

    # Like the native DTB wrapper, the assembly text stays fixed when its
    # producer's binary input changes. Exercise the actual compiler staging
    # and measured-input pruning with that indirect native prerequisite.
    embedded = ctx.actions.declare_file("embedded.bin")
    assembly = ctx.actions.declare_file("embedded.S")
    assembled = ctx.actions.declare_file("embedded.o")
    ctx.actions.run(executable = tools["python"], arguments = [tools["runner"].path, "copy", originals["embedded.txt"].path, embedded.path], inputs = [originals["embedded.txt"], tools["runner"], tools["executor"], tools["payload"]], outputs = [embedded])
    ctx.actions.run(executable = tools["python"], arguments = [tools["emit"].path, assembly.path, '#define PAYLOAD "embedded.bin"\n.section .rodata\n.incbin PAYLOAD\n'], inputs = [tools["emit"], embedded], outputs = [assembly])
    generated = {"embedded.bin": embedded, "embedded.S": assembly, "unused.h": header}
    mapping = ctx.actions.declare_file("embedded-inputs.json")
    ctx.actions.write(mapping, json.encode({name: file.path for name, file in generated.items()}))
    tree = ctx.actions.declare_directory("embedded.headers")
    ctx.actions.run(executable = tools["python"], arguments = [tools["materialize"].path, mapping.path, tree.path], inputs = [tools["materialize"], mapping] + generated.values(), outputs = [tree])
    prerequisites = compiler_prerequisites({"deps": ["embedded.S"]}, "assembly", {"deps": ["embedded.bin"]})
    compile_action(ctx, "embedded.o", "single", assembly, assembled, json.decode(additional_params["flags"]), tree, generated, depset([tools["source_root"]]), struct(files = tools["compiler_files"], tools = {"CC": additional_params["cc"]}), "assembly", [generated[name] for name in prerequisites])
    compilers["embedded.o"] = dict(kind = "assembly", source = assembly.path, object = assembled.path)
    inventory["embedded.o"] = dict(kind = "assembly", source = "embedded.S", source_file = assembly.path, directory = ".")
    artifacts["embedded.o"] = assembled.path

    # The checked-in realmode wrapper reads two products of a native child.
    # Its first binary can stay byte-identical when the relocation data changes.
    relocations = ctx.actions.declare_file("direct.relocs")
    binary = ctx.actions.declare_file("direct.bin")
    direct = ctx.actions.declare_file("direct.o")
    ctx.actions.run(executable = tools["python"], arguments = [tools["runner"].path, "copy", originals["embedded.txt"].path, relocations.path], inputs = [originals["embedded.txt"], tools["runner"], tools["executor"], tools["payload"]], outputs = [relocations])
    ctx.actions.run(executable = tools["python"], arguments = [tools["emit"].path, binary.path, "fixed binary\n"], inputs = [tools["emit"], relocations], outputs = [binary])
    generated = {"direct.bin": binary, "direct.relocs": relocations, "unused.h": header}
    mapping = ctx.actions.declare_file("direct-inputs.json")
    ctx.actions.write(mapping, json.encode({name: file.path for name, file in generated.items()}))
    tree = ctx.actions.declare_directory("direct.headers")
    ctx.actions.run(executable = tools["python"], arguments = [tools["materialize"].path, mapping.path, tree.path], inputs = [tools["materialize"], mapping] + generated.values(), outputs = [tree])
    compile_action(ctx, "direct.o", "single", originals["direct.S"], direct, json.decode(additional_params["flags"]), tree, generated, depset([tools["source_root"], originals["direct.S"]]), struct(files = tools["compiler_files"], tools = {"CC": additional_params["cc"]}), "assembly", [originals["direct.S"], binary, relocations])
    compilers["direct.o"] = dict(kind = "assembly", source = originals["direct.S"].path, object = direct.path)
    inventory["direct.o"] = dict(kind = "assembly", source = "direct.S", source_file = originals["direct.S"].path, directory = ".")
    artifacts["direct.o"] = direct.path
    manifest = ctx.actions.declare_file("kernel.json")
    ctx.actions.write(manifest, json.encode(dict(config = originals["main.c"].path, config_owner = tools["source_root"].path, setup = header.path, compilers = compilers, compiler_inventory = inventory, artifacts = artifacts, members = [])))
    write_schedule(tctx, output_directories["schedule"], jobs)

def _impl(ctx):
    compiler = ctx.attr.compiler[KernelCompilerInfo]
    execution = ctx.toolchains[EXECUTION_TOOLS_TYPE].execution
    execution_files = execution.bootstrap_files
    commands = execution.bootstrap_commands
    sources = ctx.actions.declare_directory(ctx.label.name + ".sources")
    manifest = ctx.actions.declare_file(ctx.label.name + ".sources.json")
    ctx.actions.write(manifest, json.encode(dict(sources = {f.basename: f.path for f in ctx.files.srcs}, commands = {n: f.path for n, f in commands.items()})))
    ctx.actions.run(
        executable = commands["python3"],
        arguments = [ctx.file._prepare.path, manifest.path, sources.path],
        inputs = depset(ctx.files.srcs + [manifest, ctx.file._prepare], transitive = [execution.python_files]),
        outputs = [sources],
    )
    literals = ctx.actions.declare_directory(ctx.label.name + ".graph")
    metadata = ctx.actions.declare_directory(ctx.label.name + ".schedule")
    tools = dict(python = execution.python, python_files = execution.python_files, emit = ctx.file._emit, executor = ctx.file._executor, runner = ctx.file._runner, payload = ctx.file._payload, codegen = ctx.file._codegen, materialize = ctx.file._materialize)
    tools.update({"cmd_" + n: file for n, file in commands.items()})
    tools.update({"compiler_tool_%d" % i: tool for i, tool in enumerate(compiler.tools)})
    tools.update({"execution_tool_%d" % i: tool for i, tool in enumerate(execution.bootstrap_tools)})
    ctx.actions.map_directory(
        input_directories = {"sources": sources},
        output_directories = {"kernel": literals, "schedule": metadata},
        additional_inputs = {"originals": depset(ctx.files.srcs)},
        tools = dict(tools, source_root = sources, compiler_files = compiler.files, execution_files = execution_files),
        additional_params = dict(cc = compiler.roles["CC"], flags = json.encode(compiler.context.spec["compile_flags"]), environment = json.encode(compiler.context.spec["compile_env"]), execution_environment = json.encode(execution.bootstrap_environment), path_environment = json.encode(execution.bootstrap_path_environment)),
        implementation = _plan,
    )
    result = schedule(ctx, literals, metadata, sources, depset(ctx.files.srcs), ctx.files.srcs[0].dirname, tools, compiler.files, execution_files, config = sources, backend_files = compiler.files, backend_tools = compiler.tools, backend_environment = compiler.context.spec["compile_env"], steps = 3)
    return [DefaultInfo(files = depset([result.directory, result.manifest]))]

schedule_fixture = rule(implementation = _impl, toolchains = [EXECUTION_TOOLS_TYPE], attrs = {
    "compiler": attr.label(default = "//:compiler", providers = [KernelCompilerInfo]),
    "srcs": attr.label_list(allow_files = True),
    "_prepare": attr.label(default = "//rules:prepare_source.py", allow_single_file = True),
    "_emit": attr.label(default = "//:emit.py", allow_single_file = True),
    "_executor": attr.label(default = "//:execute.py", allow_single_file = True),
    "_codegen": attr.label(default = "//tests:generate.py", allow_single_file = True),
    "_runner": attr.label(default = "//:scheduler.py", allow_single_file = True),
    "_payload": attr.label(default = "//:payload.py", allow_single_file = True),
    "_materialize": attr.label(default = "//rules:materialize.py", allow_single_file = True),
})
