"""Bazel 9 feasibility rules for execution-time Kconfig graph expansion."""

load(
    "@rules_cc//cc:find_cc_toolchain.bzl",
    "CC_TOOLCHAIN_TYPE",
    "find_cpp_toolchain",
    "use_cc_toolchain",
)
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")

visibility("public")

_HEX_DIGITS = "0123456789abcdef"
_PLAN_SCHEMA = "linux-kernel-plan-v2"
_SOURCE_INPUT_PREFIX = "source:"
_KBUILD_ARGS_SENTINEL = "__LINUX_BZL_KBUILD_ARGS_V1__"
_KBUILD_ACTIONS = {
    "ar": "linux-kbuild-ar",
    "as": "linux-kbuild-as",
    "cc": "linux-kbuild-cc",
    "cxx": "linux-kbuild-cxx",
    "ld": "linux-kbuild-ld",
    "nm": "linux-kbuild-nm",
    "objcopy": "linux-kbuild-objcopy",
    "objdump": "linux-kbuild-objdump",
    "cpp": "linux-kbuild-cpp",
    "ranlib": "linux-kbuild-ranlib",
    "readelf": "linux-kbuild-readelf",
    "strip": "linux-kbuild-strip",
}

def _is_decimal(value):
    if not value:
        return False
    for index in range(len(value)):
        if value[index] not in "0123456789":
            return False
    return True

def _is_sha256(value):
    if len(value) != 64:
        return False
    for index in range(len(value)):
        if value[index] not in _HEX_DIGITS:
            return False
    return True

def _validate_relative_path(path, what):
    if not path or path.startswith("/") or path.endswith("/") or "//" in path:
        fail("%s has invalid relative path %r" % (what, path))
    for component in path.split("/"):
        if component in ["", ".", ".."]:
            fail("%s has invalid relative path %r" % (what, path))

