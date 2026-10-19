load(":commands.bzl", "shell_quote", "shell_words", "value")
load(":execution_tools.bzl", "execution_tools", "run_recipe")
load(":expressions.bzl", "evaluate", "make_words", "program", "source_index")

def _normal(path):
    parts = []
    for p in path.split("/"):
        if p == "..":
            parts.pop()
        elif p and p != ".":
            parts.append(p)
    return "/".join(parts)

def build_objtool(ctx, prepared, architecture, host_flags, link_flags, capabilities, requests, flag_program, package_command):
    source = prepared.root.path
    files = prepared.files.to_list()
    sources = {source + "/" + p: True for p in prepared.logical}
    index = source_index(sources.keys())
    execution = execution_tools(ctx)
    index["__execution_tools__"] = {n: f.path for n, f in execution.commands.items()}

    # These are execution-platform compiler queries. Their command and final
    # objects share the selected host compiler, flags and declared libraries.
    index["__source_root__"] = source
    index["__capabilities__"] = capabilities
    index["__requests__"] = requests
    index["__probe_environment__"] = dict(execution.host_compile_env, CONFIG_SHELL = execution.shell.path)
    selected = {
        "HOSTCC": " ".join([shell_quote(execution.host_cc)] + [shell_quote(flag) for flag in host_flags]),
        "HOSTPKG_CONFIG": package_command.path,
        "AWK": execution.commands["awk"].path,
        "CONFIG_SHELL": execution.shell.path,
        "LIBSUBCMD": "",
    }

    # libsubcmd objects are linked below with their objtool peers, so no
    # separate native archive path is passed to the final host linker.
    initial = dict(selected, SRCARCH = architecture.srcarch, HOSTARCH = "x86", srctree = source, OUTPUT = "", __command_line__ = selected)
    variables = program(flag_program["image_utilities"], initial, index).variables
    variables["AWK"] = {"value": execution.commands["awk"].path}
    variables = program(flag_program["image_host_objtool"], variables, index, cells = True).variables
    generated = []

    # Native Build rules describe the architecture's two generated C tables.
    probe = ctx.actions.declare_file(ctx.label.name + ".generated/.root")
    prefix = probe.dirname + "/"
    ctx.actions.write(probe, "")
    variables["OUTPUT"] = {"value": prefix}
    architecture_program = program(flag_program["image_programs"]["tools/objtool/arch/" + architecture.srcarch + "/Build"], variables, index, cells = True)
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
    build = program(flag_program["image_host_build"], variables, index, cells = True)
    objects = [("tools/objtool/" + p, "objtool") for p in make_words(value("objtool-y", build.variables, index)) if not p.endswith("/")]

    # Each architecture Build has its own object namespace.
    arch = program(flag_program["image_programs"]["tools/objtool/arch/" + architecture.srcarch + "/Build"], dict(variables, **{"objtool-y": {"value": ""}}), index, cells = True)
    objects.extend([("tools/objtool/arch/" + architecture.srcarch + "/" + p, "objtool") for p in make_words(value("objtool-y", arch.variables, index))])
    aliases = {}
    for rule in build.rules:
        tail = make_words(rule["prerequisites"])
        if tail and tail[0].startswith("../"):
            for target in rule["targets"]:
                aliases[_normal("tools/objtool/" + target.removeprefix(prefix))] = _normal("tools/objtool/" + tail[0])
    variables["EXTRA_CFLAGS"] = {"value": value("OBJTOOL_CFLAGS", variables, index)}
    subcmd = program(flag_program["image_host_subcmd"], variables, index, cells = True).variables
    objects.extend([("tools/lib/subcmd/" + p, "subcmd") for p in make_words(value("libsubcmd-y", subcmd, index))])
    outputs = []
    for obj, group in {obj: group for obj, group in objects}.items():
        src = aliases.get(obj, obj[:-2] + ".c")
        flags = shell_words(value("OBJTOOL_CFLAGS", variables, index) if group == "objtool" else value("CFLAGS", subcmd, index))
        if group == "objtool":
            flags += shell_words(value("CFLAGS_" + obj.rsplit("/", 1)[-1], arch.variables, index))
        flags = [f.replace("../", source + "/tools/") for f in flags]
        out = ctx.actions.declare_file("objtool/" + obj + ".o")
        ctx.actions.run(
            executable = execution.host_cc,
            arguments = host_flags + flags + ["-c", source + "/" + src, "-o", out.path],
            inputs = depset(generated, transitive = [prepared.files, execution.files]),
            outputs = [out],
            env = execution.host_compile_env,
        )
        outputs.append(out)
    binary = ctx.actions.declare_file("objtool/objtool")
    ctx.actions.run(
        executable = execution.host_cc,
        arguments = link_flags + [f.path for f in outputs] + shell_words(value("OBJTOOL_LDFLAGS", variables, index)) + ["-o", binary.path],
        inputs = depset(outputs, transitive = [execution.files]),
        outputs = [binary],
        env = execution.host_link_env,
    )
    return struct(executable = binary, architecture = architecture)
