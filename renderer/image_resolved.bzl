"""Resolve configured prerequisite programs and register one action per artifact."""

load(":commands.bzl", "register", "shell_words")
load(":execution_tools.bzl", "EXECUTION_TOOLS_TYPE")
load(":expressions.bzl", "evaluate", "make_words", "match", "program", "source_index")
load(":flag_program.bzl", "FLAG_PROGRAM")
load(":host_tools.bzl", "KernelObjtoolInfo")
load(":kbuild.bzl", "KbuildFileInfo")
load(":resolved.bzl", "KbuildCatalogInfo")
load(":rules.bzl", "KernelConfigInfo")
load(":source_tree.bzl", "KernelSourceTreeInfo")
load(":toolchain.bzl", "KernelToolchainInfo")

def _catalog_impl(ctx):
    programs = {}
    for target in ctx.attr.makefiles:
        info = target[KbuildFileInfo]
        parent, _, name = info.path.rpartition("/")
        if name == "Kbuild" or (name == "Makefile" and parent not in programs):
            programs[parent if parent else "."] = info.statements
    actions = json.decode(ctx.attr.actions)
    return [KbuildCatalogInfo(actions = actions, programs = programs, aliases = {out: p for p, d in actions.items() for out in d["outs"]})]

_catalog = rule(implementation = _catalog_impl, attrs = {
    "actions": attr.string(mandatory = True),
    "makefiles": attr.label_list(providers = [KbuildFileInfo]),
})

def kbuild_catalog(name, actions, makefiles):
    _catalog(name = name, actions = json.encode(actions), makefiles = makefiles)

def _value(name, variables, sources):
    return evaluate([{"op": "reference", "name": [{"op": "literal", "value": name}]}], variables, sources)

def _normalize(path):
    parts = []
    for p in path.split("/"):
        if p == "..":
            if not parts:
                fail("Kbuild dependency escapes its tree: " + path)
            parts.pop()
        elif p and p != ".":
            parts.append(p)
    return "/".join(parts)

def _context(directory, state):
    if directory.startswith("arch/") and directory.split("/")[1] not in (state["arch"], state["setup"].architecture.header_arch):
        fail("Kbuild architecture mismatch: " + directory)
    if directory not in state["contexts"]:
        prefix = "" if directory == "." else directory + "/"
        programs = FLAG_PROGRAM["image_programs"]
        statements = programs.get(prefix + "Kbuild", programs.get(prefix + "Makefile"))
        if statements == None:
            fail("No exported Make program for " + directory)
        initial = dict(state["setup"].variables if directory == "." else state["environment"])
        if directory == ".":
            for name in ["core-y", "libs-y", "drivers-y", "drivers-m"]:
                logical = _value(name, state["setup"].variables, state["setup"].sources).replace("__object__/", "").replace("__object__", ".")
                initial[name] = {"value": logical}
        for k, v in {
            "obj": directory,
            "objtree": ".",
            "src": "__source__/" + directory,
            "srctree": "__source__",
            "srcroot": "__source__",
            "SRCARCH": state["arch"],
            "ARCH": state["setup"].architecture.arch,
            "KCONFIG_CONFIG": ".config",
            "need-builtin": "1",
            "KBUILD_BUILTIN": "1",
        }.items():
            initial[k] = {"value": v}
        initial["__config__"] = state["config"]
        if directory in state["dtbs_lists"]:
            initial["need-dtbslist"] = {"value": "1"}
        local_context = None

        # Setup already evaluated the architecture include. Replaying it here
        # repeats += assignments (including platform load addresses).
        bodies = [FLAG_PROGRAM["image_roots"], FLAG_PROGRAM["graph_init"], statements, FLAG_PROGRAM["graph_helpers"]] if directory == "." else [FLAG_PROGRAM["image_common"], FLAG_PROGRAM["graph_init"], statements, FLAG_PROGRAM["helpers"], FLAG_PROGRAM["graph_helpers"]]
        for body in bodies:
            sources = dict(state["sources"])
            if body == FLAG_PROGRAM["image_arch"]:
                sources["__includes__"] = FLAG_PROGRAM["image_graph_programs"]
            result = program(body, initial, sources, cells = True)
            initial = result.variables
            if body == statements:
                local_context = result

        # Preserve rules from the single local evaluation; reevaluating the
        # Makefile repeats += and path-prefix assignments.
        context = local_context
        dtb_rules = []
        if directory in state["dtbs_lists"] or any([p.endswith((".dtb.o", ".dtbo.o")) for p in make_words(_value("real-obj-y", initial, state["sources"]))]):
            dtbs = program(FLAG_PROGRAM["image_dtbs"], initial, state["sources"], cells = True)
            initial = dtbs.variables
            dtb_rules = dtbs.rules
        implicit = program(FLAG_PROGRAM["image_implicit_rules"], initial, state["sources"], cells = True)
        state["contexts"][directory] = struct(variables = initial, rules = context.rules + dtb_rules + implicit.rules, target_assignments = context.target_assignments)
    return state["contexts"][directory]

