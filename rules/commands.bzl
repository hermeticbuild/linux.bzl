"""Analysis-time command lowering with explicit inputs and direct LLVM actions."""

load(":execution_tools.bzl", "execution_tools", "run_recipe")
load(":expressions.bzl", "evaluate", "make_words", "match", "program", "source_index", "static_value")

def value(name, variables, sources):
    return evaluate([{"op": "reference", "name": [{"op": "literal", "value": name}]}], variables, sources)

def shell_quote(text):
    return "'" + text.replace("'", "'\\''") + "'"

def native_tool_environment(setup):
    """Render selected tools for native scripts that word-split ${CC}."""
    for argument in setup.cc_arguments:
        if not argument or any([char in argument for char in " \t\n*?[".elems()]):
            fail("Native script CC cannot preserve compiler argument: " + repr(argument))
    return dict(setup.tools, CC = " ".join(setup.cc_arguments))

def native_recipe(ctx, target, recipes, sources, setup, files, replacements = []):
    """Expand all Make shell captures before parsing the recipe's first line."""
    captures = sources["__shell_commands__"]
    texts = [recipe["command"] for recipe in recipes] + captures
    for old, new in replacements:
        texts = [text.replace(old, new) for text in texts]
    recipes = [dict(recipe, command = texts[i]) for i, recipe in enumerate(recipes)]
    if not captures:
        if len(recipes) == 1 and recipes[0]["strict"] and not recipes[0]["ignore_errors"]:
            return recipes[0]["command"]
        return "\n".join([
            shell_quote(setup.tools["CONFIG_SHELL"]) + (" -eu" if recipe["strict"] else "") +
            " -c " + shell_quote(recipe["command"]) + (" || :" if recipe["ignore_errors"] else "")
            for recipe in recipes
        ])
    spec = ctx.actions.declare_file(ctx.label.name + ".shell/" + target + ".json")
    ctx.actions.write(spec, json.encode({
        "shell": setup.tools["CONFIG_SHELL"],
        "recipes": recipes,
        "shell_commands": texts[len(recipes):],
    }))
    files.extend([spec, ctx.file._native_recipe])
    return " ".join([shell_quote(path) for path in [execution_tools(ctx).python.path, "-B", ctx.file._native_recipe.path, spec.path]])

def _stage_links(files):
    # Preserve the namespaces of declared inputs, including when this ruleset
    # and its source tree live in an external repository.
    roots = {f.path.split("/")[0]: True for f in files}
    return "\n".join(['if [ ! -e %s ]; then ln -s "$exec_root"/%s %s; fi' % (shell_quote(root), shell_quote(root), shell_quote(root)) for root in sorted(roots)])

def shell_words(text, allow_recipe = False):
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
            if allow_recipe:
                return None
            fail("Shell operation in compiler arguments: " + text)
        else:
            current += c
            active = True
    if quote or escaped:
        fail("Unclosed quote in compiler arguments: " + text)
    if active:
        words.append(current)
    return words

def _target_assignments(variables, assignments, target, sources):
    for patterns_first in [True, False]:
        for assignment in assignments:
            patterns = make_words(evaluate(assignment["targets_expression"], variables, sources))
            if any(["%" in p for p in patterns]) != patterns_first:
                continue
            if any([match(p.removeprefix("./"), target) != None for p in patterns]):
                node = dict(assignment)
                node["kind"] = "assignment"
                variables = program([node], variables, sources, cells = True).variables
    return variables