def _map_compiler_selected_objects(
        template_ctx,
        input_directories,
        output_directories,
        additional_inputs,
        tools,
        additional_params):
    """Expands the action plan after its producing action has completed."""
    plan = input_directories["plan"]
    output_directory = output_directories["objects"]

    children = {}
    for child in plan.children:
        children[child.tree_relative_path] = child

    source_markers = {}
    recipes = {}
    toolset_marker = None
    nodes = {}
    for path in sorted(children.keys()):
        parts = path.split("/")
        if parts == ["schema", _PLAN_SCHEMA]:
            pass
        elif len(parts) == 3 and parts[0] == "toolsets" and parts[1] == "target":
            identity = parts[2]
            if not identity.startswith("sha256-") or not _is_sha256(identity[len("sha256-"):]):
                fail("map_directory plan has invalid target toolset identity %r" % identity)
            if toolset_marker != None:
                fail("map_directory plan contains more than one target toolset")
            toolset_marker = children[path]
        elif len(parts) >= 4 and parts[0] == "sources":
            source_id = parts[1]
            input_tree = parts[2]
            canonical_path = "/".join(parts[3:])
            if not source_id.startswith("src-") or len(source_id) != 12 or not _is_decimal(source_id[4:]) or source_id == "src-00000000":
                fail("map_directory plan has invalid source ID %r" % source_id)
            if input_tree != "kernel":
                fail("map_directory plan references unknown input tree %r" % input_tree)
            _validate_relative_path(canonical_path, "map_directory source marker")
            if source_id in source_markers:
                fail("map_directory plan repeats source ID %s" % source_id)
            source_key = _SOURCE_INPUT_PREFIX + canonical_path
            if source_key not in additional_inputs:
                fail("map_directory plan selected undeclared source %r" % canonical_path)
            source_markers[source_id] = struct(
                canonical_path = canonical_path,
                file = additional_inputs[source_key],
                marker = children[path],
            )
        elif len(parts) == 2 and parts[0] == "recipes" and parts[1].endswith(".json"):
            recipe_id = parts[1][:-len(".json")]
            if not _is_sha256(recipe_id) or recipe_id in recipes:
                fail("map_directory plan has invalid or repeated recipe ID %r" % recipe_id)
            recipes[recipe_id] = children[path]
        elif len(parts) >= 5 and parts[0] == "nodes" and parts[1] == "target":
            node_id = parts[2]
            if not _is_sha256(node_id):
                fail("map_directory plan has invalid target node ID %r" % node_id)
            if node_id not in nodes:
                nodes[node_id] = {"deps": {}, "sources": {}, "outputs": {}}
            node = nodes[node_id]
            section = parts[3]
            if section in ["kind", "recipe", "tool"] and len(parts) == 5:
                if section in node:
                    fail("map_directory plan repeats %s for node %s" % (section, node_id))
                node[section] = parts[4]
            elif section == "in" and len(parts) == 8 and parts[4] == "source":
                role = parts[5]
                index = parts[6]
                if len(index) != 8 or not _is_decimal(index) or index in node["sources"]:
                    fail("map_directory plan has invalid source input index %r for node %s" % (index, node_id))
                node["sources"][index] = struct(role = role, source_id = parts[7])
            elif section == "in" and len(parts) == 9 and parts[4] == "node":
                role = parts[5]
                index = parts[6]
                producer = parts[7]
                slot = parts[8]
                if len(index) != 8 or not _is_decimal(index) or index in node["deps"] or not _is_sha256(producer) or slot != "00000000":
                    fail("map_directory plan has invalid node input %r for node %s" % (path, node_id))
                node["deps"][index] = struct(producer = producer, role = role, slot = slot)
            elif section == "out" and len(parts) >= 7:
                tree_key = parts[4]
                index = parts[5]
                output_path = "/".join(parts[6:])
                if tree_key != "objects" or len(index) != 8 or not _is_decimal(index) or index in node["outputs"]:
                    fail("map_directory plan has invalid output slot %r for node %s" % (path, node_id))
                _validate_relative_path(output_path, "map_directory node output")
                node["outputs"][index] = output_path
            else:
                fail("map_directory plan contains invalid node marker %r" % path)
        else:
            fail("map_directory plan contains unknown marker %r" % path)

    if "schema/%s" % _PLAN_SCHEMA not in children:
        fail("map_directory plan is missing schema/%s" % _PLAN_SCHEMA)
    if toolset_marker == None:
        fail("map_directory plan is missing its target toolset")
    if not nodes:
        fail("map_directory plan contains no action nodes")

    # The compact plan's source markers are the union of the source-input
    # closures for the selected recipes. Until the plan carries a per-recipe
    # closure, give every mapped compile that conservative union. In
    # particular, declaring only the primary .c file would leave quoted and
    # source-tree headers outside remote/sandboxed actions.
    selected_source_inputs = [
        source_markers[index].file
        for index in sorted(source_markers.keys())
    ]

    declared_paths = {}
    declared_outputs = {}
    for node_id in sorted(nodes.keys()):
        node = nodes[node_id]
        kind = node.get("kind")
        if kind not in ["compile", "composite"] or node.get("tool") != "target":
            fail("map_directory plan has unsupported target node %s kind/tool %r/%r" % (node_id, kind, node.get("tool")))
        recipe_id = node.get("recipe")
        if recipe_id not in recipes:
            fail("map_directory node %s references unknown recipe %r" % (node_id, recipe_id))
        if sorted(node["outputs"].keys()) != ["00000000"]:
            fail("map_directory node %s must have exactly one output" % node_id)
        if kind == "compile":
            if sorted(node["sources"].keys()) != ["00000000"] or node["deps"]:
                fail("map_directory compile node %s must have one source and no node inputs" % node_id)
            source_ref = node["sources"]["00000000"]
            if source_ref.role != "src" or source_ref.source_id not in source_markers:
                fail("map_directory compile node %s references unknown primary source %r" % (node_id, source_ref.source_id))
        elif node["sources"] or not node["deps"]:
            fail("map_directory composite node %s must have node inputs and no source inputs" % node_id)
        object_path = node["outputs"]["00000000"]
        if object_path in declared_paths:
            fail("map_directory plan repeats output %r" % object_path)
        declared_paths[object_path] = True
        declared_outputs[node_id] = template_ctx.declare_file(object_path, directory = output_directory)

    completed = {}
    for _ in range(len(nodes)):
        progress = False
        for node_id in sorted(nodes.keys()):
            if node_id in completed:
                continue
            node = nodes[node_id]
            if any([dep.producer not in completed for dep in node["deps"].values()]):
                continue
            kind = node["kind"]
            recipe_id = node["recipe"]
            output = declared_outputs[node_id]
            args = template_ctx.args()
            args.add("-kind", kind)
            args.add("-recipe", recipes[recipe_id])
            args.add("-tool", tools["cc"] if kind == "compile" else tools["ld"])
            args.add("-output", output)
            args.add("-expected_object", node["outputs"]["00000000"])
            args.add("-expected_content_id", node_id)
            inputs = [recipes[recipe_id], toolset_marker]
            if kind == "compile":
                source_ref = node["sources"]["00000000"]
                source = source_markers[source_ref.source_id]
                args.add("-source", source.file)
                args.add("-expected_source", source.canonical_path)
                inputs = selected_source_inputs + inputs + [source.marker]
            else:
                for index in sorted(node["deps"].keys()):
                    dep = node["deps"][index]
                    if dep.role != "member" or dep.producer not in declared_outputs:
                        fail("map_directory composite node %s has invalid member %s" % (node_id, dep.producer))
                    args.add("-input", declared_outputs[dep.producer])
                    inputs.append(declared_outputs[dep.producer])
            param_prefix = kind + "_action_arg_"
            for index in range(int(additional_params[kind + "_action_arg_count"])):
                args.add("-action_arg", additional_params[param_prefix + str(index)])
            template_ctx.run(
                executable = tools["recipe_runner"],
                inputs = inputs,
                tools = [tools["cc_files"]],
                outputs = [output],
                arguments = [args],
                progress_message = "Executing a compiler-selected Kconfig %s recipe" % kind,
            )
            completed[node_id] = True
            progress = True
        if not progress and len(completed) != len(nodes):
            fail("map_directory plan contains a cycle or an unknown producer")

