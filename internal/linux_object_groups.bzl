"""Lazy action groups for direct, content-addressed Linux object compiles."""

load(
    "@rules_cc//cc:action_names.bzl",
    "CPP_LINK_STATIC_LIBRARY_ACTION_NAME",
    "C_COMPILE_ACTION_NAME",
)
load("@rules_cc//cc:find_cc_toolchain.bzl", "CC_TOOLCHAIN_TYPE", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load(":flag_programs.bzl", "LinuxFlagProgramsInfo")
load(":graph_profile.bzl", "LinuxGraphProfileInfo")
load(
    ":linux_objects.bzl",
    "LinuxCompileEnvironmentIndexInfo",
    "LinuxImageInfo",
    "LinuxModuleObjectsInfo",
    "LinuxObjectInfo",
)
load(
    ":path_mapping.bzl",
    "add_directory_arg",
    "add_mapped_values",
    "directory_anchor",
    "path_mapped_run",
)

visibility("public")

LinuxObjectActionGroupInfo = provider(
    doc = "Objects whose actions are owned by one compact-v7 recipe/reachability group.",
    fields = {
        "mode": "Shared Kbuild mode for every object in the group.",
        "object_targets": "Canonical compact-v7 object target names.",
        "objects": "Dictionary from compact-v7 object target name to LinuxObjectInfo.",
        "reachable_configs": "Exact sorted config names sharing this action group.",
        "reachability_id": "Full compact-v7 reachability signature.",
        "recipe_id": "Full compact-v7 action recipe identity.",
    },
)

LinuxObjectProjectionInfo = provider(
    doc = "Ordered image or module projection over lazy object action groups.",
    fields = {
        "config": "Compact-v7 config name selected by this projection.",
        "mode": "Expected Kbuild mode: y for an image or m for modules.",
        "object_targets": "Ordered compact-v7 root object target names.",
        "objects": "Ordered LinuxObjectInfo values for the selected roots.",
        "objects_by_target": "Dictionary from selected target name to LinuxObjectInfo.",
    },
)

_PROFILE_ARCH_FOR_LINUX_ARCH = {
    "arm64": "aarch64",
    "x86": "x86_64",
}

_EFFECT_ORDER = [
    "argv",
    "input",
    "output",
    "graph",
]

_SPECIAL_DIRECT_OBJECTS = [
    "arch/arm64/kernel/vdso-wrap.o",
    "arch/arm64/kernel/vdso32-wrap.o",
    "arch/x86/entry/vdso/vdso-image-64.o",
    "arch/x86/kernel/cpu/capflags.o",
    "arch/x86/lib/inat.o",
    "arch/x86/purgatory/kexec-purgatory.o",
    "arch/x86/realmode/rmpiggy.o",
    "drivers/of/empty_root.dtb.o",
    "drivers/scsi/scsi_sysfs.o",
    "drivers/tty/vt/consolemap_deftbl.o",
    "drivers/tty/vt/ucs.o",
    "lib/crc/crc32-main.o",
    "lib/crc/crc64-main.o",
    "lib/crc32.o",
    "lib/crc64.o",
    "lib/oid_registry.o",
    "usr/initramfs_data.o",
]

def _validate_content_id(value, what):
    if len(value) != 64:
        fail("%s must be a full lowercase SHA-256 digest, got %r" % (what, value))
    for index in range(len(value)):
        if value[index] not in "0123456789abcdef":
            fail("%s must be a full lowercase SHA-256 digest, got %r" % (what, value))

def _validate_canonical_strings(values, what, allow_empty = False):
    if not allow_empty and not values:
        fail("%s must not be empty" % what)
    previous = ""
    for value in values:
        if type(value) != "string" or not value:
            fail("%s entries must be non-empty strings" % what)
        if previous and previous >= value:
            fail("%s must be sorted with no duplicates: %r follows %r" % (what, value, previous))
        previous = value

def _validate_source_path(path, what):
    if (
        not path or
        path.startswith("/") or
        path.startswith("../") or
        path == ".." or
        path.startswith("./") or
        path == "." or
        "//" in path or
        "/./" in path or
        path.endswith("/.") or
        "/../" in path or
        path.endswith("/..") or
        "\\" in path
    ):
        fail("%s must be a normalized source-root-relative path, got %r" % (what, path))

def _validate_effects(values, what):
    if not values:
        fail("%s must not be empty" % what)
    previous = -1
    for value in values:
        if value not in _EFFECT_ORDER:
            fail("%s has unknown effect %r" % (what, value))
        index = _EFFECT_ORDER.index(value)
        if index <= previous:
            fail("%s must be canonically ordered with no duplicates" % what)
        previous = index
    for unsupported in ["output", "graph"]:
        if unsupported in values:
            fail(
                "direct action groups do not support %s effect in %s" %
                (unsupported, what),
            )

def _single_source_files(ctx):
    if len(ctx.attr.srcs) != len(ctx.attr.source_paths):
        fail(
            "linux_object_action_group %s has %d src labels but %d source_paths" %
            (ctx.label, len(ctx.attr.srcs), len(ctx.attr.source_paths)),
        )
    files = []
    indices = {}
    root_dir = ctx.file.source_root.dirname
    for index in range(len(ctx.attr.srcs)):
        target = ctx.attr.srcs[index]
        source_path = ctx.attr.source_paths[index]
        _validate_source_path(source_path, "source_paths[%d]" % index)
        target_files = target[DefaultInfo].files.to_list()
        if len(target_files) != 1:
            fail(
                "linux_object_action_group %s source %s must provide exactly one file, got %d" %
                (ctx.label, target.label, len(target_files)),
            )
        file = target_files[0]
        expected = root_dir + "/" + source_path
        if file.path != expected:
            fail(
                "linux_object_action_group %s source_paths[%d] = %r resolves to %s, want %s" %
                (ctx.label, index, source_path, file.path, expected),
            )
        files.append(file)
        indices[source_path] = index + 1
    _validate_canonical_strings(ctx.attr.source_paths, "source_paths")
    return struct(
        files = files,
        indices = indices,
    )

def _decode_int_list(raw, field, target):
    if type(raw) != "list":
        fail("object %s field %s must be a list" % (target, field))
    out = []
    previous = 0
    for value in raw:
        if type(value) != "int" or value <= 0:
            fail("object %s field %s must contain positive integers" % (target, field))
        if value <= previous:
            fail("object %s field %s must be sorted with no duplicates" % (target, field))
        out.append(value)
        previous = value
    return out

def _decode_string_list(raw, field, target):
    if raw == None:
        return []
    if type(raw) != "list":
        fail("object %s field %s must be a list" % (target, field))
    out = []
    for value in raw:
        if type(value) != "string" or not value:
            fail("object %s field %s must contain non-empty strings" % (target, field))
        out.append(value)
    return out

def _decode_object_spec(target, encoded, source_count):
    raw = json.decode(encoded)
    if type(raw) != "dict":
        fail("object %s must decode to an object" % target)
    allowed = [
        "action_source_group",
        "compile_environment",
        "content_id",
        "deps",
        "members",
        "object",
        "primary_source",
        "source_files",
    ]
    unknown = [key for key in raw.keys() if key not in allowed]
    if unknown:
        fail("object %s has unknown field(s): %s" % (target, ", ".join(sorted(unknown))))
    required = [
        "action_source_group",
        "compile_environment",
        "content_id",
        "object",
        "primary_source",
        "source_files",
    ]
    missing = [key for key in required if key not in raw]
    if missing:
        fail("object %s is missing field(s): %s" % (target, ", ".join(missing)))

    content_id = raw["content_id"]
    action_source_group = raw["action_source_group"]
    compile_environment = raw["compile_environment"]
    object_path = raw["object"]
    primary_source = raw["primary_source"]
    if type(content_id) != "string":
        fail("object %s content_id must be a string" % target)
    if type(action_source_group) != "string":
        fail("object %s action_source_group must be a string" % target)
    if type(compile_environment) != "string":
        fail("object %s compile_environment must be a string" % target)
    if type(object_path) != "string":
        fail("object %s object must be a string" % target)
    if type(primary_source) != "int":
        fail("object %s primary_source must be an integer" % target)
    _validate_content_id(content_id, "object %s content_id" % target)
    _validate_content_id(action_source_group, "object %s action_source_group" % target)
    _validate_content_id(compile_environment, "object %s compile_environment" % target)
    _validate_source_path(object_path, "object %s object" % target)
    if not object_path.endswith(".o"):
        fail("object %s path must end in .o, got %r" % (target, object_path))

    source_files = _decode_int_list(raw["source_files"], "source_files", target)
    if not source_files:
        fail("object %s source_files must not be empty" % target)
    if source_files[-1] > source_count:
        fail(
            "object %s source file index %d is out of range 1..%d" %
            (target, source_files[-1], source_count),
        )
    if primary_source not in source_files:
        fail("object %s primary_source %d is absent from source_files" % (target, primary_source))
    deps = _decode_string_list(raw.get("deps"), "deps", target)
    members = _decode_string_list(raw.get("members"), "members", target)
    if deps:
        fail("direct action group object %s has unsupported generated-header deps" % target)
    if members:
        fail("direct action group object %s has unsupported composite members" % target)
    return struct(
        action_source_group = action_source_group,
        compile_environment = compile_environment,
        content_id = content_id,
        object = object_path,
        primary_source = primary_source,
        source_files = source_files,
    )

def _decode_aggregate_spec(target, encoded, source_count = 0, action_source = False):
    raw = json.decode(encoded)
    if type(raw) != "dict":
        fail("object %s must decode to an object" % target)
    allowed = [
        "action_source_group",
        "compile_environment",
        "content_id",
        "deps",
        "members",
        "object",
        "primary_source",
        "source_files",
    ] if action_source else [
        "content_id",
        "deps",
        "members",
        "object",
    ]
    unknown = [key for key in raw.keys() if key not in allowed]
    if unknown:
        fail("object %s has unknown field(s): %s" % (target, ", ".join(sorted(unknown))))
    required = [
        "action_source_group",
        "compile_environment",
        "content_id",
        "members",
        "object",
        "primary_source",
        "source_files",
    ] if action_source else [
        "content_id",
        "members",
        "object",
    ]
    missing = [key for key in required if key not in raw]
    if missing:
        fail("object %s is missing field(s): %s" % (target, ", ".join(missing)))
    if raw.get("deps", []):
        fail("aggregate action group object %s has unsupported deps" % target)

    content_id = raw["content_id"]
    object_path = raw["object"]
    if type(content_id) != "string":
        fail("object %s content_id must be a string" % target)
    if type(object_path) != "string":
        fail("object %s object must be a string" % target)
    _validate_content_id(content_id, "object %s content_id" % target)
    _validate_source_path(object_path, "object %s object" % target)
    if not object_path.endswith(".o"):
        fail("object %s path must end in .o, got %r" % (target, object_path))
    members = _decode_string_list(raw["members"], "members", target)
    if not members:
        fail("aggregate action group object %s requires at least one member" % target)
    seen_members = {}
    for member in members:
        if member in seen_members:
            fail("aggregate action group object %s repeats member %s" % (target, member))
        seen_members[member] = True

    action_source_group = ""
    compile_environment = ""
    primary_source = 0
    source_files = []
    if action_source:
        action_source_group = raw["action_source_group"]
        compile_environment = raw["compile_environment"]
        primary_source = raw["primary_source"]
        if type(action_source_group) != "string":
            fail("object %s action_source_group must be a string" % target)
        if type(compile_environment) != "string":
            fail("object %s compile_environment must be a string" % target)
        if type(primary_source) != "int":
            fail("object %s primary_source must be an integer" % target)
        _validate_content_id(action_source_group, "object %s action_source_group" % target)
        _validate_content_id(compile_environment, "object %s compile_environment" % target)
        source_files = _decode_int_list(raw["source_files"], "source_files", target)
        if not source_files:
            fail("object %s source_files must not be empty" % target)
        if source_files[-1] > source_count:
            fail(
                "object %s source file index %d is out of range 1..%d" %
                (target, source_files[-1], source_count),
            )
        if primary_source not in source_files:
            fail("object %s primary_source %d is absent from source_files" % (target, primary_source))
    return struct(
        action_source_group = action_source_group,
        compile_environment = compile_environment,
        content_id = content_id,
        members = members,
        object = object_path,
        primary_source = primary_source,
        source_files = source_files,
    )

def _object_stem(object_path):
    base = object_path.rsplit("/", 1)[-1]
    if base.endswith(".o"):
        base = base[:-len(".o")]
    return base.replace("-", "_").replace(",", "_").replace(" ", "_")

def _object_name_flags(object_path, modname):
    stem = _object_stem(object_path)
    mod_object = modname if modname else object_path
    modstem = _object_stem(mod_object)
    modfile = mod_object[:-len(".o")] if mod_object.endswith(".o") else mod_object
    return [
        "-DKBUILD_BASENAME=\"%s\"" % stem,
        "-DKBUILD_MODNAME=\"%s\"" % modstem,
        "-D__KBUILD_MODNAME=kmod_%s" % modstem,
        "-DKBUILD_MODFILE=\"%s\"" % modfile,
    ]

def _generated_include_groups(include_dirs, srcarch):
    groups = struct(
        arch = [],
        arch_uapi = [],
        generic = [],
        generic_uapi = [],
        other = [],
    )
    arch_generated = "/arch/%s/include/generated" % srcarch
    for include_dir in include_dirs:
        if include_dir.endswith(arch_generated + "/uapi"):
            groups.arch_uapi.append(include_dir)
        elif include_dir.endswith(arch_generated):
            groups.arch.append(include_dir)
        elif include_dir.endswith("/include/generated/uapi"):
            groups.generic_uapi.append(include_dir)
        elif include_dir.endswith("/include"):
            groups.generic.append(include_dir)
        else:
            groups.other.append(include_dir)
    return groups

def _source_include_dirs(source_root, srcarch, generated_headers):
    generated_dirs = generated_headers.include_dirs if generated_headers != None else []
    generated = _generated_include_groups(generated_dirs, srcarch)
    root = source_root.dirname
    return (
        [root + "/arch/" + srcarch + "/include"] +
        generated.arch +
        [root + "/include"] +
        generated.generic +
        [root + "/arch/" + srcarch + "/include/uapi"] +
        generated.arch_uapi +
        [root + "/include/uapi"] +
        generated.generic_uapi +
        generated.other
    )

def _directory_anchors(source_root, source, config, generated_headers):
    anchors = {
        source_root.dirname: directory_anchor(source_root),
        source.dirname: directory_anchor(source),
    }
    if config.include_dir_anchor != None:
        anchors[config.include_dir] = config.include_dir_anchor
    if generated_headers != None:
        anchors.update(generated_headers.include_dir_anchors)
    return anchors

def _direct_compile_error(ctx, spec, source, config):
    object_path = spec.object
    if object_path in _SPECIAL_DIRECT_OBJECTS:
        return "requires a generated-object action"
    if (
        object_path.endswith(".asn1.o") or
        object_path.endswith(".pi.o") or
        object_path.endswith(".stub.o")
    ):
        return "requires generated-source or post-compile processing"
    if source.basename.endswith(".c_shipped"):
        return "requires source materialization"
    if ctx.attr.language == "c" and not source.basename.endswith(".c"):
        return "recipe language c does not match primary source %s" % source.basename
    if ctx.attr.language == "asm" and not (
        source.basename.endswith(".S") or
        source.basename.endswith(".s")
    ):
        return "recipe language asm does not match primary source %s" % source.basename
    if not (source.basename.endswith(".c") or source.basename.endswith(".S") or source.basename.endswith(".s")):
        return "has unsupported primary source %s" % source.basename
    if (
        ctx.attr.mode == "m" and
        (
            ctx.attr.module_root or
            (
                ctx.attr.objtool_force and
                ctx.executable.objtool != None and
                not ctx.attr.objtool_disabled
            )
        ) and
        ctx.attr.language == "c" and
        (
            config.config_flags.get("CONFIG_LTO_CLANG") == "y" or
            config.config_flags.get("CONFIG_LTO_CLANG_THIN") == "y" or
            config.config_flags.get("CONFIG_LTO_CLANG_FULL") == "y"
        )
    ):
        return "requires the module LTO relocatable-link stage"
    return ""

def _required_preinclude_paths(language):
    paths = [
        "include/linux/compiler-version.h",
        "include/linux/kconfig.h",
    ]
    if language == "c":
        paths.append("include/linux/compiler_types.h")
    return paths

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

def _unique_values(values):
    out = []
    seen = {}
    for value in values:
        if value in seen:
            continue
        seen[value] = True
        out.append(value)
    return out

def _merged_generated_include_dir_anchors(object_infos):
    anchors = {}
    for info in object_infos:
        anchors.update(info.generated_include_dir_anchors)
    return anchors

def _merged_generated_include_dirs(object_infos):
    return _unique_values([
        include_dir
        for info in object_infos
        for include_dir in info.generated_include_dirs
    ])

def _dependency_objects(ctx, groups, mode):
    available = {}
    for group in groups:
        for config in ctx.attr.reachable_configs:
            if config not in group.reachable_configs:
                fail(
                    "%s dependency group %s is not reachable in config %s" %
                    (ctx.label, group.reachability_id, config),
                )
        for target, info in group.objects.items():
            if target in available:
                fail("%s receives member target %s from multiple groups" % (ctx.label, target))
            if info.mode != mode:
                fail(
                    "%s member target %s has mode %r, want %r" %
                    (ctx.label, target, info.mode, mode),
                )
            available[target] = info
    return available

def _linux_target_triple(arch):
    if arch == "arm64":
        return "aarch64-linux-gnu"
    if arch == "x86":
        return "x86_64-linux-gnu"
    return ""

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
        "llvm++musl+musl_libc/" in path or
        "llvm++musl+musl_libc\\" in path or
        "llvm++kernel_headers+linux_kernel_headers_" in path
    )

