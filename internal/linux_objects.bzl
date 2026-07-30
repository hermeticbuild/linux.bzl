"""Native rules for compact, content-addressed Linux build units."""

load("@rules_cc//cc:action_names.bzl", "CPP_LINK_EXECUTABLE_ACTION_NAME", "CPP_LINK_STATIC_LIBRARY_ACTION_NAME", "C_COMPILE_ACTION_NAME")
load("@rules_cc//cc:find_cc_toolchain.bzl", "CC_TOOLCHAIN_TYPE", "find_cpp_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load(":flag_programs.bzl", "LinuxFlagProgramsInfo")
load(":graph_profile.bzl", "LinuxGraphProfileInfo")
load(":host_cc_toolchain.bzl", "host_cc_toolchain_attr")
load(":kconfig.bzl", "KconfigInfo")
load(":linux_module_actions.bzl", "linux_module_actions")
load(
    ":path_mapping.bzl",
    "add_directory_arg",
    "directory_anchor",
    "path_mapped_run",
)
load(":providers.bzl", "LinuxRustSdkInfo", "LinuxVmlinuxInfo")

visibility("public")

_PERL_TOOLCHAIN = "@rules_perl//perl:toolchain_type"
_RUST_TOOLCHAIN_TYPE = Label("@rules_rust//rust:toolchain_type")

LinuxCcContextInfo = provider(
    doc = "Linux-specific view of the configured Bazel C/C++ toolchain.",
    fields = {
        "arch": "Linux ARCH value.",
        "compile_flags": "Configured C compile flags from rules_cc.",
        "image_format": "Default compressed image format for this platform.",
        "srcarch": "Linux SRCARCH value.",
        "target_triple": "Target triple used by the configured toolchain, when known.",
    },
)

LinuxConfigInfo = provider(
    doc = "Materialized Linux configuration files consumed by native Linux actions.",
    fields = {
        "aflags": "Compiler response file containing config-derived Kbuild assembler flags.",
        "auto_conf": "include/config/auto.conf output.",
        "auto_conf_cmd": "include/config/auto.conf.cmd output.",
        "autoconf_h": "include/generated/autoconf.h output.",
        "config": ".config output.",
        "config_flags": "Resolved CONFIG_* values used to materialize the files.",
        "cflags": "Compiler response file containing config-derived Kbuild C flags.",
        "files": "Depset of all generated config files.",
        "include_dir": "Generated include directory path for compiler -I flags.",
        "include_dir_anchor": "File-backed reference to include_dir for path-mapped actions.",
        "kernel_release": "include/config/kernel.release output.",
        "kernel_version": "Declared base kernel version, without the local release suffix.",
        "rustc_cfg": "include/generated/rustc_cfg output.",
        "rustc_probe": "Action-generated JSON identity for the selected rustc, or None when Rust is disabled.",
    },
)

LinuxObjectInfo = provider(
    doc = "Metadata for one content-addressed Linux object variant.",
    fields = {
        "content_id": "Stable content identity for this object action.",
        "generated_headers": "Depset of generated headers exported by this object.",
        "generated_include_dirs": "Include directories for generated headers exported by this object.",
        "generated_include_dir_anchors": "File-backed references to generated_include_dirs.",
        "mode": "Kbuild mode: y for built-in or m for module.",
        "module_root_kind": "Empty for members/built-ins, or single/composite for an in-tree module root.",
        "object": "Object path relative to the kernel source tree.",
        "objtool_args": "Kbuild target-specific objtool arguments carried to a delayed composite root.",
        "objtool_force": "Whether Kbuild explicitly enables objtool for this object.",
        "output": "Object output file.",
    },
)

LinuxArchiveInfo = provider(
    doc = "Metadata for a native Linux archive-like aggregation action.",
    fields = {
        "kind": "Archive kind, for example built-in.a or module-objects.",
        "objects": "LinuxObjectInfo values included in the archive.",
        "output": "Archive output file.",
    },
)

LinuxGeneratedHeadersInfo = provider(
    doc = "Generated Linux header tree consumed by native compile actions.",
    fields = {
        "arch": "Linux ARCH value used to generate this header tree.",
        "cflags": "Optional response file with generated architecture C flags.",
        "families": "Dictionary of generated-header family names to content-addressed file subsets.",
        "files": "Depset of generated header files.",
        "include_dirs": "Include directories for the generated header tree.",
        "include_dir_anchors": "File-backed references to include_dirs for path-mapped actions.",
        "srcarch": "Linux SRCARCH value used for source include paths.",
        "vdsomunge": "Optional exec-config vdsomunge tool for arm64 compat vDSO generation.",
    },
)

LinuxCompileEnvironmentIndexInfo = provider(
    doc = "Content-addressed Linux compile environments materialized by one index target.",
    fields = {
        "environments": "Dictionary of compile environment IDs to selected config and generated-header values.",
    },
)

LinuxSourceInputIndexInfo = provider(
    doc = "Canonical exact Linux source inputs shared by content-addressed object actions.",
    fields = {
        "file_indices": "Dictionary of source-root-relative paths to one-based file indices.",
        "files": "Canonical list of source files, each labeled exactly once by the index target.",
        "groups": "List of structs containing the depset and encoded membership for one exact input group.",
        "source_tree_info": "Linux source tree used to root and interpret the indexed files.",
    },
)

_LinuxCompactImageShapeInfo = provider(
    doc = "Private ordered content shape for compact image reconstruction.",
    fields = {
        "module_object_content_ids": "Ordered module object content IDs.",
        "object_content_ids": "Ordered built-in object content IDs.",
    },
)

LinuxSourceTreeInfo = provider(
    doc = "Linux source tree root shared by exact source input indexes.",
    fields = {
        "root": "Root marker file for the Linux source tree, usually Kconfig.",
    },
)

LinuxImageInfo = provider(
    doc = "Metadata for a native Linux kernel image output action.",
    fields = {
        "archives": "Archive providers consumed by this output.",
        "module_objects": "Configured module-root LinuxObjectInfo values.",
        "objects": "Object providers consumed by this output.",
        "output": "Kernel image output file.",
    },
)

LinuxModuleObjectsInfo = provider(
    doc = "Configured in-tree module roots kept outside the kernel image graph.",
    fields = {
        "objects": "Ordered configured module-root LinuxObjectInfo values.",
    },
)

def _source_tree_relpath(file, root_dir):
    path = file.short_path
    if root_dir and (path == root_dir or path.startswith(root_dir + "/")):
        return path[len(root_dir):].lstrip("/")
    return path

def _source_tree_root_dir(root):
    if not root:
        return ""
    path = root.short_path
    if "/" not in path:
        return ""
    return path.rsplit("/", 1)[0]

def _linux_source_tree_impl(ctx):
    return [LinuxSourceTreeInfo(
        root = ctx.file.root,
    )]

linux_source_tree = rule(
    implementation = _linux_source_tree_impl,
    attrs = {
        "root": attr.label(
            allow_single_file = True,
            mandatory = True,
            doc = "Root marker file for the Linux source tree, usually Kconfig.",
        ),
    },
    doc = "Provider wrapper for the source root shared by exact Linux source input indexes.",
)

def _positive_decimal(value, context):
    if not value or (len(value) > 1 and value.startswith("0")):
        fail("%s has invalid positive decimal %r" % (context, value))
    result = 0
    for i in range(len(value)):
        char = value[i]
        if char < "0" or char > "9":
            fail("%s has invalid positive decimal %r" % (context, value))
        result = result * 10 + int(char)
    if result <= 0:
        fail("%s has invalid positive decimal %r" % (context, value))
    return result

def _linux_source_input_index_impl(ctx):
    if not ctx.files.srcs:
        fail("linux_source_input_index %s requires non-empty srcs" % ctx.label)
    source_tree_info = ctx.attr.source_tree_info[LinuxSourceTreeInfo]
    source_root = source_tree_info.root
    if not source_root:
        fail("linux_source_input_index %s requires a rooted source_tree_info" % ctx.label)
    root_dir = _source_tree_root_dir(source_root)
    files_by_path = {}
    seen_files = {}
    for file in ctx.files.srcs:
        path = _source_tree_relpath(file, root_dir)
        if not path or path in files_by_path:
            fail("linux_source_input_index %s has duplicate or non-canonical source path %r" % (ctx.label, path))
        if file.path in seen_files:
            fail(
                "linux_source_input_index %s repeats file %s for paths %s and %s" %
                (ctx.label, file.path, seen_files[file.path], path),
            )
        files_by_path[path] = file
        seen_files[file.path] = path

    paths = sorted(files_by_path.keys())
    files = [files_by_path[path] for path in paths]
    file_indices = {}
    for i in range(len(paths)):
        path = paths[i]
        file = files[i]
        file_indices[path] = i + 1

    groups = []
    seen_groups = {}
    previous_group = ""
    for group_number in range(len(ctx.attr.groups)):
        encoded = ctx.attr.groups[group_number]
        if not encoded or (previous_group and previous_group >= encoded):
            fail(
                "linux_source_input_index %s has duplicate or non-canonical group %r" %
                (ctx.label, encoded),
            )
        if encoded in seen_groups:
            fail("linux_source_input_index %s repeats group %r" % (ctx.label, encoded))
        group_files = []
        previous_index = 0
        for value in encoded.split(","):
            index = _positive_decimal(
                value,
                "linux_source_input_index %s group %d" % (ctx.label, group_number + 1),
            )
            if index <= previous_index:
                fail(
                    "linux_source_input_index %s group %d has duplicate or non-canonical file index %d" %
                    (ctx.label, group_number + 1, index),
                )
            if index > len(files):
                fail(
                    "linux_source_input_index %s group %d file index %d is out of range 1..%d" %
                    (ctx.label, group_number + 1, index, len(files)),
                )
            group_files.append(files[index - 1])
            previous_index = index
        groups.append(struct(
            encoded_membership = "," + encoded + ",",
            files = depset(group_files),
        ))
        seen_groups[encoded] = True
        previous_group = encoded
    if not groups:
        fail("linux_source_input_index %s requires at least one source input group" % ctx.label)
    return [
        DefaultInfo(files = depset(files)),
        LinuxSourceInputIndexInfo(
            file_indices = file_indices,
            files = files,
            groups = groups,
            source_tree_info = source_tree_info,
        ),
    ]

linux_source_input_index = rule(
    implementation = _linux_source_input_index_impl,
    attrs = {
        "groups": attr.string_list(
            mandatory = True,
            doc = "Canonical comma-separated one-based file indices for each exact input group.",
        ),
        "srcs": attr.label_list(
            allow_files = True,
            mandatory = True,
            doc = "Exact source files, canonicalized by source-root-relative path during analysis.",
        ),
        "source_tree_info": attr.label(
            mandatory = True,
            providers = [LinuxSourceTreeInfo],
            doc = "Linux source tree used to derive canonical paths for srcs.",
        ),
    },
    doc = "Indexes exact source files into shared depsets selected by content-addressed Linux actions.",
)

def _cc_feature_configuration(ctx, cc_toolchain):
    return cc_common.configure_features(
        ctx = ctx,
        cc_toolchain = cc_toolchain,
        requested_features = ctx.features,
        unsupported_features = ctx.disabled_features,
    )

def _cc_compile_flags(ctx, cc_toolchain, feature_configuration):
    variables = cc_common.create_compile_variables(
        feature_configuration = feature_configuration,
        cc_toolchain = cc_toolchain,
        user_compile_flags = ctx.fragments.cpp.copts + ctx.fragments.cpp.conlyopts,
    )
    return cc_common.get_memory_inefficient_command_line(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
        variables = variables,
    )

def _linux_compile_flags(ctx, cc_toolchain, feature_configuration):
    flags = _cc_compile_flags(ctx, cc_toolchain, feature_configuration)
    flags = _linux_rewrite_target_flags(flags, _linux_kbuild_target_triple(ctx))
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
            if index + 1 < len(flags) and _linux_drop_toolchain_include(flags[index + 1]):
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
        if (
            flag.startswith("--sysroot=")
        ):
            continue
        if flag.startswith("-I") and _linux_drop_toolchain_include(flag[len("-I"):]):
            continue
        if flag.startswith("-iquote") and _linux_drop_toolchain_include(flag[len("-iquote"):]):
            continue
        if flag.startswith("-isystem") and _linux_drop_toolchain_include(flag[len("-isystem"):]):
            continue
        out.append(flag)
    if "-nostdinc" not in out:
        out.append("-nostdinc")
    if "-fintegrated-as" not in out:
        out.append("-fintegrated-as")
    return out

def _linux_kbuild_target_triple(ctx):
    arch = ctx.attr.arch
    if arch == "arm64":
        return "aarch64-linux-gnu"
    if arch == "x86":
        return "x86_64-linux-gnu"
    return ""

def _linux_rewrite_target_flags(flags, target_triple):
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

def _linux_drop_toolchain_include(path):
    return (
        "llvm++musl+musl_libc/" in path or
        "llvm++musl+musl_libc\\" in path or
        "llvm++kernel_headers+linux_kernel_headers_" in path
    )

def _cc_target_flags(ctx, cc_toolchain, feature_configuration):
    flags = _linux_compile_flags(ctx, cc_toolchain, feature_configuration)
    target_flags = []
    for index in range(len(flags)):
        flag = flags[index]
        if flag in ["-target", "--target"]:
            if index + 1 < len(flags):
                target_flags.append(flag)
                target_flags.append(flags[index + 1])
        elif flag.startswith("-target=") or flag.startswith("--target="):
            target_flags.append(flag)
    return target_flags

def _linux_compile_flags_without_target(ctx, cc_toolchain, feature_configuration):
    flags = _linux_compile_flags(ctx, cc_toolchain, feature_configuration)
    out = []
    skip_next = False
    for index in range(len(flags)):
        if skip_next:
            skip_next = False
            continue
        flag = flags[index]
        if flag in ["-target", "--target"]:
            skip_next = index + 1 < len(flags)
            continue
        if flag.startswith("-target=") or flag.startswith("--target="):
            continue
        out.append(flag)
    return out

def _linux_cc_context_impl(ctx):
    cc_toolchain = find_cpp_toolchain(ctx)
    feature_configuration = _cc_feature_configuration(ctx, cc_toolchain)
    flags = _linux_compile_flags(ctx, cc_toolchain, feature_configuration)
    out = ctx.actions.declare_file(ctx.label.name + ".linux_cc_context.txt")
    lines = [
        "arch=%s" % ctx.attr.arch,
        "srcarch=%s" % ctx.attr.srcarch,
        "target_triple=%s" % ctx.attr.target_triple,
        "image_format=%s" % ctx.attr.image_format,
    ]
    for flag in flags:
        lines.append("compile_flag=%s" % flag)
    ctx.actions.write(out, "\n".join(lines) + "\n")
    return [
        DefaultInfo(files = depset([out])),
        LinuxCcContextInfo(
            arch = ctx.attr.arch,
            compile_flags = list(flags),
            image_format = ctx.attr.image_format,
            srcarch = ctx.attr.srcarch,
            target_triple = ctx.attr.target_triple,
        ),
    ]

linux_cc_context = rule(
    implementation = _linux_cc_context_impl,
    attrs = {
        "arch": attr.string(default = "x86"),
        "image_format": attr.string(default = "bzImage"),
        "srcarch": attr.string(default = "x86"),
        "target_triple": attr.string(),
    },
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Rules_cc-backed Linux toolchain context metadata.",
)

def _config_flags(ctx):
    flags = dict(ctx.attr.config_flags)
    if ctx.attr.config and KconfigInfo in ctx.attr.config:
        flags.update(ctx.attr.config[KconfigInfo].config_flags)
    return flags

def _config_value_to_header_suffix(key, value):
    if value == "y":
        return "#define %s 1" % key
    if value == "m":
        return "#define %s_MODULE 1" % key
    if value == "n":
        return None
    return "#define %s %s" % (key, value)

def _unquote(value):
    if len(value) >= 2 and value[0] == "\"" and value[-1] == "\"":
        return value[1:-1]
    return value

def _linux_generated_include_groups(include_dirs, srcarch):
    arch_generated = []
    arch_generated_uapi = []
    generic_generated = []
    generic_generated_uapi = []
    other = []

    arch_generated_suffix = "/arch/%s/include/generated" % srcarch
    arch_generated_uapi_suffix = arch_generated_suffix + "/uapi"
    for include_dir in include_dirs:
        if include_dir.endswith(arch_generated_uapi_suffix):
            arch_generated_uapi.append(include_dir)
        elif include_dir.endswith(arch_generated_suffix):
            arch_generated.append(include_dir)
        elif include_dir.endswith("/include/generated/uapi"):
            generic_generated_uapi.append(include_dir)
        elif include_dir.endswith("/include"):
            generic_generated.append(include_dir)
        else:
            other.append(include_dir)

    return struct(
        arch_generated = arch_generated,
        arch_generated_uapi = arch_generated_uapi,
        generic_generated = generic_generated,
        generic_generated_uapi = generic_generated_uapi,
        other = other,
    )

def _linux_ordered_include_dirs(source_root, srcarch = "x86", generated_include_dirs = []):
    if not source_root:
        return []
    generated = _linux_generated_include_groups(generated_include_dirs, srcarch)
    return (
        [source_root + "/arch/" + srcarch + "/include"] +
        generated.arch_generated +
        [source_root + "/include"] +
        generated.generic_generated +
        [source_root + "/arch/" + srcarch + "/include/uapi"] +
        generated.arch_generated_uapi +
        [source_root + "/include/uapi"] +
        generated.generic_generated_uapi +
        generated.other
    )

def _linux_include_flags_for_dirs(include_dirs):
    return ["-I" + include_dir for include_dir in include_dirs]

def _directory_anchors(files, directories):
    anchors = {}
    for directory in directories:
        for file in files:
            if file.path.startswith(directory + "/"):
                anchors[directory] = directory_anchor(file, directory)
                break
        if directory not in anchors:
            fail("no generated file anchors include directory %s" % directory)
    return anchors

def _available_directory_anchors(files, directories):
    anchors = {}
    for directory in directories:
        for file in files:
            if file.path.startswith(directory + "/"):
                anchors[directory] = directory_anchor(file, directory)
                break
    return anchors

def _add_directory_flags(args, directories, anchors = {}, format = "-I%s"):
    for directory in directories:
        anchor = anchors.get(directory)
        if anchor == None:
            args.add(format % directory)
        else:
            add_directory_arg(args, anchor, format = format)

def _config_include_dir_anchor(config):
    return config.include_dir_anchor

def _generated_include_dir_anchors(generated_headers):
    return generated_headers.include_dir_anchors if generated_headers != None else {}

def _add_config_include_flag(args, config):
    anchor = _config_include_dir_anchor(config)
    if anchor == None:
        args.add("-I" + config.include_dir)
    else:
        add_directory_arg(args, anchor, format = "-I%s")

def _add_linux_source_include_flags_for_root(args, source_root, srcarch = "x86", generated_include_dirs = [], generated_include_dir_anchors = {}):
    _add_directory_flags(
        args,
        _linux_ordered_include_dirs(source_root, srcarch, generated_include_dirs),
        generated_include_dir_anchors,
    )

def _linux_source_include_flags_for_root(source_root, srcarch = "x86", generated_include_dirs = []):
    return _linux_include_flags_for_dirs(_linux_ordered_include_dirs(source_root, srcarch, generated_include_dirs))

def _add_linux_source_include_flags(ctx, args, generated_headers = None):
    generated_include_dirs = []
    if generated_headers != None:
        generated_include_dirs = generated_headers.include_dirs
    source_root = _linux_source_root_file(ctx)
    _add_linux_source_include_flags_for_root(
        args,
        source_root.dirname if source_root else "",
        _linux_rule_srcarch(ctx, generated_headers),
        generated_include_dirs,
        _generated_include_dir_anchors(generated_headers),
    )

def _linux_rule_srcarch(ctx, generated_headers = None):
    if ctx.attr.srcarch:
        return ctx.attr.srcarch
    if generated_headers != None and generated_headers.srcarch:
        return generated_headers.srcarch
    return "x86"

def _linux_rule_arch(ctx, generated_headers = None):
    if ctx.attr.arch:
        return ctx.attr.arch
    if generated_headers != None and generated_headers.arch:
        return generated_headers.arch
    return "x86"

def _linux_source_preinclude_flags_for_root(source_root, assembly = False):
    if not source_root:
        return []
    if assembly:
        return [
            "-D__KERNEL__",
            "-D__ASSEMBLY__",
            "-include",
            source_root + "/include/linux/compiler-version.h",
            "-include",
            source_root + "/include/linux/kconfig.h",
        ]
    return [
        "-D__KERNEL__",
        "-include",
        source_root + "/include/linux/compiler-version.h",
        "-include",
        source_root + "/include/linux/kconfig.h",
        "-include",
        source_root + "/include/linux/compiler_types.h",
    ]

def _is_assembly_source(src):
    return src != None and (src.basename.endswith(".S") or src.basename.endswith(".s"))

def _is_shipped_c_source(src):
    return src != None and src.basename.endswith(".c_shipped")

def _is_dtb_source(src):
    return src != None and (src.basename.endswith(".dts") or src.basename.endswith(".dtso"))

def _linux_compile_object_name(object):
    if object.endswith(".pi.o"):
        return object[:-len(".pi.o")] + ".o"
    if object.endswith(".stub.o"):
        return object[:-len(".stub.o")] + ".o"
    return object

def _linux_objcopy_flags_for_object(object, arch = ""):
    if object.endswith(".pi.o"):
        flags = [
            "--prefix-symbols=__pi_",
            "--remove-section=.note.gnu.property",
        ]
        if object.split("/")[-1].startswith("lib-"):
            flags.append("--prefix-alloc-sections=.init")
        return flags
    if object.endswith(".stub.o"):
        flags = ["--remove-section=.note.gnu.property"]
        if object.startswith("drivers/firmware/efi/libstub/"):
            if arch in ["arm64", "riscv", "loongarch"]:
                flags.extend([
                    "--prefix-alloc-sections=.init",
                    "--prefix-symbols=__efistub_",
                ])
            elif arch == "arm":
                flags.extend([
                    "--rename-section",
                    ".data=.data.efistub",
                    "--rename-section",
                    ".bss=.bss.efistub,load,alloc",
                ])
        return flags
    return []

def _linux_object_needs_relacheck(object):
    return object.startswith("arch/arm64/") and object.endswith(".pi.o")

def _linux_ftrace_remove_flags():
    return [
        "-pg",
        "-mrecord-mcount",
        "-mnop-mcount",
        "-mfentry",
        "-fpatchable-function-entry=4,2",
        "-fpatchable-function-entry=2",
    ]

def _linux_perlasm_kind(object):
    if object in [
        "lib/crypto/arm64/poly1305-core.o",
        "lib/crypto/arm64/sha256-core.o",
        "lib/crypto/arm64/sha512-core.o",
    ]:
        return "arm64_with_args"
    if object == "lib/crypto/x86/poly1305-x86_64-cryptogams.o":
        return "stdout"
    return ""

def _linux_dtb_symbol_base(src):
    basename = src.basename
    if basename.endswith(".dts"):
        basename = basename[:-len(".dts")]
        suffix = "dtb"
    elif basename.endswith(".dtso"):
        basename = basename[:-len(".dtso")]
        suffix = "dtbo"
    else:
        fail("expected .dts or .dtso source, got %s" % src.basename)
    return "__%s_%s" % (suffix, _linux_name_fix(basename))

def _linux_dtb_section(object, srcarch):
    if object.startswith("arch/%s/boot/dts/" % srcarch):
        return ".dtb.init.rodata"
    return ".rodata"

def _unique_strings(values):
    seen = {}
    out = []
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

def _single_file(target, attr_name):
    files = target.files.to_list()
    if len(files) != 1:
        fail("%s entry %s must provide exactly one file, got %d" % (attr_name, target.label, len(files)))
    return files[0]

def _linux_config_cflags(config):
    if config:
        return [config.cflags]
    return []

def _linux_config_aflags(config):
    if config:
        return [config.aflags]
    return []

def _linux_config_flags_for_source(config, src):
    if _is_assembly_source(src):
        return _linux_config_aflags(config)
    return _linux_config_cflags(config)

def _linux_generated_header_cflags(generated_headers):
    if generated_headers != None and generated_headers.cflags != None:
        return [generated_headers.cflags]
    return []

def _linux_filtered_config_flags_for_source(ctx, config, src, remove_flags, out_suffix = "filtered"):
    if not config:
        return struct(flags = [], inputs = [])
    base = config.aflags if _is_assembly_source(src) else config.cflags
    if not remove_flags:
        return struct(flags = [base], inputs = [])

    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + base.basename + "." + out_suffix + ".rsp")
    args = ctx.actions.args()
    args.add("-in", base)
    args.add("-out", out)
    args.add_all(remove_flags, before_each = "-remove")
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._flagfilter,
        inputs = [base],
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxFlagFilter",
        progress_message = "Filtering Linux compiler flags %{label}",
    )
    return struct(flags = [out], inputs = [out])

def _linux_non_lto_config_flags_for_source(ctx, config, src, out_suffix = "nolto"):
    if not config:
        return struct(flags = [], inputs = [])
    if config.config_flags.get("CONFIG_LTO_CLANG_THIN") != "y" and config.config_flags.get("CONFIG_LTO_CLANG_FULL") != "y":
        return struct(flags = _linux_config_flags_for_source(config, src), inputs = [])
    return _linux_filtered_config_flags_for_source(
        ctx,
        config,
        src,
        ["-flto=thin", "-flto", "-fsplit-lto-unit", "-fvisibility=hidden"],
        out_suffix = out_suffix,
    )

def _linux_object_directory(object):
    if "/" not in object:
        return ""
    return object.rsplit("/", 1)[0]

def _linux_source_tree_info(ctx):
    if hasattr(ctx.attr, "source_input_index"):
        return ctx.attr.source_input_index[LinuxSourceInputIndexInfo].source_tree_info
    return None

def _linux_source_root_file(ctx):
    info = _linux_source_tree_info(ctx)
    if info:
        return info.root
    if hasattr(ctx.file, "source_root"):
        return ctx.file.source_root
    fail("%s requires source_tree_info for source-backed Linux actions" % ctx.label)

def _linux_execroot_path(file):
    path = file.short_path.replace("\\", "/")
    if path.startswith("../"):
        return "external/" + path[3:]
    return path

def _linux_execroot_dir(file):
    path = _linux_execroot_path(file)
    return path.rsplit("/", 1)[0] if "/" in path else ""

def _linux_source_root_path(ctx):
    root = _linux_source_root_file(ctx)
    if not root:
        return ""
    return _linux_execroot_dir(root)

def _linux_source_input_group(ctx, rule_name):
    index = ctx.attr.source_input_index[LinuxSourceInputIndexInfo]
    group_number = ctx.attr.source_input_group
    if group_number <= 0 or group_number > len(index.groups):
        fail(
            "%s %s source_input_group %d is out of range 1..%d" %
            (rule_name, ctx.label, group_number, len(index.groups)),
        )
    return struct(
        index = index,
        value = index.groups[group_number - 1],
    )

def _optional_linux_source_input_group(ctx, rule_name):
    if not hasattr(ctx.attr, "source_input_index"):
        return None
    return _linux_source_input_group(ctx, rule_name)

def _linux_source_input_file(_ctx, selection, file_number, context):
    if selection == None:
        fail("%s requires source_input_index" % context)
    if file_number <= 0 or file_number > len(selection.index.files):
        fail(
            "%s source_input_file %d is out of range 1..%d" %
            (context, file_number, len(selection.index.files)),
        )
    if (",%d," % file_number) not in selection.value.encoded_membership:
        fail(
            "%s source input group omits source file index %d" %
            (context, file_number),
        )
    return selection.index.files[file_number - 1]

def _linux_source_input_file_for_path(ctx, relpath):
    selection = _optional_linux_source_input_group(ctx, "Linux source lookup")
    if selection == None:
        return None
    file_number = selection.index.file_indices.get(relpath, 0)
    if not file_number:
        fail("source input index for %s has no file %s" % (ctx.label, relpath))
    return _linux_source_input_file(
        ctx,
        selection,
        file_number,
        "source lookup %s for %s" % (relpath, ctx.label),
    )

def _linux_source_tree_files(ctx):
    files = []
    if hasattr(ctx.files, "source_tree"):
        files.extend(ctx.files.source_tree)
    return files

def _linux_source_tree_inputs(ctx, direct = []):
    root = _linux_source_root_file(ctx)
    inputs = list(direct)
    if root:
        inputs.append(root)
    if hasattr(ctx.files, "source_tree"):
        inputs.extend(ctx.files.source_tree)
    return inputs

def _linux_action_source_inputs(ctx, rule_name, direct = []):
    selection = _optional_linux_source_input_group(ctx, rule_name)
    if selection != None:
        return struct(
            direct = list(direct),
            transitive = [selection.value.files],
        )
    return struct(
        direct = _linux_source_tree_inputs(ctx, direct = direct),
        transitive = [],
    )

def _linux_source_tree_relpath_from_ctx(ctx, file):
    root = _linux_source_root_file(ctx)
    return _source_tree_relpath(file, _source_tree_root_dir(root))

def _linux_object_compile_source_tree_inputs(ctx, direct = []):
    selection = _linux_source_input_group(ctx, "linux_object")
    return struct(
        direct = list(direct),
        transitive = [selection.value.files],
    )

def _source_tree_file(ctx, relpath):
    indexed = _linux_source_input_file_for_path(ctx, relpath)
    if indexed != None:
        return indexed
    suffix = "/" + relpath
    for file in _linux_source_tree_files(ctx):
        if file.short_path == relpath or file.short_path.endswith(suffix):
            return file
    fail("source tree input %s required by %s was not found" % (relpath, ctx.label))

def _source_tree_file_for_root(ctx, source_root, relpath):
    indexed = _linux_source_input_file_for_path(ctx, relpath)
    if indexed != None:
        return indexed
    path = source_root + "/" + relpath
    for file in _linux_source_tree_files(ctx):
        if _linux_execroot_path(file) == path or file.short_path == path:
            return file
    return _source_tree_file(ctx, relpath)

def _copy_source_tree_file(ctx, out_relpath, in_relpath):
    src = _source_tree_file(ctx, in_relpath)
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + out_relpath)
    ctx.actions.expand_template(
        template = src,
        output = out,
        substitutions = {},
    )
    return out

def _linux_purgatory_compile(ctx, compiler, cc_toolchain, feature_configuration, config, generated_headers, source_root, src, out_relpath, extra_flags = []):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + out_relpath)
    args = ctx.actions.args()
    args.add_all(_linux_compile_flags(ctx, cc_toolchain, feature_configuration))
    args.add_all([
        "-ffreestanding",
        "-fno-builtin",
        "-fno-stack-protector",
        "-fno-zero-initialized-in-bss",
        "-fpic",
        "-fvisibility=hidden",
        "-g0",
        "-mcmodel=small",
        "-Wno-unused-command-line-argument",
        "-DDISABLE_BRANCH_PROFILING",
    ])
    args.add_all(_linux_object_name_flags(out_relpath))
    args.add_all(extra_flags)
    args.add_all(_linux_config_flags_for_source(config, src), format_each = "@%s")
    args.add_all([
        "-mcmodel=small",
        "-fno-stack-protector",
        "-fpic",
        "-fvisibility=hidden",
    ])
    args.add_all(_linux_source_preinclude_flags_for_root(source_root, _is_assembly_source(src)))
    if config:
        _add_config_include_flag(args, config)
    add_directory_arg(args, directory_anchor(src), format = "-I%s")
    _add_linux_source_include_flags(ctx, args, generated_headers)
    args.add("-c")
    args.add(src)
    args.add("-o")
    args.add(out)

    source_inputs = _linux_object_compile_source_tree_inputs(ctx, direct = [src])
    transitive_inputs = [cc_toolchain.all_files]
    if config:
        transitive_inputs.append(config.files)
    if generated_headers:
        transitive_inputs.append(generated_headers.files)
    path_mapped_run(
        ctx.actions,
        executable = compiler,
        inputs = depset(source_inputs.direct, transitive = source_inputs.transitive + transitive_inputs),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxPurgatoryCompile",
        progress_message = "Compiling Linux purgatory object %{label}",
    )
    return out

def _linux_purgatory_link(ctx, linker, cc_toolchain, objects, out_relpath, relocatable):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + out_relpath)
    args = ctx.actions.args()
    args.add("-fuse-ld=lld")
    args.add("-no-pie")
    args.add("-nostdlib")
    if relocatable:
        args.add("-r")
    args.add("-Wl,-e,purgatory_start")
    args.add("-Wl,-z,nodefaultlib")
    args.add("-o")
    args.add(out)
    args.add_all(objects)
    path_mapped_run(
        ctx.actions,
        executable = linker,
        inputs = depset(objects, transitive = [cc_toolchain.all_files]),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxPurgatoryLink",
        progress_message = "Linking Linux purgatory binary %{label}",
    )
    return out