def _valid_rule(rule, stem, context, state):
    if not rule.get("implicit") and not any(["%" in p for p in rule["targets"]]):
        return True
    prerequisites = [p for p in make_words(rule["prerequisites"]) if p not in ("FORCE", "|")]
    if not prerequisites:
        return True
    pending = [(_normalize(prerequisites[0].replace("%", stem, 1)), [])]
    seen = {}

    # Native pattern producers may chain, e.g. %.pi.o -> %.o -> %.S.
    for _ in range(10000):
        if not pending:
            return False
        path, chain = pending.pop()
        source_path = path.startswith("__source__/")
        path = path.removeprefix("__source__/")
        key = path + str(chain)
        if key in seen:
            continue
        seen[key] = True
        if path in state["source_files"] or path in state["catalog"].aliases or path in state["known_products"]:
            return True
        for index, candidate in enumerate(context.rules):
            if index in chain or not candidate.get("recipes"):
                continue
            for pattern in candidate["targets"]:
                candidate_stem = match(_normalize(pattern), path)
                if candidate_stem == None:
                    continue
                if "%" not in pattern:
                    return True
                if source_path:
                    continue
                inputs = [p for p in make_words(candidate["prerequisites"]) if p not in ("FORCE", "|")]
                if not inputs:
                    return True
                pending.append((_normalize(inputs[0].replace("%", candidate_stem, 1)), chain + [index]))
    fail("Native pattern search exceeded 10000 prerequisites")

def _rules_for_path(path, context, state):
    explicit, candidate, shortest = [], None, None
    for rule in context.rules:
        for pattern in rule["targets"]:
            stem = match(_normalize(pattern), path)
            if stem == None:
                continue
            if "%" not in pattern:
                explicit.append(rule)
            elif rule.get("recipes") and _valid_rule(rule, stem, context, state) and (shortest == None or len(stem) < shortest):
                candidate, shortest = rule, len(stem)

    # GNU Make merges explicit prerequisites but selects one pattern recipe,
    # preferring the most specific applicable pattern (shortest stem).
    return explicit + ([candidate] if candidate != None and not any([r.get("recipes") for r in explicit]) else [])

def _recipe(path, directory, state):
    context = _context(directory, state)
    prerequisites = []
    for rule in _rules_for_path(path, context, state):
        for pattern in rule["targets"]:
            stem = match(_normalize(pattern), path)
            if stem == None or not _valid_rule(rule, stem, context, state):
                continue
            tail = rule["prerequisites"].split(";", 1)[0]
            if ":" in tail:
                target_pattern, tail = tail.split(":", 1)
                stem = match(_normalize(target_pattern.strip()), path)
                if stem == None:
                    continue
            prerequisites.extend([_normalize(p.replace("%", stem, 1)) for p in make_words(tail) if p not in ("FORCE", "|")])
    prerequisites = {p: True for p in prerequisites}.keys()
    for rule in _rules_for_path(path, context, state):
        for pattern in rule["targets"]:
            stem = match(_normalize(pattern), path)
            if stem == None or not rule.get("recipes") or not _valid_rule(rule, stem, context, state):
                continue
            for terms in rule["recipes"]:
                for term in terms:
                    if term.get("op") != "function" or term.get("name") != "call":
                        continue
                    function = evaluate(term["arguments"][0], context.variables, state["sources"]).strip()
                    if function in ("if_changed", "if_changed_dep", "if_changed_rule", "cmd", "filechk"):
                        tail = rule["prerequisites"].split(";", 1)[0]
                        if ":" in tail:
                            target_pattern, tail = tail.split(":", 1)
                            stem = match(_normalize(target_pattern.strip()), path)
                            if stem == None:
                                continue
                        own_prerequisites = [_normalize(p.replace("%", stem, 1)) for p in make_words(tail) if p not in ("FORCE", "|")]
                        return {
                            "first": own_prerequisites[0] if own_prerequisites else "",
                            "function": function,
                            "arguments": term["arguments"][1:],
                            "variables": context.variables,
                            "prerequisites": prerequisites,
                            "stem": stem,
                            "assignments": context.target_assignments,
                        }
    return None

