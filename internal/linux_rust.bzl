"""Private Rust-for-Linux SDK actions."""

load("@rules_cc//cc:action_names.bzl", "CPP_LINK_EXECUTABLE_ACTION_NAME", "C_COMPILE_ACTION_NAME")
load("@rules_cc//cc:find_cc_toolchain.bzl", "CC_TOOLCHAIN_TYPE", "find_cpp_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load(":linux_objects.bzl", "LinuxConfigInfo", "LinuxGeneratedHeadersInfo", "linux_module_cc_helpers")
load(":providers.bzl", "LinuxRustSdkInfo")

visibility("//...")

_RUST_TOOLCHAIN_TYPE = Label("@rules_rust//rust:toolchain_type")
_BINDGEN_TOOLCHAIN_TYPE = Label("@rules_rs//rs:bindgen_toolchain_type")
_RUSTC_VERSION = "1.97.0"

_RUST_ALLOWED_FEATURES = [
    "asm_const",
    "asm_goto",
    "arbitrary_self_types",
    "lint_reasons",
    "offset_of_nested",
    "raw_ref_op",
    "used_with_arg",
]

_RUST_COMMON_FLAGS = [
    "--edition=2021",
    "-Zbinary_dep_depinfo=y",
    "-Astable_features",
    "-Aunused_features",
    "-Dnon_ascii_idents",
    "-Dunsafe_op_in_unsafe_fn",
    "-Wmissing_docs",
    "-Wrust_2018_idioms",
    "-Wunreachable_pub",
    "-Wclippy::all",
    "-Wclippy::as_ptr_cast_mut",
    "-Wclippy::as_underscore",
    "-Wclippy::cast_lossless",
    "-Aclippy::collapsible_if",
    "-Aclippy::collapsible_match",
    "-Wclippy::ignored_unit_patterns",
    "-Wclippy::mut_mut",
    "-Wclippy::needless_bitwise_bool",
    "-Aclippy::needless_lifetimes",
    "-Wclippy::no_mangle_with_rust_abi",
    "-Wclippy::ptr_as_ptr",
    "-Wclippy::ptr_cast_constness",
    "-Wclippy::ref_as_ptr",
    "-Wclippy::undocumented_unsafe_blocks",
    "-Aclippy::uninlined_format_args",
    "-Wclippy::unnecessary_safety_comment",
    "-Wclippy::unnecessary_safety_doc",
    "-Wrustdoc::missing_crate_level_docs",
    "-Wrustdoc::unescaped_backticks",
]

_REDIRECT_INTRINSICS = [
    "__addsf3",
    "__eqsf2",
    "__extendsfdf2",
    "__gesf2",
    "__lesf2",
    "__ltsf2",
    "__mulsf3",
    "__nesf2",
    "__truncdfsf2",
    "__unordsf2",
    "__adddf3",
    "__eqdf2",
    "__ledf2",
    "__ltdf2",
    "__muldf3",
    "__unorddf2",
    "__muloti4",
    "__multi3",
    "__udivmodti4",
    "__udivti3",
    "__umodti3",
]

def _execroot_path(file):
    path = file.short_path.replace("\\", "/")
    if path.startswith("../"):
        return "external/" + path[3:]
    return path

def _execroot_dir(file):
    path = _execroot_path(file)
    return path.rsplit("/", 1)[0] if "/" in path else ""

def _source_files(source_root, source_tree):
    root = _execroot_dir(source_root)
    files = {}
    for file in source_tree.to_list():
        path = _execroot_path(file)
        if path.startswith(root + "/"):
            files[path[len(root) + 1:]] = file
    return files

def _source_file(files, path):
    file = files.get(path)
    if file == None:
        fail("Rust-for-Linux SDK is missing source input %s" % path)
    return file

def _source_subtree(files, prefix):
    return [
        files[path]
        for path in sorted(files.keys())
        if path.startswith(prefix)
    ]

def _unwrap_cc_toolchain(toolchain):
    if hasattr(toolchain, "cc_provider_in_toolchain") and hasattr(toolchain, "cc"):
        return toolchain.cc
    return toolchain

def _host_cc_toolchain(ctx):
    return _unwrap_cc_toolchain(
        ctx.exec_groups["host_cc"].toolchains[CC_TOOLCHAIN_TYPE],
    )

def _rust_env(rust_toolchain, objtree = None, modfile = None):
    env = dict(rust_toolchain.env)
    env["RUSTC_BOOTSTRAP"] = "1"
    if objtree != None:
        env["OBJTREE"] = "{cwd}/" + objtree
    if modfile != None:
        env["RUST_MODFILE"] = modfile
    return env

def _run_rustc(
        ctx,
        rust_toolchain,
        args,
        inputs,
        outputs,
        mnemonic,
        progress_message,
        objtree = None,
        transitive_tool_inputs = []):
    runner_args = ctx.actions.args()
    runner_args.add("-cwd", ".")
    env = _rust_env(rust_toolchain, objtree = objtree)
    for name in sorted(env.keys()):
        runner_args.add("-env", name + "=" + env[name])
    runner_args.add("--")
    runner_args.add(ctx.executable._rustcversionrun)
    runner_args.add("-expected")
    runner_args.add(_RUSTC_VERSION)
    runner_args.add("--")
    runner_args.add(rust_toolchain.rustc)
    runner_args.add_all(args)
    ctx.actions.run(
        executable = ctx.executable._runincwd,
        inputs = depset(
            inputs,
            transitive = [rust_toolchain.all_files] + transitive_tool_inputs,
        ),
        outputs = outputs,
        arguments = [runner_args],
        tools = [ctx.attr._rustcversionrun[DefaultInfo].files_to_run],
        mnemonic = mnemonic,
        progress_message = progress_message,
    )

def _target_spec(ctx, config):
    features = "-mmx,+soft-float"
    if config.config_flags.get("CONFIG_MITIGATION_RETPOLINE") == "y":
        features += ",+retpoline-external-thunk,+retpoline-indirect-branches,+retpoline-indirect-calls"
    if config.config_flags.get("CONFIG_MITIGATION_SLS") == "y":
        features += ",+harden-sls-ijmp,+harden-sls-ret"
    spec = {
        "arch": "x86_64",
        "data-layout": "e-m:e-p270:32:32-p271:32:32-p272:64:64-i64:64-i128:128-f80:128-n8:16:32:64-S128",
        "emit-debug-gdb-scripts": False,
        "features": features,
        "frame-pointer": "may-omit",
        "llvm-target": "x86_64-linux-gnu",
        "rustc-abi": "x86-softfloat",
        "stack-probes": {"kind": "none"},
        "supported-sanitizers": ["kcfi", "kernel-address"],
        "target-pointer-width": 64,
    }
    out = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/scripts/target.json")
    ctx.actions.write(out, json.encode(spec) + "\n")
    return out

def _target_rust_flags(config, target_spec):
    flags = list(_RUST_COMMON_FLAGS)
    flags.extend([
        "-Cpanic=abort",
        "-Cembed-bitcode=n",
        "-Clto=n",
        "-Cforce-unwind-tables=n",
        "-Ccodegen-units=1",
        "-Csymbol-mangling-version=v0",
        "-Crelocation-model=static",
        "-Zfunction-sections=n",
        "-Wclippy::float_arithmetic",
        "--target=" + target_spec.path,
        "-Ctarget-feature=-sse,-sse2,-sse3,-ssse3,-sse4.1,-sse4.2,-avx,-avx2",
        "-Ctarget-cpu=x86-64",
        "-Ztune-cpu=generic",
        "-Cno-redzone=y",
        "-Ccode-model=kernel",
    ])
    if config.config_flags.get("CONFIG_CC_OPTIMIZE_FOR_SIZE") == "y":
        flags.append("-Copt-level=s")
    else:
        flags.append("-Copt-level=2")
    flags.extend([
        "-Cdebug-assertions=" + ("y" if config.config_flags.get("CONFIG_RUST_DEBUG_ASSERTIONS") == "y" else "n"),
        "-Coverflow-checks=" + ("y" if config.config_flags.get("CONFIG_RUST_OVERFLOW_CHECKS") == "y" else "n"),
    ])
    if config.config_flags.get("CONFIG_FRAME_POINTER") == "y":
        flags.extend([
            "-Cforce-frame-pointers=y",
            "-Zllvm_module_flag=frame-pointer:u32:2:max",
        ])
    if config.config_flags.get("CONFIG_MITIGATION_RETHUNK") == "y":
        flags.append("-Zfunction-return=thunk-extern")
    if config.config_flags.get("CONFIG_X86_KERNEL_IBT") == "y":
        flags.extend(["-Zcf-protection=branch", "-Cjump-tables=n"])
    if config.config_flags.get("CONFIG_CALL_PADDING") == "y":
        padding = config.config_flags.get("CONFIG_FUNCTION_PADDING_BYTES", "0")
        flags.append("-Zpatchable-function-entry=%s,%s" % (padding, padding))
    flags.append("@" + config.rustc_cfg.path)
    return flags

def _unsupported_rust_config_symbols(config):
    unsupported = []
    for symbol in [
        "CONFIG_MODVERSIONS",
        "CONFIG_RUST_KERNEL_DOCTESTS",
        "CONFIG_TRIM_UNUSED_KSYMS",
        "CONFIG_LD_DEAD_CODE_DATA_ELIMINATION",
        "CONFIG_LTO_CLANG",
        "CONFIG_LTO_CLANG_THIN",
        "CONFIG_LTO_CLANG_FULL",
        "CONFIG_CFI_CLANG",
        "CONFIG_KASAN",
        "CONFIG_KCSAN",
        "CONFIG_KCOV",
        "CONFIG_SHADOW_CALL_STACK",
        "CONFIG_DEBUG_INFO_BTF",
        "CONFIG_DEBUG_INFO_DWARF4",
        "CONFIG_DEBUG_INFO_DWARF5",
        "CONFIG_DEBUG_INFO_DWARF_TOOLCHAIN_DEFAULT",
    ]:
        if config.config_flags.get(symbol) in ["y", "m"]:
            unsupported.append(symbol)
    return unsupported

def _validate_rust_config(ctx, config):
    if ctx.attr.arch != "x86":
        fail("%s enables CONFIG_RUST, but the initial Rust-for-Linux SDK supports only x86_64" % ctx.label)
    if not ctx.attr.version.startswith("6.18."):
        fail("%s enables CONFIG_RUST for Linux %s; only the Linux 6.18 crate graph is supported" % (ctx.label, ctx.attr.version))
    unsupported = _unsupported_rust_config_symbols(config)
    if unsupported:
        fail("%s enables Rust configuration paths not yet modeled: %s" % (ctx.label, unsupported))

def _kernel_c_flags(ctx, config, generated_headers, cc_toolchain, feature_configuration, source_root):
    flags = []
    flags.extend(linux_module_cc_helpers.compile_flags(
        ctx,
        cc_toolchain,
        feature_configuration,
    ))
    flags.append("@" + config.cflags.path)
    if generated_headers.cflags != None:
        flags.append("@" + generated_headers.cflags.path)
    flags.extend(linux_module_cc_helpers.source_preinclude_flags(source_root))
    flags.append("-I" + config.include_dir)
    flags.extend(linux_module_cc_helpers.source_include_flags(
        source_root,
        ctx.attr.srcarch,
        generated_headers.include_dirs,
    ))
    flags.append("-fmacro-prefix-map=%s/=" % source_root)
    return flags

def _target_inputs(config, generated_headers, source_tree, cc_toolchain, direct = []):
    return depset(
        direct,
        transitive = [
            cc_toolchain.all_files,
            config.files,
            generated_headers.files,
            source_tree,
        ],
    )

def _postprocess(ctx, mode, input, output):
    args = ctx.actions.args()
    args.add("-mode", mode)
    args.add("-in", input)
    args.add("-out", output)
    ctx.actions.run(
        executable = ctx.executable._rustpostprocess,
        inputs = [input],
        outputs = [output],
        arguments = [args],
        mnemonic = "LinuxRustPostprocess",
        progress_message = "Postprocessing Rust-for-Linux output %{label}",
    )

def _process_objtool(ctx, config, input, path):
    out = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/" + path)
    if not ctx.executable.objtool:
        ctx.actions.symlink(output = out, target_file = input)
        return out
    args = ctx.actions.args()
    args.add("-config", config.config)
    args.add("-objtool", ctx.executable.objtool)
    args.add("-in", input)
    args.add("-mode", "builtin")
    args.add("-out", out)
    ctx.actions.run(
        executable = ctx.executable._objtoolrun,
        inputs = [config.config, input],
        tools = [ctx.attr.objtool[DefaultInfo].files_to_run],
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxRustObjtool",
        progress_message = "Processing Rust-for-Linux object with objtool %{label}",
    )
    return out

def _preprocess_rust_asm(
        ctx,
        config,
        generated_headers,
        source_tree,
        cc_toolchain,
        compiler,
        c_flags,
        source,
        output_path):
    raw = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/" + output_path + ".preprocessed")
    args = ctx.actions.args()
    args.add_all(c_flags)
    args.add_all(["-E", "-xc", "-C", "-P"])
    args.add(source)
    args.add("-o", raw)
    ctx.actions.run(
        executable = compiler,
        inputs = _target_inputs(
            config,
            generated_headers,
            source_tree,
            cc_toolchain,
            [source],
        ),
        outputs = [raw],
        arguments = [args],
        mnemonic = "LinuxRustAsmPreprocess",
        progress_message = "Preprocessing Rust-for-Linux assembly source %{label}",
    )
    out = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/" + output_path)
    _postprocess(ctx, "rust-asm", raw, out)
    return out

def _bindgen_common_flags():
    return [
        "--rust-target",
        "1.68",
        "--use-core",
        "--with-derive-default",
        "--ctypes-prefix",
        "ffi",
        "--no-layout-tests",
        "--no-debug",
        ".*",
        "--enable-function-attribute-detection",
    ]

def _run_bindgen_with_parameters(
        ctx,
        bindgen,
        cc_toolchain,
        config,
        generated_headers,
        source_tree,
        header,
        parameters,
        c_flags,
        output,
        mode = None):
    raw = output
    if mode != None:
        raw = ctx.actions.declare_file(output.basename + ".raw", sibling = output)
    args = ctx.actions.args()
    args.add("-args_file", parameters)
    args.add("-output", raw)
    args.add("--")
    args.add(bindgen)
    args.add(header)
    args.add("{args_file}")
    args.add_all(_bindgen_common_flags())
    args.add("-o", "{output}")
    args.add("--")
    args.add_all(c_flags)
    ctx.actions.run(
        executable = ctx.executable._lineargsrun,
        inputs = depset(
            [header, parameters],
            transitive = [
                config.files,
                generated_headers.files,
                source_tree,
            ],
        ),
        tools = depset([bindgen], transitive = [cc_toolchain.all_files]),
        outputs = [raw],
        arguments = [args],
        mnemonic = "LinuxRustBindgen",
        progress_message = "Generating Rust-for-Linux bindings %{label}",
    )
    if mode != None:
        _postprocess(ctx, mode, raw, output)
    return output

def _run_helpers_bindgen(
        ctx,
        bindgen,
        cc_toolchain,
        config,
        generated_headers,
        source_tree,
        source,
        c_flags,
        rust_dir):
    raw = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/rust/bindings/bindings_helpers_generated.rs.raw")
    args = ctx.actions.args()
    args.add(source)
    args.add("--blocklist-type", ".*")
    args.add("--allowlist-var", "")
    args.add("--allowlist-function", "rust_helper_.*")
    args.add_all(_bindgen_common_flags())
    args.add("-o", raw)
    args.add("--")
    args.add_all(c_flags)
    args.add("-I" + rust_dir)
    args.add("-Wno-missing-prototypes")
    args.add("-Wno-missing-declarations")
    ctx.actions.run(
        executable = bindgen,
        inputs = depset(
            [source],
            transitive = [
                config.files,
                generated_headers.files,
                source_tree,
            ],
        ),
        tools = cc_toolchain.all_files,
        outputs = [raw],
        arguments = [args],
        mnemonic = "LinuxRustBindgenHelpers",
        progress_message = "Generating Rust-for-Linux helper bindings %{label}",
    )
    out = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/rust/bindings/bindings_helpers_generated.rs")
    _postprocess(ctx, "helpers", raw, out)
    return out

def _compile_kernel_c(
        ctx,
        config,
        generated_headers,
        source_tree,
        cc_toolchain,
        compiler,
        c_flags,
        source,
        object_path,
        extra_flags = [],
        direct_inputs = []):
    raw = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/" + object_path + ".compile")
    args = ctx.actions.args()
    args.add_all(c_flags)
    args.add_all(extra_flags)
    args.add_all(linux_module_cc_helpers.object_name_flags(object_path, object_path))
    args.add("-c", source)
    args.add("-o", raw)
    ctx.actions.run(
        executable = compiler,
        inputs = _target_inputs(
            config,
            generated_headers,
            source_tree,
            cc_toolchain,
            [source] + direct_inputs,
        ),
        outputs = [raw],
        arguments = [args],
        mnemonic = "LinuxRustCCompile",
        progress_message = "Compiling Rust-for-Linux C support %{label}",
    )
    return _process_objtool(ctx, config, raw, object_path)

def _objcopy(ctx, input, path, flags):
    out = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/" + path)
    args = ctx.actions.args()
    args.add_all(flags)
    args.add(input)
    args.add(out)
    ctx.actions.run(
        executable = ctx.executable._llvm_objcopy,
        inputs = [input],
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxRustObjcopy",
        progress_message = "Postprocessing Rust-for-Linux object %{label}",
    )
    return out

def _rust_crate(
        ctx,
        rust_toolchain,
        config,
        objtree,
        rust_dir,
        target_flags,
        crate,
        source,
        source_inputs,
        dep_inputs,
        crate_flags = [],
        objcopy_flags = []):
    raw_object = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/rust/" + crate + ".rustc.o")
    metadata = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/rust/lib" + crate + ".rmeta")
    flags = list(target_flags)
    if crate == "core":
        flags = [
            flag
            for flag in flags
            if flag not in ["--edition=2021", "-Wunreachable_pub"]
        ]
        flags.extend(["--edition=2024", "--cfg", "no_fp_fmt_parse"])
    flags.extend(crate_flags)
    flags.extend([
        "--emit=obj=" + raw_object.path,
        "--emit=metadata=" + metadata.path,
        "--crate-type",
        "rlib",
        "-L" + rust_dir,
        "--crate-name",
        crate,
        source.path,
        "--sysroot=/dev/null",
        "-Zunstable-options",
    ])
    _run_rustc(
        ctx,
        rust_toolchain,
        flags,
        source_inputs + dep_inputs + [config.rustc_cfg],
        [raw_object, metadata],
        "LinuxRustc",
        "Compiling Rust-for-Linux crate %s %%{label}" % crate,
        objtree = objtree,
    )
    processed = raw_object
    if objcopy_flags:
        processed = _objcopy(ctx, raw_object, "rust/" + crate + ".objcopy.o", objcopy_flags)
    return struct(
        metadata = metadata,
        object = _process_objtool(ctx, config, processed, "rust/" + crate + ".o"),
    )

def _export_header(ctx, object, name):
    symbols = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/rust/." + name + ".nm")
    nm_args = ctx.actions.args()
    nm_args.add("-nm", ctx.executable._llvm_nm)
    nm_args.add("-in", object)
    nm_args.add("-out", symbols)
    ctx.actions.run(
        executable = ctx.executable._nmrun,
        inputs = [object],
        tools = [ctx.executable._llvm_nm],
        outputs = [symbols],
        arguments = [nm_args],
        mnemonic = "LinuxRustNm",
        progress_message = "Reading Rust-for-Linux exports %{label}",
    )
    out = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/rust/exports_" + name + "_generated.h")
    _postprocess(ctx, "exports", symbols, out)
    return out

def _proc_macro(
        ctx,
        rust_toolchain,
        host_cc,
        host_features,
        config,
        name,
        source,
        source_inputs,
        crate_flags = []):
    extension = rust_toolchain.dylib_ext
    output = ctx.actions.declare_file(
        ctx.label.name + ".rust_sdk/rust/lib" + name + extension,
    )
    host_compiler = cc_common.get_tool_for_action(
        feature_configuration = host_features,
        action_name = C_COMPILE_ACTION_NAME,
    )
    link_variables = cc_common.create_link_variables(
        cc_toolchain = host_cc,
        feature_configuration = host_features,
        is_linking_dynamic_library = False,
        is_using_linker = True,
    )
    host_link_flags = cc_common.get_memory_inefficient_command_line(
        feature_configuration = host_features,
        action_name = CPP_LINK_EXECUTABLE_ACTION_NAME,
        variables = link_variables,
    )
    args = list(_RUST_COMMON_FLAGS)
    args.extend(crate_flags)
    args.extend([
        "--sysroot=" + rust_toolchain.sysroot,
        "-Clinker-flavor=gcc",
        "-Clinker=" + host_compiler,
    ])
    args.extend(["-Clink-arg=" + flag for flag in host_link_flags])
    args.extend([
        "--emit=link=" + output.path,
        "--extern",
        "proc_macro",
        "--crate-type",
        "proc-macro",
        "--crate-name",
        name,
        "@" + config.rustc_cfg.path,
        source.path,
    ])
    _run_rustc(
        ctx,
        rust_toolchain,
        args,
        source_inputs + [config.rustc_cfg],
        [output],
        "LinuxRustProcMacro",
        "Compiling Rust-for-Linux procedural macro %s %%{label}" % name,
        transitive_tool_inputs = [host_cc.all_files],
    )
    return output

def _core_sources(rustc_srcs):
    marker = "/library/core/src/"
    crate_root = None
    for file in rustc_srcs:
        path = "/" + file.short_path.replace("\\", "/").lstrip("/")
        if marker in path and path.endswith("/core/src/lib.rs"):
            crate_root = file
    if crate_root == None:
        fail("pinned Rust sources do not expose library/core/src/lib.rs")
    return crate_root, rustc_srcs

def _disabled_sdk():
    return LinuxRustSdkInfo(
        compile_inputs = depset(),
        enabled = False,
        module_flags = [],
        objtool = None,
        objtree = "",
        rust_dir = "",
        rustc = None,
        rustc_env = {},
        rustc_files = depset(),
        rustc_version = "",
        rustc_version_runner = None,
        runtime_objects = [],
        target_spec = None,
    )

def _linux_disabled_rust_kernel_sdk_impl(_ctx):
    return [
        DefaultInfo(files = depset()),
        _disabled_sdk(),
    ]

linux_disabled_rust_kernel_sdk = rule(
    implementation = _linux_disabled_rust_kernel_sdk_impl,
    doc = "Provides an empty Rust SDK without resolving Rust build dependencies.",
)

def _linux_rust_kernel_sdk_impl(ctx):
    config = ctx.attr.config[LinuxConfigInfo]
    if config.config_flags.get("CONFIG_RUST") != "y":
        fail("%s requires CONFIG_RUST=y" % ctx.label)

    _validate_rust_config(ctx, config)
    rust_toolchain = ctx.toolchains[_RUST_TOOLCHAIN_TYPE]
    generated_headers = ctx.attr.generated_headers[LinuxGeneratedHeadersInfo]
    source_tree = depset(ctx.files.source_tree)
    source_files = _source_files(ctx.file.source_root, source_tree)
    source_root = _execroot_dir(ctx.file.source_root)
    cc_toolchain = find_cpp_toolchain(ctx)
    feature_configuration = linux_module_cc_helpers.configure_features(ctx, cc_toolchain)
    compiler = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
    )
    host_cc = _host_cc_toolchain(ctx)
    host_features = cc_common.configure_features(
        ctx = ctx,
        cc_toolchain = host_cc,
        requested_features = [],
        unsupported_features = [],
    )
    bindgen = ctx.toolchains[_BINDGEN_TOOLCHAIN_TYPE].bindgen
    target_spec = _target_spec(ctx, config)
    objtree = target_spec.dirname.rsplit("/", 1)[0]
    rust_dir = objtree + "/rust"
    c_flags = _kernel_c_flags(
        ctx,
        config,
        generated_headers,
        cc_toolchain,
        feature_configuration,
        source_root,
    )
    bindgen_c_flags = c_flags + [
        "-fno-builtin",
        "-D__BINDGEN__",
        "-DMODULE",
    ]

    bindgen_parameters = _source_file(source_files, "rust/bindgen_parameters")
    bindings_generated = ctx.actions.declare_file(
        ctx.label.name + ".rust_sdk/rust/bindings/bindings_generated.rs",
    )
    _run_bindgen_with_parameters(
        ctx,
        bindgen,
        cc_toolchain,
        config,
        generated_headers,
        source_tree,
        _source_file(source_files, "rust/bindings/bindings_helper.h"),
        bindgen_parameters,
        bindgen_c_flags + linux_module_cc_helpers.object_name_flags(
            "rust/bindings/bindings_generated.o",
        ),
        bindings_generated,
        mode = "bindings",
    )
    uapi_generated = ctx.actions.declare_file(
        ctx.label.name + ".rust_sdk/rust/uapi/uapi_generated.rs",
    )
    _run_bindgen_with_parameters(
        ctx,
        bindgen,
        cc_toolchain,
        config,
        generated_headers,
        source_tree,
        _source_file(source_files, "rust/uapi/uapi_helper.h"),
        bindgen_parameters,
        bindgen_c_flags + linux_module_cc_helpers.object_name_flags(
            "rust/uapi/uapi_generated.o",
        ),
        uapi_generated,
    )
    helpers_source = _source_file(source_files, "rust/helpers/helpers.c")
    bindings_helpers_generated = _run_helpers_bindgen(
        ctx,
        bindgen,
        cc_toolchain,
        config,
        generated_headers,
        source_tree,
        helpers_source,
        bindgen_c_flags + linux_module_cc_helpers.object_name_flags(
            "rust/bindings/bindings_helpers_generated.o",
        ),
        rust_dir,
    )

    generated_arch_sources = []
    if config.config_flags.get("CONFIG_JUMP_LABEL") == "y":
        generated_arch_sources.append(_preprocess_rust_asm(
            ctx,
            config,
            generated_headers,
            source_tree,
            cc_toolchain,
            compiler,
            c_flags,
            _source_file(source_files, "rust/kernel/generated_arch_static_branch_asm.rs.S"),
            "rust/kernel/generated_arch_static_branch_asm.rs",
        ))
    if config.config_flags.get("CONFIG_BUG") == "y":
        for basename in [
            "generated_arch_warn_asm.rs",
            "generated_arch_reachable_asm.rs",
        ]:
            generated_arch_sources.append(_preprocess_rust_asm(
                ctx,
                config,
                generated_headers,
                source_tree,
                cc_toolchain,
                compiler,
                c_flags,
                _source_file(source_files, "rust/kernel/" + basename + ".S"),
                "rust/kernel/" + basename,
            ))

    macros = _proc_macro(
        ctx,
        rust_toolchain,
        host_cc,
        host_features,
        config,
        "macros",
        _source_file(source_files, "rust/macros/lib.rs"),
        _source_subtree(source_files, "rust/macros/"),
    )
    pin_init_internal = _proc_macro(
        ctx,
        rust_toolchain,
        host_cc,
        host_features,
        config,
        "pin_init_internal",
        _source_file(source_files, "rust/pin-init/internal/src/lib.rs"),
        _source_subtree(source_files, "rust/pin-init/internal/src/") + [
            _source_file(source_files, "rust/macros/quote.rs"),
        ],
        crate_flags = ["--cfg", "kernel"],
    )

    target_flags = _target_rust_flags(config, target_spec)
    common_dep_inputs = [target_spec]
    core_root, core_source_inputs = _core_sources(ctx.files._rustc_srcs)
    core_objcopy_flags = []
    for symbol in _REDIRECT_INTRINSICS:
        core_objcopy_flags.extend([
            "--redefine-sym",
            symbol + "=__rust" + symbol,
        ])
    core = _rust_crate(
        ctx,
        rust_toolchain,
        config,
        objtree,
        rust_dir,
        target_flags,
        "core",
        core_root,
        core_source_inputs,
        common_dep_inputs,
        objcopy_flags = core_objcopy_flags,
    )
    compiler_builtins = _rust_crate(
        ctx,
        rust_toolchain,
        config,
        objtree,
        rust_dir,
        target_flags,
        "compiler_builtins",
        _source_file(source_files, "rust/compiler_builtins.rs"),
        [_source_file(source_files, "rust/compiler_builtins.rs")],
        common_dep_inputs + [core.metadata],
        objcopy_flags = ["-w", "-W", "__*"],
    )
    ffi = _rust_crate(
        ctx,
        rust_toolchain,
        config,
        objtree,
        rust_dir,
        target_flags,
        "ffi",
        _source_file(source_files, "rust/ffi.rs"),
        [_source_file(source_files, "rust/ffi.rs")],
        common_dep_inputs + [core.metadata, compiler_builtins.metadata],
    )
    build_error = _rust_crate(
        ctx,
        rust_toolchain,
        config,
        objtree,
        rust_dir,
        target_flags,
        "build_error",
        _source_file(source_files, "rust/build_error.rs"),
        [_source_file(source_files, "rust/build_error.rs")],
        common_dep_inputs + [core.metadata, compiler_builtins.metadata],
    )
    pin_init = _rust_crate(
        ctx,
        rust_toolchain,
        config,
        objtree,
        rust_dir,
        target_flags,
        "pin_init",
        _source_file(source_files, "rust/pin-init/src/lib.rs"),
        _source_subtree(source_files, "rust/pin-init/src/"),
        common_dep_inputs + [
            core.metadata,
            compiler_builtins.metadata,
            macros,
            pin_init_internal,
        ],
        crate_flags = [
            "--extern",
            "pin_init_internal",
            "--extern",
            "macros",
            "--cfg",
            "kernel",
        ],
    )
    bindings = _rust_crate(
        ctx,
        rust_toolchain,
        config,
        objtree,
        rust_dir,
        target_flags,
        "bindings",
        _source_file(source_files, "rust/bindings/lib.rs"),
        _source_subtree(source_files, "rust/bindings/") + [
            bindings_generated,
            bindings_helpers_generated,
        ],
        common_dep_inputs + [
            core.metadata,
            compiler_builtins.metadata,
            ffi.metadata,
            pin_init.metadata,
            macros,
            pin_init_internal,
        ],
        crate_flags = ["--extern", "ffi", "--extern", "pin_init"],
    )
    uapi = _rust_crate(
        ctx,
        rust_toolchain,
        config,
        objtree,
        rust_dir,
        target_flags,
        "uapi",
        _source_file(source_files, "rust/uapi/lib.rs"),
        _source_subtree(source_files, "rust/uapi/") + [uapi_generated],
        common_dep_inputs + [
            core.metadata,
            compiler_builtins.metadata,
            ffi.metadata,
            pin_init.metadata,
            macros,
            pin_init_internal,
        ],
        crate_flags = ["--extern", "ffi", "--extern", "pin_init"],
    )
    kernel = _rust_crate(
        ctx,
        rust_toolchain,
        config,
        objtree,
        rust_dir,
        target_flags,
        "kernel",
        _source_file(source_files, "rust/kernel/lib.rs"),
        _source_subtree(source_files, "rust/kernel/") + generated_arch_sources,
        common_dep_inputs + [
            core.metadata,
            compiler_builtins.metadata,
            ffi.metadata,
            build_error.metadata,
            pin_init.metadata,
            bindings.metadata,
            uapi.metadata,
            macros,
            pin_init_internal,
        ],
        crate_flags = [
            "--extern",
            "ffi",
            "--extern",
            "pin_init",
            "--extern",
            "build_error",
            "--extern",
            "macros",
            "--extern",
            "bindings",
            "--extern",
            "uapi",
        ],
    )

    helpers = _compile_kernel_c(
        ctx,
        config,
        generated_headers,
        source_tree,
        cc_toolchain,
        compiler,
        c_flags,
        helpers_source,
        "rust/helpers/helpers.o",
        extra_flags = [
            "-Wno-missing-prototypes",
            "-Wno-missing-declarations",
        ],
    )
    export_headers = [
        _export_header(ctx, core.object, "core"),
        _export_header(ctx, helpers, "helpers"),
        _export_header(ctx, bindings.object, "bindings"),
        _export_header(ctx, kernel.object, "kernel"),
    ]
    exports = _compile_kernel_c(
        ctx,
        config,
        generated_headers,
        source_tree,
        cc_toolchain,
        compiler,
        c_flags,
        _source_file(source_files, "rust/exports.c"),
        "rust/exports.o",
        extra_flags = ["-I" + rust_dir],
        direct_inputs = export_headers,
    )

    runtime_objects = [
        core.object,
        compiler_builtins.object,
        ffi.object,
        helpers,
        bindings.object,
        pin_init.object,
        kernel.object,
        uapi.object,
    ]
    if config.config_flags.get("CONFIG_RUST_BUILD_ASSERT_ALLOW") == "y":
        runtime_objects.append(build_error.object)
    runtime_objects.append(exports)

    metadata = [
        core.metadata,
        compiler_builtins.metadata,
        ffi.metadata,
        build_error.metadata,
        pin_init.metadata,
        bindings.metadata,
        uapi.metadata,
        kernel.metadata,
        macros,
        pin_init_internal,
        target_spec,
        config.rustc_cfg,
        bindings_generated,
        bindings_helpers_generated,
        uapi_generated,
    ] + generated_arch_sources
    compile_inputs = depset(
        metadata,
        transitive = [rust_toolchain.all_files],
    )
    module_flags = target_flags + [
        "--cfg",
        "MODULE",
        "-Zallow-features=" + ",".join(_RUST_ALLOWED_FEATURES),
        "-Zcrate-attr=no_std",
        "-Zcrate-attr=feature(%s)" % ",".join(_RUST_ALLOWED_FEATURES),
        "-Zunstable-options",
        "--extern",
        "pin_init",
        "--extern",
        "kernel",
        "--crate-type",
        "rlib",
        "-L" + rust_dir,
        "--sysroot=/dev/null",
    ]
    sdk = LinuxRustSdkInfo(
        compile_inputs = compile_inputs,
        enabled = True,
        module_flags = module_flags,
        objtool = ctx.executable.objtool,
        objtree = objtree,
        rust_dir = rust_dir,
        rustc = rust_toolchain.rustc,
        rustc_env = _rust_env(rust_toolchain),
        rustc_files = rust_toolchain.all_files,
        rustc_version = _RUSTC_VERSION,
        rustc_version_runner = ctx.executable._rustcversionrun,
        runtime_objects = runtime_objects,
        target_spec = target_spec,
    )
    return [
        DefaultInfo(files = depset(runtime_objects + metadata)),
        sdk,
        OutputGroupInfo(
            metadata = depset(metadata),
            runtime_objects = depset(runtime_objects),
        ),
    ]