def _linux_purgatory_outputs(ctx, compiler, linker, cc_toolchain, feature_configuration, config, generated_headers, source_root):
    sources = [
        ("arch/x86/purgatory/purgatory.c", "arch/x86/purgatory/purgatory.o", []),
        ("arch/x86/purgatory/stack.S", "arch/x86/purgatory/stack.o", []),
        ("arch/x86/purgatory/setup-x86_64.S", "arch/x86/purgatory/setup-x86_64.o", []),
        ("arch/x86/purgatory/entry64.S", "arch/x86/purgatory/entry64.o", []),
        ("arch/x86/boot/compressed/string.c", "arch/x86/purgatory/string.o", []),
        ("lib/crypto/sha256.c", "arch/x86/purgatory/sha256.o", ["-D__DISABLE_EXPORTS", "-D__NO_FORTIFY"]),
    ]
    objects = []
    for src_relpath, out_relpath, extra_flags in sources:
        objects.append(_linux_purgatory_compile(
            ctx,
            compiler,
            cc_toolchain,
            feature_configuration,
            config,
            generated_headers,
            source_root,
            _source_tree_file(ctx, src_relpath),
            out_relpath,
            extra_flags,
        ))
    ro = _linux_purgatory_link(ctx, linker, cc_toolchain, objects, "arch/x86/purgatory/purgatory.ro", True)
    chk = _linux_purgatory_link(ctx, linker, cc_toolchain, [ro], "arch/x86/purgatory/purgatory.chk", False)
    return struct(check = chk, ro = ro)

def _linux_realmode_compile(ctx, compiler, cc_toolchain, config, generated_headers, source_root, src, out_relpath):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + out_relpath)
    assembly = _is_assembly_source(src)
    args = ctx.actions.args()
    args.add_all(_linux_compile_flags(ctx, cc_toolchain, _cc_feature_configuration(ctx, cc_toolchain)))
    args.add_all([
        "-std=gnu11",
        "-m16",
        "-g",
        "-Os",
        "-DDISABLE_BRANCH_PROFILING",
        "-D__DISABLE_EXPORTS",
        "-Wall",
        "-Wstrict-prototypes",
        "-march=i386",
        "-mregparm=3",
        "-fno-strict-aliasing",
        "-fomit-frame-pointer",
        "-fno-pic",
        "-mno-mmx",
        "-mno-sse",
        "-fcf-protection=none",
        "-ffreestanding",
        "-fno-stack-protector",
        "-Wno-address-of-packed-member",
        "-mstack-alignment=4",
        "-Wno-gnu",
        "-D_SETUP",
        "-D_WAKEUP",
        "-fno-asynchronous-unwind-tables",
    ])
    if assembly:
        args.add("-D__ASSEMBLY__")
    args.add_all(_linux_source_preinclude_flags_for_root(source_root, assembly))
    if config:
        _add_config_include_flag(args, config)
    add_directory_arg(args, directory_anchor(src), format = "-I%s")
    args.add("-I" + source_root + "/arch/x86/realmode/rm")
    args.add("-I" + source_root + "/arch/x86/boot")
    _add_linux_source_include_flags(ctx, args, generated_headers)
    args.add("-c")
    args.add(src)
    args.add("-o")
    args.add(out)

    source_inputs = _linux_object_compile_source_tree_inputs(ctx, direct = [src])
    transitive_inputs = [cc_toolchain.all_files]
    if config:
        transitive_inputs.append(config.files)
    if generated_headers:
        transitive_inputs.append(generated_headers.files)
    path_mapped_run(
        ctx.actions,
        executable = compiler,
        inputs = depset(source_inputs.direct, transitive = source_inputs.transitive + transitive_inputs),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxRealmodeCompile",
        progress_message = "Compiling Linux x86 realmode object %{label}",
    )
    return out

def _linux_realmode_pasyms(ctx, objects):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/realmode/rm/pasyms.h")
    args = ctx.actions.args()
    args.add("-nm", ctx.executable._llvm_nm)
    args.add("-out", out)
    args.add_all(objects)
    path_mapped_run(
        ctx.actions,
        executable = ctx.attr._pasyms[DefaultInfo].files_to_run,
        inputs = objects,
        tools = [ctx.attr._llvm_nm[DefaultInfo].files_to_run],
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxRealmodePASYMS",
        progress_message = "Generating Linux x86 realmode pasyms %{label}",
    )
    return out

def _linux_realmode_linker_script(ctx, compiler, cc_toolchain, config, generated_headers, source_root, pasyms):
    src = _source_tree_file(ctx, "arch/x86/realmode/rm/realmode.lds.S")
    out = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/realmode/rm/realmode.lds")
    args = ctx.actions.args()
    args.add_all(_linux_compile_flags(ctx, cc_toolchain, _cc_feature_configuration(ctx, cc_toolchain)))
    args.add_all([
        "-E",
        "-P",
        "-Ux86",
        "-Ux86_64",
        "-D__KERNEL__",
        "-D__ASSEMBLY__",
        "-DLINKER_SCRIPT",
        "-include",
        source_root + "/include/linux/kconfig.h",
    ])
    if config:
        _add_config_include_flag(args, config)
    add_directory_arg(args, directory_anchor(pasyms), format = "-I%s")
    _add_linux_source_include_flags(ctx, args, generated_headers)
    args.add(src)
    args.add("-o")
    args.add(out)

    transitive_inputs = [cc_toolchain.all_files]
    if config:
        transitive_inputs.append(config.files)
    if generated_headers:
        transitive_inputs.append(generated_headers.files)
    source_inputs = _linux_object_compile_source_tree_inputs(ctx, direct = [src, pasyms])
    path_mapped_run(
        ctx.actions,
        executable = compiler,
        inputs = depset(
            direct = source_inputs.direct,
            transitive = source_inputs.transitive + transitive_inputs,
        ),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxRealmodeLinkerScript",
        progress_message = "Preprocessing Linux x86 realmode linker script %{label}",
    )
    return out

def _linux_realmode_link(ctx, linker, cc_toolchain, objects, linker_script):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/realmode/rm/realmode.elf")
    ld = _linux_x86_tool_sibling(linker, "ld.lld")
    args = ctx.actions.args()
    args.add_all([
        "-m",
        "elf_i386",
        "--emit-relocs",
        "-T",
        linker_script,
    ])
    args.add("-o")
    args.add(out)
    args.add_all(objects)
    path_mapped_run(
        ctx.actions,
        executable = ld,
        inputs = depset(objects + [linker_script], transitive = [cc_toolchain.all_files]),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxRealmodeLink",
        progress_message = "Linking Linux x86 realmode ELF %{label}",
    )
    return out

def _linux_realmode_outputs(ctx, compiler, linker, cc_toolchain, config, generated_headers, source_root):
    source_specs = [
        ("arch/x86/realmode/rm/header.S", "arch/x86/realmode/rm/header.o"),
        ("arch/x86/realmode/rm/trampoline_64.S", "arch/x86/realmode/rm/trampoline_64.o"),
        ("arch/x86/realmode/rm/stack.S", "arch/x86/realmode/rm/stack.o"),
        ("arch/x86/realmode/rm/reboot.S", "arch/x86/realmode/rm/reboot.o"),
        ("arch/x86/realmode/rm/wakeup_asm.S", "arch/x86/realmode/rm/wakeup_asm.o"),
        ("arch/x86/realmode/rm/wakemain.c", "arch/x86/realmode/rm/wakemain.o"),
        ("arch/x86/realmode/rm/video-mode.c", "arch/x86/realmode/rm/video-mode.o"),
        ("arch/x86/realmode/rm/copy.S", "arch/x86/realmode/rm/copy.o"),
        ("arch/x86/realmode/rm/bioscall.S", "arch/x86/realmode/rm/bioscall.o"),
        ("arch/x86/realmode/rm/regs.c", "arch/x86/realmode/rm/regs.o"),
        ("arch/x86/realmode/rm/video-vga.c", "arch/x86/realmode/rm/video-vga.o"),
        ("arch/x86/realmode/rm/video-vesa.c", "arch/x86/realmode/rm/video-vesa.o"),
        ("arch/x86/realmode/rm/video-bios.c", "arch/x86/realmode/rm/video-bios.o"),
    ]
    objects = []
    for src_relpath, out_relpath in source_specs:
        objects.append(_linux_realmode_compile(
            ctx,
            compiler,
            cc_toolchain,
            config,
            generated_headers,
            source_root,
            _source_tree_file(ctx, src_relpath),
            out_relpath,
        ))

    pasyms = _linux_realmode_pasyms(ctx, objects)
    linker_script = _linux_realmode_linker_script(ctx, compiler, cc_toolchain, config, generated_headers, source_root, pasyms)
    elf = _linux_realmode_link(ctx, linker, cc_toolchain, objects, linker_script)

    bin = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/realmode/rm/realmode.bin")
    objcopy_args = ctx.actions.args()
    objcopy_args.add("-O")
    objcopy_args.add("binary")
    objcopy_args.add(elf)
    objcopy_args.add(bin)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._llvm_objcopy,
        inputs = [elf, ctx.executable._llvm_objcopy],
        outputs = [bin],
        arguments = [objcopy_args],
        mnemonic = "LinuxRealmodeObjcopy",
        progress_message = "Generating Linux x86 realmode binary %{label}",
    )

    relocs = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/realmode/rm/realmode.relocs")
    relocs_args = ctx.actions.args()
    relocs_args.add("-in", elf)
    relocs_args.add("-out", relocs)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._realmoderelocs,
        inputs = [elf],
        outputs = [relocs],
        arguments = [relocs_args],
        mnemonic = "LinuxRealmodeRelocs",
        progress_message = "Generating Linux x86 realmode relocations %{label}",
    )
    return struct(bin = bin, relocs = relocs)

def _linux_vdso_compile(ctx, compiler, cc_toolchain, feature_configuration, config, generated_headers, source_root, src, out_relpath):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + out_relpath)
    assembly = _is_assembly_source(src)
    args = ctx.actions.args()
    args.add_all(_linux_compile_flags(ctx, cc_toolchain, feature_configuration))
    args.add_all([
        "-mcmodel=small",
        "-fPIC",
        "-O2",
        "-fasynchronous-unwind-tables",
        "-m64",
        "-fno-stack-protector",
        "-fno-omit-frame-pointer",
        "-foptimize-sibling-calls",
        "-DDISABLE_BRANCH_PROFILING",
        "-DBUILD_VDSO",
        "-Wno-unused-command-line-argument",
    ])
    args.add_all(_linux_source_preinclude_flags_for_root(source_root, assembly))
    if config:
        _add_config_include_flag(args, config)
    args.add("-I" + source_root + "/arch/x86/entry/vdso")
    _add_linux_source_include_flags(ctx, args, generated_headers)
    args.add("-c")
    args.add(src)
    args.add("-o")
    args.add(out)

    source_inputs = _linux_object_compile_source_tree_inputs(ctx, direct = [src])
    transitive_inputs = [cc_toolchain.all_files]
    if config:
        transitive_inputs.append(config.files)
    if generated_headers:
        transitive_inputs.append(generated_headers.files)
    path_mapped_run(
        ctx.actions,
        executable = compiler,
        inputs = depset(source_inputs.direct, transitive = source_inputs.transitive + transitive_inputs),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxVDSOCompile",
        progress_message = "Compiling Linux x86 vDSO object %{label}",
    )
    return out

def _linux_vdso_linker_script(ctx, compiler, cc_toolchain, feature_configuration, config, generated_headers, source_root):
    src = _source_tree_file(ctx, "arch/x86/entry/vdso/vdso.lds.S")
    out = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/entry/vdso/vdso.lds")
    args = ctx.actions.args()
    args.add_all(_linux_compile_flags(ctx, cc_toolchain, feature_configuration))
    args.add_all(_linux_config_cflags(config), format_each = "@%s")
    args.add_all(_linux_generated_header_cflags(generated_headers), format_each = "@%s")
    args.add_all([
        "-E",
        "-P",
        "-Ux86",
        "-Ux86_64",
        "-D__KERNEL__",
        "-D__ASSEMBLY__",
        "-DLINKER_SCRIPT",
        "-include",
        source_root + "/include/linux/kconfig.h",
    ])
    if config:
        _add_config_include_flag(args, config)
    args.add("-I" + source_root + "/arch/x86/entry/vdso")
    _add_linux_source_include_flags(ctx, args, generated_headers)
    args.add(src)
    args.add("-o")
    args.add(out)

    transitive_inputs = [cc_toolchain.all_files]
    if config:
        transitive_inputs.append(config.files)
    if generated_headers:
        transitive_inputs.append(generated_headers.files)
    source_inputs = _linux_object_compile_source_tree_inputs(ctx, direct = [src])
    path_mapped_run(
        ctx.actions,
        executable = compiler,
        inputs = depset(
            direct = source_inputs.direct,
            transitive = source_inputs.transitive + transitive_inputs,
        ),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxVDSOLinkerScript",
        progress_message = "Preprocessing Linux x86 vDSO linker script %{label}",
    )
    return out

def _linux_vdso_link(ctx, linker, cc_toolchain, objects, linker_script):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/entry/vdso/vdso64.so.dbg")
    args = ctx.actions.args()
    args.add("-fuse-ld=lld")
    args.add("-nostdlib")
    args.add("-shared")
    args.add("-Wl,--hash-style=both")
    args.add("-Wl,--build-id=sha1")
    args.add("-Wl,--no-undefined")
    args.add("-Wl,--eh-frame-hdr")
    args.add("-Wl,-Bsymbolic")
    args.add("-Wl,-z,noexecstack")
    args.add("-Wl,-m,elf_x86_64")
    args.add("-Wl,-soname,linux-vdso.so.1")
    args.add("-Wl,-z,max-page-size=4096")
    args.add(linker_script, format = "-Wl,-T,%s")
    args.add("-o")
    args.add(out)
    args.add_all(objects)
    path_mapped_run(
        ctx.actions,
        executable = linker,
        inputs = depset(objects + [linker_script], transitive = [cc_toolchain.all_files]),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxVDSOLink",
        progress_message = "Linking Linux x86 vDSO %{label}",
    )
    return out

def _linux_vdso_image_source(ctx, compiler, linker, cc_toolchain, feature_configuration, config, generated_headers, source_root):
    source_specs = [
        ("arch/x86/entry/vdso/vdso-note.S", "arch/x86/entry/vdso/vdso-note.o"),
        ("arch/x86/entry/vdso/vclock_gettime.c", "arch/x86/entry/vdso/vclock_gettime.o"),
        ("arch/x86/entry/vdso/vgetcpu.c", "arch/x86/entry/vdso/vgetcpu.o"),
        ("arch/x86/entry/vdso/vgetrandom.c", "arch/x86/entry/vdso/vgetrandom.o"),
        ("arch/x86/entry/vdso/vgetrandom-chacha.S", "arch/x86/entry/vdso/vgetrandom-chacha.o"),
    ]
    objects = []
    for src_relpath, out_relpath in source_specs:
        objects.append(_linux_vdso_compile(
            ctx,
            compiler,
            cc_toolchain,
            feature_configuration,
            config,
            generated_headers,
            source_root,
            _source_tree_file(ctx, src_relpath),
            out_relpath,
        ))
    linker_script = _linux_vdso_linker_script(ctx, compiler, cc_toolchain, feature_configuration, config, generated_headers, source_root)
    dbg = _linux_vdso_link(ctx, linker, cc_toolchain, objects, linker_script)

    stripped = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/entry/vdso/vdso64.so")
    objcopy_args = ctx.actions.args()
    objcopy_args.add("-S")
    objcopy_args.add("--remove-section")
    objcopy_args.add("__ex_table")
    objcopy_args.add(dbg)
    objcopy_args.add(stripped)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._llvm_objcopy,
        inputs = [dbg, ctx.executable._llvm_objcopy],
        outputs = [stripped],
        arguments = [objcopy_args],
        mnemonic = "LinuxVDSOObjcopy",
        progress_message = "Stripping Linux x86 vDSO %{label}",
    )

    out = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/entry/vdso/vdso-image-64.c")
    vdso2c_args = ctx.actions.args()
    vdso2c_args.add("-raw", dbg)
    vdso2c_args.add("-stripped", stripped)
    vdso_header = _source_tree_file(ctx, "arch/x86/include/asm/vdso.h")
    vdso2c_args.add("-vdso-header", vdso_header)
    vdso2c_args.add("-out", out)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._vdso2c,
        inputs = [dbg, stripped, vdso_header],
        outputs = [out],
        arguments = [vdso2c_args],
        mnemonic = "LinuxVDSO2C",
        progress_message = "Generating Linux x86 vDSO image source %{label}",
    )
    return out

def _linux_object_generated_inputs(ctx, compiler, linker, cc_toolchain, feature_configuration, config, generated_headers, source_root):
    files = []
    include_dirs = []
    include_dir_anchors = {}
    assembler_include_roots = []
    assembler_include_root_anchors = {}

    if ctx.attr.object == "drivers/tty/vt/ucs.o":
        for header in [
            "ucs_width_table.h",
            "ucs_recompose_table.h",
            "ucs_fallback_table.h",
        ]:
            files.append(_copy_source_tree_file(
                ctx,
                "drivers/tty/vt/" + header,
                "drivers/tty/vt/" + header + "_shipped",
            ))
        include_dirs.append(files[0].dirname)
        include_dir_anchors[files[0].dirname] = directory_anchor(files[0])

    if ctx.attr.object == "drivers/scsi/scsi_sysfs.o":
        header = _source_tree_file(ctx, "include/scsi/scsi_devinfo.h")
        out = ctx.actions.declare_file(ctx.label.name + ".obj/drivers/scsi/scsi_devinfo_tbl.c")
        args = ctx.actions.args()
        args.add("-in", header)
        args.add("-out", out)
        path_mapped_run(
            ctx.actions,
            executable = ctx.executable._scsidevinfo,
            inputs = [header],
            outputs = [out],
            arguments = [args],
            mnemonic = "LinuxSCSIDevinfo",
            progress_message = "Generating Linux SCSI devinfo table %{label}",
        )
        files.append(out)
        include_dirs.append(out.dirname)
        include_dir_anchors[out.dirname] = directory_anchor(out)

    if ctx.attr.object in ["lib/crc/crc32-main.o", "lib/crc32.o"]:
        out = ctx.actions.declare_file(
            ctx.label.name + ".obj/" + _linux_object_directory(ctx.attr.object) + "/crc32table.h",
        )
        args = ctx.actions.args()
        if ctx.attr.object == "lib/crc32.o":
            rows = 8
            if config.config_flags.get("CONFIG_CRC32_SLICEBY4") == "y":
                rows = 4
            elif config.config_flags.get("CONFIG_CRC32_SARWATE") == "y":
                rows = 1
            elif config.config_flags.get("CONFIG_CRC32_BIT") == "y":
                rows = 0
            args.add("-kind", "crc32-legacy")
            args.add("-rows", rows)
        else:
            args.add("-kind", "crc32")
        args.add("-out", out)
        path_mapped_run(
            ctx.actions,
            executable = ctx.executable._crctables,
            outputs = [out],
            arguments = [args],
            mnemonic = "LinuxCRCTables",
            progress_message = "Generating Linux CRC32 table %{label}",
        )
        files.append(out)
        include_dirs.append(out.dirname)
        include_dir_anchors[out.dirname] = directory_anchor(out)

    if ctx.attr.object in ["lib/crc/crc64-main.o", "lib/crc64.o"]:
        out = ctx.actions.declare_file(
            ctx.label.name + ".obj/" + _linux_object_directory(ctx.attr.object) + "/crc64table.h",
        )
        args = ctx.actions.args()
        args.add("-kind", "crc64")
        args.add("-out", out)
        path_mapped_run(
            ctx.actions,
            executable = ctx.executable._crctables,
            outputs = [out],
            arguments = [args],
            mnemonic = "LinuxCRCTables",
            progress_message = "Generating Linux CRC64 table %{label}",
        )
        files.append(out)
        include_dirs.append(out.dirname)
        include_dir_anchors[out.dirname] = directory_anchor(out)

    if ctx.attr.object == "lib/oid_registry.o":
        header = _source_tree_file(ctx, "include/linux/oid_registry.h")
        out = ctx.actions.declare_file(ctx.label.name + ".obj/lib/oid_registry_data.c")
        args = ctx.actions.args()
        args.add("-in", header)
        args.add("-out", out)
        path_mapped_run(
            ctx.actions,
            executable = ctx.executable._oidregistry,
            inputs = [header],
            outputs = [out],
            arguments = [args],
            mnemonic = "LinuxOIDRegistry",
            progress_message = "Generating Linux OID registry data %{label}",
        )
        files.append(out)
        include_dirs.append(out.dirname)
        include_dir_anchors[out.dirname] = directory_anchor(out)

    if ctx.attr.object == "arch/x86/lib/inat.o":
        opcode_map = _source_tree_file(ctx, "arch/x86/lib/x86-opcode-map.txt")
        inat_h = _source_tree_file(ctx, "arch/x86/include/asm/inat.h")
        out = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/lib/inat-tables.c")
        args = ctx.actions.args()
        args.add("-in", opcode_map)
        args.add("-inat_h", inat_h)
        args.add("-out", out)
        path_mapped_run(
            ctx.actions,
            executable = ctx.executable._insnattr,
            inputs = [inat_h, opcode_map],
            outputs = [out],
            arguments = [args],
            mnemonic = "LinuxInsnAttr",
            progress_message = "Generating Linux x86 instruction attribute tables %{label}",
        )
        files.append(out)
        include_dirs.append(out.dirname)
        include_dir_anchors[out.dirname] = directory_anchor(out)

    if ctx.attr.object == "usr/initramfs_data.o":
        initramfs_list = _source_tree_file(ctx, "usr/default_cpio_list")
        out = ctx.actions.declare_file(ctx.label.name + ".obj/usr/initramfs_inc_data")
        args = ctx.actions.args()
        args.add("-in", initramfs_list)
        args.add("-out", out)
        path_mapped_run(
            ctx.actions,
            executable = ctx.executable._initramfsdata,
            inputs = [initramfs_list],
            outputs = [out],
            arguments = [args],
            mnemonic = "LinuxInitramfsData",
            progress_message = "Generating Linux initramfs data %{label}",
        )
        files.append(out)
        assembler_include_roots.append(out.dirname[:-len("/usr")])
        assembler_include_root_anchors[assembler_include_roots[-1]] = directory_anchor(out, assembler_include_roots[-1])

    if ctx.attr.object == "arch/x86/purgatory/kexec-purgatory.o":
        purgatory = _linux_purgatory_outputs(
            ctx,
            compiler,
            linker,
            cc_toolchain,
            feature_configuration,
            config,
            generated_headers,
            source_root,
        )
        files.extend([purgatory.ro, purgatory.check])
        assembler_include_roots.append(purgatory.ro.dirname[:-len("/arch/x86/purgatory")])
        assembler_include_root_anchors[assembler_include_roots[-1]] = directory_anchor(purgatory.ro, assembler_include_roots[-1])

    if ctx.attr.object == "arch/x86/realmode/rmpiggy.o":
        realmode = _linux_realmode_outputs(
            ctx,
            compiler,
            linker,
            cc_toolchain,
            config,
            generated_headers,
            source_root,
        )
        files.extend([realmode.bin, realmode.relocs])
        assembler_include_roots.append(realmode.bin.dirname[:-len("/arch/x86/realmode/rm")])
        assembler_include_root_anchors[assembler_include_roots[-1]] = directory_anchor(realmode.bin, assembler_include_roots[-1])

    if ctx.attr.object == "arch/arm64/kernel/vdso-wrap.o" and generated_headers:
        vdso = _linux_generated_header(generated_headers, "arch/arm64/kernel/vdso/vdso.so")
        files.append(vdso)
        assembler_include_roots.append(vdso.dirname[:-len("/arch/arm64/kernel/vdso")])
        assembler_include_root_anchors[assembler_include_roots[-1]] = directory_anchor(vdso, assembler_include_roots[-1])

    if ctx.attr.object == "arch/arm64/kernel/vdso32-wrap.o" and generated_headers:
        if not generated_headers.vdsomunge:
            fail("arch/arm64/kernel/vdso32-wrap.o requires an arm64 generated_headers provider with vdsomunge")
        vdso32 = _linux_arm64_vdso32_outputs(
            ctx,
            cc_toolchain,
            feature_configuration,
            config,
            source_root,
            generated_headers.include_dirs,
            _generated_include_dir_anchors(generated_headers),
            generated_headers.files,
            ctx.label.name + ".obj",
            generated_headers.vdsomunge,
        ).so
        files.append(vdso32)
        assembler_include_roots.append(vdso32.dirname[:-len("/arch/arm64/kernel/vdso32")])
        assembler_include_root_anchors[assembler_include_roots[-1]] = directory_anchor(vdso32, assembler_include_roots[-1])

    return struct(
        assembler_include_root_anchors = assembler_include_root_anchors,
        assembler_include_roots = assembler_include_roots,
        files = files,
        include_dir_anchors = include_dir_anchors,
        include_dirs = include_dirs,
    )

def _linux_dtb_object_source(ctx, src, object, srcarch):
    out_base = ctx.label.name + ".obj/" + object[:-len(".o")]
    dtb = ctx.actions.declare_file(out_base)
    wrapper = ctx.actions.declare_file(out_base + ".S")
    if object != "drivers/of/empty_root.dtb.o":
        fail("linux_object %s needs generic devicetree compiler support for %s" % (ctx.label, object))
    args = ctx.actions.args()
    args.add("-in", src)
    args.add("-out", dtb)
    args.add("-wrapper_out", wrapper)
    args.add("-section", _linux_dtb_section(object, srcarch))
    args.add("-symbol", _linux_dtb_symbol_base(src))
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._emptyrootdtb,
        inputs = [src],
        outputs = [dtb, wrapper],
        arguments = [args],
        mnemonic = "LinuxEmptyRootDtb",
        progress_message = "Generating Linux empty root devicetree blob %{label}",
    )
    return struct(
        src = wrapper,
        files = [dtb, wrapper],
    )

def _linux_name_fix(value):
    return value.replace("-", "_").replace(",", "_").replace(" ", "_")

def _linux_object_stem(object):
    base = object
    if "/" in base:
        base = base.split("/")[-1]
    if base.endswith(".o"):
        base = base[:-len(".o")]
    return _linux_name_fix(base)

def _linux_object_modfile(object):
    if object.endswith(".o"):
        return object[:-len(".o")]
    return object

def _linux_object_name_flags(object, modname = ""):
    stem = _linux_object_stem(object)
    mod_object = modname if modname else object
    modstem = _linux_object_stem(mod_object)
    modfile = _linux_object_modfile(mod_object)
    return [
        "-DKBUILD_BASENAME=\"%s\"" % stem,
        "-DKBUILD_MODNAME=\"%s\"" % modstem,
        "-D__KBUILD_MODNAME=kmod_%s" % modstem,
        "-DKBUILD_MODFILE=\"%s\"" % modfile,
    ]

def _linux_module_flags(mode):
    if mode == "m":
        return ["-DMODULE"]
    return []

_X86_ASM_GENERIC_WRAPPERS = [
    "early_ioremap.h",
    "fprobe.h",
    "irq_regs.h",
    "kmap_size.h",
    "local64.h",
    "mcs_spinlock.h",
    "mmiowb.h",
    "mmzone.h",
    "module.lds.h",
    "ring_buffer.h",
    "rwonce.h",
    "unwind_user.h",
]

_X86_UAPI_ASM_GENERIC_WRAPPERS = [
    "auxvec.h",
    "bitsperlong.h",
    "bpf_perf_event.h",
    "byteorder.h",
    "errno.h",
    "fcntl.h",
    "ioctl.h",
    "ioctls.h",
    "ipcbuf.h",
    "mman.h",
    "msgbuf.h",
    "param.h",
    "poll.h",
    "posix_types.h",
    "ptrace.h",
    "resource.h",
    "sembuf.h",
    "setup.h",
    "shmbuf.h",
    "sigcontext.h",
    "siginfo.h",
    "signal.h",
    "socket.h",
    "sockios.h",
    "stat.h",
    "statfs.h",
    "termbits.h",
    "termios.h",
    "types.h",
    "unistd.h",
]

_X86_GENERATED_HEADER_FAMILIES = [
    "all",
    "asm_offsets",
    "bounds",
    "compile",
    "cpufeatures",
    "kvm_offsets",
    "rq_offsets",
    "static",
    "timeconst",
    "utsrelease",
    "utsversion",
    "version",
]

_X86_PRECISE_GENERATED_HEADER_FAMILIES = [
    "static",
    "timeconst",
    "compile",
    "version",
    "utsrelease",
    "utsversion",
    "cpufeatures",
    "bounds",
    "asm_offsets",
    "rq_offsets",
    "kvm_offsets",
]

_X86_OFFSETS_HEADER_FAMILIES = [
    "bounds",
    "asm_offsets",
    "rq_offsets",
    "kvm_offsets",
]

def _linux_offsets_header(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, src, out_path, guard, generated_inputs, include_dir_anchors = {}, srcarch = "x86", extra_include_dirs = [], extra_flags = []):
    compiler = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
    )
    asm = ctx.actions.declare_file(out_path + ".s")
    out = ctx.actions.declare_file(out_path)
    compile_args = ctx.actions.args()
    compile_args.add_all(_linux_compile_flags(ctx, cc_toolchain, feature_configuration))

    # These preparatory -S compiles are consumed by the offsets parser, so they
    # must emit real assembly even when the kernel itself is built with Clang
    # LTO. LLVM bitcode output from -flto is not parseable as offsets assembly.
    config_flags = _linux_non_lto_config_flags_for_source(ctx, config, src, out_suffix = "offsets.nolto")
    compile_args.add_all(config_flags.flags, format_each = "@%s")
    compile_args.add_all(_linux_source_preinclude_flags_for_root(source_root))
    _add_config_include_flag(compile_args, config)
    _add_linux_source_include_flags_for_root(
        compile_args,
        source_root,
        srcarch,
        include_dirs,
        include_dir_anchors,
    )
    for include_dir in extra_include_dirs:
        compile_args.add("-I" + include_dir)
    compile_args.add_all(extra_flags)
    compile_args.add("-S")
    compile_args.add(src)
    compile_args.add("-o")
    compile_args.add(asm)

    direct_inputs = _linux_source_tree_inputs(ctx, direct = [src] + generated_inputs + config_flags.inputs)
    path_mapped_run(
        ctx.actions,
        executable = compiler,
        inputs = depset(direct_inputs, transitive = [cc_toolchain.all_files, config.files]),
        outputs = [asm],
        arguments = [compile_args],
        mnemonic = "LinuxOffsetsAsm",
        progress_message = "Compiling Linux offsets assembly %{label}",
    )

    header_args = ctx.actions.args()
    header_args.add("-in", asm)
    header_args.add("-out", out)
    header_args.add("-guard", guard)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._offsetsheader,
        inputs = [asm],
        outputs = [out],
        arguments = [header_args],
        mnemonic = "LinuxOffsetsHeader",
        progress_message = "Generating Linux offsets header %{label}",
    )
    return out

def _linux_arm64_vdso_compile(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, include_dir_anchors, generated_inputs, src, out_relpath, extra_flags = []):
    compiler = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
    )
    out = ctx.actions.declare_file(out_relpath)
    assembly = _is_assembly_source(src)
    args = ctx.actions.args()
    args.add_all(_linux_compile_flags(ctx, cc_toolchain, feature_configuration))

    # vDSO objects are built as a separate miniature image, not as part of the
    # final vmlinux LTO unit. Keep their compile path non-LTO even when the
    # kernel proper uses Clang LTO.
    config_flags = _linux_non_lto_config_flags_for_source(
        ctx,
        config,
        src,
        out_suffix = out.basename + ".nolto",
    )
    args.add_all(config_flags.flags, format_each = "@%s")
    args.add_all([
        "-fno-common",
        "-fno-builtin",
        "-fno-stack-protector",
        "-ffixed-x18",
        "-DDISABLE_BRANCH_PROFILING",
        "-DBUILD_VDSO",
        "-O2",
        "-mcmodel=tiny",
        "-fasynchronous-unwind-tables",
        "-Wno-default-const-init-unsafe",
        "-Wno-default-const-init-var-unsafe",
        "-Wno-frame-address",
        "-Wno-format-security",
        "-Wno-gnu",
        "-Wno-gnu-variable-sized-type-not-at-end",
        "-Wno-initializer-overrides",
        "-Wno-override-init",
        "-Wno-pointer-sign",
        "-Wno-trigraphs",
        "-Wno-unused-command-line-argument",
    ])
    args.add_all(_linux_source_preinclude_flags_for_root(source_root, assembly))
    _add_config_include_flag(args, config)
    _add_linux_source_include_flags_for_root(args, source_root, "arm64", include_dirs, include_dir_anchors)
    args.add("-I" + source_root + "/arch/arm64/kernel/vdso")
    args.add("-I" + source_root + "/lib/vdso")
    args.add_all(extra_flags)
    args.add("-c")
    args.add(src)
    args.add("-o")
    args.add(out)
    source_inputs = _linux_action_source_inputs(
        ctx,
        "Linux arm64 vDSO compile",
        direct = [src] + generated_inputs + config_flags.inputs,
    )
    path_mapped_run(
        ctx.actions,
        executable = compiler,
        inputs = depset(
            source_inputs.direct,
            transitive = source_inputs.transitive + [cc_toolchain.all_files, config.files],
        ),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxARM64VDSOCompile",
        progress_message = "Compiling Linux arm64 vDSO object %{label}",
    )
    return out