def _profile_compile_flags(ctx, profile):
    variables = cc_common.create_compile_variables(
        feature_configuration = profile.feature_configuration,
        cc_toolchain = profile.cc_toolchain,
        user_compile_flags = ctx.fragments.cpp.copts + ctx.fragments.cpp.conlyopts,
    )
    flags = cc_common.get_memory_inefficient_command_line(
        feature_configuration = profile.feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
        variables = variables,
    )
    flags = _rewrite_target_flags(flags, _linux_target_triple(ctx.attr.arch))
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
    if profile.cc_toolchain.compiler.lower().find("clang") >= 0 and "-fintegrated-as" not in out:
        out.append("-fintegrated-as")
    return out

def _relocatable_link(
        ctx,
        profile,
        output,
        objects,
        extra_inputs = [],
        linker_script = None,
        mnemonic = "LinuxRelocatableLink",
        progress_message = "Linking Linux relocatable object %{label}"):
    args = ctx.actions.args()
    args.add("link")
    args.add("-linker", profile.kbuild_linker)
    args.add("-validation", profile.validation)
    args.add("-output", output)
    args.add_all(ctx.attr.relocatable_link_flags, before_each = "-base_arg")
    if linker_script != None:
        args.add("-linker_script", linker_script)
    args.add_all(objects, before_each = "-input")
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._ccprofile,
        inputs = depset(
            [
                profile.kbuild_linker,
                profile.validation,
            ] + objects + extra_inputs,
            transitive = [profile.toolchain_files],
        ),
        outputs = [output],
        arguments = [args],
        execution_requirements = profile.execution_requirements,
        mnemonic = mnemonic,
        progress_message = progress_message,
        toolchain = CC_TOOLCHAIN_TYPE,
    )

