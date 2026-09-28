"""Out-of-tree modules planned by the same source-derived Kbuild backend as Linux."""

load("@rules_bison//bison:toolchain_type.bzl", "BISON_TOOLCHAIN_TYPE")
load("@rules_cc//cc:find_cc_toolchain.bzl", "CC_TOOLCHAIN_TYPE", "use_cc_toolchain")
load("@rules_flex//flex:toolchain_type.bzl", "FLEX_TOOLCHAIN_TYPE")
load("@rules_m4//m4:toolchain_type.bzl", "M4_TOOLCHAIN_TYPE")
load(":execution_platform.bzl", "linux_execution_platform_attr", "linux_execution_platform_label")
load(
    ":mapped_kernel.bzl",
    "expand_linux_plan_stage",
    "linux_map_directory_params",
    "linux_map_directory_tools",
    "linux_merge_toolset_execution_requirements",
    "linux_toolset_execution_requirements",
)
load(":module_make_vars.bzl", "validate_linux_module_make_vars")
load(
    ":probe_map_directory.bzl",
    "expand_linux_probe_plan",
    "linux_probe_map_directory_params",
    "linux_probe_map_directory_tools",
)
load(":providers.bzl", "LinuxModuleInfo", "LinuxModuleSdkInfo")
load(":script_runtime_toolchain.bzl", "SCRIPT_RUNTIME_TOOLCHAIN_TYPE")

visibility("//...")

_EXTERNAL_TARGET_TREES = ["metadata", "modules", "objects"]
_HOST_DEPS_TREE = "host_deps"
_HOST_DEPS_SENTINEL = "__LINUX_BZL_HOST_DEPS__"
_PLAN_STAGES = ["prehost", "bootstrap", "host", "prep", "target"]
_SOURCE_TREE_SENTINEL = "__LINUX_BZL_SOURCE_TREE__"
_PERL_TOOLCHAIN_TYPE = str(Label("@rules_perl//perl:toolchain_type"))
_PYTHON_EXEC_TOOLS_TOOLCHAIN_TYPE = str(Label("@rules_python//python:exec_tools_toolchain_type"))

def _add_artifact_path(args, flag, artifact, format = None):
    """Adds one File/TreeArtifact without expanding it or losing path mapping."""
    args.add(flag)
    if format == None:
        args.add_all([artifact], expand_directories = False)
    else:
        args.add_all([artifact], expand_directories = False, format_each = format)

def _copy_linux_tree_file(ctx, runner, tree, path, output):
    args = ctx.actions.args()
    args.add("-copy_tree_file")
    _add_artifact_path(args, "-tree", tree)
    args.add("-path", path)
    _add_artifact_path(args, "-output", output)
    ctx.actions.run(
        executable = runner,
        inputs = [tree],
        outputs = [output],
        arguments = [args],
        mnemonic = "LinuxExternalModuleFile",
        progress_message = "Projecting external Linux module file %s" % path,
    )

def _module_name(ctx):
    name = ctx.attr.module_name.replace("-", "_")
    if name[0] >= "0" and name[0] <= "9":
        name = "_" + name
    for character in name.elems():
        if not (
            (character >= "a" and character <= "z") or
            (character >= "A" and character <= "Z") or
            (character >= "0" and character <= "9") or
            character == "_"
        ):
            fail("%s cannot be normalized to a Linux module name" % ctx.label)
    return name

def _validate_sdk_execution_context(ctx, sdk):
    # External actions execute the SDK's exact probe runners, tool Files, argv,
    # environments, and requirements directly. The module rule's matching
    # toolchain types exist only to anchor map_directory to an execution
    # platform, so exact platform identity is the compatibility contract;
    # selecting a second runner or compiler here would make aliases and select-
    # valued kernel dependencies configuration-dependent again.
    target_execution_platform = linux_execution_platform_label(ctx.attr._target_execution_platform)
    if sdk.target_execution_platform != target_execution_platform:
        fail("%s target actions resolved execution platform %s, but the configured kernel SDK requires %s" % (
            ctx.label,
            target_execution_platform,
            sdk.target_execution_platform,
        ))
    host_execution_platform = linux_execution_platform_label(ctx.attr._host_execution_platform)
    if sdk.host_execution_platform != host_execution_platform:
        fail("%s host actions resolved execution platform %s, but the configured kernel SDK requires %s" % (
            ctx.label,
            host_execution_platform,
            sdk.host_execution_platform,
        ))
    if target_execution_platform != host_execution_platform:
        fail("%s requires target and host tools on one execution platform for source-selected mixed-tool actions; got target %s and host %s" % (
            ctx.label,
            target_execution_platform,
            host_execution_platform,
        ))

