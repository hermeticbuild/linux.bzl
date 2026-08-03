"""Private actions shared by configured vmlinux and module finalization."""

load("@rules_cc//cc:action_names.bzl", "CPP_LINK_EXECUTABLE_ACTION_NAME", "C_COMPILE_ACTION_NAME")
load("@rules_cc//cc:find_cc_toolchain.bzl", "find_cpp_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load(":host_cc_toolchain.bzl", "host_cc_toolchain")
load(
    ":path_mapping.bzl",
    "add_directory_arg",
    "add_mapped_values",
    "directory_anchor",
    "path_mapped_run",
)

visibility("//...")

_MODPOST_SOURCES = [
    "scripts/mod/file2alias.c",
    "scripts/mod/modpost.c",
    "scripts/mod/sumversion.c",
    "scripts/mod/symsearch.c",
]

_PAHOLE_BTF_FEATURES = "encode_force,var,float,enum64,decl_tag,type_tag,optimized_func,consistent_func,decl_tag_kfuncs"

def _kernel_elf_class(arch):
    if arch == "armv7":
        return "ELFCLASS32"
    if arch in ["aarch64", "ppc64le", "riscv64", "x86_64"]:
        return "ELFCLASS64"
    fail("unsupported configured Linux architecture for modpost ELF class: %s" % arch)

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

def _add_include_dirs(args, include_dirs, anchors):
    for include_dir in include_dirs:
        anchor = anchors.get(include_dir)
        if anchor == None:
            args.add("-I" + include_dir)
        else:
            add_directory_arg(args, anchor, format = "-I%s")

def _add_kernel_include_dirs(args, helpers, kernel, source_root):
    config_anchor = kernel.config.include_dir_anchor
    if config_anchor == None:
        args.add("-I" + kernel.config.include_dir)
    else:
        add_directory_arg(args, config_anchor, format = "-I%s")
    _add_include_dirs(
        args,
        helpers.source_include_dirs(
            source_root,
            kernel.srcarch,
            kernel.generated_headers.include_dirs,
        ),
        kernel.generated_headers.include_dir_anchors,
    )

def _target_c_flags(ctx, helpers, kernel, target):
    source_root = _execroot_dir(kernel.source_root)
    response_files = [kernel.config.cflags]
    if kernel.generated_headers.cflags != None:
        response_files.append(kernel.generated_headers.cflags)
    return struct(
        config_include_dir = kernel.config.include_dir,
        config_include_dir_anchor = kernel.config.include_dir_anchor,
        generated_include_dir_anchors = kernel.generated_headers.include_dir_anchors,
        leading_flags = helpers.compile_flags(
            ctx,
            target.cc_toolchain,
            target.feature_configuration,
        ),
        response_files = response_files,
        source_include_dirs = helpers.source_include_dirs(
            source_root,
            kernel.srcarch,
            kernel.generated_headers.include_dirs,
        ),
        source_preinclude_flags = helpers.source_preinclude_flags(source_root),
        source_root = source_root,
    )

def _add_target_c_flags(args, flags):
    args.add_all(flags.leading_flags)
    args.add_all(flags.response_files, format_each = "@%s")
    args.add_all(flags.source_preinclude_flags)
    if flags.config_include_dir_anchor == None:
        args.add("-I" + flags.config_include_dir)
    else:
        add_directory_arg(args, flags.config_include_dir_anchor, format = "-I%s")
    _add_include_dirs(args, flags.source_include_dirs, flags.generated_include_dir_anchors)
    args.add("-fmacro-prefix-map=%s/=" % flags.source_root)

def _target_link_flags(ctx, helpers, target):
    return helpers.target_flags(
        ctx,
        target.cc_toolchain,
        target.feature_configuration,
    )

def _module_metadata_sanitizer_flags(config, source_root, version):
    values = config.config_flags
    flags = []
    if values.get("CONFIG_KASAN") == "y" and values.get("CONFIG_KASAN_HW_TAGS") != "y":
        shadow_offset = values.get("CONFIG_KASAN_SHADOW_OFFSET", "")
        stack = "1" if values.get("CONFIG_KASAN_STACK") == "y" else "0"
        if values.get("CONFIG_KASAN_GENERIC") == "y":
            flags.append("-fsanitize=kernel-address")
            if shadow_offset and shadow_offset != "n":
                flags.extend(["-mllvm", "-asan-mapping-offset=" + shadow_offset])
            flags.extend([
                "-mllvm",
                "-asan-instrumentation-with-call-threshold=" + ("10000" if values.get("CONFIG_KASAN_INLINE") == "y" else "0"),
                "-mllvm",
                "-asan-stack=" + stack,
                "-mllvm",
                "-asan-instrument-allocas=1",
                "-mllvm",
                "-asan-globals=1",
                "-mllvm",
                "-asan-kernel-mem-intrinsic-prefix=1",
            ])
        elif values.get("CONFIG_KASAN_SW_TAGS") == "y":
            flags.append("-fsanitize=kernel-hwaddress")
            if values.get("CONFIG_KASAN_INLINE") == "y":
                if shadow_offset and shadow_offset != "n":
                    flags.extend(["-mllvm", "-hwasan-mapping-offset=" + shadow_offset])
            else:
                flags.extend(["-mllvm", "-hwasan-instrument-with-calls=1"])
            flags.extend([
                "-mllvm",
                "-hwasan-instrument-stack=" + stack,
                "-mllvm",
                "-hwasan-use-short-granules=0",
                "-mllvm",
                "-hwasan-inline-all-checks=0",
            ])
            if values.get("CONFIG_CC_HAS_KASAN_MEMINTRINSIC_PREFIX") == "y":
                flags.extend(["-mllvm", "-hwasan-kernel-mem-intrinsic-prefix=1"])

    if values.get("CONFIG_UBSAN") == "y":
        for symbol, flag in [
            ("CONFIG_UBSAN_ALIGNMENT", "-fsanitize=alignment"),
            ("CONFIG_UBSAN_BOUNDS_STRICT", "-fsanitize=bounds-strict"),
            ("CONFIG_UBSAN_ARRAY_BOUNDS", "-fsanitize=array-bounds"),
            ("CONFIG_UBSAN_LOCAL_BOUNDS", "-fsanitize=local-bounds"),
            ("CONFIG_UBSAN_SHIFT", "-fsanitize=shift"),
            ("CONFIG_UBSAN_DIV_ZERO", "-fsanitize=integer-divide-by-zero"),
            ("CONFIG_UBSAN_UNREACHABLE", "-fsanitize=unreachable"),
            ("CONFIG_UBSAN_BOOL", "-fsanitize=bool"),
            ("CONFIG_UBSAN_ENUM", "-fsanitize=enum"),
        ]:
            if values.get(symbol) == "y":
                flags.append(flag)
        if values.get("CONFIG_UBSAN_TRAP") == "y":
            flags.append("-fsanitize-trap=undefined")
        if values.get("CONFIG_UBSAN_SIGNED_WRAP") == "y":
            flags.append("-fsanitize=signed-integer-overflow")
        if values.get("CONFIG_UBSAN_INTEGER_WRAP") == "y":
            flags.extend([
                "-DINTEGER_WRAP",
                "-fsanitize-undefined-ignore-overflow-pattern=all",
                "-fsanitize=signed-integer-overflow",
                "-fsanitize=unsigned-integer-overflow",
                "-fsanitize=implicit-signed-integer-truncation",
                "-fsanitize=implicit-unsigned-integer-truncation",
                "-fsanitize-ignorelist=" + source_root + "/scripts/integer-wrap-ignore.scl",
            ])
    if values.get("CONFIG_KCSAN") == "y" and _version_at_least(version, 6, 18):
        flags.extend([
            "-fsanitize=thread",
            "-fno-optimize-sibling-calls",
            "-mllvm",
            "-tsan-compound-read-before-write=1" if values.get("CONFIG_CC_HAS_TSAN_COMPOUND_READ_BEFORE_WRITE") == "y" else "-tsan-instrument-read-before-write=1",
            "-mllvm",
            "-tsan-distinguish-volatile=1",
        ])
        if values.get("CONFIG_KCSAN_WEAK_MEMORY") != "y":
            flags.extend([
                "-mllvm",
                "-tsan-instrument-func-entry-exit=0",
            ])
    return flags

def _devicetable_offsets(ctx, helpers, kernel, target, source_files):
    src = _source_file(source_files, "scripts/mod/devicetable-offsets.c")
    asm = ctx.actions.declare_file(ctx.label.name + ".module_prep/scripts/mod/devicetable-offsets.s")
    header = ctx.actions.declare_file(ctx.label.name + ".module_prep/scripts/mod/devicetable-offsets.h")

    compile_args = ctx.actions.args()
    _add_target_c_flags(compile_args, _target_c_flags(ctx, helpers, kernel, target))
    compile_args.add("-S")
    compile_args.add(src)
    compile_args.add("-o")
    compile_args.add(asm)
    path_mapped_run(
        ctx.actions,
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
    path_mapped_run(
        ctx.actions,
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
    ctx.actions.write(elfconfig, "#define KERNEL_ELFCLASS %s\n" % _kernel_elf_class(kernel.arch))

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
    host_toolchain_files = host_cc.all_files.to_list()
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
        compile_args = ctx.actions.args()
        add_mapped_values(
            compile_args,
            cc_common.get_memory_inefficient_command_line(
                feature_configuration = host_features,
                action_name = C_COMPILE_ACTION_NAME,
                variables = compile_variables,
            ),
            files = host_toolchain_files + [
                object_file,
                src,
            ],
            directory_anchors = {
                devicetable_offsets.dirname: directory_anchor(devicetable_offsets),
                source_root: directory_anchor(kernel.source_root, source_root),
            },
        )
        path_mapped_run(
            ctx.actions,
            executable = host_compiler,
            exec_group = "host_cc",
            inputs = depset(
                [src, devicetable_offsets, elfconfig],
                transitive = [kernel.source_tree],
            ),
            tools = host_cc.all_files,
            outputs = [object_file],
            arguments = [compile_args],
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
    add_mapped_values(
        link_args,
        cc_common.get_memory_inefficient_command_line(
            feature_configuration = host_features,
            action_name = CPP_LINK_EXECUTABLE_ACTION_NAME,
            variables = link_variables,
        ),
        files = host_toolchain_files + [out],
    )
    link_args.add_all(objects)
    host_linker = cc_common.get_tool_for_action(
        feature_configuration = host_features,
        action_name = CPP_LINK_EXECUTABLE_ACTION_NAME,
    )
    path_mapped_run(
        ctx.actions,
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
    args.add(kernel.config.cflags, format = "@%s")
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
    _add_kernel_include_dirs(args, helpers, kernel, source_root)
    args.add(src)
    args.add("-o")
    args.add(out)
    path_mapped_run(
        ctx.actions,
        executable = target.compiler,
        inputs = _target_compile_inputs(kernel, target, [src]),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxModuleLinkerScript",
        progress_message = "Preprocessing Linux module linker script %{label}",
    )
    return out

def _compile_module_c(ctx, helpers, kernel, target, src, out, object_name, modname, version):
    args = ctx.actions.args()
    _add_target_c_flags(args, _target_c_flags(ctx, helpers, kernel, target))
    args.add_all(helpers.module_flags("m"))
    args.add_all(helpers.object_name_flags(object_name, modname))
    args.add_all(_module_metadata_sanitizer_flags(
        kernel.config,
        _execroot_dir(kernel.source_root),
        version,
    ))
    args.add("-c")
    args.add(src)
    args.add("-o")
    args.add(out)
    path_mapped_run(
        ctx.actions,
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
        kernel.version,
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

def _symversion_cmd_path(object):
    components = object.split("/")
    if object.startswith("/") or any([component in ["", ".", ".."] for component in components]):
        fail("invalid Linux symbol-version object path %r" % object)
    if not object.endswith(".o"):
        fail("Linux symbol-version object path must end in .o, got %r" % object)
    if "/" not in object:
        return "." + object + ".cmd"
    directory, basename = object.rsplit("/", 1)
    return directory + "/." + basename + ".cmd"

def _object_symversion_records(info):
    if not hasattr(info, "symversion_records") or not info.symversion_records:
        return []
    records = []
    seen = {}
    for record in info.symversion_records:
        path = _symversion_cmd_path(record.object)
        if record.object in seen:
            fail("Linux object %s repeats symbol-version leaf %s" % (info.object, record.object))
        seen[record.object] = True
        records.append(struct(
            cmd = record.cmd,
            object = record.object,
            path = path,
        ))
    return records

def _stage_symversion_inputs(ctx, kernel, stage, module_paths):
    if kernel.config.config_flags.get("CONFIG_MODVERSIONS") != "y":
        return []

    built_in_records = []
    for info in kernel.objects:
        built_in_records.extend(_object_symversion_records(info))
    module_records = {
        info.object: _object_symversion_records(info)
        for info in kernel.module_objects
    }

    staged_by_path = {}
    inputs = []
    for owner, records in [("vmlinux", built_in_records)] + [
        (path, module_records[path])
        for path in module_paths
    ]:
        for record in records:
            previous = staged_by_path.get(record.path)
            if previous != None:
                fail(
                    "Linux symbol-version leaf %s is owned by both %s and %s" %
                    (record.object, previous, owner),
                )
            staged = ctx.actions.declare_file(stage + "/" + record.path)
            ctx.actions.symlink(output = staged, target_file = record.cmd)
            staged_by_path[record.path] = owner
            inputs.append(staged)

    vmlinux_objects = ctx.actions.declare_file(stage + "/.vmlinux.objs")
    ctx.actions.write(
        vmlinux_objects,
        "".join([record.object + "\n" for record in built_in_records]),
    )
    inputs.append(vmlinux_objects)
    for path in module_paths:
        manifest = ctx.actions.declare_file(stage + "/" + path[:-len(".o")] + ".mod")
        ctx.actions.write(
            manifest,
            "".join([record.object + "\n" for record in module_records[path]]),
        )
        inputs.append(manifest)
    return inputs

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

def _pahole_flags(config, version, external_module = False):
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
    distilled_base_minimum = 128 if _version_at_least(version, 6, 18) else 126
    if external_module and pahole_version >= distilled_base_minimum:
        flags.append("--btf_features=distilled_base")
    return flags

def _check_module_modinfo(ctx, module, output):
    args = ctx.actions.args()
    args.add("-in", module)
    args.add("-out", output)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._modulemodinfo,
        inputs = [module],
        outputs = [output],
        arguments = [args],
        mnemonic = "LinuxModuleModinfoCheck",
        progress_message = "Checking Linux module metadata %{label}",
    )

def _objtool_args(config, objtool, input, output, mode, force = False, extra_args = []):
    args = [
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
    if force:
        args.append("-force")
    for arg in extra_args:
        args.append("-objtool_arg=%s" % arg)
    return args

def _process_objtool(ctx, config, objtool, input, output, mode, mnemonic, progress_message, force = False, extra_args = []):
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
        force,
        extra_args,
    ))
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._objtoolrun,
        inputs = [config.config, input],
        tools = [objtool],
        outputs = [output],
        arguments = [args],
        mnemonic = mnemonic,
        progress_message = progress_message,
    )
    return output

def _module_root_needs_objtool(config, info):
    kind = info.module_root_kind
    if kind == "single":
        return False
    if kind != "composite":
        fail("module_root_kind must be single or composite, got %r" % kind)
    return (
        config.config_flags.get("CONFIG_LTO_CLANG") == "y" or
        config.config_flags.get("CONFIG_X86_KERNEL_IBT") == "y"
    )

def _process_module_roots(ctx, kernel, modules):
    outputs = {}
    for info in kernel.module_objects:
        path = info.object
        if not _module_root_needs_objtool(kernel.config, info):
            outputs[path] = modules[path].output
            continue
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
            force = info.objtool_force,
            extra_args = info.objtool_args,
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
    symversion_inputs = _stage_symversion_inputs(
        ctx,
        kernel,
        stage,
        module_paths,
    )
    module_symvers = ctx.actions.declare_file(stage + "/Module.symvers")
    vmlinux_export = ctx.actions.declare_file(stage + "/.vmlinux.export.c")
    outputs = [module_symvers, vmlinux_export] + [
        module_sources[path]
        for path in module_paths
    ]

    args = ctx.actions.args()
    args.add("-cwd")
    add_directory_arg(args, directory_anchor(modules_order))
    args.add("--")
    args.add(modpost)
    args.add_all(_modpost_args(kernel.config))
    args.add("-o")
    args.add("Module.symvers")
    args.add("-T")
    args.add("modules.order")
    args.add("vmlinux.o")
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._runincwd,
        exec_group = "host_cc",
        inputs = [staged_vmlinux, modules_order] + staged_modules + module_checks + symversion_inputs,
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
    kernel_elf_class = _kernel_elf_class,
    module_metadata_sanitizer_flags = _module_metadata_sanitizer_flags,
    modpost_args = _modpost_args,
    module_map = _module_map,
    module_root_needs_objtool = _module_root_needs_objtool,
    objtool_args = _objtool_args,
    pahole_flags = _pahole_flags,
    prepare = _prepare,
    process_objtool = _process_objtool,
    target_c_flags = _target_c_flags,
    add_target_c_flags = _add_target_c_flags,
    target_context = _target_context,
    target_link_flags = _target_link_flags,
    symversion_cmd_path = _symversion_cmd_path,
    version_at_least = _version_at_least,
)