def _link_descriptor(path, state):
    variables = dict(_context(".", state).variables)
    for name, val in {"objtree": ".", "srctree": "__source__", "srcroot": "__source__"}.items():
        variables[name] = {"value": val}
    libs = make_words(_value("KBUILD_VMLINUX_LIBS", variables, state["sources"]))
    objects = make_words(_value("KBUILD_VMLINUX_OBJS", variables, state["sources"]))
    lds = _value("KBUILD_LDS", variables, state["sources"])
    stages = {
        "built-in-fixup.a": ("vmlinux_a", "ar_builtin_fixup", objects + ["__source__/scripts/head-object-list.txt"], [path]),
        "vmlinux.a": ("vmlinux_a", "copy", ["built-in-fixup.a"], [path]),
        "vmlinux.o": ("vmlinux_o", "ld_vmlinux.o", ["vmlinux.a"] + libs, [path]),
        "vmlinux.symvers": ("modpost", "modpost", ["vmlinux.o", "scripts/mod/modpost"], ["vmlinux.symvers", ".vmlinux.export.c"]),
        ".vmlinux.export.c": ("modpost", "modpost", ["vmlinux.o", "scripts/mod/modpost"], ["vmlinux.symvers", ".vmlinux.export.c"]),
        "vmlinux.unstripped": ("vmlinux", "link_vmlinux", ["vmlinux.o", ".vmlinux.export.o", "init/version-timestamp.o", lds], [path, "System.map"]),
        "vmlinux": ("vmlinux", "strip_relocs", ["vmlinux.unstripped"], [path]),
    }
    if path not in stages:
        return None
    driver, command, deps, outs = stages[path]
    for rule in state["contexts"]["@vmlinux"].rules:
        if path in rule["targets"]:
            deps.extend([_normalize(p) for p in make_words(rule["prerequisites"].split(";", 1)[0]) if p not in ("FORCE", "|")])
    if path == "vmlinux.unstripped":
        outs = outs + state["postlink_outputs"]
        scripts = _context("scripts", state)
        deps.extend(["scripts/" + name for name in make_words(_value("hostprogs-always-y", scripts.variables, state["sources"]))])
    variables = program(FLAG_PROGRAM["image_link"][driver], variables, state["sources"], cells = True).variables
    if driver == "modpost":
        variables["modpost-args"] = {"append": [variables["modpost-args"], {"value": "vmlinux.o"}]}
    native = {
        "function": "cmd",
        "arguments": [[{"op": "literal", "value": command}]],
        "variables": variables,
        "prerequisites": deps,
        "stem": "",
        "assignments": [],
        "final_link": path == "vmlinux.unstripped",
        "postlink": _recipe(path, "@postlink", state) if path == "vmlinux.unstripped" and state["postlink_outputs"] else None,
    }
    return {"target": outs[0], "directory": ".", "outs": outs, "deps": deps, "kind": "generate", "driver": "link", "native": native}

