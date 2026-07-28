"""Private Rust-for-Linux SDK actions."""

load("@rules_cc//cc:action_names.bzl", "CPP_LINK_EXECUTABLE_ACTION_NAME", "C_COMPILE_ACTION_NAME")
load("@rules_cc//cc:find_cc_toolchain.bzl", "find_cpp_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load(":host_cc_toolchain.bzl", "host_cc_toolchain", "host_cc_toolchain_attr")
load(":linux_objects.bzl", "LinuxConfigInfo", "LinuxGeneratedHeadersInfo", "linux_module_cc_helpers")
load(
    ":path_mapping.bzl",
    "add_directory_arg",
    "add_mapped_values",
    "directory_anchor",
    "path_mapped_run",
)
load(":providers.bzl", "LinuxRustSdkInfo")

visibility("//...")

_RUST_TOOLCHAIN_TYPE = Label("@rules_rust//rust:toolchain_type")
_BINDGEN_TOOLCHAIN_TYPE = Label("@rules_rs//rs:bindgen_toolchain_type")
_RUSTC_VERSION = "1.97.0"
_RUST_PROFILE_SCHEMA = "linux-rust-profile-v1"
_RUSTC_SOURCE_PREFIX_CLOSURE = {
    # Rust 1.97 core imports these sibling trees with #[path].
    "rustc://library/core/": [
        "rustc://library/portable-simd/crates/core_simd/",
        "rustc://library/stdarch/crates/core_arch/",
    ],
}

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

def _required_profile_field(value, name, expected_type):
    field = value.get(name)
    if type(field) != expected_type:
        fail("Rust profile field %s must be a %s" % (name, expected_type))
    return field

def _profile_string_list(value, name):
    items = _required_profile_field(value, name, "list")
    for item in items:
        if type(item) != "string":
            fail("Rust profile field %s must contain only strings" % name)
    return items

def _validate_profile_name(name, field):
    if type(name) != "string" or not name:
        fail("Rust profile field %s must be a non-empty string" % field)
    for character in name.elems():
        if not (
            (character >= "a" and character <= "z") or
            (character >= "A" and character <= "Z") or
            (character >= "0" and character <= "9") or
            character == "_"
        ):
            fail("Rust profile field %s contains invalid name %r" % (field, name))
    return name

def _validate_profile_path(path, field):
    if (
        not path or
        path.startswith("/") or
        "\\" in path or
        path == ".." or
        path.startswith("../") or
        "/../" in path or
        path.endswith("/..")
    ):
        fail("Rust profile field %s contains invalid relative path %r" % (field, path))
    return path

def _relative_output_root(file, relative_path):
    root = file.path
    for _segment in relative_path.split("/"):
        if "/" not in root:
            fail("cannot derive output root for %s from %s" % (relative_path, file.path))
        root = root.rsplit("/", 1)[0]
    return root

def _decode_rust_profile(profile_json, arch):
    profile = json.decode(profile_json)
    if type(profile) != "dict":
        fail("Rust profile must decode to an object")
    if profile.get("schema") != _RUST_PROFILE_SCHEMA:
        fail(
            "unsupported Rust profile schema %r; expected %r" %
            (profile.get("schema"), _RUST_PROFILE_SCHEMA),
        )
    if profile.get("source_layout") not in ["legacy", "pin-init"]:
        fail("unsupported Rust profile source layout %r" % profile.get("source_layout"))
    architecture = profile.get("architecture")
    expected_architecture = "x86_64" if arch == "x86" else arch
    if architecture != expected_architecture:
        fail(
            "Rust profile architecture %r does not match Linux ARCH=%r" %
            (architecture, arch),
        )
    for field in [
        "target",
        "target_flags",
        "module",
        "bindgen",
        "exports",
    ]:
        _required_profile_field(profile, field, "dict")
    for field in [
        "common_flags",
        "proc_macros",
        "crates",
        "runtime_objects",
    ]:
        _required_profile_field(profile, field, "list")
    if type(profile.get("generated_assembly", [])) != "list":
        fail("Rust profile field generated_assembly must be a list")
    return profile

def _condition_matches(config, condition):
    symbol = _required_profile_field(condition, "config", "string")
    expected = condition.get("equals", "y")
    if type(expected) != "string":
        fail("Rust profile conditional equals field must be a string")
    return config.config_flags.get(symbol, "n") == expected

def _expand_profile_value(value, config, replacements):
    if type(value) != "string":
        fail("Rust profile flag must be a string, got %s" % type(value))
    expanded = value
    for name in sorted(replacements.keys()):
        expanded = expanded.replace("{" + name + "}", replacements[name])
    for symbol in sorted(config.config_flags.keys()):
        expanded = expanded.replace(
            "{" + symbol + "}",
            config.config_flags[symbol],
        )
    if "{" in expanded or "}" in expanded:
        fail("Rust profile contains an unsupported placeholder in %r" % value)
    return expanded

def _expand_profile_values(values, config, replacements):
    return [
        _expand_profile_value(value, config, replacements)
        for value in values
    ]

def _profile_target_flags(profile, config, replacements):
    target_flags = profile["target_flags"]
    flags = list(profile["common_flags"])
    flags.extend(_required_profile_field(target_flags, "always", "list"))
    for condition in target_flags.get("conditional", []):
        if type(condition) != "dict":
            fail("Rust profile target_flags.conditional entries must be objects")
        branch = "flags" if _condition_matches(config, condition) else "else_flags"
        flags.extend(condition.get(branch, []))
    return _expand_profile_values(flags, config, replacements)

def _profile_inputs(source_files, prefixes, paths):
    inputs = {}
    for prefix in prefixes:
        _validate_profile_path(prefix, "source_prefixes")
        for file in _source_subtree(source_files, prefix):
            inputs[file.path] = file
    for path in paths:
        _validate_profile_path(path, "source_files")
        file = _source_file(source_files, path)
        inputs[file.path] = file
    return [inputs[path] for path in sorted(inputs.keys())]

def _rustc_profile_path(path):
    prefix = "rustc://"
    if not path.startswith(prefix):
        fail("expected rustc source path, got %r" % path)
    return _validate_profile_path(path[len(prefix):], "crate source")

def _rustc_file(rustc_srcs, path):
    suffix = "/" + _rustc_profile_path(path)
    matches = [
        file
        for file in rustc_srcs
        if ("/" + file.short_path.replace("\\", "/").lstrip("/")).endswith(suffix)
    ]
    if len(matches) != 1:
        fail("pinned Rust sources contain %d matches for %s" % (len(matches), path))
    return matches[0]

def _rustc_subtree(rustc_srcs, prefix):
    relative = _rustc_profile_path(prefix)
    marker = "/" + relative
    return [
        file
        for file in rustc_srcs
        if (
            marker in ("/" + file.short_path.replace("\\", "/").lstrip("/")) and
            ("/" + file.short_path.replace("\\", "/").lstrip("/")).split(marker, 1)[1] != ""
        )
    ]

def _rustc_source_prefixes(prefixes):
    expanded = {}
    for prefix in prefixes:
        expanded[prefix] = True
        for transitive_prefix in _RUSTC_SOURCE_PREFIX_CLOSURE.get(prefix, []):
            expanded[transitive_prefix] = True
    return sorted(expanded.keys())

def _rust_env(rust_toolchain):
    env = dict(rust_toolchain.env)
    env["RUSTC_BOOTSTRAP"] = "1"
    return env

def _run_rustc(
        ctx,
        rust_toolchain,
        args,
        inputs,
        outputs,
        mnemonic,
        progress_message,
        mapped_files = [],
        mapped_directories = {},
        objtree_anchor = None,
        transitive_tool_inputs = []):
    runner_args = ctx.actions.args()
    runner_args.add("-cwd", ".")
    env = _rust_env(rust_toolchain)
    for name in sorted(env.keys()):
        runner_args.add("-env", name + "=" + env[name])
    if objtree_anchor != None:
        runner_args.add("-env")
        add_directory_arg(
            runner_args,
            objtree_anchor,
            format = "OBJTREE={cwd}/%s",
        )
    runner_args.add("--")
    runner_args.add(ctx.executable._rustcversionrun)
    runner_args.add("-expected")
    runner_args.add(_RUSTC_VERSION)
    runner_args.add("--")
    runner_args.add(rust_toolchain.rustc)
    add_mapped_values(
        runner_args,
        args,
        files = mapped_files,
        directory_anchors = mapped_directories,
    )
    path_mapped_run(
        ctx.actions,
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

def _host_rust_link(host_cc, host_features):
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
    runtime_files = host_cc.static_runtime_lib(
        feature_configuration = host_features,
    )
    flags = [
        "-Clink-self-contained=-linker",
        "-Clinker-flavor=gcc",
        "-Clinker=" + host_compiler,
    ]
    flags.extend([
        "-Clink-arg=" + file.path
        for file in runtime_files.to_list()
    ])
    flags.extend(["-Clink-arg=" + flag for flag in host_link_flags])
    return struct(
        flags = flags,
        runtime_files = runtime_files,
    )

def _target_spec(
        ctx,
        profile,
        rust_toolchain,
        host_cc,
        host_features,
        config,
        source_files):
    target = profile["target"]
    source_path = _validate_profile_path(
        _required_profile_field(target, "generator_source", "string"),
        "target.generator_source",
    )
    if target.get("stdin") != "config_auto_conf":
        fail("unsupported Rust target generator stdin recipe %r" % target.get("stdin"))
    output_path = _validate_profile_path(
        _required_profile_field(target, "output", "string"),
        "target.output",
    )
    source = _source_file(source_files, source_path)
    generator = ctx.actions.declare_file(
        ctx.label.name + ".rust_sdk/scripts/generate_rust_target",
    )
    host_link = _host_rust_link(host_cc, host_features)
    args = list(profile["common_flags"])
    args.extend(host_link.flags)
    args.extend([
        "--crate-name",
        "generate_rust_target",
        "--emit=link=" + generator.path,
        source.path,
    ])
    _run_rustc(
        ctx,
        rust_toolchain,
        args,
        [source],
        [generator],
        "LinuxRustTargetGeneratorCompile",
        "Compiling the Linux Rust target generator %{label}",
        mapped_files = [generator, source] + host_link.runtime_files.to_list(),
        transitive_tool_inputs = [host_cc.all_files, host_link.runtime_files],
    )

    out = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/" + output_path)
    run_args = ctx.actions.args()
    run_args.add("-stdin")
    run_args.add(config.auto_conf)
    run_args.add(out)
    run_args.add(generator)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._runandwrite,
        inputs = [config.auto_conf],
        tools = [generator],
        outputs = [out],
        arguments = [run_args],
        mnemonic = "LinuxRustTargetGenerate",
        progress_message = "Generating the Linux Rust target specification %{label}",
    )
    return out

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
        fail("%s enables CONFIG_RUST, but the Rust toolchain supports only x86_64" % ctx.label)
    unsupported = _unsupported_rust_config_symbols(config)
    if unsupported:
        fail("%s enables Rust configuration paths not yet modeled: %s" % (ctx.label, unsupported))

def _kernel_c_flags(ctx, config, generated_headers, cc_toolchain, feature_configuration, source_root):
    values = []
    values.extend(linux_module_cc_helpers.compile_flags(
        ctx,
        cc_toolchain,
        feature_configuration,
    ))
    values.append("@" + config.cflags.path)
    mapped_files = [config.cflags]
    if generated_headers.cflags != None:
        values.append("@" + generated_headers.cflags.path)
        mapped_files.append(generated_headers.cflags)
    values.extend(linux_module_cc_helpers.source_preinclude_flags(source_root))
    values.append("-I" + config.include_dir)
    values.extend(linux_module_cc_helpers.source_include_flags(
        source_root,
        ctx.attr.srcarch,
        generated_headers.include_dirs,
    ))
    values.append("-fmacro-prefix-map=%s/=" % source_root)
    directory_anchors = {
        config.include_dir: config.include_dir_anchor,
    }
    directory_anchors.update(generated_headers.include_dir_anchors)
    return struct(
        directory_anchors = directory_anchors,
        mapped_files = mapped_files,
        values = values,
    )

def _add_kernel_c_flags(args, flags):
    add_mapped_values(
        args,
        flags.values,
        files = flags.mapped_files,
        directory_anchors = flags.directory_anchors,
    )

def _extend_kernel_c_flags(flags, values):
    return struct(
        directory_anchors = flags.directory_anchors,
        mapped_files = flags.mapped_files,
        values = flags.values + values,
    )

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
    path_mapped_run(
        ctx.actions,
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
    path_mapped_run(
        ctx.actions,
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
    _add_kernel_c_flags(args, c_flags)
    args.add_all(["-E", "-xc", "-C", "-P"])
    args.add(source)
    args.add("-o", raw)
    path_mapped_run(
        ctx.actions,
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

def _run_bindgen_with_parameters(
        ctx,
        bindgen,
        cc_toolchain,
        config,
        generated_headers,
        source_tree,
        header,
        parameters,
        common_flags,
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
    args.add_all(common_flags)
    args.add("-o", "{output}")
    args.add("--")
    _add_kernel_c_flags(args, c_flags)
    path_mapped_run(
        ctx.actions,
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
        common_flags,
        c_flags,
        rust_dir_anchor):
    raw = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/rust/bindings/bindings_helpers_generated.rs.raw")
    args = ctx.actions.args()
    args.add(source)
    args.add("--blocklist-type", ".*")
    args.add("--allowlist-var", "")
    args.add("--allowlist-function", "rust_helper_.*")
    args.add_all(common_flags)
    args.add("-o", raw)
    args.add("--")
    _add_kernel_c_flags(args, c_flags)
    add_directory_arg(args, rust_dir_anchor, format = "-I%s")
    args.add("-Wno-missing-prototypes")
    args.add("-Wno-missing-declarations")
    path_mapped_run(
        ctx.actions,
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
        extra_directory_flags = [],
        direct_inputs = []):
    raw = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/" + object_path + ".compile")
    args = ctx.actions.args()
    _add_kernel_c_flags(args, c_flags)
    args.add_all(extra_flags)
    for directory_flag in extra_directory_flags:
        add_directory_arg(
            args,
            directory_flag.anchor,
            format = directory_flag.format,
        )
    args.add_all(linux_module_cc_helpers.object_name_flags(object_path, object_path))
    args.add("-c", source)
    args.add("-o", raw)
    path_mapped_run(
        ctx.actions,
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
    path_mapped_run(
        ctx.actions,
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
        objtree_anchor,
        rust_dir,
        rust_dir_anchor,
        target_flags,
        crate,
        source,
        source_inputs,
        dep_inputs,
        crate_flags = [],
        skip_flags = [],
        objcopy_flags = []):
    raw_object = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/rust/" + crate + ".rustc.o")
    metadata = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/rust/lib" + crate + ".rmeta")
    flags = [
        flag
        for flag in target_flags
        if flag not in skip_flags
    ]
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
        mapped_files = [
            raw_object,
            metadata,
            source,
            config.rustc_cfg,
        ] + dep_inputs,
        mapped_directories = {
            rust_dir: rust_dir_anchor,
        },
        objtree_anchor = objtree_anchor,
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
    path_mapped_run(
        ctx.actions,
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
        common_flags,
        name,
        source,
        source_inputs,
        uses_rustc_cfg,
        crate_flags = []):
    extension = rust_toolchain.dylib_ext
    output = ctx.actions.declare_file(
        ctx.label.name + ".rust_sdk/rust/lib" + name + extension,
    )
    args = list(common_flags)
    args.extend(crate_flags)
    args.append("--sysroot=" + rust_toolchain.sysroot)
    host_link = _host_rust_link(host_cc, host_features)
    args.extend(host_link.flags)
    args.extend([
        "--emit=link=" + output.path,
        "--extern",
        "proc_macro",
        "--crate-type",
        "proc-macro",
        "--crate-name",
        name,
    ])
    inputs = list(source_inputs)
    if uses_rustc_cfg:
        args.append("@" + config.rustc_cfg.path)
        inputs.append(config.rustc_cfg)
    args.append(source.path)
    _run_rustc(
        ctx,
        rust_toolchain,
        args,
        inputs,
        [output],
        "LinuxRustProcMacro",
        "Compiling Rust-for-Linux procedural macro %s %%{label}" % name,
        mapped_files = [output, source, config.rustc_cfg] + host_link.runtime_files.to_list(),
        mapped_directories = {
            rust_toolchain.sysroot: directory_anchor(
                rust_toolchain.sysroot_anchor,
                rust_toolchain.sysroot,
            ),
        },
        transitive_tool_inputs = [host_cc.all_files, host_link.runtime_files],
    )
    return output

def _disabled_sdk():
    return LinuxRustSdkInfo(
        compile_inputs = depset(),
        enabled = False,
        module_flags = [],
        objtool = None,
        objtree = "",
        objtree_anchor = None,
        rust_dir = "",
        rust_dir_anchor = None,
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
    profile = _decode_rust_profile(ctx.attr.profile_json, ctx.attr.arch)
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
    host_cc = host_cc_toolchain(ctx)
    host_features = cc_common.configure_features(
        ctx = ctx,
        cc_toolchain = host_cc,
        requested_features = [],
        unsupported_features = [],
    )
    bindgen = ctx.toolchains[_BINDGEN_TOOLCHAIN_TYPE].bindgen
    target_spec = _target_spec(
        ctx,
        profile,
        rust_toolchain,
        host_cc,
        host_features,
        config,
        source_files,
    )
    objtree = _relative_output_root(target_spec, profile["target"]["output"])
    objtree_anchor = directory_anchor(target_spec, objtree)
    rust_dir = objtree + "/rust"
    module = profile["module"]
    allowed_features = _profile_string_list(
        module,
        "allowed_features",
    )
    for feature in allowed_features:
        _validate_profile_name(feature, "module.allowed_features")
    replacements = {
        "allowed_features_csv": ",".join(allowed_features),
        "rust_dir": rust_dir,
        "rustc_cfg": config.rustc_cfg.path,
        "target_spec": target_spec.path,
    }
    common_flags = _expand_profile_values(
        profile["common_flags"],
        config,
        replacements,
    )
    target_flags = _profile_target_flags(profile, config, replacements)
    c_flags = _kernel_c_flags(
        ctx,
        config,
        generated_headers,
        cc_toolchain,
        feature_configuration,
        source_root,
    )
    bindgen_c_flags = _extend_kernel_c_flags(c_flags, [
        "-fno-builtin",
        "-D__BINDGEN__",
        "-DMODULE",
    ])

    bindgen_profile = profile["bindgen"]
    bindgen_parameters = _source_file(
        source_files,
        _validate_profile_path(
            _required_profile_field(bindgen_profile, "parameters", "string"),
            "bindgen.parameters",
        ),
    )
    bindgen_common_flags = _expand_profile_values(
        _required_profile_field(bindgen_profile, "common_flags", "list"),
        config,
        replacements,
    )
    bindings_generated = ctx.actions.declare_file(
        ctx.label.name + ".rust_sdk/rust/bindings/bindings_generated.rs",
    )
    rust_dir_anchor = directory_anchor(bindings_generated, rust_dir)
    _run_bindgen_with_parameters(
        ctx,
        bindgen,
        cc_toolchain,
        config,
        generated_headers,
        source_tree,
        _source_file(
            source_files,
            _validate_profile_path(
                _required_profile_field(bindgen_profile, "bindings_header", "string"),
                "bindgen.bindings_header",
            ),
        ),
        bindgen_parameters,
        bindgen_common_flags,
        _extend_kernel_c_flags(bindgen_c_flags, linux_module_cc_helpers.object_name_flags(
            "rust/bindings/bindings_generated.o",
        )),
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
        _source_file(
            source_files,
            _validate_profile_path(
                _required_profile_field(bindgen_profile, "uapi_header", "string"),
                "bindgen.uapi_header",
            ),
        ),
        bindgen_parameters,
        bindgen_common_flags,
        _extend_kernel_c_flags(bindgen_c_flags, linux_module_cc_helpers.object_name_flags(
            "rust/uapi/uapi_generated.o",
        )),
        uapi_generated,
    )
    helpers_source_path = _validate_profile_path(
        _required_profile_field(bindgen_profile, "helpers_source", "string"),
        "bindgen.helpers_source",
    )
    helpers_source = _source_file(source_files, helpers_source_path)
    bindings_helpers_generated = _run_helpers_bindgen(
        ctx,
        bindgen,
        cc_toolchain,
        config,
        generated_headers,
        source_tree,
        helpers_source,
        bindgen_common_flags,
        _extend_kernel_c_flags(bindgen_c_flags, linux_module_cc_helpers.object_name_flags(
            "rust/bindings/bindings_helpers_generated.o",
        )),
        rust_dir_anchor,
    )

    generated_inputs = {
        "bindings_generated": bindings_generated,
        "bindings_helpers_generated": bindings_helpers_generated,
        "uapi_generated": uapi_generated,
    }
    conditional_generated_inputs = {}
    generated_arch_sources = []
    for recipe in profile.get("generated_assembly", []):
        if type(recipe) != "dict":
            fail("Rust profile generated_assembly entries must be objects")
        output_path = _validate_profile_path(
            _required_profile_field(recipe, "output", "string"),
            "generated_assembly.output",
        )
        conditional_generated_inputs[output_path] = True
        if not _condition_matches(config, recipe):
            continue
        source_path = _validate_profile_path(
            _required_profile_field(recipe, "source", "string"),
            "generated_assembly.source",
        )
        generated = _preprocess_rust_asm(
            ctx,
            config,
            generated_headers,
            source_tree,
            cc_toolchain,
            compiler,
            c_flags,
            _source_file(source_files, source_path),
            output_path,
        )
        generated_inputs[output_path] = generated
        generated_arch_sources.append(generated)

    proc_macros = {}
    for recipe in profile["proc_macros"]:
        if type(recipe) != "dict":
            fail("Rust profile proc_macros entries must be objects")
        name = _validate_profile_name(
            _required_profile_field(recipe, "name", "string"),
            "proc_macros.name",
        )
        if name in proc_macros:
            fail("Rust profile contains duplicate proc macro %r" % name)
        source_path = _validate_profile_path(
            _required_profile_field(recipe, "source", "string"),
            "proc_macros.source",
        )
        source_inputs = _profile_inputs(
            source_files,
            _profile_string_list(recipe, "source_prefixes"),
            _profile_string_list(recipe, "source_files") + [source_path],
        )
        proc_macros[name] = _proc_macro(
            ctx,
            rust_toolchain,
            host_cc,
            host_features,
            config,
            common_flags,
            name,
            _source_file(source_files, source_path),
            source_inputs,
            _required_profile_field(recipe, "uses_rustc_cfg", "bool"),
            crate_flags = _expand_profile_values(
                _required_profile_field(recipe, "flags", "list"),
                config,
                replacements,
            ),
        )

    crates = {}
    crate_order = []
    rustc_srcs = ctx.files._rustc_srcs
    for recipe in profile["crates"]:
        if type(recipe) != "dict":
            fail("Rust profile crates entries must be objects")
        name = _validate_profile_name(
            _required_profile_field(recipe, "name", "string"),
            "crates.name",
        )
        if name in crates or name in proc_macros:
            fail("Rust profile contains duplicate crate %r" % name)
        source_path = _required_profile_field(recipe, "source", "string")
        source_inputs = []
        if source_path.startswith("rustc://"):
            source = _rustc_file(rustc_srcs, source_path)
            rustc_inputs = {source.path: source}
            for prefix in _rustc_source_prefixes(
                _profile_string_list(recipe, "source_prefixes"),
            ):
                for file in _rustc_subtree(rustc_srcs, prefix):
                    rustc_inputs[file.path] = file
            for path in _profile_string_list(recipe, "source_files"):
                file = _rustc_file(rustc_srcs, path)
                rustc_inputs[file.path] = file
            source_inputs = [
                rustc_inputs[path]
                for path in sorted(rustc_inputs.keys())
            ]
        else:
            source_path = _validate_profile_path(source_path, "crates.source")
            source = _source_file(source_files, source_path)
            source_inputs = _profile_inputs(
                source_files,
                _profile_string_list(recipe, "source_prefixes"),
                _profile_string_list(recipe, "source_files") + [source_path],
            )
        for generated_name in _profile_string_list(recipe, "generated_inputs"):
            generated = generated_inputs.get(generated_name)
            if generated == None and generated_name in conditional_generated_inputs:
                continue
            if generated == None:
                fail(
                    "Rust profile crate %s references unavailable generated input %s" %
                    (name, generated_name),
                )
            source_inputs.append(generated)

        dep_inputs = [target_spec]
        for dep in _profile_string_list(recipe, "deps"):
            if dep in crates:
                dep_inputs.append(crates[dep].metadata)
            elif dep in proc_macros:
                dep_inputs.append(proc_macros[dep])
            else:
                fail(
                    "Rust profile crate %s depends on unavailable crate %s; " +
                    "crates must be topologically ordered" %
                    (name, dep),
                )
        crate_flags = _expand_profile_values(
            _required_profile_field(recipe, "flags", "list"),
            config,
            replacements,
        )
        for extern in _profile_string_list(recipe, "externs"):
            if extern not in crates and extern not in proc_macros:
                fail("Rust profile crate %s exposes unavailable extern %s" % (name, extern))
            crate_flags.extend(["--extern", extern])
        crates[name] = _rust_crate(
            ctx,
            rust_toolchain,
            config,
            objtree_anchor,
            rust_dir,
            rust_dir_anchor,
            target_flags,
            name,
            source,
            source_inputs,
            dep_inputs,
            crate_flags = crate_flags,
            skip_flags = _expand_profile_values(
                _required_profile_field(recipe, "skip_flags", "list"),
                config,
                replacements,
            ),
            objcopy_flags = _expand_profile_values(
                _required_profile_field(recipe, "objcopy_flags", "list"),
                config,
                replacements,
            ),
        )
        crate_order.append(name)

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
    export_profile = profile["exports"]
    export_headers = []
    for name in _profile_string_list(export_profile, "crates"):
        if name == "helpers":
            export_headers.append(_export_header(ctx, helpers, name))
        elif name in crates:
            export_headers.append(_export_header(ctx, crates[name].object, name))
        else:
            fail("Rust profile exports unavailable crate %r" % name)
    exports_source_path = _validate_profile_path(
        _required_profile_field(export_profile, "source", "string"),
        "exports.source",
    )
    exports = _compile_kernel_c(
        ctx,
        config,
        generated_headers,
        source_tree,
        cc_toolchain,
        compiler,
        c_flags,
        _source_file(source_files, exports_source_path),
        "rust/exports.o",
        extra_directory_flags = [
            struct(
                anchor = rust_dir_anchor,
                format = "-I%s",
            ),
        ],
        direct_inputs = export_headers,
    )

    objects_by_path = {
        "rust/helpers/helpers.o": helpers,
        "rust/exports.o": exports,
    }
    for name in crate_order:
        objects_by_path["rust/" + name + ".o"] = crates[name].object
    runtime_objects = []
    for recipe in profile["runtime_objects"]:
        if type(recipe) != "dict":
            fail("Rust profile runtime_objects entries must be objects")
        if recipe.get("config") and not _condition_matches(config, recipe):
            continue
        path = _validate_profile_path(
            _required_profile_field(recipe, "path", "string"),
            "runtime_objects.path",
        )
        object = objects_by_path.get(path)
        if object == None:
            fail("Rust profile references unavailable runtime object %r" % path)
        runtime_objects.append(object)

    metadata = [
        target_spec,
        config.rustc_cfg,
        bindings_generated,
        bindings_helpers_generated,
        uapi_generated,
    ] + generated_arch_sources
    metadata.extend([crates[name].metadata for name in crate_order])
    metadata.extend([proc_macros[name] for name in sorted(proc_macros.keys())])
    compile_inputs = depset(
        metadata,
        transitive = [rust_toolchain.all_files],
    )
    module_flags = target_flags + _expand_profile_values(
        _required_profile_field(module, "flags", "list"),
        config,
        replacements,
    )
    sdk = LinuxRustSdkInfo(
        compile_inputs = compile_inputs,
        enabled = True,
        module_flags = module_flags,
        objtool = ctx.executable.objtool,
        objtree = objtree,
        objtree_anchor = objtree_anchor,
        rust_dir = rust_dir,
        rust_dir_anchor = rust_dir_anchor,
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
        "profile_json": attr.string(mandatory = True),
        "source_root": attr.label(
            allow_single_file = True,
            mandatory = True,
        ),
        "source_tree": attr.label_list(allow_files = True),
        "srcarch": attr.string(mandatory = True),
        "_lineargsrun": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/lineargsrun"),
            executable = True,
        ),
        "_host_cc_toolchain": host_cc_toolchain_attr(),
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
        "_runandwrite": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/runandwrite"),
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
    fragments = ["cpp"],
    toolchains = [
        _RUST_TOOLCHAIN_TYPE,
        _BINDGEN_TOOLCHAIN_TYPE,
    ] + use_cc_toolchain(),
    doc = "Builds the private configuration-specific Rust-for-Linux SDK.",
)

linux_rust_test_helpers = struct(
    decode_profile = _decode_rust_profile,
    extend_kernel_c_flags = _extend_kernel_c_flags,
    expand_profile_value = _expand_profile_value,
    profile_target_flags = _profile_target_flags,
    rustc_source_prefixes = _rustc_source_prefixes,
    unsupported_config_symbols = _unsupported_rust_config_symbols,
)
