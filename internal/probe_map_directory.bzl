"""Bazel 9 map_directory expansion for symbolic Linux compiler probes.

The callback is intentionally recipe-opaque. It parses only path-encoded plan
metadata, binds those paths to configured tools and identity markers, and
registers proberun actions. Compiler behavior remains data produced by those
actions and consumed later by Kconfig/Kbuild.
"""

load(
    ":toolchain_action_paths.bzl",
    _EXECUTION_ROOT_MARKER = "EXECUTION_ROOT_MARKER",
    _add_rendered_toolchain_action_value = "add_rendered_toolchain_action_value",
    _render_toolchain_action_value = "render_toolchain_action_value",
    _toolchain_action_path_index = "toolchain_action_path_index",
    _toolchain_action_path_index_from_list = "toolchain_action_path_index_from_list",
)

visibility("//internal/...")

_SCHEMA = "linux-probe-plan-v2"
_KBUILD_ARGS_SENTINEL = "__LINUX_BZL_KBUILD_ARGS_V1__"
_HEX = "0123456789abcdef"
_NAME = "abcdefghijklmnopqrstuvwxyz0123456789_.+-"
_SCOPES = ["host", "target"]
_PLAN_DIRECTORY = "plan"
_RESULT_DIRECTORY = "results"
_HOST_RESULTS_DIRECTORY = "host_results"
_IDENTITY_DIRECTORIES = {
    "host": "host_toolset_identity",
    "target": "target_toolset_identity",
}
_RUNNER_TOOL = "probe_runner"
_TOOLCHAIN_FILES = "toolchain_files"
_TOOLSET_MANIFEST = "toolset_manifest"
_TOOLSET_ANCHOR_PREFIX = "toolset_anchor_"
_ROLE_TOOL_PREFIX = "probe_role_"
_COMPANION_TOOL_PREFIX = "companion_tool_"
_EMPTY_RESULT_MARKER = ".empty"
_LINUX_SOURCE_ROOT = "linux"
_RUST_SOURCE_ROOT = "rust"
_SOURCE_INPUTS = ["rust_source_files", "source_files", "source_root"]

def _is_decimal(value, width = None):
    if not value or (width != None and len(value) != width):
        return False
    return all([character in "0123456789" for character in value.elems()])

def _is_sha256(value):
    return len(value) == 64 and all([character in _HEX for character in value.elems()])

def _is_identity(value):
    return value.startswith("sha256-") and _is_sha256(value[len("sha256-"):])

def _valid_name(value):
    return value and value[0] in "abcdefghijklmnopqrstuvwxyz0123456789" and all([
        character in _NAME
        for character in value.elems()
    ])

def _validate_source_path(value, what):
    if not value or value.startswith("/") or value.endswith("/") or "\\" in value:
        fail("%s has invalid relative path %r" % (what, value))
    if any([part in ["", ".", ".."] for part in value.split("/")]):
        fail("%s has invalid relative path %r" % (what, value))

def _canonical_rust_source_path(value, what):
    # Match mapped_kernel's canonical artifact namespace. Under Bazel's
    # sibling-repository layout, File.short_path uses ../repo while stable
    # planner data uses external/repo.
    if value.startswith("../"):
        value = "external/" + value[3:]
    if value.startswith("bazel-out/"):
        parts = value.split("/")
        if len(parts) < 4 or parts[2] not in ["bin", "genfiles"]:
            fail("%s has unrecognized Bazel output path %r" % (what, value))
        value = "/".join(parts[3:])
    _validate_source_path(value, what)
    return value

def _canonical_rust_source_file(file):
    canonical_path = _canonical_rust_source_path(file.path, "selected Rust source path")
    canonical_short_path = _canonical_rust_source_path(file.short_path, "selected Rust source short_path")
    if canonical_path != canonical_short_path:
        fail("selected Rust source path %r and short_path %r map to different canonical paths %r and %r" % (
            file.path,
            file.short_path,
            canonical_path,
            canonical_short_path,
        ))
    return canonical_path

def _kernel_source_path(file, source_prefix):
    canonical = file.short_path
    if source_prefix:
        prefix = source_prefix + "/"
        if not canonical.startswith(prefix):
            fail("mapped Linux source input %s is outside %s" % (file, source_prefix))
        canonical = canonical[len(prefix):]
    _validate_source_path(canonical, "mapped Linux source input")
    return canonical