def _canonical_source_path(ctx, file):
    path = file.short_path
    package_prefix = ctx.label.package + "/" if ctx.label.package else ""
    if package_prefix and path.startswith(package_prefix):
        path = path[len(package_prefix):]
    elif path.startswith("../"):
        path = "external/" + path[3:]
    components = path.split("/")
    if (
        not path or
        path.startswith("/") or
        "\\" in path or
        any([component in ["", ".", ".."] for component in components])
    ):
        fail("%s source %s has no canonical staged path" % (ctx.label, file))
    return path

def _source_bindings(ctx):
    bindings = []
    owners = {}
    for kind, files in [("source", ctx.files.srcs), ("header", ctx.files.hdrs)]:
        for file in files:
            path = _canonical_source_path(ctx, file)
            if path in owners:
                fail("%s stages both %s and %s as %s" % (ctx.label, owners[path], file, path))
            owners[path] = file
            bindings.append(struct(file = file, kind = kind, path = path))
    return bindings

def _crate_root(ctx):
    if ctx.attr.kind != "rust":
        if ctx.file.crate_root != None:
            fail("%s crate_root is valid only for a Rust module" % ctx.label)
        return None
    if not ctx.files.srcs:
        fail("%s requires at least one Rust source" % ctx.label)
    for source in ctx.files.srcs:
        if not source.basename.endswith(".rs"):
            fail("%s accepts only Rust .rs sources, got %s" % (ctx.label, source.short_path))
    if ctx.file.crate_root != None:
        if ctx.file.crate_root not in ctx.files.srcs:
            fail("%s crate_root must also appear in srcs" % ctx.label)
        return ctx.file.crate_root
    if len(ctx.files.srcs) != 1:
        fail("%s has multiple Rust sources; set crate_root explicitly" % ctx.label)
    return ctx.files.srcs[0]

def _dirname(path):
    return path.rsplit("/", 1)[0] if "/" in path else ""

def _dependency_symvers_name(index):
    decimal = str(index)
    if len(decimal) > 8:
        fail("external module dependency index %s exceeds eight digits" % decimal)
    return "00000000"[:8 - len(decimal)] + decimal + ".symvers"

def _dependency_symvers_kbuild_lines(count):
    return [
        "override KBUILD_EXTRA_SYMBOLS := $(KBUILD_EXTRA_SYMBOLS) $(src)/.linux-bzl-dependencies/" + _dependency_symvers_name(index)
        for index in range(count)
    ]

def linux_test_dependency_symvers_kbuild_lines(count):
    return _dependency_symvers_kbuild_lines(count)

def _record_external_source_copy(ctx, copies, destination, file, owner, allow_same = False):
    existing = copies.get(destination)
    if existing != None:
        if allow_same and existing == file:
            return
        fail("%s stages both %s and %s as %s" % (ctx.label, existing, owner, destination))
    copies[destination] = file

def _validate_external_source_binding(ctx, binding):
    if binding.path == "Kbuild":
        fail("%s source %s uses backend-owned staged path Kbuild" % (ctx.label, binding.file))
    dependency_namespace = ".linux-bzl-dependencies"
    if binding.path == dependency_namespace or binding.path.startswith(dependency_namespace + "/"):
        fail("%s source %s uses backend-owned staged path %s" % (ctx.label, binding.file, binding.path))