def _linux_arm64_vdso_linker_script(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, include_dir_anchors, generated_inputs, out_relpath):
    compiler = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
    )
    src = _source_tree_file(ctx, "arch/arm64/kernel/vdso/vdso.lds.S")
    out = ctx.actions.declare_file(out_relpath)
    args = ctx.actions.args()
    args.add_all(_linux_compile_flags(ctx, cc_toolchain, feature_configuration))
    args.add_all([
        "-E",
        "-P",
        "-C",
        "-Uarm64",
        "-D__KERNEL__",
        "-D__ASSEMBLY__",
        "-include",
        source_root + "/include/linux/kconfig.h",
    ])
    _add_config_include_flag(args, config)
    _add_linux_source_include_flags_for_root(args, source_root, "arm64", include_dirs, include_dir_anchors)
    args.add(src)
    args.add("-o")
    args.add(out)
    source_inputs = _linux_action_source_inputs(
        ctx,
        "Linux arm64 vDSO linker script",
        direct = [src] + generated_inputs,
    )
    path_mapped_run(
        ctx.actions,
        executable = compiler,
        inputs = depset(
            source_inputs.direct,
            transitive = source_inputs.transitive + [cc_toolchain.all_files, config.files],
        ),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxARM64VDSOLinkerScript",
        progress_message = "Preprocessing Linux arm64 vDSO linker script %{label}",
    )
    return out

def _linux_arm64_vdso_outputs(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, include_dir_anchors, generated_inputs, base):
    objects = [
        _linux_arm64_vdso_compile(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, include_dir_anchors, generated_inputs, _source_tree_file(ctx, "arch/arm64/kernel/vdso/vgettimeofday.c"), base + "/arch/arm64/kernel/vdso/vgettimeofday.o", extra_flags = ["-include", source_root + "/lib/vdso/gettimeofday.c"]),
        _linux_arm64_vdso_compile(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, include_dir_anchors, generated_inputs, _source_tree_file(ctx, "arch/arm64/kernel/vdso/note.S"), base + "/arch/arm64/kernel/vdso/note.o"),
        _linux_arm64_vdso_compile(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, include_dir_anchors, generated_inputs, _source_tree_file(ctx, "arch/arm64/kernel/vdso/sigreturn.S"), base + "/arch/arm64/kernel/vdso/sigreturn.o"),
        _linux_arm64_vdso_compile(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, include_dir_anchors, generated_inputs, _source_tree_file(ctx, "arch/arm64/kernel/vdso/vgetrandom.c"), base + "/arch/arm64/kernel/vdso/vgetrandom.o", extra_flags = ["-include", source_root + "/lib/vdso/getrandom.c"]),
        _linux_arm64_vdso_compile(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, include_dir_anchors, generated_inputs, _source_tree_file(ctx, "arch/arm64/kernel/vdso/vgetrandom-chacha.S"), base + "/arch/arm64/kernel/vdso/vgetrandom-chacha.o"),
    ]
    linker_script = _linux_arm64_vdso_linker_script(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, include_dir_anchors, generated_inputs, base + "/arch/arm64/kernel/vdso/vdso.lds")
    linker = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = CPP_LINK_EXECUTABLE_ACTION_NAME,
    )
    ld = _linux_x86_tool_sibling(linker, "ld.lld")
    dbg = ctx.actions.declare_file(base + "/arch/arm64/kernel/vdso/vdso.so.dbg")
    link_args = ctx.actions.args()
    link_args.add_all([
        "-EL",
        "-maarch64elf",
        "-z",
        "norelro",
        "-z",
        "noexecstack",
        "-shared",
        "-soname=linux-vdso.so.1",
        "-Bsymbolic",
        "--build-id=sha1",
        "-n",
        "--orphan-handling=warn",
    ])
    link_args.add("-T")
    link_args.add(linker_script)
    link_args.add_all(objects)
    link_args.add_all([
        "-o",
        dbg,
    ])
    path_mapped_run(
        ctx.actions,
        executable = ld,
        inputs = depset(objects + [linker_script], transitive = [cc_toolchain.all_files]),
        outputs = [dbg],
        arguments = [link_args],
        mnemonic = "LinuxARM64VDSOLink",
        progress_message = "Linking Linux arm64 vDSO %{label}",
    )

    so = ctx.actions.declare_file(base + "/arch/arm64/kernel/vdso/vdso.so")
    objcopy_args = ctx.actions.args()
    objcopy_args.add("-S")
    objcopy_args.add(dbg)
    objcopy_args.add(so)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._llvm_objcopy,
        inputs = [dbg, ctx.executable._llvm_objcopy],
        outputs = [so],
        arguments = [objcopy_args],
        mnemonic = "LinuxARM64VDSOObjcopy",
        progress_message = "Stripping Linux arm64 vDSO %{label}",
    )

    nm = ctx.actions.declare_file(base + "/arch/arm64/kernel/vdso/vdso.so.dbg.nm")
    nm_args = ctx.actions.args()
    nm_args.add(nm)
    nm_args.add(ctx.executable._llvm_nm)
    nm_args.add(dbg)
    path_mapped_run(
        ctx.actions,
        executable = ctx.attr._runandwrite[DefaultInfo].files_to_run,
        inputs = [dbg],
        tools = [ctx.attr._llvm_nm[DefaultInfo].files_to_run],
        outputs = [nm],
        arguments = [nm_args],
        mnemonic = "LinuxARM64VDSONM",
        progress_message = "Generating Linux arm64 vDSO symbols %{label}",
    )

    offsets = ctx.actions.declare_file(base + "/include/generated/vdso-offsets.h")
    offsets_args = ctx.actions.args()
    offsets_args.add("-vdso_nm", nm)
    offsets_args.add("-vdso_offsets_out", offsets)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._arm64headers,
        inputs = [nm],
        outputs = [offsets],
        arguments = [offsets_args],
        mnemonic = "LinuxARM64VDSOOffsets",
        progress_message = "Generating Linux arm64 vDSO offsets %{label}",
    )
    return struct(
        offsets = offsets,
        so = so,
    )

def _add_linux_arm64_vdso32_common_flags(args, ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, include_dir_anchors):
    args.add_all(_linux_compile_flags_without_target(ctx, cc_toolchain, feature_configuration))
    args.add_all([
        "--target=arm-linux-gnueabi",
        "-nostdinc",
        "-DBUILD_VDSO",
        "-D__KERNEL__",
        "-fno-PIE",
        "-mabi=aapcs-linux",
        "-mfloat-abi=soft",
        "-mlittle-endian",
        "-fPIC",
        "-fno-builtin",
        "-fno-stack-protector",
        "-DDISABLE_BRANCH_PROFILING",
        "-march=armv8-a",
        "-Wno-unused-command-line-argument",
    ])
    args.add_all(_linux_source_preinclude_flags_for_root(source_root, False))
    _add_config_include_flag(args, config)
    _add_linux_source_include_flags_for_root(args, source_root, "arm64", include_dirs, include_dir_anchors)
    args.add_all([
        "-I" + source_root + "/arch/arm64/kernel/vdso32",
        "-I" + source_root + "/lib/vdso",
    ])

def _linux_arm64_vdso32_compile(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, include_dir_anchors, generated_input_files, src, out_relpath, extra_flags = []):
    compiler = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
    )
    out = ctx.actions.declare_file(out_relpath)
    args = ctx.actions.args()
    _add_linux_arm64_vdso32_common_flags(args, ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, include_dir_anchors)
    args.add_all([
        "-Wall",
        "-Wundef",
        "-Wstrict-prototypes",
        "-Wno-trigraphs",
        "-fno-strict-aliasing",
        "-fno-common",
        "-Werror-implicit-function-declaration",
        "-Wno-format-security",
        "-std=gnu11",
        "-O2",
        "-Wno-pointer-sign",
        "-Wno-default-const-init-unsafe",
        "-Wno-default-const-init-var-unsafe",
        "-Wno-frame-address",
        "-Wno-gnu",
        "-Wno-gnu-variable-sized-type-not-at-end",
        "-Wno-initializer-overrides",
        "-Wno-override-init",
        "-fno-strict-overflow",
        "-Werror=strict-prototypes",
        "-Werror=date-time",
        "-Werror=incompatible-pointer-types",
        "-marm",
    ])
    args.add_all(extra_flags)
    args.add("-c")
    args.add(src)
    args.add("-o")
    args.add(out)
    source_inputs = _linux_action_source_inputs(
        ctx,
        "Linux arm64 compat vDSO compile",
        direct = [src],
    )
    path_mapped_run(
        ctx.actions,
        executable = compiler,
        inputs = depset(
            source_inputs.direct,
            transitive = source_inputs.transitive + [cc_toolchain.all_files, config.files, generated_input_files],
        ),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxARM64VDSO32Compile",
        progress_message = "Compiling Linux arm64 compat vDSO object %{label}",
    )
    return out

def _linux_arm64_vdso32_linker_script(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, include_dir_anchors, generated_input_files, out_relpath):
    compiler = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
    )
    src = _source_tree_file(ctx, "arch/arm64/kernel/vdso32/vdso.lds.S")
    out = ctx.actions.declare_file(out_relpath)
    args = ctx.actions.args()
    args.add_all(_linux_compile_flags_without_target(ctx, cc_toolchain, feature_configuration))
    args.add_all([
        "--target=arm-linux-gnueabi",
        "-E",
        "-P",
        "-C",
        "-Uarm64",
        "-D__KERNEL__",
        "-D__ASSEMBLY__",
        "-include",
        source_root + "/include/linux/kconfig.h",
    ])
    _add_config_include_flag(args, config)
    _add_linux_source_include_flags_for_root(args, source_root, "arm64", include_dirs, include_dir_anchors)
    args.add("-I" + source_root + "/arch/arm64/kernel/vdso32")
    args.add("-I" + source_root + "/lib/vdso")
    args.add(src)
    args.add("-o")
    args.add(out)
    source_inputs = _linux_action_source_inputs(
        ctx,
        "Linux arm64 compat vDSO linker script",
        direct = [src],
    )
    path_mapped_run(
        ctx.actions,
        executable = compiler,
        inputs = depset(
            source_inputs.direct,
            transitive = source_inputs.transitive + [cc_toolchain.all_files, config.files, generated_input_files],
        ),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxARM64VDSO32LinkerScript",
        progress_message = "Preprocessing Linux arm64 compat vDSO linker script %{label}",
    )
    return out

def _linux_arm64_vdso32_outputs(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, include_dir_anchors, generated_input_files, base, vdsomunge):
    objects = [
        _linux_arm64_vdso32_compile(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, include_dir_anchors, generated_input_files, _source_tree_file(ctx, "arch/arm64/kernel/vdso32/note.c"), base + "/arch/arm64/kernel/vdso32/note.o"),
        _linux_arm64_vdso32_compile(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, include_dir_anchors, generated_input_files, _source_tree_file(ctx, "arch/arm64/kernel/vdso32/vgettimeofday.c"), base + "/arch/arm64/kernel/vdso32/vgettimeofday.o", extra_flags = ["-include", source_root + "/lib/vdso/gettimeofday.c"]),
    ]
    linker_script = _linux_arm64_vdso32_linker_script(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, include_dir_anchors, generated_input_files, base + "/arch/arm64/kernel/vdso32/vdso.lds")
    linker = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = CPP_LINK_EXECUTABLE_ACTION_NAME,
    )
    raw = ctx.actions.declare_file(base + "/arch/arm64/kernel/vdso32/vdso.so.raw")
    link_args = ctx.actions.args()
    link_args.add_all([
        "--target=arm-linux-gnueabi",
        "-fuse-ld=lld",
        "-nostdlib",
        "-shared",
        "-Wl,-Bsymbolic",
        "-Wl,--no-undefined",
        "-Wl,-soname=linux-vdso.so.1",
        "-Wl,-z,max-page-size=4096",
        "-Wl,-z,common-page-size=4096",
        "-Wl,--build-id=sha1",
        "-Wl,--orphan-handling=warn",
    ])
    link_args.add(linker_script, format = "-Wl,-T,%s")
    link_args.add("-o")
    link_args.add(raw)
    link_args.add_all(objects)
    path_mapped_run(
        ctx.actions,
        executable = linker,
        inputs = depset(objects + [linker_script], transitive = [cc_toolchain.all_files]),
        outputs = [raw],
        arguments = [link_args],
        mnemonic = "LinuxARM64VDSO32Link",
        progress_message = "Linking Linux arm64 compat vDSO %{label}",
    )

    dbg = ctx.actions.declare_file(base + "/arch/arm64/kernel/vdso32/vdso32.so.dbg")
    munge_args = ctx.actions.args()
    munge_args.add(raw)
    munge_args.add(dbg)
    path_mapped_run(
        ctx.actions,
        executable = vdsomunge,
        inputs = [raw],
        outputs = [dbg],
        arguments = [munge_args],
        mnemonic = "LinuxARM64VDSO32Munge",
        progress_message = "Normalizing Linux arm64 compat vDSO ABI flags %{label}",
    )

    so = ctx.actions.declare_file(base + "/arch/arm64/kernel/vdso32/vdso.so")
    objcopy_args = ctx.actions.args()
    objcopy_args.add("-S")
    objcopy_args.add(dbg)
    objcopy_args.add(so)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._llvm_objcopy,
        inputs = [dbg, ctx.executable._llvm_objcopy],
        outputs = [so],
        arguments = [objcopy_args],
        mnemonic = "LinuxARM64VDSO32Objcopy",
        progress_message = "Stripping Linux arm64 compat vDSO %{label}",
    )
    return struct(so = so)

def _linux_generated_header_aggregate(infos):
    if not infos:
        fail("generated-header aggregate requires at least one family")
    first = infos[0]
    cflags = first.cflags
    vdsomunge = first.vdsomunge
    include_dirs = []
    include_dir_anchors = {}
    for info in infos:
        if info.arch != first.arch or info.srcarch != first.srcarch:
            fail("generated-header aggregate mixes architectures")
        if info.cflags != None:
            if cflags != None and info.cflags != cflags:
                fail("generated-header aggregate has multiple cflags files")
            cflags = info.cflags
        if info.vdsomunge != None:
            if vdsomunge != None and info.vdsomunge != vdsomunge:
                fail("generated-header aggregate has multiple vdsomunge tools")
            vdsomunge = info.vdsomunge
        include_dirs.extend(info.include_dirs)
        include_dir_anchors.update(info.include_dir_anchors)
    return struct(
        arch = first.arch,
        cflags = cflags,
        files = depset(transitive = [info.files for info in infos]),
        include_dir_anchors = include_dir_anchors,
        include_dirs = _unique_strings(include_dirs),
        srcarch = first.srcarch,
        vdsomunge = vdsomunge,
    )

def _generated_header_family_aggregate(name, content_id, infos):
    aggregate = _linux_generated_header_aggregate(infos)
    return struct(
        arch = aggregate.arch,
        cflags = aggregate.cflags,
        content_id = content_id,
        files = aggregate.files,
        include_dir_anchors = aggregate.include_dir_anchors,
        include_dirs = aggregate.include_dirs,
        name = name,
        srcarch = aggregate.srcarch,
        vdsomunge = aggregate.vdsomunge,
    )

def _linux_x86_reusable_header_families(ctx):
    reusable = {}
    for target in ctx.attr.reusable_generated_headers:
        provider = target[LinuxGeneratedHeadersInfo]
        if provider.arch != "x86" or provider.srcarch != "x86":
            fail("linux_x86_generated_headers reusable target %s has incompatible architecture" % target.label)
        for name in sorted(provider.families.keys()):
            if name not in _X86_GENERATED_HEADER_FAMILIES:
                fail("linux_x86_generated_headers reusable target %s publishes unknown family %s" % (target.label, name))
            family = provider.families[name]
            if family.name != name:
                fail(
                    "linux_x86_generated_headers reusable target %s publishes family %s under name %s" %
                    (target.label, family.name, name),
                )
            _validate_content_id(
                family.content_id,
                "linux_x86_generated_headers reusable target %s family %s content ID" % (target.label, name),
            )
            if family.arch != "x86" or family.srcarch != "x86":
                fail(
                    "linux_x86_generated_headers reusable target %s family %s has incompatible architecture" %
                    (target.label, name),
                )
            key = (name, family.content_id)
            if key not in reusable:
                reusable[key] = family
    return reusable

def _linux_x86_reusable_header_family(ctx, reusable, name):
    return reusable.get((name, ctx.attr.family_content_ids[name]))

def _linux_x86_header_family_base(ctx, name):
    return ctx.label.name + ".headers/" + name

def _linux_x86_header_family_dependencies(ctx):
    generation_order = {
        name: index
        for index, name in enumerate(_X86_PRECISE_GENERATED_HEADER_FAMILIES)
    }
    dependencies = {
        name: {}
        for name in _X86_OFFSETS_HEADER_FAMILIES
    }
    for edge, dependency_id in ctx.attr.family_dependency_ids.items():
        names = edge.split(":")
        if len(names) != 2:
            fail("linux_x86_generated_headers has invalid family dependency edge %r" % edge)
        family_name, dependency_name = names
        if family_name not in dependencies:
            fail("linux_x86_generated_headers family %s cannot consume generated-header dependencies" % family_name)
        if dependency_name not in generation_order:
            fail(
                "linux_x86_generated_headers family %s depends on unknown family %s" %
                (family_name, dependency_name),
            )
        if generation_order[dependency_name] >= generation_order[family_name]:
            fail(
                "linux_x86_generated_headers family %s depends on non-earlier family %s" %
                (family_name, dependency_name),
            )
        _validate_content_id(
            dependency_id,
            "linux_x86_generated_headers family %s dependency %s content ID" %
            (family_name, dependency_name),
        )
        if dependency_id != ctx.attr.family_content_ids[dependency_name]:
            fail(
                "linux_x86_generated_headers family %s dependency %s content ID does not match selected family" %
                (family_name, dependency_name),
            )
        dependencies[family_name][dependency_name] = True
    return {
        family_name: [
            dependency_name
            for dependency_name in _X86_PRECISE_GENERATED_HEADER_FAMILIES
            if dependency_name in dependencies[family_name]
        ]
        for family_name in _X86_OFFSETS_HEADER_FAMILIES
    }

def _linux_x86_static_header_family(ctx):
    base = _linux_x86_header_family_base(ctx, "static")
    headers = []
    arch_include_dir = None
    uapi_include_dir = None
    for header in _X86_ASM_GENERIC_WRAPPERS:
        out = ctx.actions.declare_file(base + "/arch/x86/include/generated/asm/" + header)
        ctx.actions.write(
            output = out,
            content = "#include <asm-generic/%s>\n" % header,
        )
        if arch_include_dir == None:
            arch_include_dir = out.dirname[:-len("/asm")]
        headers.append(out)
    for header in _X86_UAPI_ASM_GENERIC_WRAPPERS:
        out = ctx.actions.declare_file(base + "/arch/x86/include/generated/uapi/asm/" + header)
        ctx.actions.write(
            output = out,
            content = "#include <asm-generic/%s>\n" % header,
        )
        if uapi_include_dir == None:
            uapi_include_dir = out.dirname[:-len("/asm")]
        headers.append(out)

    syscall_specs = [
        (ctx.file.syscall_32_tbl, base + "/arch/x86/include/generated/uapi/asm/unistd_32.h", "i386", True, "", ""),
        (ctx.file.syscall_64_tbl, base + "/arch/x86/include/generated/uapi/asm/unistd_64.h", "common,64", True, "", ""),
        (ctx.file.syscall_64_tbl, base + "/arch/x86/include/generated/uapi/asm/unistd_x32.h", "common,x32", True, "__X32_SYSCALL_BIT", ""),
        (ctx.file.syscall_32_tbl, base + "/arch/x86/include/generated/asm/unistd_32_ia32.h", "i386", True, "", "ia32_"),
        (ctx.file.syscall_64_tbl, base + "/arch/x86/include/generated/asm/unistd_64_x32.h", "x32", True, "", "x32_"),
    ]
    for table, path, abis, emit_nr, offset, prefix in syscall_specs:
        out = ctx.actions.declare_file(path)
        args = ctx.actions.args()
        args.add("-in", table)
        args.add("-out", out)
        args.add("-abis", abis)
        if emit_nr:
            args.add("-emit-nr")
        if offset:
            args.add("-offset", offset)
        if prefix:
            args.add("-prefix", prefix)
        path_mapped_run(
            ctx.actions,
            executable = ctx.executable._syscallhdr,
            inputs = [table],
            outputs = [out],
            arguments = [args],
            mnemonic = "LinuxSyscallHeader",
            progress_message = "Generating Linux syscall header %{label}",
        )
        headers.append(out)

    syscall_table_specs = [
        (ctx.file.syscall_32_tbl, base + "/arch/x86/include/generated/asm/syscalls_32.h", "i386"),
        (ctx.file.syscall_64_tbl, base + "/arch/x86/include/generated/asm/syscalls_64.h", "common,64"),
        (ctx.file.syscall_64_tbl, base + "/arch/x86/include/generated/asm/syscalls_x32.h", "common,x32"),
    ]
    for table, path, abis in syscall_table_specs:
        out = ctx.actions.declare_file(path)
        args = ctx.actions.args()
        args.add("-in", table)
        args.add("-out", out)
        args.add("-abis", abis)
        path_mapped_run(
            ctx.actions,
            executable = ctx.executable._syscalltbl,
            inputs = [table],
            outputs = [out],
            arguments = [args],
            mnemonic = "LinuxSyscallTableHeader",
            progress_message = "Generating Linux syscall table header %{label}",
        )
        headers.append(out)

    xen_hypercalls = ctx.actions.declare_file(base + "/arch/x86/include/generated/asm/xen-hypercalls.h")
    xen_args = ctx.actions.args()
    for header in ctx.files.xen_interface_headers:
        xen_args.add("-in", header)
    xen_args.add("-out", xen_hypercalls)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._xenhypercalls,
        inputs = ctx.files.xen_interface_headers,
        outputs = [xen_hypercalls],
        arguments = [xen_args],
        mnemonic = "LinuxXenHypercalls",
        progress_message = "Generating Linux Xen hypercall header %{label}",
    )
    headers.append(xen_hypercalls)

    orc_hash = ctx.actions.declare_file(base + "/arch/x86/include/generated/asm/orc_hash.h")
    orc_hash_args = ctx.actions.args()
    orc_hash_args.add("-in", ctx.file.orc_types_h)
    orc_hash_args.add("-out", orc_hash)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._orchash,
        inputs = [ctx.file.orc_types_h],
        outputs = [orc_hash],
        arguments = [orc_hash_args],
        mnemonic = "LinuxORCHash",
        progress_message = "Generating Linux ORC hash header %{label}",
    )
    headers.append(orc_hash)
    return _generated_header_family_info(
        arch = "x86",
        cflags = None,
        content_id = ctx.attr.family_content_ids["static"],
        files = headers,
        include_dirs = [arch_include_dir, uapi_include_dir],
        name = "static",
        srcarch = "x86",
        vdsomunge = None,
    )

def _linux_x86_timeconst_header_family(ctx, config):
    base = _linux_x86_header_family_base(ctx, "timeconst")
    out = ctx.actions.declare_file(base + "/include/generated/timeconst.h")
    args = ctx.actions.args()
    args.add("-config", config.config)
    args.add("-out", out)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._timeconst,
        inputs = [config.config],
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxTimeconstHeader",
        progress_message = "Generating Linux time constants %{label}",
    )
    return _generated_header_family_info(
        arch = "x86",
        cflags = None,
        content_id = ctx.attr.family_content_ids["timeconst"],
        files = [out],
        include_dirs = [out.dirname[:-len("/generated")]],
        name = "timeconst",
        srcarch = "x86",
        vdsomunge = None,
    )

def _linux_x86_version_header_family(ctx, config, name, path, output_flag, mnemonic):
    base = _linux_x86_header_family_base(ctx, name)
    out = ctx.actions.declare_file(base + "/" + path)
    args = ctx.actions.args()
    args.add(output_flag, out)
    inputs = []
    if name == "compile":
        args.add("-machine", "x86_64")
        args.add("-compiler", _linux_compiler_version_string())
    elif name == "version":
        args.add("-kernel_version", config.kernel_version)
    elif name == "utsrelease":
        args.add("-kernel_release", config.kernel_release)
        inputs.append(config.kernel_release)
    elif name == "utsversion":
        args.add("-config", config.config)
        inputs.append(config.config)
    else:
        fail("unsupported x86 version-header family %s" % name)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._versionheaders,
        inputs = inputs,
        outputs = [out],
        arguments = [args],
        mnemonic = mnemonic,
        progress_message = "Generating Linux %s header %%{label}" % name,
    )
    if name == "version":
        include_dirs = [
            out.dirname[:-len("/generated/uapi/linux")],
            out.dirname[:-len("/linux")],
        ]
    else:
        include_dirs = [out.dirname[:-len("/generated")]]
    return _generated_header_family_info(
        arch = "x86",
        cflags = None,
        content_id = ctx.attr.family_content_ids[name],
        files = [out],
        include_dirs = include_dirs,
        name = name,
        srcarch = "x86",
        vdsomunge = None,
    )

def _linux_x86_cpufeatures_header_family(ctx, config):
    base = _linux_x86_header_family_base(ctx, "cpufeatures")
    out = ctx.actions.declare_file(base + "/arch/x86/include/generated/asm/cpufeaturemasks.h")
    args = ctx.actions.args()
    args.add("-cpufeatures", ctx.file.cpufeatures_h)
    args.add("-config", config.config)
    args.add("-out", out)
    if len(ctx.files.required_features_h) > 1:
        fail("linux_x86_generated_headers requires at most one required-features.h source")
    if ctx.files.required_features_h:
        args.add("-legacy")
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._cpufeaturemasks,
        inputs = [ctx.file.cpufeatures_h, config.config] + ctx.files.required_features_h,
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxCPUFeatureMasks",
        progress_message = "Generating Linux x86 CPU feature masks %{label}",
    )
    return _generated_header_family_info(
        arch = "x86",
        cflags = None,
        content_id = ctx.attr.family_content_ids["cpufeatures"],
        files = [out],
        include_dirs = [out.dirname[:-len("/asm")]],
        name = "cpufeatures",
        srcarch = "x86",
        vdsomunge = None,
    )

def _linux_x86_offsets_header_family(
        ctx,
        cc_toolchain,
        feature_configuration,
        config,
        source_root,
        families,
        dependency_names,
        name,
        src,
        path,
        guard,
        extra_include_dirs = []):
    if src == None:
        return _generated_header_family_info(
            arch = "x86",
            cflags = None,
            content_id = ctx.attr.family_content_ids[name],
            files = [],
            include_dirs = [],
            name = name,
            srcarch = "x86",
            vdsomunge = None,
        )
    dependencies = None
    if dependency_names:
        dependencies = _linux_generated_header_aggregate([
            families[dependency]
            for dependency in dependency_names
        ])
    out = _linux_offsets_header(
        ctx,
        cc_toolchain,
        feature_configuration,
        config,
        source_root,
        dependencies.include_dirs if dependencies != None else [],
        src,
        _linux_x86_header_family_base(ctx, name) + "/" + path,
        guard,
        dependencies.files.to_list() if dependencies != None else [],
        include_dir_anchors = dependencies.include_dir_anchors if dependencies != None else {},
        extra_include_dirs = extra_include_dirs,
    )
    if name == "kvm_offsets":
        include_dirs = [out.dirname]
    else:
        include_dirs = [out.dirname[:-len("/generated")]]
    return _generated_header_family_info(
        arch = "x86",
        cflags = None,
        content_id = ctx.attr.family_content_ids[name],
        files = [out],
        include_dirs = include_dirs,
        name = name,
        srcarch = "x86",
        vdsomunge = None,
    )

def _linux_x86_generated_header_families_impl(ctx):
    _validate_generated_header_family_content_ids(
        ctx.attr.family_content_ids,
        _X86_GENERATED_HEADER_FAMILIES,
        "linux_x86_generated_headers",
    )
    if len(ctx.files.rq_offsets_c) > 1:
        fail("linux_x86_generated_headers requires at most one rq-offsets.c source")
    cc_toolchain = find_cpp_toolchain(ctx)
    feature_configuration = _cc_feature_configuration(ctx, cc_toolchain)
    config = ctx.attr.config[LinuxConfigInfo]
    source_root = _linux_source_root_path(ctx)
    family_dependencies = _linux_x86_header_family_dependencies(ctx)
    reusable = _linux_x86_reusable_header_families(ctx)
    families = {}

    family = _linux_x86_reusable_header_family(ctx, reusable, "static")
    families["static"] = family if family != None else _linux_x86_static_header_family(ctx)
    family = _linux_x86_reusable_header_family(ctx, reusable, "timeconst")
    families["timeconst"] = family if family != None else _linux_x86_timeconst_header_family(ctx, config)

    version_specs = [
        ("compile", "include/generated/compile.h", "-compile_out", "LinuxCompileHeader"),
        ("version", "include/generated/uapi/linux/version.h", "-linux_version_out", "LinuxVersionHeader"),
        ("utsrelease", "include/generated/utsrelease.h", "-utsrelease_out", "LinuxUTSReleaseHeader"),
        ("utsversion", "include/generated/utsversion.h", "-utsversion_out", "LinuxUTSVersionHeader"),
    ]
    for name, path, output_flag, mnemonic in version_specs:
        family = _linux_x86_reusable_header_family(ctx, reusable, name)
        families[name] = family if family != None else _linux_x86_version_header_family(
            ctx,
            config,
            name,
            path,
            output_flag,
            mnemonic,
        )

    family = _linux_x86_reusable_header_family(ctx, reusable, "cpufeatures")
    families["cpufeatures"] = family if family != None else _linux_x86_cpufeatures_header_family(ctx, config)

    family = _linux_x86_reusable_header_family(ctx, reusable, "bounds")
    families["bounds"] = family if family != None else _linux_x86_offsets_header_family(
        ctx,
        cc_toolchain,
        feature_configuration,
        config,
        source_root,
        families,
        family_dependencies["bounds"],
        "bounds",
        ctx.file.bounds_c,
        "include/generated/bounds.h",
        "__LINUX_BOUNDS_H__",
    )
    family = _linux_x86_reusable_header_family(ctx, reusable, "asm_offsets")
    families["asm_offsets"] = family if family != None else _linux_x86_offsets_header_family(
        ctx,
        cc_toolchain,
        feature_configuration,
        config,
        source_root,
        families,
        family_dependencies["asm_offsets"],
        "asm_offsets",
        ctx.file.asm_offsets_c,
        "include/generated/asm-offsets.h",
        "__ASM_OFFSETS_H__",
    )
    rq_offsets_c = ctx.files.rq_offsets_c[0] if ctx.files.rq_offsets_c else None
    family = _linux_x86_reusable_header_family(ctx, reusable, "rq_offsets")
    families["rq_offsets"] = family if family != None else _linux_x86_offsets_header_family(
        ctx,
        cc_toolchain,
        feature_configuration,
        config,
        source_root,
        families,
        family_dependencies["rq_offsets"],
        "rq_offsets",
        rq_offsets_c,
        "include/generated/rq-offsets.h",
        "__RQ_OFFSETS_H__",
    )
    family = _linux_x86_reusable_header_family(ctx, reusable, "kvm_offsets")
    families["kvm_offsets"] = family if family != None else _linux_x86_offsets_header_family(
        ctx,
        cc_toolchain,
        feature_configuration,
        config,
        source_root,
        families,
        family_dependencies["kvm_offsets"],
        "kvm_offsets",
        ctx.file.kvm_asm_offsets_c,
        "arch/x86/kvm/kvm-asm-offsets.h",
        "__KVM_ASM_OFFSETS_H__",
        extra_include_dirs = [source_root + "/arch/x86/kvm"],
    )

    all_family = _linux_x86_reusable_header_family(ctx, reusable, "all")
    if all_family == None:
        all_family = _generated_header_family_aggregate(
            "all",
            ctx.attr.family_content_ids["all"],
            [
                families[name]
                for name in _X86_PRECISE_GENERATED_HEADER_FAMILIES
            ],
        )
    families["all"] = all_family
    return [
        DefaultInfo(files = all_family.files),
        LinuxGeneratedHeadersInfo(
            arch = all_family.arch,
            cflags = all_family.cflags,
            families = families,
            files = all_family.files,
            include_dir_anchors = all_family.include_dir_anchors,
            include_dirs = all_family.include_dirs,
            srcarch = all_family.srcarch,
            vdsomunge = all_family.vdsomunge,
        ),
    ]