def _record_source(files, source, file, kind):
    existing = files.get(source)
    if existing != None and existing != file:
        fail("%s path %s is provided by distinct artifacts %s and %s" % (
            kind,
            source,
            existing,
            file,
        ))
    files[source] = file

def _probe_source_context(additional_inputs, source_prefix, rust_source_root):
    unexpected = sorted([name for name in additional_inputs if name not in _SOURCE_INPUTS])
    if unexpected:
        fail("Linux probe expansion has unexpected additional inputs %r" % unexpected)

    has_source_files = "source_files" in additional_inputs
    has_source_root = "source_root" in additional_inputs
    if has_source_files != has_source_root:
        fail("Linux probe expansion requires source_files and source_root together")
    if not has_source_files:
        if "rust_source_files" in additional_inputs:
            fail("Linux probe expansion cannot receive Rust sources without Linux sources")
        if source_prefix or rust_source_root:
            fail("Linux probe expansion has source-root params without source inputs")
        return struct(
            files = {},
            kernel_files = depset(),
            linux_root = None,
            rust_files = depset(),
            rust_root = "",
            rust_witness = None,
        )

    kernel_files = additional_inputs["source_files"]
    linux_root = additional_inputs["source_root"]
    files = {}
    for file in kernel_files.to_list():
        _record_source(files, _kernel_source_path(file, source_prefix), file, "mapped Linux source")

    root_path = _kernel_source_path(linux_root, source_prefix)
    if root_path != "Kconfig":
        fail("Linux probe source_root must be the declared Kconfig, got %r" % root_path)
    indexed_root = files.get("Kconfig")
    if indexed_root == None:
        fail("Linux probe source_files omit declared Kconfig source_root")
    if indexed_root != linux_root:
        fail("Linux probe Kconfig path is provided by distinct source_root and source_files artifacts")

    rust_files = additional_inputs.get("rust_source_files", depset())
    if "rust_source_files" in additional_inputs and not rust_source_root:
        fail("Linux probe expansion has Rust source inputs without rust_source_root")
    if rust_source_root:
        _validate_source_path(rust_source_root, "Linux probe Rust source root")
        if "rust_source_files" not in additional_inputs:
            fail("Linux probe expansion has rust_source_root without Rust source inputs")
    rust_files_by_path = {}
    for file in rust_files.to_list():
        canonical = _canonical_rust_source_file(file)
        _record_source(files, canonical, file, "Linux probe source")
        _record_source(rust_files_by_path, canonical, file, "selected Rust source")
    rust_witness = None
    if rust_source_root:
        prefix = rust_source_root + "/"
        witness_paths = sorted([
            filename
            for filename in rust_files_by_path
            if filename == rust_source_root or filename.startswith(prefix)
        ])
        if not witness_paths:
            fail("Linux probe Rust source root %r contains no selected source file" % rust_source_root)
        rust_witness = rust_files_by_path[witness_paths[0]]

    return struct(
        files = files,
        kernel_files = kernel_files,
        linux_root = linux_root,
        rust_files = rust_files,
        rust_root = rust_source_root,
        rust_witness = rust_witness,
    )

def _ordinal(value):
    decimal = str(value)
    if len(decimal) > 8:
        fail("Linux probe ordinal %d exceeds eight digits" % value)
    return "00000000"[:8 - len(decimal)] + decimal

def _tool_executable(tool):
    executable = getattr(tool, "executable", None)
    return executable if executable != None else tool

def _companion_tool_bindings(tools, role):
    prefix = _COMPANION_TOOL_PREFIX + role + "_"
    return [
        tools[name]
        for name in sorted(tools)
        if name.startswith(prefix)
    ]

def _add_artifact_path(args, flag, artifact, format = None):
    args.add(flag)
    if format == None:
        args.add_all([artifact], expand_directories = False)
    else:
        args.add_all([artifact], expand_directories = False, format_each = format)

def _children_by_path(directory, what):
    children = {}
    for child in directory.children:
        relative = child.tree_relative_path
        if relative in children:
            fail("%s repeats child %r" % (what, relative))
        children[relative] = child
    return children

