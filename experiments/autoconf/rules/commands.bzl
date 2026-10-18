"""Analysis-time command lowering with explicit inputs and direct LLVM actions."""

load("@kernel_inputs//rules:flag_program.bzl", "FLAG_PROGRAM")
load(":execution_tools.bzl", "execution_tools", "run_recipe")
load(":expressions.bzl", "evaluate", "make_words", "match", "program")

def value(name, variables, sources):
    return evaluate([{"op": "reference", "name": [{"op": "literal", "value": name}]}], variables, sources)

def shell_quote(text):
    return "'" + text.replace("'", "'\\''") + "'"

def _stage_links(files):
    # Preserve the namespaces of declared inputs, including when this ruleset
    # and its source tree live in an external repository.
    roots = {f.path.split("/")[0]: True for f in files}
    return "\n".join(['if [ ! -e %s ]; then ln -s "$exec_root"/%s %s; fi' % (shell_quote(root), shell_quote(root), shell_quote(root)) for root in sorted(roots)])

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

def _scope(setup, directory, target, objtree, source, ancestry = None, memberships = {}):
    variables = dict(setup.variables)
    for key, val in {
        "objtree": objtree,
        "obj": directory,
        "src": source + "/" + directory,
        "@": target,
        "KGZIP": setup.tools["KGZIP"],
        "LEX": setup.tools["LEX"],
        "YACC": setup.tools["YACC"],
        "AWK": setup.tools["AWK"],
    }.items():
        variables[key] = {"value": val}
    inherited_c, inherited_a = [], []
    target_assignments = []
    parts = [] if directory == "." else directory.split("/")
    for parent in ancestry if ancestry != None else (["."] + ["/".join(parts[:i]) for i in range(1, len(parts) + 1)]):
        for name in ["ccflags-y", "asflags-y", "subdir-ccflags-y", "subdir-asflags-y", "HOST_EXTRACFLAGS", "HOST_EXTRACXXFLAGS"]:
            variables[name] = {"value": ""}
        variables["obj"] = {"value": parent}
        variables["src"] = {"value": source + "/" + parent}
        prefix = "" if parent == "." else parent + "/"
        path = prefix + "Kbuild" if prefix + "Kbuild" in FLAG_PROGRAM["directories"] else prefix + "Makefile"
        variables["MAKEFILE_LIST"] = {"value": source + "/" + path}
        scope = program(FLAG_PROGRAM["directories"].get(path, []), variables, setup.sources, cells = True)
        variables = scope.variables
        target_assignments.extend(scope.target_assignments)
        inherited_c.append(value("subdir-ccflags-y", variables, setup.sources))
        inherited_a.append(value("subdir-asflags-y", variables, setup.sources))
    for name, inherited in [("KBUILD_CFLAGS", inherited_c), ("KBUILD_AFLAGS", inherited_a)]:
        variables[name] = {"value": value(name, variables, setup.sources) + " " + " ".join(inherited)}
    variables = program(FLAG_PROGRAM["helpers"], variables, setup.sources, cells = True).variables
    object_scope = program(FLAG_PROGRAM["image_object_helpers"], variables, setup.sources, cells = True)
    variables = object_scope.variables
    for name, text in memberships.items():
        variables[name] = {"value": text}
    target_assignments.extend(object_scope.target_assignments)
    for patterns_first in [True, False]:
        for assignment in target_assignments:
            patterns = make_words(evaluate(assignment["targets_expression"], variables, setup.sources))
            if any(["%" in p for p in patterns]) != patterns_first:
                continue
            if any([match(p.removeprefix("./"), target) != None for p in patterns]):
                node = dict(assignment)
                node["kind"] = "assignment"
                variables = program([node], variables, setup.sources, cells = True).variables
    return variables

def _host_flags(setup, link):
    return setup.host_link_flags if link else setup.host_compile_flags

