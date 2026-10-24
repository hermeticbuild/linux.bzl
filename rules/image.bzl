"""Resolve configured prerequisite programs and register one action per artifact."""

load(":commands.bzl", "register", "shell_words")
load(":expressions.bzl", "evaluate", "make_words", "match", "program", "source_index")
load(":recipe_check.bzl", "native_recipe_check")

def compiler_prerequisites(descriptor, kind, source_descriptor = None):
    """Retain native data edges that preprocessing cannot discover."""
    prerequisites = list(descriptor.get("compiler_prerequisites", descriptor["deps"]))
    if kind == "assembly" and source_descriptor != None:
        # Generated assembly can embed its producer's binary input with
        # .incbin. Clang's preprocessor depfile cannot report assembler reads.
        # Preserve only that direct native edge; C input discovery continues
        # to depend on generated source bytes, not the generator's ancestry.
        prerequisites.extend(source_descriptor["deps"])
    return {path: True for path in prerequisites}.keys()

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

def _exported_environment(variables, sources):
    environment = {name: {"value": _value(name, variables, sources)} for name in variables.get("__exports__", {}) if name in variables}
    environment["__exports__"] = {name: True for name in environment}
    return environment

def _build_dispatch(words):
    if len(words) < 3 or words[0] != "__make_dispatch__" or not words[1].startswith("__build_dispatch__="):
        return None
    if any(["=" in word or word.startswith("-") for word in words[2:]]):
        fail("Unsupported recursive Kbuild arguments: " + str(words))
    return struct(directory = _normalize(words[1].split("=", 1)[1]), goals = words[2:])

def _enter_invocation(directory, variables, visibility, state):
    invocation = struct(environment = _exported_environment(variables, state["sources"]), visibility = {p: True for p in visibility}.keys())
    if (directory in state["contexts"] or directory in state["invocations"]) and state["invocations"].get(directory) != invocation:
        fail("Kbuild producer reached %s with a different invocation context" % directory)
    state["invocations"][directory] = invocation

def directory_context(directory, state):
    """Evaluate one selected native directory and retain its graph cells."""
    if directory.startswith("arch/") and directory.split("/")[1] not in (state["arch"], state["setup"].architecture.header_arch):
        fail("Kbuild architecture mismatch: " + directory)
    if directory not in state["contexts"]:
        prefix = "" if directory == "." else directory + "/"
        programs = state["program"]["image_programs"]
        statements = programs.get(prefix + "Kbuild", programs.get(prefix + "Makefile"))
        if statements == None:
            fail("No exported Make program for " + directory)
        invocation = state["invocations"].get(directory)
        initial = dict(state["setup"].variables if directory == "." else invocation.environment if invocation != None else state["environment"])
        if directory == ".":
            for name in ["core-y", "libs-y", "drivers-y", "drivers-m"]:
                logical = _value(name, state["setup"].variables, state["setup"].sources).replace("__object__/", "").replace("__object__", ".")
                initial[name] = {"value": logical}
        builtin, modorder = state["directory_modes"].get(directory, (True, bool(state["modules"])))
        for k, v in {
            "obj": directory,
            "objtree": ".",
            "srctree": "__source__",
            "VPATH": "__source__",
            "building_out_of_srctree": "1",
            "srcroot": "__source__",
            "SRCARCH": state["arch"],
            "ARCH": state["setup"].architecture.arch,
            "KCONFIG_CONFIG": ".config",
            "need-builtin": "1" if builtin else "",
            "need-modorder": "1" if modorder else "",
            "KBUILD_BUILTIN": "1",
            "KBUILD_MODULES": "y" if state["modules"] else "",
        }.items():
            initial[k] = {"value": v}
        initial["__config__"] = state["config"]

        # Cells later cross into command scopes; their cache IDs must stay unique.
        initial["__cell_sequence__"] = state["setup"].variables["__cell_sequence__"]
        if directory in state["dtbs_lists"]:
            initial["need-dtbslist"] = {"value": "1"}
        initial = program(state["program"]["image_build_init"], initial, state["sources"], cells = True).variables
        local_context = None
        graph_variables = {}
        root_descent = None

        # Setup already evaluated the architecture include. Replaying it here
        # repeats += assignments (including platform load addresses).
        bodies = [state["program"]["image_roots"], state["program"]["graph_init"], statements, state["program"]["graph_helpers"]] if directory == "." else [state["program"]["image_common"], state["program"]["graph_init"], statements, state["program"]["graph_helpers"], state["program"]["image_helpers"]]
        for body in bodies:
            sources = dict(state["sources"])
            result = program(body, initial, sources, cells = True)
            initial = result.variables
            if directory == "." and body == state["program"]["image_roots"] and state["program"]["image_root_descent"]:
                root_descent = program(state["program"]["image_root_descent"], initial, sources, cells = True)
            if body == statements:
                local_context = result
            if body == state["program"]["graph_helpers"]:
                for pattern in state["program"]["graph_variables"]:
                    if pattern.endswith("*"):
                        graph_variables.update({name: cell for name, cell in initial.items() if name.startswith(pattern[:-1])})
                    elif pattern in initial:
                        graph_variables[pattern] = initial[pattern]

        # Preserve rules from the single local evaluation; reevaluating the
        # Makefile repeats += and path-prefix assignments.
        context = local_context
        dtb_rules = []
        if directory in state["dtbs_lists"] or any([p.endswith((".dtb.o", ".dtbo.o")) for p in make_words(_value("real-obj-y", initial, state["sources"]))]):
            dtbs = program(state["program"]["image_dtbs"], initial, state["sources"], cells = True)
            initial = dtbs.variables
            dtb_rules = dtbs.rules
        implicit = program(state["program"]["image_implicit_rules"], initial, state["sources"], cells = True)
        initial = implicit.variables
        state["contexts"][directory] = struct(variables = initial, graph_variables = graph_variables, rules = context.rules + dtb_rules + implicit.rules, target_assignments = context.target_assignments)
        builtins = make_words(_value("subdir-builtin", initial, state["sources"]))
        modorders = make_words(_value("subdir-modorder", initial, state["sources"]))
        children = make_words(_value("subdir-ym", initial, state["sources"]))
        modes = {child: (child + "/built-in.a" in builtins, child + "/modules.order" in modorders) for child in children}
        if root_descent != None:
            descend = [r for r in root_descent.rules if "descend" in r["targets"]]
            if len(descend) != 1 or descend[0].get("recipes"):
                fail("Unsupported native top-level descent: " + str(descend))
            children = make_words(descend[0]["prerequisites"])
            for child in children:
                rules = [r for r in root_descent.rules if child in r["targets"] and r.get("recipes")]
                if len(rules) != 1 or len(rules[0]["recipes"]) != 1:
                    fail("Expected one native directory invocation for " + child)
                variables = dict(root_descent.variables)
                for name, text in {"@": child, "MAKE": "__make_dispatch__", "build": "__build_dispatch__", "Q": ""}.items():
                    variables[name] = {"value": text}
                words = shell_words(evaluate(rules[0]["recipes"][0], variables, state["sources"]).lstrip("@+"))
                if words[:2] != ["__make_dispatch__", "__build_dispatch__=" + child] or any(["=" not in word for word in words[2:]]):
                    fail("Unsupported native directory invocation: " + str(words))
                assignments = dict([word.split("=", 1) for word in words[2:]])
                if assignments != {"single-build": "", "need-builtin": "1", "need-modorder": "1"}:
                    fail("Unsupported native full-image directory modes: " + str(assignments))
                modes[child] = (bool(assignments["need-builtin"]), bool(assignments["need-modorder"]))
        state["children"][directory] = children
        for child in children:
            child = _normalize(child)
            if child in state["parents"] and state["parents"][child] != directory:
                fail("Kbuild directory %s has conflicting parents %s and %s" % (child, state["parents"][child], directory))
            state["parents"][child] = directory
            state["directory_modes"][child] = modes[child]
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