def _prepare_contexts(state, arch_context):
    """Resolve architecture preparation producers from the same native programs."""
    root = _context(".", state)
    state["bootstrap"] = make_words(_value("targets", root.variables, state["sources"]))
    state["preparation"] = []
    state["preparation_checks"] = {}
    state["early_preparation"] = []
    state["preparation_assembly"] = {}
    state["late_preparation"] = {}
    state["late_directories"] = {}
    for name in ["bounds-file", "timeconst-file", "offsets-file", "rq-offsets-file"]:
        state["preparation"].extend(make_words(_value(name, root.variables, state["sources"])))
    state["header_owners"] = {p: "." for p in state["preparation"]}
    for arch in {a: True for a in [state["arch"], state["setup"].architecture.header_arch]}:
        for uapi in ["", "uapi/"]:
            directory = "arch/" + arch + "/include/generated/" + uapi + "asm"
            variables = dict(state["setup"].variables)
            for name, text in {"obj": directory, "SRCARCH": arch, "srctree": "__source__", "srcroot": "__source__", "generic": "include/" + uapi + "asm-generic"}.items():
                variables[name] = {"value": text}
            header = program(FLAG_PROGRAM["image_asm_headers"], variables, state["sources"], cells = True)
            state["contexts"][directory] = header
            for path in make_words(_value("generic-y", header.variables, state["sources"]) + " " + _value("syscall-y", header.variables, state["sources"])):
                state["header_owners"][path] = directory
                state["preparation"].append(path)

    # Architecture Makefiles can declare generated headers outside their own
    # directory. Preserve their rule scope, rather than guessing path owners.
    state["contexts"]["@architecture"] = arch_context
    for rule in arch_context.rules:
        for path in rule["targets"]:
            if "/include/generated/" in path and "%" not in path and rule.get("recipes"):
                state["header_owners"][path] = "@architecture"
                state["preparation"].append(path)

    # Lower native architecture preparation dispatches and expand phony goals.
    work = [(arch_context, "@architecture", phase, phase == "prepare") for phase in ["archscripts", "archheaders", "archprepare", "prepare"]]
    seen = {}
    for _ in range(10000):
        if not work:
            break
        context, directory, goal, late = work.pop()
        if goal == "prepare0":
            continue
        key = directory + ":" + goal
        if key in seen:
            continue
        seen[key] = True
        matching = [r for r in context.rules if goal in r["targets"]]
        if ("/" in goal or "." in goal) and goal not in ("scripts_basic",) and goal not in state["source_files"]:
            for rule in matching:
                for prerequisite in make_words(rule["prerequisites"]):
                    if prerequisite.endswith(".s") and prerequisite not in state["source_files"]:
                        state["bootstrap"].append(prerequisite)
                        state["preparation_assembly"][prerequisite] = True
                        state["header_owners"][prerequisite] = directory
            state["header_owners"][goal] = directory
            state["preparation"].append(goal)
            if late:
                state["late_preparation"][goal] = True
            else:
                state["early_preparation"].append(goal)
            continue
        for r in matching:
            scripts = [expression for expression in r.get("recipes", []) if evaluate(expression, context.variables, state["sources"]).strip() not in ("", "@:", ":")]
            if scripts and all([not evaluate(expression, context.variables, state["sources"]).lstrip("@+- ").startswith("__make_dispatch__") for expression in scripts]):
                # Native phony preparation checks are real actions. Their
                # success marker gives subsequent compile actions an ordering
                # edge without trying to interpret shell syntax in analysis.
                state["preparation_checks"][goal] = {
                    "target": goal,
                    "directory": directory,
                    "outs": [goal],
                    "deps": [d for d in make_words(r["prerequisites"]) if d not in ("FORCE", "scripts_basic")],
                    "kind": "generate",
                    "driver": "build",
                    "native": {
                        "function": "script",
                        "scripts": scripts,
                        "arguments": [],
                        "variables": context.variables,
                        "prerequisites": [],
                        "stem": "",
                        "assignments": [],
                    },
                }
                state["preparation"].append(goal)
                if not late:
                    state["early_preparation"].append(goal)
                continue
            for dependency in make_words(r["prerequisites"]):
                if dependency not in ("FORCE", "scripts_basic"):
                    work.append((context, directory, dependency, late))
            for expression in r.get("recipes", []):
                text = evaluate(expression, context.variables, state["sources"]).strip()
                if not text or text in ("@:", ":"):
                    continue
                words = shell_words(text)
                if len(words) >= 4 and words[:3] == ["__make_dispatch__", "-f", "__source__/Makefile"]:
                    overrides = {word.split("=", 1)[0]: word.split("=", 1)[1] for word in words[3:] if "=" in word}
                    if overrides.keys() != ["ARCH"]:
                        fail("Unsupported native top-level preparation overrides: " + text)
                    arch = overrides["ARCH"]
                    variables = dict(context.variables)
                    variables.update({"ARCH": {"value": arch}, "SRCARCH": {"value": arch}})
                    child = program(FLAG_PROGRAM["image_programs"]["arch/" + arch + "/Makefile"], variables, state["sources"], cells = True)
                    child_directory = "@architecture:" + arch
                    state["contexts"][child_directory] = child
                    for child_goal in words[3:]:
                        if "=" not in child_goal and child_goal != "asm-generic":
                            work.append((child, child_directory, child_goal, late))
                    continue
                if len(words) < 3 or words[0] != "__make_dispatch__" or not words[1].startswith("__build_dispatch__="):
                    fail("Unsupported native architecture preparation dispatch: " + text)
                child = words[1].split("=", 1)[1]
                child_context = _context(child, state)
                if late:
                    state["late_directories"][child] = True
                for child_goal in words[2:]:
                    hostnames = make_words(_value("hostprogs", child_context.variables, state["sources"]) + " " + _value("hostprogs-always-y", child_context.variables, state["sources"]))
                    if child_goal in hostnames:
                        state["preparation"].append(child + "/" + child_goal)
                    else:
                        work.append((child_context, child, child_goal, late))
    if work:
        fail("Architecture preparation graph exceeded 10000 goals")
    state["preparation"] = {p: True for p in state["preparation"]}.keys()