def _external_kbuild(ctx, module_name, bindings, crate_root, dependencies):
    lines = ["obj-m += %s.o" % module_name]
    planner_vars = {}
    aliases = []
    if ctx.attr.kind == "rust":
        alias_path = module_name + ".rs"
        aliases.append(struct(file = crate_root, path = alias_path))
    else:
        members = []
        for binding in bindings:
            if binding.kind != "source":
                continue
            member = binding.path[:-len(".c")] + ".o"
            if member == module_name + ".o":
                if len(ctx.files.srcs) != 1:
                    fail("%s composite module source %s collides with its final object; rename the source" % (ctx.label, binding.file))
                members = []
                break
            members.append(member)
        if members:
            lines.append("%s-y := %s" % (module_name, " ".join(members)))

        include_dirs = {"": True}
        for binding in bindings:
            include_dirs[_dirname(binding.path)] = True
        for directory in sorted(include_dirs):
            suffix = "/" + directory if directory else ""
            lines.append("ccflags-y += -I$(src)%s" % suffix)
        flags = []
        for value in ctx.attr.defines + ctx.attr.local_defines:
            flags.append("-D" + value)
        flags.extend(ctx.attr.copts)
        for index, value in enumerate(flags):
            variable = "LINUX_BZL_EXTERNAL_CFLAG_" + ("00000000" + str(index))[-8:]
            lines.append("ccflags-y += $(%s)" % variable)
            planner_vars[variable] = value

    # KBUILD_EXTRA_SYMBOLS is consumed later by the root modpost recipe, after
    # Kbuild has changed `src` back to the kernel source directory. Capture this
    # external module's `src` while its Kbuild is being read. `override` retains
    # declared dependencies when the SDK already supplies extra symvers through
    # a Make command-line variable.
    lines.extend(_dependency_symvers_kbuild_lines(len(dependencies)))
    return "\n".join(lines) + "\n", planner_vars, aliases

def _stage_external_source(ctx, module_name, bindings, crate_root, dependencies):
    prefix = ".linux-bzl/external/" + module_name
    kbuild, planner_vars, aliases = _external_kbuild(ctx, module_name, bindings, crate_root, dependencies)
    kbuild_file = ctx.actions.declare_file(ctx.label.name + ".external.Kbuild")
    ctx.actions.write(kbuild_file, kbuild)
    staged = ctx.actions.declare_directory(ctx.label.name + ".external-source")
    copies = {}
    _record_external_source_copy(ctx, copies, prefix + "/Kbuild", kbuild_file, "generated Kbuild")
    for binding in bindings:
        _validate_external_source_binding(ctx, binding)
        _record_external_source_copy(ctx, copies, prefix + "/" + binding.path, binding.file, binding.file)
    for alias in aliases:
        destination = prefix + "/" + alias.path
        _record_external_source_copy(ctx, copies, destination, alias.file, "Rust crate-root alias %s" % alias.path, allow_same = True)
    for index, dependency in enumerate(dependencies):
        destination = prefix + "/.linux-bzl-dependencies/" + _dependency_symvers_name(index)
        _record_external_source_copy(ctx, copies, destination, dependency.module_symvers, "dependency %s Module.symvers" % index)

    args = ctx.actions.args()
    _add_artifact_path(args, "-tree_out", staged)
    for destination in sorted(copies):
        _add_artifact_path(args, "-copy", copies[destination], format = destination + "=%s")
    ctx.actions.run(
        executable = ctx.executable._actionfile,
        inputs = depset(copies.values()),
        outputs = [staged],
        arguments = [args],
        execution_requirements = {"supports-path-mapping": "1"},
        mnemonic = "LinuxExternalModuleSources",
        progress_message = "Staging declarative Kbuild module %{label}",
    )
    return struct(prefix = prefix, tree = staged, planner_vars = planner_vars)

def _source_prefix(sdk):
    return sdk.source_root.short_path.rsplit("/", 1)[0] if "/" in sdk.source_root.short_path else ""