def command_scope(setup, directory, target, objtree, source, ancestry = None, graph_variables = {}, descriptor = None, invocation_environment = None):
    """Evaluate a selected compiler command with its native graph snapshots."""
    phase = descriptor != None and descriptor.get("phase") != None
    variables = dict(descriptor["variables"] if phase else setup.variables)
    if invocation_environment != None and not phase:
        variables = {name: {"value": cell["value"].replace("__source__", source).replace("__object__", objtree)} for name, cell in invocation_environment.items() if name != "__exports__"}
        variables["__config__"] = setup.variables["__config__"]

        # Imported graph and target cells share this invocation-wide identity.
        variables["__cell_sequence__"] = setup.variables["__cell_sequence__"]
        ancestry = [directory]
    for key, val in {
        "objtree": objtree,
        "srctree": source,
        "srcroot": source,
        "building_out_of_srctree": "1",
        "@": target,
        "*": descriptor["stem"] if descriptor != None else "",
        "KGZIP": setup.tools["KGZIP"],
        "LEX": setup.tools["LEX"],
        "YACC": setup.tools["YACC"],
        "AWK": setup.tools["AWK"],
    }.items():
        variables[key] = {"value": val}
    if phase:
        # Makefile.modfinal already ran in its root invocation. It has no
        # Makefile.build directory initialization or inherited local flags.
        variables.update(descriptor.get("inherited_variables", {}))
        return _target_assignments(variables, descriptor["assignments"], target, setup.sources)
    variables["VPATH"] = {"value": source}
    variables["obj"] = {"value": directory}
    variables = program(setup.program["image_build_init"], variables, setup.sources, cells = True).variables
    if invocation_environment != None:
        variables = program(setup.program["graph_init"] + setup.program["image_common"], variables, setup.sources, cells = True).variables
    inherited_c, inherited_a = [], []
    target_assignments = []
    parts = [] if directory == "." else directory.split("/")
    for parent in ancestry if ancestry != None else (["."] + ["/".join(parts[:i]) for i in range(1, len(parts) + 1)]):
        for name in ["ccflags-y", "asflags-y", "subdir-ccflags-y", "subdir-asflags-y", "HOST_EXTRACFLAGS", "HOST_EXTRACXXFLAGS"]:
            variables[name] = {"value": ""}
        variables["obj"] = {"value": parent}
        variables = program(setup.program["image_build_init"], variables, setup.sources, cells = True).variables
        prefix = "" if parent == "." else parent + "/"
        path = prefix + "Kbuild" if prefix + "Kbuild" in setup.program["image_flag_programs"] else prefix + "Makefile"
        variables["MAKEFILE_LIST"] = {"value": source + "/" + path}
        scope = program([] if path == "Makefile" else setup.program["image_flag_programs"].get(path, []), variables, setup.sources, cells = True)
        variables = scope.variables
        target_assignments.extend(scope.target_assignments)
        inherited_c.append(value("subdir-ccflags-y", variables, setup.sources))
        inherited_a.append(value("subdir-asflags-y", variables, setup.sources))
    for name, inherited in [("KBUILD_CFLAGS", inherited_c), ("KBUILD_AFLAGS", inherited_a)]:
        variables[name] = {"value": value(name, variables, setup.sources) + " " + " ".join(inherited)}

    # Keep Kbuild's graph snapshots and recursive helper flavors. Replaying
    # graph transformations here would prefix paths and expand members twice.
    variables.update(graph_variables)
    variables = program(setup.program["image_helpers"], variables, setup.sources, cells = True).variables
    object_scope = program(setup.program["image_object_helpers"], variables, setup.sources, cells = True)
    variables = object_scope.variables
    if descriptor != None:
        variables.update(descriptor.get("inherited_variables", {}))
    target_assignments.extend(object_scope.target_assignments)
    return _target_assignments(variables, target_assignments, target, setup.sources)

def _host_flags(setup, link):
    return setup.host_link_flags if link else setup.host_compile_flags

def _rewrite_flags(flags, tree, source, generated):
    rewritten, include_next = [], ""
    for f in flags:
        if f.startswith(("-Wp,-MMD,", "-Wp,-MD,")):
            continue
        f = f.replace("__object__", tree).replace("__source__", source)
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

def compiler_flags(command, compiler, source, target, clang):
    """Recognize a single native compiler invocation without interpreting recipes."""
    args = shell_words(command, allow_recipe = True)
    if not args or args[0] != compiler or not any([flag in args for flag in ["-c", "-S", "-E"]]):
        return None
    flags, output_next, source_count, output_count = [], False, 0, 0
    for arg in args[1:]:
        if output_next:
            if arg != target:
                return None
            output_next = False
            output_count += 1
        elif arg == "-o":
            output_next = True
        elif arg == source:
            source_count += 1
        elif arg != "-c":
            flags.append(arg)
    if output_next or output_count != 1 or source_count != 1:
        return None

    # Frontend and integrated checks use distinct private working directories.
    # Keep that staging detail out of IR/debug metadata. Native flags follow
    # this default so an explicitly selected compilation directory still wins.
    return (["-fdebug-compilation-dir=."] if clang else []) + flags

def native_argument(flag, anchors):
    """Bind the command owner's known namespaces before serializing arguments."""
    parts = [flag]
    for path in sorted(anchors, key = len, reverse = True):
        bound = []
        for part in parts:
            if type(part) != "string" or path not in part:
                bound.append(part)
                continue
            pieces = part.split(path)
            for i, piece in enumerate(pieces):
                if i:
                    bound.append(anchors[path])
                if piece:
                    bound.append(piece)
        parts = bound
    return {"parts": parts} if any([type(part) != "string" for part in parts]) else flag

def header_namespace(ctx, path):
    """Anchor compiler paths without copying the transitive native closure."""
    anchor = ctx.actions.declare_file(path + "/.kbuild-directory")
    ctx.actions.write(anchor, "")
    return struct(path = anchor.dirname, anchor = anchor)

