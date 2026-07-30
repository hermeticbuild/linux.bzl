"""Repository-side validation and indexing for compact-v7 metadata."""

visibility("//...")

_NUL = json.decode('"\\u0000"')

_PROTOCOL = "compact-v7-lazy-action-graph"
_HEX = "0123456789abcdef"
_EFFECT_ORDER = ["argv", "input", "output", "graph"]
_SHORT_ID_LENGTH = 24

_TOP_LEVEL_FIELDS = [
    "action_recipe_groups",
    "action_recipes",
    "action_source_groups",
    "compile_environment_abi",
    "compile_environments",
    "config_payloads",
    "configs",
    "flag_nodes",
    "flag_programs",
    "flag_terminals",
    "generated_header_families",
    "kbuild_probes",
    "object_variants",
    "protocol",
    "reachability_signatures",
    "source_files",
    "source_sets",
    "toolchain_profile",
]

def _expect_type(value, expected, path):
    if type(value) != expected:
        fail("%s must be a %s, got %s" % (path, expected, type(value)))
    return value

def _record(value, path, required, optional = []):
    _expect_type(value, "dict", path)
    allowed = {name: True for name in required + optional}
    for name in value:
        if name not in allowed:
            fail("%s has unknown field %r" % (path, name))
    for name in required:
        if name not in value:
            fail("%s is missing required field %r" % (path, name))
    return value

def _string(value, path, allow_empty = False):
    _expect_type(value, "string", path)
    if not allow_empty and not value.strip():
        fail("%s must be non-empty" % path)
    return value

def _bool(value, path):
    return _expect_type(value, "bool", path)

def _int(value, path):
    return _expect_type(value, "int", path)

def _list(value, path):
    return _expect_type(value, "list", path)

def _string_list(value, path, allow_empty = True):
    values = _list(value, path)
    if not allow_empty and not values:
        fail("%s must be non-empty" % path)
    for index in range(len(values)):
        _string(values[index], "%s[%d]" % (path, index), allow_empty = False)
    return values

def _full_id(value, path):
    value = _string(value, path)
    if len(value) != 64:
        fail("%s %r is not a full SHA-256 digest" % (path, value))
    for index in range(len(value)):
        if value[index] not in _HEX:
            fail("%s %r is not canonical lowercase hexadecimal" % (path, value))
    return value

def _relative_path(value, path):
    value = _string(value, path)
    if value.startswith("/") or "\\" in value:
        fail("%s %r is not a canonical relative path" % (path, value))
    for component in value.split("/"):
        if component in ["", ".", ".."]:
            fail("%s %r is not a canonical relative path" % (path, value))
    return value

def _sorted_unique_strings(values, path, allow_empty = True):
    values = _string_list(values, path, allow_empty = allow_empty)
    for index in range(1, len(values)):
        if values[index - 1] >= values[index]:
            fail("%s must be sorted and unique" % path)
    return values

def _unique_strings(values, path):
    values = _string_list(values, path)
    seen = {}
    for value in values:
        if value in seen:
            fail("%s repeats %r" % (path, value))
        seen[value] = True
    return values

def _ordered_records(records, path, key, validate_id = False, allow_empty = True):
    records = _list(records, path)
    if not allow_empty and not records:
        fail("%s must be non-empty" % path)
    result = {}
    previous = None
    for index in range(len(records)):
        record = records[index]
        _expect_type(record, "dict", "%s[%d]" % (path, index))
        if key not in record:
            fail("%s[%d] is missing required field %r" % (path, index, key))
        value = record[key]
        if validate_id:
            _full_id(value, "%s[%d].%s" % (path, index, key))
        else:
            _string(value, "%s[%d].%s" % (path, index, key))
        if previous != None and previous >= value:
            fail("%s must be canonically ordered by %s" % (path, key))
        previous = value
        result[value] = record
    return records, result

def _validate_top_level(metadata, expected_toolchain_profile, expected_compile_environment_abi):
    _record(metadata, "compact-v7 metadata", _TOP_LEVEL_FIELDS)
    protocol = _string(metadata["protocol"], "compact-v7 metadata.protocol")
    if protocol != _PROTOCOL:
        fail("compact-v7 protocol %r, want %r" % (protocol, _PROTOCOL))

    expected_toolchain_profile = _full_id(
        expected_toolchain_profile,
        "expected toolchain profile",
    )
    profile = _full_id(
        metadata["toolchain_profile"],
        "compact-v7 metadata.toolchain_profile",
    )
    if profile != expected_toolchain_profile:
        fail(
            "compact-v7 toolchain profile %s does not match expected profile %s" %
            (profile, expected_toolchain_profile),
        )

    expected_compile_environment_abi = _string(
        expected_compile_environment_abi,
        "expected compile environment ABI",
    )
    abi = _string(
        metadata["compile_environment_abi"],
        "compact-v7 metadata.compile_environment_abi",
    )
    if abi != expected_compile_environment_abi:
        fail(
            "compact-v7 compile environment ABI %r does not match expected ABI %r" %
            (abi, expected_compile_environment_abi),
        )
    for name in _TOP_LEVEL_FIELDS:
        if name in [
            "compile_environment_abi",
            "protocol",
            "toolchain_profile",
        ]:
            continue
        _list(metadata[name], "compact-v7 metadata.%s" % name)
    return profile, abi

def _index_config_payloads(metadata):
    records, _ = _ordered_records(
        metadata["config_payloads"],
        "config_payloads",
        "id",
        validate_id = True,
    )
    result = {}
    for index in range(len(records)):
        path = "config_payloads[%d]" % index
        record = _record(records[index], path, ["content", "id"])
        _expect_type(record["content"], "string", path + ".content")
        result[record["id"]] = struct(
            content = record["content"],
            id = record["id"],
        )
    return result

def _index_source_files(metadata):
    records = _list(metadata["source_files"], "source_files")
    files = []
    by_path = {}
    previous = None
    for index in range(len(records)):
        path = "source_files[%d]" % index
        record = _record(records[index], path, ["digest", "path"])
        source_path = _relative_path(record["path"], path + ".path")
        digest = _full_id(record["digest"], path + ".digest")
        if previous != None and previous >= source_path:
            fail("source_files must be canonically ordered by path")
        previous = source_path
        source = struct(
            digest = digest,
            index = index + 1,
            path = source_path,
        )
        files.append(source)
        by_path[source_path] = source
    return files, by_path

def _index_source_sets(metadata, source_files):
    records, _ = _ordered_records(
        metadata["source_sets"],
        "source_sets",
        "id",
        validate_id = True,
    )
    definitions = {}
    direct_memberships = 0
    child_edges = 0
    for index in range(len(records)):
        path = "source_sets[%d]" % index
        record = _record(records[index], path, ["id"], ["children", "files"])
        files = record.get("files", [])
        children = record.get("children", [])
        _list(files, path + ".files")
        _sorted_unique_strings(children, path + ".children")
        if not files and not children:
            fail("%s must not be empty" % path)
        previous_file = 0
        for file_index in files:
            _int(file_index, path + ".files[]")
            if file_index <= previous_file:
                fail("%s.files must be sorted and unique" % path)
            if file_index > len(source_files):
                fail("%s file index %d is out of range" % (path, file_index))
            previous_file = file_index
        definitions[record["id"]] = struct(
            children = children,
            direct_file_indices = files,
            id = record["id"],
        )
        direct_memberships += len(files)
        child_edges += len(children)

    for source_set in definitions.values():
        for child in source_set.children:
            if child not in definitions:
                fail(
                    "source set %s references unknown child source set %s" %
                    (source_set.id, child),
                )

    expanded = {}
    depths = {}
    remaining = {source_set_id: True for source_set_id in definitions}
    for _ in range(len(definitions)):
        if not remaining:
            break
        progress = False
        for source_set_id in sorted(remaining.keys()):
            source_set = definitions[source_set_id]
            ready = True
            for child in source_set.children:
                if child not in expanded:
                    ready = False
            if not ready:
                continue
            seen = {file_index: True for file_index in source_set.direct_file_indices}
            depth = 1
            for child in source_set.children:
                depth = max(depth, depths[child] + 1)
                for file_index in expanded[child]:
                    if file_index in seen:
                        fail(
                            "source set %s overlaps file index %d through child %s" %
                            (source_set_id, file_index, child),
                        )
                    seen[file_index] = True
            expanded[source_set_id] = sorted(seen.keys())
            depths[source_set_id] = depth
            remaining.pop(source_set_id)
            progress = True
        if not progress and remaining:
            fail("source set graph contains a cycle at %s" % sorted(remaining.keys())[0])
    if remaining:
        fail("source set graph contains a cycle at %s" % sorted(remaining.keys())[0])

    result = {}
    expanded_memberships = 0
    for source_set_id in sorted(definitions):
        definition = definitions[source_set_id]
        indices = expanded[source_set_id]
        result[source_set_id] = struct(
            children = definition.children,
            depth = depths[source_set_id],
            direct_file_indices = definition.direct_file_indices,
            file_indices = indices,
            files = [source_files[file_index - 1] for file_index in indices],
            id = source_set_id,
        )
        expanded_memberships += len(indices)
    return struct(
        child_edges = child_edges,
        direct_memberships = direct_memberships,
        expanded_memberships = expanded_memberships,
        max_depth = max(depths.values()) if depths else 0,
        values = result,
    )

def _index_action_source_groups(metadata, source_sets, source_files):
    records, _ = _ordered_records(
        metadata["action_source_groups"],
        "action_source_groups",
        "id",
        validate_id = True,
    )
    result = {}
    memberships = 0
    for index in range(len(records)):
        path = "action_source_groups[%d]" % index
        record = _record(records[index], path, ["id", "primary_source", "source_set"])
        source_set_id = _full_id(record["source_set"], path + ".source_set")
        if source_set_id not in source_sets:
            fail("%s references unknown source set %s" % (path, source_set_id))
        primary_index = _int(record["primary_source"], path + ".primary_source")
        if primary_index <= 0 or primary_index > len(source_files):
            fail("%s primary source index %d is out of range" % (path, primary_index))
        source_set = source_sets[source_set_id]
        if primary_index not in source_set.file_indices:
            fail(
                "%s source set omits primary source %r" %
                (path, source_files[primary_index - 1].path),
            )
        result[record["id"]] = struct(
            file_indices = source_set.file_indices,
            id = record["id"],
            primary_source = source_files[primary_index - 1],
            primary_source_index = primary_index,
            source_files = source_set.files,
            source_set = source_set,
            source_set_id = source_set_id,
        )
        memberships += len(source_set.file_indices)
    return result, memberships