def _topological_order(nodes):
    # Build the reverse graph once. Repeatedly scanning every node until a
    # fixed point makes a reverse-ordered chain quadratic in the number of
    # probes, which is especially costly inside map_directory expansion.
    # Iterating sorted node IDs while populating dependents also keeps each
    # ready batch deterministic without sorting it again.
    dependents = {node_id: [] for node_id in nodes}
    pending = {}
    for node_id in sorted(nodes):
        dependencies = nodes[node_id]["inputs"].values()
        pending[node_id] = len(dependencies)
        for producer in dependencies:
            dependents[producer].append(node_id)

    ready = [node_id for node_id in sorted(nodes) if pending[node_id] == 0]
    order = []
    for index in range(len(nodes)):
        if index >= len(ready):
            fail("Linux probe plan contains a dependency cycle")
        node_id = ready[index]
        order.append(node_id)
        for consumer in dependents[node_id]:
            pending[consumer] -= 1
            if pending[consumer] == 0:
                ready.append(consumer)
    return order

def _decode_source_marker(node_id, marker, parts):
    if len(parts) < 5 or parts[-1] != "path":
        fail("Linux probe node %s has invalid source marker %r" % (node_id, marker))
    components = []
    for component in parts[3:-1]:
        if not component.startswith("+") or len(component) == 1:
            fail("Linux probe node %s has invalid source marker %r" % (node_id, marker))
        components.append(component[1:])
    source = "/".join(components)
    _validate_source_path(source, "Linux probe source marker")
    return source

def _parse_probe_plan(children):
    if "schema/%s" % _SCHEMA not in children:
        fail("Linux probe plan does not use schema %s" % _SCHEMA)

    requests = {}
    toolsets = {}
    terminals = {}
    nodes = {}
    for marker in sorted(children):
        parts = marker.split("/")
        if parts == ["schema", _SCHEMA]:
            continue
        if len(parts) == 3 and parts[0] == "toolsets" and parts[1] in _SCOPES:
            if not _is_identity(parts[2]) or parts[1] in toolsets:
                fail("Linux probe plan has invalid or repeated toolset marker %r" % marker)
            toolsets[parts[1]] = struct(file = children[marker], identity = parts[2])
            continue
        if len(parts) == 2 and parts[0] == "requests" and parts[1].endswith(".json"):
            request_id = parts[1][:-len(".json")]
            if not _is_sha256(request_id) or request_id in requests:
                fail("Linux probe plan has invalid or repeated request marker %r" % marker)
            requests[request_id] = children[marker]
            continue
        if len(parts) == 2 and parts[0] == "terminal":
            if not _is_sha256(parts[1]) or parts[1] in terminals:
                fail("Linux probe plan has invalid or repeated terminal marker %r" % marker)
            terminals[parts[1]] = children[marker]
            continue
        if len(parts) < 4 or parts[0] != "nodes" or not _is_sha256(parts[1]):
            fail("Linux probe plan contains unknown marker %r" % marker)
        node_id = parts[1]
        node = nodes.setdefault(node_id, {
            "inputs": {},
            "source_roots": {},
            "sources": {},
            "tools": {},
        })
        field = parts[2]
        if field == "scope" and len(parts) == 4:
            if parts[3] not in _SCOPES or "scope" in node:
                fail("Linux probe node %s has invalid or repeated scope" % node_id)
            node["scope"] = parts[3]
        elif field == "request" and len(parts) == 4:
            if not _is_sha256(parts[3]) or "request" in node:
                fail("Linux probe node %s has invalid or repeated request" % node_id)
            node["request"] = parts[3]
        elif field == "tool" and len(parts) == 4:
            role = parts[3]
            if not _valid_name(role) or role in node["tools"]:
                fail("Linux probe node %s has invalid or repeated tool role %r" % (node_id, role))
            node["tools"][role] = True
        elif field == "source":
            source = _decode_source_marker(node_id, marker, parts)
            if source in node["sources"]:
                fail("Linux probe node %s repeats source %r" % (node_id, source))
            node["sources"][source] = True
        elif field == "source_root" and len(parts) == 4:
            root = parts[3]
            if not _valid_name(root) or root in node["source_roots"]:
                fail("Linux probe node %s has invalid or repeated source root %r" % (node_id, root))
            node["source_roots"][root] = True
        elif field == "in" and len(parts) == 5:
            ordinal, producer = parts[3], parts[4]
            if not _is_decimal(ordinal, 8) or not _is_sha256(producer) or ordinal in node["inputs"]:
                fail("Linux probe node %s has invalid or repeated dependency %r" % (node_id, marker))
            node["inputs"][ordinal] = producer
        else:
            fail("Linux probe node %s has invalid marker %r" % (node_id, marker))

    if "target" not in toolsets:
        fail("Linux probe plan has no target toolset marker")
    has_host = False
    for node_id, node in nodes.items():
        if "scope" not in node or "request" not in node:
            fail("Linux probe node %s has incomplete metadata" % node_id)
        if node["request"] not in requests:
            fail("Linux probe node %s references unknown request %s" % (node_id, node["request"]))
        ordinals = sorted(node["inputs"])
        expected = [_ordinal(index) for index in range(len(ordinals))]
        if ordinals != expected:
            fail("Linux probe node %s has non-contiguous dependency ordinals %r" % (node_id, ordinals))
        if node["scope"] == "host":
            has_host = True
        for producer in node["inputs"].values():
            dependency = nodes.get(producer)
            if dependency == None:
                fail("Linux probe node %s references unknown dependency %s" % (node_id, producer))
            if node["scope"] == "host" and dependency.get("scope") != "host":
                fail("host Linux probe node %s depends on target node %s" % (node_id, producer))
    if has_host and "host" not in toolsets:
        fail("Linux probe plan has host nodes but no host toolset marker")
    for node_id in terminals:
        if node_id not in nodes:
            fail("Linux probe plan terminal references unknown node %s" % node_id)
    return struct(
        children = children,
        nodes = nodes,
        order = _topological_order(nodes),
        requests = requests,
        terminals = terminals,
        toolsets = toolsets,
    )