def _group_providers(ctx, objects, outputs):
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
        OutputGroupInfo(object = depset(outputs)),
    ]

def _compile_arguments(
        ctx,
        profile,
        spec,
        source,
        output,
        config,
        generated_headers,
        config_response,
        flags,
        removals):
    object_args = ["@" + config_response.path]
    mapping_files = [config_response]
    if generated_headers != None and generated_headers.cflags != None:
        object_args.append("@" + generated_headers.cflags.path)
        mapping_files.append(generated_headers.cflags)
    if ctx.attr.mode == "m":
        object_args.append("-DMODULE")
    object_args.extend(_object_name_flags(spec.object, ctx.attr.modname))

    root = ctx.file.source_root.dirname
    if ctx.attr.language == "asm":
        object_args.extend([
            "-D__KERNEL__",
            "-D__ASSEMBLY__",
            "-include",
            root + "/include/linux/compiler-version.h",
            "-include",
            root + "/include/linux/kconfig.h",
        ])
    else:
        object_args.extend([
            "-D__KERNEL__",
            "-include",
            root + "/include/linux/compiler-version.h",
            "-include",
            root + "/include/linux/kconfig.h",
            "-include",
            root + "/include/linux/compiler_types.h",
        ])
    object_args.append("-I" + config.include_dir)
    object_args.append("-fmacro-prefix-map=%s/=" % root)
    object_args.append("-I" + source.dirname)
    object_args.extend([
        "-I" + include_dir
        for include_dir in _source_include_dirs(
            ctx.file.source_root,
            ctx.attr.srcarch,
            generated_headers,
        )
    ])
    args = ctx.actions.args()
    args.add("compile")
    args.add("-template", profile.command_template)
    args.add("-validation", profile.validation)
    args.add("-source", source)
    args.add("-output", output)
    args.add("-config", config.config)
    args.add("-flags_file", flags)
    args.add("-remove_flags_file", removals)
    add_directory_arg(
        args,
        directory_anchor(ctx.file.source_root),
        format = "-source_root=%s",
    )
    args.add("-object_path", spec.object)
    anchors = _directory_anchors(
        ctx.file.source_root,
        source,
        config,
        generated_headers,
    )
    add_mapped_values(
        args,
        ["-arg=" + value for value in object_args],
        files = mapping_files,
        directory_anchors = anchors,
    )
    return args

