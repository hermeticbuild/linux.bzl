"""Private configured-kernel module finalization rule."""

load("@rules_cc//cc:find_cc_toolchain.bzl", "use_cc_toolchain")
load(":linux_module_actions.bzl", "linux_module_actions")
load(":linux_objects.bzl", "linux_module_cc_helpers")
load(
    ":path_mapping.bzl",
    "add_directory_arg",
    "directory_anchor",
    "path_mapped_run",
)
load(":providers.bzl", "LinuxModuleInfo", "LinuxModuleSdkInfo", "LinuxVmlinuxInfo")

visibility("//...")

def _link_module(ctx, target, preliminary, mod_object, module_common, module_lds, path, target_link_flags = None):
    out = ctx.actions.declare_file(ctx.label.name + ".modules/" + path[:-len(".o")] + ".ko.unprocessed")
    args = ctx.actions.args()
    if target_link_flags == None:
        target_link_flags = linux_module_cc_helpers.target_flags(
            ctx,
            target.cc_toolchain,
            target.feature_configuration,
        )
    args.add_all(target_link_flags)
    args.add_all([
        "-fuse-ld=lld",
        "-nostdlib",
        "-r",
        "-Wl,--build-id=sha1",
        "-Wl,-z,noexecstack",
    ])
    args.add(module_lds, format = "-Wl,-T,%s")
    args.add("-o")
    args.add(out)
    args.add(preliminary)
    args.add(mod_object)
    args.add(module_common)
    path_mapped_run(
        ctx.actions,
        executable = target.linker,
        inputs = depset(
            [preliminary, mod_object, module_common, module_lds],
            transitive = [target.cc_toolchain.all_files],
        ),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxModuleLink",
        progress_message = "Linking Linux module %{label}",
    )
    return out

def _sdk_target_inputs(sdk, direct = []):
    return depset(
        direct,
        transitive = [
            sdk.target.cc_toolchain.all_files,
            sdk.config.files,
            sdk.generated_headers.files,
            sdk.source_tree,
        ],
    )

def _crate_root(ctx):
    sources = ctx.files.srcs
    if not sources:
        fail("%s requires at least one Rust source" % ctx.label)
    for source in sources:
        if not source.basename.endswith(".rs"):
            fail("%s accepts only Rust .rs sources, got %s" % (ctx.label, source.short_path))
    if ctx.file.crate_root:
        if ctx.file.crate_root not in sources:
            fail("%s crate_root must also appear in srcs" % ctx.label)
        return ctx.file.crate_root
    if len(sources) != 1:
        fail("%s has multiple Rust sources; set crate_root explicitly" % ctx.label)
    return sources[0]

def _crate_name(ctx):
    name = ctx.label.name.replace("-", "_")
    if name[0] >= "0" and name[0] <= "9":
        name = "_" + name
    for character in name.elems():
        if not (
            (character >= "a" and character <= "z") or
            (character >= "A" and character <= "Z") or
            (character >= "0" and character <= "9") or
            character == "_"
        ):
            fail("%s cannot be normalized to a Rust crate name" % ctx.label)
    return name

