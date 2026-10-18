load("@kernel_inputs//rules:flag_program.bzl", "FLAG_PROGRAM")
load(":commands.bzl", "shell_quote", "shell_words", "value")
load(":execution_tools.bzl", "execution_tools", "run_recipe")
load(":expressions.bzl", "evaluate", "make_words", "program", "source_index")

KernelObjtoolInfo = provider("Built objtool and its declared inputs.", fields = ["executable", "files"])

def _normal(path):
    parts = []
    for p in path.split("/"):
        if p == "..":
            parts.pop()
        elif p and p != ".":
            parts.append(p)
    return "/".join(parts)

def build_objtool(ctx, prepared, architecture, host_flags, link_flags, capabilities):
    host_architecture = architecture
    source = prepared.root.path
    files = prepared.files.to_list()
    sources = {source + "/" + p: True for p in prepared.logical}
    index = source_index(sources.keys())
    execution = execution_tools(ctx)
    index["__execution_tools__"] = {n: f.path for n, f in execution.commands.items()}

    # The local acceptance test measures the installed libelf API.
    index["__libelf_modern_api__"] = capabilities["__libelf_modern_api__"]
    index["__host_capabilities__"] = capabilities
    variables = program(FLAG_PROGRAM["image_utilities"], {"SRCARCH": architecture.srcarch, "HOSTARCH": host_architecture.srcarch, "srctree": source, "OUTPUT": ""}, index).variables
    variables["AWK"] = {"value": execution.commands["awk"].path}
    variables = program(FLAG_PROGRAM["image_host_objtool"], variables, index, cells = True).variables
    generated = []

    # Native Build rules describe the architecture's two generated C tables.
    probe = ctx.actions.declare_file(ctx.label.name + ".generated/.root")
    prefix = probe.dirname + "/"
    ctx.actions.write(probe, "")
    variables["OUTPUT"] = {"value": prefix}
    architecture_program = program(FLAG_PROGRAM["image_host_arch"]["tools/objtool/arch/" + architecture.srcarch + "/Build"], variables, index, cells = True)
    variables = architecture_program.variables
    for rule in architecture_program.rules:
        if rule["recipe"]:
            target = rule["targets"][0]
            output = ctx.actions.declare_file(ctx.label.name + ".generated/" + target.removeprefix(prefix))
            local = dict(variables)
            local["@"] = {"value": output.path}
            command = "\n".join([evaluate(expr, local, index) for expr in rule.get("recipes", [])])
            command = command.replace("../", source + "/tools/")
            run_recipe(
                ctx,
                execution,
                command = "set -eu\nmkdir -p " + shell_quote(output.dirname) + "\n" + command,
                inputs = depset(files, transitive = [prepared.files]),
                outputs = [output],
                mnemonic = "ObjtoolTable",
            )
            generated.append(output)
    variables["objtool-y"] = {"value": ""}
    build = program(FLAG_PROGRAM["image_host_build"], variables, index, cells = True)
    objects = [("tools/objtool/" + p, "objtool") for p in make_words(value("objtool-y", build.variables, index)) if not p.endswith("/")]

    # Each architecture Build has its own object namespace.
    arch = program(FLAG_PROGRAM["image_host_arch"]["tools/objtool/arch/" + architecture.srcarch + "/Build"], dict(variables, **{"objtool-y": {"value": ""}}), index, cells = True)
    objects.extend([("tools/objtool/arch/" + architecture.srcarch + "/" + p, "objtool") for p in make_words(value("objtool-y", arch.variables, index))])
    aliases = {}
    for rule in build.rules:
        tail = make_words(rule["prerequisites"])
        if tail and tail[0].startswith("../"):
            for target in rule["targets"]:
                aliases[_normal("tools/objtool/" + target.removeprefix(prefix))] = _normal("tools/objtool/" + tail[0])
    subcmd = program(FLAG_PROGRAM["image_host_subcmd"], variables, index, cells = True).variables
    objects.extend([("tools/lib/subcmd/" + p, "subcmd") for p in make_words(value("libsubcmd-y", subcmd, index))])
    outputs = []
    for obj, group in objects:
        src = aliases.get(obj, obj[:-2] + ".c")
        flags = shell_words(value("OBJTOOL_CFLAGS", variables, index) if group == "objtool" else value("CFLAGS", subcmd, index))
        if group == "objtool":
            flags += shell_words(value("CFLAGS_" + obj.rsplit("/", 1)[-1], arch.variables, index))
        flags = [f.replace("../", source + "/tools/") for f in flags]
        out = ctx.actions.declare_file("objtool/" + obj + ".o")
        ctx.actions.run(
            executable = execution.commands["gcc"],
            arguments = host_flags + flags + ["-c", source + "/" + src, "-o", out.path],
            inputs = depset(generated, transitive = [prepared.files, execution.files]),
            outputs = [out],
        )
        outputs.append(out)
    binary = ctx.actions.declare_file("objtool/objtool")
    ctx.actions.run(
        executable = execution.commands["gcc"],
        arguments = link_flags + [f.path for f in outputs] + ["-lelf", "-lz", "-o", binary.path],
        inputs = outputs,
        outputs = [binary],
    )
    return struct(executable = binary, architecture = architecture)