def _describe(path, state):
    if path in state["preparation_checks"]:
        return state["preparation_checks"][path]
    if path in state["postlink_outputs"] or path == "System.map":
        return _link_descriptor("vmlinux.unstripped", state)
    link = _link_descriptor(path, state) if path in ["built-in-fixup.a", "vmlinux.a", "vmlinux.o", "vmlinux.symvers", ".vmlinux.export.c", "vmlinux.unstripped", "vmlinux"] else None
    if link:
        return link
    if path == "include/generated/utsversion.h":
        return {"target": path, "directory": "init", "outs": [path], "deps": [], "kind": "generate", "driver": "build", "native": _recipe(path, "init", state)}
    header_commands = {"include/generated/uapi/linux/version.h": "version.h", "include/generated/utsrelease.h": "utsrelease.h", "include/generated/compile.h": "compile.h"}
    if path in header_commands:
        command = header_commands[path]
        return {
            "target": path,
            "directory": ".",
            "outs": [path],
            "deps": [],
            "kind": "generate",
            "driver": "build",
            "native": {"function": "filechk", "arguments": [[{"op": "literal", "value": command}]], "variables": state["setup"].variables, "prerequisites": [], "stem": "", "assignments": []},
        }
    if path.endswith("/built-in.a") or path == "built-in.a" or path.endswith("/lib.a"):
        directory = path.rpartition("/")[0] or "."
        variables = _context(directory, state).variables
        members = make_words(_value("lib-y" if path.endswith("/lib.a") else "real-obj-y", variables, state["sources"]))

        # Make automatic $^ (and therefore real-prereqs) keeps the first
        # occurrence of a prerequisite, even when multiple options add it.
        members = {_normalize(p): True for p in members}.keys()
        for member in members:
            if member.endswith(("/built-in.a", "/lib.a")):
                child = member.rpartition("/")[0]
                if child != directory:
                    state["parents"][child] = directory
        extra = make_words(_value("always-y", variables, state["sources"]) + " " + _value("extra-y", variables, state["sources"]))
        return {
            "target": path,
            "directory": directory,
            "outs": [path],
            "kind": "archive",
            "driver": "build",
            "deps": members + [_normalize(p) for p in extra],
            "members": members,
        }
    static = state["catalog"].actions.get(state["catalog"].aliases.get(path, path))
    if static != None and (static["kind"] == "compile" or static.get("driver") == "asm_headers" or path in state["header_owners"] or path in state["preparation"]):
        static = None
    directory, _, name = path.rpartition("/")

    # Existing explicit base/preparation edges remain available as anchors.
    if static != None:
        result = dict(static)
        result["deps"] = list(static["deps"])
        if static["kind"] == "compile":
            result["deps"].extend(header_commands)
        if path.endswith(".lds"):
            result["deps"].extend(state["preparation"])
        if path.endswith(".o"):
            generated = path[:-2] + ".c"
            if generated in state["catalog"].aliases:
                result["deps"].append(generated)
        if static["kind"] == "generate" and static.get("driver") != "asm_headers" and directory in state["catalog"].programs:
            result["native"] = _recipe(path, directory, state)
            if result["native"] != None:
                result["deps"].extend(result["native"]["prerequisites"])
        return result
    if path == "include/generated/utsversion.h":
        directory = "init"
    if path in state["link_owners"]:
        directory = "@vmlinux"
    elif path in state["header_owners"]:
        directory = state["header_owners"][path]
    elif path in state["bootstrap"]:
        directory = "."
    if directory not in state["contexts"] and directory not in state["catalog"].programs:
        parts = directory.split("/")
        for i in reversed(range(len(parts))):
            parent = "/".join(parts[:i]) if i else "."
            if parent in state["catalog"].programs:
                directory = parent
                break
    if path.endswith("/dtbs-list") and directory not in state["dtbs_lists"]:
        state["dtbs_lists"][directory] = True
        state["contexts"].pop(directory, None)
    context = _context(directory, state)
    variables = context.variables
    hosts = make_words(_value("hostprogs", variables, state["sources"]) + " " +
                       _value("hostprogs-always-y", variables, state["sources"]) + " " +
                       _value("hostprogs-always-m", variables, state["sources"]))
    host_members = {}
    for host in hosts:
        objects = make_words(_value(host + "-objs", variables, state["sources"]) + " " +
                             _value(host + "-cxxobjs", variables, state["sources"]))
        host_members[host] = [_normalize(directory + "/" + obj) for obj in objects]
    kind = "host" if name in hosts or path in [p for members in host_members.values() for p in members] else "generate"
    dependencies = host_members.get(name, [])
    outs = [path]
    has_recipe = name in hosts
    source = path[:-2] + ".c" if path.endswith((".o", ".s")) else path + ".c"
    if path.endswith((".o", ".s")):
        # Native suffix patterns consume generated lexers/parsers too.
        generated_source = source == ".vmlinux.export.c" or any([match(_normalize(pattern), source) != None and rule["recipe"] and _valid_rule(rule, match(_normalize(pattern), source), context, state) for rule in context.rules for pattern in rule["targets"]])
        if generated_source:
            dependencies.append(source)
            has_recipe = True
        if source in state["source_files"] or source in state["catalog"].actions:
            has_recipe = True
        elif source.endswith(".lex.c") and source[:-6] + ".l" in state["source_files"]:
            dependencies.append(source)
            has_recipe = True
        elif source.endswith(".tab.c") and source[:-6] + ".y" in state["source_files"]:
            dependencies.append(source)
            has_recipe = True
        elif path[:-2] + ".S" in state["source_files"]:
            has_recipe = True
        if source in state["catalog"].actions:
            dependencies.append(source)
        if kind != "host":
            kind = "compile" if has_recipe else "generate"
    for rule in _rules_for_path(path, context, state):
        for pattern in rule["targets"]:
            stem = match(_normalize(pattern), path)
            if stem == None or not _valid_rule(rule, stem, context, state):
                continue
            has_recipe = has_recipe or rule["recipe"]
            if rule["grouped"]:
                outs = [_normalize(p.replace("%", stem, 1)) for p in rule["targets"]]
            tail = rule["prerequisites"].split(";", 1)[0]
            if ":" in tail:
                target_pattern, tail = tail.split(":", 1)
                stem = match(_normalize(target_pattern.strip()), path)
                if stem == None:
                    continue
            dependencies.extend([_normalize(d.replace("%", stem, 1)) for d in make_words(tail) if d not in ("FORCE", "|")])
    native = _recipe(path, directory, state)
    if native != None:
        command = evaluate(native["arguments"][0], native["variables"], state["sources"]).strip()
        if kind != "host" and command in FLAG_PROGRAM["image_compile_commands"]:
            native["compile"] = True
        if kind != "host" and command in ("cc_o_c", "as_o_S"):
            kind = "compile"
    if not has_recipe:
        fail("No native action producer for " + path)
    if kind == "host" and path != "scripts/basic/fixdep":
        dependencies.append("scripts/basic/fixdep")
    if kind == "compile" or (native != None and native.get("compile")) or path.endswith(".lds"):
        # Root Kbuild already declares the ordering among bootstrap assembly
        # generators. Other objects consume the configured preparation outputs.
        if path not in state["bootstrap"]:
            dependencies.extend([p for p in state["preparation"] if directory not in state["late_directories"] or p not in state["late_preparation"]])
        else:
            dependencies.extend([p for p in state["preparation"] if "/include/generated/" in p and p not in state["late_preparation"]])
            if path not in state["preparation_assembly"]:
                dependencies.extend(state["early_preparation"])
        dependencies.extend(header_commands)

        # The final version header is also consumed by native boot wrappers.
        dependencies.append("include/generated/utsversion.h")
    return {"target": path, "directory": "." if directory == "@vmlinux" else directory, "outs": outs, "deps": dependencies, "kind": kind, "driver": "build", "native": native if kind in ("generate", "compile") else None}

