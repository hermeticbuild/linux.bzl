"""Concrete compact-v6 action groups for Linux objects."""

load(
    "@rules_cc//cc:action_names.bzl",
    "CPP_LINK_EXECUTABLE_ACTION_NAME",
    "CPP_LINK_STATIC_LIBRARY_ACTION_NAME",
    "C_COMPILE_ACTION_NAME",
)
load("@rules_cc//cc:find_cc_toolchain.bzl", "find_cpp_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load(":architecture_profiles.bzl", "linux_arch_values", "linux_architecture_profile_for_arch")
load(
    ":linux_objects.bzl",
    "LinuxCompileEnvironmentIndexInfo",
    "LinuxImageInfo",
    "LinuxObjectInfo",
    "LinuxSourceInputIndexInfo",
    "linux_module_cc_helpers",
)
load(
    ":path_mapping.bzl",
    "add_mapped_values",
    "directory_anchor",
    "path_mapped_run",
)

visibility("public")

LinuxObjectActionGroupInfo = provider(
    doc = "Concrete Linux objects owned by one recipe/reachability group.",
    fields = {
        "mode": "Shared Kbuild mode.",
        "object_targets": "Canonical compact object target names.",
        "objects": "Dictionary from compact target name to LinuxObjectInfo.",
        "reachable_configs": "Sorted config names that can reach this group.",
        "reachability_id": "Content identity of reachable_configs.",
        "recipe_id": "Content identity of the concrete action recipe.",
    },
)

def _validate_content_id(value, what):
    if len(value) != 64:
        fail("%s must be a full lowercase SHA-256 digest, got %r" % (what, value))
    for index in range(len(value)):
        char = value[index]
        if char not in "0123456789abcdef":
            fail("%s must be a full lowercase SHA-256 digest, got %r" % (what, value))

def _validate_canonical_strings(values, what):
    if not values:
        fail("%s must not be empty" % what)
    previous = ""
    for value in values:
        if type(value) != "string" or not value:
            fail("%s entries must be non-empty strings" % what)
        if previous and previous >= value:
            fail("%s must be sorted with no duplicates" % what)
        previous = value

def _decode_compile_spec(target, encoded):
    raw = json.decode(encoded)
    required = [
        "compile_environment",
        "content_id",
        "object",
        "source_input_file",
        "source_input_group",
    ]
    if type(raw) != "dict" or sorted(raw.keys()) != required:
        fail("grouped object %s must contain exactly %s" % (target, ", ".join(required)))
    for key in ["compile_environment", "content_id", "object"]:
        if type(raw[key]) != "string" or not raw[key]:
            fail("grouped object %s field %s must be a non-empty string" % (target, key))
    for key in ["source_input_file", "source_input_group"]:
        if type(raw[key]) != "int" or raw[key] <= 0:
            fail("grouped object %s field %s must be a positive integer" % (target, key))
    _validate_content_id(raw["compile_environment"], "grouped object %s compile environment" % target)
    _validate_content_id(raw["content_id"], "grouped object %s content ID" % target)
    if not raw["object"].endswith(".o"):
        fail("grouped object %s path must end in .o" % target)
    return struct(
        compile_environment = raw["compile_environment"],
        content_id = raw["content_id"],
        object = raw["object"],
        source_input_file = raw["source_input_file"],
        source_input_group = raw["source_input_group"],
    )

def _decode_composite_spec(target, encoded):
    raw = json.decode(encoded)
    required = ["content_id", "members", "object"]
    if type(raw) != "dict" or sorted(raw.keys()) != required:
        fail("grouped composite %s must contain exactly %s" % (target, ", ".join(required)))
    if type(raw["content_id"]) != "string" or type(raw["object"]) != "string":
        fail("grouped composite %s has non-string identity fields" % target)
    if type(raw["members"]) != "list" or not raw["members"]:
        fail("grouped composite %s requires members" % target)
    _validate_content_id(raw["content_id"], "grouped composite %s content ID" % target)
    if not raw["object"].endswith(".o"):
        fail("grouped composite %s path must end in .o" % target)
    seen = {}
    for member in raw["members"]:
        if type(member) != "string" or not member or member in seen:
            fail("grouped composite %s has invalid or duplicate member %r" % (target, member))
        seen[member] = True
    return struct(
        content_id = raw["content_id"],
        members = raw["members"],
        object = raw["object"],
    )

def _feature_configuration(ctx, cc_toolchain):
    return cc_common.configure_features(
        ctx = ctx,
        cc_toolchain = cc_toolchain,
        requested_features = ctx.features,
        unsupported_features = ctx.disabled_features,
    )

def _target_triple(arch):
    return linux_architecture_profile_for_arch(arch).target_triple

def _version_at_least(version, major, minor):
    parts = version.split(".")
    if len(parts) < 2:
        fail("invalid Linux version %r" % version)
    return (int(parts[0]), int(parts[1])) >= (major, minor)

def _rewrite_target_flags(flags, target_triple):
    if not target_triple:
        return flags
    out = []
    inserted = False
    skip_next = False
    for index in range(len(flags)):
        if skip_next:
            skip_next = False
            continue
        flag = flags[index]
        if flag in ["-target", "--target"]:
            skip_next = index + 1 < len(flags)
            if not inserted:
                out.append("--target=%s" % target_triple)
                inserted = True
            continue
        if flag.startswith("-target=") or flag.startswith("--target="):
            if not inserted:
                out.append("--target=%s" % target_triple)
                inserted = True
            continue
        out.append(flag)
    if not inserted:
        out.insert(0, "--target=%s" % target_triple)
    return out

def _drop_toolchain_include(path):
    return (
        "llvm++glibc+" in path or
        "llvm++musl+musl_libc/" in path or
        "llvm++musl+musl_libc\\" in path or
        "llvm++kernel_headers+linux_kernel_headers_" in path
    )

def _clang_resource_include(path):
    """Whether path is Clang's compiler-provided resource header directory."""
    normalized = path.replace("\\", "/")
    marker = "/lib/clang/"
    marker_index = normalized.rfind(marker)
    if marker_index < 0:
        return False
    suffix = normalized[marker_index + len(marker):].split("/")
    return len(suffix) == 2 and bool(suffix[0]) and suffix[1] == "include"

def _compile_flags(ctx, cc_toolchain, feature_configuration):
    variables = cc_common.create_compile_variables(
        feature_configuration = feature_configuration,
        cc_toolchain = cc_toolchain,
        user_compile_flags = ctx.fragments.cpp.copts + ctx.fragments.cpp.conlyopts,
    )
    flags = cc_common.get_memory_inefficient_command_line(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
        variables = variables,
    )
    flags = _rewrite_target_flags(flags, _target_triple(ctx.attr.arch))
    out = []
    skip_next = False
    drop_count = 0
    for index in range(len(flags)):
        if skip_next:
            skip_next = False
            continue
        if drop_count:
            drop_count -= 1
            continue
        flag = flags[index]
        if flag == "-Xclang":
            if index + 1 < len(flags) and flags[index + 1] == "-internal-isystem":
                resource_path = ""
                if index + 3 < len(flags) and flags[index + 2] == "-Xclang":
                    resource_path = flags[index + 3]
                if not _clang_resource_include(resource_path):
                    drop_count = 3
                    continue
            if index + 1 < len(flags) and flags[index + 1] == "-fno-cxx-modules":
                drop_count = 1
                continue
        if flag in ["-I", "-iquote", "-isystem"]:
            if index + 1 < len(flags) and _drop_toolchain_include(flags[index + 1]):
                skip_next = True
                continue
        if flag in [
            "-fcolor-diagnostics",
            "-fstack-protector",
            "-no-canonical-prefixes",
            "-nostdlibinc",
            "-Werror=incomplete-umbrella",
            "-Wall",
            "-Wno-free-nonheap-object",
            "-Wno-module-import-in-extern-c",
            "-Wno-modules-import-nested-redundant",
            "-Wself-assign",
            "-Wthread-safety",
            "-Wunused-but-set-parameter",
        ]:
            continue
        if flag == "--sysroot":
            skip_next = index + 1 < len(flags)
            continue
        if flag.startswith("--sysroot="):
            continue
        if flag.startswith("-I") and _drop_toolchain_include(flag[len("-I"):]):
            continue
        if flag.startswith("-iquote") and _drop_toolchain_include(flag[len("-iquote"):]):
            continue
        if flag.startswith("-isystem") and _drop_toolchain_include(flag[len("-isystem"):]):
            continue
        out.append(flag)
    if "-nostdinc" not in out:
        out.append("-nostdinc")
    if "-fintegrated-as" not in out:
        out.append("-fintegrated-as")
    return out

def _target_flags(ctx, cc_toolchain, feature_configuration):
    flags = _compile_flags(ctx, cc_toolchain, feature_configuration)
    out = []
    for index in range(len(flags)):
        flag = flags[index]
        if flag in ["-target", "--target"]:
            if index + 1 < len(flags):
                out.extend([flag, flags[index + 1]])
        elif flag.startswith("-target=") or flag.startswith("--target="):
            out.append(flag)
    return out

def _execroot_path(file):
    path = file.short_path.replace("\\", "/")
    if path.startswith("../"):
        return "external/" + path[3:]
    return path

def _source_root_path(source_input_index):
    root = source_input_index.source_tree_info.root
    path = _execroot_path(root)
    return path.rsplit("/", 1)[0] if "/" in path else ""

def _source_group(index, group_number, file_number, target):
    if group_number <= 0 or group_number > len(index.groups):
        fail("grouped object %s source group %d is out of range" % (target, group_number))
    group = index.groups[group_number - 1]
    if file_number <= 0 or file_number > len(index.files):
        fail("grouped object %s source file %d is out of range" % (target, file_number))
    if (",%d," % file_number) not in group.encoded_membership:
        fail("grouped object %s source group omits primary source %d" % (target, file_number))
    return struct(
        files = group.files,
        path_files = group.path_files,
        source = index.files[file_number - 1],
        source_path = index.paths[file_number - 1],
    )

def _object_stem(object):
    base = object.rsplit("/", 1)[-1]
    if base.endswith(".o"):
        base = base[:-len(".o")]
    return base.replace("-", "_").replace(",", "_").replace(" ", "_")

def _object_name_flags(object, modname):
    stem = _object_stem(object)
    mod_object = modname if modname else object
    modstem = _object_stem(mod_object)
    modfile = mod_object[:-len(".o")] if mod_object.endswith(".o") else mod_object
    return [
        "-DKBUILD_BASENAME=\"%s\"" % stem,
        "-DKBUILD_MODNAME=\"%s\"" % modstem,
        "-D__KBUILD_MODNAME=kmod_%s" % modstem,
        "-DKBUILD_MODFILE=\"%s\"" % modfile,
    ]

def _known_empty_make_ref(ref):
    return ref.startswith("cflags-nogcse-") or ref in [
        "CC_FLAGS_CFI",
        "CC_FLAGS_FTRACE",
        "CC_FLAGS_LTO",
        "CC_FLAGS_SCS",
        "CLANG_FLAGS",
        "DISABLE_KSTACK_ERASE",
        "DISABLE_LATENT_ENTROPY_PLUGIN",
        "DISABLE_STACKLEAK_PLUGIN",
        "RANDSTRUCT_CFLAGS",
    ]

def _expand_make_refs(value, replacements, object):
    for key in sorted(replacements.keys()):
        value = value.replace("$(%s)" % key, replacements[key])
        value = value.replace("${%s}" % key, replacements[key])
    refs = []
    for index in range(len(value) - 1):
        opening = value[index:index + 2]
        if opening not in ["$(", "${"]:
            continue
        close = ")" if opening == "$(" else "}"
        end = value.find(close, index + 2)
        if end < 0:
            fail("unclosed Kbuild Make reference in flags for %s: %s" % (object, value))
        refs.append(value[index + 2:end])
    for ref in refs:
        if not _known_empty_make_ref(ref):
            fail("unexpanded Kbuild Make reference %s in flags for %s" % (ref, object))
        value = value.replace("$(%s)" % ref, "")
        value = value.replace("${%s}" % ref, "")
    return value

def _rewrite_source_root_flag(value, source_root):
    if not source_root:
        return value
    marker = "/" + source_root
    index = value.find(marker)
    if index < 0:
        return value
    if value.startswith("/"):
        return value[index + 1:]
    for prefix in ["-I", "-iquote", "-isystem", "-include"]:
        if value.startswith(prefix + "/"):
            return prefix + value[index + 1:]
    return value

def _generated_include_groups(include_dirs, srcarch):
    result = struct(
        arch = [],
        arch_uapi = [],
        generic = [],
        generic_uapi = [],
        other = [],
    )
    arch_generated = "/arch/%s/include/generated" % srcarch
    for include_dir in include_dirs:
        if include_dir.endswith(arch_generated + "/uapi"):
            result.arch_uapi.append(include_dir)
        elif include_dir.endswith(arch_generated):
            result.arch.append(include_dir)
        elif include_dir.endswith("/include/generated/uapi"):
            result.generic_uapi.append(include_dir)
        elif include_dir.endswith("/include"):
            result.generic.append(include_dir)
        else:
            result.other.append(include_dir)
    return result

def _source_include_dirs(source_root, srcarch, generated_headers):
    generated_dirs = generated_headers.include_dirs if generated_headers != None else []
    generated = _generated_include_groups(generated_dirs, srcarch)
    return (
        [source_root + "/arch/" + srcarch + "/include"] +
        generated.arch +
        [source_root + "/include"] +
        generated.generic +
        [source_root + "/arch/" + srcarch + "/include/uapi"] +
        generated.arch_uapi +
        [source_root + "/include/uapi"] +
        generated.generic_uapi +
        generated.other
    )

def _compile_arguments(ctx, base_flags, spec, source, output, config, generated_headers, source_root, depfile = None):
    replacements = dict(config.config_flags)
    replacements.update({
        "src": source.dirname,
        "srctree": source_root,
    })
    object_args = []
    config_response = config.aflags if ctx.attr.language == "asm" else config.cflags
    object_args.append("@" + config_response.path)
    mapping_files = [config_response]
    if ctx.attr.language != "asm" and generated_headers != None and generated_headers.cflags != None:
        object_args.append("@" + generated_headers.cflags.path)
        mapping_files.append(generated_headers.cflags)
    if ctx.attr.mode == "m":
        object_args.append("-DMODULE")
    object_args.extend(_object_name_flags(spec.object, ctx.attr.modname))
    if ctx.attr.language == "asm":
        object_args.extend([
            "-D__KERNEL__",
            "-D__ASSEMBLY__",
            "-include",
            source_root + "/include/linux/compiler-version.h",
            "-include",
            source_root + "/include/linux/kconfig.h",
        ])
    else:
        object_args.extend([
            "-D__KERNEL__",
            "-include",
            source_root + "/include/linux/compiler-version.h",
            "-include",
            source_root + "/include/linux/kconfig.h",
            "-include",
            source_root + "/include/linux/compiler_types.h",
        ])
    object_args.append("-I" + config.include_dir)
    object_args.append("-fmacro-prefix-map=%s/=" % source_root)
    object_args.append("-I" + source.dirname)
    object_args.extend([
        "-I" + include_dir
        for include_dir in _source_include_dirs(source_root, ctx.attr.srcarch, generated_headers)
    ])
    object_args.extend([
        _rewrite_source_root_flag(
            _expand_make_refs(flag, replacements, spec.object),
            source_root,
        )
        for flag in ctx.attr.flags
    ])

    source_root_file = ctx.attr.source_input_index[LinuxSourceInputIndexInfo].source_tree_info.root
    anchors = {
        source.dirname: directory_anchor(source),
        source_root: directory_anchor(source_root_file, source_root),
    }
    if config.include_dir_anchor != None:
        anchors[config.include_dir] = config.include_dir_anchor
    if generated_headers != None:
        anchors.update(generated_headers.include_dir_anchors)

    args = ctx.actions.args()
    args.add_all(base_flags)
    add_mapped_values(
        args,
        object_args,
        files = mapping_files,
        directory_anchors = anchors,
    )
    if depfile != None:
        args.add("-MD")
        args.add("-MF")
        args.add(depfile)
    args.add("-c")
    args.add(source)
    args.add("-o")
    args.add(output)
    return args

def _cmd_path(object):
    if "/" not in object:
        return "." + object + ".cmd"
    directory, basename = object.rsplit("/", 1)
    return directory + "/." + basename + ".cmd"

def _source_version_record(ctx, selection, spec, depfile, symversions = None):
    object_dir = spec.object.rsplit("/", 1)[0] if "/" in spec.object else ""
    staged_path_files = []
    for path_file in selection.path_files:
        path_dir = path_file.path.rsplit("/", 1)[0] if "/" in path_file.path else ""
        if path_file.path == selection.source_path or path_dir == object_dir:
            staged_path_files.append(path_file)

    out = ctx.actions.declare_file(
        ctx.label.name + ".source_versions/" + spec.content_id + "/" + _cmd_path(spec.object),
    )
    args = ctx.actions.args()
    args.add("-depfile", depfile)
    args.add("-object", spec.object)
    args.add("-out", out)
    args.add("-primary", selection.source_path)
    inputs = [depfile]
    if symversions != None:
        args.add("-symversions", symversions)
        inputs.append(symversions)
    for path_file in selection.path_files:
        args.add("-physical")
        args.add(path_file.file)
        args.add("-canonical", path_file.path)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._sourceversioncmd,
        inputs = depset(inputs, transitive = [selection.files]),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxSourceVersionCmd",
        progress_message = "Generating grouped Linux module source-version data %{label}",
    )
    return struct(
        cmd = out,
        object = spec.object,
        path_files = staged_path_files,
    )