def _tool_file_for_path(cc_toolchain, tool_path, name):
    matches = [
        file
        for file in cc_toolchain.all_files.to_list()
        if file.path == tool_path
    ]
    if len(matches) != 1:
        fail(
            "selected C/C++ toolchain must provide exactly one File for %s path %r; found: %s" % (
                name,
                tool_path,
                ", ".join(sorted([file.path for file in matches])) or "none",
            ),
        )
    return matches[0]

def _configured_kbuild_tools(cc_toolchain, feature_configuration):
    tools = {}
    arguments = {}
    environment = None
    requirements = None
    variables = cc_common.create_compile_variables(
        feature_configuration = feature_configuration,
        cc_toolchain = cc_toolchain,
    )
    for role, action_name in _KBUILD_ACTIONS.items():
        tool_path = cc_common.get_tool_for_action(
            feature_configuration = feature_configuration,
            action_name = action_name,
        )
        tools[role] = _tool_file_for_path(cc_toolchain, tool_path, role)
        argv = cc_common.get_memory_inefficient_command_line(
            feature_configuration = feature_configuration,
            action_name = action_name,
            variables = variables,
        )
        if len([argument for argument in argv if argument == _KBUILD_ARGS_SENTINEL]) != 1:
            fail("C/C++ toolchain action %r must contain exactly one %s argument; got %r" % (action_name, _KBUILD_ARGS_SENTINEL, argv))
        before = []
        after = []
        found_sentinel = False
        for argument in argv:
            if argument == _KBUILD_ARGS_SENTINEL:
                found_sentinel = True
            elif found_sentinel:
                after.append(argument)
            else:
                before.append(argument)
        arguments[role] = struct(after = after, before = before)
        action_environment = dict(cc_common.get_environment_variables(
            feature_configuration = feature_configuration,
            action_name = action_name,
            variables = variables,
        ))
        action_requirements = sorted(cc_common.get_execution_requirements(
            feature_configuration = feature_configuration,
            action_name = action_name,
        ))
        if environment == None:
            environment = action_environment
            requirements = action_requirements
        elif environment != action_environment or requirements != action_requirements:
            fail("all linux-kbuild-* C/C++ toolchain actions must use one environment and execution-requirements contract")
    return struct(
        arguments = arguments,
        environment = environment or {},
        requirements = {requirement: "1" for requirement in requirements or []},
        tools = tools,
    )

def _selected_probe_tools(kbuild):
    return struct(
        archiver = kbuild.tools["ar"],
        linker = kbuild.tools["ld"],
        nm = kbuild.tools["nm"],
        objcopy = kbuild.tools["objcopy"],
    )

