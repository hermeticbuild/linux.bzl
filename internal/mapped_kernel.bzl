"""Execution-time Linux Kconfig/Kbuild graph expansion for Bazel 9."""

load("@rules_bison//bison:toolchain_type.bzl", "BISON_TOOLCHAIN_TYPE", "bison_toolchain")
load("@rules_cc//cc:action_names.bzl", "ACTION_NAMES")
load("@rules_cc//cc:find_cc_toolchain.bzl", "CC_TOOLCHAIN_TYPE", "find_cpp_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load("@rules_cc//cc/common:cc_info.bzl", "CcInfo")
load("@rules_flex//flex:toolchain_type.bzl", "FLEX_TOOLCHAIN_TYPE", "flex_toolchain")
load("@rules_m4//m4:toolchain_type.bzl", "M4_TOOLCHAIN_TYPE", "m4_toolchain")
load(":execution_platform.bzl", "linux_execution_platform_attr", "linux_execution_platform_label")
load(":host_cc_toolchain.bzl", "host_cc_toolchain", "host_cc_toolchain_attr")
load(":module_make_vars.bzl", "validate_linux_module_make_vars")
load(":platform_transition_gateway.bzl", "linux_platform_transition")
load(
    ":probe_map_directory.bzl",
    "expand_linux_probe_plan",
    "linux_probe_map_directory_params",
    "linux_probe_map_directory_tools",
)
load(":providers.bzl", "LinuxKernelInfo", "LinuxModuleSdkInfo", "LinuxModuleTreeInfo")
load(
    ":rust_toolchain.bzl",
    "execution_bindgen_toolchain",
    "execution_rust_source_toolchain",
    "execution_rust_toolchain",
    "optional_bindgen_toolchain_type",
    "optional_rust_analyzer_toolchain_type",
    "optional_rust_toolchain_type",
    "target_rust_toolchain",
)
load(":script_runtime_toolchain.bzl", "SCRIPT_RUNTIME_TOOLCHAIN_TYPE", "script_runtime_toolchain")
load(
    ":toolchain_action_paths.bzl",
    _EXECUTION_ROOT_MARKER = "EXECUTION_ROOT_MARKER",
    _add_rendered_toolchain_action_value = "add_rendered_toolchain_action_value",
    _canonicalize_toolchain_action_value = "canonicalize_toolchain_action_value",
    _render_toolchain_action_value = "render_toolchain_action_value",
    _toolchain_action_path_index = "toolchain_action_path_index",
    _toolchain_action_path_index_from_list = "toolchain_action_path_index_from_list",
)

visibility("public")

_SCHEMA = "linux-kernel-plan-v4"
_TOOLSET_SCHEMA = "linux-kbuild-toolset-v5"
_KBUILD_ARGS_SENTINEL = "__LINUX_BZL_KBUILD_ARGS_V1__"
_SCRIPT_RUNTIME_APPLET_ROLE_PREFIX = "script-applet-"
_COMPANION_TOOL_PREFIX = "companion_tool_"
_TOOL_BINDING_SEPARATOR = "@"
_TOOLCHAIN_FILES_PREFIX = "toolchain_files@"
_HOST_DEPS_SENTINEL = "__LINUX_BZL_HOST_DEPS__"
_HOST_DEPS_TREE = "host_deps"
_PYTHON_EXEC_TOOLS_TOOLCHAIN_TYPE = str(Label("@rules_python//python:exec_tools_toolchain_type"))
_PERL_TOOLCHAIN_TYPE = str(Label("@rules_perl//perl:toolchain_type"))
_HEX = "0123456789abcdef"
_BASE36 = "0123456789abcdefghijklmnopqrstuvwxyz"
_BASE36_VALUES = {character: index for index, character in enumerate(_BASE36.elems())}
_NAME = "abcdefghijklmnopqrstuvwxyz0123456789_.+-"
_MAX_MARKER_COMPONENT = 240
_MAX_PACKED_INPUT_FILENAME = 240
_MAX_PACKED_INPUT_TUPLES = 64
_MAX_PLAN_ORDINAL = 99999999
_STAGES = ["prehost", "bootstrap", "host", "prep", "target"]
_TREES = ["prehost", "bootstrap", "prep", "host", "objects", "sdk", "vmlinux", "image", "modules", "metadata"]
_STAGE_OUTPUT_TREES = {
    "prehost": {"prehost": True},
    "bootstrap": {"bootstrap": True},
    "host": {"host": True},
    "prep": {"prep": True},
    "target": {name: True for name in ["objects", "sdk", "vmlinux", "image", "modules", "metadata"]},
}
_TOOLSET_DIRECTORIES = {
    "host": "host_toolset_identity",
    "target": "target_toolset_identity",
}
_KBUILD_ACTION_ROLES = [
    "ar",
    "as",
    "cc",
    "cxx",
    "ld",
    "nm",
    "objcopy",
    "objdump",
    "ranlib",
    "readelf",
    "strip",
]
_KERNEL_FIELDS = [
    "config",
    "image",
    "kernel_release",
    "system_map",
    "vmlinux",
]
_MODULE_FIELDS = [
    "module_symvers",
    "modules",
    "modules_builtin",
    "modules_builtin_modinfo",
    "modules_order",
]
_SHARED_PLANNER_HELPER_ATTRS = {
    "actionfile": "_actionfile",
    "scriptrun": "_scriptrun",
}

# The generic planner uses only recipe-opaque helpers shared by source-derived
# Kbuild actions. Module lifecycle steps are native Kbuild targets, not custom
# target-only wrappers.
_TARGET_PLANNER_HELPER_ATTRS = dict(_SHARED_PLANNER_HELPER_ATTRS)

def _is_decimal(value, width = None):
    if not value or (width != None and len(value) != width):
        return False
    return all([character in "0123456789" for character in value.elems()])

def _is_sha256(value):
    return len(value) == 64 and all([character in _HEX for character in value.elems()])

def _ordinal(value):
    decimal = str(value)
    if len(decimal) > 8:
        fail("mapped Linux ordinal %d exceeds eight digits" % value)
    return "00000000"[:8 - len(decimal)] + decimal

def _valid_name(value):
    return value and len(value) <= _MAX_MARKER_COMPONENT and value[0] in "abcdefghijklmnopqrstuvwxyz0123456789" and all([
        character in _NAME
        for character in value.elems()
    ])

def _scoped_tool_binding(scope, role):
    if scope not in ["host", "target"] or not _valid_name(role):
        fail("invalid configured tool binding %r/%r" % (scope, role))
    return scope + _TOOL_BINDING_SEPARATOR + role

def _split_tool_binding(binding, default_scope = None):
    parts = binding.split(_TOOL_BINDING_SEPARATOR)
    if len(parts) == 1:
        if default_scope not in ["host", "target"] or not _valid_name(binding):
            fail("invalid unscoped configured tool binding %r" % binding)
        return struct(binding = binding, role = binding, scope = default_scope, scoped = False)
    if len(parts) != 2 or parts[0] not in ["host", "target"] or not _valid_name(parts[1]):
        fail("invalid scoped configured tool binding %r" % binding)
    return struct(binding = binding, role = parts[1], scope = parts[0], scoped = True)

def _valid_scoped_tool_binding(scope, role):
    return scope in ["host", "target"] and _valid_name(role)

def _validate_path(value, what):
    if not value or value.startswith("/") or value.endswith("/") or "\\" in value:
        fail("%s has invalid relative path %r" % (what, value))
    if any([part in ["", ".", ".."] for part in value.split("/")]):
        fail("%s has invalid relative path %r" % (what, value))

def _add_artifact_path(args, flag, artifact, format = None):
    """Adds one File/TreeArtifact without expanding it or losing path mapping."""
    args.add(flag)
    if format == None:
        args.add_all([artifact], expand_directories = False)
    else:
        args.add_all([artifact], expand_directories = False, format_each = format)

def _canonical_artifact_path(value, what):
    """Maps Bazel output/sibling-repository paths into one stable namespace."""
    if value.startswith("../"):
        value = "external/" + value[3:]
    if value.startswith("bazel-out/"):
        parts = value.split("/")
        if len(parts) < 4 or parts[2] not in ["bin", "genfiles"]:
            fail("%s has unrecognized Bazel output path %r" % (what, value))
        value = "/".join(parts[3:])
    _validate_path(value, what)
    return value

def _canonical_file_path_values(path, short_path, what):
    canonical_path = _canonical_artifact_path(path, what + " path")
    canonical_short_path = _canonical_artifact_path(short_path, what + " short_path")
    if canonical_path != canonical_short_path:
        fail("%s path %r and short_path %r map to different canonical paths %r and %r" % (
            what,
            path,
            short_path,
            canonical_path,
            canonical_short_path,
        ))
    return canonical_path

def _canonical_file_path(file, what):
    return _canonical_file_path_values(file.path, file.short_path, what)

def _record_canonical_file(files, file, what):
    canonical = _canonical_file_path(file, what)
    existing = files.get(canonical)
    if existing != None and existing != file and existing.path != file.path:
        fail("%s %s (%s) collides with distinct artifact %s (%s) at canonical path %r" % (
            what,
            file,
            file.path,
            existing,
            existing.path,
            canonical,
        ))
    files[canonical] = file
    return canonical

# Test seam for Bazel's sibling-repository path spelling.
def linux_test_canonical_file_path(path, short_path):
    return _canonical_file_path_values(path, short_path, "test artifact")

def _source_input_namespace_names(directory_name, additional_params):
    namespaces = {directory_name: True}
    for tree in _TREES:
        if additional_params.get("input_tree_alias_" + tree) == directory_name:
            namespaces[tree] = True
    return sorted(namespaces)

def linux_test_source_input_namespace_names(directory_name, additional_params):
    return _source_input_namespace_names(directory_name, additional_params)

def _node_source_closure_keys(node, sources):
    """Returns source depsets needed for language-level recursive loading."""
    closures = {}
    for source_id in node["sources"].values():
        source = sources.get(source_id)
        if source != None and source.namespace == "rust":
            # rustc loads `mod` declarations relative to the crate root without
            # naming those files in argv.  The plan still selects the exact
            # Rust source repository through its namespace; carry that selected
            # closure without coupling action expansion to a compiler role.
            closures["rust_source_files"] = True
    return sorted(closures)

def linux_test_node_source_closure_keys(namespaces):
    sources = {}
    node = {"sources": {}}
    for index, namespace in enumerate(namespaces):
        source_id = "source-%d" % index
        sources[source_id] = struct(namespace = namespace)
        node["sources"]["input:%d" % index] = source_id
    return _node_source_closure_keys(node, sources)

def _parse_plan_node_index(plan):
    """Parses the global lexical node index without touching packed edges."""
    by_ordinal = {}
    ordinals = {}
    for child in plan.children:
        path = child.tree_relative_path
        if not path.startswith("index/"):
            if path == "index":
                fail("mapped Linux plan has invalid node index marker %r" % path)
            continue
        parts = path.split("/")
        if len(parts) != 3 or not _is_decimal(parts[1], 8) or not _is_sha256(parts[2]):
            fail("mapped Linux plan has invalid node index marker %r" % path)
        ordinal = int(parts[1])
        node_id = parts[2]
        if ordinal in by_ordinal or node_id in ordinals:
            fail("mapped Linux plan repeats node index ordinal or ID in %r" % path)
        by_ordinal[ordinal] = node_id
        ordinals[node_id] = ordinal

    node_ids = []
    for ordinal in range(len(by_ordinal)):
        node_id = by_ordinal.get(ordinal)
        if node_id == None:
            fail("mapped Linux plan node index is not contiguous at ordinal %s" % _ordinal(ordinal))
        node_ids.append(node_id)
    if node_ids != sorted(node_ids):
        fail("mapped Linux plan node index IDs are not in lexical order")
    return struct(
        node_ids = node_ids,
        ordinals = ordinals,
    )

def _parse_base36_ordinal(value, what):
    if not value or (len(value) > 1 and value.startswith("0")):
        fail("%s has non-canonical base36 ordinal %r" % (what, value))
    result = 0
    for character in value.elems():
        if character not in _BASE36:
            fail("%s has non-canonical base36 ordinal %r" % (what, value))
        result = result * 36 + _BASE36_VALUES[character]
        if result > _MAX_PLAN_ORDINAL:
            fail("%s base36 ordinal %r exceeds %d" % (what, value, _MAX_PLAN_ORDINAL))
    return result

def _decode_packed_node_inputs(node_id, node, node_index):
    """Decodes one node's bounded marker packs and releases them after use."""
    edges = {}
    for role, filenames in node["input_packs"].items():
        chunks = {}
        for filename in filenames:
            if len(filename) > _MAX_PACKED_INPUT_FILENAME or len(filename) < 11 or filename[8] != "." or not _is_decimal(filename[:8], 8):
                fail("mapped Linux node %s has invalid packed input filename %r" % (node_id, filename))
            chunk = int(filename[:8])
            if chunk in chunks:
                fail("mapped Linux node %s repeats packed input chunk %s/%s" % (node_id, role, _ordinal(chunk)))
            payload = filename[9:]
            tuples = payload.split(",")
            if not payload or len(tuples) > _MAX_PACKED_INPUT_TUPLES:
                fail("mapped Linux node %s has invalid packed input tuple count in %r" % (node_id, filename))
            chunks[chunk] = tuples
        previous_role_ordinal = None
        for chunk in range(len(chunks)):
            tuples = chunks.get(chunk)
            if tuples == None:
                fail("mapped Linux node %s has non-contiguous packed input chunks for role %s" % (node_id, role))
            for value in tuples:
                fields = value.split(".")
                if len(fields) != 3:
                    fail("mapped Linux node %s has invalid packed input tuple %r" % (node_id, value))
                input_ordinal = _parse_base36_ordinal(fields[0], "mapped Linux node %s input" % node_id)
                producer_ordinal = _parse_base36_ordinal(fields[1], "mapped Linux node %s producer" % node_id)
                slot = _parse_base36_ordinal(fields[2], "mapped Linux node %s slot" % node_id)
                if producer_ordinal >= len(node_index.node_ids):
                    fail("mapped Linux node %s references unknown producer ordinal %s" % (node_id, fields[1]))
                if previous_role_ordinal != None and input_ordinal <= previous_role_ordinal:
                    fail("mapped Linux node %s has non-canonical packed input order for role %s" % (node_id, role))
                previous_role_ordinal = input_ordinal
                if input_ordinal in edges:
                    fail("mapped Linux node %s repeats input ordinal %s" % (node_id, _ordinal(input_ordinal)))
                edges[input_ordinal] = struct(
                    key = role + ":" + _ordinal(input_ordinal),
                    producer = node_index.node_ids[producer_ordinal],
                    slot = _ordinal(slot),
                )

    inputs = {}
    for input_ordinal in range(len(edges)):
        edge = edges.get(input_ordinal)
        if edge == None:
            fail("mapped Linux node %s input ordinals are not contiguous at %s" % (node_id, _ordinal(input_ordinal)))
        inputs[edge.key] = struct(producer = edge.producer, slot = edge.slot)
    return inputs

def _parse_plan(plan, input_directories, additional_inputs, source_prefix, stage, additional_params = {}):
    node_index = _parse_plan_node_index(plan)
    has_schema = False
    toolsets = {}
    recipe_markers = {}
    source_markers = {}
    nodes = {}
    declared_outputs = {}
    for child in plan.children:
        path = child.tree_relative_path
        parts = path.split("/")
        if parts == ["schema", _SCHEMA]:
            has_schema = True
            continue
        if parts and parts[0] == "index":
            # The complete index was validated independently before any node
            # packs can refer to its compact ordinals.
            continue
        if len(parts) == 3 and parts[0] == "toolsets" and parts[1] in ["host", "target"]:
            if not parts[2].startswith("sha256-") or not _is_sha256(parts[2][len("sha256-"):]):
                fail("mapped Linux plan has invalid toolset marker %r" % path)
            if parts[1] in toolsets:
                fail("mapped Linux plan repeats %s toolset" % parts[1])
            toolsets[parts[1]] = struct(file = child, identity = parts[2])
            continue
        if len(parts) == 2 and parts[0] == "recipes" and parts[1].endswith(".json"):
            recipe = parts[1][:-len(".json")]
            if not _is_sha256(recipe) or recipe in recipe_markers:
                fail("mapped Linux plan has invalid recipe marker %r" % path)
            recipe_markers[recipe] = child
            continue
        if len(parts) >= 4 and parts[0] == "sources":
            source_id = parts[1]
            namespace = parts[2]
            canonical = "/".join(parts[3:])
            if not (source_id == "src-00000000" or (source_id.startswith("src-") and _is_decimal(source_id[4:], 8) and source_id != "src-00000000")):
                fail("mapped Linux plan has invalid source ID %r" % source_id)
            if not _valid_name(namespace):
                fail("mapped Linux plan references invalid source namespace %r" % namespace)
            _validate_path(canonical, "mapped Linux source")
            if source_id in source_markers:
                fail("mapped Linux plan has duplicate or unstaged source %s/%s" % (source_id, canonical))
            source_markers[source_id] = struct(namespace = namespace, path = canonical)
            continue
        if len(parts) >= 4 and parts[0] == "products":
            # Product roots are validated by the planner and projection rules;
            # they do not create actions themselves.
            continue
        if len(parts) < 5 or parts[0] != "nodes":
            fail("mapped Linux plan contains unknown marker %r" % path)
        node_stage = parts[1]
        node_id = parts[2]
        if node_stage not in _STAGES or not _is_sha256(node_id):
            fail("mapped Linux plan has invalid node marker %r" % path)
        if node_id not in node_index.ordinals:
            fail("mapped Linux plan node %s is absent from its global index" % node_id)
        field = parts[3]
        if field == "out":
            if len(parts) < 7:
                fail("mapped Linux node %s has invalid output marker %r" % (node_id, path))
            tree, slot, artifact_path = parts[4], parts[5], "/".join(parts[6:])
            if tree not in _TREES or not _is_decimal(slot, 8):
                fail("mapped Linux node %s has invalid output marker %r" % (node_id, path))
            if tree not in _STAGE_OUTPUT_TREES[node_stage]:
                fail("mapped Linux %s node %s cannot write %s tree" % (node_stage, node_id, tree))
            _validate_path(artifact_path, "mapped Linux node physical output")
            output_key = node_id + ":" + slot
            if output_key in declared_outputs:
                fail("mapped Linux plan repeats output slot %s" % output_key)

            # A stage shard contains all current outputs plus only the exact
            # cross-stage descriptors named by packed current-node edges.
            declared_outputs[output_key] = struct(artifact_path = artifact_path, tree = tree)
        if node_stage != stage:
            continue
        node = nodes.setdefault(node_id, {
            "input_packs": {},
            "outputs": {},
            "sources": {},
            "tools": {},
            "trees": {},
        })
        if field in ["kind", "product", "recipe", "tool"] and len(parts) == 5:
            if field in node or not _valid_name(parts[4]):
                fail("mapped Linux node %s has invalid or repeated %s" % (node_id, field))
            node[field] = parts[4]
        elif field == "in" and len(parts) == 8 and parts[4] == "source":
            role, ordinal, source_id = parts[5], parts[6], parts[7]
            key = role + ":" + ordinal
            if not _valid_name(role) or not _is_decimal(ordinal, 8) or key in node["sources"]:
                fail("mapped Linux node %s has invalid source input %r" % (node_id, path))
            node["sources"][key] = source_id
        elif field == "in" and len(parts) == 7 and parts[4] == "node-pack":
            role, filename = parts[5], parts[6]
            if not _valid_name(role):
                fail("mapped Linux node %s has invalid packed input role %r" % (node_id, role))
            node["input_packs"].setdefault(role, []).append(filename)
        elif field == "in" and len(parts) == 6 and parts[4] == "bindings":
            filename = parts[5]
            digest = filename[:-len(".json")] if filename.endswith(".json") else ""
            if not _is_sha256(digest):
                fail("mapped Linux node %s has invalid input binding manifest %r" % (node_id, path))
            if "input_bindings" in node:
                fail("mapped Linux node %s repeats input binding manifest" % node_id)
            node["input_bindings"] = struct(file = child, id = digest)
        elif field == "in" and len(parts) == 6 and parts[4] == "tree":
            tree = parts[5]
            if not _valid_name(tree) or tree in node["trees"]:
                fail("mapped Linux node %s has invalid or repeated tree input %r" % (node_id, path))
            node["trees"][tree] = True
        elif field == "in" and len(parts) == 8 and parts[4] == "tool":
            scope, role, binding_form = parts[5], parts[6], parts[7]
            if not _valid_scoped_tool_binding(scope, role) or binding_form not in ["scoped", "unscoped"]:
                fail("mapped Linux node %s has invalid or repeated auxiliary tool %r" % (node_id, path))
            default_scope = "host" if node_stage in ["prehost", "host"] else "target"
            if binding_form == "unscoped" and scope != default_scope:
                fail("mapped Linux node %s has cross-scope unscoped auxiliary tool %r" % (node_id, path))
            binding = role if binding_form == "unscoped" else _scoped_tool_binding(scope, role)
            if binding in node["tools"]:
                fail("mapped Linux node %s repeats auxiliary tool %r" % (node_id, binding))
            node["tools"][binding] = struct(binding = binding, role = role, scope = scope)
        elif field == "out" and len(parts) >= 7:
            slot = parts[5]
            if slot in node["outputs"]:
                fail("mapped Linux node %s has invalid output marker %r" % (node_id, path))
            node["outputs"][slot] = declared_outputs[node_id + ":" + slot]
        else:
            fail("mapped Linux node %s has invalid marker %r" % (node_id, path))
    if not has_schema:
        fail("mapped Linux plan does not use schema %s" % _SCHEMA)
    for node_id, node in nodes.items():
        if "input_bindings" not in node:
            fail("mapped Linux node %s has no input binding manifest" % node_id)

    referenced_recipes = {}
    referenced_sources = {}
    for node in nodes.values():
        recipe = node.get("recipe")
        if recipe != None:
            referenced_recipes[recipe] = True
        for source_id in node["sources"].values():
            referenced_sources[source_id] = True
    recipes = {
        recipe: file
        for recipe, file in recipe_markers.items()
        if recipe in referenced_recipes
    }
    source_markers = {
        source_id: marker
        for source_id, marker in source_markers.items()
        if source_id in referenced_sources
    }

    # Resolve only source markers referenced by this stage. The physical source
    # inputs may be much larger than a single selected Kbuild stage, and the
    # retained source structs already own every File needed by its actions.
    selected_source_keys = {}
    selected_source_namespaces = {}
    for marker in source_markers.values():
        selected_source_keys[marker.namespace + "/" + marker.path] = True
        selected_source_namespaces[marker.namespace] = True
    source_children = {}
    if "kernel" in selected_source_namespaces:
        for file in additional_inputs["source_files"].to_list():
            canonical = file.short_path
            if source_prefix:
                prefix = source_prefix + "/"
                if not canonical.startswith(prefix):
                    fail("mapped Linux source input %s is outside %s" % (file, source_prefix))
                canonical = canonical[len(prefix):]
            key = "kernel/" + canonical
            if key not in selected_source_keys:
                continue
            existing = source_children.get(key)
            if existing != None and existing != file:
                fail("mapped Linux source path %s is provided by distinct artifacts %s and %s" % (
                    canonical,
                    existing,
                    file,
                ))
            source_children[key] = file

    # Rust sources are not part of the Linux repository. They come from the
    # exact source-bearing toolchain selected by the Rust contract.
    if "rust" in selected_source_namespaces:
        for file in additional_inputs.get("rust_source_files", depset()).to_list():
            canonical = _canonical_file_path(file, "selected Rust source")
            key = "rust/" + canonical
            if key not in selected_source_keys:
                continue
            if key in source_children and source_children[key] != file:
                fail("selected Rust source path %s is provided by distinct artifacts %s and %s" % (
                    canonical,
                    source_children[key],
                    file,
                ))
            source_children[key] = file
    for key, path in {
        "auto_conf": "config/auto.conf",
        "auto_conf_cmd": "config/auto.conf.cmd",
        "autoconf": "config/autoconf.h",
        "kernel_release": "config/kernel.release",
        "resolved_config": "config/.config",
        "rustc_cfg": "config/rustc_cfg",
    }.items():
        file = additional_inputs.get(key)
        if file != None and path in selected_source_keys:
            source_children[path] = file
    for directory_name, directory in input_directories.items():
        if directory_name == "plan" or directory_name in _TOOLSET_DIRECTORIES.values():
            continue
        for namespace in _source_input_namespace_names(directory_name, additional_params):
            if not _valid_name(namespace):
                fail("mapped Linux source namespace %r is invalid" % namespace)
            if namespace not in selected_source_namespaces:
                continue
            for file in directory.children:
                key = namespace + "/" + file.tree_relative_path
                if key not in selected_source_keys:
                    continue
                if key in source_children:
                    fail("mapped Linux source input %s is staged more than once" % key)
                source_children[key] = file

    sources = {}
    for source_id, marker in source_markers.items():
        source_key = marker.namespace + "/" + marker.path
        file = source_children.get(source_key)
        if file == None:
            fail("mapped Linux plan has duplicate or unstaged source %s/%s" % (source_id, marker.path))
        sources[source_id] = struct(file = file, namespace = marker.namespace, path = marker.path)
    return struct(
        nodes = nodes,
        recipes = recipes,
        sources = sources,
        toolsets = toolsets,
        declared_outputs = declared_outputs,
        node_index = node_index,
    )

def _expected_toolset_identity(directory, scope):
    children = directory.children
    if len(children) != 1:
        fail("mapped Linux %s toolset identity must contain exactly one marker, found %d" % (scope, len(children)))
    identity = children[0].tree_relative_path
    if "/" in identity or not identity.startswith("sha256-") or not _is_sha256(identity[len("sha256-"):]):
        fail("mapped Linux %s toolset identity has invalid marker %r" % (scope, identity))
    return struct(file = children[0], identity = identity)

def _validate_plan_toolsets(parsed, input_directories):
    validated = {}
    for scope, directory_name in _TOOLSET_DIRECTORIES.items():
        planned = parsed.toolsets.get(scope)
        if planned == None:
            fail("mapped Linux plan has no %s toolset marker" % scope)
        directory = input_directories.get(directory_name)
        if directory == None:
            fail("mapped Linux callback has no %s toolset identity directory" % scope)
        expected = _expected_toolset_identity(directory, scope)
        if planned.identity != expected.identity:
            fail("mapped Linux plan selected %s toolset %s, but the configured toolchain identity is %s" % (
                scope,
                planned.identity,
                expected.identity,
            ))
        validated[scope] = struct(plan_file = planned.file, expected_file = expected.file)
    return validated

def _resolve_node_input_bindings(node_id, dependencies, outputs, prior, declared_outputs):
    """Resolves exact role/ordinal bindings without interpreting recipes."""
    bindings = {}
    for key, dependency in dependencies.items():
        artifact = outputs.get(dependency.producer + ":" + dependency.slot)
        if artifact == None:
            # Cross-stage node IDs remain globally stable. Resolve the exact
            # output from the plan-wide index and then its prior-stage file.
            producer = declared_outputs.get(dependency.producer + ":" + dependency.slot)
            if producer == None:
                fail("mapped Linux node %s references unknown producer %s" % (node_id, dependency.producer))
            artifact = prior.get(producer.tree + ":" + producer.artifact_path)
        if artifact == None:
            return None
        bindings[key] = artifact
    return bindings

def _node_input_artifact_tree_roots(node_id, dependencies, outputs, input_directories, output_directories, declared_outputs):
    """Resolves one typed TreeArtifact root for every producer output tree."""
    roots = {}
    for dependency in dependencies.values():
        if not _is_sha256(dependency.producer) or not _is_decimal(dependency.slot, 8):
            fail("mapped Linux node %s has invalid producer binding %r:%r" % (
                node_id,
                dependency.producer,
                dependency.slot,
            ))
        output_key = dependency.producer + ":" + dependency.slot
        descriptor = declared_outputs.get(output_key)
        if descriptor == None:
            fail("mapped Linux node %s references unknown producer output %s" % (node_id, output_key))
        if descriptor.tree not in _TREES:
            fail("mapped Linux node %s references invalid producer tree %r" % (node_id, descriptor.tree))

        if output_key in outputs:
            root = output_directories.get(descriptor.tree)
            origin = "current-stage output"
        else:
            directory = input_directories.get(descriptor.tree)
            root = getattr(directory, "directory", None) if directory != None else None
            origin = "prior-stage input"
        if root == None:
            fail("mapped Linux node %s requires unavailable %s artifact tree %s" % (
                node_id,
                origin,
                descriptor.tree,
            ))
        existing = roots.get(descriptor.tree)
        if existing != None and existing != root:
            fail("mapped Linux node %s resolves conflicting roots for artifact tree %s" % (node_id, descriptor.tree))
        roots[descriptor.tree] = root
    return roots

def _selected_prior_outputs(input_directories, nodes, declared_outputs):
    """Indexes every noncurrent descriptor in the exact stage shard."""
    required = {}
    for output_key, descriptor in declared_outputs.items():
        if output_key.split(":")[0] in nodes:
            continue
        required.setdefault(descriptor.tree, {})[descriptor.artifact_path] = True

    prior = {}
    for tree, paths in required.items():
        directory = input_directories.get(tree)
        if directory == None:
            continue
        for child in directory.children:
            if child.tree_relative_path in paths:
                prior[tree + ":" + child.tree_relative_path] = child
    return prior

# Test seam for the recipe-opaque map_directory edge resolver. Production and
# tests intentionally share this implementation so role/ordinal names cannot
# be reconstructed differently in either path.
def linux_test_resolve_node_input_bindings(node_id, node, outputs, prior, declared_outputs):
    return _resolve_node_input_bindings(node_id, node["inputs"], outputs, prior, declared_outputs)

def linux_test_decode_packed_node_inputs(parsed, node_id):
    return _decode_packed_node_inputs(node_id, parsed.nodes[node_id], parsed.node_index)

def linux_test_node_input_artifact_tree_roots(node_id, dependencies, outputs, input_directories, output_directories, declared_outputs):
    return _node_input_artifact_tree_roots(
        node_id,
        dependencies,
        outputs,
        input_directories,
        output_directories,
        declared_outputs,
    )

def linux_test_selected_prior_outputs(input_directories, nodes, declared_outputs):
    return _selected_prior_outputs(input_directories, nodes, declared_outputs)

def _tree_input_directory_name(tree, input_directories, output_directories, additional_params):
    directory_name = additional_params.get("input_tree_alias_" + tree, tree)
    if directory_name in input_directories:
        return directory_name
    if tree in output_directories:
        return tree
    fail("mapped Linux action requires unavailable %s tree (resolved input %s)" % (tree, directory_name))

def linux_test_tree_input_directory_name(tree, input_directories, output_directories, additional_params):
    return _tree_input_directory_name(tree, input_directories, output_directories, additional_params)

def _composed_tree_base_paths(base_paths, planned_paths, tree):
    """Selects base leaves not replaced by plan outputs and validates shape."""
    planned = {path: True for path in planned_paths}
    selected_base = [path for path in base_paths if path not in planned]
    owners = [(path, "base") for path in selected_base] + [(path, "plan") for path in planned]
    owners = sorted(owners)
    for index in range(len(owners) - 1):
        parent = owners[index]
        child = owners[index + 1]
        if child[0].startswith(parent[0] + "/"):
            fail("mapped Linux composed %s tree has file/subtree collision between %s %r and %s %r" % (
                tree,
                parent[1],
                parent[0],
                child[1],
                child[0],
            ))
    return sorted(selected_base)

def linux_test_composed_tree_base_paths(base_paths, planned_paths, tree = "prep"):
    return _composed_tree_base_paths(base_paths, planned_paths, tree)

def linux_test_parse_plan_marker_paths(paths, stage, source_paths = []):
    """Exercises the production path-only plan parser with synthetic markers."""
    plan = struct(children = [
        struct(tree_relative_path = path)
        for path in paths
    ])
    input_directories = {"plan": plan}
    if source_paths:
        input_directories["kernel"] = struct(children = [
            struct(tree_relative_path = path)
            for path in source_paths
        ])
    return _parse_plan(
        plan,
        input_directories,
        {"source_files": depset()},
        "",
        stage,
    )

def _declare_working_output(template_ctx, work_directory, node_id):
    return struct(
        artifact = template_ctx.declare_file(
            node_id + "/.linux-bzl-work-root",
            directory = work_directory,
        ),
        argument = "-working_directory_marker",
    )

def linux_test_declare_working_output(template_ctx, work_directory, node_id):
    """Exercises the Bazel-9-compatible private-work marker declaration."""
    return _declare_working_output(template_ctx, work_directory, node_id)

def _tool_executable(tool):
    executable = getattr(tool, "executable", None)
    return executable if executable != None else tool

def _runtime_tool_bindings(tools, current_scope = "target", selected_scopes = None):
    """Returns current aliases plus selected opposite-scope executable bindings."""
    if selected_scopes == None:
        selected_scopes = {current_scope: True}
    bindings = {}
    for binding, tool in tools.items():
        if binding in ["runner", "toolchain_files"] or binding.startswith(_TOOLCHAIN_FILES_PREFIX) or binding.startswith(_COMPANION_TOOL_PREFIX):
            continue
        parsed = _split_tool_binding(binding, current_scope)
        if parsed.scoped:
            if parsed.scope == current_scope or parsed.scope not in selected_scopes:
                continue
        bindings[binding] = _tool_executable(tool)
    return bindings

def linux_test_runtime_tool_bindings(tools, current_scope = "target", selected_scopes = None):
    return _runtime_tool_bindings(tools, current_scope, selected_scopes)

def _companion_tool_bindings(tools, binding, current_scope = "target"):
    parsed = _split_tool_binding(binding, current_scope)
    scoped = _scoped_tool_binding(parsed.scope, parsed.role)
    prefix = _COMPANION_TOOL_PREFIX + scoped + "_"
    return [
        tools[name]
        for name in sorted(tools)
        if name.startswith(prefix)
    ]

def linux_test_companion_tool_bindings(tools, role, current_scope = "target"):
    return _companion_tool_bindings(tools, role, current_scope)

def _render_toolchain_action_contracts(additional_params, tools):
    roles = {}
    prefix = "action_arg_count_"
    for name in additional_params:
        if name.startswith(prefix):
            roles[name[len(prefix):]] = True
    contracts = {}
    path_indexes = {}
    for role in sorted(roles):
        parsed = _split_tool_binding(role)
        path_index = path_indexes.get(parsed.scope)
        if path_index == None:
            toolchain_files = tools.get(_TOOLCHAIN_FILES_PREFIX + parsed.scope)
            if toolchain_files == None:
                fail("mapped Linux action contract %s has no %s toolchain closure" % (role, parsed.scope))
            path_index = _toolchain_action_path_index(toolchain_files)
            path_indexes[parsed.scope] = path_index
        arguments = [
            _render_toolchain_action_value(
                additional_params["action_arg_%s_%d" % (role, index)],
                path_index,
            )
            for index in range(int(additional_params["action_arg_count_" + role]))
        ]
        environment = [
            _render_toolchain_action_value(
                additional_params["action_env_%s_%d" % (role, index)],
                path_index,
            )
            for index in range(int(additional_params.get("action_env_count_" + role, "0")))
        ]
        contracts[role] = struct(arguments = arguments, environment = environment)
    return contracts

def linux_test_render_toolchain_action_value(value, artifacts):
    return _render_toolchain_action_value(value, _toolchain_action_path_index_from_list(artifacts))

def linux_test_canonicalize_toolchain_action_value(value, artifacts):
    return _canonicalize_toolchain_action_value(value, _toolchain_action_path_index_from_list(artifacts))

def linux_test_execution_root_marker():
    return _EXECUTION_ROOT_MARKER

def expand_linux_plan_stage(template_ctx, input_directories, output_directories, additional_inputs, tools, additional_params):
    stage = additional_params["stage"]
    parsed = _parse_plan(
        input_directories["plan"],
        input_directories,
        additional_inputs,
        additional_params["source_prefix"],
        stage,
        additional_params,
    )
    toolsets = _validate_plan_toolsets(parsed, input_directories)
    scope = "host" if stage in ["prehost", "host"] else "target"
    toolset = toolsets[scope]
    action_contracts = _render_toolchain_action_contracts(additional_params, tools)

    # Validate the complete stage against its bound toolset before declaring
    # any template actions. Recipes cannot introduce undeclared tool roles.
    for node_id, node in parsed.nodes.items():
        role = node.get("tool")
        if role != "generated" and role not in tools:
            fail("mapped Linux node %s requires unavailable %s tool" % (node_id, role))
        for auxiliary_binding in node["tools"]:
            if auxiliary_binding not in tools:
                fail("mapped Linux node %s requires unavailable auxiliary %s tool" % (node_id, auxiliary_binding))

    # Earlier stages are passed as input directories and expose concrete
    # TreeFile artifacts. Current-stage dependencies refer to outputs declared
    # below, allowing one expansion to retain the complete fine-grained DAG.
    prior = _selected_prior_outputs(input_directories, parsed.nodes, parsed.declared_outputs)

    outputs = {}
    output_owners = {}
    planned_paths = {tree: [] for tree in _TREES}
    for node_id in sorted(parsed.nodes):
        node = parsed.nodes[node_id]
        for slot, descriptor in sorted(node["outputs"].items()):
            if descriptor.tree not in output_directories:
                fail("mapped Linux %s node writes unavailable %s tree" % (node_id, descriptor.tree))
            owner = output_owners.get(descriptor.tree + ":" + descriptor.artifact_path)
            if owner != None:
                fail("mapped Linux nodes %s and %s both produce %s" % (owner, node_id, descriptor.artifact_path))
            output_owners[descriptor.tree + ":" + descriptor.artifact_path] = node_id
            planned_paths[descriptor.tree].append(descriptor.artifact_path)

    # Validate plan-owned leaves for every tree, including trees without an
    # immutable composition base. Bazel declare_file reports a less useful
    # prefix error after declarations begin; fail here with the two logical
    # paths and their tree before creating any action outputs.
    for tree in _TREES:
        _composed_tree_base_paths([], planned_paths[tree], tree)

    # A stage may compose an immutable base tree with its exact plan-owned
    # leaves. Exact path collisions select the plan output; file/subtree prefix
    # collisions fail before any output is declared. Every retained base leaf
    # is copied by its own action with the TreeFile as an explicit input.
    for tree in _TREES:
        base_name = additional_params.get("output_tree_base_" + tree)
        if base_name == None:
            continue
        base = input_directories.get(base_name)
        if base == None or tree not in output_directories:
            fail("mapped Linux %s composition requires input %s and output %s" % (stage, base_name, tree))
        base_children = {child.tree_relative_path: child for child in base.children}
        selected_base_paths = _composed_tree_base_paths(base_children.keys(), planned_paths[tree], tree)
        copy_tool = tools.get("actionfile")
        if copy_tool == None:
            fail("mapped Linux %s composition requires actionfile tool" % stage)
        copy_executable = _tool_executable(copy_tool)
        for relative in selected_base_paths:
            child = base_children[relative]
            output = template_ctx.declare_file(relative, directory = output_directories[tree])
            output_owners[tree + ":" + relative] = "base " + base_name
            args = template_ctx.args()
            _add_artifact_path(args, "-input", child)
            _add_artifact_path(args, "-out", output)
            args.add("-preserve_mode")
            template_ctx.run(
                executable = copy_executable,
                inputs = [child],
                tools = [copy_tool],
                outputs = [output],
                arguments = [args],
                progress_message = "Composing Linux %s tree base %%{label}" % tree,
            )

    for node_id in sorted(parsed.nodes):
        node = parsed.nodes[node_id]
        for slot, descriptor in sorted(node["outputs"].items()):
            outputs[node_id + ":" + slot] = template_ctx.declare_file(
                descriptor.artifact_path,
                directory = output_directories[descriptor.tree],
            )

    for node_id in sorted(parsed.nodes):
        node = parsed.nodes[node_id]
        recipe_id = node.get("recipe")
        role = node.get("tool")
        kind = node.get("kind")
        if recipe_id not in parsed.recipes or not _valid_name(role or "") or not _valid_name(kind or "") or "product" not in node:
            fail("mapped Linux node %s has incomplete metadata" % node_id)
        decoded_inputs = _decode_packed_node_inputs(node_id, node, parsed.node_index)
        bindings = _resolve_node_input_bindings(node_id, decoded_inputs, outputs, prior, parsed.declared_outputs)
        if bindings == None:
            fail("mapped Linux %s stage contains an unavailable prior-stage output" % stage)
        artifact_tree_roots = _node_input_artifact_tree_roots(
            node_id,
            decoded_inputs,
            outputs,
            input_directories,
            output_directories,
            parsed.declared_outputs,
        )

        args = template_ctx.args()
        args.add("-recipe", parsed.recipes[recipe_id])
        args.add("-kind", kind)
        args.add("-expected_node_id", node_id)
        args.add("-expected_recipe_id", recipe_id)
        args.add("-tool_role", role)
        input_bindings = node["input_bindings"]
        _add_artifact_path(args, "-input_bindings", input_bindings.file)
        args.add("-expected_input_bindings_id", input_bindings.id)
        for tree, root in sorted(artifact_tree_roots.items()):
            _add_artifact_path(args, "-artifact_tree", root, format = tree + "=%s")
        inputs = [parsed.recipes[recipe_id], input_bindings.file, toolset.plan_file, toolset.expected_file]
        for key, source_id in sorted(node["sources"].items()):
            source = parsed.sources.get(source_id)
            if source == None:
                fail("mapped Linux node %s references unknown source %s" % (node_id, source_id))
            _add_artifact_path(args, "-source", source.file, format = key + "=%s")
            inputs.append(source.file)
        for key in sorted(bindings):
            inputs.append(bindings[key])
        selected_scopes = {scope: True}
        for descriptor in node["tools"].values():
            selected_scopes[descriptor.scope] = True
        runtime_tools = _runtime_tool_bindings(tools, scope, selected_scopes)
        for runtime_role, runtime_tool in sorted(runtime_tools.items()):
            _add_artifact_path(args, "-runtime_tool", runtime_tool, format = runtime_role + "=%s")
        for slot in sorted(node["outputs"]):
            artifact = outputs[node_id + ":" + slot]
            _add_artifact_path(args, "-recipe_output", artifact, format = slot + "=%s")
        root_marker = additional_inputs.get("source_root")
        transitive_inputs = [
            additional_inputs[key]
            for key in _node_source_closure_keys(node, parsed.sources)
        ]
        for tree in sorted(node["trees"]):
            if tree == "kernel" and root_marker != None:
                # source_root is a File because Args can path-map Files,
                # but cannot path-map an artifact's dirname string. The
                # runner accepts the marker and resolves its directory.
                tree_path = root_marker
                inputs.append(root_marker)
                transitive_inputs.append(additional_inputs["source_files"])
            elif _tree_input_directory_name(tree, input_directories, output_directories, additional_params) in input_directories:
                directory = input_directories[_tree_input_directory_name(tree, input_directories, output_directories, additional_params)]
                tree_path = directory.directory
                inputs.append(directory.directory)
            elif tree in output_directories:
                tree_path = output_directories[tree]

                # A current-stage output directory is a namespace for sibling
                # TreeFiles, not a complete action input. The runner builds a
                # private view containing only this node's exact producer
                # inputs before exposing the tree path to the selected tool.
                args.add("-private_input_tree", tree)
            else:
                fail("mapped Linux node %s requires unavailable %s tree" % (node_id, tree))
            _add_artifact_path(args, "-input_tree", tree_path, format = tree + "=%s")
        selected_tool = None
        selected_companion_tools = []
        if role != "generated":
            selected_tool = tools.get(role)
            if selected_tool == None:
                fail("mapped Linux node %s requires unavailable %s tool" % (node_id, role))
            selected_executable = _tool_executable(selected_tool)
            _add_artifact_path(args, "-tool", selected_executable, format = role + "=%s")
            inputs.append(selected_executable)
            selected_companion_tools.extend(_companion_tool_bindings(tools, role, scope))
        selected_auxiliary_tools = []
        for auxiliary_binding in sorted(node["tools"]):
            descriptor = node["tools"][auxiliary_binding]
            auxiliary_tool = tools.get(auxiliary_binding)
            if auxiliary_tool == None:
                fail("mapped Linux node %s requires unavailable auxiliary %s tool" % (node_id, auxiliary_binding))
            auxiliary_executable = _tool_executable(auxiliary_tool)
            _add_artifact_path(args, "-tool", auxiliary_executable, format = auxiliary_binding + "=%s")
            inputs.append(auxiliary_executable)
            selected_auxiliary_tools.append(auxiliary_tool)
            selected_companion_tools.extend(_companion_tool_bindings(tools, auxiliary_binding, scope))
            configured_binding = _scoped_tool_binding(descriptor.scope, descriptor.role)
            auxiliary_contract_roles = [(auxiliary_binding, configured_binding)]
            recipe_companion_role = _driver_link_contract_role(auxiliary_binding)
            configured_companion_role = _driver_link_contract_role(configured_binding)
            if configured_companion_role != None and action_contracts.get(configured_companion_role) != None:
                auxiliary_contract_roles.append((recipe_companion_role, configured_companion_role))
            for recipe_contract_role, configured_contract_role in auxiliary_contract_roles:
                auxiliary_contract = action_contracts.get(configured_contract_role)
                if auxiliary_contract == None:
                    continue

                # Preserve the complete configured Bazel action contract
                # when a declared source tool invokes this role. The recipe
                # runner forwards this opaque role/argv/env contract; it
                # never needs to identify a compiler family.
                args.add("-auxiliary_action_role", recipe_contract_role)
                for argument in auxiliary_contract.arguments:
                    _add_rendered_toolchain_action_value(
                        args,
                        "-auxiliary_action_arg",
                        argument,
                        prefix = recipe_contract_role + "=",
                    )
                for environment in auxiliary_contract.environment:
                    _add_rendered_toolchain_action_value(
                        args,
                        "-auxiliary_action_env",
                        environment,
                        prefix = recipe_contract_role + "=",
                    )
        selected_contract = action_contracts.get(_scoped_tool_binding(scope, _node_action_contract_role(kind, role)))
        if selected_contract != None:
            for argument in selected_contract.arguments:
                _add_rendered_toolchain_action_value(args, "-action_arg", argument)
            for environment in selected_contract.environment:
                _add_rendered_toolchain_action_value(args, "-action_env", environment)
        work_directory = output_directories.get("work")
        if work_directory == None:
            fail("mapped Linux %s stage has no private work output directory" % stage)
        action_outputs = [outputs[node_id + ":" + slot] for slot in sorted(node["outputs"])]
        working_output = _declare_working_output(
            template_ctx,
            work_directory,
            node_id,
        )
        _add_artifact_path(args, working_output.argument, working_output.artifact)
        action_outputs.append(working_output.artifact)

        template_ctx.run(
            executable = tools["runner"],
            inputs = depset(inputs, transitive = transitive_inputs),
            tools = [tools[_TOOLCHAIN_FILES_PREFIX + selected_scope] for selected_scope in sorted(selected_scopes)] + ([selected_tool] if role != "generated" else []) + selected_auxiliary_tools + selected_companion_tools,
            outputs = action_outputs,
            arguments = [args],
            progress_message = "Building Linux %s node %s" % (kind, node_id[:12]),
        )

def _tool_file_path_index_from_list(files):
    path_to_file = {}
    duplicate_counts = {}
    for file in files:
        if file.path in path_to_file:
            duplicate_counts[file.path] = duplicate_counts.get(file.path, 1) + 1
        else:
            path_to_file[file.path] = file
    return struct(
        duplicate_counts = duplicate_counts,
        path_to_file = path_to_file,
    )

def _tool_file(path_index, path, role):
    duplicate_count = path_index.duplicate_counts.get(path)
    if duplicate_count != None:
        fail("selected C/C++ toolchain must expose exactly one %s File at %r; found %d" % (role, path, duplicate_count))
    file = path_index.path_to_file.get(path)
    if file == None:
        fail("selected C/C++ toolchain must expose exactly one %s File at %r; found 0" % (role, path))
    return file

def linux_test_tool_file(files, path, role):
    return _tool_file(_tool_file_path_index_from_list(files), path, role)

def _kbuild_action_name(scope, role):
    return "linux-kbuild-%s-%s" % (scope, role)

def linux_test_kbuild_action_name(scope, role):
    return _kbuild_action_name(scope, role)

def _driver_link_contract_role(role):
    parsed = _split_tool_binding(role, "target")
    if parsed.role not in ["cc", "cxx"]:
        return None
    linked = parsed.role + "-link"
    return _scoped_tool_binding(parsed.scope, linked) if parsed.scoped else linked

def _merge_action_arguments(outer, inner, owner):
    """Splices one configured action envelope through another's sentinel."""
    outer_markers = [index for index, value in enumerate(outer) if value == _KBUILD_ARGS_SENTINEL]
    inner_markers = [value for value in inner if value == _KBUILD_ARGS_SENTINEL]
    if len(outer_markers) != 1 or len(inner_markers) != 1:
        fail("%s action envelopes must each contain exactly one %s argument" % (owner, _KBUILD_ARGS_SENTINEL))
    index = outer_markers[0]
    merged = outer[:index] + inner + outer[index + 1:]
    if len([value for value in merged if value == _KBUILD_ARGS_SENTINEL]) != 1:
        fail("%s merged action envelope must contain exactly one %s argument" % (owner, _KBUILD_ARGS_SENTINEL))
    return merged

def linux_test_merge_action_arguments(outer, inner):
    return _merge_action_arguments(outer, inner, "test")

def _node_action_contract_role(kind, role):
    contract_role = _driver_link_contract_role(role)
    return contract_role if kind == "link-driver" and contract_role != None else role

def linux_test_node_action_contract_role(kind, role):
    return _node_action_contract_role(kind, role)

def _merge_action_environment(base_owner, base, extra_owner, extra):
    merged = dict(base)
    for name, value in extra.items():
        if name in merged and merged[name] != value:
            fail("configured action environments disagree on %s: %s selected %r, but %s selected %r" % (
                name,
                base_owner,
                merged[name],
                extra_owner,
                value,
            ))
        merged[name] = value
    return merged

def _configured_action_contract(features, action, variables, owner):
    arguments = cc_common.get_memory_inefficient_command_line(
        feature_configuration = features,
        action_name = action,
        variables = variables,
    )
    if len([argument for argument in arguments if argument == _KBUILD_ARGS_SENTINEL]) != 1:
        fail("C/C++ action %s must contain exactly one %s argument" % (action, _KBUILD_ARGS_SENTINEL))
    environment = dict(cc_common.get_environment_variables(
        feature_configuration = features,
        action_name = action,
        variables = variables,
    ))
    requirements = {
        key: ""
        for key in sorted(cc_common.get_execution_requirements(
            feature_configuration = features,
            action_name = action,
        ))
    }
    return struct(
        arguments = arguments,
        environment = environment,
        owner = owner,
        requirements = requirements,
    )

def _with_compile_action_arguments(arguments, additional, owner):
    """Adds configured dependency flags at the user-argument boundary."""
    markers = [index for index, value in enumerate(arguments) if value == _KBUILD_ARGS_SENTINEL]
    if len(markers) != 1:
        fail("%s action envelope must contain exactly one %s argument" % (owner, _KBUILD_ARGS_SENTINEL))
    index = markers[0]
    merged = arguments[:index + 1] + additional + arguments[index + 1:]
    if len([value for value in merged if value == _KBUILD_ARGS_SENTINEL]) != 1:
        fail("%s action envelope must retain exactly one %s argument" % (owner, _KBUILD_ARGS_SENTINEL))
    return merged

def linux_test_with_compile_action_arguments(arguments, additional):
    return _with_compile_action_arguments(arguments, additional, "test")

def _with_link_runtime_arguments(arguments, runtime_files, owner):
    """Places configured runtime archives after source-selected link inputs."""
    return _with_compile_action_arguments(
        arguments,
        [file.path for file in runtime_files],
        owner,
    )

def linux_test_with_link_runtime_arguments(arguments, runtime_files):
    return _with_link_runtime_arguments(arguments, runtime_files, "test")

def _kbuild_toolset(ctx, cc_toolchain, scope, additional_compile_flags = []):
    features = cc_common.configure_features(
        ctx = ctx,
        cc_toolchain = cc_toolchain,
        requested_features = ctx.features,
        unsupported_features = ctx.disabled_features,
    )
    variables = cc_common.create_compile_variables(
        feature_configuration = features,
        cc_toolchain = cc_toolchain,
    )
    tools = {}
    arguments = {}
    environments = {}
    requirements_by_role = {}
    make_variables = {}
    tool_file_path_index = _tool_file_path_index_from_list(cc_toolchain.all_files.to_list())
    for role in _KBUILD_ACTION_ROLES:
        action = _kbuild_action_name(scope, role)
        path = cc_common.get_tool_for_action(feature_configuration = features, action_name = action)
        tools[role] = _tool_file(tool_file_path_index, path, role)
        contract = _configured_action_contract(features, action, variables, action)
        arguments[role] = contract.arguments
        if role in ["cc", "cxx"]:
            arguments[role] = _with_compile_action_arguments(
                arguments[role],
                additional_compile_flags,
                action,
            )
        environments[role] = contract.environment
        requirements_by_role[role] = contract.requirements
        make_variables[("HOST" if scope == "host" else "") + role.upper()] = role

    # A compiler driver is also a linker, but Kbuild selects the executable
    # through CC/HOSTCC rather than Bazel's C++ link action.  Derive Bazel's
    # complete configured executable-link envelope (crt/sysroot/runtime policy)
    # and merge the source-selected cc/cxx envelope through its user-flag
    # insertion point.  The pseudo roles carry only semantics: both retain the
    # exact source-selected executable File.
    link_variables = cc_common.create_link_variables(
        cc_toolchain = cc_toolchain,
        feature_configuration = features,
        is_linking_dynamic_library = False,
        is_using_linker = True,
        user_link_flags = [_KBUILD_ARGS_SENTINEL],
    )
    link_contract = _configured_action_contract(
        features,
        ACTION_NAMES.cpp_link_executable,
        link_variables,
        ACTION_NAMES.cpp_link_executable,
    )
    link_runtime_files = cc_toolchain.static_runtime_lib(
        feature_configuration = features,
    )
    if link_runtime_files == None:
        # Bazel's provider permits toolchains without an embedded C++ runtime.
        # Feature-aware implementations normally return an empty depset when
        # static_link_cpp_runtimes is disabled; normalize older/custom
        # providers that expose None to the same contract.
        link_runtime_files = depset()
    link_arguments = _with_link_runtime_arguments(
        link_contract.arguments,
        link_runtime_files.to_list(),
        "%s C/C++ link runtime" % scope,
    )
    for role in ["cc", "cxx"]:
        contract_role = _driver_link_contract_role(role)
        compile_owner = _kbuild_action_name(scope, role)
        tools[contract_role] = tools[role]
        arguments[contract_role] = _merge_action_arguments(
            link_arguments,
            arguments[role],
            "%s %s" % (scope, contract_role),
        )
        environments[contract_role] = _merge_action_environment(
            link_contract.owner,
            link_contract.environment,
            compile_owner,
            environments[role],
        )
        requirements_by_role[contract_role] = _merge_execution_requirements(
            link_contract.owner,
            link_contract.requirements,
            [(compile_owner, requirements_by_role[role])],
        )
    return struct(
        arguments = arguments,
        companion_tools = {},
        environments = environments,
        link_runtime_files = link_runtime_files,
        requirements_by_role = requirements_by_role,
        make_variables = make_variables,
        tools = tools,
    )

def linux_kbuild_toolset(ctx, cc_toolchain, scope):
    """Returns the exact configured compiler contract used by mapped Kbuild."""
    return _kbuild_toolset(ctx, cc_toolchain, scope)

def linux_toolset_execution_requirements(requirements_by_role, owner):
    """Returns the Bazel-9-compatible union for one mapped stage.

    template_ctx.run inherits execution requirements from map_directory, so
    every role in a stage must agree when it names the same requirement.
    """
    requirements = [("%s %s action" % (owner, role), requirements_by_role[role]) for role in sorted(requirements_by_role)]
    return _merge_execution_requirements(requirements[0][0], requirements[0][1], requirements[1:]) if requirements else {}

def linux_merge_toolset_execution_requirements(target_requirements, host_requirements, owner):
    """Unions both configured toolsets and rejects contradictory requirements."""
    target = linux_toolset_execution_requirements(target_requirements, owner + " target")
    host = linux_toolset_execution_requirements(host_requirements, owner + " host")
    return _merge_execution_requirements(owner + " target toolset", target, [(owner + " host toolset", host)])

def linux_test_toolset_execution_requirements(requirements_by_role, owner):
    return linux_toolset_execution_requirements(requirements_by_role, owner)

def _merge_execution_requirements(base_owner, base, named_requirements):
    out = dict(base)
    owners = {name: base_owner for name in base}
    for owner, requirements in named_requirements:
        for name, value in requirements.items():
            if name in out and out[name] != value:
                fail("tool execution requirements disagree on %s: %s selected %r, but %s selected %r" % (
                    name,
                    owners[name],
                    out[name],
                    owner,
                    value,
                ))
            out[name] = value
            owners[name] = owner
    return out

def _with_auxiliary_tools(
        toolset,
        scope,
        auxiliary,
        make_variables = None,
        action_arguments = None):
    """Binds identity-pinned non-compiler tools into a Kbuild toolset."""
    if make_variables == None:
        make_variables = {}
    if action_arguments == None:
        action_arguments = {}
    arguments = dict(toolset.arguments)
    tools = dict(toolset.tools)
    environments = dict(toolset.environments)
    requirements_by_role = dict(toolset.requirements_by_role)
    selected_make_variables = dict(toolset.make_variables)
    for role, executable in auxiliary.items():
        if role in tools:
            fail("%s auxiliary tool role %r is already selected" % (scope, role))
        arguments[role] = action_arguments.get(role, [])
        tools[role] = executable
        environments[role] = {}
        requirements_by_role[role] = {}
    for variable, role in make_variables.items():
        if variable in selected_make_variables:
            fail("%s Make variable %r is already bound" % (scope, variable))
        if role not in auxiliary:
            fail("%s Make variable %r references undeclared auxiliary role %r" % (scope, variable, role))
        selected_make_variables[variable] = role
    unexpected_arguments = sorted([role for role in action_arguments if role not in auxiliary])
    if unexpected_arguments:
        fail("%s auxiliary action arguments reference undeclared roles %r" % (scope, unexpected_arguments))
    return struct(
        arguments = arguments,
        companion_tools = getattr(toolset, "companion_tools", {}),
        environments = environments,
        requirements_by_role = requirements_by_role,
        make_variables = selected_make_variables,
        tools = tools,
    )

def _write_pkg_config_manifest(ctx, packages):
    """Writes the bounded package database consumed by the configured shim."""
    manifest = ctx.actions.declare_file(ctx.label.name + ".pkg-config.json")
    package_values = {}
    for name in sorted(packages):
        package = packages[name]
        package_values[name] = {
            "cflags": package.compile_flags,
            "libs": package.link_flags,
        }
    ctx.actions.write(
        output = manifest,
        content = json.encode({
            "packages": package_values,
            "schema": "linux.bzl/pkg-config-manifest/v1",
        }) + "\n",
    )
    return manifest

def _with_pkg_config(toolset, executable, manifest):
    """Binds source-owned pkg-config queries to a declared package manifest."""
    role = "pkg-config"
    return _with_auxiliary_tools(
        toolset,
        "host",
        {role: executable},
        action_arguments = {
            role: [
                "-manifest",
                manifest.path,
                "--",
                _KBUILD_ARGS_SENTINEL,
            ],
        },
        make_variables = {"HOSTPKG_CONFIG": role},
    )

def _with_script_runtime(toolset, scope, runtime):
    """Binds one interpreter plus its command-name multicall overrides."""
    auxiliary = {"script-runtime": runtime.multicall}
    for name, executable in runtime.applets.items():
        role = _SCRIPT_RUNTIME_APPLET_ROLE_PREFIX + name
        if not _valid_name(role):
            fail("Linux %s script runtime has invalid applet name %r" % (scope, name))
        auxiliary[role] = executable
    return _with_auxiliary_tools(toolset, scope, auxiliary)

def _with_script_applet(toolset, scope, name, executable):
    """Adds a hermetic interpreter unless the selected runtime overrides it."""
    role = _SCRIPT_RUNTIME_APPLET_ROLE_PREFIX + name
    if role in toolset.tools:
        return toolset
    return _with_auxiliary_tools(toolset, scope, {role: executable})

def _executable_label_closure(target):
    info = target[DefaultInfo]
    return depset(transitive = [
        info.files,
        info.default_runfiles.files,
        info.data_runfiles.files,
    ])

def _hermetic_python_runtime(toolchain, scope):
    if toolchain == None or not hasattr(toolchain, "py3_runtime") or toolchain.py3_runtime == None:
        fail("Linux %s toolset requires a registered Python 3 execution toolchain" % scope)
    runtime = toolchain.py3_runtime
    if runtime.interpreter == None:
        fail("Linux %s toolset requires a hermetic Python runtime artifact; interpreter_path runtimes are not supported" % scope)
    transitive = [runtime.files] if runtime.files != None else []
    return struct(
        files = depset(direct = [runtime.interpreter], transitive = transitive),
        interpreter = runtime.interpreter,
    )

def _execution_python_runtime(toolchain, scope):
    if toolchain == None or not hasattr(toolchain, "exec_tools"):
        fail("Linux %s toolset requires a registered Python execution-tools toolchain" % scope)
    exec_tools = toolchain.exec_tools
    if exec_tools == None or not hasattr(exec_tools, "exec_interpreter"):
        fail("Linux %s toolset selected an incomplete Python execution-tools toolchain" % scope)
    interpreter = exec_tools.exec_interpreter
    if interpreter == None or platform_common.ToolchainInfo not in interpreter:
        fail("Linux %s toolset requires an execution-platform Python interpreter" % scope)
    return _hermetic_python_runtime(interpreter[platform_common.ToolchainInfo], scope)

def _filtered_toolchain_closure(base_closures, additional, scope):
    """Uses the base toolset artifact for canonical duplicates in a closure."""
    files = {}
    for closure in base_closures:
        for file in closure.to_list():
            _record_canonical_file(files, file, "%s base toolchain closure" % scope)
    additional_files = {}
    for file in additional.to_list():
        canonical = _canonical_file_path(file, "%s additional toolchain closure" % scope)
        if canonical not in files:
            additional_files[canonical] = file
    return depset(direct = [additional_files[path] for path in sorted(additional_files)])

def _sequence(value):
    return value.to_list() if type(value) == "depset" else list(value)

def _host_dependency_path(value, what):
    return _HOST_DEPS_SENTINEL + "/" + _canonical_artifact_path(value, what)

def _append_unique(values, seen, value):
    if value not in seen:
        seen[value] = True
        values.append(value)

def _host_dependency_compile_flags(compilation, name):
    flags = []
    seen = {}
    for define in _sequence(getattr(compilation, "defines", depset())):
        _append_unique(flags, seen, "-D" + define)

    # local_defines are private to the dependency target that produced the
    # CcInfo; Bazel does not propagate them to consumers, so neither do we.
    for field, option in [
        ("quote_includes", "-iquote"),
        # Kbuild consumes this CcInfo as a host system dependency (the same
        # role normally filled by pkg-config/system libelf), while retaining
        # Linux's own -Werror policy.  Classify ordinary dependency include
        # roots as system headers so warnings in staged transitive headers do
        # not become Linux build failures.  Quote-only roots retain their
        # search semantics, and explicitly typed system/external roots remain
        # system roots below.
        ("includes", "-isystem"),
        ("system_includes", "-isystem"),
        ("external_includes", "-isystem"),
    ]:
        for directory in _sequence(getattr(compilation, field, depset())):
            _append_unique(
                flags,
                seen,
                option + _host_dependency_path(directory, "%s %s" % (name, field)),
            )
    framework_includes = _sequence(getattr(compilation, "framework_includes", depset()))
    if framework_includes:
        fail("%s host dependency uses unsupported framework includes %r" % (name, framework_includes))
    return flags

def _host_library_artifact(library, what):
    if getattr(library, "alwayslink", False):
        fail("%s requires alwayslink/whole-archive semantics, which cannot be preserved by Linux host dependency staging" % what)

    # A static archive is the native representation for Kbuild's HOSTLD path.
    # rules_cc may expose only a PIC archive (notably for elfutils), which is
    # equally valid for an executable link and must not be mistaken for an
    # object-only library.
    static_library = getattr(library, "static_library", None)
    if static_library != None:
        return static_library
    pic_static_library = getattr(library, "pic_static_library", None)
    if pic_static_library != None:
        return pic_static_library

    for field in [
        "interface_library",
        "dynamic_library",
        "resolved_symlink_interface_library",
        "resolved_symlink_dynamic_library",
    ]:
        if getattr(library, field, None) != None:
            fail("%s has only dynamic/interface library inputs; Linux host dependency staging currently supports static archives" % what)
    for field in ["objects", "pic_objects"]:
        if _sequence(getattr(library, field, [])):
            fail("%s has only loose object inputs; Linux host dependency staging currently supports static archives" % what)
    fail("%s has no supported link artifact" % what)

def _host_dependency_library_search_flags(paths):
    """Returns stable search roots for source-owned -l flags.

    CcInfo gives us the exact archives Bazel selected, but Linux host-tool
    Makefiles may append a conventional flag such as -lz independently of
    pkg-config. Preserve that source-owned argv while making it hermetic by
    exposing only the parent directories of the staged CcInfo libraries.
    """
    directories = {}
    flags = []
    for library in paths:
        parts = library.split("/")
        if len(parts) < 2:
            fail("staged host dependency library %r has no parent directory" % library)
        directory = "/".join(parts[:-1])
        if directory not in directories:
            directories[directory] = True
            flags.append("-L" + directory)
    return flags

def _rewrite_host_dependency_link_token(token, paths):
    for source in sorted(paths, key = len, reverse = True):
        destination = paths[source]
        if token == source:
            return destination
        if token == "@" + source:
            return "@" + destination
        if token == "-L" + source:
            return "-L" + destination
        if token == "-T" + source:
            return "-T" + destination
        if token.endswith("=" + source):
            return token[:-len(source)] + destination
    return token

def _host_dependency_link_token_references_path(token, source):
    return (
        token == source or
        token == "@" + source or
        token == "-L" + source or
        token == "-T" + source or
        token.endswith("=" + source)
    )

def _rewrite_host_dependency_link_flag(flag, paths, what):
    # Linker flags are already argv elements. Rewrite only forms whose path
    # boundary is unambiguous: a complete argument, response file, -L/-T,
    # option assignment, or one comma-delimited -Wl component.
    rewritten = _rewrite_host_dependency_link_token(flag, paths)
    if rewritten == flag and "," in flag:
        rewritten = ",".join([
            _rewrite_host_dependency_link_token(token, paths)
            for token in flag.split(",")
        ])

    # Do not silently retain an execroot path that is known to name a staged
    # side input. More exotic encodings need an explicit representation in
    # CcInfo before they can be mapped without changing linker semantics.
    tokens = flag.split(",")
    destinations = {}
    for destination in paths.values():
        destinations[destination] = True
    for destination in destinations:
        referenced = [
            source
            for source in paths
            if paths[source] == destination and source.find("/") != -1 and flag.find(source) != -1
        ]
        if not referenced:
            continue
        source = sorted(referenced, key = len, reverse = True)[0]
        if not any([_host_dependency_link_token_references_path(token, source) for token in tokens]):
            fail("%s link flag %r embeds staged artifact path %r in an unsupported form" % (what, flag, source))
    return rewritten

def _validate_host_dependency_artifact(file, what):
    if file.is_directory:
        fail("%s is a TreeArtifact; Linux host dependency staging requires individual files" % what)

# Test seams for CcInfo projection policy. They deliberately accept ordinary
# structs and strings so policy tests do not need a configured C++ toolchain.
def linux_test_host_dependency_compile_flags(compilation, name):
    return _host_dependency_compile_flags(compilation, name)

def linux_test_host_library_artifact(library, what):
    return _host_library_artifact(library, what)

def linux_test_host_dependency_library_search_flags(paths):
    return _host_dependency_library_search_flags(paths)

def linux_test_rewrite_host_dependency_link_flag(flag, paths, what = "test dependency"):
    return _rewrite_host_dependency_link_flag(flag, paths, what)

def linux_test_validate_host_dependency_artifact(file, what):
    _validate_host_dependency_artifact(file, what)

def _host_cc_dependency_projection(target, name):
    """Projects one exec-configured CcInfo into the shared logical tree."""
    info = target[CcInfo]
    compilation = info.compilation_context
    staged = {}
    staged_paths = {}
    link_flag_paths = {}

    def record(file, what, link_flag_input = False):
        _validate_host_dependency_artifact(file, what)
        canonical = _record_canonical_file(staged, file, what)
        destination = _HOST_DEPS_SENTINEL + "/" + canonical
        for path in [file.path, file.short_path, canonical]:
            existing = staged_paths.get(path)
            if existing != None and existing != destination:
                fail("%s artifact path %r maps to both %r and %r" % (name, path, existing, destination))
            staged_paths[path] = destination
            if link_flag_input:
                link_flag_paths[path] = destination
        return destination

    for header in compilation.headers.to_list():
        record(header, "%s header" % name)

    linker_inputs = info.linking_context.linker_inputs.to_list()
    for linker_input in linker_inputs:
        for additional in _sequence(linker_input.additional_inputs):
            record(additional, "%s linker input" % name, link_flag_input = True)
        for library in _sequence(linker_input.libraries):
            artifact = _host_library_artifact(library, "%s library from %s" % (name, linker_input.owner))
            record(artifact, "%s library" % name)

    compile_flags = _host_dependency_compile_flags(compilation, name)
    library_paths = []
    for linker_input in linker_inputs:
        for library in _sequence(linker_input.libraries):
            artifact = _host_library_artifact(library, "%s library from %s" % (name, linker_input.owner))
            library_paths.append(staged_paths[artifact.path])
    link_flags = _host_dependency_library_search_flags(library_paths)
    for linker_input in linker_inputs:
        for flag in _sequence(linker_input.user_link_flags):
            link_flags.append(_rewrite_host_dependency_link_flag(
                flag,
                link_flag_paths,
                "%s linker input from %s" % (name, linker_input.owner),
            ))
        for library in _sequence(linker_input.libraries):
            artifact = _host_library_artifact(library, "%s library from %s" % (name, linker_input.owner))
            link_flags.append(staged_paths[artifact.path])
    if not link_flags:
        fail("%s host dependency has no link inputs" % name)

    return struct(
        compile_flags = compile_flags,
        files = staged,
        link_flags = link_flags,
    )

def _stage_host_cc_dependencies(ctx, targets):
    """Stages named exec-configured CcInfo packages in one logical tree."""
    staged = {}
    projections = {}
    for name in sorted(targets):
        projection = _host_cc_dependency_projection(targets[name], name)
        for canonical, file in projection.files.items():
            existing = staged.get(canonical)
            if existing != None and existing != file:
                fail("host dependencies map %r to both %s and %s" % (canonical, existing, file))
            staged[canonical] = file
        projections[name] = projection

    tree = ctx.actions.declare_directory(ctx.label.name + ".host-deps")
    args = ctx.actions.args()
    _add_artifact_path(args, "-tree_out", tree)
    for canonical in sorted(staged):
        _add_artifact_path(args, "-copy", staged[canonical], format = canonical + "=%s")
    ctx.actions.run(
        executable = ctx.executable._host_actionfile,
        inputs = depset(staged.values()),
        outputs = [tree],
        arguments = [args],
        exec_group = "host_cc",
        execution_requirements = {"supports-path-mapping": "1"},
        mnemonic = "LinuxHostDependencyTree",
        progress_message = "Staging Linux host dependencies %{label}",
    )
    packages = {}
    for name, projection in projections.items():
        packages[name] = struct(
            action_compile_flags = [
                flag.replace(_HOST_DEPS_SENTINEL, tree.path)
                for flag in projection.compile_flags
            ],
            compile_flags = projection.compile_flags,
            link_flags = projection.link_flags,
            tree = tree,
        )
    return struct(packages = packages, tree = tree)

def _add_host_dependency_variables(args, dependency):
    args.add("-var", "LIBELF_FLAGS=" + " ".join(dependency.compile_flags))
    args.add("-var", "LIBELF_LIBS=" + " ".join(dependency.link_flags))
    args.add("-source_root_map", _HOST_DEPS_SENTINEL + "=" + _HOST_DEPS_SENTINEL)

def _add_kernel_kbuild_goals(args):
    args.add("-kbuild_target", "all")
    args.add("-kbuild_prepare_target", "modules_prepare")

def _with_host_generators(
        host,
        bison,
        flex,
        m4,
        bison_m4_deny_shell,
        flex_m4_deny_shell,
        m4_deny_shell):
    arguments = dict(host.arguments)
    arguments.update({"bison": [], "flex": [], "m4": []})
    tools = dict(host.tools)
    tools.update({
        "bison": bison.bison_tool,
        "flex": flex.flex_tool,
        "m4": m4.m4_tool,
    })

    # Preserve the three upstream generator contracts independently. Their
    # deny-shell binaries are deliberately not interchangeable: each ruleset
    # owns the helper used by its public action, and Flex additionally requires
    # the selected M4 FilesToRunProvider (including its runfiles) as a tool.
    bison_environment = dict(bison.bison_env)
    bison_environment["M4_SYSCMD_SHELL"] = _tool_executable(bison_m4_deny_shell).path
    flex_environment = dict(flex.flex_env)
    if "M4" not in flex_environment:
        flex_environment["M4"] = m4.m4_tool.executable.path
    flex_environment["M4_SYSCMD_SHELL"] = _tool_executable(flex_m4_deny_shell).path
    m4_environment = dict(m4.m4_env)
    m4_environment["M4_SYSCMD_SHELL"] = _tool_executable(m4_deny_shell).path
    environments = dict(host.environments)
    environments.update({
        "bison": bison_environment,
        "flex": flex_environment,
        "m4": m4_environment,
    })
    requirements_by_role = dict(host.requirements_by_role)
    requirements_by_role.update({"bison": {}, "flex": {}, "m4": {}})
    make_variables = dict(host.make_variables)
    make_variables.update({"LEX": "flex", "M4": "m4", "YACC": "bison"})
    companion_tools = dict(getattr(host, "companion_tools", {}))
    companion_tools.update({
        "bison": [bison_m4_deny_shell],
        "flex": [flex_m4_deny_shell, m4.m4_tool],
        "m4": [m4_deny_shell],
    })
    return struct(
        arguments = arguments,
        companion_tools = companion_tools,
        environments = environments,
        requirements_by_role = requirements_by_role,
        make_variables = make_variables,
        tools = tools,
    )

def _with_rust_toolchain(scope, toolset, rust, bindgen = None):
    arguments = dict(toolset.arguments)
    tools = dict(toolset.tools)
    environments = dict(toolset.environments)
    requirements_by_role = dict(toolset.requirements_by_role)
    make_variables = dict(toolset.make_variables)

    # Toolchain providers select artifacts and their execution environment;
    # Linux's Makefiles select which of those tools to invoke.  In particular,
    # RUSTC_OR_CLIPPY and RUSTC_BOOTSTRAP are source-owned Kbuild policy.
    selected = []
    if rust != None:
        rust_environment = dict(getattr(rust, "env", {}))
        rustc = getattr(rust, "rustc", None)
        if rustc != None:
            selected.append((
                "rustc",
                rustc,
                rust_environment,
                "HOSTRUSTC" if scope == "host" else "RUSTC",
            ))
        if scope == "target":
            for role, field, variable, environment in [
                ("clippy", "clippy_driver", "CLIPPY_DRIVER", rust_environment),
                ("rustdoc", "rust_doc", "RUSTDOC", {}),
                ("rustfmt", "rustfmt", "RUSTFMT", {}),
            ]:
                executable = getattr(rust, field, None)
                if executable != None:
                    selected.append((role, executable, environment, variable))
    if bindgen != None:
        executable = getattr(bindgen, "bindgen", None)
        if executable != None:
            selected.append(("bindgen", executable, {}, "BINDGEN"))

    for role, executable, environment, variable in selected:
        if role in tools:
            fail("%s toolset already defines %s" % (scope, role))
        if variable in make_variables:
            fail("%s toolset already binds Make variable %s" % (scope, variable))
        if role in ["rustc", "clippy"]:
            # This is the generic rustc driver envelope, not a compiler-
            # capability answer: Linux still owns every Kconfig/Kbuild flag and
            # supplies the selected C linker through its source recipe. Disable
            # only rustc's bundled linker component so that explicit linker is
            # authoritative for any configured C/C++ toolchain.
            arguments[role] = [
                _KBUILD_ARGS_SENTINEL,
                "-Zunstable-options",
                "-Clink-self-contained=-linker",
            ]
        else:
            arguments[role] = []
        requirements_by_role[role] = {}
        tools[role] = executable
        environments[role] = environment
        make_variables[variable] = role
    return struct(
        arguments = arguments,
        companion_tools = getattr(toolset, "companion_tools", {}),
        environments = environments,
        requirements_by_role = requirements_by_role,
        make_variables = make_variables,
        tools = tools,
    )

def linux_test_with_rust_toolchain(scope, toolset, rust, bindgen = None):
    return _with_rust_toolchain(scope, toolset, rust, bindgen)

def _canonical_rust_source_root(workspace_root, package, rustc_srcs_path):
    components = [
        component
        for component in [workspace_root, package, rustc_srcs_path]
        if component and component != "."
    ]
    if not components:
        fail("Rust source toolchain has an empty canonical source root")
    return _canonical_artifact_path("/".join(components), "Rust source toolchain root")

def linux_test_canonical_rust_source_root(workspace_root, package, rustc_srcs_path):
    return _canonical_rust_source_root(workspace_root, package, rustc_srcs_path)

def _rust_source_selection(source_toolchain):
    rustc_srcs = source_toolchain.rustc_srcs
    files = {}
    for file in rustc_srcs[DefaultInfo].files.to_list():
        _record_canonical_file(files, file, "Rust source toolchain closure")
    root = _canonical_rust_source_root(
        rustc_srcs.label.workspace_root,
        rustc_srcs.label.package,
        source_toolchain.rustc_srcs_path,
    )
    prefix = root + "/"
    if not any([filename == root or filename.startswith(prefix) for filename in files]):
        fail("Rust source toolchain root %r contains no files from its rustc_srcs target" % root)
    return struct(
        files = depset(files.values()),
        paths = sorted(files),
        root = root,
    )

def _toolset_identity(ctx, scope, toolset, closure):
    manifest = ctx.actions.declare_file(ctx.label.name + ".toolset-" + scope + ".json")
    identity = ctx.actions.declare_directory(ctx.label.name + ".toolset-" + scope)
    closure_files = closure.to_list()
    closure_by_path = {}
    for file in closure_files:
        _record_canonical_file(closure_by_path, file, "%s toolset closure" % scope)
    path_index = _toolchain_action_path_index_from_list(closure_files)
    canonical_actions = {}
    for role in sorted(toolset.arguments):
        canonical_actions[role] = [
            _canonicalize_toolchain_action_value(value, path_index)
            for value in toolset.arguments[role]
        ]
    canonical_environments = {}
    for role in sorted(toolset.environments):
        canonical_environments[role] = {
            name: _canonicalize_toolchain_action_value(toolset.environments[role][name], path_index)
            for name in sorted(toolset.environments[role])
        }
    artifact_kinds = {}
    for path, file in closure_by_path.items():
        # File.is_directory only identifies declared TreeArtifacts/Filesets.
        # Bazel's legacy opaque source directories are ordinary SourceArtifacts
        # here, so their identity-bound kind must preserve that ambiguity for
        # execution-time inspection of the exact typed input.
        artifact_kinds[path] = "source-artifact" if file.is_source else ("generated-directory" if file.is_directory else "generated-file")
    tool_paths = {}
    for role, tool in toolset.tools.items():
        tool_path = _canonical_file_path(_tool_executable(tool), "%s %s tool" % (scope, role))
        if tool_path not in closure_by_path:
            fail("%s %s tool %r is absent from its declared toolchain closure" % (scope, role, tool_path))
        tool_paths[role] = tool_path
    for role, companions in getattr(toolset, "companion_tools", {}).items():
        if role not in toolset.tools:
            fail("%s companion tools reference unknown role %r" % (scope, role))
        for index, companion in enumerate(companions):
            companion_path = _canonical_file_path(
                _tool_executable(companion),
                "%s %s companion tool %d" % (scope, role, index),
            )
            if companion_path not in closure_by_path:
                fail("%s %s companion tool %r is absent from its declared toolchain closure" % (scope, role, companion_path))
    ctx.actions.write(
        output = manifest,
        content = json.encode({
            "actions": canonical_actions,
            "artifact_kinds": artifact_kinds,
            "closure": sorted(closure_by_path),
            "environments": canonical_environments,
            "make_variables": toolset.make_variables,
            "requirements": toolset.requirements_by_role,
            "schema": _TOOLSET_SCHEMA,
            "scope": scope,
            "tools": tool_paths,
        }) + "\n",
    )
    args = ctx.actions.args()
    args.add("-manifest", manifest)
    _add_artifact_path(args, "-out", identity)
    ctx.actions.run(
        executable = ctx.executable._toolsetidentity,
        inputs = depset(direct = [manifest], transitive = [closure]),
        outputs = [identity],
        arguments = [args],
        execution_requirements = {"supports-path-mapping": "1"},
        mnemonic = "LinuxToolsetIdentity",
        progress_message = "Identifying %s Linux toolset %%{label}" % scope,
    )
    return struct(identity = identity, manifest = manifest)

def linux_map_directory_params(
        stage,
        source_prefix,
        target_action_args,
        target_action_environments,
        host_action_args,
        host_action_environments,
        input_tree_aliases = {},
        output_tree_bases = {}):
    params = {"source_prefix": source_prefix, "stage": stage}
    for tree, directory_name in input_tree_aliases.items():
        if tree not in _TREES or not _valid_name(directory_name):
            fail("mapped Linux %s stage has invalid input-tree alias %r=%r" % (stage, tree, directory_name))
        params["input_tree_alias_" + tree] = directory_name
    for tree, directory_name in output_tree_bases.items():
        if tree not in _TREES or not _valid_name(directory_name):
            fail("mapped Linux %s stage has invalid output-tree base %r=%r" % (stage, tree, directory_name))
        params["output_tree_base_" + tree] = directory_name
    for scope, action_args, action_environments in [
        ("target", target_action_args, target_action_environments),
        ("host", host_action_args, host_action_environments),
    ]:
        for role, argv in action_args.items():
            binding = _scoped_tool_binding(scope, role)
            params["action_arg_count_" + binding] = str(len(argv))
            for index, argument in enumerate(argv):
                params["action_arg_%s_%d" % (binding, index)] = argument
            environment = action_environments.get(role, {})
            params["action_env_count_" + binding] = str(len(environment))
            for index, name in enumerate(sorted(environment)):
                params["action_env_%s_%d" % (binding, index)] = name + "=" + environment[name]
    return params

def linux_map_directory_tools(
        runner,
        current_scope,
        target_tool_files,
        target_toolchain_files,
        host_tool_files,
        host_toolchain_files,
        target_companion_tools = {},
        host_companion_tools = {}):
    if current_scope not in ["host", "target"]:
        fail("mapped Linux tools have invalid current scope %r" % current_scope)
    values = {
        "runner": runner,
        _TOOLCHAIN_FILES_PREFIX + "target": target_toolchain_files,
        _TOOLCHAIN_FILES_PREFIX + "host": host_toolchain_files,
    }
    for scope, tool_files, companion_tools in [
        ("target", target_tool_files, target_companion_tools),
        ("host", host_tool_files, host_companion_tools),
    ]:
        for role, tool in tool_files.items():
            binding = _scoped_tool_binding(scope, role)
            values[binding] = tool
            if scope == current_scope:
                values[role] = tool
        for role in sorted(companion_tools):
            if role not in tool_files:
                fail("mapped Linux %s companion tools reference unknown role %r" % (scope, role))
            if not _valid_name(role):
                fail("mapped Linux %s companion tools have invalid role %r" % (scope, role))
            binding = _scoped_tool_binding(scope, role)
            for index, companion in enumerate(companion_tools[role]):
                values[_COMPANION_TOOL_PREFIX + binding + "_" + _ordinal(index)] = companion
    return values

def _project(ctx, tree, path, output, manifest = None):
    args = ctx.actions.args()
    args.add("-copy_tree_file")
    _add_artifact_path(args, "-tree", tree)
    args.add("-path", path)
    inputs = [tree]
    if manifest != None:
        _add_artifact_path(args, "-manifest", manifest)
        inputs.append(manifest)
    _add_artifact_path(args, "-output", output)
    ctx.actions.run(
        executable = ctx.executable._recipe_runner,
        inputs = inputs,
        outputs = [output],
        arguments = [args],
        mnemonic = "LinuxMappedProjection",
    )

def _stage_resolved_object_tree(ctx, resolved, auto_conf, auto_conf_cmd, autoconf, rustc_cfg, kernel_release):
    """Creates the immutable config/object view available before host actions."""
    tree = ctx.actions.declare_directory(ctx.label.name + ".tree-prep-base")
    copies = {
        ".config": resolved,
        "include/config/auto.conf": auto_conf,
        "include/config/auto.conf.cmd": auto_conf_cmd,
        "include/config/kernel.release": kernel_release,
        "include/generated/autoconf.h": autoconf,
        "include/generated/rustc_cfg": rustc_cfg,
    }
    args = ctx.actions.args()
    _add_artifact_path(args, "-tree_out", tree)
    for destination in sorted(copies):
        _add_artifact_path(args, "-copy", copies[destination], format = destination + "=%s")
    ctx.actions.run(
        executable = ctx.executable._actionfile,
        inputs = depset(copies.values()),
        outputs = [tree],
        arguments = [args],
        execution_requirements = {"supports-path-mapping": "1"},
        mnemonic = "LinuxMappedPrepBase",
        progress_message = "Staging resolved Linux pre-host object tree %{label}",
    )
    return tree

def _linux_mapped_kernel_impl(ctx):
    validate_linux_module_make_vars(ctx.attr.module_make_vars, str(ctx.label))
    target_execution_platform = linux_execution_platform_label(ctx.attr._target_execution_platform)
    host_execution_platform = linux_execution_platform_label(ctx.attr._host_execution_platform)
    if target_execution_platform != host_execution_platform:
        fail("%s requires target and host tools on one execution platform for source-selected mixed-tool actions; got target %s and host %s" % (
            ctx.label,
            target_execution_platform,
            host_execution_platform,
        ))
    target_cc = find_cpp_toolchain(ctx)
    host_cc = host_cc_toolchain(ctx)
    target_rust = target_rust_toolchain(ctx)
    host_rust = execution_rust_toolchain(ctx, "host_cc")
    rust_source_toolchain = execution_rust_source_toolchain(ctx, "host_cc")
    bindgen = execution_bindgen_toolchain(ctx, "host_cc")
    host_dependencies = _stage_host_cc_dependencies(ctx, {
        "libcrypto": ctx.attr._libcrypto,
        "libelf": ctx.attr._libelf,
    })
    libcrypto = host_dependencies.packages["libcrypto"]
    libelf = host_dependencies.packages["libelf"]
    pkg_config_manifest = _write_pkg_config_manifest(ctx, {
        "libcrypto": libcrypto,
        "libelf": libelf,
    })

    # Target compiler probes still execute on the selected execution platform.
    # Python's target-runtime toolchain would select an ARM interpreter for an
    # ARM kernel, so bind both scopes through rules_python's exec-tools type.
    target_python = _execution_python_runtime(ctx.toolchains[_PYTHON_EXEC_TOOLS_TOOLCHAIN_TYPE], "target")
    host_python = _execution_python_runtime(ctx.exec_groups["host_cc"].toolchains[_PYTHON_EXEC_TOOLS_TOOLCHAIN_TYPE], "host")
    target_script_runtime = script_runtime_toolchain(ctx)
    host_script_runtime = script_runtime_toolchain(ctx, "host_cc")
    target_perl = ctx.toolchains[_PERL_TOOLCHAIN_TYPE].perl_runtime
    host_perl = ctx.exec_groups["host_cc"].toolchains[_PERL_TOOLCHAIN_TYPE].perl_runtime
    rust_source = _rust_source_selection(rust_source_toolchain) if rust_source_toolchain != None else None
    target = _kbuild_toolset(ctx, target_cc, "target")
    target_link_runtime_files = target.link_runtime_files
    target = _with_rust_toolchain(
        "target",
        _with_auxiliary_tools(
            target,
            "target",
            {
                "awk": ctx.executable._target_awk,
                "lz4": ctx.executable._lz4,
                "pahole": ctx.executable._pahole,
                "python3": target_python.interpreter,
            },
            make_variables = {
                "AWK": "awk",
                "LZ4": "lz4",
                "PAHOLE": "pahole",
                "PYTHON3": "python3",
            },
        ),
        target_rust,
        bindgen,
    )
    host_exec_group = ctx.exec_groups["host_cc"]
    bison = bison_toolchain(host_exec_group)
    flex = flex_toolchain(host_exec_group)
    m4 = m4_toolchain(host_exec_group)

    host = _kbuild_toolset(
        ctx,
        host_cc,
        "host",
        additional_compile_flags = libelf.action_compile_flags,
    )
    host_link_runtime_files = host.link_runtime_files
    host = _with_pkg_config(
        _with_rust_toolchain(
            "host",
            _with_auxiliary_tools(
                _with_host_generators(
                    host,
                    bison,
                    flex,
                    m4,
                    ctx.attr._bison_m4_deny_shell[DefaultInfo].files_to_run,
                    ctx.attr._flex_m4_deny_shell[DefaultInfo].files_to_run,
                    ctx.attr._m4_deny_shell[DefaultInfo].files_to_run,
                ),
                "host",
                {"awk": ctx.executable._host_awk, "python3": host_python.interpreter},
                make_variables = {
                    "AWK": "awk",
                    "PYTHON3": "python3",
                },
            ),
            host_rust,
        ),
        ctx.executable._pkg_config,
        pkg_config_manifest,
    )
    target_script_tools = {
        role: getattr(ctx.executable, attribute)
        for role, attribute in _TARGET_PLANNER_HELPER_ATTRS.items()
    }
    host_script_tools = {
        role: getattr(ctx.executable, "_host" + attribute)
        for role, attribute in _SHARED_PLANNER_HELPER_ATTRS.items()
    }
    target = _with_auxiliary_tools(target, "target script", target_script_tools)
    host = _with_auxiliary_tools(host, "host script", host_script_tools)
    target = _with_script_runtime(target, "target", target_script_runtime)
    host = _with_script_runtime(host, "host", host_script_runtime)
    target = _with_script_applet(
        target,
        "target script",
        "perl",
        target_perl.interpreter,
    )
    host = _with_script_applet(
        host,
        "host script",
        "perl",
        host_perl.interpreter,
    )
    target_base_closures = [
        target_cc.all_files,
        target_link_runtime_files,
        depset([libelf.tree]),
        _executable_label_closure(ctx.attr._target_awk),
        _executable_label_closure(ctx.attr._lz4),
        _executable_label_closure(ctx.attr._pahole),
        target_python.files,
        target_script_runtime.files,
        target_perl.runtime,
        _executable_label_closure(ctx.attr._probe_runner),
        _executable_label_closure(ctx.attr._recipe_runner),
    ] + [
        _executable_label_closure(getattr(ctx.attr, attribute))
        for attribute in _TARGET_PLANNER_HELPER_ATTRS.values()
    ]
    host_base_closures = [
        host_cc.all_files,
        host_link_runtime_files,
        depset([libelf.tree, pkg_config_manifest]),
        bison.all_files,
        flex.all_files,
        m4.all_files,
        _executable_label_closure(ctx.attr._bison_m4_deny_shell),
        _executable_label_closure(ctx.attr._flex_m4_deny_shell),
        _executable_label_closure(ctx.attr._m4_deny_shell),
        _executable_label_closure(ctx.attr._host_awk),
        _executable_label_closure(ctx.attr._pkg_config),
        host_python.files,
        host_script_runtime.files,
        host_perl.runtime,
        _executable_label_closure(ctx.attr._host_probe_runner),
        _executable_label_closure(ctx.attr._host_recipe_runner),
    ] + [
        _executable_label_closure(getattr(ctx.attr, "_host" + attribute))
        for attribute in _SHARED_PLANNER_HELPER_ATTRS.values()
    ]
    target_rust_closures = []
    if target_rust != None and getattr(target_rust, "all_files", None) != None:
        target_rust_closures.append(target_rust.all_files)
    target_bindgen = getattr(bindgen, "bindgen", None) if bindgen != None else None
    target_rust_files = _filtered_toolchain_closure(
        target_base_closures,
        depset(
            direct = [target_bindgen] if target_bindgen != None else [],
            transitive = target_rust_closures,
        ),
        "target Rust",
    ) if target_rust_closures or target_bindgen != None else depset()
    host_rust_files = _filtered_toolchain_closure(
        host_base_closures,
        host_rust.all_files,
        "host Rust",
    ) if host_rust != None and getattr(host_rust, "all_files", None) != None else depset()
    host_toolchain_files = depset(transitive = host_base_closures + [host_rust_files])
    target_toolchain_files = depset(transitive = target_base_closures + [target_rust_files])
    target_toolset_contract = _toolset_identity(ctx, "target", target, target_toolchain_files)
    host_toolset_contract = _toolset_identity(ctx, "host", host, host_toolchain_files)
    target_toolset_identity = target_toolset_contract.identity
    host_toolset_identity = host_toolset_contract.identity
    target_toolset_manifest = target_toolset_contract.manifest
    host_toolset_manifest = host_toolset_contract.manifest
    target_tools = dict(target.tools)
    host_tools = dict(host.tools)
    source_prefix = ctx.file.source_root.short_path.rsplit("/", 1)[0] if "/" in ctx.file.source_root.short_path else ""
    rust_source_root = rust_source.root if rust_source != None else ""
    probe_source_inputs = {
        "source_files": depset(ctx.files.source_files),
        "source_root": ctx.file.source_root,
    }
    if rust_source != None:
        probe_source_inputs["rust_source_files"] = rust_source.files

    target_requirements = linux_toolset_execution_requirements(target.requirements_by_role, "target")
    host_requirements = linux_toolset_execution_requirements(host.requirements_by_role, "host")

    # Compiler identity is execution data, not analysis-time configuration and
    # not something the planner may discover by executing tool paths itself.
    # First emit the architecture-independent request DAG using only the two
    # independently measured toolset identities. Bazel 9 then expands that DAG
    # under each toolset's real execution group before the Kconfig/Kbuild
    # planner is allowed to consume the canonical result trees.
    probe_plan = ctx.actions.declare_directory(ctx.label.name + ".probe-plan")
    host_probe_results = ctx.actions.declare_directory(ctx.label.name + ".probe-results-host")
    target_probe_results = ctx.actions.declare_directory(ctx.label.name + ".probe-results-target")
    probe_plan_args = ctx.actions.args()
    _add_artifact_path(probe_plan_args, "-target_toolset_identity", target_toolset_identity)
    _add_artifact_path(probe_plan_args, "-host_toolset_identity", host_toolset_identity)
    _add_artifact_path(probe_plan_args, "-probe_plan_out", probe_plan)
    ctx.actions.run(
        executable = ctx.executable._planner,
        inputs = [target_toolset_identity, host_toolset_identity],
        outputs = [probe_plan],
        arguments = [probe_plan_args],
        execution_requirements = {"supports-path-mapping": "1"},
        mnemonic = "LinuxProbePlan",
        progress_message = "Planning Linux compiler discovery %{label}",
    )
    ctx.actions.map_directory(
        implementation = expand_linux_probe_plan,
        input_directories = {
            "host_toolset_identity": host_toolset_identity,
            "plan": probe_plan,
        },
        output_directories = {"results": host_probe_results},
        tools = linux_probe_map_directory_tools(
            ctx.attr._host_probe_runner[DefaultInfo].files_to_run,
            host.tools,
            host_toolchain_files,
            host_toolset_manifest,
            host.companion_tools,
        ),
        additional_params = linux_probe_map_directory_params("host", host.arguments, host.environments),
        env = {},
        execution_requirements = dict(host_requirements, **{"supports-path-mapping": "1"}),
        exec_group = "host_cc",
        mnemonic = "LinuxMappedHostProbe",
    )
    ctx.actions.map_directory(
        implementation = expand_linux_probe_plan,
        input_directories = {
            "host_results": host_probe_results,
            "host_toolset_identity": host_toolset_identity,
            "plan": probe_plan,
            "target_toolset_identity": target_toolset_identity,
        },
        output_directories = {"results": target_probe_results},
        tools = linux_probe_map_directory_tools(
            ctx.attr._probe_runner[DefaultInfo].files_to_run,
            target.tools,
            target_toolchain_files,
            target_toolset_manifest,
            target.companion_tools,
        ),
        additional_params = linux_probe_map_directory_params("target", target.arguments, target.environments),
        env = {},
        execution_requirements = dict(target_requirements, **{"supports-path-mapping": "1"}),
        mnemonic = "LinuxMappedTargetProbe",
        toolchain = CC_TOOLCHAIN_TYPE,
    )

    # Bootstrap results select the source-derived architecture and make the
    # configured compiler identity concrete. A second planner invocation then
    # parses the actual Kconfig with a symbolic evaluator: it receives exact
    # action tokens as inert strings, but no compiler executable or toolchain
    # closure, so capability discovery cannot accidentally execute or stat a
    # compiler. The emitted DAG is expanded in host-then-target order under
    # each configured toolchain.
    kconfig_probe_plan = ctx.actions.declare_directory(ctx.label.name + ".kconfig-probe-plan")
    host_kconfig_probe_results = ctx.actions.declare_directory(ctx.label.name + ".kconfig-probe-results-host")
    target_kconfig_probe_results = ctx.actions.declare_directory(ctx.label.name + ".kconfig-probe-results-target")
    kconfig_probe_args = ctx.actions.args()
    kconfig_probe_args.add("-root", ctx.file.source_root)
    kconfig_probe_args.add("-srctree", ctx.file.source_root)
    _add_artifact_path(kconfig_probe_args, "-target_toolset_identity", target_toolset_identity)
    _add_artifact_path(kconfig_probe_args, "-host_toolset_identity", host_toolset_identity)
    _add_artifact_path(kconfig_probe_args, "-target_toolset_manifest", target_toolset_manifest)
    _add_artifact_path(kconfig_probe_args, "-host_toolset_manifest", host_toolset_manifest)
    _add_artifact_path(kconfig_probe_args, "-target_probe_results", target_probe_results)
    _add_artifact_path(kconfig_probe_args, "-host_probe_results", host_probe_results)
    _add_artifact_path(kconfig_probe_args, "-kconfig_probe_plan_out", kconfig_probe_plan)
    _add_host_dependency_variables(kconfig_probe_args, libelf)
    if rust_source != None:
        kconfig_probe_args.add("-var", "RUST_LIB_SRC=" + rust_source.root)
        kconfig_probe_args.add("-source_root_map", rust_source.root + "=" + rust_source.root)
    for name, value in ctx.attr.module_make_vars.items():
        kconfig_probe_args.add("-var", name + "=" + value)
    kconfig_probe_inputs = depset(
        direct = ctx.files.source_files + [
            ctx.file.source_root,
            target_toolset_identity,
            host_toolset_identity,
            target_toolset_manifest,
            host_toolset_manifest,
            target_probe_results,
            host_probe_results,
        ],
        transitive = [rust_source.files] if rust_source != None else [],
    )
    ctx.actions.run(
        executable = ctx.executable._planner,
        inputs = kconfig_probe_inputs,
        outputs = [kconfig_probe_plan],
        arguments = [kconfig_probe_args],
        execution_requirements = {"supports-path-mapping": "1"},
        mnemonic = "LinuxKconfigProbePlan",
        progress_message = "Planning Linux Kconfig capability discovery %{label}",
    )
    ctx.actions.map_directory(
        implementation = expand_linux_probe_plan,
        input_directories = {
            _HOST_DEPS_TREE: libelf.tree,
            "host_toolset_identity": host_toolset_identity,
            "plan": kconfig_probe_plan,
        },
        additional_inputs = probe_source_inputs,
        output_directories = {"results": host_kconfig_probe_results},
        tools = linux_probe_map_directory_tools(
            ctx.attr._host_probe_runner[DefaultInfo].files_to_run,
            host.tools,
            host_toolchain_files,
            host_toolset_manifest,
            host.companion_tools,
        ),
        additional_params = linux_probe_map_directory_params(
            "host",
            host.arguments,
            host.environments,
            source_prefix = source_prefix,
            rust_source_root = rust_source_root,
        ),
        env = {},
        execution_requirements = dict(host_requirements, **{"supports-path-mapping": "1"}),
        exec_group = "host_cc",
        mnemonic = "LinuxMappedHostKconfigProbe",
    )
    ctx.actions.map_directory(
        implementation = expand_linux_probe_plan,
        input_directories = {
            _HOST_DEPS_TREE: libelf.tree,
            "host_results": host_kconfig_probe_results,
            "host_toolset_identity": host_toolset_identity,
            "plan": kconfig_probe_plan,
            "target_toolset_identity": target_toolset_identity,
        },
        additional_inputs = probe_source_inputs,
        output_directories = {"results": target_kconfig_probe_results},
        tools = linux_probe_map_directory_tools(
            ctx.attr._probe_runner[DefaultInfo].files_to_run,
            target.tools,
            target_toolchain_files,
            target_toolset_manifest,
            target.companion_tools,
        ),
        additional_params = linux_probe_map_directory_params(
            "target",
            target.arguments,
            target.environments,
            source_prefix = source_prefix,
            rust_source_root = rust_source_root,
        ),
        env = {},
        execution_requirements = dict(target_requirements, **{"supports-path-mapping": "1"}),
        mnemonic = "LinuxMappedTargetKconfigProbe",
        toolchain = CC_TOOLCHAIN_TYPE,
    )

    # Kconfig replay fixes the exact selected configuration.  Kbuild is then
    # evaluated symbolically in a third planner invocation so every cc-option,
    # as-option, ld-option, source try-run, and compiler-derived shell query is
    # an execution action under the selected target or host toolchain.  Like
    # Kconfig discovery, this planner receives action paths only as inert
    # strings and has neither compiler binaries nor toolchain closures.
    kbuild_probe_plan = ctx.actions.declare_directory(ctx.label.name + ".kbuild-probe-plan")
    host_kbuild_probe_results = ctx.actions.declare_directory(ctx.label.name + ".kbuild-probe-results-host")
    target_kbuild_probe_results = ctx.actions.declare_directory(ctx.label.name + ".kbuild-probe-results-target")
    kbuild_probe_args = ctx.actions.args()
    kbuild_probe_args.add("-root", ctx.file.source_root)
    kbuild_probe_args.add("-srctree", ctx.file.source_root)
    kbuild_probe_args.add("-kbuild", ctx.file.kbuild)
    _add_artifact_path(kbuild_probe_args, "-resolve_config", ctx.file.config)
    if ctx.file.overlay:
        kbuild_probe_args.add("-resolve_config_overlay", ctx.file.overlay)
    kbuild_probe_args.add("-config_mode", ctx.attr.config_mode)
    kbuild_probe_args.add("-kernel_version", ctx.attr.version)
    _add_kernel_kbuild_goals(kbuild_probe_args)
    _add_artifact_path(kbuild_probe_args, "-target_toolset_identity", target_toolset_identity)
    _add_artifact_path(kbuild_probe_args, "-host_toolset_identity", host_toolset_identity)
    _add_artifact_path(kbuild_probe_args, "-target_toolset_manifest", target_toolset_manifest)
    _add_artifact_path(kbuild_probe_args, "-host_toolset_manifest", host_toolset_manifest)
    _add_artifact_path(kbuild_probe_args, "-target_probe_results", target_probe_results)
    _add_artifact_path(kbuild_probe_args, "-host_probe_results", host_probe_results)
    _add_artifact_path(kbuild_probe_args, "-host_kconfig_probe_results", host_kconfig_probe_results)
    _add_artifact_path(kbuild_probe_args, "-target_kconfig_probe_results", target_kconfig_probe_results)
    _add_artifact_path(kbuild_probe_args, "-kbuild_probe_plan_out", kbuild_probe_plan)
    _add_host_dependency_variables(kbuild_probe_args, libelf)
    if rust_source != None:
        kbuild_probe_args.add("-var", "RUST_LIB_SRC=" + rust_source.root)
        kbuild_probe_args.add("-source_root_map", rust_source.root + "=" + rust_source.root)
    for name, value in ctx.attr.module_make_vars.items():
        kbuild_probe_args.add("-var", name + "=" + value)
    kbuild_probe_inputs = ctx.files.source_files + [
        ctx.file.config,
        ctx.file.kbuild,
        ctx.file.source_root,
        target_toolset_identity,
        host_toolset_identity,
        target_toolset_manifest,
        host_toolset_manifest,
        target_probe_results,
        host_probe_results,
        host_kconfig_probe_results,
        target_kconfig_probe_results,
    ]
    if ctx.file.overlay:
        kbuild_probe_inputs.append(ctx.file.overlay)
    kbuild_probe_inputs = depset(
        direct = kbuild_probe_inputs,
        transitive = [rust_source.files] if rust_source != None else [],
    )
    ctx.actions.run(
        executable = ctx.executable._planner,
        inputs = kbuild_probe_inputs,
        outputs = [kbuild_probe_plan],
        arguments = [kbuild_probe_args],
        execution_requirements = {"supports-path-mapping": "1"},
        mnemonic = "LinuxKbuildProbePlan",
        progress_message = "Planning Linux Kbuild capability discovery %{label}",
    )
    ctx.actions.map_directory(
        implementation = expand_linux_probe_plan,
        input_directories = {
            _HOST_DEPS_TREE: libelf.tree,
            "host_toolset_identity": host_toolset_identity,
            "plan": kbuild_probe_plan,
        },
        additional_inputs = probe_source_inputs,
        output_directories = {"results": host_kbuild_probe_results},
        tools = linux_probe_map_directory_tools(
            ctx.attr._host_probe_runner[DefaultInfo].files_to_run,
            host.tools,
            host_toolchain_files,
            host_toolset_manifest,
            host.companion_tools,
        ),
        additional_params = linux_probe_map_directory_params(
            "host",
            host.arguments,
            host.environments,
            source_prefix = source_prefix,
            rust_source_root = rust_source_root,
        ),
        env = {},
        execution_requirements = dict(host_requirements, **{"supports-path-mapping": "1"}),
        exec_group = "host_cc",
        mnemonic = "LinuxMappedHostKbuildProbe",
    )
    ctx.actions.map_directory(
        implementation = expand_linux_probe_plan,
        input_directories = {
            _HOST_DEPS_TREE: libelf.tree,
            "host_results": host_kbuild_probe_results,
            "host_toolset_identity": host_toolset_identity,
            "plan": kbuild_probe_plan,
            "target_toolset_identity": target_toolset_identity,
        },
        additional_inputs = probe_source_inputs,
        output_directories = {"results": target_kbuild_probe_results},
        tools = linux_probe_map_directory_tools(
            ctx.attr._probe_runner[DefaultInfo].files_to_run,
            target.tools,
            target_toolchain_files,
            target_toolset_manifest,
            target.companion_tools,
        ),
        additional_params = linux_probe_map_directory_params(
            "target",
            target.arguments,
            target.environments,
            source_prefix = source_prefix,
            rust_source_root = rust_source_root,
        ),
        env = {},
        execution_requirements = dict(target_requirements, **{"supports-path-mapping": "1"}),
        mnemonic = "LinuxMappedTargetKbuildProbe",
        toolchain = CC_TOOLCHAIN_TYPE,
    )

    plans = {
        stage: ctx.actions.declare_directory(ctx.label.name + ".plan-v4-" + stage)
        for stage in _STAGES
    }
    arch = ctx.actions.declare_file(ctx.label.name + ".arch")
    resolved = ctx.actions.declare_file(ctx.label.name + ".config")
    auto_conf = ctx.actions.declare_file(ctx.label.name + ".auto.conf")
    auto_conf_cmd = ctx.actions.declare_file(ctx.label.name + ".auto.conf.cmd")
    autoconf = ctx.actions.declare_file(ctx.label.name + ".autoconf.h")
    rustc_cfg = ctx.actions.declare_file(ctx.label.name + ".rustc_cfg")
    kernel_release = ctx.actions.declare_file(ctx.label.name + ".kernel.release")
    output_trees = {key: ctx.actions.declare_directory(ctx.label.name + ".tree-" + key) for key in _TREES}
    work_trees = {stage: ctx.actions.declare_directory(ctx.label.name + ".tree-work-" + stage) for stage in _STAGES}

    args = ctx.actions.args()
    args.add("-root", ctx.file.source_root)

    # Pass the root File itself so output-path mapping remains active. The
    # planner normalizes a regular-file srctree argument to its parent.
    args.add("-srctree", ctx.file.source_root)
    args.add("-kbuild", ctx.file.kbuild)
    _add_artifact_path(args, "-resolve_config", ctx.file.config)
    if ctx.file.overlay:
        args.add("-resolve_config_overlay", ctx.file.overlay)
    args.add("-config_mode", ctx.attr.config_mode)
    args.add("-resolved_arch_out", arch)
    args.add("-resolved_config_out", resolved)
    args.add("-resolved_auto_conf_out", auto_conf)
    args.add("-resolved_auto_conf_cmd_out", auto_conf_cmd)
    args.add("-resolved_autoconf_out", autoconf)
    args.add("-resolved_rustc_cfg_out", rustc_cfg)
    args.add("-resolved_kernel_release_out", kernel_release)
    args.add("-kernel_version", ctx.attr.version)
    _add_kernel_kbuild_goals(args)
    _add_artifact_path(args, "-target_toolset_identity", target_toolset_identity)
    _add_artifact_path(args, "-host_toolset_identity", host_toolset_identity)
    _add_artifact_path(args, "-target_toolset_manifest", target_toolset_manifest)
    _add_artifact_path(args, "-host_toolset_manifest", host_toolset_manifest)
    _add_artifact_path(args, "-target_probe_results", target_probe_results)
    _add_artifact_path(args, "-host_probe_results", host_probe_results)
    _add_artifact_path(args, "-host_kconfig_probe_results", host_kconfig_probe_results)
    _add_artifact_path(args, "-target_kconfig_probe_results", target_kconfig_probe_results)
    _add_artifact_path(args, "-host_kbuild_probe_results", host_kbuild_probe_results)
    _add_artifact_path(args, "-target_kbuild_probe_results", target_kbuild_probe_results)
    _add_host_dependency_variables(args, libelf)
    if rust_source != None:
        # Kbuild consumes the source root as a normal make variable.  This
        # canonical path comes from the selected source toolchain's provider
        # contract, not from rustc's ambient sysroot.
        args.add("-var", "RUST_LIB_SRC=" + rust_source.root)
        args.add("-source_root_map", rust_source.root + "=" + rust_source.root)
    for stage in _STAGES:
        _add_artifact_path(
            args,
            "-action_plan_stage_out",
            plans[stage],
            format = stage + "=%s",
        )
    for name, value in ctx.attr.module_make_vars.items():
        args.add("-var", name + "=" + value)

    planner_inputs = ctx.files.source_files + [
        ctx.file.config,
        ctx.file.kbuild,
        ctx.file.source_root,
        target_toolset_identity,
        host_toolset_identity,
        target_toolset_manifest,
        host_toolset_manifest,
        target_probe_results,
        host_probe_results,
        host_kconfig_probe_results,
        target_kconfig_probe_results,
        host_kbuild_probe_results,
        target_kbuild_probe_results,
    ]
    if ctx.file.overlay:
        planner_inputs.append(ctx.file.overlay)
    planner_inputs = depset(
        direct = planner_inputs,
        transitive = [rust_source.files] if rust_source != None else [],
    )
    planner_outputs = [plans[stage] for stage in _STAGES] + [arch, resolved, auto_conf, auto_conf_cmd, autoconf, rustc_cfg, kernel_release]
    ctx.actions.run(
        executable = ctx.executable._planner,
        inputs = planner_inputs,
        outputs = planner_outputs,
        arguments = [args],
        execution_requirements = {"supports-path-mapping": "1"},
        mnemonic = "LinuxMappedPlan",
        progress_message = "Resolving Kconfig and planning Kbuild %{label}",
    )

    prep_base = _stage_resolved_object_tree(
        ctx,
        resolved,
        auto_conf,
        auto_conf_cmd,
        autoconf,
        rustc_cfg,
        kernel_release,
    )

    common_inputs = {
        _HOST_DEPS_TREE: libelf.tree,
        "host_toolset_identity": host_toolset_identity,
        "prep_base": prep_base,
        "target_toolset_identity": target_toolset_identity,
    }
    map_inputs = {
        "auto_conf": auto_conf,
        "auto_conf_cmd": auto_conf_cmd,
        "autoconf": autoconf,
        "kernel_release": kernel_release,
        "resolved_config": resolved,
        "rustc_cfg": rustc_cfg,
        "rust_source_files": rust_source.files if rust_source != None else depset(),
        "source_files": depset(ctx.files.source_files),
        "source_root": ctx.file.source_root,
    }

    def mapped_tools(runner, scope):
        return linux_map_directory_tools(
            runner,
            scope,
            target_tools,
            target_toolchain_files,
            host_tools,
            host_toolchain_files,
            target.companion_tools,
            host.companion_tools,
        )

    def mapped_params(stage, input_tree_aliases = {}, output_tree_bases = {}):
        return linux_map_directory_params(
            stage,
            source_prefix,
            target.arguments,
            target.environments,
            host.arguments,
            host.environments,
            input_tree_aliases = input_tree_aliases,
            output_tree_bases = output_tree_bases,
        )

    mapped_requirements = _merge_execution_requirements(
        "target toolset",
        target_requirements,
        [("host toolset", host_requirements)],
    )
    mapped_requirements["supports-path-mapping"] = "1"
    ctx.actions.map_directory(
        implementation = expand_linux_plan_stage,
        input_directories = dict(common_inputs, plan = plans["prehost"]),
        additional_inputs = map_inputs,
        output_directories = {"prehost": output_trees["prehost"], "work": work_trees["prehost"]},
        tools = mapped_tools(ctx.attr._host_recipe_runner[DefaultInfo].files_to_run, "host"),
        additional_params = mapped_params(
            "prehost",
            input_tree_aliases = {"prep": "prep_base"},
        ),
        env = {},
        execution_requirements = mapped_requirements,
        exec_group = "host_cc",
        mnemonic = "LinuxMappedPrehost",
    )
    ctx.actions.map_directory(
        implementation = expand_linux_plan_stage,
        input_directories = dict(common_inputs, plan = plans["bootstrap"], prehost = output_trees["prehost"]),
        additional_inputs = map_inputs,
        output_directories = {"bootstrap": output_trees["bootstrap"], "work": work_trees["bootstrap"]},
        tools = mapped_tools(ctx.attr._recipe_runner[DefaultInfo].files_to_run, "target"),
        additional_params = mapped_params(
            "bootstrap",
            input_tree_aliases = {"prep": "prep_base"},
        ),
        env = {},
        execution_requirements = mapped_requirements,
        mnemonic = "LinuxMappedBootstrap",
        toolchain = CC_TOOLCHAIN_TYPE,
    )
    ctx.actions.map_directory(
        implementation = expand_linux_plan_stage,
        input_directories = dict(common_inputs, plan = plans["host"], prehost = output_trees["prehost"], bootstrap = output_trees["bootstrap"]),
        additional_inputs = map_inputs,
        output_directories = {"host": output_trees["host"], "work": work_trees["host"]},
        tools = mapped_tools(ctx.attr._host_recipe_runner[DefaultInfo].files_to_run, "host"),
        additional_params = mapped_params(
            "host",
            input_tree_aliases = {"prep": "prep_base"},
        ),
        env = {},
        execution_requirements = mapped_requirements,
        exec_group = "host_cc",
        mnemonic = "LinuxMappedHost",
    )
    ctx.actions.map_directory(
        implementation = expand_linux_plan_stage,
        input_directories = dict(common_inputs, plan = plans["prep"], prehost = output_trees["prehost"], bootstrap = output_trees["bootstrap"], host = output_trees["host"]),
        additional_inputs = map_inputs,
        output_directories = {"prep": output_trees["prep"], "work": work_trees["prep"]},
        tools = mapped_tools(ctx.attr._recipe_runner[DefaultInfo].files_to_run, "target"),
        additional_params = mapped_params(
            "prep",
            input_tree_aliases = {"prep": "prep_base"},
            output_tree_bases = {"prep": "prep_base"},
        ),
        env = {},
        execution_requirements = mapped_requirements,
        mnemonic = "LinuxMappedPrep",
        toolchain = CC_TOOLCHAIN_TYPE,
    )
    target_input_directories = {
        key: value
        for key, value in common_inputs.items()
        if key != "prep_base"
    }
    target_input_directories.update({
        "plan": plans["target"],
        "prehost": output_trees["prehost"],
        "bootstrap": output_trees["bootstrap"],
        "host": output_trees["host"],
        "prep": output_trees["prep"],
    })
    target_outputs = {key: output_trees[key] for key in _TREES if key not in ["prehost", "bootstrap", "prep", "host"]}
    target_outputs["work"] = work_trees["target"]
    ctx.actions.map_directory(
        implementation = expand_linux_plan_stage,
        input_directories = target_input_directories,
        additional_inputs = map_inputs,
        output_directories = target_outputs,
        tools = mapped_tools(ctx.attr._recipe_runner[DefaultInfo].files_to_run, "target"),
        additional_params = mapped_params("target"),
        env = {},
        execution_requirements = mapped_requirements,
        mnemonic = "LinuxMappedTarget",
        toolchain = CC_TOOLCHAIN_TYPE,
    )

    image = ctx.actions.declare_file(ctx.label.name + ".image")
    vmlinux = ctx.actions.declare_file(ctx.label.name + ".vmlinux")
    system_map = ctx.actions.declare_file(ctx.label.name + ".System.map")
    module_symvers = ctx.actions.declare_file(ctx.label.name + ".Module.symvers")
    modules_order = ctx.actions.declare_file(ctx.label.name + ".modules.order")
    modules_builtin = ctx.actions.declare_file(ctx.label.name + ".modules.builtin")
    modules_builtin_modinfo = ctx.actions.declare_file(ctx.label.name + ".modules.builtin.modinfo")
    modules_manifest = ctx.actions.declare_file(ctx.label.name + ".modules.manifest")
    _project(ctx, output_trees["image"], "kernel", image)
    _project(ctx, output_trees["vmlinux"], "vmlinux", vmlinux)
    _project(ctx, output_trees["vmlinux"], "System.map", system_map)
    _project(ctx, output_trees["metadata"], "Module.symvers", module_symvers)
    _project(ctx, output_trees["modules"], "modules.order", modules_order)
    _project(ctx, output_trees["metadata"], "modules.builtin", modules_builtin)
    _project(ctx, output_trees["metadata"], "modules.builtin.modinfo", modules_builtin_modinfo)
    _project(ctx, output_trees["metadata"], "modules.manifest", modules_manifest)

    kernel = LinuxKernelInfo(
        arch = arch,
        version = ctx.attr.version,
        kernel_release = kernel_release,
        image = image,
        vmlinux = vmlinux,
        config = resolved,
        system_map = system_map,
    )
    module_tree = LinuxModuleTreeInfo(tree = output_trees["modules"], manifest = modules_manifest)

    # The SDK provider is intentionally tree-based: external modules must run
    # their own execution-time planner and may not inspect resolved CONFIG_*
    # values during analysis.
    module_sdk = LinuxModuleSdkInfo(
        auto_conf = auto_conf,
        auto_conf_cmd = auto_conf_cmd,
        autoconf = autoconf,
        config = resolved,
        host_action_args = host.arguments,
        host_action_environments = host.environments,
        host_action_requirements = host.requirements_by_role,
        host_companion_tools = host.companion_tools,
        host_deps = libelf.tree,
        host_execution_platform = host_execution_platform,
        host_probe_results = host_probe_results,
        host_probe_runner = ctx.attr._host_probe_runner[DefaultInfo].files_to_run,
        host_recipe_runner = ctx.attr._host_recipe_runner[DefaultInfo].files_to_run,
        host_tool_files = host_tools,
        host_toolchain_files = host_toolchain_files,
        host_toolset_identity = host_toolset_identity,
        host_toolset_manifest = host_toolset_manifest,
        kbuild = ctx.file.kbuild,
        host_kconfig_probe_results = host_kconfig_probe_results,
        kernel_key = str(ctx.label),
        kernel_release = kernel_release,
        libelf_compile_flags = libelf.compile_flags,
        libelf_link_flags = libelf.link_flags,
        make_vars = ctx.attr.module_make_vars,
        rust_source_files = rust_source.files if rust_source != None else depset(),
        rust_source_root = rust_source_root,
        rustc_cfg = rustc_cfg,
        sdk = output_trees["sdk"],
        source = depset(ctx.files.source_files),
        source_root = ctx.file.source_root,
        target_action_args = target.arguments,
        target_action_environments = target.environments,
        target_action_requirements = target.requirements_by_role,
        target_companion_tools = target.companion_tools,
        target_execution_platform = target_execution_platform,
        target_probe_results = target_probe_results,
        target_probe_runner = ctx.attr._probe_runner[DefaultInfo].files_to_run,
        target_recipe_runner = ctx.attr._recipe_runner[DefaultInfo].files_to_run,
        target_kconfig_probe_results = target_kconfig_probe_results,
        target_tool_files = target_tools,
        target_toolchain_files = target_toolchain_files,
        target_toolset_identity = target_toolset_identity,
        target_toolset_manifest = target_toolset_manifest,
        version = ctx.attr.version,
    )
    return [
        DefaultInfo(files = depset([image])),
        kernel,
        module_sdk,
        module_tree,
        OutputGroupInfo(
            arch = depset([arch]),
            config = depset([resolved]),
            image = depset([image]),
            kernel_release = depset([kernel_release]),
            module_symvers = depset([module_symvers]),
            modules = depset([output_trees["modules"]]),
            modules_builtin = depset([modules_builtin]),
            modules_builtin_modinfo = depset([modules_builtin_modinfo]),
            modules_order = depset([modules_order]),
            objects = depset([output_trees["objects"]]),
            plan = depset([plans[stage] for stage in _STAGES]),
            probes = depset([
                probe_plan,
                host_probe_results,
                target_probe_results,
                kconfig_probe_plan,
                host_kconfig_probe_results,
                target_kconfig_probe_results,
                kbuild_probe_plan,
                host_kbuild_probe_results,
                target_kbuild_probe_results,
            ]),
            sdk = depset([output_trees["sdk"]]),
            system_map = depset([system_map]),
            toolsets = depset([target_toolset_identity, host_toolset_identity]),
            vmlinux = depset([vmlinux]),
        ),
    ]

linux_mapped_kernel = rule(
    implementation = _linux_mapped_kernel_impl,
    attrs = {
        "config": attr.label(allow_single_file = True, mandatory = True),
        "config_mode": attr.string(default = "default", values = ["allnoconfig", "default"]),
        "kbuild": attr.label(allow_single_file = True, mandatory = True),
        "module_make_vars": attr.string_dict(),
        "overlay": attr.label(allow_single_file = True),
        "source_files": attr.label_list(allow_files = True, mandatory = True),
        "source_root": attr.label(allow_single_file = True, mandatory = True),
        "version": attr.string(mandatory = True),
        "_host_cc_toolchain": host_cc_toolchain_attr(exec_group = "host_cc"),
        "_host_execution_platform": linux_execution_platform_attr(exec_group = "host_cc"),
        "_libcrypto": attr.label(
            cfg = config.exec(exec_group = "host_cc"),
            default = Label("@openssl//:crypto"),
            providers = [CcInfo],
        ),
        "_libelf": attr.label(
            cfg = config.exec(exec_group = "host_cc"),
            default = Label("@elfutils//:elf"),
            providers = [CcInfo],
        ),
    } | {
        attribute: attr.label(cfg = "exec", default = Label("//internal/cmd/%s" % role), executable = True)
        for role, attribute in _TARGET_PLANNER_HELPER_ATTRS.items()
    } | {
        "_host" + attribute: attr.label(
            cfg = config.exec(exec_group = "host_cc"),
            default = Label("//internal/cmd/%s" % role),
            executable = True,
        )
        for role, attribute in _SHARED_PLANNER_HELPER_ATTRS.items()
    } | {
        "_planner": attr.label(cfg = "exec", default = Label("//internal/cmd/kconfig_parse:kconfig_parse"), executable = True),
        "_probe_runner": attr.label(cfg = "exec", default = Label("//internal/cmd/proberun"), executable = True),
        "_recipe_runner": attr.label(cfg = "exec", default = Label("//internal/cmd/mapdirectoryrecipe"), executable = True),
        "_host_probe_runner": attr.label(cfg = config.exec(exec_group = "host_cc"), default = Label("//internal/cmd/proberun"), executable = True),
        "_host_recipe_runner": attr.label(cfg = config.exec(exec_group = "host_cc"), default = Label("//internal/cmd/mapdirectoryrecipe"), executable = True),
        "_pkg_config": attr.label(cfg = config.exec(exec_group = "host_cc"), default = Label("//internal/cmd/pkgconfigshim"), executable = True),
        "_toolsetidentity": attr.label(cfg = "exec", default = Label("//internal/cmd/toolsetidentity"), executable = True),
        "_host_awk": attr.label(cfg = config.exec(exec_group = "host_cc"), default = Label("@gawk//:gawk"), executable = True),
        "_lz4": attr.label(cfg = "exec", default = Label("@lz4//programs:lz4"), executable = True),
        "_bison_m4_deny_shell": attr.label(
            cfg = config.exec(exec_group = "host_cc"),
            default = Label("@rules_bison//bison/internal:m4_deny_shell"),
            executable = True,
        ),
        "_flex_m4_deny_shell": attr.label(
            cfg = config.exec(exec_group = "host_cc"),
            default = Label("@rules_flex//flex/internal:m4_deny_shell"),
            executable = True,
        ),
        "_m4_deny_shell": attr.label(
            cfg = config.exec(exec_group = "host_cc"),
            default = Label("@rules_m4//m4/internal:deny_shell"),
            executable = True,
        ),
        "_pahole": attr.label(cfg = "exec", default = Label("@pahole//:pahole"), executable = True),
        "_target_awk": attr.label(cfg = "exec", default = Label("@gawk//:gawk"), executable = True),
        "_target_execution_platform": linux_execution_platform_attr(),
    },
    exec_groups = {"host_cc": exec_group(toolchains = use_cc_toolchain() + [
        BISON_TOOLCHAIN_TYPE,
        optional_bindgen_toolchain_type(),
        FLEX_TOOLCHAIN_TYPE,
        M4_TOOLCHAIN_TYPE,
        optional_rust_analyzer_toolchain_type(),
        optional_rust_toolchain_type(),
        _PYTHON_EXEC_TOOLS_TOOLCHAIN_TYPE,
        _PERL_TOOLCHAIN_TYPE,
        SCRIPT_RUNTIME_TOOLCHAIN_TYPE,
    ])},
    fragments = ["cpp"],
    toolchains = use_cc_toolchain() + [optional_rust_toolchain_type(), _PYTHON_EXEC_TOOLS_TOOLCHAIN_TYPE, _PERL_TOOLCHAIN_TYPE, SCRIPT_RUNTIME_TOOLCHAIN_TYPE],
)

def _kernel_projection_impl(ctx):
    info = ctx.attr.kernel[LinuxKernelInfo]
    return [DefaultInfo(files = depset([getattr(info, ctx.attr.field)]))]

_kernel_projection = rule(
    implementation = _kernel_projection_impl,
    attrs = {
        "field": attr.string(mandatory = True, values = _KERNEL_FIELDS),
        "kernel": attr.label(mandatory = True, providers = [LinuxKernelInfo]),
    },
)

def _module_projection_impl(ctx):
    return [DefaultInfo(files = getattr(ctx.attr.kernel[OutputGroupInfo], ctx.attr.field))]

_module_projection = rule(
    implementation = _module_projection_impl,
    attrs = {
        "field": attr.string(mandatory = True, values = _MODULE_FIELDS),
        "kernel": attr.label(mandatory = True, providers = [LinuxModuleSdkInfo]),
    },
)

def _validate_module_path(label, path):
    components = path.split("/")
    if (
        not path.endswith(".ko") or
        path.startswith("/") or
        "\\" in path or
        any([component in ["", ".", ".."] for component in components])
    ):
        fail("%s path must be a canonical relative .ko path, got %r" % (label, path))
    return components

def _in_tree_module_projection_impl(ctx):
    info = ctx.attr.kernel[LinuxModuleTreeInfo]
    components = _validate_module_path(ctx.label, ctx.attr.path)
    output = ctx.actions.declare_file(ctx.label.name + "/" + components[-1])
    _project(ctx, info.tree, ctx.attr.path, output, manifest = info.manifest)
    return [DefaultInfo(files = depset([output]))]

_in_tree_module_projection = rule(
    implementation = _in_tree_module_projection_impl,
    attrs = {
        "kernel": attr.label(
            mandatory = True,
            providers = [LinuxModuleTreeInfo],
        ),
        "path": attr.string(mandatory = True),
        "_recipe_runner": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/mapdirectoryrecipe"),
            executable = True,
        ),
    },
    doc = "Projects one repository-declared in-tree .ko from a kernel module tree.",
)

