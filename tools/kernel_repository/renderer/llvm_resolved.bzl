"""Resolve configured prerequisite programs and register one action per artifact."""

load(":commands.bzl", "register")
load(":expressions.bzl", "evaluate", "make_words", "match", "program", "source_index")
load(":kbuild.bzl", "KbuildFileInfo")
load(":rules.bzl", "KernelConfigInfo")
load(":toolchain.bzl", "KernelToolchainInfo")

KbuildCatalogInfo = provider(doc = "Kbuild Catalog Info.", fields = ["actions", "programs", "aliases"])

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
    if directory.startswith("arch/") and directory.split("/")[1] != state["arch"]:
        fail("Kbuild architecture mismatch: " + directory)
    if directory not in state["contexts"]:
        statements = state["catalog"].programs.get(directory)
        if statements == None:
            fail("No exported Make program for " + directory)
        initial = {
            "obj": directory,
            "objtree": ".",
            "src": "__source__/" + directory,
            "srctree": "__source__",
            "srcroot": "__source__",
            "SRCARCH": state["arch"],
            "ARCH": state["architecture"].arch,
            "KCONFIG_CONFIG": ".config",
            "__config__": state["config"],
        }
        state["contexts"][directory] = program(statements, initial, state["sources"])
    return state["contexts"][directory]

def _describe(path, state):
    static = state["catalog"].actions.get(state["catalog"].aliases.get(path, path))
    directory, _, name = path.rpartition("/")

    # Existing explicit base/preparation edges remain available as anchors.
    if static != None:
        result = dict(static)
        result["deps"] = list(static["deps"])
        if path.endswith(".o"):
            generated = path[:-2] + ".c"
            if generated in state["catalog"].aliases:
                result["deps"].append(generated)
        return result
    if directory not in state["catalog"].programs:
        parts = directory.split("/")
        for i in reversed(range(len(parts))):
            parent = "/".join(parts[:i]) if i else "."
            if parent in state["catalog"].programs:
                directory = parent
                break
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
    source = path[:-2] + ".c" if path.endswith(".o") else path + ".c"
    if path.endswith(".o"):
        # Native suffix patterns consume generated lexers/parsers too.
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
            kind = "compile"
    for rule in context.rules:
        for pattern in rule["targets"]:
            stem = match(_normalize(pattern), path)
            if stem == None:
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
    if not has_recipe:
        fail("No native action producer for " + path)
    if kind == "host" and path != "scripts/basic/fixdep":
        dependencies.append("scripts/basic/fixdep")
    if kind == "compile":
        # Same explicit preparation graph as the anchored object actions.
        anchor = state["catalog"].actions["lib/oid_registry.o"]
        dependencies.extend([d for d in anchor["deps"] if d != "lib/oid_registry_data.c"])
    return {"target": path, "directory": directory, "outs": outs, "deps": dependencies, "kind": kind, "driver": "build"}

def _impl(ctx):
    config = ctx.attr.config[KernelConfigInfo]
    setup = ctx.attr._setup[KernelToolchainInfo]
    if setup.identity != config.identity:
        fail("Mixed native setup and kernel configuration")
    files = ctx.attr._source_tree[DefaultInfo].files
    prefix = ctx.file._anchor.dirname + "/"
    source_files = {f.path[len(prefix):]: f for f in files.to_list() if f.path.startswith(prefix)}
    index = source_index(["__source__/" + p for p in source_files])
    state = {
        "catalog": ctx.attr.catalog[KbuildCatalogInfo],
        "contexts": {},
        "arch": config.architecture.srcarch,
        "architecture": config.architecture,
        "config": config.make_values,
        "source_files": source_files,
        "sources": index,
    }
    pending = list(ctx.attr.roots)
    descriptors, producers = {}, {}
    for _ in range(100000):
        if not pending:
            break
        path = _normalize(pending.pop())
        if path in (".config", "include/config/auto.conf", "include/generated/autoconf.h") or path.startswith("__source__/") or path in source_files or path in producers:
            continue
        descriptor = _describe(path, state)
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
            register(
                ctx,
                target,
                d["directory"],
                d["kind"],
                d["driver"],
                outputs,
                closure,
                files,
                ctx.file._anchor.dirname,
                config,
                setup,
                [p for p in d["deps"] if p.endswith(".o")] if d["kind"] == "host" and not target.endswith(".o") else [],
            )
            members.append({"target": target, "dependencies": sorted(deps), "outputs": {p: f.path for p, f in outputs.items()}})
            closure.update(outputs)
            finished[target] = closure
            progress = True
        if len(finished) == len(descriptors):
            break
        if not progress:
            fail("Configured Kbuild dependency cycle: %s" % [p for p in descriptors if p not in finished])
    manifest = ctx.actions.declare_file(ctx.label.name + ".json")
    ctx.actions.write(manifest, json.encode({
        "identity": config.identity,
        "config": config.config.path,
        "config_report": config.report.path,
        "setup": setup.file.path,
        "members": members,
        "artifacts": {p: f.path for p, f in artifacts.items()},
    }))
    return [DefaultInfo(files = depset([manifest, config.report, setup.file] + artifacts.values()))]

_ACTIVE_CONFIG = str(Label("//:active_config"))

# Bazel callback signatures include inputs this implementation does not use.
# buildifier: disable=unused-variable
def _select(settings, attr):
    return {_ACTIVE_CONFIG: str(attr.config)}

_transition = transition(implementation = _select, inputs = [], outputs = [_ACTIVE_CONFIG])

kbuild_resolved_build = rule(implementation = _impl, cfg = _transition, attrs = {
    "config": attr.label(mandatory = True, providers = [KernelConfigInfo]),
    "catalog": attr.label(mandatory = True, providers = [KbuildCatalogInfo]),
    "roots": attr.string_list(mandatory = True),
    "_setup": attr.label(default = "//:toolchain", providers = [KernelToolchainInfo]),
    "_materialize": attr.label(default = "//rules:materialize.py", allow_single_file = True, cfg = "exec"),
    "_source_tree": attr.label(default = "//:source_tree"),
    "_anchor": attr.label(default = "//:source/Makefile", allow_single_file = True),
    "_allowlist_function_transition": attr.label(default = "@bazel_tools//tools/allowlists/function_transition_allowlist"),
})