linux_x86_generated_headers = rule(
    implementation = _linux_x86_generated_header_families_impl,
    attrs = {
        "arch": attr.string(default = "x86"),
        "asm_offsets_c": attr.label(allow_single_file = True, mandatory = True),
        "bounds_c": attr.label(allow_single_file = True, mandatory = True),
        "config": attr.label(providers = [LinuxConfigInfo], mandatory = True),
        "cpufeatures_h": attr.label(allow_single_file = True, mandatory = True),
        "family_dependency_ids": attr.string_dict(
            mandatory = True,
            doc = "Exact family:dependency edges to dependency family content IDs.",
        ),
        "family_content_ids": attr.string_dict(
            mandatory = True,
            doc = "Map of generated-header family names to full SHA-256 content identities.",
        ),
        "kvm_asm_offsets_c": attr.label(allow_single_file = True, mandatory = True),
        "orc_types_h": attr.label(allow_single_file = True, mandatory = True),
        "required_features_h": attr.label(allow_files = True, mandatory = True),
        "reusable_generated_headers": attr.label_list(
            providers = [LinuxGeneratedHeadersInfo],
            doc = "Earlier x86 generated-header providers eligible for family-level reuse.",
        ),
        "rq_offsets_c": attr.label(allow_files = True, mandatory = True),
        "source_root": attr.label(allow_single_file = True, mandatory = True),
        "source_tree": attr.label_list(allow_files = True),
        "syscall_32_tbl": attr.label(allow_single_file = True, mandatory = True),
        "syscall_64_tbl": attr.label(allow_single_file = True, mandatory = True),
        "xen_interface_headers": attr.label_list(allow_files = True),
        "_cpufeaturemasks": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/cpufeaturemasks"),
            executable = True,
        ),
        "_offsetsheader": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/offsetsheader"),
            executable = True,
        ),
        "_flagfilter": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/flagfilter"),
            executable = True,
        ),
        "_orchash": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/orchash"),
            executable = True,
        ),
        "_syscallhdr": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/syscallhdr"),
            executable = True,
        ),
        "_syscalltbl": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/syscalltbl"),
            executable = True,
        ),
        "_timeconst": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/timeconst"),
            executable = True,
        ),
        "_versionheaders": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/versionheaders"),
            executable = True,
        ),
        "_xenhypercalls": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/xenhypercalls"),
            executable = True,
        ),
    },
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Generates the x86 header subset normally produced before compiling Linux C objects.",
)

_ARM64_ASM_GENERIC_WRAPPERS = [
    "delay.h",
    "div64.h",
    "dma-mapping.h",
    "dma.h",
    "early_ioremap.h",
    "emergency-restart.h",
    "fprobe.h",
    "hw_irq.h",
    "irq_regs.h",
    "kdebug.h",
    "kmap_size.h",
    "local.h",
    "local64.h",
    "mcs_spinlock.h",
    "mmzone.h",
    "mmiowb.h",
    "msi.h",
    "parport.h",
    "qrwlock.h",
    "qspinlock.h",
    "serial.h",
    "softirq_stack.h",
    "switch_to.h",
    "trace_clock.h",
    "unwind_user.h",
    "user.h",
    "vga.h",
    "video.h",
]

_ARM64_UAPI_ASM_GENERIC_WRAPPERS = [
    "errno.h",
    "ioctl.h",
    "ioctls.h",
    "ipcbuf.h",
    "kvm_para.h",
    "msgbuf.h",
    "poll.h",
    "resource.h",
    "sembuf.h",
    "shmbuf.h",
    "siginfo.h",
    "socket.h",
    "sockios.h",
    "stat.h",
    "swab.h",
    "termbits.h",
    "termios.h",
    "types.h",
]

def _linux_arm64_generated_headers_impl(ctx):
    _validate_generated_header_family_content_ids(
        ctx.attr.family_content_ids,
        ["all"],
        "linux_arm64_generated_headers",
    )
    base = ctx.label.name + ".headers"
    cc_toolchain = find_cpp_toolchain(ctx)
    feature_configuration = _cc_feature_configuration(ctx, cc_toolchain)
    config = ctx.attr.config[LinuxConfigInfo]
    source_root = _linux_source_root_path(ctx)
    headers = []

    if len(ctx.files.arm64_cfi_h) > 1:
        fail("linux_arm64_generated_headers requires at most one arm64 cfi.h source")
    asm_generic_wrappers = _ARM64_ASM_GENERIC_WRAPPERS

    # arm64 gained its own cfi.h in Linux 6.18; older kernels use the mandatory asm-generic fallback.
    if not ctx.files.arm64_cfi_h:
        asm_generic_wrappers = ["cfi.h"] + asm_generic_wrappers

    for header in asm_generic_wrappers:
        out = ctx.actions.declare_file(base + "/arch/arm64/include/generated/asm/" + header)
        ctx.actions.write(
            output = out,
            content = "#include <asm-generic/%s>\n" % header,
        )
        headers.append(out)

    uapi_include_dir = None
    for header in _ARM64_UAPI_ASM_GENERIC_WRAPPERS:
        out = ctx.actions.declare_file(base + "/arch/arm64/include/generated/uapi/asm/" + header)
        ctx.actions.write(
            output = out,
            content = "#include <asm-generic/%s>\n" % header,
        )
        if uapi_include_dir == None:
            uapi_include_dir = out.dirname[:-len("/asm")]
        headers.append(out)

    cpucap_defs = ctx.actions.declare_file(base + "/arch/arm64/include/generated/asm/cpucap-defs.h")
    sysreg_defs = ctx.actions.declare_file(base + "/arch/arm64/include/generated/asm/sysreg-defs.h")
    arm64_args = ctx.actions.args()
    arm64_args.add("-cpucaps", ctx.file.cpucaps)
    arm64_args.add("-cpucaps_out", cpucap_defs)
    arm64_args.add("-sysreg", ctx.file.sysreg)
    arm64_args.add("-sysreg_out", sysreg_defs)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._arm64headers,
        inputs = [ctx.file.cpucaps, ctx.file.sysreg],
        outputs = [cpucap_defs, sysreg_defs],
        arguments = [arm64_args],
        mnemonic = "LinuxARM64GeneratedHeaders",
        progress_message = "Generating Linux arm64 architecture headers %{label}",
    )
    headers.extend([cpucap_defs, sysreg_defs])

    timeconst = ctx.actions.declare_file(base + "/include/generated/timeconst.h")
    timeconst_args = ctx.actions.args()
    timeconst_args.add("-config", config.config)
    timeconst_args.add("-out", timeconst)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._timeconst,
        inputs = depset([], transitive = [config.files]),
        outputs = [timeconst],
        arguments = [timeconst_args],
        mnemonic = "LinuxTimeconstHeader",
        progress_message = "Generating Linux time constants %{label}",
    )
    headers.append(timeconst)

    compile_h = ctx.actions.declare_file(base + "/include/generated/compile.h")
    linux_version_h = ctx.actions.declare_file(base + "/include/generated/uapi/linux/version.h")
    utsrelease_h = ctx.actions.declare_file(base + "/include/generated/utsrelease.h")
    utsversion_h = ctx.actions.declare_file(base + "/include/generated/utsversion.h")
    version_args = ctx.actions.args()
    version_args.add("-config", config.config)
    version_args.add("-kernel_release", config.kernel_release)
    version_args.add("-kernel_version", config.kernel_version)
    version_args.add("-compile_out", compile_h)
    version_args.add("-linux_version_out", linux_version_h)
    version_args.add("-utsrelease_out", utsrelease_h)
    version_args.add("-utsversion_out", utsversion_h)
    version_args.add("-machine", "aarch64")
    version_args.add("-compiler", _linux_compiler_version_string())
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._versionheaders,
        inputs = [config.config, config.kernel_release],
        outputs = [compile_h, linux_version_h, utsrelease_h, utsversion_h],
        arguments = [version_args],
        mnemonic = "LinuxVersionHeaders",
        progress_message = "Generating Linux version headers %{label}",
    )
    headers.extend([compile_h, linux_version_h, utsrelease_h, utsversion_h])

    syscall_specs = [
        (ctx.file.syscall_32_tbl, base + "/arch/arm64/include/generated/asm/syscall_table_32.h", "common,32", False, "", "", True),
        (ctx.file.syscall_64_tbl, base + "/arch/arm64/include/generated/asm/syscall_table_64.h", "common,64,renameat,rlimit,memfd_secret", False, "", "", True),
        (ctx.file.syscall_32_tbl, base + "/arch/arm64/include/generated/asm/unistd_32.h", "common,32", True, "", "", False),
        (ctx.file.syscall_32_tbl, base + "/arch/arm64/include/generated/asm/unistd_compat_32.h", "common,32", True, "", "compat32_", False),
        (ctx.file.syscall_64_tbl, base + "/arch/arm64/include/generated/uapi/asm/unistd_64.h", "common,64,renameat,rlimit,memfd_secret", True, "", "", False),
    ]
    for table, path, abis, emit_nr, offset, prefix, table_header in syscall_specs:
        out = ctx.actions.declare_file(path)
        if uapi_include_dir == None and "/generated/uapi/asm/" in path:
            uapi_include_dir = out.dirname[:-len("/asm")]
        args = ctx.actions.args()
        args.add("-in", table)
        args.add("-out", out)
        args.add("-abis", abis)
        if emit_nr:
            args.add("-emit-nr")
        if offset:
            args.add("-offset", offset)
        if prefix:
            args.add("-prefix", prefix)
        if table_header:
            path_mapped_run(
                ctx.actions,
                executable = ctx.executable._syscalltbl,
                inputs = [table],
                outputs = [out],
                arguments = [args],
                mnemonic = "LinuxSyscallTableHeader",
                progress_message = "Generating Linux syscall table header %{label}",
            )
        else:
            path_mapped_run(
                ctx.actions,
                executable = ctx.executable._syscallhdr,
                inputs = [table],
                outputs = [out],
                arguments = [args],
                mnemonic = "LinuxSyscallHeader",
                progress_message = "Generating Linux syscall header %{label}",
            )
        headers.append(out)

    include_dirs = [
        headers[0].dirname[:-len("/asm")],
        timeconst.dirname[:-len("/generated")],
        linux_version_h.dirname[:-len("/linux")],
        uapi_include_dir,
    ]
    include_dir_anchors = _directory_anchors(headers, include_dirs)
    bounds_h = _linux_offsets_header(
        ctx,
        cc_toolchain,
        feature_configuration,
        config,
        source_root,
        include_dirs,
        ctx.file.bounds_c,
        base + "/include/generated/bounds.h",
        "__LINUX_BOUNDS_H__",
        headers,
        include_dir_anchors = include_dir_anchors,
        srcarch = "arm64",
    )
    headers.append(bounds_h)
    asm_offsets_h = _linux_offsets_header(
        ctx,
        cc_toolchain,
        feature_configuration,
        config,
        source_root,
        include_dirs,
        ctx.file.asm_offsets_c,
        base + "/include/generated/asm-offsets.h",
        "__ASM_OFFSETS_H__",
        headers,
        include_dir_anchors = include_dir_anchors,
        srcarch = "arm64",
    )
    headers.append(asm_offsets_h)
    generated_cflags = ctx.actions.declare_file(base + "/include/generated/bazel_arm64_cflags.rsp")
    stackprotector_args = ctx.actions.args()
    stackprotector_args.add("-asm_offsets", asm_offsets_h)
    stackprotector_args.add("-stackprotector_config", config.config)
    stackprotector_args.add("-stackprotector_out", generated_cflags)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._arm64headers,
        inputs = depset([asm_offsets_h], transitive = [config.files]),
        outputs = [generated_cflags],
        arguments = [stackprotector_args],
        mnemonic = "LinuxARM64StackProtectorFlags",
        progress_message = "Generating Linux arm64 stack protector flags %{label}",
    )
    headers.append(generated_cflags)
    if len(ctx.files.rq_offsets_c) > 1:
        fail("linux_arm64_generated_headers requires at most one rq-offsets.c source")
    if ctx.files.rq_offsets_c:
        rq_offsets_h = _linux_offsets_header(
            ctx,
            cc_toolchain,
            feature_configuration,
            config,
            source_root,
            include_dirs,
            ctx.files.rq_offsets_c[0],
            base + "/include/generated/rq-offsets.h",
            "__RQ_OFFSETS_H__",
            headers,
            include_dir_anchors = include_dir_anchors,
            srcarch = "arm64",
        )
        headers.append(rq_offsets_h)
    hyp_constants_h = _linux_offsets_header(
        ctx,
        cc_toolchain,
        feature_configuration,
        config,
        source_root,
        include_dirs,
        ctx.file.hyp_constants_c,
        base + "/arch/arm64/kvm/hyp_constants.h",
        "__HYP_CONSTANTS_H__",
        headers,
        include_dir_anchors = include_dir_anchors,
        srcarch = "arm64",
        extra_include_dirs = [source_root + "/arch/arm64/kvm/hyp/include"],
    )
    headers.append(hyp_constants_h)
    include_dirs.append(hyp_constants_h.dirname)
    include_dir_anchors = _directory_anchors(headers, include_dirs)

    vdso = _linux_arm64_vdso_outputs(
        ctx,
        cc_toolchain,
        feature_configuration,
        config,
        source_root,
        include_dirs,
        include_dir_anchors,
        headers,
        base,
    )
    headers.extend([vdso.offsets, vdso.so])

    files = depset(headers)
    families = {
        "all": _generated_header_family_info(
            arch = "arm64",
            cflags = generated_cflags,
            content_id = ctx.attr.family_content_ids["all"],
            files = headers,
            include_dirs = include_dirs,
            name = "all",
            srcarch = "arm64",
            vdsomunge = ctx.executable.vdsomunge,
        ),
    }
    return [
        DefaultInfo(files = files),
        LinuxGeneratedHeadersInfo(
            arch = "arm64",
            cflags = generated_cflags,
            families = families,
            files = files,
            include_dir_anchors = _directory_anchors(headers, include_dirs),
            include_dirs = include_dirs,
            srcarch = "arm64",
            vdsomunge = ctx.executable.vdsomunge,
        ),
    ]

linux_arm64_generated_headers = rule(
    implementation = _linux_arm64_generated_headers_impl,
    attrs = {
        "arch": attr.string(default = "arm64"),
        "arm64_cfi_h": attr.label(allow_files = True, mandatory = True),
        "asm_offsets_c": attr.label(allow_single_file = True, mandatory = True),
        "bounds_c": attr.label(allow_single_file = True, mandatory = True),
        "config": attr.label(providers = [LinuxConfigInfo], mandatory = True),
        "cpucaps": attr.label(allow_single_file = True, mandatory = True),
        "family_content_ids": attr.string_dict(
            mandatory = True,
            doc = "Map of generated-header family names to full SHA-256 content identities.",
        ),
        "hyp_constants_c": attr.label(allow_single_file = True, mandatory = True),
        "rq_offsets_c": attr.label(allow_files = True, mandatory = True),
        "source_root": attr.label(allow_single_file = True, mandatory = True),
        "source_tree": attr.label_list(allow_files = True),
        "syscall_32_tbl": attr.label(allow_single_file = True, mandatory = True),
        "syscall_64_tbl": attr.label(allow_single_file = True, mandatory = True),
        "sysreg": attr.label(allow_single_file = True, mandatory = True),
        "vdsomunge": attr.label(
            cfg = "exec",
            executable = True,
            mandatory = True,
        ),
        "_arm64headers": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/arm64headers"),
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
        "_runandwrite": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/runandwrite"),
            executable = True,
        ),
        "_offsetsheader": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/offsetsheader"),
            executable = True,
        ),
        "_flagfilter": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/flagfilter"),
            executable = True,
        ),
        "_syscallhdr": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/syscallhdr"),
            executable = True,
        ),
        "_syscalltbl": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/syscalltbl"),
            executable = True,
        ),
        "_timeconst": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/timeconst"),
            executable = True,
        ),
        "_versionheaders": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/versionheaders"),
            executable = True,
        ),
    },
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Generates the arm64 header subset normally produced before compiling Linux C objects.",
)

def _declare_linux_config(ctx, config_dir, flags, kernel_version):
    config = ctx.actions.declare_file(config_dir + "/.config")
    auto_conf = ctx.actions.declare_file(config_dir + "/include/config/auto.conf")
    auto_conf_cmd = ctx.actions.declare_file(config_dir + "/include/config/auto.conf.cmd")
    autoconf_h = ctx.actions.declare_file(config_dir + "/include/generated/autoconf.h")
    integer_wrap_h = ctx.actions.declare_file(config_dir + "/include/generated/integer-wrap.h")
    rustc_cfg = ctx.actions.declare_file(config_dir + "/include/generated/rustc_cfg")
    kernel_release = ctx.actions.declare_file(config_dir + "/include/config/kernel.release")
    aflags = ctx.actions.declare_file(config_dir + "/include/generated/bazel_kbuild_aflags.rsp")
    cflags = ctx.actions.declare_file(config_dir + "/include/generated/bazel_kbuild_cflags.rsp")

    outputs = [config, auto_conf, auto_conf_cmd, autoconf_h, integer_wrap_h, rustc_cfg, kernel_release, aflags, cflags]
    files = depset(outputs)
    include_dir = autoconf_h.dirname
    if include_dir.endswith("/generated"):
        include_dir = include_dir[:-len("/generated")]
    return struct(
        info = LinuxConfigInfo(
            aflags = aflags,
            auto_conf = auto_conf,
            auto_conf_cmd = auto_conf_cmd,
            autoconf_h = autoconf_h,
            config = config,
            config_flags = flags,
            cflags = cflags,
            files = files,
            include_dir = include_dir,
            include_dir_anchor = directory_anchor(autoconf_h, include_dir),
            kernel_release = kernel_release,
            kernel_version = kernel_version,
            rustc_cfg = rustc_cfg,
            rustc_probe = None,
        ),
        integer_wrap_h = integer_wrap_h,
        outputs = outputs,
    )

def _materialize_linux_config(ctx, config_dir, flags, arch, version):
    declared = _declare_linux_config(ctx, config_dir, flags, version)
    info = declared.info
    config_lines = []
    header_lines = [
        "/* Generated by Bazel linux_config. */",
        "#ifndef __GENERATED_AUTOCONF_H__",
        "#define __GENERATED_AUTOCONF_H__",
    ]
    rustc_lines = []
    for key in sorted(flags.keys()):
        value = flags[key]
        config_lines.append("%s=%s" % (key, value))
        header = _config_value_to_header_suffix(key, value)
        if header != None:
            header_lines.append(header)
        if value in ["y", "m"]:
            rustc_lines.append("--cfg=%s" % key)
        if value != "n":
            rendered_value = value if value.startswith('"') else '"%s"' % value
            rustc_lines.append("--cfg=%s=%s" % (key, rendered_value))
    header_lines.append("#endif")

    localversion = _unquote(flags.get("CONFIG_LOCALVERSION", ""))
    kernel_release_value = version + localversion

    ctx.actions.write(info.config, "\n".join(config_lines) + "\n")
    ctx.actions.write(info.auto_conf, "\n".join(config_lines) + "\n")
    auto_conf_cmd_args = ctx.actions.args()
    auto_conf_cmd_args.add(info.auto_conf, format = "cmd_%s := bazel linux_config")
    ctx.actions.write(info.auto_conf_cmd, auto_conf_cmd_args)
    ctx.actions.write(info.autoconf_h, "\n".join(header_lines) + "\n")
    ctx.actions.write(declared.integer_wrap_h, "")
    ctx.actions.write(info.rustc_cfg, "\n".join(rustc_lines) + "\n")
    ctx.actions.write(info.kernel_release, kernel_release_value + "\n")

    if arch:
        cflags_args = ctx.actions.args()
        cflags_args.add("-config", info.config)
        cflags_args.add("-arch", arch)
        cflags_args.add("-out", info.cflags)
        cflags_args.add("-asm_out", info.aflags)
        path_mapped_run(
            ctx.actions,
            executable = ctx.executable._kernelflags,
            inputs = [info.config],
            outputs = [info.aflags, info.cflags],
            arguments = [cflags_args],
            mnemonic = "LinuxKernelCFlags",
            progress_message = "Generating Linux compiler flags %{label}",
        )
    else:
        ctx.actions.write(info.aflags, "")
        ctx.actions.write(info.cflags, "")

    return info

def _linux_config_impl(ctx):
    info = _materialize_linux_config(
        ctx,
        ctx.label.name + ".config_tree",
        _config_flags(ctx),
        ctx.attr.arch,
        ctx.attr.version,
    )
    return [
        DefaultInfo(files = info.files),
        info,
        OutputGroupInfo(config = depset([info.config])),
    ]

linux_config = rule(
    implementation = _linux_config_impl,
    attrs = {
        "arch": attr.string(
            doc = "Linux ARCH. When set, derive global compiler and assembler flags from this config.",
        ),
        "config": attr.label(providers = [KconfigInfo]),
        "config_flags": attr.string_dict(),
        "version": attr.string(default = "6.18.2"),
        "_kernelflags": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/kernelflags:kernelflags"),
            executable = True,
        ),
    },
    doc = "Materializes Linux config files used by native compile and link actions.",
)

def _validate_content_id(content_id, what):
    if not content_id:
        fail("%s must be a full lowercase SHA-256 content ID" % what)
    if len(content_id) != 64:
        fail("%s must be a full lowercase SHA-256 content ID, got %r" % (what, content_id))
    for index in range(len(content_id)):
        if content_id[index] not in "0123456789abcdef":
            fail("%s must be a full lowercase SHA-256 content ID, got %r" % (what, content_id))

def _validate_generated_header_family_content_ids(content_ids, expected_names, what):
    if sorted(content_ids.keys()) != sorted(expected_names):
        fail(
            "%s family_content_ids has families %s, expected %s" %
            (what, sorted(content_ids.keys()), sorted(expected_names)),
        )
    for name, content_id in content_ids.items():
        _validate_content_id(content_id, "%s family %s content ID" % (what, name))

def _generated_header_family_info(
        arch,
        cflags,
        content_id,
        files,
        include_dirs,
        name,
        srcarch,
        vdsomunge):
    include_dir_anchors = _available_directory_anchors(files, include_dirs)
    return struct(
        arch = arch,
        cflags = cflags,
        content_id = content_id,
        files = depset(files),
        include_dir_anchors = include_dir_anchors,
        include_dirs = [
            include_dir
            for include_dir in include_dirs
            if include_dir in include_dir_anchors
        ],
        name = name,
        srcarch = srcarch,
        vdsomunge = vdsomunge,
    )

def _parse_config_payload(payload_id, content):
    flags = {}
    previous_key = ""
    for line in content.split("\n"):
        if not line:
            continue
        if line.startswith("# ") and line.endswith(" is not set"):
            key = line[len("# "):-len(" is not set")]
            value = "n"
        else:
            separator = line.find("=")
            if separator < 0:
                fail("config payload %s has invalid line %r" % (payload_id, line))
            key = line[:separator]
            value = line[separator + 1:]
        if not key.startswith("CONFIG_"):
            fail("config payload %s has invalid key %r" % (payload_id, key))
        if key in flags:
            fail("config payload %s repeats key %s" % (payload_id, key))
        if previous_key and key < previous_key:
            fail("config payload %s is not sorted: %s follows %s" % (payload_id, key, previous_key))
        flags[key] = value
        previous_key = key
    return flags

def _merge_compile_environment_generated_header_families(environment_id, family_ids, families_by_id):
    if not family_ids:
        return None
    infos = []
    seen_ids = {}
    seen_names = {}
    for family_id in family_ids:
        _validate_content_id(family_id, "compile environment %s generated-header family" % environment_id)
        if family_id in seen_ids:
            fail("compile environment %s repeats generated-header family %s" % (environment_id, family_id))
        seen_ids[family_id] = True
        if family_id not in families_by_id:
            fail(
                "compile environment %s references unknown generated-header family %s" %
                (environment_id, family_id),
            )
        info = families_by_id[family_id]
        if info.name in seen_names:
            fail("compile environment %s repeats generated-header family name %s" % (environment_id, info.name))
        seen_names[info.name] = True
        infos.append(info)
    if "all" in seen_names and len(seen_names) != 1:
        fail("compile environment %s mixes all with precise generated-header families" % environment_id)
    first = infos[0]
    cflags = first.cflags
    vdsomunge = first.vdsomunge
    include_dirs = []
    include_dir_anchors = {}
    for info in infos:
        if info.arch != first.arch or info.srcarch != first.srcarch:
            fail("compile environment %s mixes generated header architectures" % environment_id)
        if info.cflags != None:
            if cflags != None and info.cflags != cflags:
                fail("compile environment %s has multiple generated-header cflags files" % environment_id)
            cflags = info.cflags
        if info.vdsomunge != None:
            if vdsomunge != None and info.vdsomunge != vdsomunge:
                fail("compile environment %s has multiple vdsomunge tools" % environment_id)
            vdsomunge = info.vdsomunge
        include_dirs.extend(info.include_dirs)
        include_dir_anchors.update(info.include_dir_anchors)
    return LinuxGeneratedHeadersInfo(
        arch = first.arch,
        cflags = cflags,
        families = {
            info.name: info
            for info in infos
        },
        files = depset(transitive = [info.files for info in infos]),
        include_dir_anchors = include_dir_anchors,
        include_dirs = _unique_strings(include_dirs),
        srcarch = first.srcarch,
        vdsomunge = vdsomunge,
    )

def _linux_compile_environment_index_impl(ctx):
    if not ctx.attr.expected_abi:
        fail("linux_compile_environment_index %s expected_abi must be a non-empty string" % ctx.label)
    raw_environments = {}
    referenced_payloads = {}
    for environment_id, encoded in ctx.attr.compile_environments.items():
        _validate_content_id(environment_id, "compile environment ID")
        environment = json.decode(encoded)
        if type(environment) != "dict":
            fail("compile environment %s must decode to an object" % environment_id)
        unknown_keys = [
            key
            for key in environment.keys()
            if key not in ["abi", "config_payload", "generated_header_families"]
        ]
        if unknown_keys:
            fail("compile environment %s has unknown field(s): %s" % (environment_id, ", ".join(sorted(unknown_keys))))
        missing_keys = [
            key
            for key in ["abi", "config_payload", "generated_header_families"]
            if key not in environment
        ]
        if missing_keys:
            fail("compile environment %s is missing field(s): %s" % (environment_id, ", ".join(missing_keys)))
        payload_id = environment["config_payload"]
        family_ids = environment["generated_header_families"]
        abi = environment["abi"]
        if type(abi) != "string" or not abi:
            fail("compile environment %s abi must be a non-empty string" % environment_id)
        if abi != ctx.attr.expected_abi:
            fail(
                "compile environment %s abi %r does not match expected_abi %r" %
                (environment_id, abi, ctx.attr.expected_abi),
            )
        if type(payload_id) != "string":
            fail("compile environment %s config_payload must be a content ID" % environment_id)
        if type(family_ids) != "list":
            fail("compile environment %s generated_header_families must be a list" % environment_id)
        _validate_content_id(payload_id, "compile environment %s config payload" % environment_id)
        for family_id in family_ids:
            if type(family_id) != "string":
                fail("compile environment %s generated-header family IDs must be strings" % environment_id)
        raw_environments[environment_id] = struct(
            config_payload_id = payload_id,
            generated_header_family_ids = list(family_ids),
        )
        referenced_payloads[payload_id] = True

    config_payload_files = {}
    for target, payload_id in ctx.attr.config_payload_files.items():
        _validate_content_id(payload_id, "config payload file ID")
        if payload_id in config_payload_files:
            fail(
                "config payload ID %s is provided by both %s and %s" %
                (payload_id, config_payload_files[payload_id].owner, target.label),
            )
        config_payload_files[payload_id] = _single_file(target, "config_payload_files")
    for payload_id, content in ctx.attr.config_payload_values.items():
        _validate_content_id(payload_id, "config payload value ID")
        if payload_id not in config_payload_files:
            fail("config_payload_values contains %s without a matching config_payload_files entry" % payload_id)
        _parse_config_payload(payload_id, content)
    for payload_id in ctx.attr.config_payloads.keys():
        _validate_content_id(payload_id, "config payload ID")
        if payload_id in config_payload_files:
            fail("config payload %s is provided as both inline content and a file" % payload_id)

    inline_payload_contents_by_bucket = {}
    inline_payload_outputs_by_bucket = {}
    file_payload_inputs_by_bucket = {}
    file_payload_outputs_by_bucket = {}
    config_payloads = {}
    for payload_id in sorted(referenced_payloads.keys()):
        content = ctx.attr.config_payloads.get(payload_id)
        payload_file = config_payload_files.get(payload_id)
        if content == None and payload_file == None:
            fail("compile environment index %s references unknown config payload %s" % (ctx.label, payload_id))
        _validate_content_id(payload_id, "config payload ID")
        config_flags = {}
        if content != None:
            config_flags = _parse_config_payload(payload_id, content)
        elif payload_id in ctx.attr.config_payload_values:
            config_flags = _parse_config_payload(payload_id, ctx.attr.config_payload_values[payload_id])
        declared = _declare_linux_config(
            ctx,
            ctx.label.name + ".config_payloads/" + payload_id,
            config_flags,
            ctx.attr.version,
        )
        bucket = payload_id[0]
        if content != None:
            inline_payload_contents_by_bucket.setdefault(bucket, {})[payload_id] = content
            inline_payload_outputs_by_bucket.setdefault(bucket, []).extend(declared.outputs)
        else:
            file_payload_inputs_by_bucket.setdefault(bucket, {})[payload_id] = payload_file
            file_payload_outputs_by_bucket.setdefault(bucket, []).extend(declared.outputs)
        config_payloads[payload_id] = declared.info

    for bucket in sorted(inline_payload_contents_by_bucket.keys()):
        manifest = ctx.actions.declare_file(ctx.label.name + ".config_payloads_%s_manifest.json" % bucket)
        ctx.actions.write(
            manifest,
            json.encode({
                "arch": ctx.attr.arch,
                "payloads": inline_payload_contents_by_bucket[bucket],
                "version": ctx.attr.version,
            }) + "\n",
        )
        payload_args = ctx.actions.args()
        payload_args.add("-batch_manifest", manifest)
        payload_args.add("-batch_out_dir")
        first_payload_id = sorted(inline_payload_contents_by_bucket[bucket].keys())[0]
        first_payload = config_payloads[first_payload_id]
        payload_root = first_payload.config.dirname.rsplit("/", 1)[0]
        add_directory_arg(
            payload_args,
            directory_anchor(first_payload.config, payload_root),
        )
        path_mapped_run(
            ctx.actions,
            executable = ctx.executable._kernelflags,
            inputs = [manifest],
            outputs = inline_payload_outputs_by_bucket[bucket],
            arguments = [payload_args],
            mnemonic = "LinuxConfigPayloads",
            progress_message = "Materializing Linux config payloads %{label}",
        )

    for bucket in sorted(file_payload_inputs_by_bucket.keys()):
        payload_args = ctx.actions.args()
        payload_args.add("-batch_out_dir")
        first_payload_id = sorted(file_payload_inputs_by_bucket[bucket].keys())[0]
        first_payload = config_payloads[first_payload_id]
        payload_root = first_payload.config.dirname.rsplit("/", 1)[0]
        add_directory_arg(
            payload_args,
            directory_anchor(first_payload.config, payload_root),
        )
        payload_args.add("-arch", ctx.attr.arch)
        payload_args.add("-version", ctx.attr.version)
        inputs = []
        for payload_id in sorted(file_payload_inputs_by_bucket[bucket].keys()):
            payload_file = file_payload_inputs_by_bucket[bucket][payload_id]
            payload_args.add(payload_file, format = "-batch_payload=%s=%%s" % payload_id)
            inputs.append(payload_file)
        path_mapped_run(
            ctx.actions,
            executable = ctx.executable._kernelflags,
            inputs = inputs,
            outputs = file_payload_outputs_by_bucket[bucket],
            arguments = [payload_args],
            mnemonic = "LinuxConfigPayloads",
            progress_message = "Materializing Linux config payloads %{label}",
        )

    generated_header_families = {}
    header_targets = {
        str(target.label): target
        for target in ctx.attr.generated_headers
    }
    for label in sorted(header_targets.keys()):
        target = header_targets[label]
        info = target[LinuxGeneratedHeadersInfo]
        for family_name in sorted(info.families.keys()):
            family = info.families[family_name]
            if family.name != family_name:
                fail(
                    "generated header target %s publishes family %s under name %s" %
                    (target.label, family.name, family_name),
                )
            family_id = family.content_id
            _validate_content_id(
                family_id,
                "generated header target %s family %s content ID" % (target.label, family_name),
            )
            if family_id in generated_header_families:
                existing = generated_header_families[family_id]
                if (
                    existing.name != family.name or
                    existing.arch != family.arch or
                    existing.srcarch != family.srcarch
                ):
                    fail(
                        "generated-header family %s maps to incompatible targets including %s" %
                        (family_id, target.label),
                    )
                continue
            generated_header_families[family_id] = family

    environments = {}
    for environment_id in sorted(raw_environments.keys()):
        raw = raw_environments[environment_id]
        environments[environment_id] = struct(
            config = config_payloads[raw.config_payload_id],
            generated_headers = _merge_compile_environment_generated_header_families(
                environment_id,
                raw.generated_header_family_ids,
                generated_header_families,
            ),
        )

    return [
        DefaultInfo(files = depset(transitive = [info.files for info in config_payloads.values()])),
        LinuxCompileEnvironmentIndexInfo(
            environments = environments,
        ),
    ]

