"""Configured execution of content-addressed Kbuild flag programs."""

load("@rules_cc//cc:find_cc_toolchain.bzl", "CC_TOOLCHAIN_TYPE", "use_cc_toolchain")
load(":graph_profile.bzl", "LinuxGraphProfileInfo")
load(
    ":path_mapping.bzl",
    "add_directory_arg",
    "directory_anchor",
    "path_mapped_run",
)

visibility("//...")

LinuxFlagProgramsInfo = provider(
    doc = "Resolved compact-v7 flag programs shared by lazy Linux actions.",
    fields = {
        "programs": "Dictionary from full compact-v7 program ID to its resolved argv File.",
    },
)

_HEX = "0123456789abcdef"
_NUL = json.decode('"\\u0000"')
_SELECT_KINDS = ["as_option", "cc_option", "ld_option"]

def _validate_id(value, what):
    if len(value) != 64:
        fail("%s must be a full lowercase SHA-256 digest, got %r" % (what, value))
    for character in value.elems():
        if character not in _HEX:
            fail("%s must be a full lowercase SHA-256 digest, got %r" % (what, value))

def _decode_record(encoded, what):
    value = json.decode(encoded)
    if type(value) != "dict":
        fail("%s must decode to an object" % what)
    return value

def _validate_argv(argv, what):
    if type(argv) != "list":
        fail("%s must be a list" % what)
    for index in range(len(argv)):
        value = argv[index]
        if type(value) != "string" or not value:
            fail("%s[%d] must be a non-empty string" % (what, index))
        if "\n" in value or "\r" in value or _NUL in value:
            fail("%s[%d] contains a newline or NUL" % (what, index))
    return argv

def _terminal_files(ctx):
    files = {}
    values = {}
    for terminal_id in sorted(ctx.attr.terminals.keys()):
        _validate_id(terminal_id, "terminal ID")
        argv = _validate_argv(
            json.decode(ctx.attr.terminals[terminal_id]),
            "terminal %s argv" % terminal_id,
        )
        out = ctx.actions.declare_file(
            ctx.label.name + ".flag_nodes/" + terminal_id + ".argv",
        )
        ctx.actions.write(out, "\n".join(argv) + ("\n" if argv else ""))
        files[terminal_id] = out
        values[terminal_id] = argv
    return struct(files = files, values = values)

def _source_files(ctx):
    if not ctx.attr.source_paths:
        return {
            file.path: file
            for file in ctx.files.srcs
        }
    if len(ctx.attr.source_paths) != len(ctx.files.srcs):
        fail(
            "linux_flag_programs source_paths has %d entries for %d srcs" %
            (len(ctx.attr.source_paths), len(ctx.files.srcs)),
        )
    files = {}
    for index in range(len(ctx.attr.source_paths)):
        path = ctx.attr.source_paths[index]
        if not path or path.startswith("/") or ".." in path.split("/"):
            fail("linux_flag_programs source_paths[%d] is invalid: %r" % (index, path))
        if path in files:
            fail("linux_flag_programs repeats source path %r" % path)
        files[path] = ctx.files.srcs[index]
    return files

def _referenced_source_files(argv, source_files):
    return [
        source_files[path]
        for path in sorted(source_files.keys())
        if any([path in arg for arg in argv])
    ]

def _decode_programs(ctx):
    programs = {}
    for program_id in sorted(ctx.attr.programs.keys()):
        _validate_id(program_id, "program ID")
        root = ctx.attr.programs[program_id]
        _validate_id(root, "program %s root" % program_id)
        programs[program_id] = root
    return programs

def _decode_probes(ctx, programs):
    probes = {}
    for probe_id in sorted(ctx.attr.probes.keys()):
        _validate_id(probe_id, "probe ID")
        raw = _decode_record(ctx.attr.probes[probe_id], "probe %s" % probe_id)
        allowed = {
            "candidate_argv": True,
            "context_program": True,
            "kind": True,
            "language": True,
            "srcarch": True,
        }
        unknown = [name for name in raw.keys() if name not in allowed]
        if unknown:
            fail("probe %s has unknown field(s): %s" % (probe_id, ", ".join(sorted(unknown))))
        for name in ["candidate_argv", "context_program", "kind"]:
            if name not in raw:
                fail("probe %s is missing field %r" % (probe_id, name))
        kind = raw["kind"]
        if kind not in _SELECT_KINDS:
            fail("probe %s has unsupported kind %r" % (probe_id, kind))
        context_program = raw["context_program"]
        if type(context_program) != "string":
            fail("probe %s context_program must be a string" % probe_id)
        _validate_id(context_program, "probe %s context_program" % probe_id)
        if context_program not in programs:
            fail("probe %s references unknown context program %s" % (probe_id, context_program))
        language = raw.get("language", "")
        srcarch = raw.get("srcarch", "")
        for value, name in [(language, "language"), (srcarch, "srcarch")]:
            if type(value) != "string":
                fail("probe %s %s must be a string" % (probe_id, name))
            if "\n" in value or "\r" in value or _NUL in value:
                fail("probe %s %s contains a newline or NUL" % (probe_id, name))
        probes[probe_id] = struct(
            candidate_argv = _validate_argv(
                raw["candidate_argv"],
                "probe %s candidate_argv" % probe_id,
            ),
            context_program = context_program,
            kind = kind,
            language = language,
            srcarch = srcarch,
        )
    return probes