def _linux_object_action_group_impl(ctx):
    _validate_content_id(ctx.attr.recipe_id, "recipe_id")
    _validate_content_id(ctx.attr.reachability_id, "reachability_id")
    _validate_canonical_strings(ctx.attr.reachable_configs, "reachable_configs")
    _validate_effects(ctx.attr.flag_effects, "flag_effects")
    _validate_effects(ctx.attr.remove_flag_effects, "remove_flag_effects")
    if ctx.attr.srcarch != ctx.attr.arch:
        fail(
            "linux_object_action_group %s srcarch %r does not match supported arch %r" %
            (ctx.label, ctx.attr.srcarch, ctx.attr.arch),
        )
    if ctx.attr.module_root and ctx.attr.mode != "m":
        fail("linux_object_action_group %s marks built-in objects as module roots" % ctx.label)
    use_objtool = ctx.executable.objtool != None and not ctx.attr.objtool_disabled
    if ctx.attr.arch == "x86" and not ctx.attr.objtool_disabled and not use_objtool:
        fail(
            "linux_object_action_group %s requires the shared x86 objtool executable unless objtool is disabled" %
            ctx.label,
        )
    if ctx.attr.arch != "x86" and use_objtool:
        fail(
            "linux_object_action_group %s only supports grouped objtool processing for x86" %
            ctx.label,
        )
    if not use_objtool and not ctx.attr.objtool_disabled and (ctx.attr.objtool_args or ctx.attr.objtool_force):
        fail(
            "linux_object_action_group %s has objtool arguments or force without an objtool executable" %
            ctx.label,
        )
    sources = _single_source_files(ctx)
    source_files = sources.files

    program_info = ctx.attr.flag_programs[LinuxFlagProgramsInfo]
    profile = program_info.graph_profile
    expected_profile_arch = _PROFILE_ARCH_FOR_LINUX_ARCH[ctx.attr.arch]
    if profile.arch != expected_profile_arch:
        fail(
            "linux_object_action_group %s profile arch %r does not match Linux arch %r" %
            (ctx.label, profile.arch, ctx.attr.arch),
        )
    environment_index = ctx.attr.compile_environment_index[LinuxCompileEnvironmentIndexInfo]
    flag_programs = program_info.programs
    for program_id, what in [
        (ctx.attr.flag_program, "flag_program"),
        (ctx.attr.remove_flag_program, "remove_flag_program"),
    ]:
        _validate_content_id(program_id, what)
        if program_id not in flag_programs:
            fail(
                "linux_object_action_group %s references unknown %s %s" %
                (ctx.label, what, program_id),
            )
    flags = flag_programs[ctx.attr.flag_program]
    recipe_removals = flag_programs[ctx.attr.remove_flag_program]

    targets = sorted(ctx.attr.objects.keys())
    if not targets:
        fail("linux_object_action_group %s requires at least one object" % ctx.label)
    objects = {}
    outputs = []
    content_ids = {}
    for target in targets:
        if not target:
            fail("linux_object_action_group %s has an empty object target name" % ctx.label)
        spec = _decode_object_spec(target, ctx.attr.objects[target], len(source_files))
        if spec.content_id in content_ids:
            fail(
                "linux_object_action_group %s objects %s and %s share content ID %s" %
                (ctx.label, content_ids[spec.content_id], target, spec.content_id),
            )
        content_ids[spec.content_id] = target
        if spec.compile_environment not in environment_index.environments:
            fail(
                "linux_object_action_group %s object %s references unknown compile environment %s" %
                (ctx.label, target, spec.compile_environment),
            )
        environment = environment_index.environments[spec.compile_environment]
        config = environment.config
        generated_headers = environment.generated_headers
        source = source_files[spec.primary_source - 1]
        direct_error = _direct_compile_error(ctx, spec, source, config)
        if direct_error:
            fail("linux_object_action_group %s object %s %s" % (ctx.label, target, direct_error))
        source_indices = {index: True for index in spec.source_files}
        for required in _required_preinclude_paths(ctx.attr.language):
            if required not in sources.indices:
                fail("linux_object_action_group %s has no source label for required %s" % (ctx.label, required))
            required_index = sources.indices[required]
            if required_index not in source_indices:
                fail(
                    "linux_object_action_group %s object %s exact inputs omit required %s" %
                    (ctx.label, target, required),
                )

        config_response = config.aflags if ctx.attr.language == "asm" else config.cflags
        out = ctx.actions.declare_file(
            ctx.label.name + ".objects/" + spec.content_id + "/" + spec.object,
        )
        compile_out = out
        if use_objtool:
            compile_out = ctx.actions.declare_file(
                ctx.label.name + ".objects/" + spec.content_id + "/.objtool-input/" + spec.object,
            )
        exact_sources = depset([
            source_files[index - 1]
            for index in spec.source_files
        ])
        transitive_inputs = [
            exact_sources,
            config.files,
            profile.toolchain_files,
        ]
        if generated_headers != None:
            transitive_inputs.append(generated_headers.files)
        path_mapped_run(
            ctx.actions,
            executable = ctx.executable._ccprofile,
            inputs = depset(
                [
                    config_response,
                    ctx.file.source_root,
                    profile.command_template,
                    profile.validation,
                    flags,
                    recipe_removals,
                ],
                transitive = transitive_inputs,
            ),
            outputs = [compile_out],
            arguments = [_compile_arguments(
                ctx,
                profile,
                spec,
                source,
                compile_out,
                config,
                generated_headers,
                config_response,
                flags,
                recipe_removals,
            )],
            execution_requirements = profile.execution_requirements,
            mnemonic = "LinuxObjectCompile",
            progress_message = "Compiling Linux object %s" % spec.object,
            toolchain = CC_TOOLCHAIN_TYPE,
        )
        if use_objtool:
            _grouped_objtool(ctx, config, compile_out, out)
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
        )
        objects[target] = info
        outputs.append(out)

    return [
        DefaultInfo(files = depset(outputs)),
        LinuxObjectActionGroupInfo(
            mode = ctx.attr.mode,
            object_targets = targets,
            objects = objects,
            reachable_configs = list(ctx.attr.reachable_configs),
            reachability_id = ctx.attr.reachability_id,
            recipe_id = ctx.attr.recipe_id,
        ),
        OutputGroupInfo(object = depset(outputs)),
    ]