def _symversion_config_response(ctx, config, spec, source, source_root):
    if not ctx.attr.symversion_remove_flags:
        return struct(file = config.cflags, inputs = [])
    out = ctx.actions.declare_file(
        ctx.label.name + ".objects/" + spec.content_id + "/bazel_kbuild_cflags.symversions.rsp",
    )
    args = ctx.actions.args()
    args.add("-in", config.cflags)
    args.add("-out", out)
    replacements = dict(config.config_flags)
    replacements.update({
        "src": source.dirname,
        "srctree": source_root,
    })
    args.add_all([
        _rewrite_source_root_flag(
            _expand_make_refs(flag, replacements, spec.object),
            source_root,
        )
        for flag in ctx.attr.symversion_remove_flags
    ], before_each = "-remove")
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._flagfilter,
        inputs = [config.cflags],
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxFlagFilter",
        progress_message = "Filtering grouped Linux symbol-version flags %{label}",
    )
    return struct(file = out, inputs = [out])

def _symversion_arguments(ctx, base_flags, spec, source, config, generated_headers, source_root, config_response):
    replacements = dict(config.config_flags)
    replacements.update({
        "src": source.dirname,
        "srctree": source_root,
    })
    object_args = ["@" + config_response.path]
    mapping_files = [config_response]
    if generated_headers != None and generated_headers.cflags != None:
        object_args.append("@" + generated_headers.cflags.path)
        mapping_files.append(generated_headers.cflags)
    if ctx.attr.mode == "m":
        object_args.append("-DMODULE")
    object_args.extend(_object_name_flags(spec.object, ctx.attr.modname))
    object_args.extend([
        "-D__KERNEL__",
        "-include",
        source_root + "/include/linux/compiler-version.h",
        "-include",
        source_root + "/include/linux/kconfig.h",
        "-include",
        source_root + "/include/linux/compiler_types.h",
        "-I" + config.include_dir,
        "-fmacro-prefix-map=%s/=" % source_root,
        "-I" + source.dirname,
    ])
    object_args.extend([
        "-I" + include_dir
        for include_dir in _source_include_dirs(source_root, ctx.attr.srcarch, generated_headers)
    ])
    object_args.extend([
        _rewrite_source_root_flag(
            _expand_make_refs(flag, replacements, spec.object),
            source_root,
        )
        for flag in ctx.attr.symversion_flags
    ])
    object_args.extend(["-E", "-D__GENKSYMS__"])
    if ctx.attr.language == "asm":
        object_args.extend(["-xc", "-"])

    source_root_file = ctx.attr.source_input_index[LinuxSourceInputIndexInfo].source_tree_info.root
    anchors = {
        source.dirname: directory_anchor(source),
        source_root: directory_anchor(source_root_file, source_root),
    }
    if config.include_dir_anchor != None:
        anchors[config.include_dir] = config.include_dir_anchor
    if generated_headers != None:
        anchors.update(generated_headers.include_dir_anchors)

    args = ctx.actions.args()
    args.add_all(base_flags)
    add_mapped_values(
        args,
        object_args,
        files = mapping_files,
        directory_anchors = anchors,
    )
    if ctx.attr.language == "c":
        args.add(source)
    return args

