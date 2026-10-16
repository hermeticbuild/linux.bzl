"""Analysis-time command lowering with explicit inputs and direct LLVM actions."""

load("@rules_cc//cc:action_names.bzl", "ACTION_NAMES")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load(":expressions.bzl", "evaluate", "make_words", "match", "program")
load(":flag_program.bzl", "FLAG_PROGRAM")

def value(name, variables, sources):
    return evaluate([{"op": "reference", "name": [{"op": "literal", "value": name}]}], variables, sources)

def shell_quote(text):
    return "'" + text.replace("'", "'\\''") + "'"

def shell_words(text):
    # Split an already-expanded command argument list, retaining quoted C
    # strings. This does not parse Make expressions or execute shell syntax.
    words, current, quote, escaped, active = [], "", "", False, False
    for c in text.elems():
        if escaped:
            current += c
            escaped = False
        elif c == "\\" and quote != "'":
            escaped = True
            active = True
        elif quote:
            if c == quote:
                quote = ""
            else:
                current += c
        elif c in ["'", '"']:
            quote = c
            active = True
        elif c in [" ", "\t", "\n"]:
            if active:
                words.append(current)
                current, active = "", False
        elif c in [";", "|", "&", "<", ">", "`"]:
            fail("Shell operation in compiler arguments: " + text)
        else:
            current += c
            active = True
    if quote or escaped:
        fail("Unclosed quote in compiler arguments: " + text)
    if active:
        words.append(current)
    return words

def _scope(setup, directory, target, objtree, source):
    variables = dict(setup.variables)
    for key, val in {
        "objtree": objtree,
        "obj": directory,
        "src": source + "/" + directory,
        "@": target,
        "KGZIP": "gzip",
        "LEX": "flex",
        "YACC": "bison",
        "AWK": "awk",
    }.items():
        variables[key] = {"value": val}
    inherited_c, inherited_a = [], []
    parts = [] if directory == "." else directory.split("/")
    for i in range(len(parts) + 1):
        parent = "/".join(parts[:i]) if i else "."
        for name in ["ccflags-y", "asflags-y", "subdir-ccflags-y", "subdir-asflags-y", "HOST_EXTRACFLAGS", "HOST_EXTRACXXFLAGS"]:
            variables[name] = {"value": ""}
        variables["obj"] = {"value": parent}
        variables["src"] = {"value": source + "/" + parent}
        prefix = "" if parent == "." else parent + "/"
        path = prefix + "Kbuild" if prefix + "Kbuild" in FLAG_PROGRAM["directories"] else prefix + "Makefile"
        variables = program(FLAG_PROGRAM["directories"].get(path, []), variables, setup.sources, cells = True).variables
        inherited_c.append(value("subdir-ccflags-y", variables, setup.sources))
        inherited_a.append(value("subdir-asflags-y", variables, setup.sources))
    for name, inherited in [("KBUILD_CFLAGS", inherited_c), ("KBUILD_AFLAGS", inherited_a)]:
        variables[name] = {"value": value(name, variables, setup.sources) + " " + " ".join(inherited)}
    variables = program(FLAG_PROGRAM["helpers"], variables, setup.sources, cells = True).variables
    return variables

def _host_flags(setup, link):
    variables = cc_common.create_link_variables(cc_toolchain = setup.host_cc, feature_configuration = setup.host_features) if link else cc_common.create_compile_variables(cc_toolchain = setup.host_cc, feature_configuration = setup.host_features)
    return cc_common.get_memory_inefficient_command_line(
        feature_configuration = setup.host_features,
        action_name = ACTION_NAMES.cpp_link_executable if link else ACTION_NAMES.c_compile,
        variables = variables,
    )

def _rewrite_flags(flags, tree, source):
    rewritten, include_next = [], False
    for f in flags:
        if f.startswith(("-Wp,-MMD,", "-Wp,-MD,")):
            continue
        f = f.replace("__object__", tree)
        if f == "-I":
            include_next = True
        elif include_next or f.startswith("-I"):
            path = f if include_next else f[2:]
            if not path.startswith((source + "/", tree + "/", "external/", "bazel-out/", "/")):
                path = tree + "/" + path
            f = path if include_next else "-I" + path
            include_next = False
        rewritten.append(f)
    return rewritten