def _target_scope(path, directory, state):
    """Keep the first-reached scope within its native Make invocation."""
    context = directory_context(directory, state)
    owner, inherited = state["target_scopes"].get(path, (None, {}))
    inherited = inherited if owner == directory else {}
    variables = dict(context.variables)
    variables.update(inherited)
    descendants = dict(inherited)
    for patterns_first in [True, False]:
        for assignment in context.target_assignments:
            patterns = make_words(evaluate(assignment["targets_expression"], context.variables, state["sources"]))
            if any(["%" in p for p in patterns]) != patterns_first:
                continue
            if any([match(_normalize(p), path) != None for p in patterns]):
                node = dict(assignment)
                node["kind"] = "assignment"
                variables = program([node], variables, state["sources"], cells = True).variables
                name = evaluate(node["name_expression"], variables, state["sources"])
                if not node.get("private"):
                    descendants[name] = variables[name]
                    if node.get("export"):
                        descendants["__exports__"] = variables["__exports__"]
    return struct(variables = variables, inherited = inherited, descendants = descendants)

def rule_prerequisites(rule, path, stem, variables, sources):
    if "secondary_expression" not in rule:
        return rule["prerequisites"]
    scope = dict(variables)
    for name, text in {"@": path, "@F": path.rsplit("/", 1)[-1], "@D": path.rpartition("/")[0] or ".", "*": stem}.items():
        scope[name] = {"value": text}
    scope.update(rule["secondary_inputs"])
    return evaluate(rule["secondary_expression"], scope, sources)

def _local_recipe(path, directory, state):
    context = directory_context(directory, state)
    scope = _target_scope(path, directory, state)
    prerequisites = []
    for rule in _rules_for_path(path, context, state):
        for pattern in rule["targets"]:
            stem = match(_normalize(pattern), path)
            if stem == None or not _valid_rule(rule, stem, context, state):
                continue
            tail = rule_prerequisites(rule, path, stem, scope.variables, state["sources"]).split(";", 1)[0]
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
                    if function in ("if_changed", "if_changed_dep", "if_changed_rule", "if_changed_except", "cmd", "filechk"):
                        tail = rule_prerequisites(rule, path, stem, scope.variables, state["sources"]).split(";", 1)[0]
                        if ":" in tail:
                            target_pattern, tail = tail.split(":", 1)
                            stem = match(_normalize(target_pattern.strip()), path)
                            if stem == None:
                                continue
                        own_prerequisites = [_normalize(p.replace("%", stem, 1)) for p in make_words(tail) if p not in ("FORCE", "|")]
                        return {
                            "directory": directory,
                            "first": own_prerequisites[0] if own_prerequisites else "",
                            "function": function,
                            "arguments": term["arguments"][1:],
                            "variables": context.variables,
                            "prerequisites": prerequisites,
                            "stem": stem,
                            "assignments": context.target_assignments,
                            "inherited_variables": scope.inherited,
                        }
            variables = dict(scope.variables)
            for name, text in {"MAKE": "__make_dispatch__", "build": "__build_dispatch__", "Q": "", "@": path}.items():
                variables[name] = {"value": text}
            for terms in rule["recipes"]:
                text = evaluate(terms, variables, state["sources"]).lstrip("@+- ")
                if not text.startswith("__make_dispatch__"):
                    continue
                dispatch = _build_dispatch(shell_words(text))
                if dispatch == None or dispatch.goals != [path]:
                    fail("Unsupported recursive producer for %s: %s" % (path, text))
                return {"dispatch": dispatch.directory, "prerequisites": prerequisites, "variables": scope.variables}
    return None

def _recipe(path, directory, state):
    """Follow recursive producers while retaining one artifact identity."""
    prerequisites, visited = [], {}
    for _ in range(10000):
        if directory in visited:
            fail("Recursive Kbuild producer cycle for %s: %s" % (path, visited.keys()))
        visited[directory] = True
        native = _local_recipe(path, directory, state)
        if native == None:
            return None
        prerequisites.extend(native["prerequisites"])
        if "dispatch" not in native:
            native["prerequisites"] = {p: True for p in prerequisites}.keys()
            return native
        child = native["dispatch"]
        parent = state["invocations"].get(directory)
        visibility = (parent.visibility if parent != None else []) + native["prerequisites"]
        _enter_invocation(child, native["variables"], visibility, state)
        directory = child
    fail("Recursive Kbuild producer exceeded 10000 invocations: " + path)