def _index_generated_header_families(metadata, payloads, source_sets):
    records, _ = _ordered_records(
        metadata["generated_header_families"],
        "generated_header_families",
        "id",
        validate_id = True,
    )
    definitions = {}
    for index in range(len(records)):
        path = "generated_header_families[%d]" % index
        record = _record(
            records[index],
            path,
            ["config_payload", "id", "labels", "name", "srcarch"],
            ["dependencies", "source_set"],
        )
        name = _string(record["name"], path + ".name")
        srcarch = _string(record["srcarch"], path + ".srcarch")
        labels = _sorted_unique_strings(record["labels"], path + ".labels", allow_empty = False)
        dependencies = _sorted_unique_strings(
            record.get("dependencies", []),
            path + ".dependencies",
        )
        payload_id = _full_id(record["config_payload"], path + ".config_payload")
        if payload_id not in payloads:
            fail("%s references unknown config payload %s" % (path, payload_id))
        source_set_id = record.get("source_set", "")
        if source_set_id:
            _full_id(source_set_id, path + ".source_set")
            if source_set_id not in source_sets:
                fail("%s references unknown source set %s" % (path, source_set_id))
        definitions[record["id"]] = struct(
            config_payload = payloads[payload_id],
            config_payload_id = payload_id,
            dependencies = dependencies,
            id = record["id"],
            labels = labels,
            name = name,
            source_set = source_sets.get(source_set_id),
            source_set_id = source_set_id,
            srcarch = srcarch,
        )

    for family in definitions.values():
        for dependency in family.dependencies:
            if dependency not in definitions:
                fail(
                    "generated header family %s references unknown dependency %s" %
                    (family.id, dependency),
                )

    depths = {}
    remaining = {family_id: True for family_id in definitions}
    for _ in range(len(definitions)):
        if not remaining:
            break
        progress = False
        for family_id in sorted(remaining.keys()):
            family = definitions[family_id]
            if [dep for dep in family.dependencies if dep not in depths]:
                continue
            depths[family_id] = 1 + max(
                [depths[dep] for dep in family.dependencies] or
                [0],
            )
            remaining.pop(family_id)
            progress = True
        if not progress and remaining:
            fail(
                "generated header family graph contains a cycle at %s" %
                sorted(remaining.keys())[0],
            )
    return definitions

def _index_compile_environments(metadata, abi, payloads, families):
    records, _ = _ordered_records(
        metadata["compile_environments"],
        "compile_environments",
        "id",
        validate_id = True,
    )
    result = {}
    for index in range(len(records)):
        path = "compile_environments[%d]" % index
        record = _record(
            records[index],
            path,
            ["abi", "config_payload", "id"],
            ["generated_header_families"],
        )
        environment_abi = _string(record["abi"], path + ".abi")
        if environment_abi != abi:
            fail("%s ABI %r does not match metadata ABI %r" % (path, environment_abi, abi))
        payload_id = _full_id(record["config_payload"], path + ".config_payload")
        if payload_id not in payloads:
            fail("%s references unknown config payload %s" % (path, payload_id))
        family_ids = _sorted_unique_strings(
            record.get("generated_header_families", []),
            path + ".generated_header_families",
        )
        family_names = {}
        environment_families = []
        for family_id in family_ids:
            _full_id(family_id, path + ".generated_header_families[]")
            if family_id not in families:
                fail("%s references unknown generated header family %s" % (path, family_id))
            family = families[family_id]
            if family.name in family_names:
                fail("%s repeats generated header family name %r" % (path, family.name))
            family_names[family.name] = True
            environment_families.append(family)
        if "all" in family_names and len(family_names) != 1:
            fail("%s mixes \"all\" with precise generated header families" % path)
        result[record["id"]] = struct(
            abi = environment_abi,
            config_payload = payloads[payload_id],
            config_payload_id = payload_id,
            generated_header_families = environment_families,
            generated_header_family_ids = family_ids,
            id = record["id"],
        )
    return result

def _flag_consumes_input_operand(arg):
    return arg in [
        "--sysroot",
        "--target",
        "-D",
        "-I",
        "-U",
        "-idirafter",
        "-imacros",
        "-include",
        "-iquote",
        "-isystem",
        "-isysroot",
        "-mabi",
        "-march",
        "-mcpu",
        "-std",
        "-target",
        "-x",
    ]

def _joined_input_flag(arg):
    for prefix in [
        "--sysroot=",
        "-I",
        "-idirafter",
        "-imacros",
        "-include",
        "-iquote",
        "-isystem",
        "-isysroot",
        "-mabi=",
        "-march=",
        "-mcpu=",
    ]:
        if arg != prefix and arg.startswith(prefix):
            return True
    return False

def _flag_consumes_output_operand(arg):
    return arg in ["--serialize-diagnostics", "-MF", "-MJ", "-MQ", "-MT", "-o"]

def _joined_output_flag(arg):
    for prefix in ["-MF", "-MJ", "-MQ", "-MT", "-o"]:
        if arg != prefix and arg.startswith(prefix):
            return True
    return arg.startswith("--serialize-diagnostics=")

def _known_argv_only_flag(arg):
    if arg.startswith("-W") or arg.startswith("-O") or arg.startswith("-g"):
        return True
    return arg in [
        "-E",
        "-S",
        "-c",
        "-fhosted",
        "-fno-PIE",
        "-fno-asynchronous-unwind-tables",
        "-fno-common",
        "-fno-delete-null-pointer-checks",
        "-fno-omit-frame-pointer",
        "-fno-optimize-sibling-calls",
        "-fno-pie",
        "-fno-stack-protector",
        "-fno-strict-aliasing",
        "-fno-unwind-tables",
        "-fomit-frame-pointer",
        "-fshort-wchar",
        "-funsigned-char",
        "-pipe",
    ]

def _canonical_effects(values, path):
    seen = {}
    for value in values:
        if value not in _EFFECT_ORDER:
            fail("%s has unknown effect %r" % (path, value))
        seen[value] = True
    if not seen:
        seen["argv"] = True
    return [effect for effect in _EFFECT_ORDER if effect in seen]

def _classify_terminal_effects(argv):
    effects = {"argv": True}
    skip_operand = False
    for arg in argv:
        if skip_operand:
            skip_operand = False
            continue
        if arg == "":
            effects["output"] = True
        elif arg.startswith("@"):
            effects["input"] = True
            effects["output"] = True
        elif arg == "-flto" or arg == "-fno-lto" or arg.startswith("-flto="):
            effects["graph"] = True
        elif _flag_consumes_input_operand(arg):
            effects["input"] = True
            skip_operand = True
        elif _joined_input_flag(arg):
            effects["input"] = True
        elif _flag_consumes_output_operand(arg):
            effects["output"] = True
            skip_operand = True
        elif _joined_output_flag(arg):
            effects["output"] = True
        elif (
            arg.startswith("-D") or
            arg.startswith("-U") or
            arg.startswith("-m") or
            arg.startswith("-std=") or
            arg.startswith("--target=") or
            arg.startswith("-target=") or
            arg == "-nostdinc" or
            arg == "-nostdinc++" or
            arg == "-ffreestanding" or
            arg == "-fhosted" or
            arg.startswith("-fno-builtin")
        ):
            effects["input"] = True
        elif (
            arg.startswith("-save-temps") or
            arg.startswith("-ftime-trace") or
            arg.startswith("-fdump-") or
            arg.startswith("-fprofile-") or
            arg == "--coverage" or
            arg == "-gsplit-dwarf" or
            arg.startswith("-Wl,") or
            arg.startswith("-Wp,")
        ):
            effects["output"] = True
        elif not _known_argv_only_flag(arg):
            effects["output"] = True
    return [effect for effect in _EFFECT_ORDER if effect in effects]