def _compile_external_rust(ctx, sdk, crate_root, crate_name):
    rust = sdk.rust
    raw = ctx.actions.declare_file(ctx.label.name + ".external/" + crate_name + ".o.raw")
    args = ctx.actions.args()
    args.add("-cwd", ".")
    env = dict(rust.rustc_env)
    modfile = ctx.label.package + "/" + crate_name if ctx.label.package else crate_name
    env["RUST_MODFILE"] = modfile
    for name in sorted(env.keys()):
        args.add("-env", name + "=" + env[name])
    args.add("-env")
    add_directory_arg(
        args,
        rust.objtree_anchor,
        format = "OBJTREE={cwd}/%s",
    )
    args.add("--")
    args.add(ctx.executable._rustcrun)
    args.add("-probe")
    args.add(rust.rustc_probe)
    for predicate in rust.module_version_predicates:
        args.add("-predicate", json.encode(predicate))
    args.add("--")
    args.add(rust.rustc)
    _add_rust_sdk_flags(args, sdk, rust.module_flags)
    args.add("--crate-name")
    args.add(crate_name)
    args.add("--out-dir")
    add_directory_arg(args, directory_anchor(raw))
    args.add(raw, format = "--emit=obj=%s")
    args.add(crate_root)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._runincwd,
        inputs = depset(
            ctx.files.srcs + [rust.rustc_probe],
            transitive = [rust.compile_inputs, rust.rustc_files],
        ),
        tools = [ctx.attr._rustcrun[DefaultInfo].files_to_run],
        outputs = [raw],
        arguments = [args],
        mnemonic = "LinuxRustModuleCompile",
        progress_message = "Compiling Rust-for-Linux module %{label}",
    )

    out = ctx.actions.declare_file(ctx.label.name + ".external/" + crate_name + ".o")
    return linux_module_actions.process_objtool(
        ctx,
        sdk.config,
        rust.objtool,
        raw,
        out,
        "module",
        "LinuxRustModuleObjtool",
        "Processing Rust-for-Linux module with objtool %{label}",
    )

def _compile_external_c(ctx, sdk, source, module_name):
    raw = ctx.actions.declare_file(ctx.label.name + ".external/" + module_name + ".o.raw")
    args = ctx.actions.args()
    linux_module_actions.add_target_c_flags(args, sdk.target_c_flags)
    args.add_all(linux_module_cc_helpers.module_flags("m"))
    args.add_all(linux_module_cc_helpers.object_name_flags(
        module_name + ".o",
        module_name + ".o",
    ))
    args.add_all(linux_module_actions.module_metadata_sanitizer_flags(
        sdk.config,
        sdk.target_c_flags.source_root,
        sdk.version,
    ))
    add_directory_arg(args, directory_anchor(source), format = "-I%s")
    args.add_all(ctx.attr.copts)
    args.add("-c")
    args.add(source)
    args.add("-o")
    args.add(raw)
    path_mapped_run(
        ctx.actions,
        executable = sdk.target.compiler,
        inputs = _sdk_target_inputs(sdk, ctx.files.srcs),
        outputs = [raw],
        arguments = [args],
        mnemonic = "LinuxCModuleCompile",
        progress_message = "Compiling out-of-tree C Linux module %{label}",
    )

    out = ctx.actions.declare_file(ctx.label.name + ".external/" + module_name + ".o")
    return linux_module_actions.process_objtool(
        ctx,
        sdk.config,
        sdk.objtool,
        raw,
        out,
        "module",
        "LinuxCModuleObjtool",
        "Processing out-of-tree C Linux module with objtool %{label}",
    )

def _add_rust_sdk_flags(args, sdk, flags):
    rust = sdk.rust
    for flag in flags:
        if rust.target_spec != None and rust.target_spec.path in flag:
            args.add(
                rust.target_spec,
                format = flag.replace("%", "%%").replace(rust.target_spec.path, "%s"),
            )
        elif sdk.config.rustc_cfg.path in flag:
            args.add(
                sdk.config.rustc_cfg,
                format = flag.replace("%", "%%").replace(sdk.config.rustc_cfg.path, "%s"),
            )
        elif rust.rust_dir in flag:
            add_directory_arg(
                args,
                rust.rust_dir_anchor,
                format = flag.replace("%", "%%").replace(rust.rust_dir, "%s"),
            )
        else:
            args.add(flag)

def _check_external_modinfo(ctx, preliminary, crate_name):
    checked = ctx.actions.declare_file(
        ctx.label.name + ".external/" + crate_name + ".modinfo.checked",
    )
    check_args = ctx.actions.args()
    check_args.add("-in", preliminary)
    check_args.add("-out", checked)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._modulemodinfo,
        inputs = [preliminary],
        outputs = [checked],
        arguments = [check_args],
        mnemonic = "LinuxExternalModuleModinfoCheck",
        progress_message = "Checking external Linux module metadata %{label}",
    )
    return checked