def _planner_inputs(sdk, staged, extra = []):
    direct = [
        sdk.config,
        sdk.host_deps,
        sdk.host_kconfig_probe_results,
        sdk.host_probe_results,
        sdk.host_toolset_identity,
        sdk.host_toolset_manifest,
        sdk.kbuild,
        sdk.sdk,
        sdk.source_root,
        sdk.target_probe_results,
        sdk.target_kconfig_probe_results,
        sdk.target_toolset_identity,
        sdk.target_toolset_manifest,
        staged.tree,
    ] + extra
    return depset(direct = direct, transitive = [sdk.source, sdk.rust_source_files])

def _add_planner_contract_args(args, sdk, staged):
    args.add("-root", sdk.source_root)
    args.add("-srctree", sdk.source_root)
    args.add("-kbuild", sdk.kbuild)
    _add_artifact_path(args, "-resolve_config", sdk.config)
    args.add("-kernel_version", sdk.version)
    _add_artifact_path(args, "-target_toolset_identity", sdk.target_toolset_identity)
    _add_artifact_path(args, "-host_toolset_identity", sdk.host_toolset_identity)
    _add_artifact_path(args, "-target_toolset_manifest", sdk.target_toolset_manifest)
    _add_artifact_path(args, "-host_toolset_manifest", sdk.host_toolset_manifest)
    _add_artifact_path(args, "-target_probe_results", sdk.target_probe_results)
    _add_artifact_path(args, "-host_probe_results", sdk.host_probe_results)
    _add_artifact_path(args, "-host_kconfig_probe_results", sdk.host_kconfig_probe_results)
    _add_artifact_path(args, "-target_kconfig_probe_results", sdk.target_kconfig_probe_results)
    _add_artifact_path(args, "-object_root", sdk.sdk)
    args.add("-object_namespace", "prep")
    args.add("-selected_products_only")
    args.add("-kbuild_target", "modules")
    virtual_root = _SOURCE_TREE_SENTINEL + "/" + staged.prefix
    _add_artifact_path(
        args,
        "-source_root_map",
        staged.tree,
        format = virtual_root + "=%s/" + staged.prefix,
    )
    args.add("-source_namespace", virtual_root + "=external")
    args.add("-kbuild_var", "M=" + virtual_root)
    _add_artifact_path(args, "-source_root_map", sdk.host_deps, format = _HOST_DEPS_SENTINEL + "=%s")
    args.add("-var", "LIBELF_FLAGS=" + " ".join(sdk.libelf_compile_flags))
    args.add("-var", "LIBELF_LIBS=" + " ".join(sdk.libelf_link_flags))
    for name, value in sdk.make_vars.items():
        args.add("-var", name + "=" + value)
    for name in sorted(staged.planner_vars):
        args.add("-kbuild_var", name + "=" + staged.planner_vars[name])
    if sdk.rust_source_root:
        args.add("-var", "RUST_LIB_SRC=" + sdk.rust_source_root)
        args.add("-source_root_map", sdk.rust_source_root + "=" + sdk.rust_source_root)

def _probe_source_inputs(sdk):
    inputs = {
        "source_files": sdk.source,
        "source_root": sdk.source_root,
    }
    if sdk.rust_source_root:
        inputs["rust_source_files"] = sdk.rust_source_files
    return inputs

def _probe_input_directories(sdk, staged, plan, host_results = None, target = False):
    inputs = {
        _HOST_DEPS_TREE: sdk.host_deps,
        "external": staged.tree,
        "host_toolset_identity": sdk.host_toolset_identity,
        "plan": plan,
        "prep": sdk.sdk,
    }
    if host_results != None:
        inputs["host_results"] = host_results
    if target:
        inputs["target_toolset_identity"] = sdk.target_toolset_identity
    return inputs