def _index_flag_programs(metadata):
    terminal_records, _ = _ordered_records(
        metadata["flag_terminals"],
        "flag_terminals",
        "id",
        validate_id = True,
    )
    terminals = {}
    for index in range(len(terminal_records)):
        path = "flag_terminals[%d]" % index
        record = _record(terminal_records[index], path, ["argv", "id"])
        argv = _string_list(record["argv"], path + ".argv")
        for arg_index in range(len(argv)):
            if "\n" in argv[arg_index] or "\r" in argv[arg_index] or _NUL in argv[arg_index]:
                fail("%s.argv[%d] contains a newline or NUL" % (path, arg_index))
        terminals[record["id"]] = struct(
            argv = argv,
            id = record["id"],
        )

    probe_records, _ = _ordered_records(
        metadata["kbuild_probes"],
        "kbuild_probes",
        "id",
        validate_id = True,
    )
    probes = {}
    for index in range(len(probe_records)):
        path = "kbuild_probes[%d]" % index
        record = _record(
            probe_records[index],
            path,
            ["candidate_argv", "context_program", "id", "kind"],
            ["language", "srcarch"],
        )
        kind = _string(record["kind"], path + ".kind")
        if kind not in ["as_option", "cc_option", "ld_option"]:
            fail("%s has unsupported kind %r" % (path, kind))
        candidate_argv = _string_list(
            record["candidate_argv"],
            path + ".candidate_argv",
            allow_empty = False,
        )
        for arg_index in range(len(candidate_argv)):
            if "\n" in candidate_argv[arg_index] or "\r" in candidate_argv[arg_index] or _NUL in candidate_argv[arg_index]:
                fail("%s.candidate_argv[%d] contains a newline or NUL" % (path, arg_index))
        context_program = _full_id(
            record["context_program"],
            path + ".context_program",
        )
        probes[record["id"]] = struct(
            candidate_argv = candidate_argv,
            context_program = context_program,
            id = record["id"],
            kind = kind,
            language = _string(
                record.get("language", ""),
                path + ".language",
                allow_empty = True,
            ),
            srcarch = _string(
                record.get("srcarch", ""),
                path + ".srcarch",
                allow_empty = True,
            ),
        )

    node_records, _ = _ordered_records(
        metadata["flag_nodes"],
        "flag_nodes",
        "id",
        validate_id = True,
    )
    nodes = {}
    for index in range(len(node_records)):
        path = "flag_nodes[%d]" % index
        raw = node_records[index]
        kind = raw.get("kind", "select")
        _string(kind, path + ".kind")
        if kind == "select":
            record = _record(
                raw,
                path,
                ["id", "probe", "when_false", "when_true"],
                ["kind"],
            )
            probe_id = _full_id(record["probe"], path + ".probe")
            if probe_id not in probes:
                fail("%s references unknown probe %s" % (path, probe_id))
            when_true = _full_id(record["when_true"], path + ".when_true")
            when_false = _full_id(record["when_false"], path + ".when_false")
            if when_true == when_false:
                fail("%s is not reduced" % path)
            nodes[record["id"]] = struct(
                children = [when_true, when_false],
                id = record["id"],
                kind = kind,
                probe = probe_id,
                when_false = when_false,
                when_true = when_true,
            )
        elif kind == "concat":
            record = _record(raw, path, ["children", "id", "kind"])
            children = [
                _full_id(child, "%s.children[%d]" % (path, child_index))
                for child_index, child in enumerate(_list(record["children"], path + ".children"))
            ]
            if not children:
                fail("%s.children must not be empty" % path)
            nodes[record["id"]] = struct(
                children = children,
                id = record["id"],
                kind = kind,
            )
        else:
            fail("%s has unsupported kind %r" % (path, kind))

    root_effects = {
        terminal_id: _classify_terminal_effects(terminal.argv)
        for terminal_id, terminal in terminals.items()
    }
    root_argv = {
        terminal_id: terminal.argv
        for terminal_id, terminal in terminals.items()
    }
    pending_nodes = dict(nodes)
    for _ in range(len(nodes) + 1):
        progressed = False
        for node_id in sorted(pending_nodes.keys()):
            node = pending_nodes[node_id]
            if any([child not in root_effects for child in node.children]):
                continue
            effects = []
            for child in node.children:
                effects.extend(root_effects[child])
            argv = []
            for child in node.children:
                argv.extend(root_argv[child])
            root_effects[node_id] = _canonical_effects(
                effects,
                "flag node %s effects" % node_id,
            )
            root_argv[node_id] = argv
            pending_nodes.pop(node_id)
            progressed = True
        if not pending_nodes or not progressed:
            break
    if pending_nodes:
        fail(
            "flag node graph has unknown references or a cycle at: %s" %
            ", ".join(sorted(pending_nodes.keys())),
        )

    program_records, _ = _ordered_records(
        metadata["flag_programs"],
        "flag_programs",
        "id",
        validate_id = True,
    )
    programs = {}
    for index in range(len(program_records)):
        path = "flag_programs[%d]" % index
        record = _record(program_records[index], path, ["effects", "id", "root"])
        root = _full_id(record["root"], path + ".root")
        if root not in root_effects:
            fail("%s references unknown flag root %s" % (path, root))
        effects = _string_list(record["effects"], path + ".effects")
        canonical_effects = _canonical_effects(effects, path + ".effects")
        if effects != canonical_effects:
            fail("%s.effects must be canonical" % path)
        actual_effects = root_effects[root]
        if effects != actual_effects:
            fail(
                "%s effects %r do not match root effects %r" %
                (path, effects, actual_effects),
            )
        programs[record["id"]] = struct(
            argv = root_argv[root],
            effects = effects,
            id = record["id"],
            root = root,
        )
    for probe_id, probe in probes.items():
        if probe.context_program not in programs:
            fail(
                "Kbuild probe %s references unknown context program %s" %
                (probe_id, probe.context_program),
            )
    return struct(
        nodes = nodes,
        probes = probes,
        programs = programs,
        terminals = terminals,
    )

def _config_support_srcarch(families):
    srcarches = {
        family.srcarch: True
        for family in families.values()
    }
    if not srcarches:
        fail(
            "compact-v7 configs cannot infer srcarch because " +
            "generated_header_families is empty",
        )
    if len(srcarches) != 1:
        fail(
            "compact-v7 configs cannot infer a unique srcarch from " +
            "generated_header_families: %s" % sorted(srcarches.keys()),
        )
    return srcarches.keys()[0]

def _index_configs(metadata, payloads, source_sets, families):
    records, _ = _ordered_records(
        metadata["configs"],
        "configs",
        "name",
        allow_empty = False,
    )
    support_srcarch = _config_support_srcarch(families)
    expected_linker_script = "arch/%s/kernel/vmlinux.lds.S" % support_srcarch
    result = {}
    for index in range(len(records)):
        path = "configs[%d]" % index
        record = _record(
            records[index],
            path,
            ["config_payload", "name", "object_targets", "support_source_set"],
            ["module_object_targets"],
        )
        payload_id = _full_id(record["config_payload"], path + ".config_payload")
        if payload_id not in payloads:
            fail("%s references unknown config payload %s" % (path, payload_id))
        support_source_set_id = _full_id(
            record["support_source_set"],
            path + ".support_source_set",
        )
        if support_source_set_id not in source_sets:
            fail(
                "%s references unknown support source set %s" %
                (path, support_source_set_id),
            )
        support_source_set = source_sets[support_source_set_id]
        if not support_source_set.file_indices:
            fail("%s support source set must not be empty" % path)
        if expected_linker_script not in {
            source.path: True
            for source in support_source_set.files
        }:
            fail(
                "%s support source set %s omits expected linker script %r" %
                (path, support_source_set_id, expected_linker_script),
            )

        # Root order is Kbuild link order. Validate uniqueness without
        # canonicalizing or requiring lexical order.
        object_targets = _unique_strings(
            record["object_targets"],
            path + ".object_targets",
        )
        module_targets = _unique_strings(
            record.get("module_object_targets", []),
            path + ".module_object_targets",
        )
        overlap = [target for target in object_targets if target in module_targets]
        if overlap:
            fail("%s repeats target %r in object and module roots" % (path, overlap[0]))
        result[record["name"]] = struct(
            config_payload = payloads[payload_id],
            config_payload_id = payload_id,
            module_object_targets = module_targets,
            name = record["name"],
            object_targets = object_targets,
            support_source_set = support_source_set,
            support_source_set_id = support_source_set_id,
        )
    return result

def _index_reachability(metadata, configs):
    records, _ = _ordered_records(
        metadata["reachability_signatures"],
        "reachability_signatures",
        "id",
        validate_id = True,
    )
    result = {}
    for index in range(len(records)):
        path = "reachability_signatures[%d]" % index
        record = _record(records[index], path, ["configs", "id"])
        config_names = _sorted_unique_strings(
            record["configs"],
            path + ".configs",
            allow_empty = False,
        )
        for config_name in config_names:
            if config_name not in configs:
                fail("%s references unknown config %r" % (path, config_name))
        result[record["id"]] = struct(
            configs = config_names,
            id = record["id"],
        )
    return result

def _index_recipes(metadata, programs):
    records, _ = _ordered_records(
        metadata["action_recipes"],
        "action_recipes",
        "id",
        validate_id = True,
    )
    result = {}
    for index in range(len(records)):
        path = "action_recipes[%d]" % index
        record = _record(
            records[index],
            path,
            ["flag_program", "id", "kind", "mode", "remove_flag_program"],
            [
                "language",
                "modname",
                "module_root",
                "objtool_args",
                "objtool_disabled",
                "objtool_force",
            ],
        )
        kind = _string(record["kind"], path + ".kind")
        if kind not in ["arm64_nvhe", "compile", "composite"]:
            fail("%s has unsupported action kind %r" % (path, kind))
        language = record.get("language", "")
        _string(language, path + ".language", allow_empty = True)
        if kind == "compile" and language not in ["asm", "c"]:
            fail("%s has invalid compile language %r" % (path, language))
        if kind == "composite" and language:
            fail("%s composite recipe has compile language %r" % (path, language))
        mode = _string(record["mode"], path + ".mode")
        if mode not in ["m", "y"]:
            fail("%s has invalid mode %r" % (path, mode))
        flag_program_id = _full_id(record["flag_program"], path + ".flag_program")
        remove_program_id = _full_id(
            record["remove_flag_program"],
            path + ".remove_flag_program",
        )
        if flag_program_id not in programs:
            fail("%s references unknown flag program %s" % (path, flag_program_id))
        if remove_program_id not in programs:
            fail("%s references unknown remove-flag program %s" % (path, remove_program_id))
        module_root = record.get("module_root", False)
        objtool_disabled = record.get("objtool_disabled", False)
        objtool_force = record.get("objtool_force", False)
        _bool(module_root, path + ".module_root")
        _bool(objtool_disabled, path + ".objtool_disabled")
        _bool(objtool_force, path + ".objtool_force")
        modname = record.get("modname", "")
        _string(modname, path + ".modname", allow_empty = True)
        objtool_args = _string_list(record.get("objtool_args", []), path + ".objtool_args")
        result[record["id"]] = struct(
            flag_program = programs[flag_program_id],
            flag_program_id = flag_program_id,
            id = record["id"],
            kind = kind,
            language = language,
            mode = mode,
            modname = modname,
            module_root = module_root,
            objtool_args = objtool_args,
            objtool_disabled = objtool_disabled,
            objtool_force = objtool_force,
            remove_flag_program = programs[remove_program_id],
            remove_flag_program_id = remove_program_id,
        )
    return result