def _grouped_objtool(ctx, config, input_file, output):
    args = ctx.actions.args()
    args.add("-config", config.config)
    if ctx.attr.objtool_force:
        args.add("-force")
    for arg in ctx.attr.objtool_args:
        args.add("-objtool_arg=%s" % arg)
    args.add("-objtool", ctx.executable.objtool)
    args.add("-in", input_file)
    mode = "builtin"
    if ctx.attr.mode == "m":
        mode = "module-single" if ctx.attr.module_root else "module-member"
    args.add("-mode", mode)
    args.add("-out", output)
    path_mapped_run(
        ctx.actions,
        executable = ctx.attr._objtoolrun[DefaultInfo].files_to_run,
        inputs = [config.config, input_file],
        tools = [ctx.attr.objtool[DefaultInfo].files_to_run],
        outputs = [output],
        arguments = [args],
        mnemonic = "LinuxObjectObjtool",
        progress_message = "Processing grouped Linux object with objtool %{label}",
    )

def _group_providers(ctx, objects, outputs):
    symversion_files = []
    source_version_files = []
    for target in sorted(objects.keys()):
        info = objects[target]
        if hasattr(info, "symversion_records") and info.symversion_records:
            symversion_files.extend([record.cmd for record in info.symversion_records])
        if hasattr(info, "source_version_records") and info.source_version_records:
            source_version_files.extend([record.cmd for record in info.source_version_records])
    return [
        DefaultInfo(files = depset(outputs)),
        LinuxObjectActionGroupInfo(
            mode = ctx.attr.mode,
            object_targets = sorted(objects.keys()),
            objects = objects,
            reachable_configs = list(ctx.attr.reachable_configs),
            reachability_id = ctx.attr.reachability_id,
            recipe_id = ctx.attr.recipe_id,
        ),
        OutputGroupInfo(
            object = depset(outputs),
            source_versions = depset(source_version_files),
            symversions = depset(symversion_files),
        ),
    ]