def _expected_identity(directory, scope):
    children = directory.children
    if len(children) != 1:
        fail("configured %s probe toolset identity must contain exactly one marker, found %d" % (scope, len(children)))
    identity = children[0].tree_relative_path
    if "/" in identity or not _is_identity(identity):
        fail("configured %s probe toolset has invalid identity marker %r" % (scope, identity))
    return struct(file = children[0], identity = identity)

def _validate_toolsets(parsed, input_directories, scopes):
    validated = {}
    for scope in scopes:
        planned = parsed.toolsets.get(scope)
        if planned == None:
            fail("Linux probe plan has no %s toolset marker" % scope)
        directory_name = _IDENTITY_DIRECTORIES[scope]
        directory = input_directories.get(directory_name)
        if directory == None:
            fail("Linux probe expansion has no configured %s toolset identity" % scope)
        expected = _expected_identity(directory, scope)
        if expected.identity != planned.identity:
            fail("Linux probe plan selected %s toolset %s, configured toolset is %s" % (
                scope,
                planned.identity,
                expected.identity,
            ))
        validated[scope] = expected
    return validated

def _prior_host_results(directory, nodes):
    expected = {
        node_id: True
        for node_id, node in nodes.items()
        if node["scope"] == "host"
    }
    results = {}
    for child in directory.children:
        parts = child.tree_relative_path.split("/")
        if len(parts) != 2 or parts[0] != "results" or not parts[1].endswith(".json"):
            fail("host Linux probe result tree contains unknown child %r" % child.tree_relative_path)
        node_id = parts[1][:-len(".json")]
        if not _is_sha256(node_id) or node_id not in expected or node_id in results:
            fail("host Linux probe result tree has invalid, unexpected, or repeated node %r" % node_id)
        results[node_id] = child
    missing = sorted([node_id for node_id in expected if node_id not in results])
    if missing:
        fail("host Linux probe result tree omits nodes %r" % missing)
    return results

def _select_prior_host_results(scope, nodes, host_results):
    host_nodes = [node_id for node_id, node in nodes.items() if node["scope"] == "host"]
    if scope == "target" and host_nodes:
        if host_results == None:
            fail("target Linux probe expansion requires host result tree")
        return _prior_host_results(host_results, nodes)
    if host_results != None and host_results.children:
        # Staged planners use one fixed host-then-target action topology even
        # when a source/config combination happens not to issue host probes.
        # The producer materializes an explicit empty marker because Bazel has
        # no directory output without at least one expanded action.
        paths = [child.tree_relative_path for child in host_results.children]
        if paths != [_EMPTY_RESULT_MARKER]:
            fail("Linux probe expansion received host results for a plan with no host nodes")
    return {}

def _action_arguments(additional_params, role):
    count = int(additional_params.get("action_arg_count_" + role, "0"))
    return [additional_params["action_arg_%s_%d" % (role, index)] for index in range(count)]

def _action_environment(additional_params, role):
    count = int(additional_params.get("action_env_count_" + role, "0"))
    return [additional_params["action_env_%s_%d" % (role, index)] for index in range(count)]

def _driver_link_contract_role(role):
    return role + "-link" if role in ["cc", "cxx"] else None