def _index_objects(metadata, recipes, reachability, environments, action_source_groups):
    records, _ = _ordered_records(
        metadata["object_variants"],
        "object_variants",
        "target",
        allow_empty = False,
    )
    definitions = {}
    by_content_id = {}
    for index in range(len(records)):
        path = "object_variants[%d]" % index
        record = _record(
            records[index],
            path,
            ["content_id", "object", "reachability", "recipe", "recipe_group", "target"],
            ["action_source_group", "compile_environment", "deps", "members"],
        )
        content_id = _full_id(record["content_id"], path + ".content_id")
        if content_id in by_content_id:
            fail(
                "%s duplicates content ID used by object %r" %
                (path, by_content_id[content_id]),
            )
        target = record["target"]
        if not target.endswith("__" + content_id[:_SHORT_ID_LENGTH]):
            fail("%s target does not end in its short content ID" % path)
        object_path = _relative_path(record["object"], path + ".object")
        recipe_id = _full_id(record["recipe"], path + ".recipe")
        reachability_id = _full_id(record["reachability"], path + ".reachability")
        recipe_group_id = _full_id(record["recipe_group"], path + ".recipe_group")
        if recipe_id not in recipes:
            fail("%s references unknown recipe %s" % (path, recipe_id))
        if reachability_id not in reachability:
            fail("%s references unknown reachability %s" % (path, reachability_id))
        compile_environment_id = record.get("compile_environment", "")
        action_source_group_id = record.get("action_source_group", "")
        if compile_environment_id:
            _full_id(compile_environment_id, path + ".compile_environment")
            if compile_environment_id not in environments:
                fail("%s references unknown compile environment %s" % (path, compile_environment_id))
        if action_source_group_id:
            _full_id(action_source_group_id, path + ".action_source_group")
            if action_source_group_id not in action_source_groups:
                fail("%s references unknown action source group %s" % (path, action_source_group_id))
        deps = _sorted_unique_strings(record.get("deps", []), path + ".deps")
        members = _unique_strings(record.get("members", []), path + ".members")
        if target in deps or target in members:
            fail("%s directly references itself" % path)
        kind = recipes[recipe_id].kind
        if kind == "compile":
            if not compile_environment_id or not action_source_group_id or members:
                fail("%s compile object has invalid environment/source/member metadata" % path)
        elif kind == "arm64_nvhe":
            if not compile_environment_id or not action_source_group_id or not members:
                fail("%s arm64 nVHE object has invalid environment/source/member metadata" % path)
        elif compile_environment_id or action_source_group_id or not members:
            fail("%s composite object has compile-only metadata" % path)
        definition = struct(
            action_source_group = action_source_groups.get(action_source_group_id),
            action_source_group_id = action_source_group_id,
            compile_environment = environments.get(compile_environment_id),
            compile_environment_id = compile_environment_id,
            content_id = content_id,
            deps = deps,
            members = members,
            object = object_path,
            reachability = reachability[reachability_id],
            reachability_id = reachability_id,
            recipe = recipes[recipe_id],
            recipe_group_id = recipe_group_id,
            recipe_id = recipe_id,
            target = target,
        )
        definitions[target] = definition
        by_content_id[content_id] = target

    dependency_edges = 0
    member_edges = 0
    for obj in definitions.values():
        for dependency in obj.deps:
            if dependency not in definitions:
                fail("object %r references unknown dependency %r" % (obj.target, dependency))
        for member in obj.members:
            if member not in definitions:
                fail("object %r references unknown member %r" % (obj.target, member))
        dependency_edges += len(obj.deps)
        member_edges += len(obj.members)

    closures = {}
    depths = {}
    remaining = {target: True for target in definitions}
    for _ in range(len(definitions)):
        if not remaining:
            break
        progress = False
        for target in sorted(remaining.keys()):
            obj = definitions[target]
            children = obj.deps + obj.members
            if [child for child in children if child not in closures]:
                continue
            closure = {target: True}
            depth = 1
            for child in children:
                depth = max(depth, depths[child] + 1)
                for reachable in closures[child]:
                    closure[reachable] = True
            closures[target] = sorted(closure.keys())
            depths[target] = depth
            remaining.pop(target)
            progress = True
        if not progress and remaining:
            fail("object dependency/member graph contains a cycle at %r" % sorted(remaining.keys())[0])

    values = {}
    content_values = {}
    for target in sorted(definitions):
        obj = definitions[target]
        value = struct(
            action_source_group = obj.action_source_group,
            action_source_group_id = obj.action_source_group_id,
            closure = closures[target],
            compile_environment = obj.compile_environment,
            compile_environment_id = obj.compile_environment_id,
            content_id = obj.content_id,
            dependencies = [definitions[dep] for dep in obj.deps],
            dependency_targets = obj.deps,
            depth = depths[target],
            member_targets = obj.members,
            members = [definitions[member] for member in obj.members],
            object = obj.object,
            reachability = obj.reachability,
            reachability_id = obj.reachability_id,
            recipe = obj.recipe,
            recipe_group_id = obj.recipe_group_id,
            recipe_id = obj.recipe_id,
            target = target,
        )
        values[target] = value
        content_values[obj.content_id] = value
    return struct(
        by_content_id = content_values,
        dependency_edges = dependency_edges,
        max_depth = max(depths.values()) if depths else 0,
        member_edges = member_edges,
        values = values,
    )

def _expand_configs(configs, objects):
    expected_reachability = {target: {} for target in objects}
    result = {}
    root_memberships = 0
    reachable_memberships = 0
    for name in sorted(configs):
        config = configs[name]
        roots = config.object_targets + config.module_object_targets
        reachable = {}
        for target in roots:
            if target not in objects:
                fail("config %r references unknown object target %r" % (name, target))
            for reached in objects[target].closure:
                reachable[reached] = True
                expected_reachability[reached][name] = True
        reachable_targets = sorted(reachable.keys())
        result[name] = struct(
            config_payload = config.config_payload,
            config_payload_id = config.config_payload_id,
            module_object_targets = config.module_object_targets,
            name = name,
            object_targets = config.object_targets,
            object_variants = [objects[target] for target in reachable_targets],
            reachable_object_targets = reachable_targets,
            support_source_set = config.support_source_set,
            support_source_set_id = config.support_source_set_id,
        )
        root_memberships += len(roots)
        reachable_memberships += len(reachable_targets)

    for target in sorted(objects):
        actual = sorted(expected_reachability[target].keys())
        if not actual:
            fail("object %r is unreachable from every config" % target)
        expected = objects[target].reachability.configs
        if actual != expected:
            fail(
                "object %r reachability %r does not match config graph %r" %
                (target, expected, actual),
            )
    return result, root_memberships, reachable_memberships

def _index_recipe_groups(metadata, recipes, reachability, objects):
    records, _ = _ordered_records(
        metadata["action_recipe_groups"],
        "action_recipe_groups",
        "id",
        validate_id = True,
    )
    result = {}
    by_stable_key = {}
    object_groups = {}
    memberships = 0
    for index in range(len(records)):
        path = "action_recipe_groups[%d]" % index
        record = _record(records[index], path, ["id", "objects", "reachability", "recipe"])
        recipe_id = _full_id(record["recipe"], path + ".recipe")
        reachability_id = _full_id(record["reachability"], path + ".reachability")
        if recipe_id not in recipes:
            fail("%s references unknown recipe %s" % (path, recipe_id))
        if reachability_id not in reachability:
            fail("%s references unknown reachability %s" % (path, reachability_id))
        targets = _sorted_unique_strings(record["objects"], path + ".objects", allow_empty = False)
        stable_key = recipe_id + "__" + reachability_id
        if stable_key in by_stable_key:
            fail("%s duplicates recipe/reachability pair" % path)
        group_objects = []
        for target in targets:
            if target not in objects:
                fail("%s references unknown object %r" % (path, target))
            obj = objects[target]
            if obj.recipe_id != recipe_id or obj.reachability_id != reachability_id:
                fail("%s does not match object %r" % (path, target))
            if target in object_groups:
                fail("%s repeats object %r from another recipe group" % (path, target))
            if obj.recipe_group_id != record["id"]:
                fail(
                    "object %r recipe group %s does not match %s" %
                    (target, obj.recipe_group_id, record["id"]),
                )
            object_groups[target] = record["id"]
            group_objects.append(obj)
        group = struct(
            id = record["id"],
            object_targets = targets,
            objects = group_objects,
            reachability = reachability[reachability_id],
            reachability_id = reachability_id,
            recipe = recipes[recipe_id],
            recipe_id = recipe_id,
            stable_key = stable_key,
        )
        result[record["id"]] = group
        by_stable_key[stable_key] = group
        memberships += len(targets)
    for target in objects:
        if target not in object_groups:
            fail("object %r is not owned by an action recipe group" % target)
    return result, by_stable_key, memberships

def _require_all_used(kind, values, used):
    for key in values:
        if key not in used:
            fail("compact-v7 %s %s is not referenced" % (kind, key))