linux_compile_environment_index = rule(
    implementation = _linux_compile_environment_index_impl,
    attrs = {
        "arch": attr.string(
            doc = "Linux ARCH used to derive compiler and assembler flags for materialized payloads.",
        ),
        "compile_environments": attr.string_dict(
            mandatory = True,
            doc = "Map of full compile-environment SHA-256 IDs to JSON config-payload/generated-header-family references.",
        ),
        "config_payloads": attr.string_dict(
            doc = "Legacy map of full config-payload SHA-256 IDs to canonical sorted .config text.",
        ),
        "config_payload_files": attr.label_keyed_string_dict(
            allow_files = True,
            doc = "Map of one-file config payload targets to their full SHA-256 IDs.",
        ),
        "config_payload_values": attr.string_dict(
            doc = "Analysis-visible payload text for file-backed configurations that require CONFIG_* branching.",
        ),
        "generated_headers": attr.label_list(
            providers = [LinuxGeneratedHeadersInfo],
            doc = "Generated-header providers whose content-addressed families are indexed.",
        ),
        "expected_abi": attr.string(
            mandatory = True,
            doc = "Action ABI required for every indexed compile environment.",
        ),
        "version": attr.string(default = "6.18.2"),
        "_kernelflags": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/kernelflags:kernelflags"),
            executable = True,
        ),
    },
    doc = "Materializes and indexes content-addressed Linux compile environments without per-environment targets.",
)

def _configure_linux_probe_env(env):
    env.setdefault("CC", "clang")
    env.setdefault("CC_VERSION_TEXT", "clang version 22.1.8None")
    env.setdefault("LD", "ld.lld")
    env.setdefault("NM", "llvm-nm")
    env.setdefault("OBJCOPY", "llvm-objcopy")
    env.setdefault("AR", "llvm-ar")
    env.setdefault("CLANG_FLAGS", "-fintegrated-as")
    env["RUSTC"] = "rustc"
    env.setdefault("PAHOLE", "pahole")
    env.setdefault("BINDGEN", "bindgen")
    env.setdefault("PYTHON3", "python3")

def _linux_compiler_version_string():
    return "clang version 22.1.8None, LLD 22.1.8"

def _rustc_tool_inputs(toolchain):
    transitive = []
    if toolchain.rustc_lib != None:
        transitive.append(toolchain.rustc_lib)
    return depset([toolchain.rustc], transitive = transitive)

def _materialize_rust_toolchain_probe(ctx):
    target = ctx.toolchains[_RUST_TOOLCHAIN_TYPE]
    host_target = ctx.attr._host_rust_toolchain
    host = host_target[platform_common.ToolchainInfo]
    out = ctx.actions.declare_file(ctx.label.name + ".rust_toolchain.json")
    args = ctx.actions.args()
    args.add("-rustc", target.rustc)
    args.add("-host-rustc", host.rustc)
    args.add("-out", out)
    args.add("-minimum", ctx.attr.minimum_rustc_version)
    for name in sorted(target.env.keys()):
        args.add("-env", name + "=" + target.env[name])
    for name in sorted(host.env.keys()):
        args.add("-host-env", name + "=" + host.env[name])
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._rusttoolchainprobe,
        inputs = depset(transitive = [
            _rustc_tool_inputs(target),
            _rustc_tool_inputs(host),
        ]),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxRustToolchainProbe",
        progress_message = "Validating selected target and host Rust toolchains %{label}",
    )
    return out

def _resolve_linux_config(ctx, rust_toolchain_probe):
    config_dir = ctx.label.name + ".config_tree"
    config = ctx.actions.declare_file(config_dir + "/.config")
    auto_conf = ctx.actions.declare_file(config_dir + "/include/config/auto.conf")
    auto_conf_cmd = ctx.actions.declare_file(config_dir + "/include/config/auto.conf.cmd")
    autoconf_h = ctx.actions.declare_file(config_dir + "/include/generated/autoconf.h")
    integer_wrap_h = ctx.actions.declare_file(config_dir + "/include/generated/integer-wrap.h")
    rustc_cfg = ctx.actions.declare_file(config_dir + "/include/generated/rustc_cfg")
    kernel_release = ctx.actions.declare_file(config_dir + "/include/config/kernel.release")
    aflags = ctx.actions.declare_file(config_dir + "/include/generated/bazel_kbuild_aflags.rsp")
    cflags = ctx.actions.declare_file(config_dir + "/include/generated/bazel_kbuild_cflags.rsp")

    source_root = _linux_source_root_path(ctx) if ctx.file.source_root else _linux_execroot_dir(ctx.file.root)
    vars = dict(ctx.attr.vars)
    vars.setdefault("srctree", source_root)
    env = dict(ctx.attr.env)
    _configure_linux_probe_env(env)

    fragment = ctx.attr.config[KconfigInfo].config
    args = ctx.actions.args()
    args.add("-root", _linux_execroot_path(ctx.file.root))
    args.add("-srctree", source_root)
    extra_kconfig_inputs = []
    for target, prefix in ctx.attr.extra_kconfigs.items():
        file = _single_file(target, "extra_kconfigs")
        args.add("-source_root_map", "%s=%s" % (prefix, _linux_execroot_dir(file)))
        args.add("-kconfig_extra", "%s=%s" % (prefix, _linux_execroot_path(file)))
        extra_kconfig_inputs.append(file)
    if ctx.attr.config_mode:
        args.add("-config_mode", ctx.attr.config_mode)
    args.add("-resolve_config")
    args.add(
        fragment,
        format = (ctx.attr.config_name if ctx.attr.config_name else ctx.label.name) + "=%s",
    )
    args.add("-resolved_config_out", config)
    args.add("-resolved_auto_conf_out", auto_conf)
    args.add("-resolved_auto_conf_cmd_out", auto_conf_cmd)
    args.add("-resolved_autoconf_out", autoconf_h)
    args.add("-resolved_rustc_cfg_out", rustc_cfg)
    args.add("-resolved_kernel_release_out", kernel_release)
    args.add("-kernel_version", ctx.attr.version)
    graph_profile = ctx.attr.graph_profile[LinuxGraphProfileInfo]
    args.add("-graph_profile_projection", graph_profile.projection)
    if rust_toolchain_probe:
        args.add("-rust_toolchain_probe", rust_toolchain_probe)
        args.add("-validate_config_equivalence")
    for key, value in sorted(vars.items()):
        args.add("-var", "%s=%s" % (key, value))
    for key, value in sorted(env.items()):
        args.add("-env", "%s=%s" % (key, value))

    inputs = [
        ctx.file.root,
        fragment,
        graph_profile.projection,
        graph_profile.validation,
    ] + ctx.files.srcs + extra_kconfig_inputs
    if rust_toolchain_probe:
        inputs.append(rust_toolchain_probe)
    if ctx.file.source_root:
        inputs.append(ctx.file.source_root)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._kconfig_parse,
        inputs = depset(
            direct = inputs,
            transitive = [graph_profile.source_inputs],
        ),
        outputs = [config, auto_conf, auto_conf_cmd, autoconf_h, rustc_cfg, kernel_release],
        arguments = [args],
        mnemonic = "LinuxResolvedConfig",
        progress_message = "Resolving Linux config %{label}",
    )
    ctx.actions.write(integer_wrap_h, "")

    cflags_args = ctx.actions.args()
    cflags_args.add("-config", config)
    cflags_args.add("-arch", env.get("ARCH", "x86"))
    cflags_args.add("-out", cflags)
    cflags_args.add("-asm_out", aflags)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._kernelflags,
        inputs = [config],
        outputs = [aflags, cflags],
        arguments = [cflags_args],
        mnemonic = "LinuxKernelCFlags",
        progress_message = "Generating Linux compiler flags %{label}",
    )

    include_dir = autoconf_h.dirname
    if include_dir.endswith("/generated"):
        include_dir = include_dir[:-len("/generated")]
    files = depset([config, auto_conf, auto_conf_cmd, autoconf_h, integer_wrap_h, rustc_cfg, kernel_release, aflags, cflags])
    return [
        DefaultInfo(files = files),
        LinuxConfigInfo(
            aflags = aflags,
            auto_conf = auto_conf,
            auto_conf_cmd = auto_conf_cmd,
            autoconf_h = autoconf_h,
            config = config,
            config_flags = dict(ctx.attr.config[KconfigInfo].config_flags),
            cflags = cflags,
            files = files,
            include_dir = include_dir,
            include_dir_anchor = directory_anchor(autoconf_h, include_dir),
            kernel_release = kernel_release,
            kernel_version = ctx.attr.version,
            rustc_cfg = rustc_cfg,
            rustc_probe = rust_toolchain_probe,
        ),
        OutputGroupInfo(config = depset([config])),
    ]

def _linux_resolved_config_impl(ctx):
    return _resolve_linux_config(ctx, None)

def _linux_rust_resolved_config_impl(ctx):
    return _resolve_linux_config(ctx, _materialize_rust_toolchain_probe(ctx))

_LINUX_RESOLVED_CONFIG_ATTRS = {
    "config": attr.label(
        providers = [KconfigInfo],
        mandatory = True,
        doc = "Imported raw Linux .config fragment to resolve.",
    ),
    "config_name": attr.string(
        doc = "Stable name passed to the Kconfig resolver. Defaults to the rule name.",
    ),
    "config_mode": attr.string(
        default = "default",
        doc = "Config resolver mode passed to kconfig_parse. Supported: default, allnoconfig.",
        values = [
            "default",
            "allnoconfig",
        ],
    ),
    "env": attr.string_dict(
        doc = "Hermetic Kconfig preprocessor environment values.",
    ),
    "extra_kconfigs": attr.label_keyed_string_dict(
        allow_files = True,
        doc = "Map of extra Kconfig labels to virtual Linux source prefixes.",
    ),
    "graph_profile": attr.label(
        mandatory = True,
        providers = [LinuxGraphProfileInfo],
        doc = "Validated consumed C/tool graph projection used during config replay.",
    ),
    "root": attr.label(
        allow_single_file = True,
        mandatory = True,
        doc = "Root Kconfig input.",
    ),
    "source_root": attr.label(
        allow_single_file = True,
        doc = "A file in the Linux source root; its directory is used as srctree.",
    ),
    "srcs": attr.label_list(
        allow_files = True,
        doc = "Additional Kconfig source files read through source statements.",
    ),
    "vars": attr.string_dict(
        doc = "Kconfig preprocessor variables.",
    ),
    "version": attr.string(default = "6.18.2"),
    "_kconfig_parse": attr.label(
        cfg = "exec",
        default = Label("//internal/cmd/kconfig_parse:kconfig_parse"),
        executable = True,
    ),
    "_kernelflags": attr.label(
        cfg = "exec",
        default = Label("//internal/cmd/kernelflags:kernelflags"),
        executable = True,
    ),
}

_linux_resolved_config = rule(
    implementation = _linux_resolved_config_impl,
    attrs = _LINUX_RESOLVED_CONFIG_ATTRS,
    doc = "Resolves an imported Linux .config fragment into Kbuild config outputs.",
)

_LINUX_RUST_RESOLVED_CONFIG_ATTRS = dict(_LINUX_RESOLVED_CONFIG_ATTRS)
_LINUX_RUST_RESOLVED_CONFIG_ATTRS.update({
    "minimum_rustc_version": attr.string(mandatory = True),
    "_host_rust_toolchain": attr.label(
        cfg = "exec",
        default = Label("@rules_rust//rust/toolchain:current_rust_toolchain"),
    ),
    "_rusttoolchainprobe": attr.label(
        cfg = "exec",
        default = Label("//internal/cmd/rusttoolchainprobe"),
        executable = True,
    ),
})

_linux_rust_resolved_config = rule(
    implementation = _linux_rust_resolved_config_impl,
    attrs = _LINUX_RUST_RESOLVED_CONFIG_ATTRS,
    toolchains = [_RUST_TOOLCHAIN_TYPE],
    doc = "Resolves a Rust-enabled Linux config against the selected rules_rust toolchain.",
)

def linux_resolved_config(name, rust_enabled = False, minimum_rustc_version = "", **kwargs):
    """Resolves an imported config without making C-only targets resolve Rust."""
    if rust_enabled:
        if not minimum_rustc_version:
            fail("minimum_rustc_version is required when rust_enabled is true")
        _linux_rust_resolved_config(
            name = name,
            minimum_rustc_version = minimum_rustc_version,
            **kwargs
        )
    else:
        if minimum_rustc_version:
            fail("minimum_rustc_version is only valid when rust_enabled is true")
        _linux_resolved_config(
            name = name,
            **kwargs
        )

def _linux_perl_runtime(ctx):
    perl_runtime = ctx.toolchains[_PERL_TOOLCHAIN].perl_runtime
    return struct(
        files = perl_runtime.runtime,
        interpreter = perl_runtime.interpreter,
    )

def _linux_compile_environment(ctx, rule_name):
    _validate_content_id(ctx.attr.compile_environment_id, "%s compile_environment_id" % rule_name)
    index = ctx.attr.compile_environment_index[LinuxCompileEnvironmentIndexInfo]
    if ctx.attr.compile_environment_id not in index.environments:
        fail(
            "%s %s references unknown compile environment %s" %
            (rule_name, ctx.label, ctx.attr.compile_environment_id),
        )
    return index.environments[ctx.attr.compile_environment_id]

def _linux_object_flag_programs(ctx):
    program_info = ctx.attr.flag_programs[LinuxFlagProgramsInfo]
    programs = program_info.programs
    for program_id, what in [
        (ctx.attr.flag_program, "flag_program"),
        (ctx.attr.remove_flag_program, "remove_flag_program"),
    ]:
        _validate_content_id(program_id, "linux_object " + what)
        if program_id not in programs:
            fail(
                "linux_object %s references unknown %s %s" %
                (ctx.label, what, program_id),
            )
    profile = program_info.graph_profile
    expected_arch = {
        "arm64": "aarch64",
        "x86": "x86_64",
    }.get(ctx.attr.arch)
    if expected_arch == None:
        fail("linux_object %s has unsupported lazy arch %r" % (ctx.label, ctx.attr.arch))
    if profile.arch != expected_arch:
        fail(
            "linux_object %s profile arch %r does not match Linux arch %r" %
            (ctx.label, profile.arch, ctx.attr.arch),
        )
    return struct(
        flags = programs[ctx.attr.flag_program],
        profile = profile,
        removals = programs[ctx.attr.remove_flag_program],
    )

def _linux_object_impl(ctx):
    _validate_content_id(ctx.attr.content_id, "linux_object content_id")
    flag_programs = _linux_object_flag_programs(ctx)
    if ctx.attr.needs_utsversion_tmp and not ctx.attr.needs_object_dir:
        fail(
            "linux_object %s needs_utsversion_tmp requires needs_object_dir" %
            ctx.label,
        )
    source_selection = _linux_source_input_group(ctx, "linux_object")
    source_file = _linux_source_input_file(
        ctx,
        source_selection,
        ctx.attr.source_input_file,
        "linux_object %s" % ctx.label,
    )
    if ctx.attr.object in [
        "certs/blacklist_hashes.o",
        "certs/revocation_certificates.o",
        "certs/system_certificates.o",
    ]:
        fail(
            "linux_object %s builds %s, but hermetic certificate embedding and signing are not implemented" %
            (ctx.label, ctx.attr.object),
        )

    cc_toolchain = find_cpp_toolchain(ctx)
    if cc_toolchain.compiler.lower().find("clang") < 0:
        fail(
            "linux_object %s requires the Hermetic LLVM Clang toolchain, got compiler %r" %
            (ctx.label, cc_toolchain.compiler),
        )
    feature_configuration = _cc_feature_configuration(ctx, cc_toolchain)
    compiler = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
    )
    linker = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = CPP_LINK_EXECUTABLE_ACTION_NAME,
    )
    compile_environment = _linux_compile_environment(ctx, "linux_object")
    config = compile_environment.config
    generated_headers = compile_environment.generated_headers
    config_values = config.config_flags
    if ctx.attr.module_root and ctx.attr.mode != "m":
        fail("linux_object %s marks a non-module object as a module root" % ctx.label)

    out = ctx.actions.declare_file(ctx.label.name + ".o")
    cmd = ctx.actions.declare_file(ctx.label.name + ".cmd")
    compile_object = _linux_compile_object_name(ctx.attr.object)
    objcopy_flags = _linux_objcopy_flags_for_object(ctx.attr.object, ctx.attr.arch)
    needs_relacheck = _linux_object_needs_relacheck(ctx.attr.object)
    if needs_relacheck and not ctx.executable.relacheck:
        fail("linux_object %s builds %s and requires relacheck" % (ctx.label, ctx.attr.object))
    needs_module_lto_link = (
        ctx.attr.mode == "m" and
        config_values.get("CONFIG_LTO_CLANG") == "y" and
        not _is_assembly_source(source_file) and
        not _linux_perlasm_kind(ctx.attr.object) and
        not _is_dtb_source(source_file) and
        (ctx.attr.module_root or (ctx.attr.objtool_force and ctx.executable.objtool))
    )
    compile_out = out
    if objcopy_flags:
        compile_out = ctx.actions.declare_file(ctx.label.name + ".obj/" + compile_object)
    elif ctx.executable.objtool or needs_module_lto_link:
        compile_out = ctx.actions.declare_file(
            ctx.label.name + ".obj/objtool-input/" + ctx.attr.object,
        )
    objtool_out = out
    if ctx.executable.objtool and objcopy_flags:
        objtool_out = ctx.actions.declare_file(
            ctx.label.name + ".obj/objtool-output/" + compile_object,
        )
    objcopy_out = out
    if objcopy_flags and needs_relacheck:
        objcopy_out = ctx.actions.declare_file(ctx.label.name + ".obj/" + ctx.attr.object)
    generated_object_headers = []
    exported_generated_headers = []
    exported_generated_include_dirs = []
    generated_sources = []
    utsversion_tmp = None
    object_root_file = None
    src = source_file
    if _is_shipped_c_source(source_file):
        src = ctx.actions.declare_file(ctx.label.name + ".obj/" + ctx.attr.object[:-len(".o")] + ".c")
        ctx.actions.expand_template(
            template = source_file,
            output = src,
            substitutions = {},
        )
        generated_sources.append(src)
    if ctx.attr.object.endswith(".asn1.o"):
        if not ctx.executable.asn1_compiler:
            fail("linux_object %s builds an ASN.1 source and requires asn1_compiler" % ctx.label)
        generated_c = ctx.actions.declare_file(ctx.label.name + ".obj/" + ctx.attr.object[:-len(".o")] + ".c")
        generated_h = ctx.actions.declare_file(ctx.label.name + ".obj/" + ctx.attr.object[:-len(".o")] + ".h")
        asn1_args = ctx.actions.args()
        asn1_args.add(source_file)
        asn1_args.add(generated_c)
        asn1_args.add(generated_h)
        path_mapped_run(
            ctx.actions,
            executable = ctx.executable.asn1_compiler,
            inputs = [source_file],
            outputs = [generated_c, generated_h],
            arguments = [asn1_args],
            mnemonic = "LinuxASN1Compiler",
            progress_message = "Generating Linux ASN.1 parser %{label}",
        )
        src = generated_c
        generated_sources.extend([generated_c, generated_h])
        generated_object_headers.append(generated_h)
        exported_generated_headers.append(generated_h)
        exported_generated_include_dirs.append(generated_h.dirname)
    perlasm_kind = _linux_perlasm_kind(ctx.attr.object)
    if perlasm_kind:
        generated = ctx.actions.declare_file(ctx.label.name + ".obj/" + ctx.attr.object[:-len(".o")] + ".S")
        perl_runtime = _linux_perl_runtime(ctx)
        if perlasm_kind == "arm64_with_args":
            perlasm_args = ctx.actions.args()
            perlasm_args.add(source_file)
            perlasm_args.add("void")
            perlasm_args.add(generated)
            path_mapped_run(
                ctx.actions,
                executable = perl_runtime.interpreter,
                inputs = depset(
                    [source_file],
                    transitive = [perl_runtime.files],
                ),
                outputs = [generated],
                arguments = [perlasm_args],
                mnemonic = "LinuxPerlAsm",
                progress_message = "Generating Linux perlasm source %{label}",
            )
        else:
            perlasm_args = ctx.actions.args()
            perlasm_args.add(generated)
            perlasm_args.add(perl_runtime.interpreter)
            perlasm_args.add(source_file)
            path_mapped_run(
                ctx.actions,
                executable = ctx.attr._runandwrite[DefaultInfo].files_to_run,
                inputs = depset(
                    [source_file],
                    transitive = [perl_runtime.files],
                ),
                outputs = [generated],
                arguments = [perlasm_args],
                mnemonic = "LinuxPerlAsm",
                progress_message = "Generating Linux perlasm source %{label}",
            )
        src = generated
        generated_sources.append(generated)
    if config and ctx.attr.needs_utsversion_tmp:
        utsversion_tmp = ctx.actions.declare_file(ctx.label.name + ".obj/utsversion-tmp.h")
        uts_args = ctx.actions.args()
        uts_args.add("-config", config.config)
        uts_args.add("-utsversion_out", utsversion_tmp)
        uts_args.add("-build_version=")
        uts_args.add("-build_timestamp=")
        path_mapped_run(
            ctx.actions,
            executable = ctx.executable._versionheaders,
            inputs = [config.config],
            outputs = [utsversion_tmp],
            arguments = [uts_args],
            mnemonic = "LinuxObjectVersionHeader",
            progress_message = "Generating Linux object version header %{label}",
        )
        generated_object_headers.append(utsversion_tmp)
        object_root_file = utsversion_tmp
    elif ctx.attr.needs_object_dir:
        obj_marker = ctx.actions.declare_file(ctx.label.name + ".obj/.bazel-dir")
        ctx.actions.write(obj_marker, "")
        generated_object_headers.append(obj_marker)
        object_root_file = obj_marker

    source_root = _linux_source_root_path(ctx)
    if _is_dtb_source(source_file):
        if not source_root:
            fail("linux_object %s builds a devicetree blob and requires source_root" % ctx.label)
        generated = _linux_dtb_object_source(
            ctx,
            source_file,
            ctx.attr.object,
            _linux_rule_srcarch(ctx, generated_headers),
        )
        src = generated.src
        generated_sources.extend(generated.files)
    source_relpath = _linux_source_tree_relpath_from_ctx(ctx, source_file)
    if source_relpath.startswith("lib/fdt") and source_relpath.endswith(".c"):
        generated_sources.append(_source_tree_file(ctx, "scripts/dtc/libfdt/" + source_relpath.rsplit("/", 1)[-1]))
    if ctx.attr.object == "init/version.o":
        generated_sources.append(_source_tree_file(ctx, "init/version-timestamp.c"))
    if ctx.attr.object == "arch/x86/kernel/cpu/capflags.o":
        generated = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/kernel/cpu/capflags.c")
        cap_args = ctx.actions.args()
        cpufeatures = _source_tree_file(ctx, "arch/x86/include/asm/cpufeatures.h")
        vmxfeatures = _source_tree_file(ctx, "arch/x86/include/asm/vmxfeatures.h")
        cap_args.add("-cpufeatures", cpufeatures)
        cap_args.add("-vmxfeatures", vmxfeatures)
        cap_args.add("-out", generated)
        path_mapped_run(
            ctx.actions,
            executable = ctx.executable._capflags,
            inputs = [cpufeatures, vmxfeatures],
            outputs = [generated],
            arguments = [cap_args],
            mnemonic = "LinuxCapflags",
            progress_message = "Generating Linux x86 CPU capflags %{label}",
        )
        src = generated
        generated_sources.append(generated)
    if ctx.attr.object == "drivers/tty/vt/consolemap_deftbl.o":
        generated = ctx.actions.declare_file(ctx.label.name + ".obj/drivers/tty/vt/consolemap_deftbl.c")
        con_args = ctx.actions.args()
        con_args.add("-in", source_file)
        con_args.add("-out", generated)
        path_mapped_run(
            ctx.actions,
            executable = ctx.executable._conmakehash,
            inputs = [source_file],
            outputs = [generated],
            arguments = [con_args],
            mnemonic = "LinuxConsoleMap",
            progress_message = "Generating Linux console map %{label}",
        )
        src = generated
        generated_sources.append(generated)
    if ctx.attr.object == "arch/x86/entry/vdso/vdso-image-64.o":
        generated = _linux_vdso_image_source(
            ctx,
            compiler,
            linker,
            cc_toolchain,
            feature_configuration,
            config,
            generated_headers,
            source_root,
        )
        src = generated
        generated_sources.append(generated)
    generated_inputs = _linux_object_generated_inputs(
        ctx,
        compiler,
        linker,
        cc_toolchain,
        feature_configuration,
        config,
        generated_headers,
        source_root,
    )
    expanded_remove_flags = []
    if ctx.attr.arch == "arm64" and ctx.attr.object.startswith("arch/arm64/kernel/pi/") and ctx.attr.object.endswith(".pi.o"):
        expanded_remove_flags = expanded_remove_flags + _linux_ftrace_remove_flags() + ["-flto=thin", "-flto", "-fsplit-lto-unit", "-fvisibility=hidden"]
    if ctx.attr.arch == "x86" and ctx.attr.object.startswith("arch/x86/boot/startup/") and ctx.attr.object.endswith(".pi.o"):
        expanded_remove_flags = expanded_remove_flags + _linux_ftrace_remove_flags() + ["-flto=thin", "-flto", "-fsplit-lto-unit", "-fvisibility=hidden"]
    config_flag_inputs = _linux_filtered_config_flags_for_source(ctx, config, src, expanded_remove_flags)

    args = ctx.actions.args()
    args.add_all(config_flag_inputs.flags, format_each = "@%s")
    args.add_all(_linux_generated_header_cflags(generated_headers), format_each = "@%s")
    args.add_all(_linux_module_flags(ctx.attr.mode))
    args.add_all(_linux_object_name_flags(compile_object, ctx.attr.modname))
    if config and not source_root:
        args.add("-include")
        args.add(config.autoconf_h)
    args.add_all(_linux_source_preinclude_flags_for_root(source_root, _is_assembly_source(src)))
    if config:
        _add_config_include_flag(args, config)
    if source_root:
        args.add("-fmacro-prefix-map=%s/=" % source_root)
    dep_generated_header_inputs = []
    for dep in ctx.attr.deps:
        dep_info = dep[LinuxObjectInfo]
        _add_directory_flags(
            args,
            dep_info.generated_include_dirs,
            dep_info.generated_include_dir_anchors,
        )
        dep_generated_header_inputs.append(dep_info.generated_headers)
    add_directory_arg(args, directory_anchor(src), format = "-I%s")
    if src.dirname != source_file.dirname:
        add_directory_arg(args, directory_anchor(source_file), format = "-I%s")
    _add_directory_flags(args, generated_inputs.include_dirs, generated_inputs.include_dir_anchors)
    _add_linux_source_include_flags(ctx, args, generated_headers)
    for include in ctx.attr.include_dirs:
        args.add("-I" + include)
    if _is_assembly_source(src):
        _add_directory_flags(
            args,
            generated_inputs.assembler_include_roots,
            generated_inputs.assembler_include_root_anchors,
            format = "-Wa,-I,%s",
        )
    direct_inputs = [src] + generated_object_headers + generated_sources + generated_inputs.files + config_flag_inputs.inputs
    if src != source_file:
        direct_inputs.append(source_file)
    source_inputs = _linux_object_compile_source_tree_inputs(
        ctx,
        direct = direct_inputs,
    )
    transitive_inputs = [cc_toolchain.all_files]
    if config:
        transitive_inputs.append(config.files)
    if generated_headers:
        transitive_inputs.append(generated_headers.files)
    transitive_inputs.extend(dep_generated_header_inputs)

    profile = flag_programs.profile
    compile_args = ctx.actions.args()
    compile_args.add("compile")
    compile_args.add("-template", profile.command_template)
    compile_args.add("-validation", profile.validation)
    compile_args.add("-source", src)
    compile_args.add("-kbuild_source", source_file)
    compile_args.add("-output", compile_out)
    compile_args.add("-config", config.config)
    compile_args.add("-flags_file", flag_programs.flags)
    compile_args.add("-remove_flags_file", flag_programs.removals)
    add_directory_arg(
        compile_args,
        directory_anchor(_linux_source_root_file(ctx)),
        format = "-source_root=%s",
    )
    compile_args.add("-object_path=" + ctx.attr.object)
    if object_root_file != None:
        add_directory_arg(
            compile_args,
            directory_anchor(object_root_file),
            format = "-object_root=%s",
        )
    if utsversion_tmp != None:
        compile_args.add(utsversion_tmp, format = "-utsversion_tmp=%s")
    compile_args.add_all(
        _unique_strings(expanded_remove_flags),
        before_each = "-remove",
    )
    compile_args.add("--")
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._ccprofile,
        inputs = depset(
            source_inputs.direct + [
                flag_programs.flags,
                flag_programs.removals,
                profile.command_template,
                profile.validation,
            ],
            transitive = (
                source_inputs.transitive +
                transitive_inputs +
                [profile.toolchain_files]
            ),
        ),
        outputs = [compile_out],
        arguments = [compile_args, args],
        execution_requirements = profile.execution_requirements,
        mnemonic = "LinuxObjectCompile",
        progress_message = "Compiling Linux object %{label}",
        toolchain = CC_TOOLCHAIN_TYPE,
    )

    objtool_input = compile_out
    if needs_module_lto_link:
        objtool_input = _linux_link_relocatable(
            ctx,
            linker,
            cc_toolchain,
            feature_configuration,
            "module-lto/" + compile_object,
            [compile_out],
            output = None if ctx.executable.objtool or objcopy_flags else out,
        )

    if ctx.executable.objtool:
        objtool_args = ctx.actions.args()
        objtool_args.add("-config", config.config)
        if ctx.attr.objtool_force:
            objtool_args.add("-force")
        for arg in ctx.attr.objtool_args:
            objtool_args.add("-objtool_arg=%s" % arg)
        objtool_args.add("-objtool", ctx.executable.objtool)
        objtool_args.add("-in", objtool_input)
        objtool_mode = "builtin"
        if ctx.attr.mode == "m":
            objtool_mode = "module-single" if ctx.attr.module_root else "module-member"
        objtool_args.add("-mode", objtool_mode)
        objtool_args.add("-out", objtool_out)
        path_mapped_run(
            ctx.actions,
            executable = ctx.attr._objtoolrun[DefaultInfo].files_to_run,
            inputs = [config.config, objtool_input],
            tools = [ctx.attr.objtool[DefaultInfo].files_to_run],
            outputs = [objtool_out],
            arguments = [objtool_args],
            mnemonic = "LinuxObjectObjtool",
            progress_message = "Processing Linux object with objtool %{label}",
        )
    else:
        objtool_out = objtool_input

    if objcopy_flags:
        objcopy_args = ctx.actions.args()
        objcopy_args.add_all(objcopy_flags)
        objcopy_args.add(objtool_out)
        objcopy_args.add(objcopy_out)
        path_mapped_run(
            ctx.actions,
            executable = ctx.executable._llvm_objcopy,
            inputs = [objtool_out, ctx.executable._llvm_objcopy],
            outputs = [objcopy_out],
            arguments = [objcopy_args],
            mnemonic = "LinuxObjectObjcopy",
            progress_message = "Objcopying Linux object %{label}",
        )
        if needs_relacheck:
            relacheck_args = ctx.actions.args()
            relacheck_args.add(objcopy_out)
            relacheck_args.add(out)
            relacheck_args.add(ctx.executable.relacheck)
            relacheck_args.add(compile_out)
            path_mapped_run(
                ctx.actions,
                executable = ctx.attr._copyandrun[DefaultInfo].files_to_run,
                inputs = [objcopy_out, compile_out],
                tools = [ctx.attr.relacheck[DefaultInfo].files_to_run],
                outputs = [out],
                arguments = [relacheck_args],
                mnemonic = "LinuxObjectRelacheck",
                progress_message = "Checking Linux arm64 PI relocations %{label}",
            )

    command_lines = [
        "compiler=%s" % compiler,
        "source=%s" % source_file.short_path,
        "object=%s" % ctx.attr.object,
        "output=%s" % out.short_path,
    ]
    command_lines.extend([
        "flag_program=%s" % ctx.attr.flag_program,
        "flag_program_file=%s" % flag_programs.flags.short_path,
        "remove_flag_program=%s" % ctx.attr.remove_flag_program,
        "remove_flag_program_file=%s" % flag_programs.removals.short_path,
    ])
    ctx.actions.write(
        output = cmd,
        content = "\n".join(command_lines) + "\n",
    )

    info = LinuxObjectInfo(
        content_id = ctx.attr.content_id,
        mode = ctx.attr.mode,
        module_root_kind = "single" if ctx.attr.module_root else "",
        object = ctx.attr.object,
        objtool_args = list(ctx.attr.objtool_args),
        objtool_force = ctx.attr.objtool_force,
        output = out,
        generated_headers = depset(exported_generated_headers),
        generated_include_dir_anchors = _directory_anchors(exported_generated_headers, exported_generated_include_dirs),
        generated_include_dirs = exported_generated_include_dirs,
    )
    return [
        DefaultInfo(files = depset([out, cmd])),
        info,
        OutputGroupInfo(
            command = depset([cmd]),
            object = depset([out]),
        ),
    ]