def _decode_nodes(ctx, probes):
    nodes = {}
    for node_id in sorted(ctx.attr.nodes.keys()):
        _validate_id(node_id, "flag node ID")
        raw = _decode_record(ctx.attr.nodes[node_id], "flag node %s" % node_id)
        kind = raw.get("kind", "select")
        if kind == "select":
            allowed = {
                "kind": True,
                "probe": True,
                "when_false": True,
                "when_true": True,
            }
            required = ["probe", "when_false", "when_true"]
        elif kind == "concat":
            allowed = {
                "children": True,
                "kind": True,
            }
            required = ["children"]
        else:
            fail("flag node %s has unsupported kind %r" % (node_id, kind))
        unknown = [name for name in raw.keys() if name not in allowed]
        if unknown:
            fail("flag node %s has unknown field(s): %s" % (node_id, ", ".join(sorted(unknown))))
        missing = [name for name in required if name not in raw]
        if missing:
            fail("flag node %s is missing field(s): %s" % (node_id, ", ".join(missing)))
        if kind == "select":
            probe = raw["probe"]
            if type(probe) != "string" or probe not in probes:
                fail("flag node %s references unknown probe %r" % (node_id, probe))
            when_true = raw["when_true"]
            when_false = raw["when_false"]
            for value, name in [(when_true, "when_true"), (when_false, "when_false")]:
                if type(value) != "string":
                    fail("flag node %s %s must be a string" % (node_id, name))
                _validate_id(value, "flag node %s %s" % (node_id, name))
            if when_true == when_false:
                fail("flag node %s is not reduced" % node_id)
            nodes[node_id] = struct(
                children = [when_true, when_false],
                kind = kind,
                probe = probe,
                when_false = when_false,
                when_true = when_true,
            )
        else:
            children = raw["children"]
            if type(children) != "list" or not children:
                fail("concat flag node %s children must be a non-empty list" % node_id)
            for index in range(len(children)):
                child = children[index]
                if type(child) != "string":
                    fail("concat flag node %s children[%d] must be a string" % (node_id, index))
                _validate_id(child, "concat flag node %s children[%d]" % (node_id, index))
            nodes[node_id] = struct(
                children = children,
                kind = kind,
            )
    return nodes

def _resolve_select_node(
        ctx,
        profile,
        node_id,
        probe,
        context,
        when_true,
        when_false,
        source_inputs):
    out = ctx.actions.declare_file(
        ctx.label.name + ".flag_nodes/" + node_id + ".argv",
    )
    args = ctx.actions.args()
    args.add("resolve-node")
    args.add("-template", profile.command_template)
    args.add("-validation", profile.validation)
    args.add("-linker", profile.kbuild_linker)
    args.add("-kind", probe.kind)
    args.add(
        "-language",
        probe.language if probe.language else {
            "as_option": "asm",
            "cc_option": "c",
            "ld_option": "link",
        }[probe.kind],
    )
    args.add(
        "-srcarch",
        probe.srcarch if probe.srcarch else {
            "aarch64": "arm64",
            "x86_64": "x86",
        }[profile.arch],
    )
    args.add_all(probe.candidate_argv, before_each = "-candidate_arg")
    args.add("-context", context)
    args.add("-when_true", when_true)
    args.add("-when_false", when_false)
    add_directory_arg(
        args,
        directory_anchor(ctx.file.source_root, ctx.file.source_root.dirname),
        format = "-source_root=%s",
    )
    add_directory_arg(
        args,
        directory_anchor(out, out.dirname),
        format = "-object_root=%s",
    )
    args.add("-out", out)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._ccprofile,
        inputs = depset(
            [
                context,
                ctx.file.source_root,
                profile.command_template,
                profile.kbuild_linker,
                profile.validation,
                when_false,
                when_true,
            ] + source_inputs,
            transitive = [profile.toolchain_files],
        ),
        outputs = [out],
        arguments = [args],
        execution_requirements = profile.execution_requirements,
        mnemonic = "LinuxFlagSelect",
        progress_message = "Resolving Kbuild flag node %s" % node_id,
        toolchain = CC_TOOLCHAIN_TYPE,
    )
    return out