def _node_source_bindings(node_id, node, source_context, input_directories):
    sources = {}
    for source in sorted(node["sources"]):
        file = source_context.files.get(source)
        if file == None:
            fail("Linux probe node %s requests unavailable source %r" % (node_id, source))
        sources[source] = file

    linux_anchor = None
    rust_root = ""
    rust_witness = None
    directory_roots = {}
    reserved = [_HOST_RESULTS_DIRECTORY, _PLAN_DIRECTORY] + _IDENTITY_DIRECTORIES.values()
    for root in sorted(node["source_roots"]):
        if root == _LINUX_SOURCE_ROOT:
            linux_anchor = source_context.linux_root
            if linux_anchor == None:
                fail("Linux probe node %s requests unavailable source root %r" % (node_id, root))
            declared = sources.get("Kconfig")
            if declared == None:
                fail("Linux probe node %s source root %r requires declared source %r" % (
                    node_id,
                    root,
                    "Kconfig",
                ))
            if declared != linux_anchor:
                fail("Linux probe node %s source root %r is not anchored by declared Kconfig" % (node_id, root))
        elif root == _RUST_SOURCE_ROOT:
            rust_root = source_context.rust_root
            if not rust_root:
                fail("Linux probe node %s requests unavailable source root %r" % (node_id, root))
            rust_witness = source_context.rust_witness
            if rust_witness == None:
                fail("Linux probe node %s source root %r has no selected witness" % (node_id, root))
        elif root in input_directories and root not in reserved:
            directory_roots[root] = input_directories[root]
        else:
            fail("Linux probe node %s requests unknown source root %r" % (node_id, root))
    return struct(
        directory_roots = directory_roots,
        linux_anchor = linux_anchor,
        rust_root = rust_root,
        rust_witness = rust_witness,
        sources = sources,
    )

def _render_probe_action_value_cached(value, path_index, rendered_values):
    rendered = rendered_values.get(value)
    if rendered == None:
        rendered = _render_toolchain_action_value(value, path_index)
        rendered_values[value] = rendered
    return rendered