def _architecture_invocations(roots, state):
    """Retain the completed parent phase visible to a selected boot invocation."""
    if not roots:
        return
    context = state["contexts"]["@architecture"]
    selected = {}
    for rule in context.rules:
        if not any([term.get("op") == "reference" and term.get("name") == [{"op": "literal", "value": "MAKE"}] for terms in rule.get("recipes", []) for term in terms]):
            continue
        for goal in rule["targets"]:
            if "%" in goal:
                continue
            variables = dict(_target_scope(goal, "@architecture", state).variables)
            variables["@"] = {"value": goal}
            preceding = []
            for terms in rule["recipes"]:
                text = evaluate(terms, variables, state["sources"]).lstrip("@+- ")
                words = shell_words(text, allow_recipe = True) if text.startswith("__make_dispatch__") else None
                if words != None and any([path in roots for path in words[2:]]):
                    dispatch = _build_dispatch(words)
                    if dispatch == None:
                        fail("Unsupported architecture boot dispatch %s: %s" % (goal, text))
                    if preceding:
                        fail("Unsupported commands before architecture boot dispatch %s: %s" % (goal, preceding))
                    prerequisites = [p for r in _rules_for_path(goal, context, state) for p in make_words(r["prerequisites"]) if p not in ("FORCE", "|")]
                    _enter_invocation(dispatch.directory, variables, prerequisites, state)
                    for path in dispatch.goals:
                        if path in selected and selected[path] != dispatch.directory:
                            fail("Multiple architecture invocations produce " + path)
                        selected[path] = dispatch.directory
                    break
                if text and text != ":":
                    preceding.append(text)

def _logical_setup_text(text, setup):
    return text.replace(setup.sources["__source_root__"], "__source__").replace("__object__/", "").replace("__object__", ".").replace("__config__", ".config")

def architecture_context(setup):
    """Dispatch retained native architecture rules with completed setup values."""
    variables = dict(setup.variables)
    for name, text in {"MAKE": "__make_dispatch__", "build": "__build_dispatch__", "Q": "", "KCONFIG_CONFIG": ".config", "srctree": "__source__", "srcroot": "__source__", "objtree": "."}.items():
        variables[name] = {"value": text}

    # Ordinary prerequisites were expanded at the include position. Keep
    # those values while projecting the setup roots into the planner namespace.
    rules = []
    for rule in setup.architecture_rules:
        projected = dict(rule, targets = [_logical_setup_text(target, setup) for target in rule["targets"]], prerequisites = _logical_setup_text(rule["prerequisites"], setup))
        if "secondary_inputs" in rule:
            projected["secondary_inputs"] = {name: {"value": _logical_setup_text(cell["value"], setup)} for name, cell in rule["secondary_inputs"].items()}
        rules.append(projected)
    return struct(variables = variables, rules = rules, target_assignments = setup.architecture_assignments)

def _postlink(path, state):
    post = state["contexts"].get("@postlink")
    if post == None:
        return struct(commands = [], outputs = [], prerequisites = [])
    native = _local_recipe(path, "@postlink", state)
    prerequisites = native["prerequisites"] if native != None else []
    variables = dict(_target_scope(path, "@postlink", state).variables)
    for name, text in {
        "@": path,
        "@F": path.rsplit("/", 1)[-1],
        "@D": path.rpartition("/")[0] or ".",
        "<": native["first"] if native != None else "",
        "^": " ".join(prerequisites),
        "*": native["stem"] if native != None else "",
        "real-prereqs": " ".join(prerequisites),
    }.items():
        variables[name] = {"value": text}
    commands, outputs = [], []
    for rule in _rules_for_path(path, post, state):
        for terms in rule.get("recipes", []):
            significant = [term for term in terms if term.get("op") != "literal" or term["value"].strip() not in ("", "@", "+")]
            if len(significant) == 1 and significant[0].get("op") == "literal" and significant[0]["value"].lstrip("@+").strip() == "true":
                continue
            if len(significant) != 1 or significant[0].get("name") != "call":
                fail("Unsupported native postlink recipe: " + str(terms))
            arguments = significant[0]["arguments"]
            function = evaluate(arguments[0], variables, state["sources"]).strip()
            if function not in ("cmd", "if_changed", "if_changed_dep") or len(arguments) < 2:
                fail("Unsupported native postlink command: " + str(terms))
            name = evaluate(arguments[1], variables, state["sources"]).strip()
            commands.append({"arguments": arguments[1:], "variables": variables, "own_automatic_variables": True})
            outputs.extend([_normalize(evaluate(expr, variables, state["sources"])) for expr in state["program"]["image_postlink_outputs"][state["postlink_file"]].get(name, [])])
    return struct(commands = commands, outputs = outputs, prerequisites = prerequisites)