def _validate_referenced_tables(
        configs,
        payloads,
        environments,
        families,
        source_sets,
        source_files,
        action_source_groups,
        probes,
        nodes,
        terminals,
        programs,
        reachability,
        recipes,
        recipe_groups,
        objects):
    used_payloads = {config.config_payload_id: True for config in configs.values()}
    used_environments = {}

    # Generated-header labels are implicit root consumers: the root rule keeps
    # its complete family contract even if no selected object uses one directly.
    used_families = {family_id: True for family_id in families}
    used_source_sets = {
        config.support_source_set_id: True
        for config in configs.values()
    }
    used_action_source_groups = {}
    used_probes = {}
    used_nodes = {}
    used_programs = {}
    used_terminals = {}
    used_reachability = {}
    used_recipes = {}
    used_recipe_groups = {}

    for obj in objects.values():
        used_recipes[obj.recipe_id] = True
        used_reachability[obj.reachability_id] = True
        used_recipe_groups[obj.recipe_group_id] = True
        if obj.compile_environment_id:
            used_environments[obj.compile_environment_id] = True
        if obj.action_source_group_id:
            used_action_source_groups[obj.action_source_group_id] = True
    for recipe_id in used_recipes:
        recipe = recipes[recipe_id]
        used_programs[recipe.flag_program_id] = True
        used_programs[recipe.remove_flag_program_id] = True
    expanded_programs = {}
    expanded_roots = {}
    for _ in range(len(programs) + len(nodes) + 1):
        progressed = False
        for program_id in sorted(used_programs.keys()):
            if program_id in expanded_programs:
                continue
            expanded_programs[program_id] = True
            root = programs[program_id].root
            expanded_roots.setdefault(root, True)
            progressed = True
        for root in sorted(expanded_roots.keys()):
            if expanded_roots[root] == False:
                continue
            expanded_roots[root] = False
            progressed = True
            if root in terminals:
                used_terminals[root] = True
                continue
            if root not in nodes:
                fail("compact-v7 references unknown flag root %s" % root)
            node = nodes[root]
            used_nodes[root] = True
            for child in node.children:
                expanded_roots.setdefault(child, True)
            if node.kind == "select":
                used_probes[node.probe] = True
                used_programs[probes[node.probe].context_program] = True
        if not progressed:
            break
    for environment_id in used_environments:
        environment = environments[environment_id]
        used_payloads[environment.config_payload_id] = True
        for family_id in environment.generated_header_family_ids:
            used_families[family_id] = True
    for _ in range(len(families)):
        for family_id in list(used_families.keys()):
            family = families[family_id]
            used_payloads[family.config_payload_id] = True
            if family.source_set_id:
                used_source_sets[family.source_set_id] = True
            for dependency in family.dependencies:
                used_families[dependency] = True
    for group_id in used_action_source_groups:
        used_source_sets[action_source_groups[group_id].source_set_id] = True
    for _ in range(len(source_sets)):
        for source_set_id in list(used_source_sets.keys()):
            for child in source_sets[source_set_id].children:
                used_source_sets[child] = True
    used_source_files = {}
    for source_set_id in used_source_sets:
        for file_index in source_sets[source_set_id].direct_file_indices:
            used_source_files[file_index] = True

    _require_all_used("config payload", payloads, used_payloads)
    _require_all_used("compile environment", environments, used_environments)
    _require_all_used("generated header family", families, used_families)
    _require_all_used("source set", source_sets, used_source_sets)
    _require_all_used("action source group", action_source_groups, used_action_source_groups)
    _require_all_used("Kbuild probe", probes, used_probes)
    _require_all_used("flag node", nodes, used_nodes)
    _require_all_used("flag terminal", terminals, used_terminals)
    _require_all_used("flag program", programs, used_programs)
    _require_all_used("reachability signature", reachability, used_reachability)
    _require_all_used("action recipe", recipes, used_recipes)
    _require_all_used("action recipe group", recipe_groups, used_recipe_groups)
    for source in source_files:
        if source.index not in used_source_files:
            fail("compact-v7 source file %d %r is not referenced" % (source.index, source.path))

def compact_v7_repository_model(
        metadata,
        expected_toolchain_profile,
        expected_compile_environment_abi):
    """Validates compact-v7 metadata and returns repository-ready indices."""
    profile, abi = _validate_top_level(
        metadata,
        expected_toolchain_profile,
        expected_compile_environment_abi,
    )
    payloads = _index_config_payloads(metadata)
    source_files, source_files_by_path = _index_source_files(metadata)
    source_set_index = _index_source_sets(metadata, source_files)
    source_sets = source_set_index.values
    action_source_groups, action_source_memberships = _index_action_source_groups(
        metadata,
        source_sets,
        source_files,
    )
    families = _index_generated_header_families(metadata, payloads, source_sets)
    environments = _index_compile_environments(metadata, abi, payloads, families)
    program_index = _index_flag_programs(metadata)
    terminals = program_index.terminals
    probes = program_index.probes
    nodes = program_index.nodes
    programs = program_index.programs
    raw_configs = _index_configs(metadata, payloads, source_sets, families)
    reachability = _index_reachability(metadata, raw_configs)
    recipes = _index_recipes(metadata, programs)
    object_index = _index_objects(
        metadata,
        recipes,
        reachability,
        environments,
        action_source_groups,
    )
    objects = object_index.values
    configs, config_roots, config_memberships = _expand_configs(raw_configs, objects)
    recipe_groups, recipe_groups_by_stable_key, recipe_group_memberships = _index_recipe_groups(
        metadata,
        recipes,
        reachability,
        objects,
    )
    _validate_referenced_tables(
        configs,
        payloads,
        environments,
        families,
        source_sets,
        source_files,
        action_source_groups,
        probes,
        nodes,
        terminals,
        programs,
        reachability,
        recipes,
        recipe_groups,
        objects,
    )

    kind_counts = {
        "arm64_nvhe": 0,
        "compile": 0,
        "composite": 0,
    }
    for obj in objects.values():
        kind_counts[obj.recipe.kind] += 1
    graph_stats = struct(
        action_source_file_memberships = action_source_memberships,
        action_source_group_count = len(action_source_groups),
        arm64_nvhe_object_count = kind_counts["arm64_nvhe"],
        compile_environment_count = len(environments),
        compile_object_count = kind_counts["compile"],
        composite_object_count = kind_counts["composite"],
        config_count = len(configs),
        config_object_memberships = config_memberships,
        config_payload_count = len(payloads),
        config_root_memberships = config_roots,
        dependency_edge_count = object_index.dependency_edges,
        flag_program_count = len(programs),
        flag_node_count = len(nodes),
        flag_terminal_count = len(terminals),
        kbuild_probe_count = len(probes),
        generated_header_family_count = len(families),
        max_object_depth = object_index.max_depth,
        max_source_set_depth = source_set_index.max_depth,
        member_edge_count = object_index.member_edges,
        object_count = len(objects),
        reachability_signature_count = len(reachability),
        recipe_count = len(recipes),
        recipe_group_count = len(recipe_groups),
        recipe_group_object_memberships = recipe_group_memberships,
        source_file_count = len(source_files),
        source_set_child_edges = source_set_index.child_edges,
        source_set_count = len(source_sets),
        source_set_direct_file_memberships = source_set_index.direct_memberships,
        source_set_expanded_file_memberships = source_set_index.expanded_memberships,
    )
    return struct(
        action_source_groups = action_source_groups,
        compile_environment_abi = abi,
        compile_environments = environments,
        config_payloads = payloads,
        configs = configs,
        flag_programs = programs,
        flag_nodes = nodes,
        flag_terminals = terminals,
        generated_header_families = families,
        graph_stats = graph_stats,
        object_variants = objects,
        objects_by_content_id = object_index.by_content_id,
        protocol = _PROTOCOL,
        reachability_signatures = reachability,
        recipe_groups = recipe_groups,
        recipe_groups_by_stable_key = recipe_groups_by_stable_key,
        recipes = recipes,
        source_files = source_files,
        source_files_by_path = source_files_by_path,
        source_sets = source_sets,
        kbuild_probes = probes,
        toolchain_profile = profile,
    )

_EMITTER_SPECIAL_DIRECT_OBJECTS = {
    "arch/arm64/kernel/vdso-wrap.o": True,
    "arch/arm64/kernel/vdso32-wrap.o": True,
    "arch/x86/entry/vdso/vdso-image-64.o": True,
    "arch/x86/kernel/cpu/capflags.o": True,
    "arch/x86/lib/inat.o": True,
    "arch/x86/purgatory/kexec-purgatory.o": True,
    "arch/x86/realmode/rmpiggy.o": True,
    "drivers/of/empty_root.dtb.o": True,
    "drivers/scsi/scsi_sysfs.o": True,
    "drivers/tty/vt/consolemap_deftbl.o": True,
    "drivers/tty/vt/ucs.o": True,
    "lib/crc/crc32-main.o": True,
    "lib/crc/crc64-main.o": True,
    "lib/crc32.o": True,
    "lib/crc64.o": True,
    "lib/oid_registry.o": True,
    "usr/initramfs_data.o": True,
}

_EMITTER_KNOWN_EMPTY_MAKE_REFS = {
    "CC_FLAGS_CFI": True,
    "CC_FLAGS_FTRACE": True,
    "CC_FLAGS_LTO": True,
    "CC_FLAGS_SCS": True,
    "CLANG_FLAGS": True,
    "DISABLE_KSTACK_ERASE": True,
    "DISABLE_LATENT_ENTROPY_PLUGIN": True,
    "DISABLE_STACKLEAK_PLUGIN": True,
    "RANDSTRUCT_CFLAGS": True,
    "cflags-nogcse-yy": True,
}

def _emitter_label(label_package, target):
    if not label_package:
        return "//:" + target
    if label_package.startswith("@") or label_package.startswith("//"):
        return (label_package[:-1] if label_package.endswith(":") else label_package) + ":" + target
    return "//" + label_package + ":" + target

def _emitter_rules_label(rules_repo, file):
    if rules_repo:
        return (rules_repo[:-1] if rules_repo.endswith("/") else rules_repo) + "//internal:" + file
    return "//internal:" + file

def _emitter_rule(lines, kind, name, attrs):
    lines.append(kind + "(")
    lines.append("    name = %r," % name)
    for key in sorted(attrs.keys()):
        value = attrs[key]
        if value == None:
            continue
        lines.append("    %s = %r," % (key, value))
    lines.append(")")
    lines.append("")

def _emitter_json(values):
    ordered = {}
    for key in sorted(values.keys()):
        ordered[key] = values[key]
    return json.encode(ordered)

def _emitter_make_refs(value):
    refs = []
    for index in range(max(0, len(value) - 1)):
        opening = value[index:index + 2]
        if opening not in ["$(", "${"]:
            continue
        close = ")" if opening == "$(" else "}"
        end = value.find(close, index + 2)
        refs.append(value[index + 2:end] if end >= 0 else value[index:])
    return refs

def _emitter_config_enabled(content, names):
    enabled = {name: True for name in names}
    for line in content.splitlines():
        if not line.startswith("CONFIG_") or "=" not in line:
            continue
        name, value = line.split("=", 1)
        if name in enabled and value == "y":
            return True
    return False