def _header_aliases(tree, dependencies):
    aliases = {}
    for logical, file in dependencies.items():
        if not logical or logical == "." or logical.startswith("/") or ".." in logical.split("/"):
            fail("Generated input escapes its header namespace: " + logical)
        aliases[tree.path + "/" + logical] = file
    return aliases

def compile_action(ctx, target, phase, source_file, output, flags, tree, dependencies, files, setup, source_kind, mandatory):
    execution = execution_tools(ctx)
    arguments = flags + (["-c"] if not any([flag in flags for flag in ["-S", "-E"]]) else []) + [source_file.path, "-o", output.path]

    # The scheduler constructs this header namespace in execute.py's private
    # working directory. Keep native build-root-relative .incbin paths there;
    # the executor owns its lifetime and the compiler only reads its aliases.
    command = """set -eu
exec_root="$PWD"
cd %s
%s
%s "$@"
""" % (shell_quote(tree.path), _stage_links(files.to_list() + setup.files.to_list()), '"$exec_root/"' + shell_quote(setup.tools["CC"]))
    compiler = dict(phase = phase, target = target, source = source_file.path, kind = source_kind, executable = setup.tools["CC"], arguments = arguments, environment = execution.backend_environment)
    run_recipe(
        ctx,
        execution,
        command = command,
        arguments = arguments,
        inputs = depset([source_file] + dependencies.values(), transitive = [files, setup.files]),
        outputs = [output],
        mnemonic = "KbuildCC",
        progress_message = "%s compile %s" % (phase, target),
        compiler = compiler,
        scan = dict(argument_start = 3, header_tree = tree.path, generated = {p: f.path for p, f in dependencies.items()}, mandatory = [f.path for f in mandatory], directories = [target.rpartition("/")[0] or "."]),
    )

# tools/objtool/builtin-check.c parses these observed options into checks or
# annotations on the sole object operand opened by objtool_run(). Add an option
# only after auditing its native reads; unknown options retain the source tree.
_OBJTOOL_OBJECT_FLAGS = {
    "--hacks=jump_label": True,
    "--hacks=noinstr": True,
    "--noabs": True,
    "--orc": True,
    "--static-call": True,
    "--uaccess": True,
}

def _objtool_postprocess(command, target, tool):
    literal = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./+-"
    if any([char not in literal for char in (target + tool).elems()]):
        return False
    directory, _, basename = target.rpartition("/")
    dot_target = (directory + "/" if directory else "") + "." + basename

    # This native dependency append writes literal Make text; the wildcard is
    # single-quoted shell data, not an executed command or a source read.
    if command == "{ echo ; echo '%s: $(wildcard %s)' ; } >> %s.cmd" % (target, tool, dot_target):
        return True
    if any([char not in literal + "=, \t" for char in command.elems()]):
        return False
    arguments = [argument for argument in command.replace("\t", " ").split(" ") if argument]
    if len(arguments) < 2 or arguments[0] != tool or arguments[-1] != target:
        return False
    for argument in arguments[1:-1]:
        if argument in _OBJTOOL_OBJECT_FLAGS:
            continue
        return False
    return True

def postprocess_commands(setup, target, variables, commands, dependencies, environment = {}):
    """Resolve native tails once, retaining captures even when their value is empty."""
    command_sources = dict(setup.sources, __shell_commands__ = [], __shell_cache__ = {})
    variables = dict(variables, objtree = {"value": "."})
    for name in setup.program["image_objtool_variables"]:
        if name in variables and "tools/objtool/objtool" in dependencies:
            variables[name] = {"value": dependencies["tools/objtool/objtool"].path}
    expanded = []
    for name in commands:
        command = value(name, variables, command_sources).strip().lstrip("; ")
        if not command:
            continue
        if name in ["cmd_gen_ksymdeps", "cmd_modversions_c", "cmd_modversions_S", "cmd_gen_symversions_c", "cmd_gen_symversions_S"]:
            fail("Native compiler stage requires exported symbol-version metadata: " + name)
        expanded.append(command)
    if not expanded and not command_sources["__shell_commands__"]:
        return None
    tool = dependencies.get("tools/objtool/objtool")

    # objtool parses environment options before argv. Its complete effective
    # environment consists of the declared action values and selected exports.
    environment_options = any([name in environment or name in setup.tools for name in ["OBJTOOL_ARGS", "OBJTOOL_VERBOSE"]])
    object_only = tool != None and not environment_options and not command_sources["__shell_commands__"] and all([
        _objtool_postprocess(command, target, tool.path)
        for command in expanded
    ])
    return struct(commands = expanded, sources = command_sources, object_only = object_only)