def _link_descriptor(path, state):
    if state["program"]["image_script_link"]:
        final_link = path == state["final_link"]
        directory = "@vmlinux" if final_link else "@modpost"
        context = state["contexts"][directory]
        symdump = _value("output-symdump", state["contexts"]["@modpost"].variables, state["sources"]).strip()
        module_sources = [p[:-3] + ".mod.c" for p in state["modules"]]
        if path in module_sources:
            path = symdump
        if not final_link and path not in (symdump, "Module.symvers"):
            return None
        native = _recipe(path, directory, state)
        if native == None:
            fail("No native link-stage producer for " + path)
        dependencies = [p for p in native["prerequisites"] if p not in state["empty_archive_controls"]]
        outputs = [path]
        if final_link:
            outputs.extend(["System.map"] + state["link_cooutputs"])
            post = state["postlinks"][path]
            outputs.extend(post.outputs)
            dependencies.extend(post.prerequisites)
            dependencies.append("scripts/mod/modpost")
            scripts = directory_context("scripts", state)
            dependencies.extend(["scripts/" + name for name in make_words(_value("hostprogs-always-y", scripts.variables, state["sources"]))])
            native["script_modpost"] = _recipe("vmlinux.symvers", "@vmlinux_modpost", state)
            native["postlink"] = post.commands
            native["pahole"] = state["pahole"]
            if state["pahole"] != None:
                dependencies.extend(make_words(_value("RESOLVE_BTFIDS", context.variables, state["sources"])))
        elif path == symdump:
            outputs.extend(module_sources)
            dependencies.append("scripts/mod/modpost")
        native["final_link"] = final_link
        return {"target": path, "directory": ".", "outs": outputs, "deps": dependencies, "kind": "generate", "driver": "link", "native": native}
    if path in ("built-in-fixup.a", "vmlinux.a"):
        native = _recipe(path, "@archive", state)
        if native == None:
            fail("No native kernel archive producer for " + path)
        dependencies = [p for p in native["prerequisites"] if p not in state["empty_archive_controls"]]
        return {"target": path, "directory": ".", "outs": [path], "deps": dependencies, "kind": "generate", "driver": "link", "native": native}
    if path in state.get("link_object_targets", {}):
        native = _recipe(path, "@vmlinux_o", state)
        if native == None:
            fail("No native intermediate-image producer for " + path)
        command = evaluate(native["arguments"][0], native["variables"], state["sources"]).strip()
        native["postprocess"] = state["program"]["image_link_postprocess"].get(command, [])
        return {"target": path, "directory": ".", "outs": [path], "deps": native["prerequisites"], "kind": "generate", "driver": "link", "native": native}
    variables = dict(directory_context(".", state).variables)
    for name, val in {"objtree": ".", "srctree": "__source__", "srcroot": "__source__"}.items():
        variables[name] = {"value": val}
    symdump = "Module.symvers" if state["modules"] else "vmlinux.symvers"
    modpost_outputs = [symdump, ".vmlinux.export.c"] + [p[:-3] + ".mod.c" for p in state["modules"]]
    modpost_deps = ["vmlinux.o", "scripts/mod/modpost"]
    if state["modules"]:
        modpost_deps += ["modules.order"] + [p[:-3] + suffix for p in state["modules"] for suffix in [".o", ".mod"]]
    stages = {
        symdump: ("modpost", "modpost", modpost_deps, modpost_outputs),
        ".vmlinux.export.c": ("modpost", "modpost", modpost_deps, modpost_outputs),
    }
    final_link = path == state["final_link"]
    if path in ["vmlinux", state["final_link"]]:
        recipe = _recipe(path, "@vmlinux", state)
        if recipe == None:
            fail("No native final-image producer for " + path)
        command = evaluate(recipe["arguments"][0], recipe["variables"], state["sources"]).strip()
        deps = list(recipe["prerequisites"])
        if final_link:
            deps.extend([symdump, "init/version-timestamp.o"])
            if state["pahole"] != None:
                deps.extend(make_words(_value("RESOLVE_BTFIDS", variables, state["sources"])))
        stages[path] = ("vmlinux", command, deps, [path, "System.map"] if final_link else [path])
    for module in state["modules"]:
        stages[module[:-3] + ".mod.c"] = ("modpost", "modpost", modpost_deps, modpost_outputs)
    if path not in stages:
        return None
    driver, command, deps, outs = stages[path]
    if final_link:
        post = state["postlinks"][path]
        outs = outs + post.outputs
        deps.extend(post.prerequisites)
        scripts = directory_context("scripts", state)
        deps.extend(["scripts/" + name for name in make_words(_value("hostprogs-always-y", scripts.variables, state["sources"]))])
    variables["KBUILD_MODULES"] = {"value": "y" if state["modules"] else ""}
    variables = program(state["program"]["image_link"][driver], variables, state["sources"], cells = True).variables
    if final_link and state["pahole"] != None:
        # Bazel schedules the action; keep pahole's internal worker count
        # explicit instead of passing the native Makefile's empty -j argument.
        variables["MAKEFLAGS"] = {"value": "-j1"}
        variables = program(state["program"]["image_common"] + state["program"]["image_btf"], variables, state["sources"], cells = True).variables
    if driver == "modpost":
        if _value("missing-input", variables, state["sources"]):
            fail("Native modpost has a missing declared input: " + _value("missing-input", variables, state["sources"]))
        if _value("output-symdump", variables, state["sources"]).strip() != symdump:
            fail("Unexpected native modpost output: " + _value("output-symdump", variables, state["sources"]))
        deps.extend(make_words(_value("modpost-deps", variables, state["sources"])))
    native = {
        "function": "cmd",
        "arguments": [[{"op": "literal", "value": command}]],
        "variables": variables,
        "prerequisites": deps,
        "stem": "",
        "assignments": [],
        "final_link": final_link,
        "postlink": state["postlinks"][path].commands if final_link else [],
        "pahole": state["pahole"] if final_link else None,
    }
    return {"target": outs[0], "directory": ".", "outs": outs, "deps": deps, "kind": "generate", "driver": "link", "native": native}

def _prepare_contexts(state, arch_context):
    """Resolve architecture preparation producers from the same native programs."""
    root = directory_context(".", state)
    state["bootstrap"] = [_normalize(p) for p in make_words(_value("targets", root.variables, state["sources"]))]
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
            header = program(state["program"]["image_asm_headers"], variables, state["sources"], cells = True)
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
                    child = program(state["program"]["image_programs"]["arch/" + arch + "/Makefile"], variables, state["sources"], cells = True)
                    child_directory = "@architecture:" + arch
                    state["contexts"][child_directory] = child
                    for child_goal in words[3:]:
                        if "=" not in child_goal and child_goal != "asm-generic":
                            work.append((child, child_directory, child_goal, late))
                    continue
                dispatch = _build_dispatch(words)
                if dispatch == None:
                    fail("Unsupported native architecture preparation dispatch: " + text)
                child = dispatch.directory
                child_context = directory_context(child, state)
                if late:
                    state["late_directories"][child] = True
                for child_goal in dispatch.goals:
                    hostnames = make_words(_value("hostprogs", child_context.variables, state["sources"]) + " " + _value("hostprogs-always-y", child_context.variables, state["sources"]))
                    if child_goal in hostnames:
                        state["preparation"].append(child + "/" + child_goal)
                    else:
                        work.append((child_context, child, child_goal, late))
    if work:
        fail("Architecture preparation graph exceeded 10000 goals")
    state["preparation"] = {p: True for p in state["preparation"]}.keys()

def _module_order_variables(state):
    variables = dict(state["setup"].variables)
    variables["MAKECMDGOALS"] = {"value": " ".join(state["modules"])}
    variables["single-ko"] = {"value": " ".join(state["modules"])}
    variables["Q"] = {"value": ""}
    return variables