linux_object_action_group = rule(
    implementation = _linux_object_action_group_impl,
    attrs = {
        "arch": attr.string(
            mandatory = True,
            values = ["arm64", "x86"],
        ),
        "flag_program": attr.string(mandatory = True),
        "flag_programs": attr.label(
            mandatory = True,
            providers = [LinuxFlagProgramsInfo],
        ),
        "compile_environment_index": attr.label(
            mandatory = True,
            providers = [LinuxCompileEnvironmentIndexInfo],
        ),
        "flag_effects": attr.string_list(
            mandatory = True,
            doc = "Canonical compact-v7 effects of the resolved flag program.",
        ),
        "language": attr.string(
            mandatory = True,
            values = ["asm", "c"],
        ),
        "mode": attr.string(
            mandatory = True,
            values = ["m", "y"],
        ),
        "modname": attr.string(),
        "module_root": attr.bool(),
        "objtool": attr.label(
            cfg = "exec",
            doc = "Arch-level x86 objtool executable shared by every enabled object in this recipe group.",
            executable = True,
        ),
        "objtool_args": attr.string_list(
            doc = "Kbuild arguments passed to grouped per-object objtool processing.",
        ),
        "objtool_disabled": attr.bool(
            doc = "Whether Kbuild disabled objtool. Suppression alone does not make a direct compile unsupported.",
        ),
        "objtool_force": attr.bool(
            doc = "Whether Kbuild forces grouped objtool processing despite delayed mode.",
        ),
        "objects": attr.string_dict(
            mandatory = True,
            doc = "Compact-v7 object specs keyed by canonical object target name.",
        ),
        "reachable_configs": attr.string_list(mandatory = True),
        "reachability_id": attr.string(mandatory = True),
        "recipe_id": attr.string(mandatory = True),
        "remove_flag_program": attr.string(mandatory = True),
        "remove_flag_effects": attr.string_list(
            mandatory = True,
            doc = "Canonical compact-v7 effects of the resolved remove-flag program.",
        ),
        "source_paths": attr.string_list(
            mandatory = True,
            doc = "Canonical source-root-relative paths parallel to srcs.",
        ),
        "source_root": attr.label(
            allow_single_file = True,
            mandatory = True,
        ),
        "srcarch": attr.string(mandatory = True),
        "srcs": attr.label_list(
            allow_files = True,
            mandatory = True,
            doc = "Union of exact source files used by objects in this group.",
        ),
        "_ccprofile": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/ccprofile"),
            executable = True,
        ),
        "_objtoolrun": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/objtoolrun"),
            executable = True,
        ),
    },
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Owns one direct compile action per object in one compact-v7 recipe/reachability group.",
)

def _linux_object_action_group_import_impl(ctx):
    _validate_content_id(ctx.attr.recipe_id, "recipe_id")
    _validate_content_id(ctx.attr.reachability_id, "reachability_id")
    _validate_canonical_strings(ctx.attr.reachable_configs, "reachable_configs")
    _validate_canonical_strings(ctx.attr.object_targets, "object_targets")
    if len(ctx.attr.object_targets) != len(ctx.attr.objects):
        fail(
            "linux_object_action_group_import %s has %d object_targets but %d objects" %
            (ctx.label, len(ctx.attr.object_targets), len(ctx.attr.objects)),
        )

    objects = {}
    outputs = []
    content_ids = {}
    modes = {}
    for index in range(len(ctx.attr.object_targets)):
        target = ctx.attr.object_targets[index]
        info = ctx.attr.objects[index][LinuxObjectInfo]
        _validate_content_id(
            info.content_id,
            "linux_object_action_group_import %s object %s content_id" % (ctx.label, target),
        )
        if info.content_id in content_ids:
            fail(
                "linux_object_action_group_import %s objects %s and %s share content ID %s" %
                (ctx.label, content_ids[info.content_id], target, info.content_id),
            )
        content_ids[info.content_id] = target
        modes[info.mode] = True
        objects[target] = info
        outputs.append(info.output)
    if len(modes) != 1:
        fail(
            "linux_object_action_group_import %s requires one shared object mode, got %s" %
            (ctx.label, ", ".join(sorted(modes.keys()))),
        )

    mode = modes.keys()[0]
    return [
        DefaultInfo(files = depset(outputs)),
        LinuxObjectActionGroupInfo(
            mode = mode,
            object_targets = list(ctx.attr.object_targets),
            objects = objects,
            reachable_configs = list(ctx.attr.reachable_configs),
            reachability_id = ctx.attr.reachability_id,
            recipe_id = ctx.attr.recipe_id,
        ),
        OutputGroupInfo(object = depset(outputs)),
    ]

linux_object_action_group_import = rule(
    implementation = _linux_object_action_group_import_impl,
    attrs = {
        "object_targets": attr.string_list(
            mandatory = True,
            doc = "Canonical compact-v7 target names parallel to objects.",
        ),
        "objects": attr.label_list(
            mandatory = True,
            providers = [LinuxObjectInfo],
            doc = "Exceptional legacy object targets imported without creating actions.",
        ),
        "reachable_configs": attr.string_list(mandatory = True),
        "reachability_id": attr.string(mandatory = True),
        "recipe_id": attr.string(mandatory = True),
    },
    doc = "Adapts exceptional legacy LinuxObjectInfo targets into a lazy action group.",
)