def _rewrite_flags(flags, tree, source, generated):
    rewritten, include_next = [], ""
    for f in flags:
        if f.startswith(("-Wp,-MMD,", "-Wp,-MD,")):
            continue
        f = f.replace("__object__", tree)
        if f in ("-I", "-include", "-imacros", "-iquote", "-isystem"):
            include_next = f
        elif include_next or f.startswith("-I"):
            path = f if include_next else f[2:]

            # Bare forced headers use the compiler's include search path.
            # Only relocate them when they name an actual generated input.
            search_header = include_next in ("-include", "-imacros") and path not in generated
            if not search_header and path not in (source, tree, "external", "bazel-out") and not path.startswith((source + "/", tree + "/", "external/", "bazel-out/", "/")):
                path = tree + "/" + path
            f = path if include_next else "-I" + path
            include_next = ""
        rewritten.append(f)
    return rewritten

# These helpers register actions through the callback adapter, rather than BUILD macros.
# buildifier: disable=unnamed-macro
def register(ctx, target, directory, kind, driver, outputs, dependencies, files, source, config, setup, object_members = [], native = None, ancestry = None, memberships = {}):
    if kind == "archive":
        variables = dict(setup.variables)
        for key, val in {
            "obj": ".",
            "@": outputs[target].path,
            "AR": setup.tools["AR"],
            "real-prereqs": " ".join([dependencies[p].path for p in object_members]),
        }.items():
            variables[key] = {"value": val}
        expr = FLAG_PROGRAM["image_archives"]["library" if target.endswith("/lib.a") else "builtin"]
        command = evaluate(expr, variables, setup.sources)
        run_recipe(
            ctx,
            execution_tools(ctx),
            command = "set -eu\n" + command,
            inputs = depset(dependencies.values(), transitive = [setup.files]),
            outputs = outputs.values(),
            mnemonic = "KbuildAR",
            progress_message = "Archive %s" % target,
        )
        return

    if kind in ["host", "compile"]:
        link = False
        raw = None
        tree = ctx.actions.declare_directory(outputs.values()[0].basename + ".headers/" + target)
        mapping = ctx.actions.declare_file(tree.basename + ".inputs/" + target + ".json")
        ctx.actions.write(mapping, json.encode({p: f.path for p, f in dependencies.items()}))
        ctx.actions.run(
            executable = execution_tools(ctx).python,
            arguments = [ctx.file._materialize.path, mapping.path, tree.path, directory],
            inputs = depset([ctx.file._materialize, mapping] + dependencies.values(), transitive = [execution_tools(ctx).files]),
            outputs = [tree],
            mnemonic = "KbuildHeaders",
        )

        # Resolve native expressions in the logical object tree; relocate paths
        # only after probes and target-specific decisions have been evaluated.
        variables = _scope(setup, directory, target, "__object__", source, ancestry, memberships)
        flags = []
        extra_inputs = []
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
                compile_flags = _rewrite_flags(compile_flags, tree.path, source, dependencies)
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
            path = target[:-2] + ".c"
            if path not in dependencies and source + "/" + path not in setup.sources and target.endswith(".o"):
                path = target[:-2] + ".S"
            if native != None and native.get("first"):
                path = native["first"].removeprefix("__source__/")
            flags.extend(shell_words(value("a_flags" if path.endswith(".S") else "c_flags", variables, setup.sources)))
            flags.append("-S" if target.endswith(".s") else "-c")
            flags.append(dependencies[path].path if path in dependencies else source + "/" + path)
        flags = _rewrite_flags(flags, tree.path, source, dependencies)
        postprocess = ""
        compiler_outputs = outputs.values()
        if kind == "compile" and target.endswith(".o") and config.make_values.get("CONFIG_OBJTOOL") == "y":
            variables["objtool"] = {"value": dependencies["tools/objtool/objtool"].path}

            # Membership and target-specific arguments use the logical Kbuild
            # name. Freeze those decisions before substituting the action path.
            for name in ["objtool-enabled", "objtool-args"]:
                variables[name] = {"value": value(name, variables, setup.sources)}
            variables["@"] = {"value": outputs[target].path}
            postprocess = value("cmd_objtool", variables, setup.sources).strip()
            if postprocess:
                raw = ctx.actions.declare_file(outputs[target].basename + ".raw/" + target)
                compiler_outputs = [raw]
        flags.extend(["-o", compiler_outputs[0].path])
        args = ctx.actions.args()
        args.add_all(flags)
        action = {
            "arguments": [args],
            "inputs": depset([tree] + dependencies.values() + extra_inputs, transitive = [files, setup.files]),
            "outputs": compiler_outputs,
            "env": (execution_tools(ctx).host_link_env if link else execution_tools(ctx).host_compile_env) if kind == "host" else execution_tools(ctx).environment,
            "mnemonic": (("KbuildHostLink" if link else "KbuildHostCC") if kind == "host" else "KbuildCC"),
            "progress_message": "Compile %s" % target,
        }
        if kind == "compile":
            # Kbuild assembly and inline assembly can use .incbin with a
            # build-root-relative filename. Preserve that namespace in a
            # private working directory, with only declared generated inputs.
            command = """set -eu
exec_root="$PWD"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cp -a %s/. "$work/"
cd "$work"
%s
%s "$@"
""" % (shell_quote(tree.path), _stage_links(files.to_list() + setup.files.to_list()), shell_quote(setup.tools["CC"]) if setup.tools["CC"].startswith("/") else '"$exec_root/"' + shell_quote(setup.tools["CC"]))
            run_recipe(ctx, execution_tools(ctx), command = command, **action)
        else:
            ctx.actions.run(executable = setup.tools["HOSTCC"], **action)
        if postprocess:
            run_recipe(
                ctx,
                execution_tools(ctx),
                command = "set -eu\ncp " + shell_quote(raw.path) + " " + shell_quote(outputs[target].path) + "\nchmod u+w " + shell_quote(outputs[target].path) + "\n" + postprocess.lstrip("; "),
                inputs = depset([raw] + dependencies.values(), transitive = [setup.files]),
                outputs = outputs.values(),
                mnemonic = "KbuildObjtool",
                progress_message = "Objtool %s" % target,
            )
        return
    if driver == "asm_headers":
        ctx.actions.write(outputs[target], "#include <asm-generic/%s>\n" % target.rsplit("/", 1)[-1])
        return
    codegen(ctx, target, directory, outputs, dependencies, files, source, config, setup, native, ancestry)