def _describe(path, state, owner = None):
    path = state["postlink_outputs"].get(path, path)
    if path in state["link_cooutputs"]:
        path = state["final_link"]
    if path in state["preparation_checks"]:
        return state["preparation_checks"][path]
    if path == "System.map":
        return _link_descriptor(state["final_link"], state)
    link = _link_descriptor(path, state) if path in ["built-in-fixup.a", "vmlinux.a", "vmlinux.o", "vmlinux.symvers", "Module.symvers", "modules-only.symvers", ".vmlinux.export.c", "vmlinux.unstripped", "vmlinux"] or path in state.get("link_object_targets", {}) or path in [m[:-3] + ".mod.c" for m in state["modules"]] else None
    if link:
        return link
    if state["modules"] and path == "modules.order":
        variables = _module_order_variables(state)
        return {
            "target": path,
            "directory": ".",
            "outs": [path],
            "deps": [p for terms in state["program"]["image_module_order_prerequisites"] for p in make_words(evaluate(terms, variables, state["sources"]))],
            "kind": "generate",
            "driver": "link",
            "native": {"function": "script", "scripts": state["program"]["image_modules_order"], "arguments": [], "variables": variables, "prerequisites": [], "stem": "", "assignments": []},
        }
    if path in state["module_final"]:
        native = _recipe(path, "@modfinal", state)
        if native == None:
            fail("No native module finalization producer for " + path)
        compiling = path.endswith(".o")
        native["phase"] = "modfinal"
        dependencies = list(native["prerequisites"])
        outputs = [path]
        if compiling:
            dependencies.extend(state["preparation"])
            dependencies.extend(state["version_headers"])
        else:
            post = state["postlinks"][path]
            native["postlink_lowered"] = True
            native["postlink"] = list(post.commands)
            outputs.extend(post.outputs)
            dependencies.extend(post.prerequisites)
            if state["config"].get("CONFIG_DEBUG_INFO_BTF_MODULES") == "y":
                native["pahole"] = state["pahole"]
                native["postlink"].append({
                    "arguments": [[{"op": "literal", "value": "btf_ko"}]],
                    "variables": native["variables"],
                })
                dependencies.append("tools/bpf/resolve_btfids/resolve_btfids")
        return {"target": path, "directory": ".", "outs": outputs, "deps": dependencies, "kind": "compile" if compiling else "generate", "driver": "link", "native": native}
    if path == "include/generated/utsversion.h":
        return {"target": path, "directory": "init", "outs": [path], "deps": [], "kind": "generate", "driver": "build", "native": _recipe(path, "init", state)}
    header_commands = {"include/generated/uapi/linux/" + name if name == "version.h" else "include/generated/" + name: name for name in state["program"]["image_header_commands"]}
    if path == "include/generated/compile.h" and "compile.h" not in state["program"]["image_header_commands"]:
        native = _recipe(path, "init", state)
        if native == None:
            context = directory_context("init", state)
            rules = _rules_for_path(path, context, state)
            if len(rules) != 1 or not rules[0].get("recipes"):
                fail("Expected one native compile.h producer, got " + str(rules))
            native = {"function": "script", "scripts": rules[0]["recipes"], "arguments": [], "variables": context.variables, "prerequisites": [], "stem": "", "assignments": []}
        return {"target": path, "directory": "init", "outs": [path], "deps": native["prerequisites"], "kind": "generate", "driver": "build", "native": native}
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
        variables = directory_context(directory, state).variables
        members = make_words(_value("lib-y" if path.endswith("/lib.a") else "real-obj-y", variables, state["sources"]))

        # Make automatic $^ (and therefore real-prereqs) keeps the first
        # occurrence of a prerequisite, even when multiple options add it.
        members = {_normalize(p): True for p in members}.keys()
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
    directory = path.rpartition("/")[0]
    if owner != None and any([r.get("recipes") and path in [_normalize(p) for p in r["targets"] if "%" not in p] for r in directory_context(owner, state).rules]):
        # A Makefile can own outputs outside its directory (for example a
        # sibling generated header). Only an explicit producer establishes
        # that ownership; generic compiler patterns belong to each directory.
        directory, static = owner, None
    module_owner = state["module_owners"].get(path)
    if module_owner != None:
        directory = module_owner

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
        if static["kind"] == "generate" and static.get("driver") != "asm_headers" and static["directory"] in state["catalog"].programs:
            result["native"] = _recipe(path, static["directory"], state)
            if result["native"] != None:
                result["directory"] = result["native"]["directory"]
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
    context = directory_context(directory, state)
    variables = context.variables
    hosts = make_words(_value("hostprogs", variables, state["sources"]) + " " +
                       _value("hostprogs-always-y", variables, state["sources"]) + " " +
                       _value("hostprogs-always-m", variables, state["sources"]))
    host_members = {}
    for host in hosts:
        objects = make_words(_value(host + "-objs", variables, state["sources"]) + " " +
                             _value(host + "-cxxobjs", variables, state["sources"]))
        host_members[host] = [_normalize(directory + "/" + obj) for obj in objects]
    host_name = path.removeprefix(directory + "/") if directory != "." else path
    kind = "host" if host_name in hosts or path in [p for members in host_members.values() for p in members] else "generate"
    dependencies = host_members.get(host_name, [])
    outs = [path]
    has_recipe = host_name in hosts
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
            tail = rule_prerequisites(rule, path, stem, _target_scope(path, directory, state).variables, state["sources"]).split(";", 1)[0]
            if ":" in tail:
                target_pattern, tail = tail.split(":", 1)
                stem = match(_normalize(target_pattern.strip()), path)
                if stem == None:
                    continue
            dependencies.extend([_normalize(d.replace("%", stem, 1)) for d in make_words(tail) if d not in ("FORCE", "|")])
    native = _recipe(path, directory, state)
    if native != None:
        if native["directory"] != directory:
            directory = native["directory"]
            dependencies.extend(native["prerequisites"])
        command = evaluate(native["arguments"][0], native["variables"], state["sources"]).strip()
        if kind != "host" and command in state["program"]["image_compile_commands"]:
            native["compile"] = True
        if kind != "host" and command in ("cc_o_c", "as_o_S"):
            kind = "compile"
    if not has_recipe:
        fail("No native action producer for " + path)
    compiler_prerequisites = list(dependencies)
    postprocess_dependencies = []
    recordmcount = state["program"]["image_recordmcount"]
    if kind == "compile" and native != None and native["function"] == "if_changed_rule" and recordmcount != None:
        command = evaluate(native["arguments"][0], native["variables"], state["sources"]).strip()
        if "record_mcount" in state["program"]["image_compile_postprocess"][command] and path != recordmcount["excluded"]:
            recording = program(recordmcount["program"], native["variables"], state["sources"], cells = True)
            if recording.variables.get("sub_cmd_record_mcount", {}).get("expr") == recordmcount["expression"]:
                postprocess_dependencies.append(recordmcount["tool"])
    dependencies.extend(postprocess_dependencies)
    if kind == "compile" or (native != None and native.get("compile")) or path.endswith(".lds"):
        # Root Kbuild already declares the ordering among bootstrap assembly
        # generators. Other objects consume the configured preparation outputs.
        if path not in state["bootstrap"]:
            dependencies.extend([p for p in state["preparation"] if directory not in state["late_directories"] or p not in state["late_preparation"]])
        else:
            dependencies.extend([p for p in state["preparation"] if "/include/generated/" in p and p not in state["late_preparation"]])
            if path not in state["preparation_assembly"]:
                dependencies.extend(state["early_preparation"])
        dependencies.extend(state["version_headers"])
    return {"target": path, "directory": "." if directory == "@vmlinux" else directory, "outs": outs, "deps": dependencies, "compiler_prerequisites": compiler_prerequisites, "postprocess_dependencies": postprocess_dependencies, "kind": kind, "driver": "build", "native": native if kind in ("generate", "compile") else None}