def _canonical_sources(ctx):
    prefix = ctx.label.package + "/" if ctx.label.package else ""
    sources = {}
    for file in ctx.files.srcs:
        path = file.short_path
        if not path.startswith(prefix):
            fail("map_directory spike source %s is outside package %s" % (file, ctx.label.package))
        canonical_path = path[len(prefix):]
        _validate_relative_path(canonical_path, "map_directory candidate source")
        if canonical_path in sources:
            fail("map_directory spike repeats candidate source %r" % canonical_path)
        sources[canonical_path] = file
    return sources

def _configured_compile_action(_ctx, cc_toolchain, feature_configuration):
    kbuild = _configured_kbuild_tools(cc_toolchain, feature_configuration)
    return struct(
        action_args = kbuild.arguments["cc"].before + [_KBUILD_ARGS_SENTINEL] + kbuild.arguments["cc"].after,
        environment = kbuild.environment,
        probe_prefix = kbuild.arguments["cc"].before,
        probe_suffix = kbuild.arguments["cc"].after,
    )

def _configured_linker_driver_prefix(_ctx, cc_toolchain, feature_configuration):
    kbuild = _configured_kbuild_tools(cc_toolchain, feature_configuration)
    return struct(environment = kbuild.environment, flags = kbuild.arguments["ld"].before, suffix_flags = kbuild.arguments["ld"].after)

# Shared by the prototype and the production config-materialization action so
# they measure exactly the same selected Bazel C/C++ toolchain invocation.
linux_kconfig_toolchain_probe_helpers = struct(
    configured_kbuild_tools = _configured_kbuild_tools,
    configured_compile_action = _configured_compile_action,
    configured_linker_driver_prefix = _configured_linker_driver_prefix,
    selected_probe_tools = _selected_probe_tools,
    tool_file_for_path = _tool_file_for_path,
)