def linux_mapped_image_targets(
        name,
        config,
        config_mode,
        module_make_vars,
        module_targets,
        overlay,
        platform,
        source_repo,
        version,
        visibility = ["//visibility:public"]):
    """Defines one execution-time mapped graph behind stable image labels."""
    graph = name + "__mapped"
    linux_mapped_kernel(
        name = graph,
        config = config,
        config_mode = config_mode,
        kbuild = source_repo + "//:Kbuild",
        module_make_vars = module_make_vars,
        overlay = overlay,
        source_files = [source_repo + "//:all_files"],
        source_root = source_repo + "//:Kconfig",
        version = version,
        visibility = ["//visibility:private"],
    )
    linux_platform_transition(
        name = name,
        graph = ":" + graph,
        platform = platform,
        visibility = visibility,
    )
    for field in _KERNEL_FIELDS:
        _kernel_projection(
            name = field,
            field = field,
            kernel = ":" + name,
            visibility = visibility,
        )
    for field in _MODULE_FIELDS:
        _module_projection(
            name = field,
            field = field,
            kernel = ":" + name,
            visibility = visibility,
        )
    for module_name in sorted(module_targets.keys()):
        _in_tree_module_projection(
            name = module_name,
            kernel = ":" + name,
            path = module_targets[module_name],
            visibility = visibility,
        )