def _kbuild_probe_actions(ctx, sdk, staged):
    plan = ctx.actions.declare_directory(ctx.label.name + ".external-kbuild-probe-plan")
    host_results = ctx.actions.declare_directory(ctx.label.name + ".external-kbuild-probes-host")
    target_results = ctx.actions.declare_directory(ctx.label.name + ".external-kbuild-probes-target")
    args = ctx.actions.args()
    _add_planner_contract_args(args, sdk, staged)
    _add_artifact_path(args, "-kbuild_probe_plan_out", plan)
    ctx.actions.run(
        executable = ctx.executable._planner,
        inputs = _planner_inputs(sdk, staged),
        outputs = [plan],
        arguments = [args],
        execution_requirements = {"supports-path-mapping": "1"},
        mnemonic = "LinuxExternalKbuildProbePlan",
        progress_message = "Planning external module Kbuild capability discovery %{label}",
    )

    host_requirements = linux_toolset_execution_requirements(sdk.host_action_requirements, "external host probe")
    target_requirements = linux_toolset_execution_requirements(sdk.target_action_requirements, "external target probe")
    ctx.actions.map_directory(
        implementation = expand_linux_probe_plan,
        input_directories = _probe_input_directories(sdk, staged, plan),
        additional_inputs = _probe_source_inputs(sdk),
        output_directories = {"results": host_results},
        tools = linux_probe_map_directory_tools(
            sdk.host_probe_runner,
            sdk.host_tool_files,
            sdk.host_toolchain_files,
            sdk.host_toolset_manifest,
            sdk.host_toolset_anchors,
            sdk.host_companion_tools,
        ),
        additional_params = linux_probe_map_directory_params(
            "host",
            sdk.host_action_args,
            sdk.host_action_environments,
            source_prefix = _source_prefix(sdk),
            rust_source_root = sdk.rust_source_root,
        ),
        env = {},
        execution_requirements = dict(host_requirements, **{"supports-path-mapping": "1"}),
        exec_group = "host_cc",
        mnemonic = "LinuxExternalMappedHostKbuildProbe",
    )
    ctx.actions.map_directory(
        implementation = expand_linux_probe_plan,
        input_directories = _probe_input_directories(sdk, staged, plan, host_results = host_results, target = True),
        additional_inputs = _probe_source_inputs(sdk),
        output_directories = {"results": target_results},
        tools = linux_probe_map_directory_tools(
            sdk.target_probe_runner,
            sdk.target_tool_files,
            sdk.target_toolchain_files,
            sdk.target_toolset_manifest,
            sdk.target_toolset_anchors,
            sdk.target_companion_tools,
        ),
        additional_params = linux_probe_map_directory_params(
            "target",
            sdk.target_action_args,
            sdk.target_action_environments,
            source_prefix = _source_prefix(sdk),
            rust_source_root = sdk.rust_source_root,
        ),
        env = {},
        execution_requirements = dict(target_requirements, **{"supports-path-mapping": "1"}),
        mnemonic = "LinuxExternalMappedTargetKbuildProbe",
        toolchain = CC_TOOLCHAIN_TYPE,
    )
    return struct(plan = plan, host = host_results, target = target_results)

def _external_action_plan(ctx, sdk, staged, probes):
    plans = {
        stage: ctx.actions.declare_directory(ctx.label.name + ".external-plan-v4-" + stage)
        for stage in _PLAN_STAGES
    }
    args = ctx.actions.args()
    _add_planner_contract_args(args, sdk, staged)
    _add_artifact_path(args, "-host_kbuild_probe_results", probes.host)
    _add_artifact_path(args, "-target_kbuild_probe_results", probes.target)
    for stage in _PLAN_STAGES:
        _add_artifact_path(
            args,
            "-action_plan_stage_out",
            plans[stage],
            format = stage + "=%s",
        )
    ctx.actions.run(
        executable = ctx.executable._planner,
        inputs = _planner_inputs(sdk, staged, extra = [probes.host, probes.target]),
        outputs = [plans[stage] for stage in _PLAN_STAGES],
        arguments = [args],
        execution_requirements = {"supports-path-mapping": "1"},
        mnemonic = "LinuxExternalModulePlan",
        progress_message = "Planning external module through native Kbuild %{label}",
    )
    return plans