def postprocess(ctx, target, raw, output, source_file, post, dependencies, tree, files, source, setup):
    if post == None:
        # Keep the backend output path stable when configuration disables its
        # native tail, without staging the unused source/header namespace.
        run_recipe(ctx, execution_tools(ctx), command = "cp %s %s\nchmod u+w %s" % (shell_quote(raw.path), shell_quote(output.path), shell_quote(output.path)), inputs = [raw], outputs = [output], mnemonic = "KbuildPostprocess", progress_message = "Copy compiler output " + target)
        return
    support = depset(transitive = [files, setup.files])
    if post.object_only:
        # Keep every native prerequisite and tool. The verified object-tool
        # invocation needs neither aggregate source nor header staging.
        support = depset([file for file in support.to_list() if file.path != source])
    recipe_files = []
    command = native_recipe(ctx, target, [{"command": "\n".join(post.commands), "strict": True, "ignore_errors": False}], post.sources, setup, recipe_files, [("__source__", source), ("__object__", ".")])
    command = "\n".join(["export " + name + "=" + shell_quote(tool) for name, tool in native_tool_environment(setup).items()]) + "\n" + command
    script = """set -eu
exec_root="$PWD"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
%s
cd "$work"
%s
mkdir -p %s
cp "$exec_root"/%s %s
chmod u+w %s
%s
cp %s "$exec_root"/%s
""" % ("" if post.object_only else 'cp -R -L -p %s/. "$work/"\nchmod -R u+w "$work"' % shell_quote(tree.path), _stage_links([raw, source_file] + recipe_files + support.to_list()), shell_quote(target.rpartition("/")[0] or "."), shell_quote(raw.path), shell_quote(target), shell_quote(target), command, shell_quote(target), shell_quote(output.path))
    run_recipe(ctx, execution_tools(ctx), command = script, inputs = depset([raw, source_file] + dependencies.values() + recipe_files, transitive = [support]), input_aliases = {} if post.object_only else _header_aliases(tree, dependencies), empty_directories = [] if post.object_only else [tree.path], outputs = [output], mnemonic = "KbuildPostprocess", progress_message = "Postprocess " + target)

def _ir_diagnostics(ctx, flags, tree, dependencies, files, setup):
    execution = execution_tools(ctx)
    report = ctx.actions.declare_file("kernel.validation/diagnostics/report.json")
    spec = ctx.actions.declare_file("kernel.validation/diagnostics/spec.json")
    cases, bitcodes = [], []
    for name, define in [("attribute", "IR_ATTRIBUTE_ERROR"), ("assembly", "IR_ASSEMBLY_ERROR")]:
        bitcode = ctx.actions.declare_file("kernel.validation/diagnostics/" + name + ".bc")
        bitcodes.append(bitcode)
        commands = []
        for phase in ["integrated", "frontend", "backend"]:
            extra = ["-D" + define]
            if phase == "frontend":
                extra.extend(["-emit-llvm", "-Xclang", "-disable-llvm-passes"])
            elif phase == "backend":
                extra.append("-Wno-unused-command-line-argument")
            commands.append({
                "phase": phase,
                "argv": [setup.tools["CC"]] + flags + extra + ["-c", bitcode.path if phase == "backend" else ctx.file._diagnostic_source.path, "-o", bitcode.path if phase == "frontend" else "/dev/null"],
                "environment": execution.backend_environment,
            })
        cases.append({"name": name, "bitcode": bitcode.path, "bitcode_runfile": bitcode.path, "commands": commands})
    ctx.actions.write(spec, json.encode(cases))
    ctx.actions.run(
        executable = execution.python,
        arguments = ["-B", ctx.file._diagnostic_driver.path, spec.path, report.path],
        inputs = depset([spec, ctx.file._diagnostic_driver, ctx.file._diagnostic_source] + dependencies.values(), transitive = [files, setup.files]),
        input_aliases = _header_aliases(tree, dependencies),
        empty_directories = [tree.path],
        outputs = [report] + bitcodes,
        mnemonic = "KernelIRDiagnostics",
    )
    return [report] + bitcodes, {file.path: file.path for file in bitcodes}