def _emitter_direct_fallback_reason(obj):
    recipe = obj.recipe
    source_group = obj.action_source_group
    source = source_group.primary_source.path
    if obj.dependency_targets:
        return "has generated-header object dependencies"
    if "output" in recipe.flag_program.effects or "graph" in recipe.flag_program.effects:
        return "flag program has output or graph effects"
    if "output" in recipe.remove_flag_program.effects or "graph" in recipe.remove_flag_program.effects:
        return "remove-flag program has output or graph effects"
    if obj.object in _EMITTER_SPECIAL_DIRECT_OBJECTS:
        return "requires a generated-object action"
    if (
        obj.object.endswith(".asn1.o") or
        obj.object.endswith(".pi.o") or
        obj.object.endswith(".stub.o")
    ):
        return "requires generated-source or post-compile processing"
    if source.endswith(".c_shipped"):
        return "requires source materialization"
    if recipe.language == "c" and not source.endswith(".c"):
        return "C recipe has a non-C primary source"
    if recipe.language == "asm" and not (source.endswith(".S") or source.endswith(".s")):
        return "assembler recipe has a non-assembler primary source"
    if not (source.endswith(".c") or source.endswith(".S") or source.endswith(".s")):
        return "has an unsupported primary source"
    required = [
        "include/linux/compiler-version.h",
        "include/linux/kconfig.h",
    ]
    if recipe.language == "c":
        required.append("include/linux/compiler_types.h")
    source_paths = {source.path: True for source in source_group.source_files}
    for path in required:
        if path not in source_paths:
            return "exact source set omits required preinclude " + path
    for value in recipe.flag_program.argv + recipe.remove_flag_program.argv:
        if "$(obj)" in value or "${obj}" in value or "utsversion-tmp.h" in value:
            return "requires an object-local generated directory"
        for ref in _emitter_make_refs(value):
            if ref not in ["src", "srctree"] and ref not in _EMITTER_KNOWN_EMPTY_MAKE_REFS:
                return "contains analysis-time Make reference " + ref
    use_objtool = not recipe.objtool_disabled
    if (
        recipe.mode == "m" and
        recipe.language == "c" and
        (
            recipe.module_root or
            (recipe.objtool_force and use_objtool)
        ) and
        _emitter_config_enabled(
            obj.compile_environment.config_payload.content,
            [
                "CONFIG_LTO_CLANG",
                "CONFIG_LTO_CLANG_FULL",
                "CONFIG_LTO_CLANG_THIN",
            ],
        )
    ):
        return "requires the module LTO relocatable-link stage"
    return ""

def _emitter_classify_fallbacks(model, arch):
    reasons = {}
    for target in sorted(model.object_variants.keys()):
        obj = model.object_variants[target]
        kind = obj.recipe.kind
        if kind == "compile":
            reason = _emitter_direct_fallback_reason(obj)
            if reason:
                reasons[target] = reason
        elif kind == "composite":
            if obj.dependency_targets:
                fail(
                    "compact-v7 emitter cannot preserve composite object %s dependencies %s" %
                    (target, obj.dependency_targets),
                )
        elif kind == "arm64_nvhe":
            if arch != "arm64" or obj.recipe.mode != "y":
                fail("compact-v7 nVHE object %s requires an arm64 built-in graph" % target)
            if obj.object != "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.o":
                fail("compact-v7 nVHE object %s has unsupported path %s" % (target, obj.object))
            if obj.dependency_targets:
                fail(
                    "compact-v7 emitter cannot preserve nVHE object %s dependencies %s" %
                    (target, obj.dependency_targets),
                )

    # A legacy rule can only consume dependency/member labels. Promote those
    # children into the same small legacy island instead of duplicating actions.
    for _ in range(len(model.object_variants)):
        changed = False
        for target in sorted(list(reasons.keys())):
            obj = model.object_variants[target]
            children = list(obj.dependency_targets)
            if obj.recipe.kind in ["arm64_nvhe", "composite"]:
                children.extend(obj.member_targets)
            for child in children:
                if child not in reasons:
                    reasons[child] = "required by legacy object " + target
                    changed = True
        if not changed:
            break
    return reasons

def _emitter_group_name(group, suffix):
    return "_group_" + group.id + "_" + suffix

def _emitter_legacy_name(target):
    return "_legacy_" + target

def _emitter_local_sources(model, objects):
    used = {}
    for obj in objects:
        for index in obj.action_source_group.file_indices:
            used[index] = True
    global_indices = sorted(used.keys())
    local_by_global = {}
    for offset in range(len(global_indices)):
        local_by_global[global_indices[offset]] = offset + 1
    return struct(
        global_indices = global_indices,
        local_by_global = local_by_global,
        paths = [model.source_files[index - 1].path for index in global_indices],
    )

def _emitter_group_object_specs(objects, local_sources, include_action_source):
    specs = {}
    for obj in objects:
        value = {
            "content_id": obj.content_id,
            "deps": obj.dependency_targets,
            "members": obj.member_targets,
            "object": obj.object,
        }
        if include_action_source:
            group = obj.action_source_group
            value.update({
                "action_source_group": obj.action_source_group_id,
                "compile_environment": obj.compile_environment_id,
                "primary_source": local_sources.local_by_global[group.primary_source_index],
                "source_files": [
                    local_sources.local_by_global[index]
                    for index in group.file_indices
                ],
            })
        specs[obj.target] = _emitter_json(value)
    return specs

def _emitter_source_labels(source_label_package, paths):
    return [
        _emitter_label(source_label_package, path)
        for path in paths
    ]

def _emitter_compile_environment_index_name(reachability_id):
    return "_compile_environment_index_" + reachability_id

def _emitter_fallback_source_index_name(reachability_id):
    return "_fallback_source_input_index_" + reachability_id

def _emitter_generated_header_cover(environments, reachability_id):
    required_family_ids = {}
    family_ids_by_label = {}
    for environment in environments:
        for family in environment.generated_header_families:
            required_family_ids[family.id] = True
            for label in family.labels:
                family_ids_by_label.setdefault(label, {})[family.id] = True

    uncovered = dict(required_family_ids)
    selected = []
    for _ in range(len(required_family_ids)):
        if not uncovered:
            break
        best_label = ""
        best_count = 0
        for label in sorted(family_ids_by_label.keys()):
            covered_count = len([
                family_id
                for family_id in family_ids_by_label[label]
                if family_id in uncovered
            ])
            if covered_count > best_count:
                best_count = covered_count
                best_label = label
        if not best_label:
            fail(
                "compact-v7 reachability %s has generated-header families with no producer labels: %s" %
                (reachability_id, sorted(uncovered.keys())),
            )
        selected.append(best_label)
        covered = family_ids_by_label[best_label]
        uncovered = {
            family_id: True
            for family_id in uncovered
            if family_id not in covered
        }
    if uncovered:
        fail(
            "compact-v7 reachability %s did not cover generated-header families: %s" %
            (reachability_id, sorted(uncovered.keys())),
        )
    return sorted(selected)

def _emitter_compile_environment_partitions(model, fallback_reasons):
    environment_ids_by_reachability = {}
    analysis_environment_ids_by_reachability = {}
    for target in sorted(model.object_variants.keys()):
        obj = model.object_variants[target]
        if not obj.compile_environment_id:
            continue
        reachability_id = obj.reachability_id
        environment_ids_by_reachability.setdefault(reachability_id, {})[obj.compile_environment_id] = True
        if target in fallback_reasons:
            analysis_environment_ids_by_reachability.setdefault(reachability_id, {})[obj.compile_environment_id] = True

    partitions = {}
    payload_files = {}
    for reachability_id in sorted(environment_ids_by_reachability.keys()):
        environment_ids = environment_ids_by_reachability[reachability_id]
        analysis_environment_ids = analysis_environment_ids_by_reachability.get(reachability_id, {})
        environments = {}
        environment_values = []
        referenced_payload_ids = {}
        for environment_id in sorted(environment_ids.keys()):
            environment = model.compile_environments[environment_id]
            environment_values.append(environment)
            environments[environment_id] = _emitter_json({
                "abi": environment.abi,
                "config_payload": environment.config_payload_id,
                "generated_header_families": environment.generated_header_family_ids,
            })
            referenced_payload_ids[environment.config_payload_id] = True

        payload_labels = {}
        for payload_id in sorted(referenced_payload_ids.keys()):
            path = "_config_payloads/" + payload_id + ".config"
            payload_files[path] = model.config_payloads[payload_id].content
            payload_labels[":" + path] = payload_id

        analysis_values = {}
        for environment_id in sorted(analysis_environment_ids.keys()):
            environment = model.compile_environments[environment_id]
            analysis_values[environment.config_payload_id] = environment.config_payload.content
        partitions[reachability_id] = struct(
            analysis_values = analysis_values,
            environments = environments,
            generated_headers = _emitter_generated_header_cover(
                environment_values,
                reachability_id,
            ),
            name = _emitter_compile_environment_index_name(reachability_id),
            payload_labels = payload_labels,
            reachability_id = reachability_id,
        )

    return struct(
        partitions = partitions,
        payload_files = payload_files,
    )

def _emitter_legacy_source_partition(model, reachability_id, source_objects):
    used = {}
    raw_groups = {}
    for obj in source_objects:
        for index in obj.action_source_group.file_indices:
            used[index] = True

    global_indices = sorted(used.keys())
    local_by_global = {}
    for offset in range(len(global_indices)):
        local_by_global[global_indices[offset]] = offset + 1
    encoded_by_target = {}
    for obj in source_objects:
        encoded = ",".join([
            str(local_by_global[index])
            for index in obj.action_source_group.file_indices
        ])
        raw_groups[encoded] = True
        encoded_by_target[obj.target] = encoded
    groups = sorted(raw_groups.keys())
    group_by_encoded = {}
    for offset in range(len(groups)):
        group_by_encoded[groups[offset]] = offset + 1
    return struct(
        global_indices = global_indices,
        group_by_target = {
            target: group_by_encoded[encoded]
            for target, encoded in encoded_by_target.items()
        },
        local_by_global = local_by_global,
        name = _emitter_fallback_source_index_name(reachability_id),
        paths = [model.source_files[index - 1].path for index in global_indices],
        groups = groups,
        reachability_id = reachability_id,
    )

def _emitter_legacy_source_partitions(model, fallback_reasons):
    objects_by_reachability = {}
    for target in sorted(fallback_reasons.keys()):
        obj = model.object_variants[target]
        if obj.recipe.kind not in ["arm64_nvhe", "compile"]:
            continue
        objects_by_reachability.setdefault(obj.reachability_id, []).append(obj)
    return {
        reachability_id: _emitter_legacy_source_partition(
            model,
            reachability_id,
            objects_by_reachability[reachability_id],
        )
        for reachability_id in sorted(objects_by_reachability.keys())
    }