def _linux_composite_object_action_group_impl(ctx):
    _validate_content_id(ctx.attr.recipe_id, "recipe_id")
    _validate_content_id(ctx.attr.reachability_id, "reachability_id")
    _validate_canonical_strings(ctx.attr.reachable_configs, "reachable_configs")
    if ctx.attr.module_root and ctx.attr.mode != "m":
        fail("linux_composite_object_action_group %s marks built-in objects as module roots" % ctx.label)

    profile = ctx.attr.graph_profile[LinuxGraphProfileInfo]
    expected_profile_arch = _PROFILE_ARCH_FOR_LINUX_ARCH[ctx.attr.arch]
    if profile.arch != expected_profile_arch:
        fail(
            "linux_composite_object_action_group %s profile arch %r does not match Linux arch %r" %
            (ctx.label, profile.arch, ctx.attr.arch),
        )
    dependency_groups = [
        target[LinuxObjectActionGroupInfo]
        for target in ctx.attr.member_groups
    ]
    available = _dependency_objects(ctx, dependency_groups, ctx.attr.mode)

    specs = {}
    content_ids = {}
    for target in sorted(ctx.attr.objects.keys()):
        if not target:
            fail("linux_composite_object_action_group %s has an empty object target name" % ctx.label)
        if target in available:
            fail("linux_composite_object_action_group %s shadows member target %s" % (ctx.label, target))
        spec = _decode_aggregate_spec(target, ctx.attr.objects[target])
        if spec.content_id in content_ids:
            fail(
                "linux_composite_object_action_group %s objects %s and %s share content ID %s" %
                (ctx.label, content_ids[spec.content_id], target, spec.content_id),
            )
        content_ids[spec.content_id] = target
        specs[target] = spec
    if not specs:
        fail("linux_composite_object_action_group %s requires at least one object" % ctx.label)

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
            _relocatable_link(
                ctx,
                profile,
                out,
                [info.output for info in member_infos],
                mnemonic = "LinuxCompositeObject",
                progress_message = "Linking grouped Linux composite object %{label}",
            )
            info = LinuxObjectInfo(
                content_id = spec.content_id,
                generated_headers = depset(
                    transitive = [member.generated_headers for member in member_infos],
                ),
                generated_include_dir_anchors = _merged_generated_include_dir_anchors(member_infos),
                generated_include_dirs = _merged_generated_include_dirs(member_infos),
                mode = ctx.attr.mode,
                module_root_kind = "composite" if ctx.attr.module_root else "",
                object = spec.object,
                objtool_args = list(ctx.attr.objtool_args),
                objtool_force = ctx.attr.objtool_force,
                output = out,
            )
            available[target] = info
            objects[target] = info
            outputs.append(out)
            pending.pop(target)
            progressed = True
        if not pending:
            break
        if not progressed:
            break
    if pending:
        unresolved = []
        for target in sorted(pending.keys()):
            missing = [
                member
                for member in pending[target].members
                if member not in available
            ]
            unresolved.append("%s -> %s" % (target, ", ".join(missing)))
        fail(
            "linux_composite_object_action_group %s has unresolved or cyclic members: %s" %
            (ctx.label, "; ".join(unresolved)),
        )
    return _group_providers(ctx, objects, outputs)

linux_composite_object_action_group = rule(
    implementation = _linux_composite_object_action_group_impl,
    attrs = {
        "arch": attr.string(
            mandatory = True,
            values = ["arm64", "x86"],
        ),
        "graph_profile": attr.label(
            mandatory = True,
            providers = [LinuxGraphProfileInfo],
        ),
        "member_groups": attr.label_list(
            mandatory = True,
            providers = [LinuxObjectActionGroupInfo],
            doc = "Action groups publishing members referenced by aggregate object specs.",
        ),
        "mode": attr.string(
            mandatory = True,
            values = ["m", "y"],
        ),
        "module_root": attr.bool(),
        "objects": attr.string_dict(
            mandatory = True,
            doc = "Compact-v7 composite specs keyed by canonical object target name.",
        ),
        "objtool_args": attr.string_list(),
        "objtool_force": attr.bool(),
        "reachable_configs": attr.string_list(mandatory = True),
        "reachability_id": attr.string(mandatory = True),
        "recipe_id": attr.string(mandatory = True),
        "relocatable_link_flags": attr.string_list(
            mandatory = True,
            doc = "Profile-selected driver arguments for a hermetic relocatable link.",
        ),
        "_ccprofile": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/ccprofile"),
            executable = True,
        ),
    },
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Owns relocatable links for one compact-v7 composite recipe/reachability group.",
)

def _nvhe_linker_script(
        ctx,
        profile,
        spec,
        source_files,
        config,
        generated_headers,
        output):
    source = source_files[spec.primary_source - 1]
    args = ctx.actions.args()
    args.add_all(_profile_compile_flags(ctx, profile))
    mapped = [
        "-E",
        "-P",
        "-D__KERNEL__",
        "-D__ASSEMBLY__",
        "-DLINKER_SCRIPT",
        "-include",
        ctx.file.source_root.dirname + "/include/linux/kconfig.h",
        "-Uarm64",
        "-I" + config.include_dir,
    ] + [
        "-I" + include_dir
        for include_dir in _source_include_dirs(
            ctx.file.source_root,
            "arm64",
            generated_headers,
        )
    ]
    add_mapped_values(
        args,
        mapped,
        directory_anchors = _directory_anchors(
            ctx.file.source_root,
            source,
            config,
            generated_headers,
        ),
    )
    args.add(source)
    args.add("-o")
    args.add(output)
    exact_sources = depset([
        source_files[index - 1]
        for index in spec.source_files
    ])
    path_mapped_run(
        ctx.actions,
        executable = profile.compiler,
        inputs = depset(
            [source, ctx.file.source_root],
            transitive = [
                exact_sources,
                config.files,
                generated_headers.files,
                profile.toolchain_files,
            ],
        ),
        outputs = [output],
        arguments = [args],
        execution_requirements = profile.execution_requirements,
        mnemonic = "LinuxArm64NvheLinkerScript",
        progress_message = "Preprocessing grouped Linux arm64 nVHE linker script %{label}",
        toolchain = CC_TOOLCHAIN_TYPE,
    )

def _nvhe_compile_reloc(
        ctx,
        profile,
        exact_sources,
        config,
        generated_headers,
        source,
        output):
    object_args = ["@" + config.aflags.path]
    mapping_files = [config.aflags]
    if generated_headers.cflags != None:
        object_args.append("@" + generated_headers.cflags.path)
        mapping_files.append(generated_headers.cflags)
    object_args.extend(_object_name_flags(
        "arch/arm64/kvm/hyp/nvhe/hyp-reloc.o",
        "",
    ))
    object_args.extend([
        "-D__KERNEL__",
        "-D__ASSEMBLY__",
        "-include",
        ctx.file.source_root.dirname + "/include/linux/compiler-version.h",
        "-include",
        ctx.file.source_root.dirname + "/include/linux/kconfig.h",
        "-I" + config.include_dir,
        "-I" + source.dirname,
    ])
    object_args.extend([
        "-I" + include_dir
        for include_dir in _source_include_dirs(
            ctx.file.source_root,
            "arm64",
            generated_headers,
        )
    ])

    args = ctx.actions.args()
    args.add("compile")
    args.add("-template", profile.command_template)
    args.add("-validation", profile.validation)
    args.add("-source", source)
    args.add("-output", output)
    args.add("-config", config.config)
    anchors = _directory_anchors(
        ctx.file.source_root,
        source,
        config,
        generated_headers,
    )
    add_mapped_values(
        args,
        ["-arg=" + value for value in object_args],
        files = mapping_files,
        directory_anchors = anchors,
    )
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._ccprofile,
        inputs = depset(
            [
                config.aflags,
                ctx.file.source_root,
                profile.command_template,
                profile.validation,
                source,
            ],
            transitive = [
                exact_sources,
                config.files,
                generated_headers.files,
                profile.toolchain_files,
            ],
        ),
        outputs = [output],
        arguments = [args],
        execution_requirements = profile.execution_requirements,
        mnemonic = "LinuxVmlinuxCompile",
        progress_message = "Compiling grouped Linux arm64 nVHE relocation object %{label}",
        toolchain = CC_TOOLCHAIN_TYPE,
    )