def _map_external_plan(ctx, sdk, plans, staged):
    trees = {
        "prehost": ctx.actions.declare_directory(ctx.label.name + ".tree-prehost"),
        "bootstrap": ctx.actions.declare_directory(ctx.label.name + ".tree-bootstrap"),
        "host": ctx.actions.declare_directory(ctx.label.name + ".tree-host"),
        "metadata": ctx.actions.declare_directory(ctx.label.name + ".tree-metadata"),
        "modules": ctx.actions.declare_directory(ctx.label.name + ".tree-modules"),
        "objects": ctx.actions.declare_directory(ctx.label.name + ".tree-objects"),
        "prep": ctx.actions.declare_directory(ctx.label.name + ".tree-prep"),
        "work_prehost": ctx.actions.declare_directory(ctx.label.name + ".tree-work-prehost"),
        "work_bootstrap": ctx.actions.declare_directory(ctx.label.name + ".tree-work-bootstrap"),
        "work_host": ctx.actions.declare_directory(ctx.label.name + ".tree-work-host"),
        "work_prep": ctx.actions.declare_directory(ctx.label.name + ".tree-work-prep"),
        "work_target": ctx.actions.declare_directory(ctx.label.name + ".tree-work-target"),
    }
    common_inputs = {
        _HOST_DEPS_TREE: sdk.host_deps,
        "external": staged.tree,
        "host_toolset_identity": sdk.host_toolset_identity,
        "prep_base": sdk.sdk,
        "target_toolset_identity": sdk.target_toolset_identity,
    }
    additional_inputs = {
        "auto_conf": sdk.auto_conf,
        "auto_conf_cmd": sdk.auto_conf_cmd,
        "autoconf": sdk.autoconf,
        "kernel_release": sdk.kernel_release,
        "resolved_config": sdk.config,
        "rust_source_files": sdk.rust_source_files,
        "rustc_cfg": sdk.rustc_cfg,
        "source_files": sdk.source,
        "source_root": sdk.source_root,
    }
    mapped_requirements = linux_merge_toolset_execution_requirements(
        sdk.target_action_requirements,
        sdk.host_action_requirements,
        "external module",
    )
    mapped_requirements["supports-path-mapping"] = "1"

    def mapped_tools(runner, scope):
        return linux_map_directory_tools(
            runner,
            scope,
            sdk.target_tool_files,
            sdk.target_toolchain_files,
            sdk.target_toolset_manifest,
            sdk.target_toolset_anchors,
            sdk.host_tool_files,
            sdk.host_toolchain_files,
            sdk.host_toolset_manifest,
            sdk.host_toolset_anchors,
            sdk.target_companion_tools,
            sdk.host_companion_tools,
        )

    def mapped_params(stage, input_tree_aliases = {}, output_tree_bases = {}):
        return linux_map_directory_params(
            stage,
            _source_prefix(sdk),
            sdk.target_action_args,
            sdk.target_action_environments,
            sdk.host_action_args,
            sdk.host_action_environments,
            input_tree_aliases = input_tree_aliases,
            output_tree_bases = output_tree_bases,
        )

    ctx.actions.map_directory(
        implementation = expand_linux_plan_stage,
        input_directories = dict(common_inputs, plan = plans["prehost"]),
        additional_inputs = additional_inputs,
        output_directories = {"prehost": trees["prehost"], "work": trees["work_prehost"]},
        tools = mapped_tools(sdk.host_recipe_runner, "host"),
        additional_params = mapped_params(
            "prehost",
            input_tree_aliases = {"prep": "prep_base"},
        ),
        env = {},
        execution_requirements = mapped_requirements,
        exec_group = "host_cc",
        mnemonic = "LinuxExternalMappedPrehost",
    )
    ctx.actions.map_directory(
        implementation = expand_linux_plan_stage,
        input_directories = dict(common_inputs, plan = plans["bootstrap"], prehost = trees["prehost"]),
        additional_inputs = additional_inputs,
        output_directories = {"bootstrap": trees["bootstrap"], "work": trees["work_bootstrap"]},
        tools = mapped_tools(sdk.target_recipe_runner, "target"),
        additional_params = mapped_params(
            "bootstrap",
            input_tree_aliases = {"prep": "prep_base"},
        ),
        env = {},
        execution_requirements = mapped_requirements,
        mnemonic = "LinuxExternalMappedBootstrap",
        toolchain = CC_TOOLCHAIN_TYPE,
    )
    ctx.actions.map_directory(
        implementation = expand_linux_plan_stage,
        input_directories = dict(common_inputs, plan = plans["host"], prehost = trees["prehost"], bootstrap = trees["bootstrap"]),
        additional_inputs = additional_inputs,
        output_directories = {"host": trees["host"], "work": trees["work_host"]},
        tools = mapped_tools(sdk.host_recipe_runner, "host"),
        additional_params = mapped_params(
            "host",
            input_tree_aliases = {"prep": "prep_base"},
        ),
        env = {},
        execution_requirements = mapped_requirements,
        exec_group = "host_cc",
        mnemonic = "LinuxExternalMappedHost",
    )
    ctx.actions.map_directory(
        implementation = expand_linux_plan_stage,
        input_directories = dict(common_inputs, plan = plans["prep"], prehost = trees["prehost"], bootstrap = trees["bootstrap"], host = trees["host"]),
        additional_inputs = additional_inputs,
        output_directories = {"prep": trees["prep"], "work": trees["work_prep"]},
        tools = mapped_tools(sdk.target_recipe_runner, "target"),
        additional_params = mapped_params(
            "prep",
            input_tree_aliases = {"prep": "prep_base"},
            output_tree_bases = {"prep": "prep_base"},
        ),
        env = {},
        execution_requirements = mapped_requirements,
        mnemonic = "LinuxExternalMappedPrep",
        toolchain = CC_TOOLCHAIN_TYPE,
    )
    target_inputs = {
        key: value
        for key, value in common_inputs.items()
        if key != "prep_base"
    }
    target_inputs.update({
        "plan": plans["target"],
        "prehost": trees["prehost"],
        "bootstrap": trees["bootstrap"],
        "host": trees["host"],
        "prep": trees["prep"],
    })
    ctx.actions.map_directory(
        implementation = expand_linux_plan_stage,
        input_directories = target_inputs,
        additional_inputs = additional_inputs,
        output_directories = dict({name: trees[name] for name in _EXTERNAL_TARGET_TREES}, work = trees["work_target"]),
        tools = mapped_tools(sdk.target_recipe_runner, "target"),
        additional_params = mapped_params("target"),
        env = {},
        execution_requirements = mapped_requirements,
        mnemonic = "LinuxExternalMappedTarget",
        toolchain = CC_TOOLCHAIN_TYPE,
    )
    return trees