def _external_modpost(ctx, sdk, preliminary, crate_name, modinfo_check):
    stage = ctx.label.name + ".external/modpost"
    staged_object = ctx.actions.declare_file(stage + "/" + crate_name + ".o")
    ctx.actions.symlink(output = staged_object, target_file = preliminary)
    manifest = ctx.actions.declare_file(stage + "/" + crate_name + ".mod")
    ctx.actions.write(manifest, crate_name + ".o\n")
    modules_order = ctx.actions.declare_file(stage + "/modules.order")
    ctx.actions.write(modules_order, crate_name + ".o\n")
    kernel_symvers = ctx.actions.declare_file(stage + "/Kernel.symvers")
    ctx.actions.symlink(output = kernel_symvers, target_file = sdk.module_symvers)

    dep_symvers = []
    for index, dep in enumerate(ctx.attr.deps):
        info = dep[LinuxModuleInfo]
        if info.kernel_key != sdk.kernel_key:
            fail(
                "%s dependency %s was built against a different configured kernel" %
                (ctx.label, dep.label),
            )
        staged = ctx.actions.declare_file(stage + "/dependency_%d.symvers" % index)
        ctx.actions.symlink(output = staged, target_file = info.module_symvers)
        dep_symvers.append(staged)

    mod_source = ctx.actions.declare_file(stage + "/" + crate_name + ".mod.c")
    module_symvers = ctx.actions.declare_file(stage + "/Module.symvers")
    args = ctx.actions.args()
    args.add("-cwd")
    add_directory_arg(args, directory_anchor(modules_order))
    args.add("--")
    args.add(sdk.modpost)
    args.add_all(linux_module_actions.modpost_args(sdk.config))
    args.add("-e")
    args.add("-i")
    args.add("Kernel.symvers")
    for dep in dep_symvers:
        args.add("-i")
        args.add(dep.basename)
    args.add("-o")
    args.add("Module.symvers")
    args.add("-T")
    args.add("modules.order")
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._runincwd,
        inputs = [
            staged_object,
            manifest,
            modules_order,
            kernel_symvers,
            modinfo_check,
        ] + dep_symvers,
        tools = [sdk.modpost],
        outputs = [mod_source, module_symvers],
        arguments = [args],
        mnemonic = "LinuxExternalModpost",
        progress_message = "Running Linux modpost for %{label}",
    )
    return mod_source, module_symvers

def _compile_external_mod_source(ctx, sdk, source, crate_name):
    out = ctx.actions.declare_file(ctx.label.name + ".external/" + crate_name + ".mod.o")
    args = ctx.actions.args()
    linux_module_actions.add_target_c_flags(args, sdk.target_c_flags)
    args.add_all(linux_module_cc_helpers.module_flags("m"))
    args.add_all(linux_module_cc_helpers.object_name_flags(
        crate_name + ".mod.o",
        crate_name + ".o",
    ))
    args.add_all(linux_module_actions.module_metadata_sanitizer_flags(
        sdk.config,
        sdk.target_c_flags.source_root,
        sdk.version,
    ))
    args.add("-c")
    args.add(source)
    args.add("-o")
    args.add(out)
    path_mapped_run(
        ctx.actions,
        executable = sdk.target.compiler,
        inputs = _sdk_target_inputs(sdk, [source]),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxExternalModuleMetadataCompile",
        progress_message = "Compiling external Linux module metadata %{label}",
    )
    return out