def _impl(ctx):
    config = ctx.attr.config[KernelConfigInfo]
    for name, expected in ctx.attr.required_options.items():
        if config.values.get(name, "n") != expected:
            fail("Required kernel option %s=%s resolved to %s" % (name, expected, config.values.get(name, "n")))
    setup = ctx.attr._setup[KernelToolchainInfo]
    if setup.identity != config.identity:
        fail("Mixed native setup and kernel configuration")
    prepared = ctx.attr._prepared_source[KernelSourceTreeInfo]
    files = prepared.files
    source_files = prepared.logical
    index = source_index(["__source__/" + p for p in source_files])
    index["__includes__"] = FLAG_PROGRAM["image_programs"]
    index["__capabilities__"] = setup.sources["__capabilities__"]
    index["__host_machine__"] = setup.sources["__host_machine__"]
    index["__compiler_include__"] = setup.sources.get("__compiler_include__", "")
    index["__execution_tools__"] = setup.sources["__execution_tools__"]
    state = {
        "catalog": ctx.attr.catalog[KbuildCatalogInfo],
        "contexts": {},
        "parents": {},
        "dtbs_lists": {},
        "arch": config.architecture.srcarch,
        "config": config.make_values,
        "source_files": source_files,
        "sources": index,
        "setup": setup,
    }

    # Recursive Make expands only exported parent variables into a child's
    # environment. Unexported names such as core-y must not affect core.o.
    state["environment"] = {name: {"expr": [{"op": "reference", "name": [{"op": "literal", "value": name}]}], "scope": setup.variables, "raw": ""} for name in setup.variables.get("__exports__", {}) if name in setup.variables}
    dispatch = dict(setup.variables)
    for name, text in {"MAKE": "__make_dispatch__", "build": "__build_dispatch__", "Q": "", "KCONFIG_CONFIG": ".config", "srctree": "__source__", "srcroot": "__source__", "objtree": "."}.items():
        dispatch[name] = {"value": text}
    arch_context = program(FLAG_PROGRAM["image_programs"]["arch/" + config.architecture.srcarch + "/Makefile"], dispatch, state["sources"], cells = True)
    state["known_products"] = {}
    _prepare_contexts(state, arch_context)
    link_context = program(FLAG_PROGRAM["image_programs"]["scripts/Makefile.vmlinux"], _context(".", state).variables, state["sources"], cells = True)
    state["contexts"]["@vmlinux"] = link_context
    state["link_owners"] = {p: True for p in make_words(_value("targets", link_context.variables, state["sources"]))}
    postlink = "arch/" + config.architecture.srcarch + "/Makefile.postlink"
    state["postlink_outputs"] = []
    if postlink in FLAG_PROGRAM["image_programs"]:
        post = program(FLAG_PROGRAM["image_programs"][postlink], _context(".", state).variables, state["sources"], cells = True)
        state["contexts"]["@postlink"] = post
        state["postlink_outputs"] = [_normalize(evaluate(expr, post.variables, state["sources"])) for expr in FLAG_PROGRAM["image_postlink_outputs"].get(postlink, [])]
    state["known_products"] = {p: True for p in ["built-in-fixup.a", "vmlinux.a", "vmlinux.o", "vmlinux.symvers", ".vmlinux.export.c", "vmlinux.unstripped", "vmlinux", "System.map", ".config", "include/config/auto.conf", "include/generated/autoconf.h"] + state["postlink_outputs"]}
    pending = list(ctx.attr.roots) + ctx.attr.architecture_roots.get(config.architecture.srcarch, [])
    descriptors, producers = {}, {}
    for _ in range(100000):
        if not pending:
            break
        path = _normalize(pending.pop())
        if path in (".config", "include/config/auto.conf", "include/generated/autoconf.h") or path.startswith("__source__/") or path == "tools/objtool/objtool" or path in source_files or path in producers:
            continue
        descriptor = _describe(path, state)
        descriptor["deps"] = [_normalize(p) for p in descriptor["deps"]]
        descriptors[descriptor["target"]] = descriptor
        for out in descriptor["outs"]:
            if out in producers and producers[out] != descriptor["target"]:
                fail("Multiple configured producers for " + out)
            producers[out] = descriptor["target"]
        pending.extend(descriptor["deps"])
    if pending:
        fail("Kbuild graph exceeded 100000 dependency expansions")

    # Declare every output before registering jobs. This permits a direct,
    # configured topological check and avoids label cycles between alternatives.
    artifacts = {out: ctx.actions.declare_file(ctx.label.name + ".artifacts/" + out) for out in producers}
    common = {"include/generated/autoconf.h": config.header}
    if config.values.get("OBJTOOL") == "y":
        objtool = ctx.attr._objtool[KernelObjtoolInfo]
        if objtool.executable == None:
            fail("CONFIG_OBJTOOL requires a native objtool decoder for SRCARCH=" + config.architecture.srcarch)
        common["tools/objtool/objtool"] = objtool.executable
    finished = {}
    members = []
    for _ in range(len(descriptors) + 1):
        progress = False
        for target, d in descriptors.items():
            if target in finished:
                continue
            deps = {producers[p]: True for p in d["deps"] if p in producers and producers[p] != target}
            if any([p not in finished for p in deps]):
                continue
            closure = dict(common)
            for dep in deps:
                closure.update(finished[dep])
            outputs = {out: artifacts[out] for out in d["outs"]}
            ancestry, parent = [d["directory"]], d["directory"]
            for _parent in range(len(state["parents"]) + 1):
                parent = state["parents"].get(parent, ".")
                if parent == ".":
                    break
                if parent in ancestry:
                    fail("Native directory recursion cycle: " + str(ancestry + [parent]))
                ancestry.append(parent)
            if ancestry[-1] != ".":
                ancestry.append(".")
            ancestry = list(reversed(ancestry))
            memberships = {}
            if d["kind"] == "compile":
                scope = _context(d["directory"], state).variables
                memberships = {name: _value(name, scope, state["sources"]) for name in ["lib-y", "real-obj-y", "real-obj-m", "multi-obj-y", "multi-obj-m", "multi-obj-ym"]}
            register(
                ctx,
                target,
                d["directory"],
                d["kind"],
                d["driver"],
                outputs,
                closure,
                files,
                prepared.root.path,
                config,
                setup,
                d.get("members", [p for p in d["deps"] if p.endswith(".o")] if d["kind"] == "host" and not target.endswith(".o") else []),
                native = d.get("native"),
                ancestry = ancestry,
                memberships = memberships,
            )
            members.append({"target": target, "dependencies": sorted(deps), "outputs": {p: f.path for p, f in outputs.items()}})
            closure.update(outputs)
            finished[target] = closure
            progress = True
        if len(finished) == len(descriptors):
            break
        if not progress:
            blocked = {
                t: [producers[p] for p in d["deps"] if p in producers and producers[p] != t and producers[p] not in finished]
                for t, d in descriptors.items()
                if t not in finished
            }
            trail, positions, current = [], {}, blocked.keys()[0]
            for _edge in range(len(blocked) + 1):
                if current in positions:
                    fail("Configured Kbuild dependency cycle: " + str(trail[positions[current]:] + [current]))
                positions[current] = len(trail)
                trail.append(current)
                current = blocked[current][0]
            fail("Unable to trace configured Kbuild dependency cycle")
    manifest = ctx.actions.declare_file(ctx.label.name + ".json")
    ctx.actions.write(manifest, json.encode({
        "identity": config.identity,
        "config": config.config.path,
        "config_report": config.report.path,
        "setup": setup.file.path,
        "members": members,
        "artifacts": {p: f.path for p, f in artifacts.items()},
    }))
    return [
        DefaultInfo(files = depset([manifest, config.report, setup.file] + artifacts.values())),
        OutputGroupInfo(boot = depset([artifacts[p] for p in ctx.attr.architecture_roots.get(config.architecture.srcarch, [])]), kernel = depset([artifacts["vmlinux"]] if "vmlinux" in artifacts else []), manifest = depset([manifest])),
    ]