def expand_linux_probe_plan(template_ctx, input_directories, output_directories, additional_inputs, tools, additional_params):
    """Expands one host or target scope of a path-encoded ProbePlan."""
    scope = additional_params.get("scope")
    if scope not in _SCOPES:
        fail("Linux probe expansion has invalid scope %r" % scope)
    source_context = _probe_source_context(
        additional_inputs,
        additional_params.get("source_prefix", ""),
        additional_params.get("rust_source_root", ""),
    )
    if sorted(output_directories.keys()) != [_RESULT_DIRECTORY]:
        fail("Linux probe expansion requires exactly the %s output directory" % _RESULT_DIRECTORY)
    reserved_input_directories = [_HOST_RESULTS_DIRECTORY, _PLAN_DIRECTORY] + _IDENTITY_DIRECTORIES.values()
    for name in input_directories:
        if name not in reserved_input_directories and not _valid_name(name):
            fail("Linux probe expansion has invalid source-root input directory %r" % name)
    plan_directory = input_directories.get(_PLAN_DIRECTORY)
    if plan_directory == None:
        fail("Linux probe expansion has no plan input directory")
    parsed = _parse_probe_plan(_children_by_path(plan_directory, "Linux probe plan"))

    required_identity_scopes = {scope: True}
    for node in parsed.nodes.values():
        if node["scope"] == scope:
            for producer in node["inputs"].values():
                required_identity_scopes[parsed.nodes[producer]["scope"]] = True
    identities = _validate_toolsets(parsed, input_directories, sorted(required_identity_scopes))

    if _RUNNER_TOOL not in tools or _TOOLCHAIN_FILES not in tools or _TOOLSET_MANIFEST not in tools:
        fail("Linux probe expansion has no runner, toolset manifest, or toolchain closure")
    action_path_index = _toolchain_action_path_index(tools[_TOOLCHAIN_FILES])

    # Every probe in this callback uses the same validated toolchain closure.
    # Retain successful exact-value renders only here: the same path text can
    # bind different Files in another scope or callback. Keep rendering lazy so
    # unused action roles retain their existing validation behavior.
    rendered_action_values = {}
    for node_id, node in parsed.nodes.items():
        if node["scope"] != scope:
            continue
        for role in node["tools"]:
            if _ROLE_TOOL_PREFIX + role not in tools:
                fail("Linux probe node %s requires unavailable %s tool" % (node_id, role))

    prior = _select_prior_host_results(
        scope,
        parsed.nodes,
        input_directories.get(_HOST_RESULTS_DIRECTORY),
    )

    outputs = {}
    for node_id, node in parsed.nodes.items():
        if node["scope"] == scope:
            outputs[node_id] = template_ctx.declare_file(
                "results/%s.json" % node_id,
                directory = output_directories[_RESULT_DIRECTORY],
            )

    if not outputs:
        marker = template_ctx.declare_file(
            _EMPTY_RESULT_MARKER,
            directory = output_directories[_RESULT_DIRECTORY],
        )
        writer = tools[_ROLE_TOOL_PREFIX + "actionfile"]
        args = template_ctx.args()
        _add_artifact_path(args, "-out", marker)
        args.add("-content_base64", "")
        template_ctx.run(
            executable = _tool_executable(writer),
            tools = [writer],
            outputs = [marker],
            arguments = [args],
            progress_message = "Materializing empty Linux %s probe result" % scope,
        )

    registered = {}
    for node_id in parsed.order:
        node = parsed.nodes[node_id]
        if node["scope"] != scope:
            continue
        args = template_ctx.args()
        request = parsed.requests[node["request"]]
        output = outputs[node_id]
        _add_artifact_path(args, "-request", request)
        _add_artifact_path(args, "-result", output)
        args.add("-node_id", node_id)
        args.add("-request_id", node["request"])
        args.add("-scope", scope)
        _add_artifact_path(args, "-toolset_manifest", tools[_TOOLSET_MANIFEST])
        inputs = [request, tools[_TOOLSET_MANIFEST]]
        source_bindings = _node_source_bindings(node_id, node, source_context, input_directories)
        for source in sorted(source_bindings.sources):
            file = source_bindings.sources[source]
            _add_artifact_path(args, "-source", file, format = source + "=%s")
            inputs.append(file)
        if source_bindings.linux_anchor != None:
            _add_artifact_path(
                args,
                "-source_root_anchor",
                source_bindings.linux_anchor,
                format = _LINUX_SOURCE_ROOT + "=%s",
            )
            inputs.append(source_bindings.linux_anchor)
        if source_bindings.rust_root:
            args.add("-source_root", _RUST_SOURCE_ROOT + "=" + source_bindings.rust_root)
            _add_artifact_path(
                args,
                "-source_root_witness",
                source_bindings.rust_witness,
                format = _RUST_SOURCE_ROOT + "=%s",
            )
            inputs.append(source_bindings.rust_witness)
        for root, directory in sorted(source_bindings.directory_roots.items()):
            _add_artifact_path(args, "-source_root_tree", directory.directory, format = root + "=%s")
            inputs.append(directory.directory)

        dependency_scopes = {scope: True}
        for ordinal, producer in sorted(node["inputs"].items()):
            producer_scope = parsed.nodes[producer]["scope"]
            dependency_scopes[producer_scope] = True
            artifact = outputs.get(producer) if producer_scope == scope else prior.get(producer)
            if artifact == None or (producer_scope == scope and producer not in registered):
                fail("Linux probe node %s has unavailable dependency %s" % (node_id, producer))
            _add_artifact_path(args, "-input", artifact, format = ordinal + "=%s")
            inputs.append(artifact)
        for identity_scope in sorted(dependency_scopes):
            identity = identities.get(identity_scope)
            if identity == None:
                fail("Linux probe node %s has no validated %s identity" % (node_id, identity_scope))
            _add_artifact_path(args, "-toolset_marker", identity.file, format = identity_scope + "=%s")
            inputs.append(identity.file)

        selected_tools = []
        for tool_name, runtime_tool in sorted(tools.items()):
            if not tool_name.startswith(_ROLE_TOOL_PREFIX):
                continue
            runtime_role = tool_name[len(_ROLE_TOOL_PREFIX):]
            runtime_executable = _tool_executable(runtime_tool)
            _add_artifact_path(args, "-runtime_tool", runtime_executable, format = runtime_role + "=%s")
            inputs.append(runtime_executable)
            selected_tools.append(runtime_tool)
            selected_tools.extend(_companion_tool_bindings(tools, runtime_role))
        for role in sorted(node["tools"]):
            tool = tools[_ROLE_TOOL_PREFIX + role]
            executable = _tool_executable(tool)
            contract_roles = [role]
            companion_role = _driver_link_contract_role(role)
            if companion_role != None and "action_arg_count_" + companion_role in additional_params:
                contract_roles.append(companion_role)
            for contract_role in contract_roles:
                # Semantic companion roles deliberately bind the same
                # source-selected compiler executable; only their configured
                # action envelope differs.
                _add_artifact_path(args, "-tool", executable, format = contract_role + "=%s")
                for argument in _action_arguments(additional_params, contract_role):
                    _add_rendered_toolchain_action_value(
                        args,
                        "-action_arg",
                        _render_probe_action_value_cached(argument, action_path_index, rendered_action_values),
                        prefix = contract_role + "=",
                    )
                for environment in _action_environment(additional_params, contract_role):
                    _add_rendered_toolchain_action_value(
                        args,
                        "-action_env",
                        _render_probe_action_value_cached(environment, action_path_index, rendered_action_values),
                        prefix = contract_role + "=",
                    )

        transitive_inputs = []
        if node["sources"] or node["source_roots"]:
            # Script probes commonly load sibling helpers and fixtures that do
            # not appear in argv. Declare both selected source closures so the
            # runner has the same hermetic tree locally and on remote workers.
            transitive_inputs.append(source_context.kernel_files)
            if source_context.rust_root:
                transitive_inputs.append(source_context.rust_files)

        anchor_args = template_ctx.args()
        anchors = [
            (name[len(_TOOLSET_ANCHOR_PREFIX):], tools[name])
            for name in sorted(tools)
            if name.startswith(_TOOLSET_ANCHOR_PREFIX)
        ]
        if not anchors:
            fail("Linux probe expansion has no toolset root anchors")
        for root, anchor in anchors:
            anchor_args.add_all(
                [anchor],
                expand_directories = False,
                format_each = "-toolset_anchor=" + root + "=%s",
            )
        template_ctx.run(
            executable = tools[_RUNNER_TOOL],
            inputs = depset(direct = inputs, transitive = transitive_inputs),
            tools = [tools[_TOOLCHAIN_FILES]] + selected_tools,
            outputs = [output],
            arguments = [args, anchor_args],
            progress_message = "Probing Linux %s compiler capability %s" % (scope, node_id[:12]),
        )
        registered[node_id] = True