def _linux_module_impl(ctx):
    sdk = ctx.attr.kernel[LinuxModuleSdkInfo]
    if sdk.config.config_flags.get("CONFIG_MODULES") != "y":
        fail("%s requires a kernel with CONFIG_MODULES=y" % ctx.label)
    if sdk.rust == None or not sdk.rust.enabled:
        fail("%s requires a kernel with CONFIG_RUST=y" % ctx.label)
    crate_root = _crate_root(ctx)
    crate_name = _crate_name(ctx)
    preliminary = _compile_external_rust(ctx, sdk, crate_root, crate_name)
    modinfo_check = _check_external_modinfo(ctx, preliminary, crate_name)
    mod_source, module_symvers = _external_modpost(
        ctx,
        sdk,
        preliminary,
        crate_name,
        modinfo_check,
    )
    mod_object = _compile_external_mod_source(ctx, sdk, mod_source, crate_name)
    linked = _link_module(
        ctx,
        sdk.target,
        preliminary,
        mod_object,
        sdk.module_common,
        sdk.module_lds,
        crate_name + ".o",
        target_link_flags = sdk.target_link_flags,
    )
    out = ctx.actions.declare_file(ctx.label.name + ".ko")
    _btf_module(
        ctx,
        sdk.config,
        sdk.vmlinux,
        linked,
        out,
        sdk.version,
        sdk.btf_tools,
        external_module = True,
    )
    return [
        DefaultInfo(files = depset([out])),
        LinuxModuleInfo(
            kernel_key = sdk.kernel_key,
            ko = out,
            module_symvers = module_symvers,
        ),
        OutputGroupInfo(module_symvers = depset([module_symvers])),
    ]

def _linux_cc_module_impl(ctx):
    sdk = ctx.attr.kernel[LinuxModuleSdkInfo]
    if sdk.config.config_flags.get("CONFIG_MODULES") != "y":
        fail("%s requires a kernel with CONFIG_MODULES=y" % ctx.label)
    if len(ctx.files.srcs) != 1:
        fail("%s requires exactly one C source" % ctx.label)
    source = ctx.files.srcs[0]
    module_name = _crate_name(ctx)
    preliminary = _compile_external_c(ctx, sdk, source, module_name)
    modinfo_check = _check_external_modinfo(ctx, preliminary, module_name)
    mod_source, module_symvers = _external_modpost(
        ctx,
        sdk,
        preliminary,
        module_name,
        modinfo_check,
    )
    mod_object = _compile_external_mod_source(ctx, sdk, mod_source, module_name)
    linked = _link_module(
        ctx,
        sdk.target,
        preliminary,
        mod_object,
        sdk.module_common,
        sdk.module_lds,
        module_name + ".o",
        target_link_flags = sdk.target_link_flags,
    )
    out = ctx.actions.declare_file(ctx.label.name + ".ko")
    _btf_module(
        ctx,
        sdk.config,
        sdk.vmlinux,
        linked,
        out,
        sdk.version,
        sdk.btf_tools,
        external_module = True,
    )
    return [
        DefaultInfo(files = depset([out])),
        LinuxModuleInfo(
            kernel_key = sdk.kernel_key,
            ko = out,
            module_symvers = module_symvers,
        ),
        OutputGroupInfo(module_symvers = depset([module_symvers])),
    ]