def _emitter_group_targets(model, fallback_reasons):
    by_object = {}
    grouped = {}
    fallback = {}
    for group_id in sorted(model.recipe_groups.keys()):
        group = model.recipe_groups[group_id]
        grouped_targets = [
            target
            for target in group.object_targets
            if target not in fallback_reasons
        ]
        fallback_targets = [
            target
            for target in group.object_targets
            if target in fallback_reasons
        ]
        if grouped_targets:
            name = _emitter_group_name(group, "actions")
            grouped[group_id] = struct(
                name = name,
                objects = [model.object_variants[target] for target in grouped_targets],
                targets = grouped_targets,
            )
            for target in grouped_targets:
                by_object[target] = name
        if fallback_targets:
            name = _emitter_group_name(group, "legacy")
            fallback[group_id] = struct(
                name = name,
                objects = [model.object_variants[target] for target in fallback_targets],
                targets = fallback_targets,
            )
            for target in fallback_targets:
                by_object[target] = name
    return struct(
        by_object = by_object,
        fallback = fallback,
        grouped = grouped,
    )

def _emitter_member_group_labels(objects, own_name, group_targets):
    labels = {}
    for obj in objects:
        for member in obj.member_targets:
            name = group_targets.by_object[member]
            if name != own_name:
                labels[":" + name] = True
    return sorted(labels.keys())

def _emitter_flag_program_attrs(model):
    terminals = {
        terminal_id: json.encode(terminal.argv)
        for terminal_id, terminal in model.flag_terminals.items()
    }
    probes = {}
    for probe_id, probe in model.kbuild_probes.items():
        probes[probe_id] = json.encode({
            "candidate_argv": probe.candidate_argv,
            "context_program": probe.context_program,
            "kind": probe.kind,
            "language": probe.language,
            "srcarch": probe.srcarch,
        })
    nodes = {}
    for node_id, node in model.flag_nodes.items():
        if node.kind == "select":
            value = {
                "kind": node.kind,
                "probe": node.probe,
                "when_false": node.when_false,
                "when_true": node.when_true,
            }
        else:
            value = {
                "children": node.children,
                "kind": node.kind,
            }
        nodes[node_id] = json.encode(value)
    return struct(
        nodes = nodes,
        probes = probes,
        programs = {
            program_id: program.root
            for program_id, program in model.flag_programs.items()
        },
        terminals = terminals,
    )

def _emitter_emit_grouped_rules(
        lines,
        model,
        group_targets,
        compile_environment_partitions,
        arch,
        srcarch,
        source_label_package,
        source_root_label,
        graph_profile,
        flag_programs,
        source_objtool,
        source_objcopy,
        relocatable_link_flags,
        toolchain_remove_flags):
    for group_id in sorted(group_targets.grouped.keys()):
        emitted = group_targets.grouped[group_id]
        group = model.recipe_groups[group_id]
        recipe = group.recipe
        kind = recipe.kind
        common = {
            "reachable_configs": group.reachability.configs,
            "reachability_id": group.reachability_id,
            "recipe_id": group.recipe_id,
            "tags": ["manual"],
        }
        if kind == "compile":
            local_sources = _emitter_local_sources(model, emitted.objects)
            compile_environment_index = compile_environment_partitions[group.reachability_id]
            attrs = dict(common)
            attrs.update({
                "arch": arch,
                "flag_program": recipe.flag_program_id,
                "flag_programs": flag_programs,
                "graph_profile": graph_profile,
                "compile_environment_index": ":" + compile_environment_index.name,
                "flag_effects": recipe.flag_program.effects,
                "language": recipe.language,
                "mode": recipe.mode,
                "modname": recipe.modname,
                "module_root": recipe.module_root,
                "objects": _emitter_group_object_specs(
                    emitted.objects,
                    local_sources,
                    include_action_source = True,
                ),
                "objtool_args": recipe.objtool_args,
                "objtool_disabled": recipe.objtool_disabled,
                "objtool_force": recipe.objtool_force,
                "remove_flag_effects": recipe.remove_flag_program.effects,
                "remove_flag_program": recipe.remove_flag_program_id,
                "source_paths": local_sources.paths,
                "source_root": source_root_label,
                "srcarch": srcarch,
                "srcs": _emitter_source_labels(source_label_package, local_sources.paths),
                "toolchain_remove_flags": toolchain_remove_flags,
            })
            if arch == "x86" and not recipe.objtool_disabled:
                if not source_objtool:
                    fail("compact-v7 grouped x86 recipe %s requires source_objtool" % recipe.id)
                attrs["objtool"] = source_objtool
            _emitter_rule(lines, "linux_object_action_group", emitted.name, attrs)
        elif kind == "composite":
            attrs = dict(common)
            attrs.update({
                "arch": arch,
                "flag_program": recipe.flag_program_id,
                "flag_programs": flag_programs,
                "graph_profile": graph_profile,
                "member_groups": _emitter_member_group_labels(
                    emitted.objects,
                    emitted.name,
                    group_targets,
                ),
                "mode": recipe.mode,
                "module_root": recipe.module_root,
                "objects": _emitter_group_object_specs(
                    emitted.objects,
                    None,
                    include_action_source = False,
                ),
                "objtool_args": recipe.objtool_args,
                "objtool_force": recipe.objtool_force,
                "remove_flag_program": recipe.remove_flag_program_id,
                "relocatable_link_flags": relocatable_link_flags,
            })
            _emitter_rule(lines, "linux_composite_object_action_group", emitted.name, attrs)
        elif kind == "arm64_nvhe":
            if not source_objcopy:
                fail("compact-v7 arm64 nVHE recipe %s requires source_objcopy" % recipe.id)
            local_sources = _emitter_local_sources(model, emitted.objects)
            compile_environment_index = compile_environment_partitions[group.reachability_id]
            attrs = dict(common)
            attrs.update({
                "flag_program": recipe.flag_program_id,
                "flag_programs": flag_programs,
                "graph_profile": graph_profile,
                "compile_environment_index": ":" + compile_environment_index.name,
                "member_groups": _emitter_member_group_labels(
                    emitted.objects,
                    emitted.name,
                    group_targets,
                ),
                "objcopy": source_objcopy,
                "objects": _emitter_group_object_specs(
                    emitted.objects,
                    local_sources,
                    include_action_source = True,
                ),
                "relocatable_link_flags": relocatable_link_flags,
                "remove_flag_program": recipe.remove_flag_program_id,
                "source_paths": local_sources.paths,
                "source_root": source_root_label,
                "srcs": _emitter_source_labels(source_label_package, local_sources.paths),
                "toolchain_remove_flags": toolchain_remove_flags,
            })
            _emitter_rule(lines, "linux_arm64_nvhe_object_action_group", emitted.name, attrs)

def _emitter_emit_legacy_rules(
        lines,
        model,
        group_targets,
        compile_environment_partitions,
        fallback_reasons,
        source_partitions,
        arch,
        srcarch,
        source_objtool,
        source_asn1_compiler,
        source_relacheck):
    legacy_names = {
        target: _emitter_legacy_name(target)
        for target in fallback_reasons
    }
    for target in sorted(fallback_reasons.keys()):
        obj = model.object_variants[target]
        recipe = obj.recipe
        if (
            recipe.flag_program.root in model.flag_nodes or
            recipe.remove_flag_program.root in model.flag_nodes
        ):
            fail(
                "compact-v7 legacy fallback object %s cannot consume a dynamic flag program" %
                target,
            )
        attrs = {
            "arch": arch,
            "content_id": obj.content_id,
            "mode": recipe.mode,
            "object": obj.object,
            "tags": ["manual"],
        }
        if recipe.kind == "compile":
            group = obj.action_source_group
            compile_environment_index = compile_environment_partitions[obj.reachability_id]
            source_index = source_partitions[obj.reachability_id]
            attrs.update({
                "compile_environment_id": obj.compile_environment_id,
                "compile_environment_index": ":" + compile_environment_index.name,
                "deps": [":" + legacy_names[dependency] for dependency in obj.dependency_targets],
                "flags": recipe.flag_program.argv,
                "modname": recipe.modname,
                "module_root": recipe.module_root,
                "objtool_args": recipe.objtool_args,
                "objtool_force": recipe.objtool_force,
                "remove_flags": recipe.remove_flag_program.argv,
                "source_input_file": source_index.local_by_global[group.primary_source_index],
                "source_input_group": source_index.group_by_target[target],
                "source_input_index": ":" + source_index.name,
                "srcarch": srcarch,
            })
            if arch == "x86" and not recipe.objtool_disabled:
                if not source_objtool:
                    fail("compact-v7 legacy x86 object %s requires source_objtool" % target)
                attrs["objtool"] = source_objtool
            if obj.object.endswith(".asn1.o"):
                if not source_asn1_compiler:
                    fail("compact-v7 legacy ASN.1 object %s requires source_asn1_compiler" % target)
                attrs["asn1_compiler"] = source_asn1_compiler
            if arch == "arm64" and obj.object.endswith(".pi.o"):
                if not source_relacheck:
                    fail("compact-v7 legacy arm64 PI object %s requires source_relacheck" % target)
                attrs["relacheck"] = source_relacheck
            _emitter_rule(lines, "linux_object", legacy_names[target], attrs)
        elif recipe.kind == "composite":
            attrs.update({
                "module_root": recipe.module_root,
                "objects": [":" + legacy_names[member] for member in obj.member_targets],
                "objtool_args": recipe.objtool_args,
                "objtool_force": recipe.objtool_force,
            })
            _emitter_rule(lines, "linux_composite_object", legacy_names[target], attrs)
        elif recipe.kind == "arm64_nvhe":
            compile_environment_index = compile_environment_partitions[obj.reachability_id]
            source_index = source_partitions[obj.reachability_id]
            attrs.update({
                "compile_environment_id": obj.compile_environment_id,
                "compile_environment_index": ":" + compile_environment_index.name,
                "objects": [":" + legacy_names[member] for member in obj.member_targets],
                "source_input_group": source_index.group_by_target[target],
                "source_input_index": ":" + source_index.name,
                "srcarch": srcarch,
            })
            _emitter_rule(lines, "linux_arm64_nvhe_object", legacy_names[target], attrs)

    for group_id in sorted(group_targets.fallback.keys()):
        emitted = group_targets.fallback[group_id]
        group = model.recipe_groups[group_id]
        _emitter_rule(
            lines,
            "linux_object_action_group_import",
            emitted.name,
            {
                "object_targets": emitted.targets,
                "objects": [":" + legacy_names[target] for target in emitted.targets],
                "reachable_configs": group.reachability.configs,
                "reachability_id": group.reachability_id,
                "recipe_id": group.recipe_id,
                "tags": ["manual"],
            },
        )