def linux_probe_map_directory_params(
        scope,
        action_args,
        action_environments,
        source_prefix = "",
        rust_source_root = ""):
    """Flattens exact configured action envelopes into Bazel 9 scalar params."""
    if scope not in _SCOPES:
        fail("Linux probe map_directory has invalid scope %r" % scope)
    if source_prefix:
        if source_prefix.startswith("/") or source_prefix.endswith("/") or "\\" in source_prefix:
            fail("Linux probe map_directory has invalid source_prefix %r" % source_prefix)
        parts = source_prefix.split("/")
        if any([part in ["", "."] for part in parts]) or ".." in parts[1:] or len([part for part in parts if part == ".."]) > 1:
            fail("Linux probe map_directory has invalid source_prefix %r" % source_prefix)
    if rust_source_root:
        _validate_source_path(rust_source_root, "Linux probe map_directory rust_source_root")
    params = {
        "rust_source_root": rust_source_root,
        "scope": scope,
        "source_prefix": source_prefix,
    }
    for role in sorted(action_args):
        if not _valid_name(role):
            fail("Linux probe action has invalid role %r" % role)
        argv = action_args[role]
        if argv and len([value for value in argv if value == _KBUILD_ARGS_SENTINEL]) != 1:
            fail("Linux probe %s action must contain exactly one Kbuild argument sentinel" % role)
        params["action_arg_count_" + role] = str(len(argv))
        for index, argument in enumerate(argv):
            params["action_arg_%s_%d" % (role, index)] = argument
        environment = action_environments.get(role, {})
        params["action_env_count_" + role] = str(len(environment))
        for index, name in enumerate(sorted(environment)):
            if not name or "=" in name:
                fail("Linux probe %s action has invalid environment name %r" % (role, name))
            params["action_env_%s_%d" % (role, index)] = name + "=" + environment[name]
    unexpected = sorted([role for role in action_environments if role not in action_args])
    if unexpected:
        fail("Linux probe environments have no action argv for roles %r" % unexpected)
    return params