def _linux_object_action_group_impl(ctx):
    _validate_content_id(ctx.attr.recipe_id, "recipe_id")
    _validate_content_id(ctx.attr.reachability_id, "reachability_id")
    _validate_canonical_strings(ctx.attr.reachable_configs, "reachable_configs")
    if ctx.attr.module_root and ctx.attr.mode != "m":
        fail("%s marks built-in objects as module roots" % ctx.label)
    if ctx.attr.srcarch != ctx.attr.arch:
        fail("%s srcarch %r does not match arch %r" % (ctx.label, ctx.attr.srcarch, ctx.attr.arch))
    if ctx.attr.remove_flags:
        fail("%s does not support grouped remove flags" % ctx.label)

    cc_toolchain = find_cpp_toolchain(ctx)
    if cc_toolchain.compiler.lower().find("clang") < 0:
        fail("%s requires Clang, got compiler %r" % (ctx.label, cc_toolchain.compiler))
    feature_configuration = _feature_configuration(ctx, cc_toolchain)
    compiler = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
    )
    base_flags = _compile_flags(ctx, cc_toolchain, feature_configuration)
    environment_index = ctx.attr.compile_environment_index[LinuxCompileEnvironmentIndexInfo]
    source_index = ctx.attr.source_input_index[LinuxSourceInputIndexInfo]
    source_root_file = source_index.source_tree_info.root
    source_root = _source_root_path(source_index)
    if not source_root:
        fail("%s requires a source root directory" % ctx.label)

    targets = sorted(ctx.attr.objects.keys())
    _validate_canonical_strings(targets, "grouped object targets")
    objects = {}
    outputs = []
    content_ids = {}
    use_objtool = ctx.executable.objtool != None and not ctx.attr.objtool_disabled
    if ctx.attr.symversions and not ctx.executable.genksyms:
        fail("%s requires genksyms when symversions are enabled" % ctx.label)
    for target in targets:
        spec = _decode_compile_spec(target, ctx.attr.objects[target])
        if spec.content_id in content_ids:
            fail("grouped objects %s and %s share content ID %s" % (content_ids[spec.content_id], target, spec.content_id))
        content_ids[spec.content_id] = target
        if spec.compile_environment not in environment_index.environments:
            fail("grouped object %s references unknown compile environment %s" % (target, spec.compile_environment))
        environment = environment_index.environments[spec.compile_environment]
        config = environment.config
        generated_headers = environment.generated_headers
        selection = _source_group(
            source_index,
            spec.source_input_group,
            spec.source_input_file,
            target,
        )
        source = selection.source
        if ctx.attr.language == "c" and not source.basename.endswith(".c"):
            fail("grouped C object %s has source %s" % (target, source.basename))
        if ctx.attr.language == "asm" and not (source.basename.endswith(".S") or source.basename.endswith(".s")):
            fail("grouped assembler object %s has source %s" % (target, source.basename))

        out = ctx.actions.declare_file(
            ctx.label.name + ".objects/" + spec.content_id + "/" + spec.object,
        )
        compile_out = out
        if use_objtool:
            compile_out = ctx.actions.declare_file(
                ctx.label.name + ".objects/" + spec.content_id + "/.objtool-input/" + spec.object,
            )
        source_version_depfile = None
        if ctx.attr.mode == "m":
            source_version_depfile = ctx.actions.declare_file(
                ctx.label.name + ".source_versions/" + spec.content_id + "/" + spec.object + ".d",
            )
        transitive_inputs = [
            selection.files,
            config.files,
            cc_toolchain.all_files,
        ]
        if generated_headers != None:
            transitive_inputs.append(generated_headers.files)
        compile_outputs = [compile_out]
        if source_version_depfile != None:
            compile_outputs.append(source_version_depfile)
        path_mapped_run(
            ctx.actions,
            executable = compiler,
            inputs = depset(
                [source, source_root_file],
                transitive = transitive_inputs,
            ),
            outputs = compile_outputs,
            arguments = [_compile_arguments(
                ctx,
                base_flags,
                spec,
                source,
                compile_out,
                config,
                generated_headers,
                source_root,
                depfile = source_version_depfile,
            )],
            mnemonic = "LinuxObjectCompile",
            progress_message = "Compiling grouped Linux object %s" % spec.object,
        )
        if use_objtool:
            _grouped_objtool(ctx, config, compile_out, out)
        symversion_records = []
        symversion_cmd = None
        if ctx.attr.symversions:
            if config.config_flags.get("CONFIG_MODVERSIONS") != "y":
                fail("%s enables symversions without CONFIG_MODVERSIONS=y" % ctx.label)
            if _version_at_least(ctx.attr.version, 6, 18) and config.config_flags.get("CONFIG_GENKSYMS") != "y":
                fail("%s requires CONFIG_GENKSYMS=y for symbol versions" % ctx.label)
            config_response = _symversion_config_response(
                ctx,
                config,
                spec,
                source,
                source_root,
            )
            symversion_cmd = ctx.actions.declare_file(
                ctx.label.name + ".symversions/" + spec.content_id + "/" + spec.object + ".cmd",
            )
            runner_args = ctx.actions.args()
            runner_args.add("-mode", ctx.attr.language)
            llvm_nm = linux_module_cc_helpers.llvm_nm(cc_toolchain)
            runner_args.add("-nm", llvm_nm)
            runner_args.add("-object", compile_out)
            runner_args.add("-compiler", compiler)
            runner_args.add("-genksyms", ctx.executable.genksyms)
            runner_args.add("-out", symversion_cmd)
            runner_args.add("-linux-version", ctx.attr.version)
            symversion_extra_inputs = []
            if not _version_at_least(ctx.attr.version, 6, 18):
                reference = ctx.actions.declare_file(
                    ctx.label.name + ".symversions/" + spec.content_id + "/" + spec.object + ".symref",
                )
                ctx.actions.write(reference, "")
                runner_args.add("-reference", reference)
                symversion_extra_inputs.append(reference)
            runner_args.add("--")
            path_mapped_run(
                ctx.actions,
                executable = ctx.executable._genksymsrun,
                inputs = depset(
                    [compile_out, source, source_root_file] + config_response.inputs + symversion_extra_inputs,
                    transitive = transitive_inputs,
                ),
                tools = [
                    llvm_nm,
                    ctx.attr.genksyms[DefaultInfo].files_to_run,
                ],
                outputs = [symversion_cmd],
                arguments = [
                    runner_args,
                    _symversion_arguments(
                        ctx,
                        base_flags,
                        spec,
                        source,
                        config,
                        generated_headers,
                        source_root,
                        config_response.file,
                    ),
                ],
                mnemonic = "LinuxGenksyms",
                progress_message = "Generating grouped Linux symbol versions %s" % spec.object,
            )
            symversion_records.append(struct(
                cmd = symversion_cmd,
                object = spec.object,
            ))
        source_version_records = []
        if source_version_depfile != None:
            source_version_records.append(_source_version_record(
                ctx,
                selection,
                spec,
                source_version_depfile,
                symversions = symversion_cmd,
            ))
        info = LinuxObjectInfo(
            content_id = spec.content_id,
            generated_headers = depset([]),
            generated_include_dir_anchors = {},
            generated_include_dirs = [],
            mode = ctx.attr.mode,
            module_root_kind = "single" if ctx.attr.module_root else "",
            object = spec.object,
            objtool_args = list(ctx.attr.objtool_args) if use_objtool else [],
            objtool_force = ctx.attr.objtool_force if use_objtool else False,
            output = out,
            source_version_records = source_version_records,
            symversion_records = symversion_records,
        )
        objects[target] = info
        outputs.append(out)
    return _group_providers(ctx, objects, outputs)