# These helpers register actions through the callback adapter, rather than BUILD macros.
# buildifier: disable=unnamed-macro
def codegen(ctx, target, directory, outputs, dependencies, files, source, config, setup, native = None, ancestry = None):
    if native != None:
        native_codegen(ctx, target, outputs, dependencies, files, source, config, setup, native)
        return
    variables = _scope(setup, directory, target, "__object__", source, ancestry)
    key = "Kbuild" if directory == "." else directory + "/Kbuild"
    if key not in FLAG_PROGRAM["commands"]:
        key = "Makefile" if directory == "." else directory + "/Makefile"
    for path in ["scripts/Makefile.lib", "scripts/Makefile.host", "scripts/Makefile.build", key, "arch/" + config.architecture.srcarch + "/Makefile"]:
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
            "asm-offsets": "arch/" + config.architecture.srcarch + "/kernel/asm-offsets.s",
            "rq-offsets": "kernel/sched/rq-offsets.s",
            "devicetable-offsets": "scripts/mod/devicetable-offsets.s",
        }.get(stem)
        if not assembly:
            fail("Unsupported offset generator: " + target)
        first = "__object__/" + assembly
    elif "/unistd_" in target or "/syscalls_" in target:
        name = "syshdr" if "/unistd_" in target else "systbl"
        first = source + "/arch/" + config.architecture.srcarch + "/entry/syscalls/syscall_" + ("32" if "32" in target and "x32" not in target else "64") + ".tbl"
        key = "arch/" + config.architecture.srcarch + "/entry/syscalls/Makefile"
        variables = program(FLAG_PROGRAM["commands"][key], variables, setup.sources, cells = True).variables
        for assignment in FLAG_PROGRAM["target_assignments"][key]:
            patterns = make_words(evaluate(assignment["targets_expression"], variables, setup.sources))
            if any([match(p, target) != None for p in patterns]):
                n = evaluate(assignment["name_expression"], variables, setup.sources)
                variables[n] = {"value": evaluate(assignment["value_expression"], variables, setup.sources)}
    else:
        if native != None:
            native_codegen(ctx, target, outputs, dependencies, files, source, config, setup, native)
            return
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
    run_recipe(
        ctx,
        execution_tools(ctx),
        command = "set -eu\n" + command,
        inputs = depset([config.config] + dependencies.values(), transitive = [files, setup.files]),
        outputs = outputs.values(),
        mnemonic = "KbuildGenerate",
        progress_message = "Generate %s" % target,
        env = {"KBUILD_VERBOSE": "0", "srctree": source},
    )