def _linux_map_directory_kconfig_spike_impl(ctx):
    cc_toolchain = find_cpp_toolchain(ctx)
    feature_configuration = cc_common.configure_features(
        ctx = ctx,
        cc_toolchain = cc_toolchain,
        requested_features = ctx.features,
        unsupported_features = ctx.disabled_features,
    )
    kbuild = _configured_kbuild_tools(cc_toolchain, feature_configuration)
    compiler = kbuild.tools["cc"]
    compiler_prefix = kbuild.arguments["cc"].before
    compiler_suffix = kbuild.arguments["cc"].after
    linker_driver = struct(flags = kbuild.arguments["ld"].before, suffix_flags = kbuild.arguments["ld"].after)
    compile_action = struct(action_args = compiler_prefix + [_KBUILD_ARGS_SENTINEL] + compiler_suffix)
    tool_environment = dict(kbuild.environment)
    tool_environment["EXECROOT"] = "."
    execution_requirements = dict(kbuild.requirements)
    execution_requirements["supports-path-mapping"] = "1"
    probe_tools = _selected_probe_tools(kbuild)
    sources = _canonical_sources(ctx)

    plan = ctx.actions.declare_directory(ctx.label.name + ".plan")
    objects = ctx.actions.declare_directory(ctx.label.name + ".objects-v2")
    validated = ctx.actions.declare_file(ctx.label.name + ".validated")

    plan_args = ctx.actions.args()
    plan_args.add("-root")
    plan_args.add(ctx.file.kconfig)
    plan_args.add("-kbuild")
    plan_args.add(ctx.file.kbuild)
    plan_args.add("-config")
    plan_args.add(ctx.file.config, format = ctx.attr.config_name + "=%s")
    plan_args.add("-generated_headers_for_config")
    plan_args.add(ctx.attr.config_name + "=" + str(ctx.label) + "_unused_generated_headers")
    plan_args.add("-compile_environment_abi")
    plan_args.add("map-directory-spike/" + ctx.attr.target_profile)
    plan_args.add("-target_profile")
    plan_args.add(ctx.attr.target_profile)
    plan_args.add("-linux_arch")
    plan_args.add(ctx.attr.linux_arch)
    plan_args.add("-target_triple")
    plan_args.add(ctx.attr.target_triple)
    plan_args.add("-probe_cc")
    plan_args.add(compiler)
    plan_args.add("-probe_ld")
    plan_args.add(probe_tools.linker)
    plan_args.add("-probe_ar")
    plan_args.add(probe_tools.archiver)
    plan_args.add("-probe_nm")
    plan_args.add(probe_tools.nm)
    plan_args.add("-probe_objcopy")
    plan_args.add(probe_tools.objcopy)
    for flag in compiler_prefix:
        plan_args.add("-probe_cc_arg")
        plan_args.add(flag)
    for flag in compiler_suffix:
        plan_args.add("-probe_cc_suffix_arg")
        plan_args.add(flag)
    for flag in linker_driver.flags:
        plan_args.add("-probe_ld_arg")
        plan_args.add(flag)
    for flag in linker_driver.suffix_flags:
        plan_args.add("-probe_ld_suffix_arg")
        plan_args.add(flag)
    plan_args.add("-compact_action_plan_out")
    plan_args.add_all([plan], expand_directories = False)

    ctx.actions.run(
        executable = ctx.attr._kconfig_parse[DefaultInfo].files_to_run,
        inputs = [ctx.file.kconfig, ctx.file.kbuild, ctx.file.config] + ctx.files.srcs,
        tools = depset(
            direct = [probe_tools.archiver, probe_tools.linker, probe_tools.nm, probe_tools.objcopy],
            transitive = [cc_toolchain.all_files],
        ),
        outputs = [plan],
        arguments = [plan_args],
        mnemonic = "MapDirectoryKconfigPlan",
        progress_message = "Probing the selected compiler and generating a Kconfig action plan for %{label}",
        execution_requirements = execution_requirements,
        env = tool_environment,
        toolchain = CC_TOOLCHAIN_TYPE,
    )

    additional_inputs = {}
    for canonical_path, file in sources.items():
        additional_inputs[_SOURCE_INPUT_PREFIX + canonical_path] = file
    map_params = {"compile_action_arg_count": str(len(compile_action.action_args))}
    for index, flag in enumerate(compile_action.action_args):
        map_params["compile_action_arg_%d" % index] = flag
    linker_action_args = kbuild.arguments["ld"].before + [_KBUILD_ARGS_SENTINEL] + kbuild.arguments["ld"].after
    map_params["composite_action_arg_count"] = str(len(linker_action_args))
    for index, flag in enumerate(linker_action_args):
        map_params["composite_action_arg_%d" % index] = flag
    ctx.actions.map_directory(
        implementation = _map_compiler_selected_objects,
        input_directories = {"plan": plan},
        output_directories = {"objects": objects},
        additional_inputs = additional_inputs,
        tools = {
            "cc": compiler,
            "cc_files": cc_toolchain.all_files,
            "ld": probe_tools.linker,
            "recipe_runner": ctx.attr._recipe_runner[DefaultInfo].files_to_run,
        },
        additional_params = map_params,
        execution_requirements = execution_requirements,
        env = tool_environment,
        mnemonic = "MapDirectoryKconfigCompile",
        toolchain = CC_TOOLCHAIN_TYPE,
    )

    validation_args = ctx.actions.args()
    validation_args.add("-plan")
    validation_args.add_all([plan], expand_directories = False)
    validation_args.add("-objects")
    validation_args.add_all([objects], expand_directories = False)
    validation_args.add("-out")
    validation_args.add(validated)
    validation_args.add("-compiler")
    validation_args.add(compiler)
    validation_args.add("-linker")
    validation_args.add(probe_tools.linker)
    validation_args.add("-archiver")
    validation_args.add(probe_tools.archiver)
    validation_args.add("-nm")
    validation_args.add(probe_tools.nm)
    validation_args.add("-objcopy")
    validation_args.add(probe_tools.objcopy)
    validation_args.add("-target_profile")
    validation_args.add(ctx.attr.target_profile)
    validation_args.add("-linux_arch")
    validation_args.add(ctx.attr.linux_arch)
    validation_args.add("-target_triple")
    validation_args.add(ctx.attr.target_triple)
    for flag in compiler_prefix:
        validation_args.add("-compiler_arg")
        validation_args.add(flag)
    for flag in compiler_suffix:
        validation_args.add("-compiler_suffix_arg")
        validation_args.add(flag)
    for flag in linker_driver.flags:
        validation_args.add("-linker_arg")
        validation_args.add(flag)
    for flag in linker_driver.suffix_flags:
        validation_args.add("-linker_suffix_arg")
        validation_args.add(flag)
    ctx.actions.run(
        executable = ctx.executable.validator,
        inputs = [
            plan,
            objects,
            compiler,
            probe_tools.archiver,
            probe_tools.linker,
            probe_tools.nm,
            probe_tools.objcopy,
        ],
        tools = [cc_toolchain.all_files],
        outputs = [validated],
        arguments = [validation_args],
        mnemonic = "MapDirectoryKconfigValidate",
        progress_message = "Validating mapped compiler output for %{label}",
        execution_requirements = {"supports-path-mapping": "1"},
        env = tool_environment,
    )

    return [
        DefaultInfo(files = depset([validated])),
        OutputGroupInfo(
            objects = depset([objects]),
            plan = depset([plan]),
        ),
    ]

