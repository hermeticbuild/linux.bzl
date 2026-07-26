"""Private configured-kernel module finalization rule."""

load("@rules_cc//cc:find_cc_toolchain.bzl", "use_cc_toolchain")
load(":linux_module_actions.bzl", "linux_module_actions")
load(":linux_objects.bzl", "linux_module_cc_helpers")
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
        "-Wl,-T," + module_lds.path,
        "-o",
        out,
        preliminary,
        mod_object,
        module_common,
    ])
    ctx.actions.run(
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
    env["OBJTREE"] = "{cwd}/" + rust.objtree
    modfile = ctx.label.package + "/" + crate_name if ctx.label.package else crate_name
    env["RUST_MODFILE"] = modfile
    for name in sorted(env.keys()):
        args.add("-env", name + "=" + env[name])
    args.add("--")
    args.add(rust.rustc_version_runner)
    args.add("-expected")
    args.add(rust.rustc_version)
    args.add("--")
    args.add(rust.rustc)
    args.add_all(rust.module_flags)
    args.add("--crate-name")
    args.add(crate_name)
    args.add_all(_external_rust_output_flags(raw.dirname, raw.path))
    args.add(crate_root)
    ctx.actions.run(
        executable = ctx.executable._runincwd,
        inputs = depset(
            ctx.files.srcs,
            transitive = [rust.compile_inputs, rust.rustc_files],
        ),
        tools = [rust.rustc_version_runner],
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

def _external_rust_output_flags(output_dir, output_path):
    return [
        "--out-dir",
        output_dir,
        "--emit=obj=" + output_path,
    ]

def _check_external_modinfo(ctx, preliminary, crate_name):
    checked = ctx.actions.declare_file(
        ctx.label.name + ".external/" + crate_name + ".modinfo.checked",
    )
    check_args = ctx.actions.args()
    check_args.add("-in", preliminary)
    check_args.add("-out", checked)
    ctx.actions.run(
        executable = ctx.executable._modulemodinfo,
        inputs = [preliminary],
        outputs = [checked],
        arguments = [check_args],
        mnemonic = "LinuxRustModuleModinfoCheck",
        progress_message = "Checking Rust-for-Linux module metadata %{label}",
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
    args.add("-cwd", modules_order.dirname)
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
    ctx.actions.run(
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
    args.add_all(sdk.target_c_flags)
    args.add_all(linux_module_cc_helpers.module_flags("m"))
    args.add_all(linux_module_cc_helpers.object_name_flags(
        crate_name + ".mod.o",
        crate_name + ".o",
    ))
    args.add("-c")
    args.add(source)
    args.add("-o")
    args.add(out)
    ctx.actions.run(
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
    if sdk.arch != "x86_64" or not sdk.version.startswith("6.18."):
        fail("%s supports Rust modules only for x86_64 Linux 6.18.x" % ctx.label)
    if sdk.config.config_flags.get("CONFIG_DEBUG_INFO_BTF_MODULES") == "y":
        fail("%s does not yet support Rust module BTF" % ctx.label)

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
    ctx.actions.symlink(output = out, target_file = linked)
    return [
        DefaultInfo(files = depset([out])),
        LinuxModuleInfo(
            kernel_key = sdk.kernel_key,
            ko = out,
            module_symvers = module_symvers,
        ),
        OutputGroupInfo(module_symvers = depset([module_symvers])),
    ]

def _btf_module(ctx, vmlinux, linked, path, version):
    if vmlinux.config.config_flags.get("CONFIG_DEBUG_INFO_BTF_MODULES") != "y":
        out = ctx.actions.declare_file(ctx.label.name + ".modules/" + path[:-len(".o")] + ".ko")
        ctx.actions.symlink(output = out, target_file = linked)
        return out
    if not ctx.executable.pahole:
        fail("%s enables CONFIG_DEBUG_INFO_BTF_MODULES and requires pahole" % ctx.label)
    if not ctx.executable.resolve_btfids_tool:
        fail("%s enables CONFIG_DEBUG_INFO_BTF_MODULES and requires resolve_btfids_tool" % ctx.label)

    encoded = ctx.actions.declare_file(ctx.label.name + ".modules/" + path[:-len(".o")] + ".ko.btf")
    pahole_args = ctx.actions.args()
    pahole_args.add("-input", linked)
    pahole_args.add("-output", encoded)
    pahole_args.add("-env", "LLVM_OBJCOPY=" + ctx.executable._llvm_objcopy.path)
    pahole_args.add("--")
    pahole_args.add(ctx.executable.pahole)
    pahole_args.add("-J")
    pahole_args.add_all(linux_module_actions.pahole_flags(vmlinux.config, version))
    pahole_args.add("--btf_base")
    pahole_args.add(vmlinux.vmlinux)
    pahole_args.add("{output}")
    ctx.actions.run(
        executable = ctx.executable._btfmutate,
        inputs = [linked, vmlinux.vmlinux],
        tools = [ctx.attr.pahole[DefaultInfo].files_to_run, ctx.attr._llvm_objcopy[DefaultInfo].files_to_run],
        outputs = [encoded],
        arguments = [pahole_args],
        mnemonic = "LinuxModuleBTF",
        progress_message = "Encoding Linux module BTF %{label}",
    )

    out = ctx.actions.declare_file(ctx.label.name + ".modules/" + path[:-len(".o")] + ".ko")
    resolve_args = ctx.actions.args()
    resolve_args.add("-input", encoded)
    resolve_args.add("-output", out)
    resolve_args.add("--")
    resolve_args.add(ctx.executable.resolve_btfids_tool)
    resolve_args.add("-b")
    resolve_args.add(vmlinux.vmlinux)
    resolve_args.add("{output}")
    ctx.actions.run(
        executable = ctx.executable._btfmutate,
        inputs = [encoded, vmlinux.vmlinux],
        tools = [ctx.attr.resolve_btfids_tool[DefaultInfo].files_to_run],
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

def _builtin_module_metadata(ctx, vmlinux):
    raw = ctx.actions.declare_file(ctx.label.name + ".sdk/modules.builtin.modinfo.raw")
    objcopy_args = ctx.actions.args()
    objcopy_args.add_all([
        "-j",
        ".modinfo",
        "-O",
        "binary",
        vmlinux.vmlinux_unstripped,
        raw,
    ])
    ctx.actions.run(
        executable = ctx.executable._llvm_objcopy,
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
    ctx.actions.run(
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

    modules_builtin, modules_builtin_modinfo = _builtin_module_metadata(ctx, vmlinux)
    ko_files = []

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
            ko_files.append(_btf_module(ctx, vmlinux, linked, path, ctx.attr.version))
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
        "_llvm_objcopy": attr.label(
            allow_single_file = True,
            cfg = "exec",
            default = Label("@llvm//tools:llvm-objcopy"),
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
    },
    doc = "Builds one out-of-tree Rust-for-Linux loadable module.",
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
            "@platforms//cpu:x86_64",
            "@platforms//os:linux",
        ],
        kernel = kernel,
        srcs = srcs,
        **kwargs
    )

linux_modules_test_helpers = struct(
    external_rust_output_flags = _external_rust_output_flags,
)