def _select_modules(state):
    """Follow the configured recursive Kbuild graph to each requested owner."""
    for module in state["modules"]:
        object_path = module[:-3] + ".o"
        work = ["."]
        seen = {}
        selected = False
        for _ in range(10000):
            if not work:
                fail("Requested module is not selected by Kbuild: " + module)
            directory = work.pop()
            if directory in seen:
                continue
            seen[directory] = True
            variables = directory_context(directory, state).variables
            if object_path in make_words(_value("obj-m", variables, state["sources"])):
                state["module_owners"][object_path] = directory
                state["module_owners"][module[:-3] + ".mod"] = directory
                selected = True
                break
            work.extend([child for child in state["children"][directory] if object_path.startswith(child + "/")])
        if not selected:
            fail("Module selection exceeded 10000 directories: " + module)

# Callback action planner, not a BUILD macro.
# buildifier: disable=unnamed-macro
def build_image(ctx, config, setup, prepared, catalog, objtool, resolve_btfids = None):
    modules = sorted({path: True for path in ctx.attr.roots if path.endswith(".ko")})
    for path in modules:
        if _normalize(path) != path or path.startswith("/"):
            fail("Expected a source-relative Kbuild module path: " + path)
    if modules:
        if config.values.get("MODULES") != "y":
            fail("Requested modules require CONFIG_MODULES=y")
        for option in ["MODVERSIONS", "MODULE_SRCVERSION_ALL"]:
            if config.values.get(option) == "y":
                fail("Requested modules with CONFIG_%s need native .cmd metadata, which is not yet exported" % option)
    for name, expected in ctx.attr.required_options.items():
        if config.values.get(name) != expected:
            fail("Required kernel option %s=%s resolved to %s" % (name, expected, config.values.get(name)))
    if setup.identity != config.identity:
        fail("Mixed native setup and kernel configuration")
    files = prepared.files
    source_files = prepared.logical
    index = source_index(["__source__/" + p for p in source_files] + ["vmlinux.o", "vmlinux.symvers"])
    index["__includes__"] = setup.program["image_programs"]
    index["__capabilities__"] = setup.sources["__capabilities__"]
    index["__requests__"] = setup.sources["__requests__"]
    index["__probe_environment__"] = setup.sources["__probe_environment__"]
    index["__source_root__"] = setup.sources["__source_root__"]
    index["__host_machine__"] = setup.sources["__host_machine__"]
    index["__compiler_include__"] = setup.sources.get("__compiler_include__", "")
    index["__execution_tools__"] = setup.sources["__execution_tools__"]
    state = {
        "program": setup.program,
        "catalog": catalog,
        "contexts": {},
        "invocations": {},
        "target_scopes": {},
        "parents": {},
        "children": {},
        "directory_modes": {},
        "modules": modules,
        "module_owners": {},
        "module_final": {p: True for module in modules for p in [module, module[:-3] + ".mod.o"]},
        "dtbs_lists": {},
        "arch": config.architecture.srcarch,
        "config": config.make_values,
        "source_files": source_files,
        "sources": index,
        "setup": setup,
        "pahole": config.pahole if config.values.get("DEBUG_INFO_BTF") == "y" else None,
        "version_headers": ["include/generated/uapi/linux/version.h", "include/generated/utsrelease.h", "include/generated/compile.h"] + (["include/generated/utsversion.h"] if setup.program["image_timestamp_header"] else []),
    }
    external_tools = {}
    if config.values.get("DEBUG_INFO_BTF") == "y":
        if config.pahole == None:
            fail("CONFIG_DEBUG_INFO_BTF requires the declared pahole used to configure the kernel")
        if resolve_btfids == None:
            fail("CONFIG_DEBUG_INFO_BTF requires the declared native resolve_btfids tool")
        external_tools["tools/bpf/resolve_btfids/resolve_btfids"] = resolve_btfids

    # Recursive Make expands only exported parent variables into a child's
    # environment. Unexported names such as core-y must not affect core.o.
    state["environment"] = {name: {"expr": [{"op": "reference", "name": [{"op": "literal", "value": name}]}], "scope": setup.variables, "raw": ""} for name in setup.variables.get("__exports__", {}) if name in setup.variables}
    state["environment"]["__exports__"] = dict(setup.variables.get("__exports__", {}))
    state["environment"]["__command_line__"] = setup.variables["__command_line__"]
    state["environment"]["__cell_sequence__"] = setup.variables["__cell_sequence__"]
    arch_context = architecture_context(setup)
    state["known_products"] = {}
    _prepare_contexts(state, arch_context)
    _select_modules(state)
    if modules:
        state["module_final"][".module-common.o"] = True
        variables = dict(setup.variables)
        for name, text in {"objtree": ".", "srctree": "__source__", "srcroot": "__source__", "KBUILD_BUILTIN": "1", "MAKEFLAGS": "-j1"}.items():
            variables[name] = {"value": text}
        if setup.program["image_module_order_contents"] != None:
            variables["__selected_module_order"] = {"value": evaluate(setup.program["image_module_order_contents"], _module_order_variables(state), state["sources"])}
        state["contexts"]["@modfinal"] = program(setup.program["image_common"] + setup.program["image_btf"] + setup.program["image_modfinal"], variables, state["sources"], cells = True)
    state["contexts"]["@archive"] = program(setup.program["image_archive"], directory_context(".", state).variables, state["sources"], cells = True)
    controls = program(setup.program["image_archive_controls"], dict(state["contexts"]["@archive"].variables, PHONY = {"value": ""}), state["sources"], cells = True)
    phony = {p: True for rule in controls.rules if ".PHONY" in rule["targets"] for p in make_words(rule["prerequisites"])}
    state["empty_archive_controls"] = {}
    for path in phony:
        # A phony target with no selected rule is also empty. Inspect the
        # records directly: an unsupported active recipe must never disappear.
        rules = [rule for rule in controls.rules if path in rule["targets"]]
        if not any([rule.get("recipes") or make_words(rule["prerequisites"]) for rule in rules]):
            state["empty_archive_controls"][path] = True
    link_context = program(setup.program["image_link_context"], directory_context(".", state).variables, state["sources"], cells = True)
    state["contexts"]["@vmlinux"] = link_context
    state["link_owners"] = {p: True for p in make_words(_value("targets", link_context.variables, state["sources"]))}
    final_targets = {}
    for rule in link_context.rules:
        for terms in rule.get("recipes", []):
            for term in terms:
                if term.get("op") == "function" and term.get("name") == "call" and len(term["arguments"]) > 1 and evaluate(term["arguments"][1], link_context.variables, state["sources"]).strip() == setup.program["image_final_command"]:
                    final_targets.update({p: True for p in rule["targets"]})
    if len(final_targets) != 1:
        fail("Expected one native link_vmlinux target, got " + str(final_targets.keys()))
    state["final_link"] = final_targets.keys()[0]
    state["link_cooutputs"] = []
    module_roots = list(modules)
    if setup.program["image_script_link"]:
        state["link_cooutputs"] = ["vmlinux.o", "vmlinux.symvers", "modules.builtin", "modules.builtin.modinfo"]
        variables = dict(directory_context(".", state).variables)
        if setup.program["image_module_order_contents"] != None:
            variables["__selected_module_order"] = {"value": evaluate(setup.program["image_module_order_contents"], _module_order_variables(state), state["sources"])}
        state["contexts"]["@modpost"] = program(setup.program["image_link"]["modpost"], variables, state["sources"], cells = True)
        if modules:
            module_roots.extend([p for rule in state["contexts"]["@modpost"].rules if "__modpost" in rule["targets"] for p in make_words(rule["prerequisites"])])
        variables["MODPOST_VMLINUX"] = {"value": "1"}
        state["contexts"]["@vmlinux_modpost"] = program(setup.program["image_link"]["modpost"], variables, state["sources"], cells = True)
    state["link_object_targets"] = {}
    if not setup.program["image_script_link"]:
        object_variables = dict(directory_context(".", state).variables)
        object_variables.update({"objtree": {"value": "."}, "srctree": {"value": "__source__"}, "srcroot": {"value": "__source__"}})
        object_context = program(setup.program["image_link"]["vmlinux_o"], object_variables, state["sources"], cells = True)
        state["contexts"]["@vmlinux_o"] = object_context
        state["link_object_targets"] = {p: True for rule in object_context.rules if rule.get("recipes") for p in rule["targets"] if "%" not in p}
    postlink = "arch/" + config.architecture.srcarch + "/Makefile.postlink"
    state["postlink_file"] = postlink
    if postlink in setup.program["image_programs"]:
        state["contexts"]["@postlink"] = program(setup.program["image_programs"][postlink], directory_context(".", state).variables, state["sources"], cells = True)
    state["postlinks"] = {path: _postlink(path, state) for path in [state["final_link"]] + modules}
    state["postlink_outputs"] = {}
    for path, post in state["postlinks"].items():
        for output in post.outputs:
            if output == path or output in state["postlink_outputs"]:
                fail("Multiple native postlink producers for " + output)
            state["postlink_outputs"][output] = path
    state["known_products"] = {p: True for p in ["built-in-fixup.a", "vmlinux.a", "vmlinux.o", "vmlinux.symvers", ".vmlinux.export.c", "vmlinux.unstripped", "vmlinux", "System.map", ".config", "include/config/auto.conf", "include/generated/autoconf.h"] + state["postlink_outputs"].keys() + state["module_final"].keys() + [m[:-3] + suffix for m in modules for suffix in [".o", ".mod", ".mod.c"]]}
    _architecture_invocations(ctx.attr.roots, state)
    pending = [(path, None, {}) for path in reversed(list(ctx.attr.roots) + module_roots)]
    descriptors, producers = {}, {}
    for _ in range(100000):
        if not pending:
            break
        path, owner, inherited = pending.pop()
        path = _normalize(path)
        if path in (".config", "include/config/auto.conf", "include/generated/autoconf.h") or path.startswith("__source__/") or path == "tools/objtool/objtool" or path in external_tools or path in source_files or path in producers:
            continue
        state["target_scopes"][path] = (owner, inherited)
        descriptor = _describe(path, state, owner)
        descriptor["deps"] = [_normalize(p) for p in descriptor["deps"]]
        invocation = state["invocations"].get(descriptor["directory"])
        descriptor["visibility"] = [_normalize(p) for p in invocation.visibility] if invocation != None else []
        descriptors[descriptor["target"]] = descriptor
        for out in descriptor["outs"]:
            if out in producers and producers[out] != descriptor["target"]:
                fail("Multiple configured producers for " + out)
            producers[out] = descriptor["target"]
        descendants = _target_scope(path, descriptor["directory"], state).descendants
        pending.extend([(dependency, None, {}) for dependency in reversed(descriptor["visibility"])])
        pending.extend([(dependency, descriptor["directory"], descendants) for dependency in reversed(descriptor["deps"])])
    if pending:
        fail("Kbuild graph exceeded 100000 dependency expansions")

    # Declare every output before registering jobs. This permits a direct,
    # configured topological check and avoids label cycles between alternatives.
    artifacts = {out: ctx.actions.declare_file(ctx.label.name + ".artifacts/" + out) for out in producers}
    for root in ctx.attr.roots:
        if root in external_tools:
            artifact = ctx.actions.declare_file(ctx.label.name + ".artifacts/" + root)
            ctx.actions.run(executable = ctx.execution.commands["cp"], arguments = [external_tools[root].path, artifact.path], inputs = [external_tools[root]], outputs = [artifact], mnemonic = "KbuildToolExport")
            artifacts[root] = artifact
    common = {"include/generated/autoconf.h": config.header}
    if config.values.get("OBJTOOL") == "y" or any(["tools/objtool/objtool" in d["deps"] for d in descriptors.values()]):
        if objtool.executable == None:
            fail("CONFIG_OBJTOOL requires a native objtool decoder for SRCARCH=" + config.architecture.srcarch)
        common["tools/objtool/objtool"] = objtool.executable
    finished = {}
    members = []
    compilers = {}
    compiler_inventory = {}
    diagnostics, diagnostic_cases = [], {}
    for _ in range(len(descriptors) + 1):
        progress = False
        for target, d in descriptors.items():
            if target in finished:
                continue
            deps = {producers[p]: True for p in d["deps"] + d["visibility"] if p in producers and producers[p] != target}
            if any([p not in finished for p in deps]):
                continue
            closure = dict(common)
            for dep in deps:
                closure.update(finished[dep])
            closure.update({p: external_tools[p] for p in d["deps"] if p in external_tools})
            for dependency in d["deps"]:
                logical = dependency.removeprefix("__source__/")
                if logical in source_files and type(source_files[logical]) == "File":
                    closure[dependency] = source_files[logical]
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
            graph_variables = {}
            if not d["directory"].startswith("@") and (d["kind"] in ["host", "compile"] or (d["kind"] == "generate" and d.get("native") == None and d["driver"] != "asm_headers")):
                graph_variables = directory_context(d["directory"], state).graph_variables
            object_members = d.get("members", [p for p in d["deps"] if p.endswith(".o")] if d["kind"] == "host" and not target.endswith(".o") else [])
            compiler_descriptor = d["kind"] == "compile" or (d.get("native") != None and d["native"].get("compile")) or (d["kind"] == "host" and not object_members)
            native_prerequisites = d.get("compiler_prerequisites", d["deps"])
            if compiler_descriptor:
                native_source = d["native"]["first"].removeprefix("__source__/") if d.get("native") != None and d["native"].get("first") else target[:-2] + ".c" if target.endswith((".o", ".s")) else target + ".c"
                if native_source not in prepared.logical and native_source not in closure and target.endswith(".o"):
                    native_source = target[:-2] + ".S"
                native_kind = "host" if d["kind"] == "host" else "assembly" if native_source.endswith(".S") else "c" if native_source.endswith(".c") and target.endswith(".o") else "codegen"
                native_source_file = closure.get(native_source, closure.get("__source__/" + native_source, prepared.logical.get(native_source)))
                if native_source_file == None:
                    fail("Native compiler descriptor has no declared source File: " + target + ": " + native_source)
                compiler_inventory[target] = {"kind": native_kind, "directory": d["directory"], "source": native_source, "source_file": native_source_file.path}
                native_prerequisites = compiler_prerequisites(d, native_kind, descriptors[producers[native_source]] if native_source in producers else None)
            compiled = register(
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
                object_members,
                native = d.get("native"),
                ancestry = ancestry,
                graph_variables = graph_variables,
                compiler_kind = compiler_inventory[target]["kind"] if compiler_descriptor else None,
                original_sources = prepared.logical,
                compiler_prerequisites = {p: closure[p] for p in native_prerequisites if p in closure},
                postprocess_dependencies = {p: closure[p] for p in d.get("postprocess_dependencies", [])},
                invocation_environment = state["invocations"][d["directory"]].environment if d["directory"] in state["invocations"] else None,
            )
            if compiled != None:
                if target not in compiler_inventory:
                    fail("Compiler action has no native descriptor: " + target)
                diagnostics.extend(compiled.pop("diagnostics", []))
                diagnostic_cases.update(compiled.pop("diagnostic_cases", {}))
                compilers[target] = compiled
            elif compiler_descriptor:
                fail("Native compiler descriptor did not register a compiler: " + target)
            members.append({"target": target, "dependencies": sorted(deps), "outputs": {p: f.path for p, f in outputs.items()}})
            closure.update(outputs)
            finished[target] = closure
            progress = True
        if len(finished) == len(descriptors):
            break
        if not progress:
            blocked = {
                t: [producers[p] for p in d["deps"] + d["visibility"] if p in producers and producers[p] != t and producers[p] not in finished]
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
    recipe_checks = native_recipe_check(ctx, setup, prepared.logical) if ctx.attr.validate_native_objects else {}
    manifest = ctx.actions.declare_file(ctx.label.name + ".json")
    ctx.actions.write(manifest, json.encode({
        "identity": config.identity,
        "compiler": "clang" if config.values.get("CC_IS_CLANG") == "y" else "other",
        "compilers": compilers,
        "compiler_inventory": compiler_inventory,
        "compiler_diagnostics": diagnostics,
        "compiler_diagnostic_cases": diagnostic_cases,
        "native_recipe_checks": {role: file.path for role, file in recipe_checks.items()},
        "config": config.config.path,
        "config_owner": config.config_owner.path,
        "setup": setup.file.path,
        "members": members,
        "roots": list(ctx.attr.roots),
        "artifacts": {p: f.path for p, f in artifacts.items()},
    }))