def linux_probe_map_directory_tools(runner, tool_files, toolchain_files, toolset_manifest, toolset_anchors, companion_tools = {}):
    """Namespaces configured probe tools away from callback implementation tools."""
    values = {
        _RUNNER_TOOL: runner,
        _TOOLCHAIN_FILES: toolchain_files,
        _TOOLSET_MANIFEST: toolset_manifest,
    }
    if not toolset_anchors:
        fail("Linux probe map_directory has no toolset root anchors")
    for root, anchor in toolset_anchors.items():
        if not _valid_name(root):
            fail("Linux probe map_directory has invalid toolset root anchor %r" % root)
        values[_TOOLSET_ANCHOR_PREFIX + root] = anchor
    for role, tool in tool_files.items():
        if not _valid_name(role):
            fail("Linux probe tool has invalid role %r" % role)
        values[_ROLE_TOOL_PREFIX + role] = tool
    for role in sorted(companion_tools):
        if role not in tool_files:
            fail("Linux probe companion tools reference unknown role %r" % role)
        if not _valid_name(role):
            fail("Linux probe companion tools have invalid role %r" % role)
        for index, companion in enumerate(companion_tools[role]):
            values[_COMPANION_TOOL_PREFIX + role + "_" + _ordinal(index)] = companion
    return values

def linux_test_render_probe_action_value(value, artifacts):
    return _render_toolchain_action_value(value, _toolchain_action_path_index_from_list(artifacts))

def linux_test_render_probe_action_values(values, artifacts, cached = True):
    """Exercises callback-local rendering without reconstructing path semantics."""
    path_index = _toolchain_action_path_index_from_list(artifacts)
    rendered_values = {}
    return [
        _render_probe_action_value_cached(value, path_index, rendered_values) if cached else _render_toolchain_action_value(value, path_index)
        for value in values
    ]

def linux_test_probe_execution_root_marker():
    return _EXECUTION_ROOT_MARKER

# Pure test seams. The production callback and unit tests share the parser and
# topological sort so test fixtures cannot silently reconstruct marker meaning.
def linux_test_parse_probe_marker_paths(paths):
    children = {}
    for marker in paths:
        if marker in children:
            fail("test Linux probe plan repeats marker %r" % marker)
        children[marker] = marker
    return _parse_probe_plan(children)

def linux_test_probe_topological_order(nodes):
    """Exercises the production linear-time probe dependency scheduler."""
    return _topological_order(nodes)

def linux_test_select_prior_host_result_paths(scope, parsed, paths):
    """Exercises production host-result selection with path-only test artifacts."""
    directory = None
    if paths != None:
        directory = struct(children = [struct(tree_relative_path = path) for path in paths])
    return sorted(_select_prior_host_results(scope, parsed.nodes, directory).keys())

def _test_source_context(kernel_paths, source_prefix, rust_paths, rust_source_root):
    kernel_files = [
        struct(path = path, short_path = path, test_identity = str(index))
        for index, path in enumerate(kernel_paths)
    ]
    root_candidates = [
        file
        for file in kernel_files
        if _kernel_source_path(file, source_prefix) == "Kconfig"
    ]
    source_root = root_candidates[0] if root_candidates else struct(
        path = "missing/Kconfig",
        short_path = "missing/Kconfig",
        test_identity = "missing",
    )
    additional_inputs = {
        "source_files": depset(kernel_files),
        "source_root": source_root,
    }
    if rust_paths != None:
        rust_files = [
            struct(path = path, short_path = path, test_identity = "rust-" + str(index))
            for index, path in enumerate(rust_paths)
        ]
        additional_inputs["rust_source_files"] = depset(rust_files)
    return _probe_source_context(additional_inputs, source_prefix, rust_source_root)

def linux_test_index_probe_source_paths(
        kernel_paths,
        source_prefix,
        rust_paths = None,
        rust_source_root = ""):
    """Returns production logical source keys for path-only test artifacts."""
    return sorted(_test_source_context(
        kernel_paths,
        source_prefix,
        rust_paths,
        rust_source_root,
    ).files)

def linux_test_resolve_probe_source_paths(
        parsed,
        node_id,
        kernel_paths,
        source_prefix,
        rust_paths = None,
        rust_source_root = ""):
    """Resolves one parsed node through the production source/root binder."""
    context = _test_source_context(kernel_paths, source_prefix, rust_paths, rust_source_root)
    bindings = _node_source_bindings(node_id, parsed.nodes[node_id], context, {})
    return struct(
        linux_anchor = bindings.linux_anchor.short_path if bindings.linux_anchor != None else "",
        rust_root = bindings.rust_root,
        rust_witness = bindings.rust_witness.short_path if bindings.rust_witness != None else "",
        sources = {
            source: file.short_path
            for source, file in bindings.sources.items()
        },
    )

def linux_test_validate_probe_additional_input_names(names):
    """Exercises strict callback input-key validation before value decoding."""
    return _probe_source_context({name: None for name in names}, "", "")