linux_map_directory_kconfig_spike = rule(
    implementation = _linux_map_directory_kconfig_spike_impl,
    attrs = {
        "config": attr.label(allow_single_file = True, mandatory = True),
        "config_name": attr.string(default = "spike"),
        "kbuild": attr.label(allow_single_file = True, mandatory = True),
        "kconfig": attr.label(allow_single_file = True, mandatory = True),
        "linux_arch": attr.string(default = "x86"),
        "srcs": attr.label_list(allow_files = [".c", ".h"], mandatory = True),
        "target_profile": attr.string(default = "x86_64"),
        "target_triple": attr.string(default = "x86_64-linux-gnu"),
        "validator": attr.label(cfg = "exec", executable = True, mandatory = True),
        "_kconfig_parse": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/kconfig_parse:kconfig_parse"),
            executable = True,
        ),
        "_recipe_runner": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/mapdirectoryrecipe"),
            executable = True,
        ),
    },
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
)

def _linux_upstream_kconfig_impl(ctx):
    """Resolves the complete upstream Kconfig tree with the selected compiler."""
    cc_toolchain = find_cpp_toolchain(ctx)
    feature_configuration = cc_common.configure_features(
        ctx = ctx,
        cc_toolchain = cc_toolchain,
        requested_features = ctx.features,
        unsupported_features = ctx.disabled_features,
    )
    kbuild = _configured_kbuild_tools(cc_toolchain, feature_configuration)
    compiler = kbuild.tools["cc"]
    probe_tools = _selected_probe_tools(kbuild)
    compiler_prefix = kbuild.arguments["cc"].before
    compiler_suffix = kbuild.arguments["cc"].after
    linker_driver = struct(flags = kbuild.arguments["ld"].before, suffix_flags = kbuild.arguments["ld"].after)
    tool_environment = dict(kbuild.environment)
    tool_environment["EXECROOT"] = "."
    execution_requirements = dict(kbuild.requirements)
    execution_requirements["supports-path-mapping"] = "1"

    resolved = ctx.actions.declare_file(ctx.label.name + ".config")
    auto_conf = ctx.actions.declare_file(ctx.label.name + ".auto.conf")
    auto_conf_cmd = ctx.actions.declare_file(ctx.label.name + ".auto.conf.cmd")
    autoconf = ctx.actions.declare_file(ctx.label.name + ".autoconf.h")
    rustc_cfg = ctx.actions.declare_file(ctx.label.name + ".rustc_cfg")
    kernel_release = ctx.actions.declare_file(ctx.label.name + ".kernel.release")
    validated = ctx.actions.declare_file(ctx.label.name + ".validated")

    args = ctx.actions.args()
    args.add("-root")
    args.add(ctx.file.kconfig)
    args.add("-srctree")
    args.add(ctx.file.kconfig.dirname)
    args.add("-resolve_config")
    args.add(ctx.file.config, format = ctx.attr.config_name + "=%s")
    args.add("-resolved_config_out")
    args.add(resolved)
    args.add("-resolved_auto_conf_out")
    args.add(auto_conf)
    args.add("-resolved_auto_conf_cmd_out")
    args.add(auto_conf_cmd)
    args.add("-resolved_autoconf_out")
    args.add(autoconf)
    args.add("-resolved_rustc_cfg_out")
    args.add(rustc_cfg)
    args.add("-resolved_kernel_release_out")
    args.add(kernel_release)
    args.add("-kernel_version")
    args.add("6.18.39")
    args.add("-target_profile")
    args.add("x86_64")
    args.add("-linux_arch")
    args.add("x86")
    args.add("-target_triple")
    args.add("x86_64-linux-gnu")
    args.add("-probe_cc")
    args.add(compiler)
    args.add("-probe_ld")
    args.add(probe_tools.linker)
    args.add("-probe_ar")
    args.add(probe_tools.archiver)
    args.add("-probe_nm")
    args.add(probe_tools.nm)
    args.add("-probe_objcopy")
    args.add(probe_tools.objcopy)

    for flag in compiler_prefix:
        args.add("-probe_cc_arg")
        args.add(flag)
    for flag in compiler_suffix:
        args.add("-probe_cc_suffix_arg")
        args.add(flag)
    for flag in linker_driver.flags:
        args.add("-probe_ld_arg")
        args.add(flag)
    for flag in linker_driver.suffix_flags:
        args.add("-probe_ld_suffix_arg")
        args.add(flag)

    resolved_outputs = [
        resolved,
        auto_conf,
        auto_conf_cmd,
        autoconf,
        rustc_cfg,
        kernel_release,
    ]
    ctx.actions.run(
        executable = ctx.attr._kconfig_parse[DefaultInfo].files_to_run,
        inputs = depset(
            direct = [ctx.file.kconfig, ctx.file.config],
            transitive = [ctx.attr.kconfig_files[DefaultInfo].files],
        ),
        tools = depset(
            direct = [probe_tools.archiver, probe_tools.linker, probe_tools.nm, probe_tools.objcopy],
            transitive = [cc_toolchain.all_files],
        ),
        outputs = resolved_outputs,
        arguments = [args],
        mnemonic = "UpstreamKconfigResolve",
        progress_message = "Resolving upstream Linux 6.18.39 Kconfig with the selected compiler for %{label}",
        execution_requirements = execution_requirements,
        env = tool_environment,
        toolchain = CC_TOOLCHAIN_TYPE,
    )

    validation_args = ctx.actions.args()
    validation_args.add("-config")
    validation_args.add(resolved)
    validation_args.add("-out")
    validation_args.add(validated)
    ctx.actions.run(
        executable = ctx.executable.validator,
        inputs = [resolved],
        outputs = [validated],
        arguments = [validation_args],
        mnemonic = "UpstreamKconfigValidate",
        progress_message = "Validating upstream compiler-derived Kconfig fields for %{label}",
        execution_requirements = {"supports-path-mapping": "1"},
    )
    return [
        DefaultInfo(files = depset([validated])),
        OutputGroupInfo(resolved_config = depset(resolved_outputs)),
    ]