linux_object_action_group = rule(
    implementation = _linux_object_action_group_impl,
    attrs = {
        "arch": attr.string(mandatory = True, values = linux_arch_values()),
        "compile_environment_index": attr.label(
            mandatory = True,
            providers = [LinuxCompileEnvironmentIndexInfo],
        ),
        "flags": attr.string_list(),
        "genksyms": attr.label(cfg = "exec", executable = True),
        "language": attr.string(mandatory = True, values = ["asm", "c"]),
        "mode": attr.string(mandatory = True, values = ["m", "y"]),
        "modname": attr.string(),
        "module_root": attr.bool(),
        "objects": attr.string_dict(mandatory = True),
        "objtool": attr.label(cfg = "exec", executable = True),
        "objtool_args": attr.string_list(),
        "objtool_disabled": attr.bool(),
        "objtool_force": attr.bool(),
        "reachable_configs": attr.string_list(mandatory = True),
        "reachability_id": attr.string(mandatory = True),
        "recipe_id": attr.string(mandatory = True),
        "remove_flags": attr.string_list(),
        "symversion_flags": attr.string_list(),
        "symversion_remove_flags": attr.string_list(),
        "symversions": attr.bool(),
        "source_input_index": attr.label(
            mandatory = True,
            providers = [LinuxSourceInputIndexInfo],
        ),
        "srcarch": attr.string(mandatory = True),
        "version": attr.string(default = "6.18.0"),
        "_flagfilter": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/flagfilter"),
            executable = True,
        ),
        "_genksymsrun": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/genksymsrun"),
            executable = True,
        ),
        "_objtoolrun": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/objtoolrun"),
            executable = True,
        ),
        "_sourceversioncmd": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/sourceversioncmd"),
            executable = True,
        ),
    },
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Owns simple concrete compile actions with one recipe and reachability.",
)