_ACTIVE_CONFIG = str(Label("//:active_config"))

# Bazel callback signatures include inputs this implementation does not use.
# buildifier: disable=unused-variable
def _select(settings, attr):
    return {_ACTIVE_CONFIG: str(attr.config)}

_transition = transition(implementation = _select, inputs = [], outputs = [_ACTIVE_CONFIG])

def _setup_impl(ctx):
    config = ctx.attr.config[KernelConfigInfo]
    setup = ctx.attr._setup[KernelToolchainInfo]
    if config.identity != setup.identity:
        fail("Setup snapshot and configuration must match")
    return [config, setup, DefaultInfo(files = depset([config.config, config.report, setup.file]))]

kbuild_resolved_setup = rule(implementation = _setup_impl, cfg = _transition, attrs = {
    "config": attr.label(mandatory = True, providers = [KernelConfigInfo]),
    "_setup": attr.label(default = "//:toolchain", providers = [KernelToolchainInfo]),
    "_allowlist_function_transition": attr.label(default = "@bazel_tools//tools/allowlists/function_transition_allowlist"),
})

kbuild_resolved_build = rule(implementation = _impl, cfg = _transition, toolchains = [EXECUTION_TOOLS_TYPE], attrs = {
    "config": attr.label(mandatory = True, providers = [KernelConfigInfo]),
    "catalog": attr.label(mandatory = True, providers = [KbuildCatalogInfo]),
    "required_options": attr.string_dict(),
    "roots": attr.string_list(mandatory = True),
    "architecture_roots": attr.string_list_dict(),
    "_link_script": attr.label(default = "//rules:link-vmlinux.sh", allow_single_file = True),
    "_objtool": attr.label(default = "//:objtool", providers = [KernelObjtoolInfo]),
    "_setup": attr.label(default = "//:toolchain", providers = [KernelToolchainInfo]),
    "_materialize": attr.label(default = "//rules:materialize.py", allow_single_file = True, cfg = "exec"),
    "_source_tree": attr.label(default = "//:source_tree"),
    "_prepared_source": attr.label(default = "//:prepared_source", providers = [KernelSourceTreeInfo], cfg = "exec"),
    "_anchor": attr.label(default = "//:source/Makefile", allow_single_file = True),
    "_allowlist_function_transition": attr.label(default = "@bazel_tools//tools/allowlists/function_transition_allowlist"),
})