def _linux_arm64_nvhe_object_action_group_impl(ctx):
    _validate_content_id(ctx.attr.recipe_id, "recipe_id")
    _validate_content_id(ctx.attr.reachability_id, "reachability_id")
    _validate_canonical_strings(ctx.attr.reachable_configs, "reachable_configs")
    sources = _single_source_files(ctx)

    profile = ctx.attr.graph_profile[LinuxGraphProfileInfo]
    if profile.arch != "aarch64":
        fail(
            "linux_arm64_nvhe_object_action_group %s requires an aarch64 profile, got %r" %
            (ctx.label, profile.arch),
        )
    environment_index = ctx.attr.compile_environment_index[LinuxCompileEnvironmentIndexInfo]

    dependency_groups = [
        target[LinuxObjectActionGroupInfo]
        for target in ctx.attr.member_groups
    ]
    available = _dependency_objects(ctx, dependency_groups, "y")
    objects = {}
    outputs = []
    content_ids = {}
    for target in sorted(ctx.attr.objects.keys()):
        if not target:
            fail("linux_arm64_nvhe_object_action_group %s has an empty object target name" % ctx.label)
        spec = _decode_aggregate_spec(
            target,
            ctx.attr.objects[target],
            source_count = len(sources.files),
            action_source = True,
        )
        if spec.object != "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.o":
            fail(
                "linux_arm64_nvhe_object_action_group %s object %s has unsupported path %s" %
                (ctx.label, target, spec.object),
            )
        if spec.content_id in content_ids:
            fail(
                "linux_arm64_nvhe_object_action_group %s objects %s and %s share content ID %s" %
                (ctx.label, content_ids[spec.content_id], target, spec.content_id),
            )
        content_ids[spec.content_id] = target
        if spec.compile_environment not in environment_index.environments:
            fail(
                "linux_arm64_nvhe_object_action_group %s object %s references unknown compile environment %s" %
                (ctx.label, target, spec.compile_environment),
            )
        environment = environment_index.environments[spec.compile_environment]
        config = environment.config
        generated_headers = environment.generated_headers
        if config == None:
            fail("linux_arm64_nvhe_object_action_group %s object %s requires config" % (ctx.label, target))
        if generated_headers == None:
            fail("linux_arm64_nvhe_object_action_group %s object %s requires generated headers" % (ctx.label, target))
        if generated_headers.arch != "arm64" or generated_headers.srcarch != "arm64":
            fail(
                "linux_arm64_nvhe_object_action_group %s object %s requires arm64 generated headers" %
                (ctx.label, target),
            )
        missing_members = [member for member in spec.members if member not in available]
        if missing_members:
            fail(
                "linux_arm64_nvhe_object_action_group %s object %s has unknown members: %s" %
                (ctx.label, target, ", ".join(missing_members)),
            )
        if sources.indices.get("arch/arm64/kvm/hyp/nvhe/hyp.lds.S") != spec.primary_source:
            fail(
                "linux_arm64_nvhe_object_action_group %s object %s primary source must be hyp.lds.S" %
                (ctx.label, target),
            )
        source_indices = {index: True for index in spec.source_files}
        for required in [
            "arch/arm64/kvm/hyp/nvhe/hyp.lds.S",
            "include/linux/compiler-version.h",
            "include/linux/kconfig.h",
        ]:
            if required not in sources.indices or sources.indices[required] not in source_indices:
                fail(
                    "linux_arm64_nvhe_object_action_group %s object %s exact inputs omit required %s" %
                    (ctx.label, target, required),
                )

        base = ctx.label.name + ".objects/" + spec.content_id + ".pipeline"
        linker_script = ctx.actions.declare_file(
            base + "/arch/arm64/kvm/hyp/nvhe/hyp.lds",
        )
        _nvhe_linker_script(
            ctx,
            profile,
            spec,
            sources.files,
            config,
            generated_headers,
            linker_script,
        )
        member_infos = [available[member] for member in spec.members]
        member_outputs = [info.output for info in member_infos]
        tmp = ctx.actions.declare_file(
            base + "/arch/arm64/kvm/hyp/nvhe/kvm_nvhe.tmp.o",
        )
        _relocatable_link(
            ctx,
            profile,
            tmp,
            member_outputs,
            extra_inputs = [linker_script],
            linker_script = linker_script,
        )

        reloc_s = ctx.actions.declare_file(
            base + "/arch/arm64/kvm/hyp/nvhe/hyp-reloc.S",
        )
        reloc_args = ctx.actions.args()
        reloc_args.add(reloc_s)
        reloc_args.add(ctx.executable._genhyprel)
        reloc_args.add(tmp)
        path_mapped_run(
            ctx.actions,
            executable = ctx.attr._runandwrite[DefaultInfo].files_to_run,
            inputs = [tmp],
            tools = [ctx.attr._genhyprel[DefaultInfo].files_to_run],
            outputs = [reloc_s],
            arguments = [reloc_args],
            mnemonic = "LinuxArm64NvheHypReloc",
            progress_message = "Generating grouped Linux arm64 nVHE relocation source %{label}",
        )

        exact_sources = depset([
            sources.files[index - 1]
            for index in spec.source_files
        ])
        hyp_reloc = ctx.actions.declare_file(
            base + "/arch/arm64/kvm/hyp/nvhe/hyp-reloc.o",
        )
        _nvhe_compile_reloc(
            ctx,
            profile,
            exact_sources,
            config,
            generated_headers,
            reloc_s,
            hyp_reloc,
        )
        rel = ctx.actions.declare_file(
            base + "/arch/arm64/kvm/hyp/nvhe/kvm_nvhe.rel.o",
        )
        _relocatable_link(
            ctx,
            profile,
            rel,
            [tmp, hyp_reloc],
        )

        out = ctx.actions.declare_file(
            ctx.label.name + ".objects/" + spec.content_id + "/" + spec.object,
        )
        objcopy_args = ctx.actions.args()
        objcopy_args.add("--prefix-symbols=__kvm_nvhe_")
        objcopy_args.add(rel)
        objcopy_args.add(out)
        path_mapped_run(
            ctx.actions,
            executable = ctx.executable.objcopy,
            inputs = [rel],
            tools = [ctx.attr.objcopy[DefaultInfo].files_to_run],
            outputs = [out],
            arguments = [objcopy_args],
            mnemonic = "LinuxArm64NvheObjcopy",
            progress_message = "Objcopying grouped Linux arm64 nVHE object %{label}",
        )

        info = LinuxObjectInfo(
            content_id = spec.content_id,
            generated_headers = depset(
                transitive = [member.generated_headers for member in member_infos],
            ),
            generated_include_dir_anchors = _merged_generated_include_dir_anchors(member_infos),
            generated_include_dirs = _merged_generated_include_dirs(member_infos),
            mode = "y",
            module_root_kind = "",
            object = spec.object,
            objtool_args = [],
            objtool_force = False,
            output = out,
        )
        objects[target] = info
        outputs.append(out)
    if not objects:
        fail("linux_arm64_nvhe_object_action_group %s requires at least one object" % ctx.label)
    return _group_providers(ctx, objects, outputs)