def _compile_target(ctx, target, directory, outputs, dependencies, files, source, config, setup, native, ancestry, graph_variables, original_sources, compiler_prerequisites, postprocess_dependencies, invocation_environment, compiler_kind):
    path = target[:-2] + ".c"
    if path not in dependencies and path not in original_sources:
        path = target[:-2] + ".S"
    if native != None and native.get("first"):
        path = native["first"].removeprefix("__source__/")
    source_file = dependencies.get(path, dependencies.get("__source__/" + path, original_sources.get(path)))
    if source_file == None:
        fail("No declared compiler source %s for %s" % (path, target))
    tree = header_namespace(ctx, "kernel.headers/" + target)
    variables = command_scope(setup, directory, target, "__object__", source, ancestry, graph_variables, native, invocation_environment)
    compile_variables = program(setup.program["image_command_helpers"], variables, setup.sources, cells = True).variables
    compile_variables.update(variables)
    compile_variables["<"] = {"value": source_file.path}
    post_variables = dict(compile_variables)
    compile_variables["cmd_objtool"] = {"value": ""}
    compile_variables["cmd_ld_single"] = {"value": ""}
    name = evaluate(native["arguments"][0], variables, setup.sources).strip() if native else ("as_o_S" if path.endswith(".S") else "cc_s_c" if target.endswith(".s") else "cc_o_c")
    post = ["cmd_ld_single", "cmd_objtool"] if target.endswith(".o") else []
    if native != None and native["function"] == "if_changed_rule":
        post = setup.program["image_compile_tails"][name] + ["cmd_" + stage for stage in setup.program["image_compile_postprocess"][name]]
        for tail in setup.program["image_compile_tails"][name]:
            compile_variables[tail] = {"value": ""}
    command = value("cmd_" + name, compile_variables, setup.sources).strip()
    native_flags = compiler_flags(command, setup.tools["CC"], source_file.path, target, config.values.get("CC_IS_CLANG") == "y")
    if native_flags == None:
        fail("Expected one native compiler command for %s: %s" % (target, command))
    flags = _rewrite_flags(native_flags, tree.path, source, dependencies)
    flags.extend(["-I" + tree.path, "-I" + source])
    raw = ctx.actions.declare_file("kernel.raw/" + target) if post else outputs[target]
    post = postprocess_commands(setup, target, post_variables, post, dependencies, execution_tools(ctx).environment)
    split = config.values.get("CC_IS_CLANG") == "y" and path.endswith(".c") and not any([flag in flags for flag in ["-S", "-E"]])
    source_kind = compiler_kind
    mandatory = [source_file] + [file for name, file in compiler_prerequisites.items() if name not in postprocess_dependencies and name != "tools/objtool/objtool"]
    result = dict(kind = source_kind, source = source_file.path, object = raw.path, flags = flags)
    if ctx.planning:
        # Native command and tail evaluation above discovers probes. The
        # remaining compiler lowering only registers actions for the final pass.
        return result
    if split:
        bitcode = ctx.actions.declare_file("kernel.ir/" + target.removesuffix(".o") + ".bc")
        compile_action(ctx, target, "frontend", source_file, bitcode, flags + ["-emit-llvm", "-Xclang", "-disable-llvm-passes"], tree, dependencies, files, setup, source_kind, mandatory)

        # Bind native namespaces to Files; these anchors do not themselves add
        # header/config inputs to the backend. Only actual native prerequisites
        # and the selected compiler closure belong to that spawn.
        anchors = {
            tree.path: {"file": tree.anchor.path, "parent": 1},
            source: {"file": source, "parent": 0},
        }
        for file in execution_tools(ctx).target_compiler_files.to_list():
            anchors[file.path] = {"file": file.path, "parent": 0}
            if not file.is_directory:
                anchors.setdefault(file.dirname, {"file": file.path, "parent": 1})
        arguments = [native_argument(flag, anchors) for flag in flags]
        inputs = {source_file.path: source_file, bitcode.path: bitcode}
        search_roots = {}
        for logical, file in compiler_prerequisites.items():
            if logical in postprocess_dependencies or logical == "tools/objtool/objtool":
                continue
            inputs[file.path] = file
            logical = logical.removeprefix("__source__/")
            if logical in original_sources and original_sources[logical] == file:
                parent = len(logical.split("/"))
            else:
                # Scheduled native products retain their logical suffix under
                # artifacts/. This is the sparse namespace read by .incbin.
                if not file.path.endswith("/" + logical):
                    fail("Native compiler prerequisite lost its logical namespace: " + logical)
                parent = len(logical.split("/"))
            search_roots[(file.path, parent)] = True
        if path in original_sources:
            search_roots[(source_file.path, len(path.split("/")))] = True
        for file_path, parent in search_roots:
            arguments.append({"parts": ["-I", {"file": file_path, "parent": parent}]})
        arguments.extend(["-Wno-unused-command-line-argument", "-c", {"parts": [{"file": bitcode.path, "parent": 0}]}, "-o", {"parts": [{"file": raw.path, "parent": 0}]}])
        ctx.actions.run(executable = setup.tools["CC"], arguments = [], inputs = inputs.values(), outputs = [raw], mnemonic = "KbuildLLVMBackend", progress_message = "Backend compile " + target, compiler = dict(phase = "backend", target = target, source = source_file.path, kind = source_kind, executable = setup.tools["CC"], arguments = arguments, environment = execution_tools(ctx).backend_environment))
        result.update(frontend = bitcode.path, bitcode = bitcode.path)
        if ctx.attr.validate_native_objects:
            integrated = ctx.actions.declare_file("kernel.integrated/" + target)
            compile_action(ctx, target, "integrated", source_file, integrated, flags, tree, dependencies, files, setup, source_kind, mandatory)
            result["integrated"] = integrated.path
            if target == "lib/ctype.o":
                diagnostics, diagnostic_cases = _ir_diagnostics(ctx, flags, tree, dependencies, files, setup)
                result["diagnostics"] = [file.path for file in diagnostics]
                result["diagnostic_cases"] = diagnostic_cases
    else:
        compile_action(ctx, target, "single", source_file, raw, flags, tree, dependencies, files, setup, source_kind, mandatory)
    if raw != outputs[target]:
        postprocess(ctx, target, raw, outputs[target], source_file, post, dependencies, tree, files, source, setup)
    return result