linux_object = rule(
    implementation = _linux_object_impl,
    attrs = {
        "arch": attr.string(default = "x86"),
        "compile_environment_id": attr.string(
            mandatory = True,
            doc = "Full SHA-256 ID selected from compile_environment_index.",
        ),
        "compile_environment_index": attr.label(
            mandatory = True,
            providers = [LinuxCompileEnvironmentIndexInfo],
            doc = "Content-addressed compile environment index.",
        ),
        "content_id": attr.string(
            mandatory = True,
            doc = "Full SHA-256 content identity for this object action.",
        ),
        "deps": attr.label_list(providers = [LinuxObjectInfo]),
        "flag_program": attr.string(
            mandatory = True,
            doc = "Resolved compact-v7 flag program ID. Requires flag_programs and graph_profile.",
        ),
        "flag_programs": attr.label(
            mandatory = True,
            providers = [LinuxFlagProgramsInfo],
            doc = "Shared compact-v7 flag program DAG for lazy object compilation.",
        ),
        "needs_object_dir": attr.bool(
            mandatory = True,
            doc = "Whether any possible flag branch references the object-local directory.",
        ),
        "needs_utsversion_tmp": attr.bool(
            mandatory = True,
            doc = "Whether any possible flag branch consumes object-local utsversion-tmp.h.",
        ),
        "remove_flag_program": attr.string(
            mandatory = True,
            doc = "Resolved compact-v7 removal program ID. Requires flag_programs and graph_profile.",
        ),
        "include_dirs": attr.string_list(),
        "mode": attr.string(values = ["y", "m"], mandatory = True),
        "module_root": attr.bool(
            doc = "Whether this leaf object is a single-object in-tree module root.",
        ),
        "modname": attr.string(),
        "object": attr.string(mandatory = True),
        "objtool": attr.label(
            cfg = "exec",
            doc = "Kernel-source-specific objtool executable. When set, processes this translation unit after compilation.",
            executable = True,
        ),
        "objtool_args": attr.string_list(
            doc = "Additional Kbuild-derived arguments for this translation unit's objtool action.",
        ),
        "objtool_force": attr.bool(
            doc = "Run objtool when Kbuild explicitly enables this translation unit despite delayed processing.",
        ),
        "srcarch": attr.string(),
        "source_input_file": attr.int(
            mandatory = True,
            doc = "One-based primary source file selected from source_input_index.",
        ),
        "source_input_group": attr.int(
            mandatory = True,
            doc = "One-based exact input group selected from source_input_index.",
        ),
        "source_input_index": attr.label(
            mandatory = True,
            providers = [LinuxSourceInputIndexInfo],
            doc = "Canonical exact source input index for content-addressed actions.",
        ),
        "asn1_compiler": attr.label(
            cfg = "exec",
            doc = "Kernel-source-specific scripts/asn1_compiler executable. Required only for .asn1.o objects.",
            executable = True,
        ),
        "relacheck": attr.label(
            cfg = "exec",
            doc = "Kernel-source-specific arch/arm64/kernel/pi/relacheck executable. Required only for arm64 .pi.o objects.",
            executable = True,
        ),
        "_versionheaders": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/versionheaders"),
            executable = True,
        ),
        "_capflags": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/capflags"),
            executable = True,
        ),
        "_ccprofile": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/ccprofile"),
            executable = True,
        ),
        "_copyandrun": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/copyandrun"),
            executable = True,
        ),
        "_conmakehash": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/conmakehash"),
            executable = True,
        ),
        "_crctables": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/crctables"),
            executable = True,
        ),
        "_emptyrootdtb": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/emptyrootdtb"),
            executable = True,
        ),
        "_flagfilter": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/flagfilter"),
            executable = True,
        ),
        "_objtoolrun": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/objtoolrun"),
            executable = True,
        ),
        "_initramfsdata": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/initramfsdata"),
            executable = True,
        ),
        "_insnattr": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/insnattr"),
            executable = True,
        ),
        "_oidregistry": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/oidregistry"),
            executable = True,
        ),
        "_scsidevinfo": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/scsidevinfo"),
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
        "_pasyms": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/pasyms"),
            executable = True,
        ),
        "_realmoderelocs": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/realmoderelocs"),
            executable = True,
        ),
        "_runandwrite": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/runandwrite"),
            executable = True,
        ),
        "_vdso2c": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/vdso2c"),
            executable = True,
        ),
    },
    fragments = ["cpp"],
    toolchains = use_cc_toolchain() + [_PERL_TOOLCHAIN],
    doc = "Compiles one content-addressed, source-backed Linux object variant.",
)

def _linux_composite_object_impl(ctx):
    _validate_content_id(ctx.attr.content_id, "linux_composite_object content_id")
    object_infos = [obj[LinuxObjectInfo] for obj in ctx.attr.objects]
    if not object_infos:
        fail("linux_composite_object %s requires at least one member object" % ctx.label)
    if ctx.attr.module_root and ctx.attr.mode != "m":
        fail("linux_composite_object %s marks a non-module object as a module root" % ctx.label)
    cc_toolchain = find_cpp_toolchain(ctx)
    feature_configuration = _cc_feature_configuration(ctx, cc_toolchain)
    linker = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = CPP_LINK_EXECUTABLE_ACTION_NAME,
    )
    out = ctx.actions.declare_file(ctx.label.name + ".o")
    args = ctx.actions.args()
    args.add_all(_cc_target_flags(ctx, cc_toolchain, feature_configuration))
    args.add("-fuse-ld=lld")
    args.add("-nostdlib")
    args.add("-r")
    args.add("-o")
    args.add(out)
    args.add_all([info.output for info in object_infos])
    path_mapped_run(
        ctx.actions,
        executable = linker,
        inputs = depset([info.output for info in object_infos], transitive = [cc_toolchain.all_files]),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxCompositeObject",
        progress_message = "Linking Linux composite object %{label}",
    )
    info = LinuxObjectInfo(
        content_id = ctx.attr.content_id,
        mode = ctx.attr.mode,
        module_root_kind = "composite" if ctx.attr.module_root else "",
        object = ctx.attr.object,
        objtool_args = list(ctx.attr.objtool_args),
        objtool_force = ctx.attr.objtool_force,
        output = out,
        generated_headers = depset(transitive = [info.generated_headers for info in object_infos]),
        generated_include_dir_anchors = _merged_generated_include_dir_anchors(object_infos),
        generated_include_dirs = _unique_strings([include_dir for info in object_infos for include_dir in info.generated_include_dirs]),
    )
    return [
        DefaultInfo(files = depset([out])),
        info,
        OutputGroupInfo(object = depset([out])),
    ]

linux_composite_object = rule(
    implementation = _linux_composite_object_impl,
    attrs = {
        "arch": attr.string(default = "x86"),
        "content_id": attr.string(
            mandatory = True,
            doc = "Full SHA-256 content identity for this composite object.",
        ),
        "mode": attr.string(values = ["y", "m"], mandatory = True),
        "module_root": attr.bool(
            doc = "Whether this composite object is an in-tree module root.",
        ),
        "object": attr.string(mandatory = True),
        "objtool_args": attr.string_list(
            doc = "Kbuild target-specific arguments for delayed root objtool processing.",
        ),
        "objtool_force": attr.bool(
            doc = "Whether Kbuild explicitly enables objtool for this composite.",
        ),
        "objects": attr.label_list(providers = [LinuxObjectInfo], mandatory = True),
    },
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Links Kbuild composite object members into one relocatable object.",
)

def _linux_arm64_nvhe_linker_script(ctx, compiler, cc_toolchain, feature_configuration, config, generated_headers, source_root):
    src = _source_tree_file(ctx, "arch/arm64/kvm/hyp/nvhe/hyp.lds.S")
    out = ctx.actions.declare_file(ctx.label.name + ".obj/arch/arm64/kvm/hyp/nvhe/hyp.lds")
    args = ctx.actions.args()
    args.add_all(_linux_compile_flags(ctx, cc_toolchain, feature_configuration))
    args.add_all([
        "-E",
        "-P",
        "-D__KERNEL__",
        "-D__ASSEMBLY__",
        "-DLINKER_SCRIPT",
        "-include",
        source_root + "/include/linux/kconfig.h",
    ])
    args.add_all(_linux_cpp_undef_flags("arm64", "arm64"))
    _add_config_include_flag(args, config)
    _add_linux_source_include_flags_for_root(
        args,
        source_root,
        "arm64",
        generated_headers.include_dirs,
        _generated_include_dir_anchors(generated_headers),
    )
    args.add(src)
    args.add("-o")
    args.add(out)
    source_selection = _linux_source_input_group(ctx, "linux_arm64_nvhe_object")
    direct_inputs = [src]
    transitive_inputs = [cc_toolchain.all_files, config.files, generated_headers.files, source_selection.value.files]
    path_mapped_run(
        ctx.actions,
        executable = compiler,
        inputs = depset(
            direct_inputs,
            transitive = transitive_inputs,
        ),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxArm64NvheLinkerScript",
        progress_message = "Preprocessing Linux arm64 nVHE linker script %{label}",
    )
    return out

def _linux_link_relocatable(ctx, linker, cc_toolchain, feature_configuration, out_relpath, objects, flags = [], extra_inputs = [], linker_script = None, output = None):
    out = output if output else ctx.actions.declare_file(ctx.label.name + ".obj/" + out_relpath)
    args = ctx.actions.args()
    args.add_all(_cc_target_flags(ctx, cc_toolchain, feature_configuration))
    args.add("-fuse-ld=lld")
    args.add("-nostdlib")
    args.add("-r")
    args.add_all(flags)
    if linker_script:
        args.add(linker_script, format = "-Wl,-T,%s")
    args.add("-o")
    args.add(out)
    args.add_all(objects)
    path_mapped_run(
        ctx.actions,
        executable = linker,
        inputs = depset(objects + extra_inputs, transitive = [cc_toolchain.all_files]),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxRelocatableLink",
        progress_message = "Linking Linux relocatable object %{label}",
    )
    return out

def _linux_arm64_nvhe_object_impl(ctx):
    _validate_content_id(ctx.attr.content_id, "linux_arm64_nvhe_object content_id")
    _linux_source_input_group(ctx, "linux_arm64_nvhe_object")
    object_infos = [obj[LinuxObjectInfo] for obj in ctx.attr.objects]
    if not object_infos:
        fail("linux_arm64_nvhe_object %s requires at least one member object" % ctx.label)
    compile_environment = _linux_compile_environment(ctx, "linux_arm64_nvhe_object")
    config = compile_environment.config
    generated_headers = compile_environment.generated_headers
    if not config:
        fail("linux_arm64_nvhe_object %s requires config" % ctx.label)
    if not generated_headers:
        fail("linux_arm64_nvhe_object %s requires generated_headers" % ctx.label)
    source_root_file = _linux_source_root_file(ctx)
    if not source_root_file:
        fail("linux_arm64_nvhe_object %s requires source_root" % ctx.label)

    cc_toolchain = find_cpp_toolchain(ctx)
    feature_configuration = _cc_feature_configuration(ctx, cc_toolchain)
    compiler = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
    )
    linker = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = CPP_LINK_EXECUTABLE_ACTION_NAME,
    )
    source_root = _linux_source_root_path(ctx)
    linker_script = _linux_arm64_nvhe_linker_script(
        ctx,
        compiler,
        cc_toolchain,
        feature_configuration,
        config,
        generated_headers,
        source_root,
    )

    member_outputs = [info.output for info in object_infos]
    tmp = _linux_link_relocatable(
        ctx,
        linker,
        cc_toolchain,
        feature_configuration,
        "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.tmp.o",
        member_outputs,
        extra_inputs = [linker_script],
        linker_script = linker_script,
    )

    reloc_s = ctx.actions.declare_file(ctx.label.name + ".obj/arch/arm64/kvm/hyp/nvhe/hyp-reloc.S")
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
        progress_message = "Generating Linux arm64 nVHE relocation source %{label}",
    )

    hyp_reloc = _linux_vmlinux_compile_source(
        ctx,
        compiler,
        cc_toolchain,
        feature_configuration,
        config,
        generated_headers,
        source_root,
        reloc_s,
        "arch/arm64/kvm/hyp/nvhe/hyp-reloc.o",
        "arch/arm64/kvm/hyp/nvhe/hyp-reloc.o",
    )
    rel = _linux_link_relocatable(
        ctx,
        linker,
        cc_toolchain,
        feature_configuration,
        "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.rel.o",
        [tmp, hyp_reloc],
    )

    out = ctx.actions.declare_file(ctx.label.name + ".o")
    objcopy_args = ctx.actions.args()
    objcopy_args.add("--prefix-symbols=__kvm_nvhe_")
    objcopy_args.add(rel)
    objcopy_args.add(out)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._llvm_objcopy,
        inputs = [rel, ctx.executable._llvm_objcopy],
        outputs = [out],
        arguments = [objcopy_args],
        mnemonic = "LinuxArm64NvheObjcopy",
        progress_message = "Objcopying Linux arm64 nVHE object %{label}",
    )

    info = LinuxObjectInfo(
        content_id = ctx.attr.content_id,
        mode = ctx.attr.mode,
        module_root_kind = "",
        object = ctx.attr.object,
        objtool_args = [],
        objtool_force = False,
        output = out,
        generated_headers = depset(transitive = [info.generated_headers for info in object_infos]),
        generated_include_dir_anchors = _merged_generated_include_dir_anchors(object_infos),
        generated_include_dirs = _unique_strings([include_dir for info in object_infos for include_dir in info.generated_include_dirs]),
    )
    return [
        DefaultInfo(files = depset([out])),
        info,
        OutputGroupInfo(object = depset([out])),
    ]

linux_arm64_nvhe_object = rule(
    implementation = _linux_arm64_nvhe_object_impl,
    attrs = {
        "arch": attr.string(default = "arm64"),
        "compile_environment_id": attr.string(
            mandatory = True,
            doc = "Full SHA-256 ID selected from compile_environment_index.",
        ),
        "compile_environment_index": attr.label(
            mandatory = True,
            providers = [LinuxCompileEnvironmentIndexInfo],
            doc = "Content-addressed config and generated-header index.",
        ),
        "content_id": attr.string(
            mandatory = True,
            doc = "Full SHA-256 content identity for this composite object.",
        ),
        "mode": attr.string(values = ["y", "m"], mandatory = True),
        "object": attr.string(mandatory = True),
        "objects": attr.label_list(
            mandatory = True,
            providers = [LinuxObjectInfo],
        ),
        "source_input_group": attr.int(
            mandatory = True,
            doc = "One-based exact input group selected from source_input_index.",
        ),
        "source_input_index": attr.label(
            mandatory = True,
            providers = [LinuxSourceInputIndexInfo],
            doc = "Canonical exact source input index for the nVHE linker-script action.",
        ),
        "srcarch": attr.string(default = "arm64"),
        "_genhyprel": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/genhyprel"),
            executable = True,
        ),
        "_llvm_objcopy": attr.label(
            allow_single_file = True,
            cfg = "exec",
            default = Label("@llvm//tools:llvm-objcopy"),
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
    doc = "Builds the arm64 nVHE KVM custom composite object with hyp relocations.",
)

def _linux_archive_impl(ctx):
    object_infos = [obj[LinuxObjectInfo] for obj in ctx.attr.objects]
    cc_toolchain = find_cpp_toolchain(ctx)
    feature_configuration = _cc_feature_configuration(ctx, cc_toolchain)
    archiver = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = CPP_LINK_STATIC_LIBRARY_ACTION_NAME,
    )
    out = ctx.actions.declare_file(ctx.label.name + ".a")
    args = ctx.actions.args()
    args.add("cDPrST")
    args.add(out)
    args.add_all([info.output for info in object_infos])
    path_mapped_run(
        ctx.actions,
        executable = archiver,
        inputs = depset([info.output for info in object_infos], transitive = [cc_toolchain.all_files]),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxArchive",
        progress_message = "Archiving Linux objects %{label}",
    )
    info = LinuxArchiveInfo(
        kind = ctx.attr.kind,
        objects = object_infos,
        output = out,
    )
    return [
        DefaultInfo(files = depset([out])),
        info,
    ]

linux_archive = rule(
    implementation = _linux_archive_impl,
    attrs = {
        "arch": attr.string(default = "x86"),
        "kind": attr.string(
            default = "built-in.a",
            values = [
                "built-in.a",
                "module-objects",
                "lib.a",
            ],
        ),
        "objects": attr.label_list(providers = [LinuxObjectInfo]),
    },
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Archives compiled Linux objects into built-in.a, lib.a, or module object groups.",
)

def _linux_compact_image_impl(ctx):
    object_infos = [obj[LinuxObjectInfo] for obj in ctx.attr.objects]
    module_object_infos = [obj[LinuxObjectInfo] for obj in ctx.attr.module_objects]
    if not object_infos:
        fail("linux_compact_image %s requires at least one compiled object" % ctx.label)
    for info in object_infos:
        if info.mode != "y":
            fail(
                "linux_compact_image %s built-in object %s has mode %r, want \"y\"" %
                (ctx.label, info.object, info.mode),
            )
    for info in module_object_infos:
        if info.mode != "m":
            fail(
                "linux_compact_image %s module object %s has mode %r, want \"m\"" %
                (ctx.label, info.object, info.mode),
            )
    cc_toolchain = find_cpp_toolchain(ctx)
    feature_configuration = _cc_feature_configuration(ctx, cc_toolchain)
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
        progress_message = "Archiving compact Linux image %{label}",
    )
    info = LinuxImageInfo(
        archives = [],
        module_objects = module_object_infos,
        objects = object_infos,
        output = out,
    )
    return [
        DefaultInfo(files = depset([out])),
        info,
        _LinuxCompactImageShapeInfo(
            module_object_content_ids = [
                obj.content_id
                for obj in module_object_infos
            ],
            object_content_ids = [
                obj.content_id
                for obj in object_infos
            ],
        ),
    ]

linux_compact_image = rule(
    implementation = _linux_compact_image_impl,
    attrs = {
        "arch": attr.string(default = "x86"),
        "module_objects": attr.label_list(providers = [LinuxObjectInfo]),
        "objects": attr.label_list(providers = [LinuxObjectInfo]),
    },
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Archives compact Linux object variants into one relocatable image object.",
)

def _linux_compact_modules_impl(ctx):
    objects = [target[LinuxObjectInfo] for target in ctx.attr.objects]
    for info in objects:
        if info.mode != "m":
            fail(
                "linux_compact_modules %s object %s has mode %r, want \"m\"" %
                (ctx.label, info.object, info.mode),
            )
    return [
        DefaultInfo(files = depset([info.output for info in objects])),
        LinuxModuleObjectsInfo(objects = objects),
    ]

linux_compact_modules = rule(
    implementation = _linux_compact_modules_impl,
    attrs = {
        "objects": attr.label_list(providers = [LinuxObjectInfo]),
    },
    doc = "Collects configured in-tree module roots outside the kernel image graph.",
)

def _compact_objects_by_content_id(objects, what, expected_mode):
    by_id = {}
    order = []
    for obj in objects:
        if obj.mode != expected_mode:
            fail("%s object %s has mode %r, want %r" % (what, obj.object, obj.mode, expected_mode))
        content_id = obj.content_id
        _validate_content_id(content_id, "%s object %s content_id" % (what, obj.object))
        if content_id in by_id:
            fail("%s repeats object content ID %s" % (what, content_id))
        by_id[content_id] = obj
        order.append(content_id)
    return struct(by_id = by_id, order = order)

def _linux_compact_delta_image_impl(ctx):
    base_target = ctx.attr.base_image
    if _LinuxCompactImageShapeInfo not in base_target:
        fail("linux_compact_delta_image %s requires a compact base_image" % ctx.label)
    base = base_target[LinuxImageInfo]
    base_shape = base_target[_LinuxCompactImageShapeInfo]
    if base.archives:
        fail("linux_compact_delta_image %s base_image must not contain archive groups" % ctx.label)

    base_objects = _compact_objects_by_content_id(base.objects, "base built-in", "y")
    base_modules = _compact_objects_by_content_id(base.module_objects, "base module", "m")
    if base_shape.object_content_ids != base_objects.order or base_shape.module_object_content_ids != base_modules.order:
        fail("linux_compact_delta_image %s base_image has inconsistent compact shape metadata" % ctx.label)
    base_content_ids = {}
    for content_id in base_objects.by_id.keys():
        if content_id in base_modules.by_id:
            fail("base image %s uses content ID %s for both built-in and module objects" % (base_target.label, content_id))
        base_content_ids[content_id] = True
    for content_id in base_modules.by_id.keys():
        base_content_ids[content_id] = True

    removals = {}
    for content_id in ctx.attr.remove_content_ids:
        _validate_content_id(content_id, "remove_content_ids entry")
        if content_id in removals:
            fail("linux_compact_delta_image %s repeats removal %s" % (ctx.label, content_id))
        if content_id not in base_objects.by_id and content_id not in base_modules.by_id:
            fail("linux_compact_delta_image %s removes unknown content ID %s" % (ctx.label, content_id))
        removals[content_id] = True

    built_ins = {
        content_id: obj
        for content_id, obj in base_objects.by_id.items()
        if content_id not in removals
    }
    modules = {
        content_id: obj
        for content_id, obj in base_modules.by_id.items()
        if content_id not in removals
    }
    for target in ctx.attr.add_objects:
        obj = target[LinuxObjectInfo]
        content_id = obj.content_id
        _validate_content_id(content_id, "added object %s content_id" % target.label)
        if content_id in base_content_ids:
            fail("linux_compact_delta_image %s re-adds base content ID %s" % (ctx.label, content_id))
        if content_id in built_ins or content_id in modules:
            fail("linux_compact_delta_image %s adds duplicate content ID %s" % (ctx.label, content_id))
        if obj.mode == "m":
            modules[content_id] = obj
        elif obj.mode == "y":
            built_ins[content_id] = obj
        else:
            fail("linux_compact_delta_image %s object %s has invalid mode %r" % (ctx.label, target.label, obj.mode))

    ordered_ids = []
    ordered_seen = {}
    for content_id in ctx.attr.ordered_content_ids:
        _validate_content_id(content_id, "ordered_content_ids entry")
        if content_id in ordered_seen:
            fail("linux_compact_delta_image %s repeats ordered content ID %s" % (ctx.label, content_id))
        if content_id not in built_ins:
            fail("linux_compact_delta_image %s orders unknown built-in content ID %s" % (ctx.label, content_id))
        ordered_seen[content_id] = True
        ordered_ids.append(content_id)
    missing_built_ins = [content_id for content_id in built_ins.keys() if content_id not in ordered_seen]
    if missing_built_ins:
        fail(
            "linux_compact_delta_image %s omits built-in content ID(s) from ordered_content_ids: %s" %
            (ctx.label, ", ".join(sorted(missing_built_ins))),
        )
    object_infos = [built_ins[content_id] for content_id in ordered_ids]
    if not object_infos:
        fail("linux_compact_delta_image %s requires at least one compiled built-in object" % ctx.label)

    module_ids = []
    module_seen = {}
    for content_id in ctx.attr.ordered_module_content_ids:
        _validate_content_id(content_id, "ordered_module_content_ids entry")
        if content_id in module_seen:
            fail("linux_compact_delta_image %s repeats ordered module content ID %s" % (ctx.label, content_id))
        if content_id not in modules:
            fail("linux_compact_delta_image %s orders unknown module content ID %s" % (ctx.label, content_id))
        module_seen[content_id] = True
        module_ids.append(content_id)
    missing_modules = [content_id for content_id in modules.keys() if content_id not in module_seen]
    if missing_modules:
        fail(
            "linux_compact_delta_image %s omits module content ID(s) from ordered_module_content_ids: %s" %
            (ctx.label, ", ".join(sorted(missing_modules))),
        )
    module_object_infos = [modules[content_id] for content_id in module_ids]

    cc_toolchain = find_cpp_toolchain(ctx)
    feature_configuration = _cc_feature_configuration(ctx, cc_toolchain)
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
        mnemonic = "LinuxCompactDeltaImageArchive",
        progress_message = "Archiving compact Linux delta image %{label}",
    )
    return [
        DefaultInfo(files = depset([out])),
        LinuxImageInfo(
            archives = [],
            module_objects = module_object_infos,
            objects = object_infos,
            output = out,
        ),
        _LinuxCompactImageShapeInfo(
            module_object_content_ids = module_ids,
            object_content_ids = ordered_ids,
        ),
    ]

linux_compact_delta_image = rule(
    implementation = _linux_compact_delta_image_impl,
    attrs = {
        "add_objects": attr.label_list(providers = [LinuxObjectInfo]),
        "arch": attr.string(default = "x86"),
        "base_image": attr.label(
            mandatory = True,
            providers = [LinuxImageInfo],
        ),
        "ordered_content_ids": attr.string_list(
            mandatory = True,
            doc = "Authoritative final built-in object order as full SHA-256 content IDs.",
        ),
        "ordered_module_content_ids": attr.string_list(
            mandatory = True,
            doc = "Authoritative final module object order as full SHA-256 content IDs.",
        ),
        "remove_content_ids": attr.string_list(
            doc = "Built-in or module object content IDs removed from the base image.",
        ),
    },
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Reconstructs an ordered compact image from content-addressed additions and removals.",
)

def _linux_cpp_undef_flags(arch, srcarch):
    if srcarch == "x86":
        return ["-Ux86", "-Ux86_64"]
    if arch != srcarch:
        return ["-U" + arch, "-U" + srcarch]
    return ["-U" + arch]

def _linux_vmlinux_linker_script(ctx, compiler, cc_toolchain, feature_configuration, config, generated_headers, source_root):
    arch = _linux_rule_arch(ctx, generated_headers)
    srcarch = _linux_rule_srcarch(ctx, generated_headers)
    out = ctx.actions.declare_file(ctx.label.name + ".obj/arch/" + srcarch + "/kernel/vmlinux.lds")
    args = ctx.actions.args()
    args.add_all(_linux_compile_flags(ctx, cc_toolchain, feature_configuration))
    args.add_all([
        "-E",
        "-P",
        "-D__KERNEL__",
        "-D__ASSEMBLY__",
        "-DLINKER_SCRIPT",
        "-include",
        source_root + "/include/linux/kconfig.h",
    ])
    args.add_all(_linux_cpp_undef_flags(arch, srcarch))
    _add_config_include_flag(args, config)
    _add_linux_source_include_flags_for_root(
        args,
        source_root,
        srcarch,
        generated_headers.include_dirs,
        _generated_include_dir_anchors(generated_headers),
    )
    args.add(ctx.file.linker_script)
    args.add("-o")
    args.add(out)

    path_mapped_run(
        ctx.actions,
        executable = compiler,
        inputs = depset(
            _linux_source_tree_inputs(ctx, direct = [ctx.file.linker_script]),
            transitive = [cc_toolchain.all_files, config.files, generated_headers.files],
        ),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxVmlinuxLinkerScript",
        progress_message = "Preprocessing Linux vmlinux linker script %{label}",
    )
    return out

def _linux_version_at_least(version, major, minor):
    parts = version.split(".")
    if len(parts) < 2:
        fail("invalid Linux version %r" % version)
    return (int(parts[0]), int(parts[1])) >= (major, minor)

def _linux_vmlinux_export_uses_objtool(config, version):
    if not _linux_version_at_least(version, 6, 13):
        return False
    return _linux_version_at_least(version, 6, 18) or config.config_flags.get("CONFIG_MODULES") == "y"

def _linux_vmlinux_compile_source(
        ctx,
        compiler,
        cc_toolchain,
        feature_configuration,
        config,
        generated_headers,
        source_root,
        src,
        out_relpath,
        object_name,
        extra_flags = [],
        objtool_mode = ""):
    if objtool_mode not in ["", "builtin", "builtin-always"]:
        fail("unsupported vmlinux source objtool mode %r" % objtool_mode)

    delay_objtool = (
        config.config_flags.get("CONFIG_LTO_CLANG") == "y" or
        config.config_flags.get("CONFIG_X86_KERNEL_IBT") == "y"
    )
    process_with_objtool = (
        objtool_mode != "" and
        ctx.executable.objtool and
        config.config_flags.get("CONFIG_OBJTOOL") == "y" and
        (objtool_mode == "builtin-always" or not delay_objtool)
    )
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + out_relpath)
    compile_out = out
    if process_with_objtool:
        compile_out = ctx.actions.declare_file(
            ctx.label.name + ".obj/" + out_relpath + ".objtool-input",
        )
    assembly = _is_assembly_source(src)
    args = ctx.actions.args()
    args.add_all(_linux_compile_flags(ctx, cc_toolchain, feature_configuration))
    args.add_all(_linux_config_flags_for_source(config, src), format_each = "@%s")
    args.add_all(_linux_generated_header_cflags(generated_headers), format_each = "@%s")
    args.add_all(_linux_object_name_flags(object_name))
    args.add_all(_linux_source_preinclude_flags_for_root(source_root, assembly))
    _add_config_include_flag(args, config)
    add_directory_arg(args, directory_anchor(src), format = "-I%s")
    _add_linux_source_include_flags(ctx, args, generated_headers)
    args.add_all(extra_flags)
    args.add("-c")
    args.add(src)
    args.add("-o")
    args.add(compile_out)
    source_inputs = _linux_action_source_inputs(
        ctx,
        "Linux vmlinux support compile",
        direct = [src],
    )

    path_mapped_run(
        ctx.actions,
        executable = compiler,
        inputs = depset(
            source_inputs.direct,
            transitive = source_inputs.transitive + [cc_toolchain.all_files, config.files, generated_headers.files],
        ),
        outputs = [compile_out],
        arguments = [args],
        mnemonic = "LinuxVmlinuxCompile",
        progress_message = "Compiling Linux vmlinux support object %{label}",
    )

    if not process_with_objtool:
        return out

    objtool_input = compile_out
    if objtool_mode == "builtin-always" and config.config_flags.get("CONFIG_LTO_CLANG") == "y":
        objtool_input = _linux_link_relocatable(
            ctx,
            cc_common.get_tool_for_action(
                feature_configuration = feature_configuration,
                action_name = CPP_LINK_EXECUTABLE_ACTION_NAME,
            ),
            cc_toolchain,
            feature_configuration,
            out_relpath + ".objtool-linked.o",
            [compile_out],
        )

    objtool_args = ctx.actions.args()
    objtool_args.add("-config", config.config)
    objtool_args.add("-objtool", ctx.executable.objtool)
    objtool_args.add("-in", objtool_input)
    objtool_args.add("-mode", objtool_mode)
    objtool_args.add("-out", out)
    path_mapped_run(
        ctx.actions,
        executable = ctx.attr._objtoolrun[DefaultInfo].files_to_run,
        inputs = [config.config, objtool_input],
        tools = [ctx.attr.objtool[DefaultInfo].files_to_run],
        outputs = [out],
        arguments = [objtool_args],
        mnemonic = "LinuxVmlinuxObjectObjtool",
        progress_message = "Processing Linux vmlinux support object with objtool %{label}",
    )
    return out