linux_upstream_kconfig = rule(
    implementation = _linux_upstream_kconfig_impl,
    attrs = {
        "config": attr.label(allow_single_file = True, mandatory = True),
        "config_name": attr.string(default = "upstream"),
        "kconfig": attr.label(allow_single_file = True, mandatory = True),
        "kconfig_files": attr.label(mandatory = True),
        "validator": attr.label(cfg = "exec", executable = True, mandatory = True),
        "_kconfig_parse": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/kconfig_parse:kconfig_parse"),
            executable = True,
        ),
    },
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
)

def _map_directory_probe_identity_comparison_impl(ctx):
    output = ctx.actions.declare_file(ctx.label.name + ".validated")
    args = ctx.actions.args()
    args.add("-plan")
    args.add_all([ctx.file.first], expand_directories = False)
    args.add("-other_plan")
    args.add_all([ctx.file.second], expand_directories = False)
    args.add("-out")
    args.add(output)
    ctx.actions.run(
        executable = ctx.executable.validator,
        inputs = [ctx.file.first, ctx.file.second],
        outputs = [output],
        arguments = [args],
        mnemonic = "MapDirectoryKconfigCompare",
        progress_message = "Comparing compiler-selected Kconfig plan identities for %{label}",
        execution_requirements = {"supports-path-mapping": "1"},
    )
    return DefaultInfo(files = depset([output]))

map_directory_probe_identity_comparison = rule(
    implementation = _map_directory_probe_identity_comparison_impl,
    attrs = {
        "first": attr.label(allow_single_file = True, mandatory = True),
        "second": attr.label(allow_single_file = True, mandatory = True),
        "validator": attr.label(cfg = "exec", executable = True, mandatory = True),
    },
)
