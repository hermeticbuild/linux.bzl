"""Bazel 9 feasibility spike for execution-time Kconfig graph expansion."""

load("@rules_cc//cc:action_names.bzl", "C_COMPILE_ACTION_NAME")
load(
    "@rules_cc//cc:find_cc_toolchain.bzl",
    "CC_TOOLCHAIN_TYPE",
    "find_cpp_toolchain",
    "use_cc_toolchain",
)
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")

visibility("//tests/map_directory")

_HEX_DIGITS = "0123456789abcdef"
_PLAN_VERSION = "v1"
_SOURCE_INPUT_PREFIX = "source:"

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
    probe_marker = None
    compile_paths = []
    for path in sorted(children.keys()):
        parts = path.split("/")
        if len(parts) == 3 and parts[0] == _PLAN_VERSION and parts[1] == "probe":
            identity = parts[2]
            if not identity.startswith("sha256-") or not _is_sha256(identity[len("sha256-"):]):
                fail("map_directory plan has invalid measured probe identity %r" % identity)
            if probe_marker != None:
                fail("map_directory plan contains more than one measured probe marker")
            probe_marker = children[path]
        elif len(parts) >= 4 and parts[0] == _PLAN_VERSION and parts[1] == "source":
            index = parts[2]
            canonical_path = "/".join(parts[3:])
            if len(index) != 8 or not _is_decimal(index) or index == "00000000":
                fail("map_directory plan has invalid source index %r" % index)
            _validate_relative_path(canonical_path, "map_directory source marker")
            if index in source_markers:
                fail("map_directory plan repeats source index %s" % index)
            source_key = _SOURCE_INPUT_PREFIX + canonical_path
            if source_key not in additional_inputs:
                fail("map_directory plan selected undeclared source %r" % canonical_path)
            source_markers[index] = struct(
                file = additional_inputs[source_key],
                marker = children[path],
            )
        elif len(parts) >= 5 and parts[0] == _PLAN_VERSION and parts[1] == "compile":
            compile_paths.append(path)
        else:
            fail("map_directory plan contains unknown marker %r" % path)

    if probe_marker == None:
        fail("map_directory plan is missing its measured probe marker")
    if not compile_paths:
        fail("map_directory plan selected no compile actions")

    declared_outputs = {}
    for path in compile_paths:
        marker = children[path]
        parts = path.split("/")
        content_id = parts[2]
        source_index = parts[3]
        object_json = "/".join(parts[4:])
        if not _is_sha256(content_id):
            fail("map_directory plan has invalid compile content ID %r" % content_id)
        if len(source_index) != 8 or source_index not in source_markers:
            fail("map_directory plan compile marker references unknown source index %r" % source_index)
        if not object_json.endswith(".o.json"):
            fail("map_directory plan compile marker does not name an object recipe: %r" % path)
        object_path = object_json[:-len(".json")]
        _validate_relative_path(object_path, "map_directory compile marker")
        output_path = content_id + "/" + object_path
        if output_path in declared_outputs:
            fail("map_directory plan repeats output %r" % output_path)
        declared_outputs[output_path] = True

        source = source_markers[source_index]
        output = template_ctx.declare_file(output_path, directory = output_directory)
        args = template_ctx.args()
        args.add("--target=" + additional_params["target_triple"])
        args.add(additional_params["compile_flag"])
        args.add("-Werror")
        args.add("-c")
        args.add(source.file)
        args.add("-o")
        args.add(output)
        template_ctx.run(
            executable = tools["cc"],
            inputs = [
                marker,
                probe_marker,
                source.file,
                source.marker,
            ],
            tools = [tools["cc_files"]],
            outputs = [output],
            arguments = [args],
            progress_message = "Compiling a compiler-selected Kconfig object",
        )

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

def _tool_file_for_basename(cc_toolchain, basename):
    basenames = [basename, basename + ".exe"]
    matches = [
        file
        for file in cc_toolchain.all_files.to_list()
        if file.basename in basenames
    ]
    if len(matches) != 1:
        fail(
            "selected C/C++ toolchain must provide exactly one %s; found: %s" % (
                basename,
                ", ".join(sorted([file.path for file in matches])) or "none",
            ),
        )
    return matches[0]

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

def _linux_map_directory_kconfig_spike_impl(ctx):
    cc_toolchain = find_cpp_toolchain(ctx)
    feature_configuration = cc_common.configure_features(
        ctx = ctx,
        cc_toolchain = cc_toolchain,
        requested_features = ctx.features,
        unsupported_features = ctx.disabled_features,
    )
    compiler_path = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
    )
    compiler = _tool_file_for_path(cc_toolchain, compiler_path, "C compiler")
    linker = _tool_file_for_basename(cc_toolchain, "ld.lld")
    sources = _canonical_sources(ctx)

    plan = ctx.actions.declare_directory(ctx.label.name + ".plan")
    objects = ctx.actions.declare_directory(ctx.label.name + ".objects")
    validated = ctx.actions.declare_file(ctx.label.name + ".validated")

    plan_args = ctx.actions.args()
    plan_args.add("-root")
    plan_args.add(ctx.file.kconfig)
    plan_args.add("-kbuild")
    plan_args.add(ctx.file.kbuild)
    plan_args.add("-config")
    plan_args.add(ctx.file.config, format = ctx.attr.config_name + "=%s")
    plan_args.add("-generated_headers_for_config")
    plan_args.add(ctx.attr.config_name + "=//tests/map_directory:unused_generated_headers")
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
    plan_args.add(linker)
    plan_args.add("-compact_action_plan_out")
    plan_args.add_all([plan], expand_directories = False)

    ctx.actions.run(
        executable = ctx.attr._kconfig_parse[DefaultInfo].files_to_run,
        inputs = [ctx.file.kconfig, ctx.file.kbuild, ctx.file.config] + ctx.files.srcs,
        tools = cc_toolchain.all_files,
        outputs = [plan],
        arguments = [plan_args],
        mnemonic = "MapDirectoryKconfigPlan",
        progress_message = "Probing the selected compiler and generating a Kconfig action plan for %{label}",
        execution_requirements = {"supports-path-mapping": "1"},
        toolchain = CC_TOOLCHAIN_TYPE,
    )

    additional_inputs = {}
    for canonical_path, file in sources.items():
        additional_inputs[_SOURCE_INPUT_PREFIX + canonical_path] = file
    ctx.actions.map_directory(
        implementation = _map_compiler_selected_objects,
        input_directories = {"plan": plan},
        output_directories = {"objects": objects},
        additional_inputs = additional_inputs,
        tools = {
            "cc": compiler,
            "cc_files": cc_toolchain.all_files,
        },
        additional_params = {
            "compile_flag": "-fno-omit-frame-pointer",
            "target_triple": ctx.attr.target_triple,
        },
        execution_requirements = {"supports-path-mapping": "1"},
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
    ctx.actions.run(
        executable = ctx.executable.validator,
        inputs = [plan, objects],
        outputs = [validated],
        arguments = [validation_args],
        mnemonic = "MapDirectoryKconfigValidate",
        progress_message = "Validating mapped compiler output for %{label}",
        execution_requirements = {"supports-path-mapping": "1"},
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
    },
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
)