# These helpers register actions through the callback adapter, rather than BUILD macros.
# buildifier: disable=unnamed-macro
def register(ctx, target, directory, kind, driver, outputs, dependencies, files, source, config, setup, object_members = [], native = None, ancestry = None, graph_variables = {}, original_sources = {}, compiler_prerequisites = {}, postprocess_dependencies = {}, invocation_environment = None, compiler_kind = None):
    if kind == "archive":
        variables = dict(setup.variables)
        for key, val in {
            "obj": ".",
            "@": outputs[target].path,
            "AR": setup.tools["AR"],
            "real-prereqs": " ".join([dependencies[p].path for p in object_members]),
        }.items():
            variables[key] = {"value": val}
        expr = setup.program["image_archives"]["library" if target.endswith("/lib.a") else "builtin"]
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
        return None

    if kind == "compile" or (native != None and native.get("compile")):
        return _compile_target(ctx, target, directory, outputs, dependencies, files, source, config, setup, native, ancestry, graph_variables, original_sources, compiler_prerequisites, postprocess_dependencies, invocation_environment, compiler_kind)

    if kind == "host":
        tree = header_namespace(ctx, "kernel.host-headers/" + target)
        variables = command_scope(setup, directory, target, "__object__", source, ancestry, graph_variables, native, invocation_environment)
        link = not target.endswith(".o")
        path = target[:-2] + ".c" if target.endswith(".o") else target + ".c"
        objects = [dependencies[p] for p in object_members]
        result = None
        if not objects:
            source_file = dependencies.get(path, original_sources.get(path))
            if source_file == None:
                fail("No declared host compiler source: " + path)
            obj = ctx.actions.declare_file("kernel.host-objects/" + target + ".o") if link else outputs[target]
            flags = _rewrite_flags(_host_flags(setup, False) + shell_words(value("hostc_flags", variables, setup.sources)), tree.path, source, dependencies)
            flags += ["-c", source_file.path, "-o", obj.path]
            ctx.actions.run(executable = setup.tools["HOSTCC"], arguments = flags, env = execution_tools(ctx).host_compile_env, inputs = depset([source_file] + dependencies.values(), transitive = [files, setup.files]), outputs = [obj], mnemonic = "KbuildHostCC", progress_message = "Compile host " + target, compiler = dict(phase = "single", target = target, source = source_file.path, kind = "host", executable = setup.tools["HOSTCC"], arguments = flags, environment = execution_tools(ctx).host_compile_env), scan = dict(argument_start = 0, header_tree = tree.path, generated = {p: f.path for p, f in dependencies.items()}, mandatory = [source_file.path] + [file.path for file in compiler_prerequisites.values()], directories = [directory]))
            objects.append(obj)
            result = dict(kind = "host", source = source_file.path, object = obj.path, flags = flags)
        if link:
            flags = _host_flags(setup, True) + shell_words(value("KBUILD_HOSTLDFLAGS", variables, setup.sources)) + [obj.path for obj in objects] + shell_words(value("KBUILD_HOSTLDLIBS", variables, setup.sources))
            flags = _rewrite_flags(flags, tree.path, source, dependencies) + ["-o", outputs[target].path]
            ctx.actions.run(executable = setup.tools["HOSTCC"], arguments = flags, env = execution_tools(ctx).host_link_env, inputs = depset(objects + dependencies.values(), transitive = [files, setup.files]), input_aliases = _header_aliases(tree, dependencies), empty_directories = [tree.path], outputs = outputs.values(), mnemonic = "KbuildHostLink", progress_message = "Link host " + target)
        return result
    if driver == "asm_headers":
        ctx.actions.write(outputs[target], "#include <asm-generic/%s>\n" % target.rsplit("/", 1)[-1])
        return None
    codegen(ctx, target, directory, outputs, dependencies, files, source, config, setup, native, ancestry)
    return None

