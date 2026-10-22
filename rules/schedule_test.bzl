"""Small real-compiler graph exercising generated headers and source pruning."""

load("//execution_tools:defs.bzl", "EXECUTION_TOOLS_TYPE")
load(":commands.bzl", "compile_action", "compiler_flags", "header_namespace", "native_argument", "postprocess", "shell_quote")
load(":compiler.bzl", "KernelCompilerInfo")
load(":execution_tools.bzl", "run_recipe")
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
    setup = struct(files = tools["compiler_files"], tools = {"CC": additional_params["cc"]}, cc_arguments = [additional_params["cc"]])
    header = ctx.actions.declare_file("generated.h")
    large_product = ctx.actions.declare_file("vmlinux.unstripped")
    ctx.actions.run(executable = tools["python"], arguments = [tools["codegen"].path, header.path, large_product.path], inputs = [tools["codegen"]], outputs = [header, large_product])
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
    wrapper = ctx.actions.declare_file("wrapper.h")
    ctx.actions.write(wrapper, "#include <value.h>\n")
    debug_bitcodes = []
    for source, name in [("main.c", "main.c"), ("other.c", "other.c"), ("main.c", "main-repeat.c")]:
        bitcode = ctx.actions.declare_file(name + ".bc")
        output = ctx.actions.declare_file(name + ".o")
        product = ctx.actions.declare_file(name + ".product")

        # Kernel recipes own their flags; unlike the generic toolchain context,
        # they do not already select a reproducible compilation directory.
        native_flags = [flag for flag in json.decode(additional_params["flags"]) if not flag.startswith(("-ffile-compilation-dir=", "-fdebug-compilation-dir="))]
        boot_flags = ["-m16", "-fno-pic"] if additional_params["x86_64"] else []
        native_command = " ".join([shell_quote(argument) for argument in [additional_params["cc"]] + native_flags + ["-g"] + boot_flags + ["-c", originals[source].path, "-o", output.path]])
        flags = compiler_flags(native_command, additional_params["cc"], originals[source].path, output.path, True)
        compile_flags = flags + ["-I", header.dirname]
        integrated = None
        anchors = {}
        if source == "main.c":
            # Native generated mappings can contain original source Files,
            # including the mandatory source and an unused candidate.
            compile_flags += ["-I", tools["source_root"].path, "-include", "wrapper.h"]
            generated = {"main.c": originals[source], "unused.c": originals["other.c"], "generated.h": header, "wrapper.h": wrapper, "__source__/value.h": originals["value.h"], "vmlinux.unstripped": large_product}
            tree = header_namespace(ctx, name + ".headers")
            flags.append("-I" + tree.path)
            compile_flags.append("-I" + tree.path)
            anchors[tree.path] = {"file": tree.anchor.path, "parent": 1}
            compile_action(ctx, name + ".o", "frontend", originals[source], bitcode, compile_flags + ["-emit-llvm", "-Xclang", "-disable-llvm-passes"], tree, generated, depset([tools["source_root"], originals[source]]), setup, "c", [originals[source]])
            debug_bitcodes.append(bitcode)
            integrated = ctx.actions.declare_file(name + ".integrated.o")
            compile_action(ctx, name + ".o", "integrated", originals[source], integrated, compile_flags, tree, generated, depset([tools["source_root"], originals[source]]), setup, "c", [originals[source]])

            # A native tail can read the full prerequisite namespace and
            # modify its private copies, unlike the measured compiler inputs.
            postprocess(ctx, name + ".o", output, product, originals[source], struct(commands = ["test -f vmlinux.unstripped\ntest -w wrapper.h\nprintf private > wrapper.h"], sources = {"__shell_commands__": []}, object_only = False), generated, tree, depset([tools["source_root"]]), tools["source_root"].path, setup)
        else:
            ctx.actions.run(
                executable = additional_params["cc"],
                arguments = compile_flags + ["-emit-llvm", "-Xclang", "-disable-llvm-passes", "-c", originals[source].path, "-o", bitcode.path],
                inputs = depset([tools["source_root"], originals[source], header], transitive = [tools["compiler_files"]]),
                outputs = [bitcode],
                mnemonic = "FixtureCompile",
                progress_message = "Frontend fixture " + name,
                compiler = dict(phase = "frontend", target = name + ".o", kind = "c", source = originals[source].path),
            )
            ctx.actions.run(executable = tools["python"], arguments = [tools["runner"].path, "copy", output.path, product.path], inputs = [output, tools["runner"], tools["executor"], tools["payload"]], outputs = [product])
        for file in tools["compiler_files"].to_list():
            anchors[file.path] = {"file": file.path, "parent": 0}
            if not file.is_directory:
                anchors.setdefault(file.dirname, {"file": file.path, "parent": 1})
        arguments = [native_argument(flag, anchors) for flag in flags]
        arguments.extend(["-I", {"parts": [{"file": header.path, "parent": 1}]}, {"parts": ["-ffile-prefix-map=", {"file": header.path, "parent": 1}, "=fixture"]}, "-Wno-unused-command-line-argument", "-c", {"parts": [{"file": bitcode.path, "parent": 0}]}, "-o", {"parts": [{"file": output.path, "parent": 0}]}])
        ctx.actions.run(executable = additional_params["cc"], inputs = [bitcode, originals[source]], outputs = [output], compiler = dict(phase = "backend", target = name + ".o", source = originals[source].path, kind = "c", executable = additional_params["cc"], arguments = arguments, environment = environment))
        if integrated != None:
            parity = ctx.actions.declare_file(name + ".parity")
            run_recipe(ctx, ctx.execution, command = "cmp %s %s\nprintf passed > %s" % (shell_quote(output.path), shell_quote(integrated.path), shell_quote(parity.path)), inputs = [output, integrated], outputs = [parity])
        compilers[name + ".o"] = dict(kind = "c", source = originals[source].path, object = output.path, frontend = bitcode.path, bitcode = bitcode.path)
        if integrated != None:
            compilers[name + ".o"]["integrated"] = integrated.path
        inventory[name + ".o"] = dict(kind = "c", source = source, source_file = originals[source].path, directory = ".")
        artifacts[name + ".product"] = product.path

    deterministic = ctx.actions.declare_file("debug-directory.parity")
    first, second = debug_bitcodes
    run_recipe(ctx, ctx.execution, command = "cmp %s %s\nprintf passed > %s" % (shell_quote(first.path), shell_quote(second.path), shell_quote(deterministic.path)), inputs = debug_bitcodes, outputs = [deterministic])

    # Like the native DTB wrapper, the assembly text stays fixed when its
    # producer's binary input changes. Exercise the actual compiler staging
    # and measured-input pruning with that indirect native prerequisite.
    embedded = ctx.actions.declare_file("embedded.bin")
    assembly = ctx.actions.declare_file("embedded.S")
    assembled = ctx.actions.declare_file("embedded.o")
    ctx.actions.run(executable = tools["python"], arguments = [tools["runner"].path, "copy", originals["embedded.txt"].path, embedded.path], inputs = [originals["embedded.txt"], tools["runner"], tools["executor"], tools["payload"]], outputs = [embedded])
    ctx.actions.run(executable = tools["python"], arguments = [tools["emit"].path, assembly.path, '#define PAYLOAD "embedded.bin"\n.section .rodata\n.incbin PAYLOAD\n'], inputs = [tools["emit"], embedded], outputs = [assembly])
    generated = {"embedded.bin": embedded, "embedded.S": assembly, "unused.h": header, "vmlinux.unstripped": large_product}
    tree = header_namespace(ctx, "embedded.headers")
    prerequisites = compiler_prerequisites({"deps": ["embedded.S"]}, "assembly", {"deps": ["embedded.bin"]})
    compile_action(ctx, "embedded.o", "single", assembly, assembled, json.decode(additional_params["flags"]), tree, generated, depset([tools["source_root"]]), setup, "assembly", [generated[name] for name in prerequisites])
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
    generated = {"direct.bin": binary, "direct.relocs": relocations, "unused.h": header, "vmlinux.unstripped": large_product}
    tree = header_namespace(ctx, "direct.headers")
    compile_action(ctx, "direct.o", "single", originals["direct.S"], direct, json.decode(additional_params["flags"]), tree, generated, depset([tools["source_root"], originals["direct.S"]]), setup, "assembly", [originals["direct.S"], binary, relocations])
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
    tools = dict(python = execution.python, python_files = execution.python_files, emit = ctx.file._emit, executor = ctx.file._executor, runner = ctx.file._runner, payload = ctx.file._payload, codegen = ctx.file._codegen)
    tools.update({"cmd_" + n: file for n, file in commands.items()})
    tools.update({"compiler_tool_%d" % i: tool for i, tool in enumerate(compiler.tools)})
    tools.update({"execution_tool_%d" % i: tool for i, tool in enumerate(execution.bootstrap_tools)})
    ctx.actions.map_directory(
        input_directories = {"sources": sources},
        output_directories = {"kernel": literals, "schedule": metadata},
        additional_inputs = {"originals": depset(ctx.files.srcs)},
        tools = dict(tools, source_root = sources, compiler_files = compiler.files, execution_files = execution_files),
        additional_params = dict(cc = compiler.roles["CC"], flags = json.encode(compiler.context.spec["compile_flags"]), environment = json.encode(compiler.context.spec["compile_env"]), execution_environment = json.encode(execution.bootstrap_environment), path_environment = json.encode(execution.bootstrap_path_environment), x86_64 = ctx.target_platform_has_constraint(ctx.attr._x86_64[platform_common.ConstraintValueInfo])),
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
    "_x86_64": attr.label(default = "@platforms//cpu:x86_64"),
})