def register(ctx, target, directory, kind, driver, outputs, dependencies, files, source, config, setup, object_members = []):
    if kind in ["host", "compile"]:
        tree = ctx.actions.declare_directory(outputs.values()[0].basename + ".headers/" + target)
        mapping = ctx.actions.declare_file(tree.basename + ".inputs/" + target + ".json")
        ctx.actions.write(mapping, json.encode({p: f.path for p, f in dependencies.items()}))
        ctx.actions.run(
            executable = ctx.file._materialize,
            arguments = [mapping.path, tree.path],
            inputs = [mapping] + dependencies.values(),
            outputs = [tree],
            mnemonic = "KbuildHeaders",
        )
        variables = _scope(setup, directory, target, tree.path, source)
        flags = []
        extra_inputs = []
        link = False
        if kind == "host":
            link = not target.endswith(".o")
            path = target[:-2] + ".c" if target.endswith(".o") else target + ".c"
            source_file = dependencies[path].path if path in dependencies else source + "/" + path
            if link and not object_members:
                # Keep compile and link feature sets separate: LLVM uses its
                # builtin headers for compilation and a runtime resource tree
                # for linking. Concatenating both sets changes header lookup.
                obj = ctx.actions.declare_file(outputs[target].basename + ".host-objects/" + target + ".o")
                compile_flags = _host_flags(setup, False) + shell_words(value("hostc_flags", variables, setup.sources))
                compile_flags = _rewrite_flags(compile_flags, tree.path, source)
                compile_flags += ["-c", source_file, "-o", obj.path]
                ctx.actions.run(
                    executable = setup.tools["HOSTCC"],
                    arguments = compile_flags,
                    inputs = depset([tree] + dependencies.values(), transitive = [files, setup.files]),
                    outputs = [obj],
                    mnemonic = "KbuildHostCC",
                    progress_message = "LLVM compile %s" % target,
                )
                extra_inputs.append(obj)
                flags.append(obj.path)
            elif link:
                flags.extend([dependencies[p].path for p in object_members])
            else:
                flags.extend(["-c", source_file])
            flags = _host_flags(setup, link) + shell_words(value("KBUILD_HOSTLDFLAGS" if link else "hostc_flags", variables, setup.sources)) + flags
            if link:
                flags.extend(shell_words(value("KBUILD_HOSTLDLIBS", variables, setup.sources)))
        else:
            flags.extend(shell_words(value("a_flags" if target.endswith(".S.o") else "c_flags", variables, setup.sources)))
            flags.append("-S" if target.endswith(".s") else "-c")
            path = target[:-2] + ".c"
            flags.append(dependencies[path].path if path in dependencies else source + "/" + path)
        flags = _rewrite_flags(flags, tree.path, source)
        flags.extend(["-o", outputs[target].path])
        args = ctx.actions.args()
        args.add_all(flags)
        ctx.actions.run(
            executable = setup.tools["HOSTCC" if kind == "host" else "CC"],
            arguments = [args],
            inputs = depset([tree] + dependencies.values() + extra_inputs, transitive = [files, setup.files]),
            outputs = outputs.values(),
            mnemonic = ("KbuildHostLink" if link else "KbuildHostCC") if kind == "host" else "KbuildCC",
            progress_message = "LLVM %s" % target,
        )
        return
    if driver == "asm_headers":
        ctx.actions.write(outputs[target], "#include <asm-generic/%s>\n" % target.rsplit("/", 1)[-1])
        return
    codegen(ctx, target, directory, outputs, dependencies, files, source, config, setup)