# These helpers register actions through the callback adapter, rather than BUILD macros.
# buildifier: disable=unnamed-macro
def codegen(ctx, target, directory, outputs, dependencies, files, source, config, setup, native = None, ancestry = None):
    if native != None:
        native_codegen(ctx, target, outputs, dependencies, files, source, config, setup, native)
        return
    variables = command_scope(setup, directory, target, "__object__", source, ancestry)
    key = "Kbuild" if directory == "." else directory + "/Kbuild"
    if key not in setup.program["commands"]:
        key = "Makefile" if directory == "." else directory + "/Makefile"
    for path in ["scripts/Makefile.lib", "scripts/Makefile.host", "scripts/Makefile.build", key, "arch/" + config.architecture.srcarch + "/Makefile"]:
        variables = program(setup.program["commands"].get(path, []), variables, setup.sources, cells = True).variables
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
        variables = program(setup.program["commands"][key], variables, setup.sources, cells = True).variables
        for assignment in setup.program["target_assignments"][key]:
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
    command_sources = dict(setup.sources, __shell_commands__ = [], __shell_cache__ = {})
    command = evaluate(expr, variables, command_sources).strip()
    if not command:
        fail("Empty native command for " + target)
    all_files = dict(dependencies)
    all_files.update(outputs)
    replacements = []
    for path in sorted(all_files, key = len, reverse = True):
        replacements.append(("__object__/" + path, all_files[path].path))
    replacements.append(("__config__", config.config.path))
    texts = [command] + command_sources["__shell_commands__"]
    for old, new in replacements:
        texts = [text.replace(old, new) for text in texts]
    if any(["__object__" in text for text in texts]):
        fail("Undeclared generated dependency in %s: %s" % (target, texts))
    if filechk:
        command = "{ " + command + "; } > " + shell_quote(outputs[target].path)
    recipe_files = []
    command = native_recipe(ctx, target, [{"command": command, "strict": True, "ignore_errors": False}], command_sources, setup, recipe_files, replacements)
    run_recipe(
        ctx,
        execution_tools(ctx),
        command = "set -eu\n" + command,
        inputs = depset([config.config] + dependencies.values() + recipe_files, transitive = [files, setup.files]),
        outputs = outputs.values(),
        mnemonic = "KbuildGenerate",
        progress_message = "Generate %s" % target,
        env = dict(execution_tools(ctx).environment, KBUILD_VERBOSE = "0", srctree = source),
    )