def _linux_object_action_group_import_impl(ctx):
    _validate_content_id(ctx.attr.recipe_id, "recipe_id")
    _validate_content_id(ctx.attr.reachability_id, "reachability_id")
    _validate_canonical_strings(ctx.attr.reachable_configs, "reachable_configs")
    _validate_canonical_strings(ctx.attr.object_targets, "object_targets")
    if len(ctx.attr.object_targets) != len(ctx.attr.objects):
        fail("%s has mismatched object_targets and objects" % ctx.label)
    objects = {}
    outputs = []
    for index in range(len(ctx.attr.object_targets)):
        target = ctx.attr.object_targets[index]
        info = ctx.attr.objects[index][LinuxObjectInfo]
        if info.mode != ctx.attr.mode:
            fail("imported object %s has mode %r, want %r" % (target, info.mode, ctx.attr.mode))
        objects[target] = info
        outputs.append(info.output)
    return _group_providers(ctx, objects, outputs)

linux_object_action_group_import = rule(
    implementation = _linux_object_action_group_import_impl,
    attrs = {
        "mode": attr.string(mandatory = True, values = ["m", "y"]),
        "object_targets": attr.string_list(mandatory = True),
        "objects": attr.label_list(mandatory = True, providers = [LinuxObjectInfo]),
        "reachable_configs": attr.string_list(mandatory = True),
        "reachability_id": attr.string(mandatory = True),
        "recipe_id": attr.string(mandatory = True),
    },
    doc = "Imports an exceptional legacy object island into the grouped graph.",
)