def _btf_module(ctx, config, vmlinux, linked, out, version, tools, external_module = False):
    if config.config_flags.get("CONFIG_DEBUG_INFO_BTF_MODULES") != "y":
        ctx.actions.symlink(output = out, target_file = linked)
        return out
    if not tools.pahole:
        fail("%s enables CONFIG_DEBUG_INFO_BTF_MODULES and requires pahole" % ctx.label)
    if not tools.resolve_btfids:
        fail("%s enables CONFIG_DEBUG_INFO_BTF_MODULES and requires resolve_btfids_tool" % ctx.label)

    encoded = ctx.actions.declare_file(out.basename + ".btf", sibling = out)
    pahole_args = ctx.actions.args()
    pahole_args.add("-input", linked)
    pahole_args.add("-output", encoded)
    pahole_args.add("-env")
    pahole_args.add(tools.llvm_objcopy, format = "LLVM_OBJCOPY=%s")
    pahole_args.add("--")
    pahole_args.add(tools.pahole.executable)
    pahole_args.add("-J")
    pahole_args.add_all(linux_module_actions.pahole_flags(
        config,
        version,
        external_module = external_module,
    ))
    pahole_args.add("--btf_base")
    pahole_args.add(vmlinux)
    pahole_args.add("{output}")
    path_mapped_run(
        ctx.actions,
        executable = tools.btfmutate,
        inputs = [linked, vmlinux],
        tools = [tools.pahole, tools.llvm_objcopy],
        outputs = [encoded],
        arguments = [pahole_args],
        mnemonic = "LinuxModuleBTF",
        progress_message = "Encoding Linux module BTF %{label}",
    )

    resolve_args = ctx.actions.args()
    resolve_args.add("-input", encoded)
    resolve_args.add("-output", out)
    resolve_args.add("--")
    resolve_args.add(tools.resolve_btfids.executable)
    resolve_args.add("-b")
    resolve_args.add(vmlinux)
    resolve_args.add("{output}")
    path_mapped_run(
        ctx.actions,
        executable = tools.btfmutate,
        inputs = [encoded, vmlinux],
        tools = [tools.resolve_btfids],
        outputs = [out],
        arguments = [resolve_args],
        mnemonic = "LinuxModuleResolveBTFIDs",
        progress_message = "Resolving Linux module BTF IDs %{label}",
    )
    return out

def _empty_file(ctx, path):
    out = ctx.actions.declare_file(ctx.label.name + ".sdk/" + path)
    ctx.actions.write(out, "")
    return out

def _builtin_module_metadata(ctx, cc_toolchain, vmlinux):
    raw = ctx.actions.declare_file(ctx.label.name + ".sdk/modules.builtin.modinfo.raw")
    llvm_objcopy = linux_module_cc_helpers.llvm_objcopy(cc_toolchain)
    objcopy_args = ctx.actions.args()
    objcopy_args.add_all([
        "-j",
        ".modinfo",
        "-O",
        "binary",
        vmlinux.vmlinux_unstripped,
        raw,
    ])
    path_mapped_run(
        ctx.actions,
        executable = llvm_objcopy,
        inputs = [vmlinux.vmlinux_unstripped],
        outputs = [raw],
        arguments = [objcopy_args],
        mnemonic = "LinuxBuiltinModinfoExtract",
        progress_message = "Extracting built-in Linux module metadata %{label}",
    )

    modules_builtin = ctx.actions.declare_file(ctx.label.name + ".sdk/modules.builtin")
    modules_builtin_modinfo = ctx.actions.declare_file(ctx.label.name + ".sdk/modules.builtin.modinfo")
    metadata_args = ctx.actions.args()
    metadata_args.add("-input", raw)
    metadata_args.add("-modinfo_out", modules_builtin_modinfo)
    metadata_args.add("-modules_out", modules_builtin)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._builtinmodinfo,
        inputs = [raw],
        outputs = [modules_builtin, modules_builtin_modinfo],
        arguments = [metadata_args],
        mnemonic = "LinuxBuiltinModinfo",
        progress_message = "Generating built-in Linux module metadata %{label}",
    )
    return modules_builtin, modules_builtin_modinfo