# These helpers register actions through the callback adapter, rather than BUILD macros.
# buildifier: disable=unnamed-macro
def native_codegen(ctx, target, outputs, dependencies, files, source, config, setup, native):
    variables = program(setup.program["image_command_helpers"], native["variables"], setup.sources, cells = True).variables
    variables.update(native["variables"])
    for name, tool in setup.make_tools.items():
        variables[name] = {"value": tool}
    pahole = native.get("pahole")
    if pahole != None:
        variables["PAHOLE"] = {"value": pahole.path}
        for name in setup.program["image_btf_shell"]:
            variables[name] = {"value": "${" + name + "}"}
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
    variables.update(native.get("inherited_variables", {}))
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
    command_sources = dict(setup.sources, __shell_commands__ = [], __shell_cache__ = {})

    # Native wildcard tests see the same generated input names staged below.
    for path, entry in source_index(dependencies.keys()).items():
        command_sources[path] = command_sources.get(path, []) + entry if type(entry) == "list" else entry
    recipes = []
    link_script = ctx.file._link_script
    if native.get("final_link"):
        modpost = native.get("script_modpost")
        if modpost != None:
            modpost_variables = dict(modpost["variables"])
            modpost_variables.update({name: {"value": tool} for name, tool in setup.make_tools.items()})
            modpost_variables.update({name: {"value": text} for name, text in {"@": "vmlinux.symvers", "<": modpost["first"], "^": " ".join(modpost["prerequisites"])}.items()})
            modpost_name = evaluate(modpost["arguments"][0], modpost_variables, setup.sources).strip()
            modpost_command = value("cmd_" + modpost_name, modpost_variables, command_sources).replace("__source__", source).replace("__object__", ".")
            if command_sources["__shell_commands__"]:
                fail("Native link-script modpost requires deferred expansion: " + target)
            link_script = ctx.actions.declare_file("kernel.scripts/" + target + "/link-vmlinux.sh")
            ctx.actions.write(link_script, ctx.native_link_script.replace("__KBUILD_VMLINUX_MODPOST__", modpost_command))
        variables["<"] = {"value": link_script.path}
    if native.get("final_link") or native.get("postlink_lowered"):
        variables["ARCH_POSTLINK"] = {"value": ""}
    if native["function"] == "script":
        for expression in native["scripts"]:
            text = evaluate(expression, variables, command_sources).strip()
            body = text.lstrip("@+-")
            modifiers = text[:len(text) - len(body)]
            recipes.append({"command": body, "strict": False, "ignore_errors": "-" in modifiers})
        command = "touch " + shell_quote(target)
    else:
        command = value(("filechk_" if native["function"] == "filechk" else "cmd_") + name, variables, command_sources).strip()
    if native.get("final_link") and not setup.program["image_script_link"]:
        command = shell_quote(setup.tools["CONFIG_SHELL"]) + " " + command
    for stage in native.get("postprocess", []):
        command += "\n" + value("cmd_" + stage, variables, command_sources)
    for post in native.get("postlink", []):
        post_variables = dict(variables)
        post_variables.update(post["variables"])
        for key in ["CONFIG_SHELL"] + list(setup.tools):
            post_variables[key] = variables[key]
        if not post.get("own_automatic_variables"):
            for key in ["@", "@F", "@D", "<", "^", "*", "real-prereqs"]:
                post_variables[key] = variables[key]
        post_name = evaluate(post["arguments"][0], post_variables, setup.sources).strip()
        command += "\n" + value("cmd_" + post_name, post_variables, command_sources)
    if not command:
        fail("No exported native command %r for %s" % (name, target))
    mapping = dict(dependencies)
    mapping.update(outputs)
    replacements = [("__object__/" + path, path) for path in sorted(mapping, key = len, reverse = True)] + [
        ("__source__/", source + "/"),
        ("__object__/", ""),
        ("__object__", "."),
    ]
    if native["function"] == "filechk":
        command = "{ " + command + "; } > " + shell_quote(target)
    recipes.append({"command": command, "strict": True, "ignore_errors": False})
    exports = native_tool_environment(setup)
    if pahole != None:
        exports["PAHOLE"] = pahole.path
    selected_exports = variables.get("__exports__", {})
    for name in sorted(selected_exports):
        if name not in variables:
            continue
        if name in setup.tools:
            continue
        if pahole != None and name in setup.program["image_btf_shell"]:
            continue
        exports[name] = static_value(value(name, variables, command_sources), "native export " + name).replace("__source__", source).replace("__object__", ".")
    exports.update({"objtree": ".", "CONFIG_SHELL": setup.tools["CONFIG_SHELL"], "KBUILD_BUILD_USER": "bazel", "KBUILD_BUILD_HOST": "bazel"})
    export_commands = ["export " + name + "=" + shell_quote(val) for name, val in exports.items()]
    recipe_files = []
    command = native_recipe(ctx, target, recipes, command_sources, setup, recipe_files, replacements)
    stage = ctx.actions.declare_directory(outputs.values()[0].basename + ".native-inputs/" + target)
    manifest = ctx.actions.declare_file(stage.basename + ".mapping/" + target + ".json")
    arranged = {p: f.path for p, f in dependencies.items()}
    arranged["include/config/auto.conf"] = config.auto_conf.path
    arranged[".config"] = config.config.path
    ctx.actions.write(manifest, json.encode(arranged))
    ctx.actions.run(
        executable = execution_tools(ctx).python,
        arguments = [ctx.file._materialize.path, manifest.path, stage.path],
        inputs = depset([ctx.file._materialize, manifest, config.config, config.auto_conf] + dependencies.values(), transitive = [execution_tools(ctx).files]),
        outputs = [stage],
        mnemonic = "KbuildNativeInputs",
    )
    if pahole != None:
        for name, expression in setup.program["image_btf_shell"].items():
            helper_sources = dict(setup.sources, __shell_commands__ = [], __shell_cache__ = {})
            native_command = evaluate(expression, variables, helper_sources).replace("__source__", source).replace("__object__", ".")
            if helper_sources["__shell_commands__"]:
                fail("Native pahole prelude needs nested deferred expansion: " + name)
            export_commands.extend([name + '=\"$(' + native_command + ')\"', "export " + name])
    command = "\n".join(export_commands) + "\n" + command

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
""" % (shell_quote(stage.path), _stage_links(files.to_list() + setup.files.to_list() + [link_script] + recipe_files), "mkdir -p " + " ".join([shell_quote(p.rpartition("/")[0] or ".") for p in outputs]), command)
    for path, output in outputs.items():
        script += "cp %s %s\n" % (shell_quote(path), shell_quote(output.path))
    run_recipe(
        ctx,
        execution_tools(ctx),
        command = script,
        inputs = depset([stage, config.config, link_script] + dependencies.values() + recipe_files, transitive = [files, setup.files]),
        outputs = outputs.values(),
        mnemonic = "KbuildCC" if native.get("compile") else "KbuildGenerate",
        progress_message = "Native generator %s" % target,
        env = dict(execution_tools(ctx).environment, KBUILD_VERBOSE = "0", srctree = source),
    )