def _dependency_objects(ctx):
    available = {}
    for target in ctx.attr.member_groups:
        group = target[LinuxObjectActionGroupInfo]
        for config in ctx.attr.reachable_configs:
            if config not in group.reachable_configs:
                fail("%s member group %s is not reachable in config %s" % (ctx.label, target.label, config))
        for object_target, info in group.objects.items():
            if object_target in available:
                fail("%s receives object %s from multiple member groups" % (ctx.label, object_target))
            if info.mode != ctx.attr.mode:
                fail("%s member %s has mode %r, want %r" % (ctx.label, object_target, info.mode, ctx.attr.mode))
            available[object_target] = info
    return available

def _merged_generated_include_dir_anchors(infos):
    anchors = {}
    for info in infos:
        anchors.update(info.generated_include_dir_anchors)
    return anchors

def _merged_symversion_records(infos):
    records = []
    for info in infos:
        if hasattr(info, "symversion_records") and info.symversion_records:
            records.extend(info.symversion_records)
    return records

def _merged_source_version_records(infos):
    records = []
    for info in infos:
        if hasattr(info, "source_version_records") and info.source_version_records:
            records.extend(info.source_version_records)
    return records

def _unique_generated_include_dirs(infos):
    seen = {}
    out = []
    for info in infos:
        for include_dir in info.generated_include_dirs:
            if include_dir not in seen:
                seen[include_dir] = True
                out.append(include_dir)
    return out