def _emitter_config_source_paths(model, config):
    indices = {}
    for index in config.support_source_set.file_indices:
        indices[index] = True
    family_ids = {}
    core_targets = {}
    for root in config.object_targets:
        for target in model.object_variants[root].closure:
            core_targets[target] = True
    for target in sorted(core_targets.keys()):
        obj = model.object_variants[target]
        if obj.action_source_group != None:
            for index in obj.action_source_group.file_indices:
                indices[index] = True
        if obj.compile_environment != None:
            for family_id in obj.compile_environment.generated_header_family_ids:
                family_ids[family_id] = True
    for _ in range(len(model.generated_header_families)):
        changed = False
        for family_id in sorted(list(family_ids.keys())):
            family = model.generated_header_families[family_id]
            if family.source_set != None:
                for index in family.source_set.file_indices:
                    indices[index] = True
            for dependency in family.dependencies:
                if dependency not in family_ids:
                    family_ids[dependency] = True
                    changed = True
        if not changed:
            break
    return [
        model.source_files[index - 1].path
        for index in sorted(indices.keys())
    ]

def _emitter_emit_config_targets(
        lines,
        model,
        group_targets,
        source_label_package,
        graph_profile):
    result = {}
    config_names = sorted(model.configs.keys())
    for index in range(len(config_names)):
        config_name = config_names[index]
        config = model.configs[config_name]
        prefix = "_config_" + str(index)
        image_projection = prefix + "_image_objects"
        module_projection = prefix + "_module_objects"
        image = prefix + "_image"
        modules = prefix + "_modules"
        sources = prefix + "_sources"
        if not config.object_targets:
            fail("compact-v7 config %r has no built-in image roots" % config_name)
        image_groups = sorted({
            ":" + group_targets.by_object[target]: True
            for target in config.object_targets
        }.keys())
        module_groups = sorted({
            ":" + group_targets.by_object[target]: True
            for target in config.module_object_targets
        }.keys())
        _emitter_rule(
            lines,
            "linux_image_object_projection",
            image_projection,
            {
                "config": config_name,
                "groups": image_groups,
                "object_targets": config.object_targets,
                "tags": ["manual"],
            },
        )
        _emitter_rule(
            lines,
            "linux_module_object_projection",
            module_projection,
            {
                "config": config_name,
                "groups": module_groups,
                "object_targets": config.module_object_targets,
                "tags": ["manual"],
            },
        )
        _emitter_rule(
            lines,
            "linux_grouped_image",
            image,
            {
                "graph_profile": graph_profile,
                "objects": ":" + image_projection,
                "tags": ["manual"],
            },
        )
        _emitter_rule(
            lines,
            "linux_grouped_modules",
            modules,
            {
                "objects": ":" + module_projection,
                "tags": ["manual"],
            },
        )
        source_paths = _emitter_config_source_paths(model, config)
        _emitter_rule(
            lines,
            "filegroup",
            sources,
            {
                "srcs": _emitter_source_labels(
                    source_label_package,
                    source_paths,
                ),
                "tags": ["manual"],
            },
        )
        result[config_name] = struct(
            image = image,
            image_label = ":" + image,
            modules = modules,
            modules_label = ":" + modules,
            sources = sources,
            sources_label = ":" + sources,
            source_paths = source_paths,
        )
    return result

def compact_v7_repository_build(
        model,
        arch,
        srcarch,
        rules_repo,
        source_label_package,
        source_root_label,
        graph_profile,
        version,
        source_objtool = "",
        source_asn1_compiler = "",
        source_relacheck = "",
        source_objcopy = "",
        relocatable_link_flags = None,
        toolchain_remove_flags = None,
        visibility = None):
    """Emits a deterministic lazy compact-v7 graph package.

    The returned config-payload files are intentionally separate from BUILD text
    so a repository rule can materialize them without embedding .config contents
    in the analysis graph.
    """
    if arch not in ["arm64", "x86"]:
        fail("compact-v7 emitter has unsupported Linux arch %r" % arch)
    if srcarch != arch:
        fail(
            "compact-v7 grouped actions require srcarch %r to match arch %r" %
            (srcarch, arch),
        )
    for value, name in [
        (source_label_package, "source_label_package"),
        (source_root_label, "source_root_label"),
        (graph_profile, "graph_profile"),
        (version, "version"),
    ]:
        if not value:
            fail("compact-v7 emitter requires %s" % name)
    if relocatable_link_flags == None:
        relocatable_link_flags = ["-r"]
    if toolchain_remove_flags == None:
        toolchain_remove_flags = []
    if visibility == None:
        visibility = ["//:__subpackages__"]

    fallback_reasons = _emitter_classify_fallbacks(model, arch)
    group_targets = _emitter_group_targets(model, fallback_reasons)
    compile_environment_index = _emitter_compile_environment_partitions(
        model,
        fallback_reasons,
    )
    compile_environment_partitions = compile_environment_index.partitions
    fallback_source_partitions = _emitter_legacy_source_partitions(
        model,
        fallback_reasons,
    )

    lines = [
        "# Generated from compact-v7 lazy action-graph metadata. Do not edit.",
        "",
        "load(%r, %r, %r, %r, %r, %r, %r, %r, %r)" % (
            _emitter_rules_label(rules_repo, "linux_object_groups.bzl"),
            "linux_arm64_nvhe_object_action_group",
            "linux_composite_object_action_group",
            "linux_grouped_image",
            "linux_grouped_modules",
            "linux_image_object_projection",
            "linux_module_object_projection",
            "linux_object_action_group",
            "linux_object_action_group_import",
        ),
        "load(%r, %r, %r, %r, %r, %r, %r)" % (
            _emitter_rules_label(rules_repo, "linux_objects.bzl"),
            "linux_arm64_nvhe_object",
            "linux_compile_environment_index",
            "linux_composite_object",
            "linux_object",
            "linux_source_input_index",
            "linux_source_tree",
        ),
        "load(%r, %r)" % (
            _emitter_rules_label(rules_repo, "flag_programs.bzl"),
            "linux_flag_programs",
        ),
        "",
        "package(default_visibility = %r)" % visibility,
        "",
        'exports_files(["graph_profile_projection.json", "metadata.json"], visibility = ["//visibility:public"])',
        "",
    ]

    flag_program_attrs = _emitter_flag_program_attrs(model)
    _emitter_rule(
        lines,
        "linux_flag_programs",
        "_flag_programs",
        {
            "nodes": flag_program_attrs.nodes,
            "probes": flag_program_attrs.probes,
            "graph_profile": graph_profile,
            "programs": flag_program_attrs.programs,
            "source_root": source_root_label,
            "source_paths": [source.path for source in model.source_files],
            "srcs": _emitter_source_labels(
                source_label_package,
                [source.path for source in model.source_files],
            ),
            "tags": ["manual"],
            "terminals": flag_program_attrs.terminals,
        },
    )

    for reachability_id in sorted(compile_environment_partitions.keys()):
        partition = compile_environment_partitions[reachability_id]
        _emitter_rule(
            lines,
            "linux_compile_environment_index",
            partition.name,
            {
                "arch": arch,
                "compile_environments": partition.environments,
                "config_payload_files": partition.payload_labels,
                "config_payload_values": partition.analysis_values,
                "expected_abi": model.compile_environment_abi,
                "generated_headers": partition.generated_headers,
                "tags": ["manual"],
                "version": version,
            },
        )
    if fallback_source_partitions:
        _emitter_rule(
            lines,
            "linux_source_tree",
            "_fallback_source_tree",
            {
                "root": source_root_label,
                "tags": ["manual"],
            },
        )
        for reachability_id in sorted(fallback_source_partitions.keys()):
            partition = fallback_source_partitions[reachability_id]
            _emitter_rule(
                lines,
                "linux_source_input_index",
                partition.name,
                {
                    "groups": partition.groups,
                    "source_tree_info": ":_fallback_source_tree",
                    "srcs": _emitter_source_labels(
                        source_label_package,
                        partition.paths,
                    ),
                    "tags": ["manual"],
                },
            )

    _emitter_emit_grouped_rules(
        lines,
        model,
        group_targets,
        compile_environment_partitions,
        arch,
        srcarch,
        source_label_package,
        source_root_label,
        graph_profile,
        ":_flag_programs",
        source_objtool,
        source_objcopy,
        relocatable_link_flags,
        toolchain_remove_flags,
    )
    if fallback_reasons:
        _emitter_emit_legacy_rules(
            lines,
            model,
            group_targets,
            compile_environment_partitions,
            fallback_reasons,
            fallback_source_partitions,
            arch,
            srcarch,
            source_objtool,
            source_asn1_compiler,
            source_relacheck,
        )
    config_targets = _emitter_emit_config_targets(
        lines,
        model,
        group_targets,
        source_label_package,
        graph_profile,
    )
    analysis_config_payload_ids = {}
    for partition in compile_environment_partitions.values():
        for payload_id in partition.analysis_values:
            analysis_config_payload_ids[payload_id] = True
    return struct(
        analysis_config_payload_ids = sorted(analysis_config_payload_ids.keys()),
        build_file = "\n".join(lines),
        compile_environment_index_by_reachability = {
            reachability_id: ":" + compile_environment_partitions[reachability_id].name
            for reachability_id in sorted(compile_environment_partitions.keys())
        },
        config_payload_files = compile_environment_index.payload_files,
        config_targets = config_targets,
        fallback_reasons = fallback_reasons,
        fallback_source_index_by_reachability = {
            reachability_id: ":" + fallback_source_partitions[reachability_id].name
            for reachability_id in sorted(fallback_source_partitions.keys())
        },
        fallback_targets = sorted(fallback_reasons.keys()),
        group_target_by_object = group_targets.by_object,
    )