def codegen(ctx, target, directory, outputs, dependencies, files, source, config, setup):
    variables = _scope(setup, directory, target, "__object__", source)
    key = "Kbuild" if directory == "." else directory + "/Kbuild"
    if key not in FLAG_PROGRAM["commands"]:
        key = "Makefile" if directory == "." else directory + "/Makefile"
    for path in ["scripts/Makefile.lib", "scripts/Makefile.host", "scripts/Makefile.build", key, "arch/x86/Makefile"]:
        variables = program(FLAG_PROGRAM["commands"].get(path, []), variables, setup.sources, cells = True).variables
    variables["src"] = {"value": source + "/" + directory}
    variables["obj"] = {"value": "__object__/" + directory}
    variables["@"] = {"value": "__object__/" + target}
    name, first, header_guard, filechk = "", "", "", False
    if target.endswith(".asn1.c"):
        name, first = "asn1_compiler", source + "/" + target[:-2]
    elif target.endswith(".tab.c"):
        name, first = "bison", source + "/" + target[:-6] + ".y"
    elif target.endswith(".lex.c"):
        name, first = "flex", source + "/" + target[:-6] + ".l"
    elif target.endswith("oid_registry_data.c"):
        name, first = "build_OID_registry", source + "/include/linux/oid_registry.h"
    elif target == "kernel/config_data":
        name, first, filechk = "cat", "__config__", True
    elif target == "kernel/config_data.gz":
        name, first = "gzip", "__object__/kernel/config_data"
    elif target.endswith("/elfconfig.h"):
        name, first = "elfconfig", "__object__/scripts/mod/empty.o"
    elif target.endswith("/timeconst.h"):
        name, first, filechk = "gentimeconst", source + "/kernel/time/timeconst.bc", True
    elif target.endswith("/cpufeaturemasks.h"):
        name, filechk = "gen_featuremasks", True
    elif target.endswith("offsets.h") or target.endswith("/bounds.h"):
        name, filechk = "offsets", True
        stem = target.rsplit("/", 1)[-1][:-2]
        header_guard = {
            "bounds": "__LINUX_BOUNDS_H__",
            "asm-offsets": "__ASM_OFFSETS_H__",
            "rq-offsets": "__RQ_OFFSETS_H__",
            "devicetable-offsets": "__DEVICETABLE_OFFSETS_H__",
        }.get(stem)
        assembly = {
            "bounds": "kernel/bounds.s",
            "asm-offsets": "arch/x86/kernel/asm-offsets.s",
            "rq-offsets": "kernel/sched/rq-offsets.s",
            "devicetable-offsets": "scripts/mod/devicetable-offsets.s",
        }.get(stem)
        if not assembly:
            fail("Unsupported offset generator: " + target)
        first = "__object__/" + assembly
    elif "/unistd_" in target or "/syscalls_" in target:
        name = "syshdr" if "/unistd_" in target else "systbl"
        first = source + "/arch/x86/entry/syscalls/syscall_" + ("32" if "32" in target and "x32" not in target else "64") + ".tbl"
        key = "arch/x86/entry/syscalls/Makefile"
        variables = program(FLAG_PROGRAM["commands"][key], variables, setup.sources, cells = True).variables
        for assignment in FLAG_PROGRAM["target_assignments"][key]:
            patterns = make_words(evaluate(assignment["targets_expression"], variables, setup.sources))
            if any([match(p, target) != None for p in patterns]):
                n = evaluate(assignment["name_expression"], variables, setup.sources)
                variables[n] = {"value": evaluate(assignment["value_expression"], variables, setup.sources)}
    else:
        fail("No direct codegen lowering for " + target)
    variables["<"] = {"value": first}
    variables["^"] = {"value": first}
    variables["KCONFIG_CONFIG"] = {"value": "__config__"}
    expr = [{"op": "function", "name": "call", "arguments": [
        [{"op": "literal", "value": ("filechk_" if filechk else "cmd_") + name}],
        [{"op": "literal", "value": ""}],
        [{"op": "literal", "value": header_guard or ""}],
    ]}]
    command = evaluate(expr, variables, setup.sources).strip()
    if not command:
        fail("Empty native command for " + target)
    all_files = dict(dependencies)
    all_files.update(outputs)
    for path in sorted(all_files, key = len, reverse = True):
        command = command.replace("__object__/" + path, all_files[path].path)
    command = command.replace("__config__", config.config.path)
    if "__object__" in command:
        fail("Undeclared generated dependency in %s: %s" % (target, command))
    if filechk:
        command = "{ " + command + "; } > " + shell_quote(outputs[target].path)
    ctx.actions.run_shell(
        command = "set -eu\n" + command,
        inputs = depset([config.config] + dependencies.values(), transitive = [files, setup.files]),
        outputs = outputs.values(),
        mnemonic = "KbuildGenerate",
        progress_message = "Generate %s" % target,
        env = {"KBUILD_VERBOSE": "0", "srctree": source},
        use_default_shell_env = True,
    )