def _linux_external_module_impl(ctx):
    sdk = ctx.attr.kernel[LinuxModuleSdkInfo]
    validate_linux_module_make_vars(sdk.make_vars, "configured kernel %s" % ctx.attr.kernel.label)
    _validate_sdk_execution_context(ctx, sdk)
    module_name = _module_name(ctx)
    if not ctx.files.srcs:
        fail("%s requires at least one source" % ctx.label)
    if ctx.attr.kind == "c":
        for source in ctx.files.srcs:
            if not source.basename.endswith(".c"):
                fail("%s accepts only C .c sources, got %s" % (ctx.label, source.short_path))
    crate_root = _crate_root(ctx)
    dependencies = []
    for dependency in ctx.attr.deps:
        info = dependency[LinuxModuleInfo]
        if info.kernel_key != sdk.kernel_key:
            fail("%s dependency %s was built against a different configured kernel" % (ctx.label, dependency.label))
        dependencies.append(info)

    bindings = _source_bindings(ctx)
    staged = _stage_external_source(ctx, module_name, bindings, crate_root, dependencies)
    probes = _kbuild_probe_actions(ctx, sdk, staged)
    plans = _external_action_plan(ctx, sdk, staged, probes)
    trees = _map_external_plan(ctx, sdk, plans, staged)
    ko = ctx.actions.declare_file(ctx.attr.output_basename + ".ko")
    module_symvers = ctx.actions.declare_file(ctx.attr.output_basename + ".Module.symvers")
    _copy_linux_tree_file(ctx, sdk.target_recipe_runner, trees["modules"], staged.prefix + "/" + module_name + ".ko", ko)
    _copy_linux_tree_file(ctx, sdk.target_recipe_runner, trees["metadata"], staged.prefix + "/Module.symvers", module_symvers)
    return [
        DefaultInfo(files = depset([ko])),
        LinuxModuleInfo(
            kernel_key = sdk.kernel_key,
            module_symvers = module_symvers,
        ),
        OutputGroupInfo(
            kbuild_probes = depset([probes.plan, probes.host, probes.target]),
            module_symvers = depset([module_symvers]),
            modules = depset([trees["modules"]]),
            objects = depset([trees["objects"]]),
            plan = depset([plans[stage] for stage in _PLAN_STAGES]),
        ),
    ]