linux_arm64_nvhe_object_action_group = rule(
    implementation = _linux_arm64_nvhe_object_action_group_impl,
    attrs = {
        "arch": attr.string(default = "arm64", values = ["arm64"]),
        "graph_profile": attr.label(
            mandatory = True,
            providers = [LinuxGraphProfileInfo],
        ),
        "compile_environment_index": attr.label(
            mandatory = True,
            providers = [LinuxCompileEnvironmentIndexInfo],
        ),
        "member_groups": attr.label_list(
            mandatory = True,
            providers = [LinuxObjectActionGroupInfo],
        ),
        "mode": attr.string(default = "y", values = ["y"]),
        "objects": attr.string_dict(mandatory = True),
        "objcopy": attr.label(
            allow_single_file = True,
            cfg = "exec",
            executable = True,
            mandatory = True,
        ),
        "reachable_configs": attr.string_list(mandatory = True),
        "reachability_id": attr.string(mandatory = True),
        "recipe_id": attr.string(mandatory = True),
        "relocatable_link_flags": attr.string_list(
            mandatory = True,
            doc = "Profile-selected driver arguments for a hermetic relocatable link.",
        ),
        "source_paths": attr.string_list(mandatory = True),
        "source_root": attr.label(
            allow_single_file = True,
            mandatory = True,
        ),
        "srcs": attr.label_list(
            allow_files = True,
            mandatory = True,
        ),
        "_ccprofile": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/ccprofile"),
            executable = True,
        ),
        "_genhyprel": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/genhyprel"),
            executable = True,
        ),
        "_runandwrite": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/runandwrite"),
            executable = True,
        ),
    },
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Owns specialized arm64 nVHE pipelines for one compact-v7 recipe/reachability group.",
)

def _linux_object_projection_impl(ctx):
    groups = [target[LinuxObjectActionGroupInfo] for target in ctx.attr.groups]
    available = {}
    for group in groups:
        if group.mode != ctx.attr.mode:
            fail(
                "linux_object_projection %s mode %r includes %r group %s" %
                (ctx.label, ctx.attr.mode, group.mode, group.recipe_id),
            )
        if ctx.attr.config not in group.reachable_configs:
            fail(
                "linux_object_projection %s config %r is outside group %s reachability %s" %
                (ctx.label, ctx.attr.config, group.reachability_id, group.reachable_configs),
            )
        for target, info in group.objects.items():
            if target in available:
                fail("linux_object_projection %s receives object %s from multiple groups" % (ctx.label, target))
            available[target] = info

    selected = []
    selected_by_target = {}
    for target in ctx.attr.object_targets:
        if target in selected_by_target:
            fail("linux_object_projection %s repeats object target %s" % (ctx.label, target))
        if target not in available:
            fail("linux_object_projection %s references unknown object target %s" % (ctx.label, target))
        info = available[target]
        if info.mode != ctx.attr.mode:
            fail(
                "linux_object_projection %s object %s has mode %r, want %r" %
                (ctx.label, target, info.mode, ctx.attr.mode),
            )
        selected.append(info)
        selected_by_target[target] = info
    return [
        DefaultInfo(files = depset([info.output for info in selected])),
        LinuxObjectProjectionInfo(
            config = ctx.attr.config,
            mode = ctx.attr.mode,
            object_targets = list(ctx.attr.object_targets),
            objects = selected,
            objects_by_target = selected_by_target,
        ),
    ]

linux_object_projection = rule(
    implementation = _linux_object_projection_impl,
    attrs = {
        "config": attr.string(mandatory = True),
        "groups": attr.label_list(
            mandatory = True,
            providers = [LinuxObjectActionGroupInfo],
        ),
        "mode": attr.string(
            mandatory = True,
            values = ["m", "y"],
        ),
        "object_targets": attr.string_list(
            doc = "Ordered compact-v7 image or module root targets.",
        ),
    },
    doc = "Projects ordered image or module roots without introducing one target per object.",
)

def linux_image_object_projection(name, **kwargs):
    linux_object_projection(name = name, mode = "y", **kwargs)

def linux_module_object_projection(name, **kwargs):
    linux_object_projection(name = name, mode = "m", **kwargs)

def _linux_grouped_image_impl(ctx):
    projection = ctx.attr.objects[LinuxObjectProjectionInfo]
    if projection.mode != "y":
        fail("linux_grouped_image %s requires a built-in object projection" % ctx.label)
    if not projection.objects:
        fail("linux_grouped_image %s requires at least one built-in object" % ctx.label)
    profile = ctx.attr.graph_profile[LinuxGraphProfileInfo]
    archiver = cc_common.get_tool_for_action(
        feature_configuration = profile.feature_configuration,
        action_name = CPP_LINK_STATIC_LIBRARY_ACTION_NAME,
    )
    output = ctx.actions.declare_file(ctx.label.name + ".vmlinux.a")
    object_outputs = [info.output for info in projection.objects]
    args = ctx.actions.args()
    args.add("cDPrST")
    args.add(output)
    args.add_all(object_outputs)
    path_mapped_run(
        ctx.actions,
        executable = archiver,
        inputs = depset(object_outputs, transitive = [profile.toolchain_files]),
        outputs = [output],
        arguments = [args],
        execution_requirements = profile.execution_requirements,
        mnemonic = "LinuxCompactImageArchive",
        progress_message = "Archiving compact Linux image %{label}",
        toolchain = CC_TOOLCHAIN_TYPE,
    )
    return [
        DefaultInfo(files = depset([output])),
        LinuxImageInfo(
            archives = [],
            module_objects = [],
            objects = projection.objects,
            output = output,
        ),
    ]

linux_grouped_image = rule(
    implementation = _linux_grouped_image_impl,
    attrs = {
        "graph_profile": attr.label(
            mandatory = True,
            providers = [LinuxGraphProfileInfo],
        ),
        "objects": attr.label(
            mandatory = True,
            providers = [LinuxObjectProjectionInfo],
        ),
    },
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Archives one lazy built-in object projection behind the existing LinuxImageInfo contract.",
)

def _linux_grouped_modules_impl(ctx):
    projection = ctx.attr.objects[LinuxObjectProjectionInfo]
    if projection.mode != "m":
        fail("linux_grouped_modules %s requires a module object projection" % ctx.label)
    return [
        DefaultInfo(files = depset([info.output for info in projection.objects])),
        LinuxModuleObjectsInfo(objects = projection.objects),
    ]

linux_grouped_modules = rule(
    implementation = _linux_grouped_modules_impl,
    attrs = {
        "objects": attr.label(
            mandatory = True,
            providers = [LinuxObjectProjectionInfo],
        ),
    },
    doc = "Adapts one lazy module projection to the existing LinuxModuleObjectsInfo contract.",
)
