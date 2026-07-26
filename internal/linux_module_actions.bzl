"""Private actions shared by configured vmlinux and module finalization."""

load("@rules_cc//cc:action_names.bzl", "CPP_LINK_EXECUTABLE_ACTION_NAME", "C_COMPILE_ACTION_NAME")
load("@rules_cc//cc:find_cc_toolchain.bzl", "find_cpp_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load(":host_cc_toolchain.bzl", "host_cc_toolchain")

visibility("//...")

_MODPOST_SOURCES = [
    "scripts/mod/file2alias.c",
    "scripts/mod/modpost.c",
    "scripts/mod/sumversion.c",
    "scripts/mod/symsearch.c",
]

_PAHOLE_BTF_FEATURES = "encode_force,var,float,enum64,decl_tag,type_tag,optimized_func,consistent_func,decl_tag_kfuncs"

def _execroot_path(file):
    path = file.short_path.replace("\\", "/")
    if path.startswith("../"):
        return "external/" + path[3:]
    return path

def _execroot_dir(file):
    path = _execroot_path(file)
    return path.rsplit("/", 1)[0] if "/" in path else ""

def _source_files(kernel):
    root = _execroot_dir(kernel.source_root)
    files = {}
    for file in kernel.source_tree.to_list():
        path = _execroot_path(file)
        if path == root:
            relpath = ""
        elif path.startswith(root + "/"):
            relpath = path[len(root) + 1:]
        else:
            continue
        files[relpath] = file
    return files

def _source_file(source_files, relpath):
    file = source_files.get(relpath)
    if file == None:
        fail("configured Linux module actions are missing source input %s" % relpath)
    return file

def _target_context(ctx, helpers):
    cc_toolchain = find_cpp_toolchain(ctx)
    feature_configuration = helpers.configure_features(ctx, cc_toolchain)
    return struct(
        cc_toolchain = cc_toolchain,
        compiler = cc_common.get_tool_for_action(
            feature_configuration = feature_configuration,
            action_name = C_COMPILE_ACTION_NAME,
        ),
        feature_configuration = feature_configuration,
        linker = cc_common.get_tool_for_action(
            feature_configuration = feature_configuration,
            action_name = CPP_LINK_EXECUTABLE_ACTION_NAME,
        ),
    )

def _target_compile_inputs(kernel, target, direct = []):
    return depset(
        direct,
        transitive = [
            target.cc_toolchain.all_files,
            kernel.config.files,
            kernel.generated_headers.files,
            kernel.source_tree,
        ],
    )

def _target_c_flags(ctx, helpers, kernel, target):
    source_root = _execroot_dir(kernel.source_root)
    flags = []
    flags.extend(helpers.compile_flags(
        ctx,
        target.cc_toolchain,
        target.feature_configuration,
    ))
    flags.append("@" + kernel.config.cflags.path)
    if kernel.generated_headers.cflags != None:
        flags.append("@" + kernel.generated_headers.cflags.path)
    flags.extend(helpers.source_preinclude_flags(source_root))
    flags.append("-I" + kernel.config.include_dir)
    flags.extend(helpers.source_include_flags(
        source_root,
        kernel.srcarch,
        kernel.generated_headers.include_dirs,
    ))
    flags.append("-fmacro-prefix-map=%s/=" % source_root)
    return flags

def _target_link_flags(ctx, helpers, target):
    return helpers.target_flags(
        ctx,
        target.cc_toolchain,
        target.feature_configuration,
    )

def _devicetable_offsets(ctx, helpers, kernel, target, source_files):
    src = _source_file(source_files, "scripts/mod/devicetable-offsets.c")
    asm = ctx.actions.declare_file(ctx.label.name + ".module_prep/scripts/mod/devicetable-offsets.s")
    header = ctx.actions.declare_file(ctx.label.name + ".module_prep/scripts/mod/devicetable-offsets.h")

    compile_args = ctx.actions.args()
    compile_args.add_all(_target_c_flags(ctx, helpers, kernel, target))
    compile_args.add("-S")
    compile_args.add(src)
    compile_args.add("-o")
    compile_args.add(asm)
    ctx.actions.run(
        executable = target.compiler,
        inputs = _target_compile_inputs(kernel, target, [src]),
        outputs = [asm],
        arguments = [compile_args],
        mnemonic = "LinuxModuleOffsetsAsm",
        progress_message = "Compiling Linux module device-table offsets %{label}",
    )

    header_args = ctx.actions.args()
    header_args.add("-in", asm)
    header_args.add("-out", header)
    header_args.add("-guard", "__DEVICETABLE_OFFSETS_H__")
    ctx.actions.run(
        executable = ctx.executable._offsetsheader,
        inputs = [asm],
        outputs = [header],
        arguments = [header_args],
        mnemonic = "LinuxModuleOffsetsHeader",
        progress_message = "Generating Linux module device-table offsets %{label}",
    )
    return header

def _build_modpost(ctx, helpers, kernel, target, source_files):
    devicetable_offsets = _devicetable_offsets(ctx, helpers, kernel, target, source_files)
    elfconfig = ctx.actions.declare_file(ctx.label.name + ".module_prep/scripts/mod/elfconfig.h")
    ctx.actions.write(elfconfig, "#define KERNEL_ELFCLASS ELFCLASS64\n")

    host_cc = host_cc_toolchain(ctx)
    host_features = cc_common.configure_features(
        ctx = ctx,
        cc_toolchain = host_cc,
        requested_features = [],
        unsupported_features = [],
    )
    host_compiler = cc_common.get_tool_for_action(
        feature_configuration = host_features,
        action_name = C_COMPILE_ACTION_NAME,
    )
    sources = [_source_file(source_files, path) for path in _MODPOST_SOURCES]
    source_root = _execroot_dir(kernel.source_root)
    compile_flags = [
        "-std=gnu11",
        "-O2",
        "-D_GNU_SOURCE",
        "-I" + devicetable_offsets.dirname,
        "-I" + source_root + "/scripts/mod",
        "-I" + source_root + "/scripts/include",
    ]
    objects = []
    for src in sources:
        object_file = ctx.actions.declare_file(
            ctx.label.name + ".module_prep/scripts/mod/" + src.basename[:-len(".c")] + ".o",
        )
        compile_variables = cc_common.create_compile_variables(
            cc_toolchain = host_cc,
            feature_configuration = host_features,
            output_file = object_file.path,
            source_file = src.path,
            user_compile_flags = compile_flags,
        )
        ctx.actions.run(
            executable = host_compiler,
            exec_group = "host_cc",
            inputs = depset(
                [src, devicetable_offsets, elfconfig],
                transitive = [kernel.source_tree],
            ),
            tools = host_cc.all_files,
            outputs = [object_file],
            arguments = cc_common.get_memory_inefficient_command_line(
                feature_configuration = host_features,
                action_name = C_COMPILE_ACTION_NAME,
                variables = compile_variables,
            ),
            env = cc_common.get_environment_variables(
                feature_configuration = host_features,
                action_name = C_COMPILE_ACTION_NAME,
                variables = compile_variables,
            ),
            mnemonic = "LinuxModpostHostCompile",
            progress_message = "Compiling hermetic Linux modpost %{label}",
        )
        objects.append(object_file)

    out = ctx.actions.declare_file(ctx.label.name + ".module_prep/scripts/mod/modpost")
    link_variables = cc_common.create_link_variables(
        cc_toolchain = host_cc,
        feature_configuration = host_features,
        is_linking_dynamic_library = False,
        is_using_linker = True,
        output_file = out.path,
    )
    link_args = ctx.actions.args()
    link_args.add_all(cc_common.get_memory_inefficient_command_line(
        feature_configuration = host_features,
        action_name = CPP_LINK_EXECUTABLE_ACTION_NAME,
        variables = link_variables,
    ))
    link_args.add_all(objects)
    host_linker = cc_common.get_tool_for_action(
        feature_configuration = host_features,
        action_name = CPP_LINK_EXECUTABLE_ACTION_NAME,
    )
    ctx.actions.run(
        executable = host_linker,
        exec_group = "host_cc",
        inputs = objects,
        tools = host_cc.all_files,
        outputs = [out],
        arguments = [link_args],
        env = cc_common.get_environment_variables(
            feature_configuration = host_features,
            action_name = CPP_LINK_EXECUTABLE_ACTION_NAME,
            variables = link_variables,
        ),
        mnemonic = "LinuxModpostHostLink",
        progress_message = "Linking hermetic Linux modpost %{label}",
    )
    return out

def _module_linker_script(ctx, helpers, kernel, target, source_files):
    src = _source_file(source_files, "scripts/module.lds.S")
    out = ctx.actions.declare_file(ctx.label.name + ".module_prep/scripts/module.lds")
    source_root = _execroot_dir(kernel.source_root)
    args = ctx.actions.args()
    args.add_all(helpers.compile_flags(
        ctx,
        target.cc_toolchain,
        target.feature_configuration,
    ))
    args.add("@" + kernel.config.cflags.path)
    args.add_all([
        "-E",
        "-P",
        "-D__KERNEL__",
        "-D__ASSEMBLY__",
        "-DLINKER_SCRIPT",
        "-include",
        source_root + "/include/linux/kconfig.h",
    ])
    args.add_all(helpers.cpp_undef_flags(
        kernel.generated_headers.arch,
        kernel.srcarch,
    ))
    args.add("-I" + kernel.config.include_dir)
    args.add_all(helpers.source_include_flags(
        source_root,
        kernel.srcarch,
        kernel.generated_headers.include_dirs,
    ))
    args.add(src)
    args.add("-o")
    args.add(out)
    ctx.actions.run(
        executable = target.compiler,
        inputs = _target_compile_inputs(kernel, target, [src]),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxModuleLinkerScript",
        progress_message = "Preprocessing Linux module linker script %{label}",
    )
    return out

def _compile_module_c(ctx, helpers, kernel, target, src, out, object_name, modname):
    args = ctx.actions.args()
    args.add_all(_target_c_flags(ctx, helpers, kernel, target))
    args.add_all(helpers.module_flags("m"))
    args.add_all(helpers.object_name_flags(object_name, modname))
    args.add("-c")
    args.add(src)
    args.add("-o")
    args.add(out)
    ctx.actions.run(
        executable = target.compiler,
        inputs = _target_compile_inputs(kernel, target, [src]),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxModuleCompile",
        progress_message = "Compiling Linux module metadata %{label}",
    )
    return out

def _module_common(ctx, helpers, kernel, target, source_files):
    src = _source_file(source_files, "scripts/module-common.c")
    out = ctx.actions.declare_file(ctx.label.name + ".module_prep/.module-common.o")
    return _compile_module_c(
        ctx,
        helpers,
        kernel,
        target,
        src,
        out,
        ".module-common.o",
        ".module-common.o",
    )

def _module_map(module_objects):
    modules = {}
    for info in module_objects:
        path = info.object
        if path.startswith("/") or path == ".." or path.startswith("../") or "/../" in path:
            fail("invalid configured Linux module object path %r" % path)
        if not path.endswith(".o"):
            fail("configured Linux module object must end in .o, got %r" % path)
        if path in modules:
            fail("duplicate configured Linux module object %r" % path)
        modules[path] = info
    return modules

def _modpost_args(config):
    args = []
    if config.config_flags.get("CONFIG_MODULES") == "y":
        args.append("-M")
    if config.config_flags.get("CONFIG_MODVERSIONS") == "y":
        args.append("-m")
    if config.config_flags.get("CONFIG_BASIC_MODVERSIONS") == "y":
        args.append("-b")
    if config.config_flags.get("CONFIG_EXTENDED_MODVERSIONS") == "y":
        args.append("-x")
    if config.config_flags.get("CONFIG_MODULE_SRCVERSION_ALL") == "y":
        args.append("-a")
    if config.config_flags.get("CONFIG_SECTION_MISMATCH_WARN_ONLY") != "y":
        args.append("-E")
    if config.config_flags.get("CONFIG_MODULE_ALLOW_MISSING_NAMESPACE_IMPORTS") == "y":
        args.append("-N")
    return args

def _version_at_least(version, major, minor):
    parts = version.split(".")
    if len(parts) < 2:
        fail("invalid Linux version %r" % version)
    return (int(parts[0]), int(parts[1])) >= (major, minor)

def _pahole_flags(config, version):
    pahole_version = int(config.config_flags.get("CONFIG_PAHOLE_VERSION", "0"))
    flags = []
    if pahole_version <= 125:
        if pahole_version >= 118 and pahole_version <= 121:
            flags.append("--skip_encoding_btf_vars")
        if pahole_version >= 121:
            flags.append("--btf_gen_floats")
        if pahole_version >= 125:
            flags.extend([
                "--skip_encoding_btf_inconsistent_proto",
                "--btf_gen_optimized",
            ])
    elif pahole_version >= 126:
        flags.append("--btf_features=" + _PAHOLE_BTF_FEATURES)
        if _version_at_least(version, 6, 18) and pahole_version >= 130:
            flags.append("--btf_features=attributes")
    if config.config_flags.get("CONFIG_PAHOLE_HAS_LANG_EXCLUDE") == "y":
        flags.append("--lang_exclude=rust")
    return flags

def _check_module_modinfo(ctx, module, output):
    args = ctx.actions.args()
    args.add("-in", module)
    args.add("-out", output)
    ctx.actions.run(
        executable = ctx.executable._modulemodinfo,
        inputs = [module],
        outputs = [output],
        arguments = [args],
        mnemonic = "LinuxModuleModinfoCheck",
        progress_message = "Checking Linux module metadata %{label}",
    )

def _objtool_args(config, objtool, input, output, mode):
    return [
        "-config",
        config,
        "-objtool",
        objtool,
        "-in",
        input,
        "-mode",
        mode,
        "-out",
        output,
    ]

def _process_objtool(ctx, config, objtool, input, output, mode, mnemonic, progress_message):
    if objtool == None:
        ctx.actions.symlink(output = output, target_file = input)
        return output

    args = ctx.actions.args()
    args.add_all(_objtool_args(
        config.config,
        objtool,
        input,
        output,
        mode,
    ))
    ctx.actions.run(
        executable = ctx.executable._objtoolrun,
        inputs = [config.config, input],
        tools = [objtool],
        outputs = [output],
        arguments = [args],
        mnemonic = mnemonic,
        progress_message = progress_message,
    )
    return output

def _process_module_roots(ctx, kernel, modules):
    outputs = {}
    for path in [info.object for info in kernel.module_objects]:
        output = ctx.actions.declare_file(
            ctx.label.name + ".module_prep/objtool/" + path,
        )
        outputs[path] = _process_objtool(
            ctx,
            kernel.config,
            ctx.executable.objtool,
            modules[path].output,
            output,
            "module",
            "LinuxModuleObjtool",
            "Processing in-tree Linux module with objtool %{label}",
        )
    return outputs

def _run_modpost(ctx, kernel, modpost, module_outputs):
    stage = ctx.label.name + ".modpost"
    staged_vmlinux = ctx.actions.declare_file(stage + "/vmlinux.o")
    ctx.actions.symlink(output = staged_vmlinux, target_file = kernel.vmlinux_object)

    staged_modules = []
    module_checks = []
    module_sources = {}
    module_paths = [info.object for info in kernel.module_objects]
    for path in module_paths:
        staged = ctx.actions.declare_file(stage + "/" + path)
        ctx.actions.symlink(output = staged, target_file = module_outputs[path])
        staged_modules.append(staged)
        checked = ctx.actions.declare_file(stage + "/" + path + ".modinfo.checked")
        _check_module_modinfo(ctx, module_outputs[path], checked)
        module_checks.append(checked)
        module_sources[path] = ctx.actions.declare_file(stage + "/" + path[:-len(".o")] + ".mod.c")

    modules_order = ctx.actions.declare_file(stage + "/modules.order")
    ctx.actions.write(
        modules_order,
        "".join([path + "\n" for path in module_paths]),
    )
    module_symvers = ctx.actions.declare_file(stage + "/Module.symvers")
    vmlinux_export = ctx.actions.declare_file(stage + "/.vmlinux.export.c")
    outputs = [module_symvers, vmlinux_export] + [
        module_sources[path]
        for path in module_paths
    ]

    args = ctx.actions.args()
    args.add("-cwd", modules_order.dirname)
    args.add("--")
    args.add(modpost)
    args.add_all(_modpost_args(kernel.config))
    args.add("-o")
    args.add("Module.symvers")
    args.add("-T")
    args.add("modules.order")
    args.add("vmlinux.o")
    ctx.actions.run(
        executable = ctx.executable._runincwd,
        exec_group = "host_cc",
        inputs = [staged_vmlinux, modules_order] + staged_modules + module_checks,
        tools = [modpost],
        outputs = outputs,
        arguments = [args],
        mnemonic = "LinuxModpost",
        progress_message = "Running Linux modpost %{label}",
    )
    return struct(
        export_source = vmlinux_export,
        module_sources = module_sources,
        module_symvers = module_symvers,
        modules_order = modules_order,
    )

def _prepare(ctx, helpers, kernel):
    modules = _module_map(kernel.module_objects)
    if modules and kernel.config.config_flags.get("CONFIG_MODULES") != "y":
        fail("%s has configured module objects but CONFIG_MODULES is disabled" % ctx.label)
    modules_enabled = kernel.config.config_flags.get("CONFIG_MODULES") == "y"
    if not modules_enabled and not _version_at_least(kernel.version, 6, 18):
        return None

    target = _target_context(ctx, helpers)
    source_files = _source_files(kernel)
    modpost = _build_modpost(ctx, helpers, kernel, target, source_files)
    module_lds = _module_linker_script(ctx, helpers, kernel, target, source_files) if modules_enabled else None
    module_common = _module_common(ctx, helpers, kernel, target, source_files) if modules_enabled else None
    module_outputs = _process_module_roots(ctx, kernel, modules)
    outputs = _run_modpost(ctx, kernel, modpost, module_outputs)
    return struct(
        export_source = outputs.export_source,
        module_common = module_common,
        module_lds = module_lds,
        module_outputs = module_outputs,
        module_sources = outputs.module_sources,
        module_symvers = outputs.module_symvers,
        modules_order = outputs.modules_order,
        modpost = modpost,
    )

linux_module_actions = struct(
    compile_module_c = _compile_module_c,
    modpost_args = _modpost_args,
    module_map = _module_map,
    objtool_args = _objtool_args,
    pahole_flags = _pahole_flags,
    prepare = _prepare,
    process_objtool = _process_objtool,
    target_c_flags = _target_c_flags,
    target_context = _target_context,
    target_link_flags = _target_link_flags,
)