# These helpers register actions through the callback adapter, rather than BUILD macros.
# buildifier: disable=unnamed-macro
def native_codegen(ctx, target, outputs, dependencies, files, source, config, setup, native):
    variables = program(FLAG_PROGRAM["image_command_helpers"], native["variables"], setup.sources, cells = True).variables
    variables.update(native["variables"])
    for name, tool in setup.tools.items():
        variables[name] = {"value": tool}
    prerequisites = []
    for path in native["prerequisites"]:
        if path.startswith("__source__/"):
            prerequisites.append(source + "/" + path[len("__source__/"):])
        elif source + "/" + path in setup.sources:
            prerequisites.append(source + "/" + path)
        else:
            prerequisites.append(path)
    first = prerequisites[0] if prerequisites else ""
    if native.get("first"):
        path = native["first"]
        first = source + "/" + path.removeprefix("__source__/") if path.startswith("__source__/") or source + "/" + path in setup.sources else path
    for name, text in {
        "@": target,
        "@F": target.rsplit("/", 1)[-1],
        "@D": target.rpartition("/")[0] or ".",
        "<": first,
        "^": " ".join(prerequisites),
        "*": native["stem"],
        "real-prereqs": " ".join(prerequisites),
        "depfile": ".native-dependencies.d",
        "tmp-target": ".native-temporary",
        "CONFIG_SHELL": setup.tools["CONFIG_SHELL"],
    }.items():
        variables[name] = {"value": text}
    for assignment in native["assignments"]:
        patterns = make_words(evaluate(assignment["targets_expression"], variables, setup.sources))
        if any([match(p.removeprefix("./"), target) != None for p in patterns]):
            node = dict(assignment)
            node["kind"] = "assignment"
            variables = program([node], variables, setup.sources, cells = True).variables
    arguments = [evaluate(a, variables, setup.sources) for a in native["arguments"]]
    name = arguments[0].strip() if arguments else ""
    for i, argument in enumerate(arguments):
        variables[str(i + 1)] = {"value": argument}
    command_sources = dict(setup.sources)
    command_sources["__shell_commands__"] = True
    if native["function"] == "script":
        lines = []
        for expression in native["scripts"]:
            text = evaluate(expression, variables, command_sources).strip()
            body = text.lstrip("@+-")
            modifiers = text[:len(text) - len(body)]
            lines.append(shell_quote(setup.tools["CONFIG_SHELL"]) + " -c " + shell_quote(body) + (" || :" if "-" in modifiers else ""))
        command = "\n".join(lines) + "\ntouch " + shell_quote(target)
    else:
        command = value(("filechk_" if native["function"] == "filechk" else "cmd_") + name, variables, command_sources).strip()
    if native != None and native.get("final_link"):
        command = shell_quote(setup.tools["CONFIG_SHELL"]) + " " + ctx.file._link_script.path + " " + " ".join([shell_quote(setup.tools["LD"]), shell_quote(value("KBUILD_LDFLAGS", variables, setup.sources)), shell_quote(value("LDFLAGS_vmlinux", variables, setup.sources)), shell_quote(target)])
    if native.get("postlink") != None:
        post = native["postlink"]
        post_variables = dict(variables)
        post_variables.update(post["variables"])
        for key in ["@", "@F", "@D", "<", "^", "*", "real-prereqs", "CONFIG_SHELL"] + list(setup.tools):
            post_variables[key] = variables[key]
        post_name = evaluate(post["arguments"][0], post_variables, setup.sources).strip()
        command += "\n" + value("cmd_" + post_name, post_variables, command_sources)
    if not command:
        fail("No exported native command %r for %s" % (name, target))
    mapping = dict(dependencies)
    mapping.update(outputs)
    for path in sorted(mapping, key = len, reverse = True):
        command = command.replace("__object__/" + path, path)
    command = command.replace("__source__/", source + "/")
    command = command.replace("__object__/", "").replace("__object__", ".")
    if native["function"] == "filechk":
        command = "{ " + command + "; } > " + shell_quote(target)
    stage = ctx.actions.declare_directory(outputs.values()[0].basename + ".native-inputs/" + target)
    manifest = ctx.actions.declare_file(stage.basename + ".mapping/" + target + ".json")
    arranged = {p: f.path for p, f in dependencies.items()}
    arranged["include/config/auto.conf"] = config.config.path
    arranged[".config"] = config.config.path
    ctx.actions.write(manifest, json.encode(arranged))
    ctx.actions.run(
        executable = execution_tools(ctx).python,
        arguments = [ctx.file._materialize.path, manifest.path, stage.path],
        inputs = depset([ctx.file._materialize, manifest, config.config] + dependencies.values(), transitive = [execution_tools(ctx).files]),
        outputs = [stage],
        mnemonic = "KbuildNativeInputs",
    )
    exports = dict(setup.tools)
    for name in ["KBUILD_VMLINUX_LIBS", "KBUILD_LDS", "KERNELRELEASE", "SRCARCH"] + (FLAG_PROGRAM["image_link_exports"] if native.get("final_link") else []):
        if name not in ("KBUILD_VMLINUX_LIBS", "KBUILD_LDS", "KERNELRELEASE", "SRCARCH") and name not in variables.get("__exports__", {}):
            continue
        exports[name] = value(name, variables, setup.sources).replace("__source__", source).replace("__object__", ".")
    exports.update({"objtree": ".", "CONFIG_SHELL": setup.tools["CONFIG_SHELL"], "KBUILD_BUILD_USER": "bazel", "KBUILD_BUILD_HOST": "bazel"})
    command = "\n".join(["export " + name + "=" + shell_quote(val) for name, val in exports.items()]) + "\n" + command

    # Driver-based native links must find the declared linker beside the
    # selected tools, even when the compiler is a multicall-binary symlink.
    linker_directory = setup.driver_tools
    driver_search = shell_quote(linker_directory) if linker_directory.startswith("/") else '"$exec_root"/' + shell_quote(linker_directory)
    command = "export COMPILER_PATH=" + driver_search + "\n" + command
    script = """set -eu
exec_root="$PWD"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cp -R -L -p %s/. "$work/"
chmod -R u+w "$work"
cd "$work"
%s
%s
%s
""" % (shell_quote(stage.path), _stage_links(files.to_list() + setup.files.to_list() + [ctx.file._link_script]), "mkdir -p " + " ".join([shell_quote(p.rpartition("/")[0] or ".") for p in outputs]), command)
    for path, output in outputs.items():
        script += "cp %s %s\n" % (shell_quote(path), shell_quote(output.path))
    run_recipe(
        ctx,
        execution_tools(ctx),
        command = script,
        inputs = depset([stage, config.config, ctx.file._link_script] + dependencies.values(), transitive = [files, setup.files]),
        outputs = outputs.values(),
        mnemonic = "KbuildCC" if native.get("compile") else "KbuildGenerate",
        progress_message = "Native generator %s" % target,
        env = {"KBUILD_VERBOSE": "0", "srctree": source},
    )