def _linux_vmlinux_export_object(ctx, compiler, cc_toolchain, feature_configuration, config, generated_headers, source_root, src = None):
    if src == None:
        src = ctx.actions.declare_file(ctx.label.name + ".obj/.vmlinux.export.c")
        ctx.actions.write(
            output = src,
            content = """#include <linux/export-internal.h>
#include <linux/module.h>
#undef __MODULE_INFO_PREFIX
#define __MODULE_INFO_PREFIX
""",
        )
    return _linux_vmlinux_compile_source(
        ctx,
        compiler,
        cc_toolchain,
        feature_configuration,
        config,
        generated_headers,
        source_root,
        src,
        ".vmlinux.export.o",
        ".vmlinux.export.o",
        objtool_mode = "builtin-always" if _linux_vmlinux_export_uses_objtool(config, ctx.attr.version) else "",
    )

def _linux_system_map(ctx, input, name):
    nm_out = ctx.actions.declare_file(ctx.label.name + ".obj/" + name + ".nm")
    nm_args = ctx.actions.args()
    nm_args.add("-nm", ctx.executable._llvm_nm)
    nm_args.add("-in", input)
    nm_args.add("-out", nm_out)
    path_mapped_run(
        ctx.actions,
        executable = ctx.attr._nmrun[DefaultInfo].files_to_run,
        inputs = [input],
        tools = [ctx.attr._llvm_nm[DefaultInfo].files_to_run],
        outputs = [nm_out],
        arguments = [nm_args],
        mnemonic = "LinuxVmlinuxNM",
        progress_message = "Generating Linux vmlinux nm output %{label}",
    )

    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + name + ".syms")
    map_args = ctx.actions.args()
    map_args.add("-in", nm_out)
    map_args.add("-out", out)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._mksysmap,
        inputs = [nm_out],
        outputs = [out],
        arguments = [map_args],
        mnemonic = "LinuxVmlinuxSystemMap",
        progress_message = "Generating Linux System.map data %{label}",
    )
    return out

def _linux_sorttable(ctx, config, input):
    out = ctx.actions.declare_file(ctx.label.name + ".sorted.vmlinux")
    nm_out = ctx.actions.declare_file(ctx.label.name + ".obj/.tmp_vmlinux.nm-sort")
    inputs = [input, config.config]
    tools = [ctx.attr._llvm_nm[DefaultInfo].files_to_run]
    if ctx.executable.sorttable_tool:
        tools.append(ctx.attr.sorttable_tool[DefaultInfo].files_to_run)
    args = ctx.actions.args()
    args.add("-config", config.config)
    args.add("-nm", ctx.executable._llvm_nm)
    if ctx.executable.sorttable_tool:
        args.add("-sorttable")
        args.add(ctx.executable.sorttable_tool)
    args.add("-in", input)
    args.add("-nm_out", nm_out)
    args.add("-out", out)
    path_mapped_run(
        ctx.actions,
        executable = ctx.attr._sorttablerun[DefaultInfo].files_to_run,
        inputs = inputs,
        tools = tools,
        outputs = [out, nm_out],
        arguments = [args],
        mnemonic = "LinuxVmlinuxSortTable",
        progress_message = "Sorting Linux kernel tables %{label}",
    )
    return out

def _linux_strip_vmlinux(ctx, config, input, out):
    set_flags = ["--set-section-flags", ".modinfo=noload"]
    remove_flags = [
        "--remove-section=.modinfo",
        "-w",
        "--strip-unneeded-symbol=__mod_device_table__*",
    ]
    if config.config_flags.get("CONFIG_ARCH_VMLINUX_NEEDS_RELOCS") == "y":
        set_flags.extend([
            "--set-section-flags",
            ".rel*=noload",
            "--set-section-flags",
            "!.rel*.dyn=noload",
            "--set-section-flags",
            ".rel.*=noload",
        ])
        remove_flags.extend([
            "--remove-section=.rel*",
            "--remove-section=!.rel*.dyn",
            "--remove-section=.rel.*",
        ])

    prepared = ctx.actions.declare_file(ctx.label.name + ".obj/vmlinux.strip-prepared")
    prepare_args = ctx.actions.args()
    prepare_args.add_all(set_flags)
    prepare_args.add(input)
    prepare_args.add(prepared)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._llvm_objcopy,
        inputs = [input, ctx.executable._llvm_objcopy],
        outputs = [prepared],
        arguments = [prepare_args],
        mnemonic = "LinuxVmlinuxPrepareStrip",
        progress_message = "Preparing Linux vmlinux for stripping %{label}",
    )
    strip_args = ctx.actions.args()
    strip_args.add_all(remove_flags)
    strip_args.add(prepared)
    strip_args.add(out)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._llvm_objcopy,
        inputs = [prepared, ctx.executable._llvm_objcopy],
        outputs = [out],
        arguments = [strip_args],
        mnemonic = "LinuxVmlinuxStrip",
        progress_message = "Stripping Linux vmlinux %{label}",
    )
    return out

def _linux_kallsyms_object(ctx, compiler, cc_toolchain, feature_configuration, config, generated_headers, source_root, system_map, name, kallsyms_tool):
    asm = ctx.actions.declare_file(ctx.label.name + ".obj/" + name + ".kallsyms.S")
    kallsyms_flags = []
    if ctx.attr.kallsyms_all or config.config_flags.get("CONFIG_KALLSYMS_ALL") == "y":
        kallsyms_flags.append("--all-symbols")
    if ctx.attr.kallsyms_pc_relative:
        kallsyms_flags.append("--pc-relative")
    kallsyms_args = ctx.actions.args()
    kallsyms_args.add(asm)
    kallsyms_args.add(kallsyms_tool)
    kallsyms_args.add_all(kallsyms_flags)
    kallsyms_args.add(system_map)
    path_mapped_run(
        ctx.actions,
        executable = ctx.attr._runandwrite[DefaultInfo].files_to_run,
        inputs = [system_map],
        tools = [ctx.attr.kallsyms_tool[DefaultInfo].files_to_run],
        outputs = [asm],
        arguments = [kallsyms_args],
        mnemonic = "LinuxKallsyms",
        progress_message = "Generating Linux kallsyms assembly %{label}",
    )
    return _linux_vmlinux_compile_source(
        ctx,
        compiler,
        cc_toolchain,
        feature_configuration,
        config,
        generated_headers,
        source_root,
        asm,
        name + ".kallsyms.o",
        name + ".kallsyms.o",
    )

def _linux_btf_object(ctx, config, input):
    if config.config_flags.get("CONFIG_DEBUG_INFO_BTF") != "y":
        return None
    if not ctx.executable.pahole:
        fail("linux_vmlinux %s has DEBUG_INFO_BTF enabled and requires pahole" % ctx.label)

    btf_vmlinux = ctx.actions.declare_file(ctx.label.name + ".obj/" + input.basename + ".btf")
    pahole_args = ctx.actions.args()
    pahole_args.add("-input", input)
    pahole_args.add("-output", btf_vmlinux)
    pahole_args.add("-env")
    pahole_args.add(ctx.executable._llvm_objcopy, format = "LLVM_OBJCOPY=%s")
    pahole_args.add("--")
    pahole_args.add(ctx.executable.pahole)
    pahole_args.add("-J")
    pahole_args.add_all(linux_module_actions.pahole_flags(config, ctx.attr.version))
    pahole_args.add("{output}")
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._btfmutate,
        inputs = [input],
        tools = [
            ctx.attr.pahole[DefaultInfo].files_to_run,
            ctx.attr._llvm_objcopy[DefaultInfo].files_to_run,
        ],
        outputs = [btf_vmlinux],
        arguments = [pahole_args],
        mnemonic = "LinuxBTFEncode",
        progress_message = "Encoding Linux vmlinux BTF %{label}",
    )

    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + input.basename + ".btf.o")
    extract_args = ctx.actions.args()
    extract_args.add("-input", btf_vmlinux)
    extract_args.add("-output", out)
    extract_args.add(
        "-elf-et-rel-endian",
        "big" if config.config_flags.get("CONFIG_CPU_BIG_ENDIAN") == "y" else "little",
    )
    extract_args.add("--")
    extract_args.add(ctx.executable._llvm_objcopy)
    extract_args.add("--only-section=.BTF")
    extract_args.add("--set-section-flags")
    extract_args.add(".BTF=alloc,readonly")
    extract_args.add("--strip-all")
    extract_args.add(btf_vmlinux)
    extract_args.add("{output}")
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._btfmutate,
        inputs = [btf_vmlinux],
        tools = [ctx.attr._llvm_objcopy[DefaultInfo].files_to_run],
        outputs = [out],
        arguments = [extract_args],
        mnemonic = "LinuxBTFExtract",
        progress_message = "Extracting Linux vmlinux BTF %{label}",
    )
    return out

def _linux_resolve_btfids(ctx, config, input):
    if config.config_flags.get("CONFIG_DEBUG_INFO_BTF") != "y":
        return input
    if not ctx.executable.resolve_btfids_tool:
        fail("linux_vmlinux %s has DEBUG_INFO_BTF enabled and requires resolve_btfids_tool" % ctx.label)

    out = ctx.actions.declare_file(ctx.label.name + ".btfids.vmlinux.unstripped")
    args = ctx.actions.args()
    args.add("-input", input)
    args.add("-output", out)
    args.add("--")
    args.add(ctx.executable.resolve_btfids_tool)
    if config.config_flags.get("CONFIG_WERROR") == "y":
        args.add("--fatal_warnings")
    args.add("{output}")
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._btfmutate,
        inputs = [input],
        tools = [ctx.attr.resolve_btfids_tool[DefaultInfo].files_to_run],
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxResolveBTFIDs",
        progress_message = "Resolving Linux vmlinux BTF IDs %{label}",
    )
    return out

def _linux_vmlinux_link(ctx, linker, cc_toolchain, feature_configuration, image_object, image_object_inputs, export_object, version_object, linker_script, kallsyms_object, btf_object, out, strip_debug):
    inputs = depset(
        [image_object, export_object, version_object, linker_script],
        transitive = [image_object_inputs, cc_toolchain.all_files],
    )
    executable = linker
    args = ctx.actions.args()
    if ctx.attr.format == "arm64":
        executable = _linux_x86_tool_sibling(linker, "ld.lld")
        args.add_all(_linux_arm64_vmlinux_ld_flags(ctx.attr.config[LinuxConfigInfo]))
        args.add(linker_script, format = "--script=%s")
        if strip_debug:
            args.add("--strip-debug")
        whole_archive = "--whole-archive"
        no_whole_archive = "--no-whole-archive"
    else:
        args.add_all(_cc_target_flags(ctx, cc_toolchain, feature_configuration))
        args.add_all(_linux_vmlinux_link_flags(ctx, ctx.attr.config[LinuxConfigInfo]))
        args.add(linker_script, format = "-Wl,--script=%s")
        if strip_debug:
            args.add("-Wl,--strip-debug")
        whole_archive = "-Wl,--whole-archive"
        no_whole_archive = "-Wl,--no-whole-archive"
    args.add("-o")
    args.add(out)
    args.add(whole_archive)
    args.add(image_object)
    args.add(export_object)
    args.add(version_object)
    args.add(no_whole_archive)
    if ctx.attr.format == "arm64":
        args.add("--start-group")
        args.add("--end-group")
    else:
        args.add("-Wl,--start-group")
        args.add("-Wl,--end-group")
    if kallsyms_object:
        args.add(kallsyms_object)
        inputs = depset([kallsyms_object], transitive = [inputs])
    if btf_object:
        args.add(btf_object)
        inputs = depset([btf_object], transitive = [inputs])

    path_mapped_run(
        ctx.actions,
        executable = executable,
        inputs = inputs,
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxVmlinuxLink",
        progress_message = "Linking Linux vmlinux %{label}",
    )
    return out

def _linux_arm64_vmlinux_ld_flags(config):
    flags = [
        "-EL",
        "-maarch64elf",
        "--no-undefined",
        "-X",
        "--pic-veneer",
        "-z",
        "norelro",
        "-z",
        "noexecstack",
        "--build-id=sha1",
        "--orphan-handling=warn",
    ]
    if config.config_flags.get("CONFIG_LD_DEAD_CODE_DATA_ELIMINATION") == "y":
        flags.append("--gc-sections")
    if config.config_flags.get("CONFIG_ARCH_VMLINUX_NEEDS_RELOCS") == "y":
        flags.extend([
            "--emit-relocs",
            "--discard-none",
        ])
    if config.config_flags.get("CONFIG_RELOCATABLE") == "y":
        flags.extend([
            "-shared",
            "-Bsymbolic",
            "-z",
            "notext",
            "--no-apply-dynamic-relocs",
        ])
    return flags

def _linux_vmlinux_link_flags(ctx, config):
    if ctx.attr.format == "arm64":
        flags = [
            "-fuse-ld=lld",
            "-nostdlib",
            "-Wl,--no-undefined",
            "-Wl,-X",
            "-Wl,--pic-veneer",
            "-Wl,-EL",
            "-Wl,-maarch64elf",
            "-Wl,-z,norelro",
            "-Wl,-z,noexecstack",
            "-Wl,--build-id=sha1",
            "-Wl,--orphan-handling=warn",
        ]
        if config.config_flags.get("CONFIG_LD_DEAD_CODE_DATA_ELIMINATION") == "y":
            flags.append("-Wl,--gc-sections")
        if config.config_flags.get("CONFIG_LTO_CLANG_THIN") == "y":
            flags.extend([
                "-flto=thin",
                "-Wl,-mllvm,-import-instr-limit=5",
            ])
        elif config.config_flags.get("CONFIG_LTO_CLANG_FULL") == "y":
            flags.extend([
                "-flto",
                "-Wl,-mllvm,-import-instr-limit=5",
            ])
        if config.config_flags.get("CONFIG_ARCH_VMLINUX_NEEDS_RELOCS") == "y":
            flags.extend([
                "-Wl,--emit-relocs",
                "-Wl,--discard-none",
            ])
        if config.config_flags.get("CONFIG_RELOCATABLE") == "y":
            flags.extend([
                "-shared",
                "-Wl,-Bsymbolic",
                "-Wl,-z,notext",
                "-Wl,--no-apply-dynamic-relocs",
            ])
        else:
            flags.append("-no-pie")
        return flags
    flags = [
        "-fuse-ld=lld",
        "-nostdlib",
        "-no-pie",
        "-Wl,-m,elf_x86_64",
        "-Wl,-z,noexecstack",
        "-Wl,-z,max-page-size=0x200000",
        "-Wl,--build-id=sha1",
        "-Wl,--orphan-handling=warn",
    ]
    if config.config_flags.get("CONFIG_LD_DEAD_CODE_DATA_ELIMINATION") == "y":
        flags.append("-Wl,--gc-sections")
    if config.config_flags.get("CONFIG_LTO_CLANG_THIN") == "y":
        flags.extend([
            "-flto=thin",
            "-Wl,-mllvm,-import-instr-limit=5",
        ])
    elif config.config_flags.get("CONFIG_LTO_CLANG_FULL") == "y":
        flags.extend([
            "-flto",
            "-Wl,-mllvm,-import-instr-limit=5",
        ])
    if config.config_flags.get("CONFIG_ARCH_VMLINUX_NEEDS_RELOCS") == "y":
        flags.extend([
            "-Wl,--emit-relocs",
            "-Wl,--discard-none",
        ])
    return flags

def _linux_vmlinux_initcalls_linker_script(ctx, config):
    if config.config_flags.get("CONFIG_LTO_CLANG_THIN") != "y" and config.config_flags.get("CONFIG_LTO_CLANG_FULL") != "y":
        return None

    out = ctx.actions.declare_file(ctx.label.name + ".obj/.tmp_initcalls.lds")
    ctx.actions.write(out, """SECTIONS {
  .initcallearly.init : { *(.initcallearly.init..*) }
  .initcall0.init : { *(.initcall0.init..*) }
  .initcall0s.init : { *(.initcall0s.init..*) }
  .initcall1.init : { *(.initcall1.init..*) }
  .initcall1s.init : { *(.initcall1s.init..*) }
  .initcall2.init : { *(.initcall2.init..*) }
  .initcall2s.init : { *(.initcall2s.init..*) }
  .initcall3.init : { *(.initcall3.init..*) }
  .initcall3s.init : { *(.initcall3s.init..*) }
  .initcall4.init : { *(.initcall4.init..*) }
  .initcall4s.init : { *(.initcall4s.init..*) }
  .initcall5.init : { *(.initcall5.init..*) }
  .initcall5s.init : { *(.initcall5s.init..*) }
  .initcallrootfs.init : { *(.initcallrootfs.init..*) }
  .initcall6.init : { *(.initcall6.init..*) }
  .initcall6s.init : { *(.initcall6s.init..*) }
  .initcall7.init : { *(.initcall7.init..*) }
  .initcall7s.init : { *(.initcall7s.init..*) }
  .con_initcall.init : { *(.con_initcall.init..*) }
}
""")
    return out

def _linux_vmlinux_relocatable_object(ctx, config, linker, cc_toolchain, feature_configuration, image_object, image_object_inputs, extra_objects):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/vmlinux.o")
    initcalls_linker_script = _linux_vmlinux_initcalls_linker_script(ctx, config)
    args = ctx.actions.args()
    if ctx.attr.format == "arm64":
        executable = _linux_x86_tool_sibling(linker, "ld.lld")
        flags = [
            "-EL",
            "-maarch64elf",
            "-z",
            "norelro",
            "-z",
            "noexecstack",
            "-r",
        ]
        if config.config_flags.get("CONFIG_LTO_CLANG_THIN") == "y":
            flags.extend([
                "-mllvm",
                "-import-instr-limit=5",
            ])
        elif config.config_flags.get("CONFIG_LTO_CLANG_FULL") == "y":
            flags.extend([
                "-mllvm",
                "-import-instr-limit=5",
            ])
        flags.extend([
            "-o",
            out,
        ])
        args.add_all(flags)
        if initcalls_linker_script:
            args.add("-T")
            args.add(initcalls_linker_script)
        args.add("--whole-archive")
        args.add(image_object)
        args.add_all(extra_objects)
        args.add("--no-whole-archive")
    else:
        executable = linker
        args.add_all(_cc_target_flags(ctx, cc_toolchain, feature_configuration))
        flags = [
            "-fuse-ld=lld",
            "-nostdlib",
            "-no-pie",
            "-Wl,-r",
            "-Wl,-m,elf_x86_64",
        ]
        if config.config_flags.get("CONFIG_LTO_CLANG_THIN") == "y":
            flags.extend([
                "-flto=thin",
                "-Wl,-mllvm,-import-instr-limit=5",
            ])
        elif config.config_flags.get("CONFIG_LTO_CLANG_FULL") == "y":
            flags.extend([
                "-flto",
                "-Wl,-mllvm,-import-instr-limit=5",
            ])
        flags.extend([
            "-o",
            out,
        ])
        args.add_all(flags)
        if initcalls_linker_script:
            args.add(initcalls_linker_script, format = "-Wl,-T,%s")
        args.add("-Wl,--whole-archive")
        args.add(image_object)
        args.add_all(extra_objects)
        args.add("-Wl,--no-whole-archive")

    path_mapped_run(
        ctx.actions,
        executable = executable,
        inputs = depset(
            [image_object] + extra_objects + ([initcalls_linker_script] if initcalls_linker_script else []),
            transitive = [image_object_inputs, cc_toolchain.all_files],
        ),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxVmlinuxRelocatable",
        progress_message = "Linking Linux relocatable vmlinux.o %{label}",
    )
    return out

def _linux_vmlinux_objtool(ctx, config, linker, cc_toolchain, feature_configuration, image_object, image_object_inputs, extra_objects):
    # Upstream modpost consumes vmlinux.o even when neither LTO nor objtool is
    # enabled. Always materialize the relocatable link so the configured kernel
    # can expose a complete private module SDK without reconstructing it later.
    image_object = _linux_vmlinux_relocatable_object(
        ctx,
        config,
        linker,
        cc_toolchain,
        feature_configuration,
        image_object,
        image_object_inputs,
        extra_objects,
    )
    if not ctx.executable.objtool:
        return image_object

    out = ctx.actions.declare_file(ctx.label.name + ".obj/vmlinux.o.objtool")
    args = ctx.actions.args()
    args.add("-config", config.config)
    args.add("-objtool", ctx.executable.objtool)
    args.add("-in", image_object)
    args.add("-mode", "vmlinux")
    args.add("-out", out)
    path_mapped_run(
        ctx.actions,
        executable = ctx.attr._objtoolrun[DefaultInfo].files_to_run,
        inputs = [config.config, image_object],
        tools = [ctx.attr.objtool[DefaultInfo].files_to_run],
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxObjtool",
        progress_message = "Processing Linux vmlinux.o with objtool %{label}",
    )
    return out

def _linux_vmlinux_impl(ctx):
    if not ctx.attr.config:
        fail("linux_vmlinux with image requires config")
    if not ctx.attr.generated_headers:
        fail("linux_vmlinux with image requires generated_headers")
    if not ctx.file.source_root:
        fail("linux_vmlinux with image requires source_root")
    if not ctx.file.linker_script:
        fail("linux_vmlinux with image requires linker_script")

    image = ctx.attr.image[LinuxImageInfo]
    config = ctx.attr.config[LinuxConfigInfo]
    generated_headers = ctx.attr.generated_headers[LinuxGeneratedHeadersInfo]
    rust_sdk = ctx.attr.rust_sdk[LinuxRustSdkInfo] if ctx.attr.rust_sdk else None
    rust_enabled = config.config_flags.get("CONFIG_RUST") == "y"
    if rust_sdk == None or rust_sdk.enabled != rust_enabled:
        fail(
            "%s requires a Rust SDK whose enabled state matches CONFIG_RUST=%s" %
            (ctx.label, "y" if rust_enabled else "n"),
        )
    cc_toolchain = find_cpp_toolchain(ctx)
    feature_configuration = _cc_feature_configuration(ctx, cc_toolchain)
    compiler = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
    )
    linker = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = CPP_LINK_EXECUTABLE_ACTION_NAME,
    )
    source_root = _linux_source_root_path(ctx)
    linker_script = _linux_vmlinux_linker_script(ctx, compiler, cc_toolchain, feature_configuration, config, generated_headers, source_root)
    version_object = _linux_vmlinux_compile_source(
        ctx,
        compiler,
        cc_toolchain,
        feature_configuration,
        config,
        generated_headers,
        source_root,
        _source_tree_file(ctx, "init/version-timestamp.c"),
        "init/version-timestamp.o",
        "init/version-timestamp.o",
        extra_flags = ["-fno-function-sections", "-fno-data-sections", "-include", "generated/utsversion.h"],
    )
    image_object_inputs = depset([info.output for info in image.objects])
    image_object = _linux_vmlinux_objtool(
        ctx,
        config,
        linker,
        cc_toolchain,
        feature_configuration,
        image.output,
        image_object_inputs,
        rust_sdk.runtime_objects,
    )
    module_prep = linux_module_actions.prepare_vmlinux(
        ctx,
        linux_module_cc_helpers,
        struct(
            config = config,
            generated_headers = generated_headers,
            source_root = _linux_source_root_file(ctx),
            source_tree = depset(ctx.files.source_tree),
            srcarch = ctx.attr.srcarch,
            version = ctx.attr.version,
            vmlinux_object = image_object,
        ),
    )
    export_object = _linux_vmlinux_export_object(
        ctx,
        compiler,
        cc_toolchain,
        feature_configuration,
        config,
        generated_headers,
        source_root,
        module_prep.export_source if module_prep != None else None,
    )

    kallsyms_object = None
    if ctx.attr.kallsyms == "auto":
        kallsyms_enabled = config.config_flags.get("CONFIG_KALLSYMS") == "y"
    else:
        kallsyms_enabled = ctx.attr.kallsyms == "true"
    if kallsyms_enabled:
        if not ctx.executable.kallsyms_tool:
            fail("linux_vmlinux %s has kallsyms enabled and requires kallsyms_tool" % ctx.label)
        empty_map = ctx.actions.declare_file(ctx.label.name + ".obj/.tmp_vmlinux0.syms")
        ctx.actions.write(empty_map, "")
        kallsyms_object = _linux_kallsyms_object(ctx, compiler, cc_toolchain, feature_configuration, config, generated_headers, source_root, empty_map, ".tmp_vmlinux0", ctx.executable.kallsyms_tool)

    btf_object = None
    if config.config_flags.get("CONFIG_DEBUG_INFO_BTF") == "y":
        btf_base = ctx.actions.declare_file(ctx.label.name + ".obj/.tmp_vmlinux.btf")
        _linux_vmlinux_link(
            ctx,
            linker,
            cc_toolchain,
            feature_configuration,
            image_object,
            image_object_inputs,
            export_object,
            version_object,
            linker_script,
            kallsyms_object,
            None,
            btf_base,
            False,
        )
        btf_object = _linux_btf_object(ctx, config, btf_base)

    if kallsyms_enabled:
        for i in range(1, 5):
            tmp = ctx.actions.declare_file(ctx.label.name + ".obj/.tmp_vmlinux%d" % i)
            _linux_vmlinux_link(ctx, linker, cc_toolchain, feature_configuration, image_object, image_object_inputs, export_object, version_object, linker_script, kallsyms_object, btf_object, tmp, True)
            system_map = _linux_system_map(ctx, tmp, ".tmp_vmlinux%d" % i)
            kallsyms_object = _linux_kallsyms_object(ctx, compiler, cc_toolchain, feature_configuration, config, generated_headers, source_root, system_map, ".tmp_vmlinux%d" % i, ctx.executable.kallsyms_tool)

    unstripped = ctx.actions.declare_file(ctx.label.name + ".vmlinux.unstripped")
    linked = _linux_vmlinux_link(ctx, linker, cc_toolchain, feature_configuration, image_object, image_object_inputs, export_object, version_object, linker_script, kallsyms_object, btf_object, unstripped, False)
    linked = _linux_resolve_btfids(ctx, config, linked)
    unstripped = _linux_sorttable(ctx, config, linked)
    out = ctx.actions.declare_file(ctx.label.name + ".vmlinux")
    out = _linux_strip_vmlinux(ctx, config, unstripped, out)
    system_map = _linux_system_map(ctx, out, "System.map")
    info = LinuxImageInfo(
        archives = image.archives,
        module_objects = image.module_objects,
        objects = image.objects,
        output = out,
    )
    return [
        DefaultInfo(files = depset([out, system_map])),
        info,
        LinuxVmlinuxInfo(
            arch = "aarch64" if ctx.attr.arch == "arm64" else "x86_64",
            config = config,
            generated_headers = generated_headers,
            module_symvers = module_prep.module_symvers if module_prep != None else None,
            modpost = module_prep.modpost if module_prep != None else None,
            source_root = _linux_source_root_file(ctx),
            source_tree = depset(ctx.files.source_tree),
            srcarch = ctx.attr.srcarch,
            rust = rust_sdk,
            vmlinux = out,
            vmlinux_unstripped = unstripped,
            vmlinux_object = image_object,
        ),
        OutputGroupInfo(system_map = depset([system_map]), vmlinux = depset([out])),
    ]

linux_vmlinux = rule(
    implementation = _linux_vmlinux_impl,
    attrs = {
        "arch": attr.string(default = "x86"),
        "config": attr.label(providers = [LinuxConfigInfo]),
        "format": attr.string(
            default = "x86_64",
            values = [
                "arm64",
                "x86_64",
            ],
        ),
        "generated_headers": attr.label(providers = [LinuxGeneratedHeadersInfo]),
        "image": attr.label(providers = [LinuxImageInfo], mandatory = True),
        "kallsyms": attr.string(default = "auto", values = ["auto", "false", "true"]),
        "kallsyms_all": attr.bool(),
        "kallsyms_pc_relative": attr.bool(),
        "linker_script": attr.label(allow_single_file = True),
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
        "rust_sdk": attr.label(providers = [LinuxRustSdkInfo]),
        "source_root": attr.label(allow_single_file = True),
        "source_tree": attr.label_list(allow_files = True),
        "srcarch": attr.string(),
        "version": attr.string(mandatory = True),
        "kallsyms_tool": attr.label(
            cfg = "exec",
            doc = "Kernel-source-specific scripts/kallsyms executable. Required when kallsyms is enabled for a vmlinux link.",
            executable = True,
        ),
        "sorttable_tool": attr.label(
            cfg = "exec",
            doc = "Kernel-source-specific scripts/sorttable executable. Required when BUILDTIME_TABLE_SORT is enabled.",
            executable = True,
        ),
        "_llvm_nm": attr.label(
            allow_single_file = True,
            cfg = "exec",
            default = Label("@llvm//tools:llvm-nm"),
            executable = True,
        ),
        "_btfmutate": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/btfmutate"),
            executable = True,
        ),
        "_host_cc_toolchain": host_cc_toolchain_attr(exec_group = "host_cc"),
        "_llvm_objcopy": attr.label(
            allow_single_file = True,
            cfg = "exec",
            default = Label("@llvm//tools:llvm-objcopy"),
            executable = True,
        ),
        "_modulemodinfo": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/modulemodinfo"),
            executable = True,
        ),
        "_offsetsheader": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/offsetsheader"),
            executable = True,
        ),
        "_mksysmap": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/mksysmap"),
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
        "_sorttablerun": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/sorttablerun"),
            executable = True,
        ),
        "_runandwrite": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/runandwrite"),
            executable = True,
        ),
        "_runincwd": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/runincwd"),
            executable = True,
        ),
    },
    exec_groups = {
        "host_cc": exec_group(toolchains = use_cc_toolchain()),
    },
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Links a native vmlinux ELF from a compact image.",
)

def _linux_x86_config_enabled(config, key):
    return config.config_flags.get(key) == "y"

def _linux_x86_generated_header(generated_headers, suffix):
    return _linux_generated_header(generated_headers, suffix)

def _linux_generated_header(generated_headers, suffix):
    for file in generated_headers.files.to_list():
        if file.short_path.endswith(suffix):
            return file
    fail("generated Linux header %s was not found" % suffix)

def _linux_x86_boot_inputs(ctx, cc_toolchain, config, generated_headers, extra = []):
    return depset(
        _linux_source_tree_inputs(ctx, direct = extra),
        transitive = [cc_toolchain.all_files, config.files, generated_headers.files],
    )

def _linux_x86_add_include_flags(args, config, generated_headers, source_root, extra = [], extra_anchors = {}):
    _add_config_include_flag(args, config)
    _add_linux_source_include_flags_for_root(
        args,
        source_root,
        "x86",
        generated_headers.include_dirs,
        _generated_include_dir_anchors(generated_headers),
    )
    _add_directory_flags(args, extra, extra_anchors)

def _linux_x86_run_x86boot(ctx, outputs, arguments, inputs = [], tools = []):
    args = ctx.actions.args()
    args.add_all(arguments)
    path_mapped_run(
        ctx.actions,
        executable = ctx.attr._x86boot[DefaultInfo].files_to_run,
        inputs = inputs,
        tools = tools,
        outputs = outputs,
        arguments = [args],
        mnemonic = "LinuxX86BootTool",
        progress_message = "Generating Linux x86 boot data %{label}",
    )

def _linux_x86_tool_sibling(tool, name):
    parts = tool.rsplit("/", 1)
    if len(parts) == 1:
        return name
    return parts[0] + "/" + name

def _linux_x86_objcopy(ctx, input, out_relpath, flags):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + out_relpath)
    args = ctx.actions.args()
    args.add_all(flags)
    args.add(input)
    args.add(out)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._llvm_objcopy,
        inputs = [input, ctx.executable._llvm_objcopy],
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxX86Objcopy",
        progress_message = "Objcopying Linux x86 boot input %{label}",
    )
    return out

def _linux_x86_relocs(ctx, vmlinux):
    if not ctx.executable.x86_relocs_tool:
        fail("linux_compressed_image with CONFIG_X86_NEED_RELOCS requires x86_relocs_tool")
    out = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/boot/compressed/vmlinux.relocs")
    _linux_x86_run_x86boot(
        ctx,
        [out],
        ["relocs", "-tool", ctx.executable.x86_relocs_tool, "-in", vmlinux, "-out", out],
        inputs = [vmlinux],
        tools = [ctx.attr.x86_relocs_tool[DefaultInfo].files_to_run],
    )
    return out

def _linux_x86_concat(ctx, inputs, out_relpath):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + out_relpath)
    args = ["concat"]
    for input in inputs:
        args.extend(["-in", input])
    args.extend(["-out", out])
    _linux_x86_run_x86boot(ctx, [out], args, inputs = inputs)
    return out

def _linux_x86_append_size(ctx, payload, size_inputs, out_relpath):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + out_relpath)
    args = ["append-size"]
    for input in payload:
        args.extend(["-in", input])
    for input in size_inputs:
        args.extend(["-size-in", input])
    args.extend(["-out", out])
    _linux_x86_run_x86boot(ctx, [out], args, inputs = payload + size_inputs)
    return out

def _linux_x86_lz4(ctx, input, out_relpath):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + out_relpath)
    args = ctx.actions.args()
    args.add_all(["-l", "-9"])
    args.add(input)
    args.add(out)
    path_mapped_run(
        ctx.actions,
        executable = ctx.attr._lz4[DefaultInfo].files_to_run,
        inputs = [input],
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxX86LZ4",
        progress_message = "Compressing Linux x86 kernel payload %{label}",
    )
    return out