def _linux_module_sdk_impl(ctx):
    vmlinux = ctx.attr.vmlinux[LinuxVmlinuxInfo]
    modules = linux_module_actions.module_map(vmlinux.module_objects)
    target = linux_module_actions.target_context(ctx, linux_module_cc_helpers)
    target_c_flags = linux_module_actions.target_c_flags(
        ctx,
        linux_module_cc_helpers,
        vmlinux,
        target,
    )
    target_link_flags = linux_module_actions.target_link_flags(
        ctx,
        linux_module_cc_helpers,
        target,
    )

    modules_builtin, modules_builtin_modinfo = _builtin_module_metadata(ctx, target.cc_toolchain, vmlinux)
    ko_files = []
    btf_tools = struct(
        btfmutate = ctx.attr._btfmutate[DefaultInfo].files_to_run,
        llvm_objcopy = linux_module_cc_helpers.llvm_objcopy(target.cc_toolchain),
        pahole = ctx.attr.pahole[DefaultInfo].files_to_run if ctx.attr.pahole else None,
        resolve_btfids = ctx.attr.resolve_btfids_tool[DefaultInfo].files_to_run if ctx.attr.resolve_btfids_tool else None,
    )

    modules_enabled = vmlinux.config.config_flags.get("CONFIG_MODULES") == "y"
    if modules and not modules_enabled:
        fail("%s has configured module objects but CONFIG_MODULES is disabled" % ctx.label)
    if modules_enabled:
        for field in [
            "module_common",
            "module_lds",
            "module_symvers",
            "modules_order",
            "modpost",
        ]:
            if getattr(vmlinux, field) == None:
                fail("%s is missing prepared module field %s" % (ctx.label, field))

        module_common = vmlinux.module_common
        module_lds = vmlinux.module_lds
        module_symvers = vmlinux.module_symvers
        modules_order = vmlinux.modules_order
        modpost = vmlinux.modpost

        for info in vmlinux.module_objects:
            path = info.object
            mod_source = vmlinux.module_sources.get(path)
            if mod_source == None:
                fail("%s is missing modpost source for module %s" % (ctx.label, path))
            mod_object = ctx.actions.declare_file(
                ctx.label.name + ".modules/" + path[:-len(".o")] + ".mod.o",
            )
            linux_module_actions.compile_module_c(
                ctx,
                linux_module_cc_helpers,
                vmlinux,
                target,
                mod_source,
                mod_object,
                path[:-len(".o")] + ".mod.o",
                path,
                ctx.attr.version,
            )
            linked = _link_module(
                ctx,
                target,
                vmlinux.module_outputs[path],
                mod_object,
                module_common,
                module_lds,
                path,
            )
            out = ctx.actions.declare_file(ctx.label.name + ".modules/" + path[:-len(".o")] + ".ko")
            ko_files.append(_btf_module(
                ctx,
                vmlinux.config,
                vmlinux.vmlinux,
                linked,
                out,
                ctx.attr.version,
                btf_tools,
            ))
    else:
        modules_order = _empty_file(ctx, "modules.order")
        module_symvers = _empty_file(ctx, "Module.symvers")
        module_common = _empty_file(ctx, ".module-common.o")
        module_lds = _empty_file(ctx, "scripts/module.lds")
        modpost = _empty_file(ctx, "scripts/mod/modpost")

    all_outputs = depset(
        ko_files + [
            module_symvers,
            modules_order,
            modules_builtin,
            modules_builtin_modinfo,
        ],
    )
    return [
        DefaultInfo(files = all_outputs),
        LinuxModuleSdkInfo(
            arch = vmlinux.arch,
            btf_tools = btf_tools,
            config = vmlinux.config,
            generated_headers = vmlinux.generated_headers,
            kernel_key = str(ctx.label),
            kernel_release = vmlinux.config.kernel_release,
            module_common = module_common,
            module_lds = module_lds,
            module_symvers = module_symvers,
            modules = depset(ko_files),
            modules_builtin = modules_builtin,
            modules_builtin_modinfo = modules_builtin_modinfo,
            modules_order = modules_order,
            modpost = modpost,
            objtool = ctx.executable.objtool,
            source_root = vmlinux.source_root,
            source_tree = vmlinux.source_tree,
            srcarch = vmlinux.srcarch,
            rust = vmlinux.rust,
            target = target,
            target_c_flags = target_c_flags,
            target_link_flags = target_link_flags,
            version = ctx.attr.version,
            vmlinux = vmlinux.vmlinux,
            vmlinux_object = vmlinux.vmlinux_object,
        ),
        OutputGroupInfo(
            module_symvers = depset([module_symvers]),
            modules = depset(ko_files),
            modules_builtin = depset([modules_builtin]),
            modules_builtin_modinfo = depset([modules_builtin_modinfo]),
            modules_order = depset([modules_order]),
        ),
    ]

