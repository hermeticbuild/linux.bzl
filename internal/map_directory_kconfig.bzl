"""Bazel 9 feasibility spike for execution-time Kconfig graph expansion."""

load(
    "@rules_cc//cc:action_names.bzl",
    "CPP_LINK_EXECUTABLE_ACTION_NAME",
    "C_COMPILE_ACTION_NAME",
)
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
_GCC_TOOLCHAIN_SETTING = "//tests/map_directory:use_gcc_toolchain"
_COMPILE_OUTPUT_SENTINEL = "__linux_bzl_map_output__.o"
_COMPILE_RECIPE_FLAGS_SENTINEL = "-D__LINUX_BZL_MAP_RECIPE_FLAGS__"
_COMPILE_SOURCE_SENTINEL = "__linux_bzl_map_source__.c"

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
                canonical_path = canonical_path,
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

    # The compact plan's source markers are the union of the source-input
    # closures for the selected recipes. Until the plan carries a per-recipe
    # closure, give every mapped compile that conservative union. In
    # particular, declaring only the primary .c file would leave quoted and
    # source-tree headers outside remote/sandboxed actions.
    selected_source_inputs = [
        source_markers[index].file
        for index in sorted(source_markers.keys())
    ]

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
        args.add("-recipe")
        args.add(marker)
        args.add("-compiler")
        args.add(tools["cc"])
        args.add("-source")
        args.add(source.file)
        args.add("-output")
        args.add(output)
        args.add("-expected_source")
        args.add(source.canonical_path)
        args.add("-expected_object")
        args.add(object_path)
        args.add("-expected_content_id")
        args.add(content_id)
        for index in range(int(additional_params["action_arg_count"])):
            args.add("-action_arg")
            args.add(additional_params["action_arg_%d" % index])
        template_ctx.run(
            executable = tools["recipe_runner"],
            inputs = selected_source_inputs + [
                marker,
                probe_marker,
                source.marker,
            ],
            tools = [tools["cc_files"]],
            outputs = [output],
            arguments = [args],
            progress_message = "Replaying a compiler-selected Kconfig recipe",
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

def _tool_file_for_sibling(cc_toolchain, tool, basename, name):
    path = tool.dirname + "/" + basename
    return _tool_file_for_path(cc_toolchain, path, name)

def _single_file(target, name):
    files = target[DefaultInfo].files.to_list()
    if len(files) != 1:
        fail("%s must provide exactly one executable File; found: %s" % (
            name,
            ", ".join(sorted([file.path for file in files])) or "none",
        ))
    return files[0]

def _selected_probe_tools(ctx, cc_toolchain, feature_configuration, compiler, compiler_family):
    if compiler_family == "clang":
        link_driver_path = cc_common.get_tool_for_action(
            feature_configuration = feature_configuration,
            action_name = CPP_LINK_EXECUTABLE_ACTION_NAME,
        )
        link_driver = _tool_file_for_path(cc_toolchain, link_driver_path, "C++ link driver")
        return struct(
            archiver = _tool_file_for_sibling(cc_toolchain, compiler, "llvm-ar", "archiver"),
            linker = _tool_file_for_sibling(cc_toolchain, link_driver, "ld.lld", "linker"),
            nm = _tool_file_for_sibling(cc_toolchain, compiler, "llvm-nm", "nm"),
            objcopy = _tool_file_for_sibling(cc_toolchain, compiler, "llvm-objcopy", "objcopy"),
        )
    return struct(
        archiver = _single_file(ctx.attr._gcc_ar, "GCC test archiver"),
        linker = _single_file(ctx.attr._gcc_ld, "GCC test linker"),
        nm = _single_file(ctx.attr._gcc_nm, "GCC test nm"),
        objcopy = _single_file(ctx.attr._gcc_objcopy, "GCC test objcopy"),
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

def _configured_compile_action(ctx, cc_toolchain, feature_configuration):
    variables = cc_common.create_compile_variables(
        feature_configuration = feature_configuration,
        cc_toolchain = cc_toolchain,
        output_file = _COMPILE_OUTPUT_SENTINEL,
        source_file = _COMPILE_SOURCE_SENTINEL,
        user_compile_flags = ctx.fragments.cpp.copts + ctx.fragments.cpp.conlyopts + [_COMPILE_RECIPE_FLAGS_SENTINEL],
    )
    action_args = cc_common.get_memory_inefficient_command_line(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
        variables = variables,
    )
    if len([argument for argument in action_args if argument == _COMPILE_RECIPE_FLAGS_SENTINEL]) != 1:
        fail("selected C/C++ toolchain must preserve exactly one Kbuild flags marker")
    if not any([_COMPILE_SOURCE_SENTINEL in argument for argument in action_args]):
        fail("selected C/C++ toolchain compile command does not contain its source operand")
    if not any([_COMPILE_OUTPUT_SENTINEL in argument for argument in action_args]):
        fail("selected C/C++ toolchain compile command does not contain its output operand")

    controlled = {}
    for index, argument in enumerate(action_args):
        if argument == _COMPILE_RECIPE_FLAGS_SENTINEL or argument == "-c" or _COMPILE_SOURCE_SENTINEL in argument or _COMPILE_OUTPUT_SENTINEL in argument:
            controlled[index] = True
            if index > 0 and action_args[index - 1] in ["-o", "--output"]:
                controlled[index - 1] = True
    flags = [
        argument
        for index, argument in enumerate(action_args)
        if index not in controlled
    ]
    prohibited = [
        "--output",
        "--save-temps",
        "--serialize-diagnostics",
        "-E",
        "-M",
        "-MD",
        "-MF",
        "-MJ",
        "-MM",
        "-MMD",
        "-MQ",
        "-MT",
        "-S",
        "-c",
        "-o",
        "-save-temps",
    ]
    prohibited_prefixes = [
        "--output=",
        "--save-temps=",
        "--serialize-diagnostics=",
        "-MF=",
        "-MJ=",
        "-save-temps=",
    ]
    for flag in flags:
        if flag.startswith("@") or flag in prohibited:
            fail("selected C/C++ toolchain produced unsafe compiler prefix argument %r" % flag)
        for prefix in prohibited_prefixes:
            if flag.startswith(prefix):
                fail("selected C/C++ toolchain produced unsafe compiler prefix argument %r" % flag)
    return struct(
        action_args = action_args,
        environment = dict(cc_common.get_environment_variables(
            feature_configuration = feature_configuration,
            action_name = C_COMPILE_ACTION_NAME,
            variables = variables,
        )),
        probe_prefix = flags,
    )

def _configured_linker_driver_prefix(ctx, cc_toolchain, feature_configuration):
    """Returns selected executable-link flags without Bazel-owned operands."""
    sentinel = "__linux_bzl_probe_output__"
    variables = cc_common.create_link_variables(
        cc_toolchain = cc_toolchain,
        feature_configuration = feature_configuration,
        is_linking_dynamic_library = False,
        is_using_linker = True,
        output_file = sentinel,
        user_link_flags = ctx.fragments.cpp.linkopts,
    )
    command_line = cc_common.get_memory_inefficient_command_line(
        feature_configuration = feature_configuration,
        action_name = CPP_LINK_EXECUTABLE_ACTION_NAME,
        variables = variables,
    )
    flags = []
    skip_output = False
    expect_operand = None
    safe_operand_options = [
        "-resource-dir",
        "-target",
    ]
    for argument in command_line:
        if skip_output:
            if argument != sentinel:
                fail("selected C/C++ toolchain link command has unexpected output operand %r" % argument)
            skip_output = False
        elif expect_operand != None:
            if not argument or argument.startswith("-") or argument.startswith("@"):
                fail("selected C/C++ toolchain link command has unsafe %s operand %r" % (expect_operand, argument))
            flags.append(argument)
            expect_operand = None
        elif argument == "-o":
            skip_output = True
        elif argument == sentinel or argument == "--output=" + sentinel:
            pass
        elif argument in safe_operand_options:
            flags.append(argument)
            expect_operand = argument
        elif argument.startswith("@") or not argument.startswith("-"):
            fail("selected C/C++ toolchain produced unsafe linker-driver prefix argument %r" % argument)
        elif argument in ["-c", "-E", "-S", "--output"]:
            fail("selected C/C++ toolchain produced unsafe linker-driver prefix argument %r" % argument)
        else:
            flags.append(argument)
    if skip_output:
        fail("selected C/C++ toolchain link command is missing its output operand")
    if expect_operand != None:
        fail("selected C/C++ toolchain link command is missing its %s operand" % expect_operand)
    return struct(
        environment = dict(cc_common.get_environment_variables(
            feature_configuration = feature_configuration,
            action_name = CPP_LINK_EXECUTABLE_ACTION_NAME,
            variables = variables,
        )),
        flags = flags,
    )

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
    compile_action = _configured_compile_action(ctx, cc_toolchain, feature_configuration)
    compiler_prefix = compile_action.probe_prefix
    linker_driver = _configured_linker_driver_prefix(ctx, cc_toolchain, feature_configuration)
    tool_environment = dict(compile_action.environment)
    tool_environment["EXECROOT"] = "."
    for name, value in linker_driver.environment.items():
        if name in tool_environment and tool_environment[name] != value:
            fail("map_directory spike has conflicting compile/link toolchain environment %s" % name)
        tool_environment[name] = value
    compiler_id = cc_toolchain.compiler.lower()
    compiler_family = ""
    if "clang" in compiler_id:
        compiler_family = "clang"
    elif "gcc" in compiler_id:
        compiler_family = "gcc"
    else:
        fail("map_directory spike does not recognize selected compiler kind %r for %s" % (cc_toolchain.compiler, compiler.path))
    probe_tools = _selected_probe_tools(ctx, cc_toolchain, feature_configuration, compiler, compiler_family)
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
    for flag in linker_driver.flags:
        plan_args.add("-probe_link_arg")
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
        execution_requirements = {"supports-path-mapping": "1"},
        env = tool_environment,
        toolchain = CC_TOOLCHAIN_TYPE,
    )

    additional_inputs = {}
    for canonical_path, file in sources.items():
        additional_inputs[_SOURCE_INPUT_PREFIX + canonical_path] = file
    map_params = {"action_arg_count": str(len(compile_action.action_args))}
    for index, flag in enumerate(compile_action.action_args):
        map_params["action_arg_%d" % index] = flag
    ctx.actions.map_directory(
        implementation = _map_compiler_selected_objects,
        input_directories = {"plan": plan},
        output_directories = {"objects": objects},
        additional_inputs = additional_inputs,
        tools = {
            "cc": compiler,
            "cc_files": cc_toolchain.all_files,
            "recipe_runner": ctx.attr._recipe_runner[DefaultInfo].files_to_run,
        },
        additional_params = map_params,
        execution_requirements = {"supports-path-mapping": "1"},
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
    for flag in linker_driver.flags:
        validation_args.add("-linker_driver_arg")
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
        "_gcc_ar": attr.label(default = Label("@map_directory_gcc_x86_64//:ar")),
        "_gcc_ld": attr.label(default = Label("@map_directory_gcc_x86_64//:ld")),
        "_gcc_nm": attr.label(default = Label("@map_directory_gcc_x86_64//:nm")),
        "_gcc_objcopy": attr.label(default = Label("@map_directory_gcc_x86_64//:objcopy")),
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

def _linux_upstream_gcc_kconfig_impl(ctx):
    """Resolves the complete upstream Kconfig tree with the selected GCC."""
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
    if "gcc" not in cc_toolchain.compiler.lower():
        fail("upstream GCC Kconfig test selected compiler kind %r, want gcc" % cc_toolchain.compiler)
    probe_tools = _selected_probe_tools(ctx, cc_toolchain, feature_configuration, compiler, "gcc")
    compile_action = _configured_compile_action(ctx, cc_toolchain, feature_configuration)
    compiler_prefix = compile_action.probe_prefix
    linker_driver = _configured_linker_driver_prefix(ctx, cc_toolchain, feature_configuration)
    tool_environment = dict(compile_action.environment)
    tool_environment["EXECROOT"] = "."
    for name, value in linker_driver.environment.items():
        if name in tool_environment and tool_environment[name] != value:
            fail("upstream GCC Kconfig test has conflicting compile/link toolchain environment %s" % name)
        tool_environment[name] = value

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

    # gcc_toolchain's sysroot is materialized through absolute symlinks and its
    # plugin headers are not exposed as declared targets. Fail these two
    # capabilities closed explicitly, so local sandbox, standalone, and remote
    # execution cannot select different Kconfig graphs from ambient files.
    args.add("-probe_allow_cc_can_link=false")
    args.add("-probe_allow_gcc_plugins=false")
    for flag in compiler_prefix:
        args.add("-probe_cc_arg")
        args.add(flag)
    for flag in linker_driver.flags:
        args.add("-probe_link_arg")
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
        mnemonic = "UpstreamGCCKconfigResolve",
        progress_message = "Resolving upstream Linux 6.18.39 Kconfig with selected GCC for %{label}",
        execution_requirements = {"supports-path-mapping": "1"},
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
        mnemonic = "UpstreamGCCKconfigValidate",
        progress_message = "Validating upstream GCC Kconfig fields for %{label}",
        execution_requirements = {"supports-path-mapping": "1"},
    )
    return [
        DefaultInfo(files = depset([validated])),
        OutputGroupInfo(resolved_config = depset(resolved_outputs)),
    ]

_linux_upstream_gcc_kconfig = rule(
    implementation = _linux_upstream_gcc_kconfig_impl,
    attrs = {
        "config": attr.label(allow_single_file = True, mandatory = True),
        "config_name": attr.string(default = "upstream_gcc"),
        "kconfig": attr.label(allow_single_file = True, mandatory = True),
        "kconfig_files": attr.label(mandatory = True),
        "validator": attr.label(cfg = "exec", executable = True, mandatory = True),
        "_gcc_ar": attr.label(default = Label("@map_directory_gcc_x86_64//:ar")),
        "_gcc_ld": attr.label(default = Label("@map_directory_gcc_x86_64//:ld")),
        "_gcc_nm": attr.label(default = Label("@map_directory_gcc_x86_64//:nm")),
        "_gcc_objcopy": attr.label(default = Label("@map_directory_gcc_x86_64//:objcopy")),
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

def _select_gcc_toolchain_impl(settings, _attr):
    return {
        _GCC_TOOLCHAIN_SETTING: True,
        "//command_line_option:copt": settings["//command_line_option:copt"] + [
            "-DMAP_DIRECTORY_TOOLCHAIN_PREFIX_REPLAYED=1",
        ],
    }

_select_gcc_toolchain = transition(
    implementation = _select_gcc_toolchain_impl,
    inputs = ["//command_line_option:copt"],
    outputs = [
        _GCC_TOOLCHAIN_SETTING,
        "//command_line_option:copt",
    ],
)

def _gcc_spike_transition_impl(ctx):
    actual = ctx.attr.actual[0]
    providers = [actual[DefaultInfo]]
    if OutputGroupInfo in actual:
        providers.append(actual[OutputGroupInfo])
    return providers

_gcc_spike_transition = rule(
    implementation = _gcc_spike_transition_impl,
    attrs = {
        "actual": attr.label(cfg = _select_gcc_toolchain, mandatory = True),
        "_allowlist_function_transition": attr.label(
            default = "@bazel_tools//tools/allowlists/function_transition_allowlist",
        ),
    },
)

def linux_map_directory_gcc_kconfig_spike(name, tags = [], visibility = None, **kwargs):
    """Creates a private GCC-selected variant of the map_directory spike."""
    actual_name = name + "_gcc_impl"
    linux_map_directory_kconfig_spike(
        name = actual_name,
        tags = tags,
        visibility = ["//visibility:private"],
        **kwargs
    )
    _gcc_spike_transition(
        name = name,
        actual = ":" + actual_name,
        tags = tags,
        visibility = visibility,
    )

def linux_upstream_gcc_kconfig_test(name, tags = [], visibility = None, **kwargs):
    """Resolves upstream Linux Kconfig under the registered hermetic GCC."""
    actual_name = name + "_gcc_impl"
    _linux_upstream_gcc_kconfig(
        name = actual_name,
        tags = tags,
        visibility = ["//visibility:private"],
        **kwargs
    )
    _gcc_spike_transition(
        name = name,
        actual = ":" + actual_name,
        tags = tags,
        visibility = visibility,
    )