def _linux_x86_gzip(ctx, input, out_relpath):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + out_relpath)
    _linux_x86_run_x86boot(
        ctx,
        [out],
        ["gzip", "-in", input, "-out", out],
        inputs = [input],
    )
    return out

def _linux_x86_compress(ctx, config, input):
    if _linux_x86_config_enabled(config, "CONFIG_KERNEL_GZIP"):
        return _linux_x86_gzip(ctx, input, "arch/x86/boot/compressed/vmlinux.bin.gz.raw")
    if _linux_x86_config_enabled(config, "CONFIG_KERNEL_LZ4"):
        return _linux_x86_lz4(ctx, input, "arch/x86/boot/compressed/vmlinux.bin.lz4.raw")
    fail("x86 kernel compression must select CONFIG_KERNEL_GZIP=y or CONFIG_KERNEL_LZ4=y")

def _linux_x86_piggy(ctx, compressed):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/boot/compressed/piggy.S")
    _linux_x86_run_x86boot(
        ctx,
        [out],
        ["piggy", "-in", compressed, "-out", out],
        inputs = [compressed],
    )
    return out

def _linux_x86_offsets(ctx, input, kind, out_relpath):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + out_relpath)
    _linux_x86_run_x86boot(
        ctx,
        [out],
        ["offsets", "-kind", kind, "-in", input, "-out", out],
        inputs = [input],
    )
    return out

def _linux_x86_bzimage(ctx, setup_bin, vmlinux_bin):
    out = ctx.actions.declare_file(ctx.label.name + "." + ctx.attr.extension)
    _linux_x86_run_x86boot(
        ctx,
        [out],
        ["bzimage", "-setup", setup_bin, "-kernel", vmlinux_bin, "-out", out],
        inputs = [setup_bin, vmlinux_bin],
    )
    return out

def _linux_x86_inat_tables(ctx):
    opcode_map = _source_tree_file(ctx, "arch/x86/lib/x86-opcode-map.txt")
    out = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/lib/inat-tables.c")
    args = ctx.actions.args()
    args.add("-in", opcode_map)
    args.add("-out", out)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._insnattr,
        inputs = [opcode_map],
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxX86InsnAttr",
        progress_message = "Generating Linux x86 boot instruction tables %{label}",
    )
    return out

def _linux_x86_compressed_compile(ctx, compiler, cc_toolchain, feature_configuration, config, generated_headers, source_root, src, object, extra_inputs = [], extra_include_dirs = []):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + object)
    hidden_header = _source_tree_file(ctx, "include/linux/hidden.h")
    assembly = _is_assembly_source(src)
    args = ctx.actions.args()
    args.add_all(_linux_compile_flags(ctx, cc_toolchain, feature_configuration))
    args.add_all([
        "-m64",
        "-O2",
        "-std=gnu11",
        "-fno-strict-aliasing",
        "-fPIE",
        "-Wundef",
        "-DDISABLE_BRANCH_PROFILING",
        "-mcmodel=small",
        "-mno-red-zone",
        "-mno-mmx",
        "-mno-sse",
        "-ffreestanding",
        "-fshort-wchar",
        "-fno-stack-protector",
        "-Wno-address-of-packed-member",
        "-Wno-gnu",
        "-Wno-pointer-sign",
        "-Wno-unused-command-line-argument",
        "-fno-asynchronous-unwind-tables",
        "-D__DISABLE_EXPORTS",
        "-Wa,-mrelax-relocations=no",
    ])
    if assembly:
        args.add("-D__ASSEMBLY__")
    args.add_all(_linux_object_name_flags(object))
    args.add_all(_linux_source_preinclude_flags_for_root(source_root, assembly))
    args.add("-include")
    args.add(hidden_header)
    _linux_x86_add_include_flags(
        args,
        config,
        generated_headers,
        source_root,
        extra = [
            source_root + "/arch/x86/boot/compressed",
            source_root + "/arch/x86/boot",
        ] + extra_include_dirs,
        extra_anchors = _available_directory_anchors([out] + extra_inputs, extra_include_dirs),
    )
    args.add("-c")
    args.add(src)
    args.add("-o")
    args.add(out)
    path_mapped_run(
        ctx.actions,
        executable = compiler,
        inputs = _linux_x86_boot_inputs(ctx, cc_toolchain, config, generated_headers, extra = [src, hidden_header] + extra_inputs),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxX86CompressedCompile",
        progress_message = "Compiling Linux x86 compressed boot object %{label}",
    )
    return out

def _linux_x86_compressed_linker_script(ctx, compiler, cc_toolchain, feature_configuration, config, generated_headers, source_root):
    src = _source_tree_file(ctx, "arch/x86/boot/compressed/vmlinux.lds.S")
    out = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/boot/compressed/vmlinux.lds")
    args = ctx.actions.args()
    args.add_all(_linux_compile_flags(ctx, cc_toolchain, feature_configuration))
    args.add_all([
        "-E",
        "-P",
        "-Ux86",
        "-Ux86_64",
        "-D__KERNEL__",
        "-D__ASSEMBLY__",
        "-DLINKER_SCRIPT",
        "-include",
        source_root + "/include/linux/kconfig.h",
    ])
    _linux_x86_add_include_flags(args, config, generated_headers, source_root, extra = [source_root + "/arch/x86/boot/compressed"])
    args.add(src)
    args.add("-o")
    args.add(out)
    path_mapped_run(
        ctx.actions,
        executable = compiler,
        inputs = _linux_x86_boot_inputs(ctx, cc_toolchain, config, generated_headers, extra = [src]),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxX86CompressedLinkerScript",
        progress_message = "Preprocessing Linux x86 compressed linker script %{label}",
    )
    return out

def _linux_x86_efi_stub_compile(ctx, compiler, cc_toolchain, feature_configuration, config, generated_headers, source_root, src_relpath, object):
    src = _source_tree_file_for_root(ctx, source_root, src_relpath)
    hidden_header = _source_tree_file(ctx, "include/linux/hidden.h")
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + object)
    compile_out = ctx.actions.declare_file(ctx.label.name + ".obj/" + object[:-len(".stub.o")] + ".o")
    args = ctx.actions.args()
    args.add_all(_linux_compile_flags(ctx, cc_toolchain, feature_configuration))
    args.add_all([
        "-m64",
        "-D__KERNEL__",
        "-std=gnu11",
        "-fPIC",
        "-fno-strict-aliasing",
        "-mno-red-zone",
        "-mno-mmx",
        "-mno-sse",
        "-fshort-wchar",
        "-Wno-pointer-sign",
        "-Wno-address-of-packed-member",
        "-Wno-gnu",
        "-Wno-unused-command-line-argument",
        "-fno-asynchronous-unwind-tables",
        "-mcmodel=small",
        "-Os",
        "-DDISABLE_BRANCH_PROFILING",
        "-D__NO_FORTIFY",
        "-ffreestanding",
        "-fno-stack-protector",
        "-fno-addrsig",
        "-D__DISABLE_EXPORTS",
    ])
    args.add_all(_linux_object_name_flags(_linux_compile_object_name(object)))
    args.add_all(_linux_source_preinclude_flags_for_root(source_root, False))
    args.add("-include")
    args.add(hidden_header)
    _linux_x86_add_include_flags(
        args,
        config,
        generated_headers,
        source_root,
        extra = [source_root + "/drivers/firmware/efi/libstub"],
    )
    args.add("-c")
    args.add(src)
    args.add("-o")
    args.add(compile_out)
    path_mapped_run(
        ctx.actions,
        executable = compiler,
        inputs = _linux_x86_boot_inputs(ctx, cc_toolchain, config, generated_headers, extra = [src, hidden_header]),
        outputs = [compile_out],
        arguments = [args],
        mnemonic = "LinuxX86EFIStubCompile",
        progress_message = "Compiling Linux x86 EFI stub object %{label}",
    )

    objcopy_args = ctx.actions.args()
    objcopy_args.add("--remove-section=.note.gnu.property")
    objcopy_args.add(compile_out)
    objcopy_args.add(out)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._llvm_objcopy,
        inputs = [compile_out, ctx.executable._llvm_objcopy],
        outputs = [out],
        arguments = [objcopy_args],
        mnemonic = "LinuxX86EFIStubObjcopy",
        progress_message = "Objcopying Linux x86 EFI stub object %{label}",
    )
    return out

def _linux_x86_archive(ctx, archiver, cc_toolchain, out_relpath, objects):
    if not objects:
        return None
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + out_relpath)
    args = ctx.actions.args()
    args.add("cDPrS")
    args.add(out)
    args.add_all(objects)
    path_mapped_run(
        ctx.actions,
        executable = archiver,
        inputs = depset(objects, transitive = [cc_toolchain.all_files]),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxX86Archive",
        progress_message = "Archiving Linux x86 boot library %{label}",
    )
    return out

def _linux_x86_image_object_outputs(image, exact = [], prefix = ""):
    wanted = {path: True for path in exact}
    out = []
    for object in image.objects:
        if object.object in wanted or (prefix and object.object.startswith(prefix)):
            out.append(object.output)
    return out

def _linux_x86_compressed_vmlinux(ctx, compiler, linker, archiver, cc_toolchain, feature_configuration, config, generated_headers, source_root, image, voffset):
    stripped = _linux_x86_objcopy(ctx, image.output, "arch/x86/boot/compressed/vmlinux.bin", ["-R", ".comment", "-S"])
    payload_inputs = [stripped]
    if ctx.executable.x86_relocs_tool:
        payload_inputs.append(_linux_x86_relocs(ctx, image.output))
    payload = _linux_x86_concat(ctx, payload_inputs, "arch/x86/boot/compressed/vmlinux.bin.all")
    compressed = _linux_x86_compress(ctx, config, payload)
    compressed_with_size = _linux_x86_append_size(ctx, [compressed], [payload], "arch/x86/boot/compressed/vmlinux.bin.compressed")
    piggy = _linux_x86_piggy(ctx, compressed_with_size)
    inat_tables = _linux_x86_inat_tables(ctx)

    source_specs = [
        ("arch/x86/boot/compressed/kernel_info.S", "arch/x86/boot/compressed/kernel_info.o", [], []),
        ("arch/x86/boot/compressed/head_64.S", "arch/x86/boot/compressed/head_64.o", [], []),
        ("arch/x86/boot/compressed/misc.c", "arch/x86/boot/compressed/misc.o", [voffset], [voffset.dirname + "/compressed"]),
        ("arch/x86/boot/compressed/string.c", "arch/x86/boot/compressed/string.o", [], []),
        ("arch/x86/boot/compressed/cmdline.c", "arch/x86/boot/compressed/cmdline.o", [], []),
        ("arch/x86/boot/compressed/error.c", "arch/x86/boot/compressed/error.o", [], []),
        ("arch/x86/boot/compressed/cpuflags.c", "arch/x86/boot/compressed/cpuflags.o", [], []),
        ("arch/x86/boot/compressed/ident_map_64.c", "arch/x86/boot/compressed/ident_map_64.o", [], []),
        ("arch/x86/boot/compressed/idt_64.c", "arch/x86/boot/compressed/idt_64.o", [], []),
        ("arch/x86/boot/compressed/idt_handlers_64.S", "arch/x86/boot/compressed/idt_handlers_64.o", [], []),
        ("arch/x86/boot/compressed/pgtable_64.c", "arch/x86/boot/compressed/pgtable_64.o", [], []),
    ]
    if _linux_x86_config_enabled(config, "CONFIG_EARLY_PRINTK"):
        source_specs.append(("arch/x86/boot/compressed/early_serial_console.c", "arch/x86/boot/compressed/early_serial_console.o", [], []))
    if _linux_x86_config_enabled(config, "CONFIG_RANDOMIZE_BASE"):
        source_specs.append(("arch/x86/boot/compressed/kaslr.c", "arch/x86/boot/compressed/kaslr.o", [], []))
    if _linux_x86_config_enabled(config, "CONFIG_ACPI"):
        source_specs.append(("arch/x86/boot/compressed/acpi.c", "arch/x86/boot/compressed/acpi.o", [], []))
    if _linux_x86_config_enabled(config, "CONFIG_AMD_MEM_ENCRYPT"):
        source_specs.extend([
            ("arch/x86/boot/compressed/mem_encrypt.S", "arch/x86/boot/compressed/mem_encrypt.o", [], []),
            ("arch/x86/boot/compressed/sev.c", "arch/x86/boot/compressed/sev.o", [], []),
            ("arch/x86/boot/compressed/sev-handle-vc.c", "arch/x86/boot/compressed/sev-handle-vc.o", [inat_tables], [inat_tables.dirname]),
        ])
    if _linux_x86_config_enabled(config, "CONFIG_UNACCEPTED_MEMORY"):
        source_specs.append(("arch/x86/boot/compressed/mem.c", "arch/x86/boot/compressed/mem.o", [], []))
    if _linux_x86_config_enabled(config, "CONFIG_EFI"):
        source_specs.append(("arch/x86/boot/compressed/efi.c", "arch/x86/boot/compressed/efi.o", [], []))

    objects = []
    for src_relpath, object, extra_inputs, extra_include_dirs in source_specs:
        objects.append(_linux_x86_compressed_compile(
            ctx,
            compiler,
            cc_toolchain,
            feature_configuration,
            config,
            generated_headers,
            source_root,
            _source_tree_file(ctx, src_relpath),
            object,
            extra_inputs = extra_inputs,
            extra_include_dirs = extra_include_dirs,
        ))
    objects.append(_linux_x86_compressed_compile(
        ctx,
        compiler,
        cc_toolchain,
        feature_configuration,
        config,
        generated_headers,
        source_root,
        piggy,
        "arch/x86/boot/compressed/piggy.o",
        extra_inputs = [compressed_with_size],
    ))

    linker_script = _linux_x86_compressed_linker_script(ctx, compiler, cc_toolchain, feature_configuration, config, generated_headers, source_root)
    archives = []
    efi_archive = None
    if _linux_x86_config_enabled(config, "CONFIG_EFI_STUB"):
        efi_stub_specs = [
            ("drivers/firmware/efi/libstub/efi-stub-helper.c", "drivers/firmware/efi/libstub/efi-stub-helper.stub.o"),
            ("drivers/firmware/efi/libstub/gop.c", "drivers/firmware/efi/libstub/gop.stub.o"),
            ("drivers/firmware/efi/libstub/secureboot.c", "drivers/firmware/efi/libstub/secureboot.stub.o"),
            ("drivers/firmware/efi/libstub/tpm.c", "drivers/firmware/efi/libstub/tpm.stub.o"),
            ("drivers/firmware/efi/libstub/file.c", "drivers/firmware/efi/libstub/file.stub.o"),
            ("drivers/firmware/efi/libstub/mem.c", "drivers/firmware/efi/libstub/mem.stub.o"),
            ("drivers/firmware/efi/libstub/random.c", "drivers/firmware/efi/libstub/random.stub.o"),
            ("drivers/firmware/efi/libstub/randomalloc.c", "drivers/firmware/efi/libstub/randomalloc.stub.o"),
            ("drivers/firmware/efi/libstub/pci.c", "drivers/firmware/efi/libstub/pci.stub.o"),
            ("drivers/firmware/efi/libstub/skip_spaces.c", "drivers/firmware/efi/libstub/skip_spaces.stub.o"),
            ("lib/cmdline.c", "drivers/firmware/efi/libstub/lib-cmdline.stub.o"),
            ("lib/ctype.c", "drivers/firmware/efi/libstub/lib-ctype.stub.o"),
            ("drivers/firmware/efi/libstub/alignedmem.c", "drivers/firmware/efi/libstub/alignedmem.stub.o"),
            ("drivers/firmware/efi/libstub/relocate.c", "drivers/firmware/efi/libstub/relocate.stub.o"),
            ("drivers/firmware/efi/libstub/printk.c", "drivers/firmware/efi/libstub/printk.stub.o"),
            ("drivers/firmware/efi/libstub/vsprintf.c", "drivers/firmware/efi/libstub/vsprintf.stub.o"),
            ("drivers/firmware/efi/libstub/x86-stub.c", "drivers/firmware/efi/libstub/x86-stub.stub.o"),
            ("drivers/firmware/efi/libstub/smbios.c", "drivers/firmware/efi/libstub/smbios.stub.o"),
            ("drivers/firmware/efi/libstub/x86-5lvl.c", "drivers/firmware/efi/libstub/x86-5lvl.stub.o"),
        ]
        if _linux_x86_config_enabled(config, "CONFIG_UNACCEPTED_MEMORY"):
            efi_stub_specs.extend([
                ("drivers/firmware/efi/libstub/unaccepted_memory.c", "drivers/firmware/efi/libstub/unaccepted_memory.stub.o"),
                ("drivers/firmware/efi/libstub/bitmap.c", "drivers/firmware/efi/libstub/bitmap.stub.o"),
                ("drivers/firmware/efi/libstub/find.c", "drivers/firmware/efi/libstub/find.stub.o"),
            ])

        efi_stub_objects = []
        for src_relpath, object in efi_stub_specs:
            efi_stub_objects.append(_linux_x86_efi_stub_compile(
                ctx,
                compiler,
                cc_toolchain,
                feature_configuration,
                config,
                generated_headers,
                source_root,
                src_relpath,
                object,
            ))

        efi_archive = _linux_x86_archive(
            ctx,
            archiver,
            cc_toolchain,
            "drivers/firmware/efi/libstub/lib.a",
            efi_stub_objects,
        )
        if efi_archive:
            archives.append(efi_archive)
    startup_archive = _linux_x86_archive(
        ctx,
        archiver,
        cc_toolchain,
        "arch/x86/boot/startup/lib.a",
        _linux_x86_image_object_outputs(image, exact = ["arch/x86/boot/startup/la57toggle.o"]),
    )
    if startup_archive:
        archives.append(startup_archive)

    out = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/boot/compressed/vmlinux")
    args = ctx.actions.args()
    args.add("-fuse-ld=lld")
    args.add("-nostdlib")
    args.add("-pie")
    args.add("-Wl,-m,elf_x86_64")
    args.add("-Wl,--no-dynamic-linker")
    args.add("-Wl,-z,noexecstack")
    if _linux_x86_config_enabled(config, "CONFIG_LD_ORPHAN_WARN"):
        args.add("-Wl,--orphan-handling=" + _unquote(config.config_flags.get("CONFIG_LD_ORPHAN_WARN_LEVEL", "warn")))
    if efi_archive:
        args.add("-Wl,-u,efi_pe_entry")
    args.add(linker_script, format = "-Wl,-T,%s")
    args.add("-o")
    args.add(out)
    args.add_all(objects)
    args.add_all(archives)
    path_mapped_run(
        ctx.actions,
        executable = linker,
        inputs = depset(objects + archives + [linker_script], transitive = [cc_toolchain.all_files]),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxX86CompressedLink",
        progress_message = "Linking Linux x86 compressed boot kernel %{label}",
    )
    return out

def _linux_x86_setup_compile(ctx, compiler, cc_toolchain, feature_configuration, config, generated_headers, source_root, src, object, extra_inputs = [], extra_include_dirs = [], extra_flags = []):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + object)
    assembly = _is_assembly_source(src)
    args = ctx.actions.args()
    args.add_all(_linux_compile_flags(ctx, cc_toolchain, feature_configuration))
    args.add_all([
        "-std=gnu11",
        "-m16",
        "-g",
        "-Os",
        "-DDISABLE_BRANCH_PROFILING",
        "-D__DISABLE_EXPORTS",
        "-Wall",
        "-Wstrict-prototypes",
        "-march=i386",
        "-mregparm=3",
        "-fno-strict-aliasing",
        "-fomit-frame-pointer",
        "-fno-pic",
        "-mno-mmx",
        "-mno-sse",
        "-fcf-protection=none",
        "-ffreestanding",
        "-fno-stack-protector",
        "-Wno-address-of-packed-member",
        "-mstack-alignment=4",
        "-Wno-gnu",
        "-D_SETUP",
        "-fno-asynchronous-unwind-tables",
        "-Wno-unused-command-line-argument",
    ])
    if assembly:
        args.add("-D__ASSEMBLY__")
    args.add_all(_linux_object_name_flags(object))
    args.add_all(_linux_source_preinclude_flags_for_root(source_root, assembly))
    _linux_x86_add_include_flags(
        args,
        config,
        generated_headers,
        source_root,
        extra = [source_root + "/arch/x86/boot"] + extra_include_dirs,
        extra_anchors = _available_directory_anchors([out] + extra_inputs, extra_include_dirs),
    )
    args.add_all(extra_flags)
    args.add("-c")
    args.add(src)
    args.add("-o")
    args.add(out)
    path_mapped_run(
        ctx.actions,
        executable = compiler,
        inputs = _linux_x86_boot_inputs(ctx, cc_toolchain, config, generated_headers, extra = [src] + extra_inputs),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxX86SetupCompile",
        progress_message = "Compiling Linux x86 setup object %{label}",
    )
    return out

def _linux_x86_cpustr(ctx, generated_headers):
    cpufeatures = _source_tree_file(ctx, "arch/x86/include/asm/cpufeatures.h")
    masks = _linux_x86_generated_header(generated_headers, "arch/x86/include/generated/asm/cpufeaturemasks.h")
    out = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/boot/cpustr.h")
    _linux_x86_run_x86boot(
        ctx,
        [out],
        ["cpustr", "-cpufeatures", cpufeatures, "-masks", masks, "-out", out],
        inputs = [cpufeatures, masks],
    )
    return out

def _linux_x86_setup_bin(ctx, compiler, linker, cc_toolchain, feature_configuration, config, generated_headers, source_root, voffset, zoffset):
    cpustr = _linux_x86_cpustr(ctx, generated_headers)
    source_specs = [
        ("arch/x86/boot/a20.c", "arch/x86/boot/a20.o", [], [], []),
        ("arch/x86/boot/bioscall.S", "arch/x86/boot/bioscall.o", [], [], []),
        ("arch/x86/boot/cmdline.c", "arch/x86/boot/cmdline.o", [], [], []),
        ("arch/x86/boot/copy.S", "arch/x86/boot/copy.o", [], [], []),
        ("arch/x86/boot/cpu.c", "arch/x86/boot/cpu.o", [cpustr], [cpustr.dirname], []),
        ("arch/x86/boot/cpuflags.c", "arch/x86/boot/cpuflags.o", [], [], []),
        ("arch/x86/boot/cpucheck.c", "arch/x86/boot/cpucheck.o", [], [], []),
        ("arch/x86/boot/early_serial_console.c", "arch/x86/boot/early_serial_console.o", [], [], []),
        ("arch/x86/boot/edd.c", "arch/x86/boot/edd.o", [], [], []),
        ("arch/x86/boot/header.S", "arch/x86/boot/header.o", [voffset, zoffset], [voffset.dirname], ["-DSVGA_MODE=NORMAL_VGA"]),
        ("arch/x86/boot/main.c", "arch/x86/boot/main.o", [], [], []),
        ("arch/x86/boot/memory.c", "arch/x86/boot/memory.o", [], [], []),
        ("arch/x86/boot/pm.c", "arch/x86/boot/pm.o", [], [], []),
        ("arch/x86/boot/pmjump.S", "arch/x86/boot/pmjump.o", [], [], []),
        ("arch/x86/boot/printf.c", "arch/x86/boot/printf.o", [], [], []),
        ("arch/x86/boot/regs.c", "arch/x86/boot/regs.o", [], [], []),
        ("arch/x86/boot/string.c", "arch/x86/boot/string.o", [], [], []),
        ("arch/x86/boot/tty.c", "arch/x86/boot/tty.o", [], [], []),
        ("arch/x86/boot/video.c", "arch/x86/boot/video.o", [], [], []),
        ("arch/x86/boot/video-mode.c", "arch/x86/boot/video-mode.o", [], [], []),
        ("arch/x86/boot/version.c", "arch/x86/boot/version.o", [], [], []),
        ("arch/x86/boot/video-vga.c", "arch/x86/boot/video-vga.o", [], [], []),
        ("arch/x86/boot/video-vesa.c", "arch/x86/boot/video-vesa.o", [], [], []),
        ("arch/x86/boot/video-bios.c", "arch/x86/boot/video-bios.o", [], [], []),
    ]
    if _linux_x86_config_enabled(config, "CONFIG_X86_APM_BOOT"):
        source_specs.append(("arch/x86/boot/apm.c", "arch/x86/boot/apm.o", [], [], []))

    objects = []
    for src_relpath, object, extra_inputs, extra_include_dirs, extra_flags in source_specs:
        objects.append(_linux_x86_setup_compile(
            ctx,
            compiler,
            cc_toolchain,
            feature_configuration,
            config,
            generated_headers,
            source_root,
            _source_tree_file(ctx, src_relpath),
            object,
            extra_inputs = extra_inputs,
            extra_include_dirs = extra_include_dirs,
            extra_flags = extra_flags,
        ))

    setup_ld = _source_tree_file(ctx, "arch/x86/boot/setup.ld")
    setup_elf = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/boot/setup.elf")
    ld = _linux_x86_tool_sibling(linker, "ld.lld")
    args = ctx.actions.args()
    args.add_all([
        "-m",
        "elf_i386",
        "-z",
        "noexecstack",
        "-T",
        setup_ld,
    ])
    args.add("-o")
    args.add(setup_elf)
    args.add_all(objects)
    path_mapped_run(
        ctx.actions,
        executable = ld,
        inputs = depset(objects + [setup_ld], transitive = [cc_toolchain.all_files]),
        outputs = [setup_elf],
        arguments = [args],
        mnemonic = "LinuxX86SetupLink",
        progress_message = "Linking Linux x86 setup ELF %{label}",
    )
    return _linux_x86_objcopy(ctx, setup_elf, "arch/x86/boot/setup.bin", ["-O", "binary"])

def _linux_x86_bzimage_impl(ctx):
    if not ctx.attr.config:
        fail("linux_compressed_image with x86_bzimage format requires config")
    if not ctx.attr.generated_headers:
        fail("linux_compressed_image with x86_bzimage format requires generated_headers")
    if not ctx.file.source_root:
        fail("linux_compressed_image with x86_bzimage format requires source_root")

    image = ctx.attr.image[LinuxImageInfo]
    config = ctx.attr.config[LinuxConfigInfo]
    generated_headers = ctx.attr.generated_headers[LinuxGeneratedHeadersInfo]
    cc_toolchain = find_cpp_toolchain(ctx)
    feature_configuration = _cc_feature_configuration(ctx, cc_toolchain)
    compiler = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
    )
    linker = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = CPP_LINK_EXECUTABLE_ACTION_NAME,
    )
    archiver = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = CPP_LINK_STATIC_LIBRARY_ACTION_NAME,
    )
    source_root = _linux_source_root_path(ctx)

    voffset = _linux_x86_offsets(ctx, image.output, "voffset", "arch/x86/boot/voffset.h")
    compressed_vmlinux = _linux_x86_compressed_vmlinux(
        ctx,
        compiler,
        linker,
        archiver,
        cc_toolchain,
        feature_configuration,
        config,
        generated_headers,
        source_root,
        image,
        voffset,
    )
    zoffset = _linux_x86_offsets(ctx, compressed_vmlinux, "zoffset", "arch/x86/boot/zoffset.h")
    vmlinux_bin = _linux_x86_objcopy(ctx, compressed_vmlinux, "arch/x86/boot/vmlinux.bin", ["-O", "binary", "-R", ".note", "-R", ".comment", "-S"])
    setup_bin = _linux_x86_setup_bin(ctx, compiler, linker, cc_toolchain, feature_configuration, config, generated_headers, source_root, voffset, zoffset)
    out = _linux_x86_bzimage(ctx, setup_bin, vmlinux_bin)
    info = LinuxImageInfo(
        archives = image.archives,
        module_objects = image.module_objects,
        objects = image.objects,
        output = out,
    )
    return [
        DefaultInfo(files = depset([out])),
        info,
        OutputGroupInfo(bzimage = depset([out])),
    ]

def _linux_compressed_image_impl(ctx):
    if ctx.attr.format == "x86_bzimage":
        return _linux_x86_bzimage_impl(ctx)
    if ctx.attr.format == "arm64_image":
        return _linux_objcopy_image_impl(ctx, [
            "-O",
            "binary",
            "-R",
            ".modinfo",
            "-R",
            ".note",
            "-R",
            ".note.gnu.build-id",
            "-R",
            ".comment",
            "-S",
        ])
    fail(
        "linux_compressed_image %s requires format \"x86_bzimage\" or \"arm64_image\"" %
        ctx.label,
    )

def _linux_objcopy_image_impl(ctx, objcopy_flags):
    image = ctx.attr.image[LinuxImageInfo]
    out = ctx.actions.declare_file(ctx.label.name + "." + ctx.attr.extension)
    args = ctx.actions.args()
    args.add_all(objcopy_flags)
    args.add(image.output)
    args.add(out)
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._llvm_objcopy,
        inputs = [image.output, ctx.executable._llvm_objcopy],
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxKernelObjcopyImage",
        progress_message = "Objcopying Linux kernel image %{label}",
    )
    info = LinuxImageInfo(
        archives = image.archives,
        module_objects = image.module_objects,
        objects = image.objects,
        output = out,
    )
    return [
        DefaultInfo(files = depset([out])),
        info,
        OutputGroupInfo(image = depset([out])),
    ]

linux_compressed_image = rule(
    implementation = _linux_compressed_image_impl,
    attrs = {
        "arch": attr.string(default = "x86"),
        "config": attr.label(providers = [LinuxConfigInfo]),
        "extension": attr.string(default = "image"),
        "format": attr.string(
            mandatory = True,
            values = [
                "arm64_image",
                "x86_bzimage",
            ],
        ),
        "generated_headers": attr.label(providers = [LinuxGeneratedHeadersInfo]),
        "image": attr.label(providers = [LinuxImageInfo], mandatory = True),
        "source_root": attr.label(allow_single_file = True),
        "source_tree": attr.label_list(allow_files = True),
        "srcarch": attr.string(),
        "x86_relocs_tool": attr.label(
            cfg = "exec",
            executable = True,
        ),
        "_insnattr": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/insnattr"),
            executable = True,
        ),
        "_llvm_objcopy": attr.label(
            allow_single_file = True,
            cfg = "exec",
            default = Label("@llvm//tools:llvm-objcopy"),
            executable = True,
        ),
        "_lz4": attr.label(
            cfg = "exec",
            default = Label("@lz4//programs:lz4"),
            executable = True,
        ),
        "_x86boot": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/x86boot"),
            executable = True,
        ),
    },
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Builds an x86 bzImage or arm64 Image from a linked kernel.",
)

def _collect_image_objects(image):
    objects = []
    objects.extend(image.objects)
    for archive in image.archives:
        objects.extend(archive.objects)
    return objects

def _linux_cache_shape_check_impl(ctx):
    expected = {name: False for name in ctx.attr.shared_objects}
    shared_outputs = {}
    lines = []
    for image_target in ctx.attr.images:
        image = image_target[LinuxImageInfo]
        for obj in _collect_image_objects(image):
            if obj.object not in expected:
                continue
            expected[obj.object] = True
            output = obj.output.short_path
            previous = shared_outputs.get(obj.object)
            if previous != None and previous != output:
                fail("object %s is not cache-shared: %s != %s" % (obj.object, previous, output))
            shared_outputs[obj.object] = output
            lines.append("%s %s %s" % (image_target.label, obj.object, output))

    missing = [name for name, seen in expected.items() if not seen]
    if missing:
        fail("shared object(s) not present in checked images: %s" % ", ".join(sorted(missing)))

    out = ctx.actions.declare_file(ctx.label.name + ".cache_shape.txt")
    ctx.actions.write(out, "\n".join(sorted(lines)) + "\n")
    return [DefaultInfo(files = depset([out]))]

linux_cache_shape_check = rule(
    implementation = _linux_cache_shape_check_impl,
    attrs = {
        "images": attr.label_list(providers = [LinuxImageInfo], mandatory = True),
        "shared_objects": attr.string_list(mandatory = True),
    },
    doc = "Analysis-time check that shared object variants keep the same provider output across image targets.",
)

# Narrow private helper surface shared with internal/linux_modules.bzl. Keeping
# these functions behind one struct avoids making the implementation helpers
# individual loadable symbols or part of the root public API.
linux_module_cc_helpers = struct(
    compile_flags = _linux_compile_flags,
    configure_features = _cc_feature_configuration,
    cpp_undef_flags = _linux_cpp_undef_flags,
    module_flags = _linux_module_flags,
    object_name_flags = _linux_object_name_flags,
    source_include_dirs = _linux_ordered_include_dirs,
    source_include_flags = _linux_source_include_flags_for_root,
    source_preinclude_flags = _linux_source_preinclude_flags_for_root,
    target_flags = _cc_target_flags,
)