def _linux_composite_object_action_group_impl(ctx):
    _validate_content_id(ctx.attr.recipe_id, "recipe_id")
    _validate_content_id(ctx.attr.reachability_id, "reachability_id")
    _validate_canonical_strings(ctx.attr.reachable_configs, "reachable_configs")
    if ctx.attr.module_root and ctx.attr.mode != "m":
        fail("%s marks built-in composites as module roots" % ctx.label)
    cc_toolchain = find_cpp_toolchain(ctx)
    if cc_toolchain.compiler.lower().find("clang") < 0:
        fail("%s requires Clang, got compiler %r" % (ctx.label, cc_toolchain.compiler))
    feature_configuration = _feature_configuration(ctx, cc_toolchain)
    linker = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = CPP_LINK_EXECUTABLE_ACTION_NAME,
    )
    available = _dependency_objects(ctx)
    specs = {
        target: _decode_composite_spec(target, encoded)
        for target, encoded in ctx.attr.objects.items()
    }
    if not specs:
        fail("%s requires at least one composite object" % ctx.label)
    pending = dict(specs)
    objects = {}
    outputs = []
    for _ in range(len(specs)):
        progressed = False
        for target in sorted(pending.keys()):
            spec = pending[target]
            if any([member not in available for member in spec.members]):
                continue
            member_infos = [available[member] for member in spec.members]
            out = ctx.actions.declare_file(
                ctx.label.name + ".objects/" + spec.content_id + "/" + spec.object,
            )
            args = ctx.actions.args()
            args.add_all(_target_flags(ctx, cc_toolchain, feature_configuration))
            args.add_all(["-fuse-ld=lld", "-nostdlib", "-r", "-o"])
            args.add(out)
            args.add_all([info.output for info in member_infos])
            path_mapped_run(
                ctx.actions,
                executable = linker,
                inputs = depset(
                    [info.output for info in member_infos],
                    transitive = [cc_toolchain.all_files],
                ),
                outputs = [out],
                arguments = [args],
                mnemonic = "LinuxCompositeObject",
                progress_message = "Linking grouped Linux composite object %s" % spec.object,
            )
            info = LinuxObjectInfo(
                content_id = spec.content_id,
                generated_headers = depset(
                    transitive = [member.generated_headers for member in member_infos],
                ),
                generated_include_dir_anchors = _merged_generated_include_dir_anchors(member_infos),
                generated_include_dirs = _unique_generated_include_dirs(member_infos),
                mode = ctx.attr.mode,
                module_root_kind = "composite" if ctx.attr.module_root else "",
                object = spec.object,
                objtool_args = list(ctx.attr.objtool_args),
                objtool_force = ctx.attr.objtool_force,
                output = out,
                source_version_records = _merged_source_version_records(member_infos),
                symversion_records = _merged_symversion_records(member_infos),
            )
            available[target] = info
            objects[target] = info
            outputs.append(out)
            pending.pop(target)
            progressed = True
        if not pending or not progressed:
            break
    if pending:
        unresolved = [
            "%s -> %s" % (
                target,
                ", ".join([member for member in pending[target].members if member not in available]),
            )
            for target in sorted(pending.keys())
        ]
        fail("%s has unresolved or cyclic composite members: %s" % (ctx.label, "; ".join(unresolved)))
    return _group_providers(ctx, objects, outputs)

linux_composite_object_action_group = rule(
    implementation = _linux_composite_object_action_group_impl,
    attrs = {
        "arch": attr.string(mandatory = True, values = linux_arch_values()),
        "member_groups": attr.label_list(
            mandatory = True,
            providers = [LinuxObjectActionGroupInfo],
        ),
        "mode": attr.string(mandatory = True, values = ["m", "y"]),
        "module_root": attr.bool(),
        "objects": attr.string_dict(mandatory = True),
        "objtool_args": attr.string_list(),
        "objtool_force": attr.bool(),
        "reachable_configs": attr.string_list(mandatory = True),
        "reachability_id": attr.string(mandatory = True),
        "recipe_id": attr.string(mandatory = True),
    },
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Owns concrete relocatable links for one composite recipe/reachability.",
)

def _linux_grouped_compact_image_impl(ctx):
    available = {}
    for target in ctx.attr.groups:
        group = target[LinuxObjectActionGroupInfo]
        if ctx.attr.config not in group.reachable_configs:
            fail("%s receives unreachable group %s for config %s" % (ctx.label, target.label, ctx.attr.config))
        for object_target, info in group.objects.items():
            if object_target in available:
                fail("%s receives object %s from multiple groups" % (ctx.label, object_target))
            available[object_target] = info
    object_infos = []
    for target in ctx.attr.object_targets:
        if target not in available:
            fail("%s cannot project missing built-in object %s" % (ctx.label, target))
        info = available[target]
        if info.mode != "y":
            fail("%s built-in object %s has mode %r" % (ctx.label, target, info.mode))
        object_infos.append(info)
    module_infos = []
    for target in ctx.attr.module_object_targets:
        if target not in available:
            fail("%s cannot project missing module object %s" % (ctx.label, target))
        info = available[target]
        if info.mode != "m":
            fail("%s module object %s has mode %r" % (ctx.label, target, info.mode))
        module_infos.append(info)
    if not object_infos:
        fail("%s requires at least one built-in object" % ctx.label)

    cc_toolchain = find_cpp_toolchain(ctx)
    feature_configuration = _feature_configuration(ctx, cc_toolchain)
    archiver = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = CPP_LINK_STATIC_LIBRARY_ACTION_NAME,
    )
    out = ctx.actions.declare_file(ctx.label.name + ".vmlinux.a")
    object_outputs = [info.output for info in object_infos]
    args = ctx.actions.args()
    args.add("cDPrST")
    args.add(out)
    args.add_all(object_outputs)
    path_mapped_run(
        ctx.actions,
        executable = archiver,
        inputs = depset(object_outputs, transitive = [cc_toolchain.all_files]),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxCompactImageArchive",
        progress_message = "Archiving grouped compact Linux image %{label}",
    )
    return [
        DefaultInfo(files = depset([out])),
        LinuxImageInfo(
            archives = [],
            module_objects = module_infos,
            objects = object_infos,
            output = out,
        ),
    ]

linux_grouped_compact_image = rule(
    implementation = _linux_grouped_compact_image_impl,
    attrs = {
        "arch": attr.string(default = "x86"),
        "config": attr.string(mandatory = True),
        "groups": attr.label_list(
            mandatory = True,
            providers = [LinuxObjectActionGroupInfo],
        ),
        "module_object_targets": attr.string_list(),
        "object_targets": attr.string_list(mandatory = True),
    },
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Archives one ordered config projection over shared object action groups.",
)