linux_module_sdk = rule(
    implementation = _linux_module_sdk_impl,
    attrs = {
        "arch": attr.string(
            mandatory = True,
            values = ["arm64", "x86"],
        ),
        "objtool": attr.label(
            cfg = "exec",
            executable = True,
        ),
        "pahole": attr.label(
            cfg = "exec",
            executable = True,
        ),
        "resolve_btfids_tool": attr.label(
            cfg = "exec",
            executable = True,
        ),
        "version": attr.string(mandatory = True),
        "vmlinux": attr.label(
            mandatory = True,
            providers = [LinuxVmlinuxInfo],
        ),
        "_btfmutate": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/btfmutate"),
            executable = True,
        ),
        "_builtinmodinfo": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/builtinmodinfo"),
            executable = True,
        ),
    },
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Finalizes configured in-tree modules and exposes the private module SDK.",
)

_linux_module = rule(
    implementation = _linux_module_impl,
    attrs = {
        "crate_root": attr.label(
            allow_single_file = [".rs"],
        ),
        "deps": attr.label_list(
            providers = [LinuxModuleInfo],
        ),
        "kernel": attr.label(
            mandatory = True,
            providers = [LinuxModuleSdkInfo],
        ),
        "srcs": attr.label_list(
            allow_files = [".rs"],
            mandatory = True,
        ),
        "_objtoolrun": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/objtoolrun"),
            executable = True,
        ),
        "_modulemodinfo": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/modulemodinfo"),
            executable = True,
        ),
        "_runincwd": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/runincwd"),
            executable = True,
        ),
        "_rustcrun": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/rustcrun"),
            executable = True,
        ),
    },
    doc = "Builds one out-of-tree Rust-for-Linux loadable module.",
)

_linux_cc_module = rule(
    implementation = _linux_cc_module_impl,
    attrs = {
        "copts": attr.string_list(),
        "deps": attr.label_list(
            providers = [LinuxModuleInfo],
        ),
        "kernel": attr.label(
            mandatory = True,
            providers = [LinuxModuleSdkInfo],
        ),
        "srcs": attr.label_list(
            allow_files = [".c"],
            mandatory = True,
        ),
        "_modulemodinfo": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/modulemodinfo"),
            executable = True,
        ),
        "_objtoolrun": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/objtoolrun"),
            executable = True,
        ),
        "_runincwd": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/runincwd"),
            executable = True,
        ),
    },
    doc = "Builds one out-of-tree C Linux loadable module.",
)

def linux_module(
        name,
        kernel,
        srcs,
        crate_root = None,
        deps = [],
        **kwargs):
    """Builds a Rust-for-Linux module on the supported Linux executor."""
    _linux_module(
        name = name,
        crate_root = crate_root,
        deps = deps,
        exec_compatible_with = [
            Label("@platforms//cpu:x86_64"),
            Label("@platforms//os:linux"),
        ],
        kernel = kernel,
        srcs = srcs,
        **kwargs
    )

def linux_cc_module(
        name,
        kernel,
        srcs,
        copts = [],
        deps = [],
        **kwargs):
    """Builds one out-of-tree C Linux module on the supported Linux executor."""
    _linux_cc_module(
        name = name,
        copts = copts,
        deps = deps,
        exec_compatible_with = [
            Label("@platforms//cpu:x86_64"),
            Label("@platforms//os:linux"),
        ],
        kernel = kernel,
        srcs = srcs,
        **kwargs
    )