# Match linux_mapped_kernel's execution-platform anchors exactly. Rust compiler
# executables already arrive through the SDK's identity-bound action contract;
# resolving another target-aware Rust toolchain here can move the module onto a
# different execution platform than the SDK which supplied those executables.
_linux_external_module = rule(
    implementation = _linux_external_module_impl,
    attrs = {
        "copts": attr.string_list(),
        "crate_root": attr.label(allow_single_file = [".rs"]),
        "defines": attr.string_list(),
        "deps": attr.label_list(providers = [LinuxModuleInfo]),
        "hdrs": attr.label_list(allow_files = True),
        "kernel": attr.label(mandatory = True, providers = [LinuxModuleSdkInfo]),
        "kind": attr.string(mandatory = True, values = ["c", "rust"]),
        "local_defines": attr.string_list(),
        "module_name": attr.string(mandatory = True),
        "output_basename": attr.string(mandatory = True),
        "srcs": attr.label_list(allow_files = True, mandatory = True),
        "_actionfile": attr.label(cfg = "exec", default = Label("//internal/cmd/actionfile"), executable = True),
        "_host_execution_platform": linux_execution_platform_attr(exec_group = "host_cc"),
        "_planner": attr.label(cfg = "exec", default = Label("//internal/cmd/kconfig_parse:kconfig_parse"), executable = True),
        "_target_execution_platform": linux_execution_platform_attr(),
    },
    exec_groups = {"host_cc": exec_group(toolchains = use_cc_toolchain() + [
        BISON_TOOLCHAIN_TYPE,
        FLEX_TOOLCHAIN_TYPE,
        M4_TOOLCHAIN_TYPE,
        _PYTHON_EXEC_TOOLS_TOOLCHAIN_TYPE,
        _PERL_TOOLCHAIN_TYPE,
        SCRIPT_RUNTIME_TOOLCHAIN_TYPE,
    ])},
    toolchains = use_cc_toolchain() + [_PYTHON_EXEC_TOOLS_TOOLCHAIN_TYPE, _PERL_TOOLCHAIN_TYPE, SCRIPT_RUNTIME_TOOLCHAIN_TYPE],
    doc = "Builds one external module through the configured kernel's generic Kbuild planner.",
)

def _linux_external_module_targets(name, kernel, **kwargs):
    _linux_external_module(
        name = name,
        kernel = kernel,
        module_name = name,
        output_basename = name,
        **kwargs
    )

def linux_module(
        name,
        kernel,
        srcs,
        crate_root = None,
        deps = [],
        **kwargs):
    """Builds one Rust-for-Linux module through native Kbuild."""
    _linux_external_module_targets(
        name = name,
        crate_root = crate_root,
        deps = deps,
        kernel = kernel,
        kind = "rust",
        srcs = srcs,
        **kwargs
    )

def linux_cc_module(
        name,
        kernel,
        srcs,
        copts = [],
        deps = [],
        hdrs = [],
        defines = [],
        local_defines = [],
        **kwargs):
    """Builds one C Linux module through native Kbuild."""
    _linux_external_module_targets(
        name = name,
        copts = copts,
        defines = defines,
        deps = deps,
        hdrs = hdrs,
        kernel = kernel,
        kind = "c",
        local_defines = local_defines,
        srcs = srcs,
        **kwargs
    )