linux_rust_kernel_sdk = rule(
    implementation = _linux_rust_kernel_sdk_impl,
    attrs = {
        "arch": attr.string(mandatory = True),
        "config": attr.label(
            mandatory = True,
            providers = [LinuxConfigInfo],
        ),
        "generated_headers": attr.label(
            mandatory = True,
            providers = [LinuxGeneratedHeadersInfo],
        ),
        "objtool": attr.label(
            cfg = "exec",
            executable = True,
        ),
        "source_root": attr.label(
            allow_single_file = True,
            mandatory = True,
        ),
        "source_tree": attr.label_list(allow_files = True),
        "srcarch": attr.string(mandatory = True),
        "version": attr.string(mandatory = True),
        "_lineargsrun": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/lineargsrun"),
            executable = True,
        ),
        "_llvm_nm": attr.label(
            allow_single_file = True,
            cfg = "exec",
            default = Label("@llvm//tools:llvm-nm"),
            executable = True,
        ),
        "_llvm_objcopy": attr.label(
            allow_single_file = True,
            cfg = "exec",
            default = Label("@llvm//tools:llvm-objcopy"),
            executable = True,
        ),
        "_nmrun": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/nmrun"),
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
        "_rustpostprocess": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/rustpostprocess"),
            executable = True,
        ),
        "_rustc_srcs": attr.label(
            default = Label("@linux_rust_sources//:rustc_srcs"),
        ),
        "_rustcversionrun": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/rustcversionrun"),
            executable = True,
        ),
    },
    exec_groups = {
        "host_cc": exec_group(toolchains = use_cc_toolchain()),
    },
    fragments = ["cpp"],
    toolchains = [
        _RUST_TOOLCHAIN_TYPE,
        _BINDGEN_TOOLCHAIN_TYPE,
    ] + use_cc_toolchain(),
    doc = "Builds the private configuration-specific Rust-for-Linux SDK.",
)

linux_rust_test_helpers = struct(
    unsupported_config_symbols = _unsupported_rust_config_symbols,
)