def _resolve_concat_node(ctx, node_id, children):
    out = ctx.actions.declare_file(
        ctx.label.name + ".flag_nodes/" + node_id + ".argv",
    )
    args = ctx.actions.args()
    args.add("concat-node")
    args.add_all(children, before_each = "-input")
    args.add("-out", out)
    ctx.actions.run(
        executable = ctx.executable._ccprofile,
        inputs = children,
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxFlagConcat",
        progress_message = "Concatenating Kbuild flag node %s" % node_id,
    )
    return out

def _linux_flag_programs_impl(ctx):
    terminals = _terminal_files(ctx)
    files = terminals.files
    possible_argv = terminals.values
    source_files = _source_files(ctx)
    programs = _decode_programs(ctx)
    probes = _decode_probes(ctx, programs)
    nodes = _decode_nodes(ctx, probes)
    profile = ctx.attr.graph_profile[LinuxGraphProfileInfo]

    pending = dict(nodes)
    for _ in range(len(nodes) + 1):
        progressed = False
        for node_id in sorted(pending.keys()):
            node = pending[node_id]
            if any([child not in files for child in node.children]):
                continue
            if node.kind == "select":
                probe = probes[node.probe]
                context_root = programs[probe.context_program]
                if context_root not in files:
                    continue
                possible_argv[node_id] = (
                    possible_argv[node.when_true] +
                    possible_argv[node.when_false]
                )
                files[node_id] = _resolve_select_node(
                    ctx,
                    profile,
                    node_id,
                    probe,
                    files[context_root],
                    files[node.when_true],
                    files[node.when_false],
                    _referenced_source_files(
                        probe.candidate_argv + possible_argv[context_root],
                        source_files,
                    ),
                )
            else:
                possible_argv[node_id] = []
                for child in node.children:
                    possible_argv[node_id].extend(possible_argv[child])
                files[node_id] = _resolve_concat_node(
                    ctx,
                    node_id,
                    [files[child] for child in node.children],
                )
            pending.pop(node_id)
            progressed = True
        if not pending or not progressed:
            break
    if pending:
        fail(
            "linux_flag_programs %s has unresolved or cyclic flag nodes: %s" %
            (ctx.label, ", ".join(sorted(pending.keys()))),
        )

    program_files = {}
    for program_id in sorted(programs.keys()):
        root = programs[program_id]
        if root not in files:
            fail("program %s references unknown flag root %s" % (program_id, root))
        program_files[program_id] = files[root]
    return [
        DefaultInfo(files = depset(program_files.values())),
        LinuxFlagProgramsInfo(programs = program_files),
        OutputGroupInfo(nodes = depset(files.values())),
    ]

linux_flag_programs = rule(
    implementation = _linux_flag_programs_impl,
    attrs = {
        "nodes": attr.string_dict(
            doc = "JSON-encoded select or concat nodes keyed by full content ID.",
        ),
        "probes": attr.string_dict(
            doc = "JSON-encoded Kbuild probes keyed by full content ID.",
        ),
        "graph_profile": attr.label(
            mandatory = True,
            providers = [LinuxGraphProfileInfo],
        ),
        "programs": attr.string_dict(
            mandatory = True,
            doc = "Program ID to root node or terminal ID.",
        ),
        "source_root": attr.label(
            allow_single_file = True,
            mandatory = True,
        ),
        "source_paths": attr.string_list(
            doc = "Canonical source-root-relative paths parallel to srcs.",
        ),
        "srcs": attr.label_list(
            allow_files = True,
            doc = "Source files that dynamic probe arguments may reference.",
        ),
        "terminals": attr.string_dict(
            mandatory = True,
            doc = "JSON argv arrays keyed by full terminal content ID.",
        ),
        "_ccprofile": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/ccprofile"),
            executable = True,
        ),
    },
    doc = "Resolves one shared compact-v7 flag DAG for the active C toolchain.",
    toolchains = use_cc_toolchain(),
)
