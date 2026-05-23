"""Native rules for compact, fragment-keyed Linux build units."""

load("@rules_cc//cc:action_names.bzl", "CPP_LINK_EXECUTABLE_ACTION_NAME", "CPP_LINK_STATIC_LIBRARY_ACTION_NAME", "C_COMPILE_ACTION_NAME")
load("@rules_cc//cc:find_cc_toolchain.bzl", "find_cpp_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load(":kconfig.bzl", "KconfigInfo")

_PERL_TOOLCHAIN = "@rules_perl//perl:toolchain_type"

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
        "kernel_release": "include/config/kernel.release output.",
        "rustc_cfg": "include/generated/rustc_cfg output.",
    },
)

LinuxObjectInfo = provider(
    doc = "Metadata for one Linux object variant keyed by a reduced Kconfig fragment.",
    fields = {
        "config_fragment": "Dictionary of CONFIG_* values that affect this object action.",
        "flags": "Kbuild flags that affect this object action.",
        "mode": "Kbuild mode: y for built-in or m for module.",
        "object": "Object path relative to the kernel source tree.",
        "output": "Object output file.",
        "real": "Whether output is a compiled object instead of placeholder metadata.",
        "generated_headers": "Depset of generated headers exported by this object.",
        "generated_include_dirs": "Include directories for generated headers exported by this object.",
        "source": "Source file short path, when this is a real compiled object.",
    },
)

LinuxArchiveInfo = provider(
    doc = "Metadata for a native Linux archive-like aggregation action.",
    fields = {
        "kind": "Archive kind, for example built-in.a or module-objects.",
        "objects": "LinuxObjectInfo values included in the archive.",
        "output": "Archive output file.",
        "real": "Whether output is a real archive instead of placeholder metadata.",
    },
)

LinuxGeneratedInfo = provider(
    doc = "Metadata for a generated Linux source/header action.",
    fields = {
        "output": "Generated placeholder output file.",
        "target": "Kbuild target path represented by the action.",
    },
)

LinuxGeneratedHeadersInfo = provider(
    doc = "Generated Linux header tree consumed by native compile actions.",
    fields = {
        "arch": "Linux ARCH value used to generate this header tree.",
        "cflags": "Optional response file with generated architecture C flags.",
        "files": "Depset of generated header files.",
        "include_dirs": "Include directories for the generated header tree.",
        "srcarch": "Linux SRCARCH value used for source include paths.",
        "vdsomunge": "Optional exec-config vdsomunge tool for arm64 compat vDSO generation.",
    },
)

LinuxSourceTreeInfo = provider(
    doc = "Shared Linux source tree inputs consumed by generated object targets.",
    fields = {
        "files": "Depset of Linux source tree files.",
        "root": "Root marker file for the Linux source tree, usually Kconfig.",
    },
)

LinuxImageInfo = provider(
    doc = "Metadata for a native Linux image/module output action.",
    fields = {
        "archives": "Archive providers consumed by this output.",
        "generated": "Generated file providers consumed by this output.",
        "objects": "Object providers consumed by this output.",
        "output": "Placeholder output file.",
    },
)

def _linux_source_tree_impl(ctx):
    return [LinuxSourceTreeInfo(
        files = depset(ctx.files.srcs),
        root = ctx.file.root,
    )]

linux_source_tree = rule(
    implementation = _linux_source_tree_impl,
    attrs = {
        "root": attr.label(
            allow_single_file = True,
            doc = "Root marker file for the Linux source tree, usually Kconfig.",
        ),
        "srcs": attr.label_list(
            allow_files = True,
            doc = "Files in the Linux source tree.",
        ),
    },
    doc = "Provider wrapper for source tree inputs shared by generated Linux object targets.",
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

def _linux_platform_transition_impl(_, attr):
    arch = getattr(attr, "arch", "")
    if arch == "arm64":
        platform = "@llvm//platforms:linux_arm64_musl"
    elif arch == "x86":
        platform = "@llvm//platforms:linux_x86_64_musl"
    else:
        fail("unsupported Linux rule arch for platform transition: %s" % arch)
    return {
        "//command_line_option:platforms": platform,
        "//command_line_option:copt": [],
        "//command_line_option:conlyopt": [],
        "//command_line_option:cxxopt": [],
        "//command_line_option:features": [],
        "//command_line_option:linkopt": [],
    }

_linux_platform_transition = transition(
    implementation = _linux_platform_transition_impl,
    inputs = [],
    outputs = [
        "//command_line_option:platforms",
        "//command_line_option:copt",
        "//command_line_option:conlyopt",
        "//command_line_option:cxxopt",
        "//command_line_option:features",
        "//command_line_option:linkopt",
    ],
)

def _linux_platform_transition_allowlist_attr():
    return attr.label(default = "@bazel_tools//tools/allowlists/function_transition_allowlist")

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
            "-Wno-builtin-macro-redefined",
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
            flag.startswith("--sysroot=") or
            flag.startswith("-D__DATE__=") or
            flag.startswith("-D__TIMESTAMP__=") or
            flag.startswith("-D__TIME__=") or
            flag.startswith("-ffile-compilation-dir=")
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
    arch = getattr(ctx.attr, "arch", "")
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
        "_allowlist_function_transition": _linux_platform_transition_allowlist_attr(),
        "arch": attr.string(default = "x86"),
        "image_format": attr.string(default = "bzImage"),
        "srcarch": attr.string(default = "x86"),
        "target_triple": attr.string(),
    },
    cfg = _linux_platform_transition,
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

def _make_config_values(config, config_fragment):
    values = dict(config_fragment)
    if config:
        values.update(config.config_flags)
    return values

def _expand_make_refs(value, replacements, object):
    for key in sorted(replacements.keys()):
        replacement = replacements[key]
        value = value.replace("$(%s)" % key, replacement)
        value = value.replace("${%s}" % key, replacement)
    if "$(" in value or "${" in value:
        fail("unexpanded Kbuild Make reference in flags for %s: %s" % (object, value))
    return value

def _expand_flag_refs(flags, config_values, make_values, object):
    replacements = dict(config_values)
    replacements.update(make_values)
    for key in [
        "CC_FLAGS_CFI",
        "CC_FLAGS_FTRACE",
        "CC_FLAGS_LTO",
        "CC_FLAGS_SCS",
        "CLANG_FLAGS",
        "DISABLE_KSTACK_ERASE",
        "DISABLE_LATENT_ENTROPY_PLUGIN",
        "DISABLE_STACKLEAK_PLUGIN",
        "RANDSTRUCT_CFLAGS",
        "cflags-nogcse-yy",
    ]:
        replacements[key] = ""
    return [_expand_make_refs(flag, replacements, object) for flag in flags]

def _rewrite_source_root_flags(flags, source_root):
    if not source_root:
        return flags
    marker = "/" + source_root
    out = []
    for flag in flags:
        index = flag.find(marker)
        if index < 0:
            out.append(flag)
            continue
        if flag.startswith("/"):
            out.append(flag[index + 1:])
            continue
        replaced = False
        for prefix in ["-I", "-iquote", "-isystem", "-include"]:
            if flag.startswith(prefix + "/"):
                out.append(prefix + flag[index + 1:])
                replaced = True
                break
        if not replaced:
            out.append(flag)
    return out

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

def _linux_source_include_flags_for_root(source_root, srcarch = "x86", generated_include_dirs = []):
    return _linux_include_flags_for_dirs(_linux_ordered_include_dirs(source_root, srcarch, generated_include_dirs))

def _linux_source_include_flags(ctx, generated_headers = None):
    generated_include_dirs = []
    if generated_headers != None:
        generated_include_dirs = generated_headers.include_dirs
    source_root = _linux_source_root_file(ctx)
    return _linux_source_include_flags_for_root(
        source_root.dirname if source_root else "",
        _linux_rule_srcarch(ctx, generated_headers),
        generated_include_dirs,
    )

def _linux_rule_srcarch(ctx, generated_headers = None):
    if hasattr(ctx.attr, "srcarch") and ctx.attr.srcarch:
        return ctx.attr.srcarch
    if generated_headers != None and hasattr(generated_headers, "srcarch") and generated_headers.srcarch:
        return generated_headers.srcarch
    return "x86"

def _linux_rule_arch(ctx, generated_headers = None):
    if hasattr(ctx.attr, "arch") and ctx.attr.arch:
        return ctx.attr.arch
    if generated_headers != None and hasattr(generated_headers, "arch") and generated_headers.arch:
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

def _single_file(target, attr_name):
    files = target.files.to_list()
    if len(files) != 1:
        fail("%s entry %s must provide exactly one file, got %d" % (attr_name, target.label, len(files)))
    return files[0]

def _linux_config_cflags(config):
    if config:
        return ["@" + config.cflags.path]
    return []

def _linux_config_aflags(config):
    if config:
        return ["@" + config.aflags.path]
    return []

def _linux_config_flags_for_source(config, src):
    if _is_assembly_source(src):
        return _linux_config_aflags(config)
    return _linux_config_cflags(config)

def _linux_generated_header_cflags(generated_headers):
    if generated_headers != None and generated_headers.cflags != None:
        return ["@" + generated_headers.cflags.path]
    return []

def _linux_filtered_config_flags_for_source(ctx, config, src, remove_flags):
    if not config:
        return struct(flags = [], inputs = [])
    base = config.aflags if _is_assembly_source(src) else config.cflags
    if not remove_flags:
        return struct(flags = ["@" + base.path], inputs = [])

    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + base.basename + ".filtered.rsp")
    args = ctx.actions.args()
    args.add("-in", base)
    args.add("-out", out)
    args.add_all(remove_flags, before_each = "-remove")
    ctx.actions.run(
        executable = ctx.executable._flagfilter,
        inputs = [base],
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxFlagFilter",
        progress_message = "Filtering Linux compiler flags %{label}",
    )
    return struct(flags = ["@" + out.path], inputs = [out])

def _flags_need_obj_dir(flags):
    for flag in flags:
        if "$(obj)" in flag or "${obj}" in flag:
            return True
    return False

def _flags_need_utsversion_tmp(flags):
    for flag in flags:
        if "utsversion-tmp.h" in flag:
            return True
    return False

def _linux_object_directory(object):
    if "/" not in object:
        return ""
    return object.rsplit("/", 1)[0]

def _linux_source_tree_info(ctx):
    if hasattr(ctx.attr, "source_tree_info") and ctx.attr.source_tree_info:
        return ctx.attr.source_tree_info[LinuxSourceTreeInfo]
    return None

def _linux_source_root_file(ctx):
    info = _linux_source_tree_info(ctx)
    if info and info.root:
        return info.root
    if hasattr(ctx.file, "source_root"):
        return ctx.file.source_root
    fail("%s requires source_tree_info for source-backed Linux actions" % ctx.label)

def _linux_source_tree_files(ctx):
    files = []
    info = _linux_source_tree_info(ctx)
    if info:
        files.extend(info.files.to_list())
    if hasattr(ctx.files, "source_tree"):
        files.extend(ctx.files.source_tree)
    return files

def _linux_source_tree_inputs(ctx, direct = []):
    root = _linux_source_root_file(ctx)
    inputs = list(direct)
    if root:
        inputs.append(root)
    inputs.extend(_linux_source_tree_files(ctx))
    return inputs

def _rewrite_utsversion_tmp_flags(flags, object, utsversion_tmp):
    object_dir = _linux_object_directory(object)
    candidates = ["utsversion-tmp.h"]
    if object_dir:
        candidates.append(object_dir + "/utsversion-tmp.h")
    rewritten = []
    for flag in flags:
        if flag in candidates:
            rewritten.append(utsversion_tmp.path)
        else:
            rewritten.append(flag)
    return rewritten

def _source_tree_file(ctx, relpath):
    suffix = "/" + relpath
    for file in _linux_source_tree_files(ctx):
        if file.short_path == relpath or file.short_path.endswith(suffix):
            return file
    fail("source tree input %s required by %s was not found" % (relpath, ctx.label))

def _source_tree_file_for_root(ctx, source_root, relpath):
    path = source_root + "/" + relpath
    for file in _linux_source_tree_files(ctx):
        if file.path == path or file.short_path == path:
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
    args.add_all(_linux_config_flags_for_source(config, src))
    args.add_all([
        "-mcmodel=small",
        "-fno-stack-protector",
        "-fpic",
        "-fvisibility=hidden",
    ])
    args.add_all(_linux_source_preinclude_flags_for_root(source_root, _is_assembly_source(src)))
    if config:
        args.add("-I" + config.include_dir)
    args.add("-I" + src.dirname)
    args.add_all(_linux_source_include_flags(ctx, generated_headers))
    args.add("-c")
    args.add(src)
    args.add("-o")
    args.add(out)

    direct_inputs = _linux_source_tree_inputs(ctx, direct = [src])
    transitive_inputs = [cc_toolchain.all_files]
    if config:
        transitive_inputs.append(config.files)
    if generated_headers:
        transitive_inputs.append(generated_headers.files)
    ctx.actions.run(
        executable = compiler,
        inputs = depset(direct_inputs, transitive = transitive_inputs),
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
    ctx.actions.run(
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
        args.add("-I" + config.include_dir)
    args.add("-I" + src.dirname)
    args.add("-I" + source_root + "/arch/x86/realmode/rm")
    args.add("-I" + source_root + "/arch/x86/boot")
    args.add_all(_linux_source_include_flags(ctx, generated_headers))
    args.add("-c")
    args.add(src)
    args.add("-o")
    args.add(out)

    direct_inputs = _linux_source_tree_inputs(ctx, direct = [src])
    transitive_inputs = [cc_toolchain.all_files]
    if config:
        transitive_inputs.append(config.files)
    if generated_headers:
        transitive_inputs.append(generated_headers.files)
    ctx.actions.run(
        executable = compiler,
        inputs = depset(direct_inputs, transitive = transitive_inputs),
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
    ctx.actions.run(
        executable = ctx.executable._pasyms,
        inputs = objects + [ctx.executable._llvm_nm],
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
        args.add("-I" + config.include_dir)
    args.add("-I" + pasyms.dirname)
    args.add_all(_linux_source_include_flags(ctx, generated_headers))
    args.add(src)
    args.add("-o")
    args.add(out)

    transitive_inputs = [cc_toolchain.all_files]
    if config:
        transitive_inputs.append(config.files)
    if generated_headers:
        transitive_inputs.append(generated_headers.files)
    ctx.actions.run(
        executable = compiler,
        inputs = depset(_linux_source_tree_inputs(ctx, direct = [src, pasyms]), transitive = transitive_inputs),
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
    ctx.actions.run(
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
    ctx.actions.run(
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
    ctx.actions.run(
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
        args.add("-I" + config.include_dir)
    args.add("-I" + source_root + "/arch/x86/entry/vdso")
    args.add_all(_linux_source_include_flags(ctx, generated_headers))
    args.add("-c")
    args.add(src)
    args.add("-o")
    args.add(out)

    direct_inputs = _linux_source_tree_inputs(ctx, direct = [src])
    transitive_inputs = [cc_toolchain.all_files]
    if config:
        transitive_inputs.append(config.files)
    if generated_headers:
        transitive_inputs.append(generated_headers.files)
    ctx.actions.run(
        executable = compiler,
        inputs = depset(direct_inputs, transitive = transitive_inputs),
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
    args.add_all(_linux_config_cflags(config))
    args.add_all(_linux_generated_header_cflags(generated_headers))
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
        args.add("-I" + config.include_dir)
    args.add("-I" + source_root + "/arch/x86/entry/vdso")
    args.add_all(_linux_source_include_flags(ctx, generated_headers))
    args.add(src)
    args.add("-o")
    args.add(out)

    transitive_inputs = [cc_toolchain.all_files]
    if config:
        transitive_inputs.append(config.files)
    if generated_headers:
        transitive_inputs.append(generated_headers.files)
    ctx.actions.run(
        executable = compiler,
        inputs = depset(_linux_source_tree_inputs(ctx, direct = [src]), transitive = transitive_inputs),
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
    args.add("-Wl,-T," + linker_script.path)
    args.add("-o")
    args.add(out)
    args.add_all(objects)
    ctx.actions.run(
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
    ctx.actions.run(
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
    vdso2c_args.add("-out", out)
    ctx.actions.run(
        executable = ctx.executable._vdso2c,
        inputs = [dbg, stripped],
        outputs = [out],
        arguments = [vdso2c_args],
        mnemonic = "LinuxVDSO2C",
        progress_message = "Generating Linux x86 vDSO image source %{label}",
    )
    return out

def _linux_object_generated_inputs(ctx, compiler, linker, cc_toolchain, feature_configuration, config, generated_headers, source_root):
    files = []
    include_dirs = []
    assembler_include_roots = []

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

    if ctx.attr.object == "drivers/scsi/scsi_sysfs.o":
        header = _source_tree_file(ctx, "include/scsi/scsi_devinfo.h")
        out = ctx.actions.declare_file(ctx.label.name + ".obj/drivers/scsi/scsi_devinfo_tbl.c")
        args = ctx.actions.args()
        args.add("-in", header)
        args.add("-out", out)
        ctx.actions.run(
            executable = ctx.executable._scsidevinfo,
            inputs = [header],
            outputs = [out],
            arguments = [args],
            mnemonic = "LinuxSCSIDevinfo",
            progress_message = "Generating Linux SCSI devinfo table %{label}",
        )
        files.append(out)
        include_dirs.append(out.dirname)

    if ctx.attr.object == "lib/crc/crc32-main.o":
        out = ctx.actions.declare_file(ctx.label.name + ".obj/lib/crc/crc32table.h")
        args = ctx.actions.args()
        args.add("-kind", "crc32")
        args.add("-out", out)
        ctx.actions.run(
            executable = ctx.executable._crctables,
            outputs = [out],
            arguments = [args],
            mnemonic = "LinuxCRCTables",
            progress_message = "Generating Linux CRC32 table %{label}",
        )
        files.append(out)
        include_dirs.append(out.dirname)

    if ctx.attr.object == "lib/crc/crc64-main.o":
        out = ctx.actions.declare_file(ctx.label.name + ".obj/lib/crc/crc64table.h")
        args = ctx.actions.args()
        args.add("-kind", "crc64")
        args.add("-out", out)
        ctx.actions.run(
            executable = ctx.executable._crctables,
            outputs = [out],
            arguments = [args],
            mnemonic = "LinuxCRCTables",
            progress_message = "Generating Linux CRC64 table %{label}",
        )
        files.append(out)
        include_dirs.append(out.dirname)

    if ctx.attr.object == "lib/oid_registry.o":
        header = _source_tree_file(ctx, "include/linux/oid_registry.h")
        out = ctx.actions.declare_file(ctx.label.name + ".obj/lib/oid_registry_data.c")
        args = ctx.actions.args()
        args.add("-in", header)
        args.add("-out", out)
        ctx.actions.run(
            executable = ctx.executable._oidregistry,
            inputs = [header],
            outputs = [out],
            arguments = [args],
            mnemonic = "LinuxOIDRegistry",
            progress_message = "Generating Linux OID registry data %{label}",
        )
        files.append(out)
        include_dirs.append(out.dirname)

    if ctx.attr.object == "arch/x86/lib/inat.o":
        opcode_map = _source_tree_file(ctx, "arch/x86/lib/x86-opcode-map.txt")
        out = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/lib/inat-tables.c")
        args = ctx.actions.args()
        args.add("-in", opcode_map)
        args.add("-out", out)
        ctx.actions.run(
            executable = ctx.executable._insnattr,
            inputs = [opcode_map],
            outputs = [out],
            arguments = [args],
            mnemonic = "LinuxInsnAttr",
            progress_message = "Generating Linux x86 instruction attribute tables %{label}",
        )
        files.append(out)
        include_dirs.append(out.dirname)

    if ctx.attr.object == "certs/system_certificates.o":
        signing_key = ctx.actions.declare_file(ctx.label.name + ".obj/certs/signing_key.x509")
        cert_list = ctx.actions.declare_file(ctx.label.name + ".obj/certs/x509_certificate_list")
        if config:
            args = ctx.actions.args()
            args.add("-config", config.config)
            args.add("-signing_key_out", signing_key)
            args.add("-cert_list_out", cert_list)
            ctx.actions.run(
                executable = ctx.executable._certdata,
                inputs = depset([], transitive = [config.files]),
                outputs = [signing_key, cert_list],
                arguments = [args],
                mnemonic = "LinuxCertData",
                progress_message = "Generating Linux certificate data %{label}",
            )
        else:
            ctx.actions.write(signing_key, "")
            ctx.actions.write(cert_list, "")
        files.extend([signing_key, cert_list])
        assembler_include_roots.append(signing_key.dirname[:-len("/certs")])

    if ctx.attr.object == "usr/initramfs_data.o":
        initramfs_list = _source_tree_file(ctx, "usr/default_cpio_list")
        out = ctx.actions.declare_file(ctx.label.name + ".obj/usr/initramfs_inc_data")
        args = ctx.actions.args()
        args.add("-in", initramfs_list)
        args.add("-out", out)
        ctx.actions.run(
            executable = ctx.executable._initramfsdata,
            inputs = [initramfs_list],
            outputs = [out],
            arguments = [args],
            mnemonic = "LinuxInitramfsData",
            progress_message = "Generating Linux initramfs data %{label}",
        )
        files.append(out)
        assembler_include_roots.append(out.dirname[:-len("/usr")])

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

    if ctx.attr.object == "arch/arm64/kernel/vdso-wrap.o" and generated_headers:
        vdso = _linux_generated_header(generated_headers, "arch/arm64/kernel/vdso/vdso.so")
        files.append(vdso)
        assembler_include_roots.append(vdso.dirname[:-len("/arch/arm64/kernel/vdso")])

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
            generated_headers.files.to_list(),
            ctx.label.name + ".obj",
            generated_headers.vdsomunge,
        ).so
        files.append(vdso32)
        assembler_include_roots.append(vdso32.dirname[:-len("/arch/arm64/kernel/vdso32")])

    return struct(
        assembler_include_roots = assembler_include_roots,
        files = files,
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
    ctx.actions.run(
        executable = ctx.executable._emptyrootdtb,
        inputs = [src],
        outputs = [dtb],
        arguments = [args],
        mnemonic = "LinuxEmptyRootDtb",
        progress_message = "Generating Linux empty root devicetree blob %{label}",
    )

    symbol = _linux_dtb_symbol_base(src)
    ctx.actions.write(
        output = wrapper,
        content = "\n".join([
            "#include <asm-generic/vmlinux.lds.h>",
            ".section %s,\"a\"" % _linux_dtb_section(object, srcarch),
            ".balign STRUCT_ALIGNMENT",
            ".global %s_begin" % symbol,
            "%s_begin:" % symbol,
            ".incbin \"%s\"" % dtb.path,
            ".global %s_end" % symbol,
            "%s_end:" % symbol,
            ".balign STRUCT_ALIGNMENT",
            "",
        ]),
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

def _linux_offsets_header(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, src, out_path, guard, generated_inputs, srcarch = "x86", extra_include_dirs = [], extra_flags = []):
    compiler = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
    )
    asm = ctx.actions.declare_file(out_path + ".s")
    out = ctx.actions.declare_file(out_path)

    compile_args = ctx.actions.args()
    compile_args.add_all(_linux_compile_flags(ctx, cc_toolchain, feature_configuration))
    compile_args.add_all(_linux_config_cflags(config))
    compile_args.add_all(_linux_source_preinclude_flags_for_root(source_root))
    compile_args.add("-I" + config.include_dir)
    compile_args.add_all(_linux_source_include_flags_for_root(source_root, srcarch, include_dirs))
    for include_dir in extra_include_dirs:
        compile_args.add("-I" + include_dir)
    compile_args.add_all(extra_flags)
    compile_args.add("-S")
    compile_args.add(src)
    compile_args.add("-o")
    compile_args.add(asm)

    direct_inputs = _linux_source_tree_inputs(ctx, direct = [src] + generated_inputs)
    ctx.actions.run(
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
    ctx.actions.run(
        executable = ctx.executable._offsetsheader,
        inputs = [asm],
        outputs = [out],
        arguments = [header_args],
        mnemonic = "LinuxOffsetsHeader",
        progress_message = "Generating Linux offsets header %{label}",
    )
    return out

def _linux_arm64_vdso_compile(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, generated_inputs, src, out_relpath, extra_flags = []):
    compiler = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
    )
    out = ctx.actions.declare_file(out_relpath)
    assembly = _is_assembly_source(src)
    args = ctx.actions.args()
    args.add_all(_linux_compile_flags(ctx, cc_toolchain, feature_configuration))
    args.add_all(_linux_config_flags_for_source(config, src))
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
    args.add("-I" + config.include_dir)
    args.add_all(_linux_source_include_flags_for_root(source_root, "arm64", include_dirs))
    args.add("-I" + source_root + "/arch/arm64/kernel/vdso")
    args.add("-I" + source_root + "/lib/vdso")
    args.add_all(extra_flags)
    args.add("-c")
    args.add(src)
    args.add("-o")
    args.add(out)
    ctx.actions.run(
        executable = compiler,
        inputs = depset(
            _linux_source_tree_inputs(ctx, direct = [src] + generated_inputs),
            transitive = [cc_toolchain.all_files, config.files],
        ),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxARM64VDSOCompile",
        progress_message = "Compiling Linux arm64 vDSO object %{label}",
    )
    return out

def _linux_arm64_vdso_linker_script(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, generated_inputs, out_relpath):
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
    args.add("-I" + config.include_dir)
    args.add_all(_linux_source_include_flags_for_root(source_root, "arm64", include_dirs))
    args.add(src)
    args.add("-o")
    args.add(out)
    ctx.actions.run(
        executable = compiler,
        inputs = depset(
            _linux_source_tree_inputs(ctx, direct = [src] + generated_inputs),
            transitive = [cc_toolchain.all_files, config.files],
        ),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxARM64VDSOLinkerScript",
        progress_message = "Preprocessing Linux arm64 vDSO linker script %{label}",
    )
    return out

def _linux_arm64_vdso_outputs(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, generated_inputs, base):
    objects = [
        _linux_arm64_vdso_compile(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, generated_inputs, _source_tree_file(ctx, "arch/arm64/kernel/vdso/vgettimeofday.c"), base + "/arch/arm64/kernel/vdso/vgettimeofday.o", extra_flags = ["-include", source_root + "/lib/vdso/gettimeofday.c"]),
        _linux_arm64_vdso_compile(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, generated_inputs, _source_tree_file(ctx, "arch/arm64/kernel/vdso/note.S"), base + "/arch/arm64/kernel/vdso/note.o"),
        _linux_arm64_vdso_compile(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, generated_inputs, _source_tree_file(ctx, "arch/arm64/kernel/vdso/sigreturn.S"), base + "/arch/arm64/kernel/vdso/sigreturn.o"),
        _linux_arm64_vdso_compile(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, generated_inputs, _source_tree_file(ctx, "arch/arm64/kernel/vdso/vgetrandom.c"), base + "/arch/arm64/kernel/vdso/vgetrandom.o", extra_flags = ["-include", source_root + "/lib/vdso/getrandom.c"]),
        _linux_arm64_vdso_compile(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, generated_inputs, _source_tree_file(ctx, "arch/arm64/kernel/vdso/vgetrandom-chacha.S"), base + "/arch/arm64/kernel/vdso/vgetrandom-chacha.o"),
    ]
    linker_script = _linux_arm64_vdso_linker_script(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, generated_inputs, base + "/arch/arm64/kernel/vdso/vdso.lds")
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
        "-T",
        linker_script.path,
    ])
    link_args.add_all(objects)
    link_args.add_all([
        "-o",
        dbg,
    ])
    ctx.actions.run(
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
    ctx.actions.run(
        executable = ctx.executable._llvm_objcopy,
        inputs = [dbg, ctx.executable._llvm_objcopy],
        outputs = [so],
        arguments = [objcopy_args],
        mnemonic = "LinuxARM64VDSOObjcopy",
        progress_message = "Stripping Linux arm64 vDSO %{label}",
    )

    nm = ctx.actions.declare_file(base + "/arch/arm64/kernel/vdso/vdso.so.dbg.nm")
    ctx.actions.run_shell(
        inputs = [dbg, ctx.executable._llvm_nm],
        outputs = [nm],
        command = "\"$1\" \"$2\" > \"$3\"",
        arguments = [ctx.executable._llvm_nm.path, dbg.path, nm.path],
        mnemonic = "LinuxARM64VDSONM",
        progress_message = "Generating Linux arm64 vDSO symbols %{label}",
    )

    offsets = ctx.actions.declare_file(base + "/include/generated/vdso-offsets.h")
    offsets_args = ctx.actions.args()
    offsets_args.add("-vdso_nm", nm)
    offsets_args.add("-vdso_offsets_out", offsets)
    ctx.actions.run(
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

def _linux_arm64_vdso32_common_flags(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs):
    flags = []
    flags.extend(_linux_compile_flags_without_target(ctx, cc_toolchain, feature_configuration))
    flags.extend([
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
    flags.extend(_linux_source_preinclude_flags_for_root(source_root, False))
    flags.append("-I" + config.include_dir)
    flags.extend(_linux_source_include_flags_for_root(source_root, "arm64", include_dirs))
    flags.extend([
        "-I" + source_root + "/arch/arm64/kernel/vdso32",
        "-I" + source_root + "/lib/vdso",
    ])
    return flags

def _linux_arm64_vdso32_compile(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, generated_inputs, src, out_relpath, extra_flags = []):
    compiler = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
    )
    out = ctx.actions.declare_file(out_relpath)
    args = ctx.actions.args()
    args.add_all(_linux_arm64_vdso32_common_flags(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs))
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
    ctx.actions.run(
        executable = compiler,
        inputs = depset(
            _linux_source_tree_inputs(ctx, direct = [src] + generated_inputs),
            transitive = [cc_toolchain.all_files, config.files],
        ),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxARM64VDSO32Compile",
        progress_message = "Compiling Linux arm64 compat vDSO object %{label}",
    )
    return out

def _linux_arm64_vdso32_linker_script(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, generated_inputs, out_relpath):
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
    args.add("-I" + config.include_dir)
    args.add_all(_linux_source_include_flags_for_root(source_root, "arm64", include_dirs))
    args.add("-I" + source_root + "/arch/arm64/kernel/vdso32")
    args.add("-I" + source_root + "/lib/vdso")
    args.add(src)
    args.add("-o")
    args.add(out)
    ctx.actions.run(
        executable = compiler,
        inputs = depset(
            _linux_source_tree_inputs(ctx, direct = [src] + generated_inputs),
            transitive = [cc_toolchain.all_files, config.files],
        ),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxARM64VDSO32LinkerScript",
        progress_message = "Preprocessing Linux arm64 compat vDSO linker script %{label}",
    )
    return out

def _linux_arm64_vdso32_outputs(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, generated_inputs, base, vdsomunge):
    objects = [
        _linux_arm64_vdso32_compile(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, generated_inputs, _source_tree_file(ctx, "arch/arm64/kernel/vdso32/note.c"), base + "/arch/arm64/kernel/vdso32/note.o"),
        _linux_arm64_vdso32_compile(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, generated_inputs, _source_tree_file(ctx, "arch/arm64/kernel/vdso32/vgettimeofday.c"), base + "/arch/arm64/kernel/vdso32/vgettimeofday.o", extra_flags = ["-include", source_root + "/lib/vdso/gettimeofday.c"]),
    ]
    linker_script = _linux_arm64_vdso32_linker_script(ctx, cc_toolchain, feature_configuration, config, source_root, include_dirs, generated_inputs, base + "/arch/arm64/kernel/vdso32/vdso.lds")
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
        "-Wl,-T," + linker_script.path,
        "-o",
        raw,
    ])
    link_args.add_all(objects)
    ctx.actions.run(
        executable = linker,
        inputs = depset(objects + [linker_script], transitive = [cc_toolchain.all_files]),
        outputs = [raw],
        arguments = [link_args],
        mnemonic = "LinuxARM64VDSO32Link",
        progress_message = "Linking Linux arm64 compat vDSO %{label}",
    )

    dbg = ctx.actions.declare_file(base + "/arch/arm64/kernel/vdso32/vdso32.so.dbg")
    ctx.actions.run(
        executable = vdsomunge,
        inputs = [raw],
        outputs = [dbg],
        arguments = [raw.path, dbg.path],
        mnemonic = "LinuxARM64VDSO32Munge",
        progress_message = "Normalizing Linux arm64 compat vDSO ABI flags %{label}",
    )

    so = ctx.actions.declare_file(base + "/arch/arm64/kernel/vdso32/vdso.so")
    objcopy_args = ctx.actions.args()
    objcopy_args.add("-S")
    objcopy_args.add(dbg)
    objcopy_args.add(so)
    ctx.actions.run(
        executable = ctx.executable._llvm_objcopy,
        inputs = [dbg, ctx.executable._llvm_objcopy],
        outputs = [so],
        arguments = [objcopy_args],
        mnemonic = "LinuxARM64VDSO32Objcopy",
        progress_message = "Stripping Linux arm64 compat vDSO %{label}",
    )
    return struct(so = so)

def _linux_x86_generated_headers_impl(ctx):
    base = ctx.label.name + ".headers"
    cc_toolchain = find_cpp_toolchain(ctx)
    feature_configuration = _cc_feature_configuration(ctx, cc_toolchain)
    config = ctx.attr.config[LinuxConfigInfo]
    source_root = ctx.file.source_root.dirname
    headers = []
    for header in _X86_ASM_GENERIC_WRAPPERS:
        out = ctx.actions.declare_file(base + "/arch/x86/include/generated/asm/" + header)
        ctx.actions.write(
            output = out,
            content = "#include <asm-generic/%s>\n" % header,
        )
        headers.append(out)
    for header in _X86_UAPI_ASM_GENERIC_WRAPPERS:
        out = ctx.actions.declare_file(base + "/arch/x86/include/generated/uapi/asm/" + header)
        ctx.actions.write(
            output = out,
            content = "#include <asm-generic/%s>\n" % header,
        )
        headers.append(out)

    timeconst = ctx.actions.declare_file(base + "/include/generated/timeconst.h")
    timeconst_args = ctx.actions.args()
    timeconst_args.add("-config", config.config)
    timeconst_args.add("-out", timeconst)
    ctx.actions.run(
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
    version_args.add("-compile_out", compile_h)
    version_args.add("-linux_version_out", linux_version_h)
    version_args.add("-utsrelease_out", utsrelease_h)
    version_args.add("-utsversion_out", utsversion_h)
    version_args.add("-machine", "x86_64")
    version_args.add("-compiler", _linux_compiler_version_string())
    ctx.actions.run(
        executable = ctx.executable._versionheaders,
        inputs = depset([], transitive = [config.files]),
        outputs = [compile_h, linux_version_h, utsrelease_h, utsversion_h],
        arguments = [version_args],
        mnemonic = "LinuxVersionHeaders",
        progress_message = "Generating Linux version headers %{label}",
    )
    headers.extend([compile_h, linux_version_h, utsrelease_h, utsversion_h])

    syscall_specs = [
        (ctx.file.syscall_32_tbl, base + "/arch/x86/include/generated/uapi/asm/unistd_32.h", "i386", True, "", ""),
        (ctx.file.syscall_64_tbl, base + "/arch/x86/include/generated/uapi/asm/unistd_64.h", "common,64", True, "", ""),
        (ctx.file.syscall_64_tbl, base + "/arch/x86/include/generated/uapi/asm/unistd_x32.h", "common,x32", True, "__X32_SYSCALL_BIT", ""),
        (ctx.file.syscall_32_tbl, base + "/arch/x86/include/generated/asm/unistd_32_ia32.h", "i386", True, "", "ia32_"),
        (ctx.file.syscall_64_tbl, base + "/arch/x86/include/generated/asm/unistd_64_x32.h", "x32", True, "", "x32_"),
    ]
    uapi_include_dir = None
    for table, path, abis, emit_nr, offset, prefix in syscall_specs:
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
        ctx.actions.run(
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
        ctx.actions.run(
            executable = ctx.executable._syscalltbl,
            inputs = [table],
            outputs = [out],
            arguments = [args],
            mnemonic = "LinuxSyscallTableHeader",
            progress_message = "Generating Linux syscall table header %{label}",
        )
        headers.append(out)

    cpufeaturemasks = ctx.actions.declare_file(base + "/arch/x86/include/generated/asm/cpufeaturemasks.h")
    cpufeature_args = ctx.actions.args()
    cpufeature_args.add("-cpufeatures", ctx.file.cpufeatures_h)
    cpufeature_args.add("-config", config.config)
    cpufeature_args.add("-out", cpufeaturemasks)
    ctx.actions.run(
        executable = ctx.executable._cpufeaturemasks,
        inputs = depset([ctx.file.cpufeatures_h], transitive = [config.files]),
        outputs = [cpufeaturemasks],
        arguments = [cpufeature_args],
        mnemonic = "LinuxCPUFeatureMasks",
        progress_message = "Generating Linux x86 CPU feature masks %{label}",
    )
    headers.append(cpufeaturemasks)

    xen_hypercalls = ctx.actions.declare_file(base + "/arch/x86/include/generated/asm/xen-hypercalls.h")
    xen_args = ctx.actions.args()
    for header in ctx.files.xen_interface_headers:
        xen_args.add("-in", header)
    xen_args.add("-out", xen_hypercalls)
    ctx.actions.run(
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
    ctx.actions.run(
        executable = ctx.executable._orchash,
        inputs = [ctx.file.orc_types_h],
        outputs = [orc_hash],
        arguments = [orc_hash_args],
        mnemonic = "LinuxORCHash",
        progress_message = "Generating Linux ORC hash header %{label}",
    )
    headers.append(orc_hash)

    include_dirs = [
        timeconst.dirname[:-len("/generated")],
        linux_version_h.dirname[:-len("/linux")],
        headers[0].dirname[:-len("/asm")],
        uapi_include_dir,
    ]
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
    )
    headers.append(asm_offsets_h)
    rq_offsets_h = _linux_offsets_header(
        ctx,
        cc_toolchain,
        feature_configuration,
        config,
        source_root,
        include_dirs,
        ctx.file.rq_offsets_c,
        base + "/include/generated/rq-offsets.h",
        "__RQ_OFFSETS_H__",
        headers,
    )
    headers.append(rq_offsets_h)
    kvm_asm_offsets_h = _linux_offsets_header(
        ctx,
        cc_toolchain,
        feature_configuration,
        config,
        source_root,
        include_dirs,
        ctx.file.kvm_asm_offsets_c,
        base + "/arch/x86/kvm/kvm-asm-offsets.h",
        "__KVM_ASM_OFFSETS_H__",
        headers,
        extra_include_dirs = [source_root + "/arch/x86/kvm"],
    )
    headers.append(kvm_asm_offsets_h)

    files = depset(headers)
    return [
        DefaultInfo(files = files),
        LinuxGeneratedHeadersInfo(
            arch = "x86",
            cflags = None,
            files = files,
            include_dirs = include_dirs + [kvm_asm_offsets_h.dirname],
            srcarch = "x86",
            vdsomunge = None,
        ),
    ]

linux_x86_generated_headers = rule(
    implementation = _linux_x86_generated_headers_impl,
    attrs = {
        "_allowlist_function_transition": _linux_platform_transition_allowlist_attr(),
        "arch": attr.string(default = "x86"),
        "asm_offsets_c": attr.label(allow_single_file = True, mandatory = True),
        "bounds_c": attr.label(allow_single_file = True, mandatory = True),
        "config": attr.label(providers = [LinuxConfigInfo], mandatory = True),
        "cpufeatures_h": attr.label(allow_single_file = True, mandatory = True),
        "kvm_asm_offsets_c": attr.label(allow_single_file = True, mandatory = True),
        "orc_types_h": attr.label(allow_single_file = True, mandatory = True),
        "rq_offsets_c": attr.label(allow_single_file = True, mandatory = True),
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
    cfg = _linux_platform_transition,
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
    base = ctx.label.name + ".headers"
    cc_toolchain = find_cpp_toolchain(ctx)
    feature_configuration = _cc_feature_configuration(ctx, cc_toolchain)
    config = ctx.attr.config[LinuxConfigInfo]
    source_root = ctx.file.source_root.dirname
    headers = []

    for header in _ARM64_ASM_GENERIC_WRAPPERS:
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
    ctx.actions.run(
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
    ctx.actions.run(
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
    version_args.add("-compile_out", compile_h)
    version_args.add("-linux_version_out", linux_version_h)
    version_args.add("-utsrelease_out", utsrelease_h)
    version_args.add("-utsversion_out", utsversion_h)
    version_args.add("-machine", "aarch64")
    version_args.add("-compiler", _linux_compiler_version_string())
    ctx.actions.run(
        executable = ctx.executable._versionheaders,
        inputs = depset([], transitive = [config.files]),
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
            ctx.actions.run(
                executable = ctx.executable._syscalltbl,
                inputs = [table],
                outputs = [out],
                arguments = [args],
                mnemonic = "LinuxSyscallTableHeader",
                progress_message = "Generating Linux syscall table header %{label}",
            )
        else:
            ctx.actions.run(
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
        srcarch = "arm64",
    )
    headers.append(asm_offsets_h)
    generated_cflags = ctx.actions.declare_file(base + "/include/generated/bazel_arm64_cflags.rsp")
    stackprotector_args = ctx.actions.args()
    stackprotector_args.add("-asm_offsets", asm_offsets_h)
    stackprotector_args.add("-stackprotector_config", config.config)
    stackprotector_args.add("-stackprotector_out", generated_cflags)
    ctx.actions.run(
        executable = ctx.executable._arm64headers,
        inputs = depset([asm_offsets_h], transitive = [config.files]),
        outputs = [generated_cflags],
        arguments = [stackprotector_args],
        mnemonic = "LinuxARM64StackProtectorFlags",
        progress_message = "Generating Linux arm64 stack protector flags %{label}",
    )
    headers.append(generated_cflags)
    rq_offsets_h = _linux_offsets_header(
        ctx,
        cc_toolchain,
        feature_configuration,
        config,
        source_root,
        include_dirs,
        ctx.file.rq_offsets_c,
        base + "/include/generated/rq-offsets.h",
        "__RQ_OFFSETS_H__",
        headers,
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
        srcarch = "arm64",
        extra_include_dirs = [source_root + "/arch/arm64/kvm/hyp/include"],
    )
    headers.append(hyp_constants_h)
    include_dirs.append(hyp_constants_h.dirname)

    vdso = _linux_arm64_vdso_outputs(
        ctx,
        cc_toolchain,
        feature_configuration,
        config,
        source_root,
        include_dirs,
        headers,
        base,
    )
    headers.extend([vdso.offsets, vdso.so])

    files = depset(headers)
    return [
        DefaultInfo(files = files),
        LinuxGeneratedHeadersInfo(
            arch = "arm64",
            cflags = generated_cflags,
            files = files,
            include_dirs = include_dirs,
            srcarch = "arm64",
            vdsomunge = ctx.executable.vdsomunge,
        ),
    ]

linux_arm64_generated_headers = rule(
    implementation = _linux_arm64_generated_headers_impl,
    attrs = {
        "_allowlist_function_transition": _linux_platform_transition_allowlist_attr(),
        "arch": attr.string(default = "arm64"),
        "asm_offsets_c": attr.label(allow_single_file = True, mandatory = True),
        "bounds_c": attr.label(allow_single_file = True, mandatory = True),
        "config": attr.label(providers = [LinuxConfigInfo], mandatory = True),
        "cpucaps": attr.label(allow_single_file = True, mandatory = True),
        "hyp_constants_c": attr.label(allow_single_file = True, mandatory = True),
        "rq_offsets_c": attr.label(allow_single_file = True, mandatory = True),
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
        "_offsetsheader": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/offsetsheader"),
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
    cfg = _linux_platform_transition,
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Generates the arm64 header subset normally produced before compiling Linux C objects.",
)

def _linux_config_impl(ctx):
    config_dir = ctx.label.name + ".config_tree"
    config = ctx.actions.declare_file(config_dir + "/.config")
    auto_conf = ctx.actions.declare_file(config_dir + "/include/config/auto.conf")
    auto_conf_cmd = ctx.actions.declare_file(config_dir + "/include/config/auto.conf.cmd")
    autoconf_h = ctx.actions.declare_file(config_dir + "/include/generated/autoconf.h")
    rustc_cfg = ctx.actions.declare_file(config_dir + "/include/generated/rustc_cfg")
    kernel_release = ctx.actions.declare_file(config_dir + "/include/config/kernel.release")
    aflags = ctx.actions.declare_file(config_dir + "/include/generated/bazel_kbuild_aflags.rsp")
    cflags = ctx.actions.declare_file(config_dir + "/include/generated/bazel_kbuild_cflags.rsp")

    flags = _config_flags(ctx)
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
        if value == "y":
            rustc_lines.append("--cfg=%s" % key)
        elif value == "m":
            rustc_lines.append("--cfg=%s_MODULE" % key)
    header_lines.append("#endif")

    localversion = _unquote(flags.get("CONFIG_LOCALVERSION", ""))
    kernel_release_value = ctx.attr.version + localversion

    ctx.actions.write(config, "\n".join(config_lines) + "\n")
    ctx.actions.write(auto_conf, "\n".join(config_lines) + "\n")
    ctx.actions.write(auto_conf_cmd, "cmd_%s := bazel linux_config\n" % auto_conf.short_path)
    ctx.actions.write(autoconf_h, "\n".join(header_lines) + "\n")
    ctx.actions.write(rustc_cfg, "\n".join(rustc_lines) + "\n")
    ctx.actions.write(kernel_release, kernel_release_value + "\n")
    ctx.actions.write(aflags, "")
    ctx.actions.write(cflags, "")

    files = depset([config, auto_conf, auto_conf_cmd, autoconf_h, rustc_cfg, kernel_release, aflags, cflags])
    include_dir = autoconf_h.dirname
    if include_dir.endswith("/generated"):
        include_dir = include_dir[:-len("/generated")]
    return [
        DefaultInfo(files = files),
        LinuxConfigInfo(
            aflags = aflags,
            auto_conf = auto_conf,
            auto_conf_cmd = auto_conf_cmd,
            autoconf_h = autoconf_h,
            config = config,
            config_flags = flags,
            cflags = cflags,
            files = files,
            include_dir = include_dir,
            kernel_release = kernel_release,
            rustc_cfg = rustc_cfg,
        ),
    ]

linux_config = rule(
    implementation = _linux_config_impl,
    attrs = {
        "config": attr.label(providers = [KconfigInfo]),
        "config_flags": attr.string_dict(),
        "version": attr.string(default = "6.18.2"),
    },
    doc = "Materializes Linux config files used by native compile and link actions.",
)

def _add_linux_probe_args(args, allow_shell, model, values):
    if not allow_shell:
        return
    args.add("-allow_shell")
    if model:
        args.add("-linux_probe_model", model)
    for key, value in sorted(values.items()):
        args.add("-linux_probe_value", "%s=%s" % (key, value))

def _configure_linux_probe_env(allow_shell, env):
    if not allow_shell:
        return
    env.setdefault("CC", "clang")
    env.setdefault("CC_VERSION_TEXT", "clang version 22.1.4None")
    env.setdefault("LD", "ld.lld")
    env.setdefault("CLANG_FLAGS", "-fintegrated-as")
    env.setdefault("RUSTC", "rustc")
    env.setdefault("PAHOLE", "pahole")
    env.setdefault("BINDGEN", "bindgen")

def _linux_compiler_version_string():
    return "clang version 22.1.4None, LLD 22.1.4"

def _linux_resolved_config_impl(ctx):
    config_dir = ctx.label.name + ".config_tree"
    config = ctx.actions.declare_file(config_dir + "/.config")
    auto_conf = ctx.actions.declare_file(config_dir + "/include/config/auto.conf")
    auto_conf_cmd = ctx.actions.declare_file(config_dir + "/include/config/auto.conf.cmd")
    autoconf_h = ctx.actions.declare_file(config_dir + "/include/generated/autoconf.h")
    rustc_cfg = ctx.actions.declare_file(config_dir + "/include/generated/rustc_cfg")
    kernel_release = ctx.actions.declare_file(config_dir + "/include/config/kernel.release")
    aflags = ctx.actions.declare_file(config_dir + "/include/generated/bazel_kbuild_aflags.rsp")
    cflags = ctx.actions.declare_file(config_dir + "/include/generated/bazel_kbuild_cflags.rsp")

    source_root = ctx.file.source_root.dirname if ctx.file.source_root else ctx.file.root.dirname
    vars = dict(ctx.attr.vars)
    vars.setdefault("srctree", source_root)
    env = dict(ctx.attr.env)
    _configure_linux_probe_env(ctx.attr.allow_shell, env)

    fragment = ctx.attr.config[KconfigInfo].config
    args = ctx.actions.args()
    args.add("-root", ctx.file.root)
    args.add("-srctree", source_root)
    extra_kconfig_inputs = []
    for target, prefix in ctx.attr.extra_kconfigs.items():
        file = _single_file(target, "extra_kconfigs")
        args.add("-source_root_map", "%s=%s" % (prefix, file.dirname))
        args.add("-kconfig_extra", "%s=%s" % (prefix, file.path))
        extra_kconfig_inputs.append(file)
    if ctx.attr.config_mode:
        args.add("-config_mode", ctx.attr.config_mode)
    args.add("-resolve_config", "%s=%s" % (ctx.attr.config_name if ctx.attr.config_name else ctx.label.name, fragment.path))
    args.add("-resolved_config_out", config)
    args.add("-resolved_auto_conf_out", auto_conf)
    args.add("-resolved_auto_conf_cmd_out", auto_conf_cmd)
    args.add("-resolved_autoconf_out", autoconf_h)
    args.add("-resolved_rustc_cfg_out", rustc_cfg)
    args.add("-resolved_kernel_release_out", kernel_release)
    args.add("-kernel_version", ctx.attr.version)
    _add_linux_probe_args(args, ctx.attr.allow_shell, ctx.attr.probe_model, ctx.attr.probe_values)
    for key, value in sorted(vars.items()):
        args.add("-var", "%s=%s" % (key, value))
    for key, value in sorted(env.items()):
        args.add("-env", "%s=%s" % (key, value))

    inputs = [ctx.file.root, fragment] + ctx.files.srcs + extra_kconfig_inputs
    if ctx.file.source_root:
        inputs.append(ctx.file.source_root)
    ctx.actions.run(
        executable = ctx.executable._kconfig_parse,
        inputs = depset(inputs),
        outputs = [config, auto_conf, auto_conf_cmd, autoconf_h, rustc_cfg, kernel_release],
        arguments = [args],
        mnemonic = "LinuxResolvedConfig",
        progress_message = "Resolving Linux config %{label}",
    )

    cflags_args = ctx.actions.args()
    cflags_args.add("-config", config)
    cflags_args.add("-arch", env.get("ARCH", "x86"))
    cflags_args.add("-out", cflags)
    cflags_args.add("-asm_out", aflags)
    ctx.actions.run(
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
    files = depset([config, auto_conf, auto_conf_cmd, autoconf_h, rustc_cfg, kernel_release, aflags, cflags])
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
            kernel_release = kernel_release,
            rustc_cfg = rustc_cfg,
        ),
    ]

linux_resolved_config = rule(
    implementation = _linux_resolved_config_impl,
    attrs = {
        "allow_shell": attr.bool(
            default = True,
            doc = "Allow deterministic $(shell,...) expansion while resolving Kconfig defaults.",
        ),
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
        "probe_model": attr.string(
            default = "linux_llvm",
            doc = "Hermetic Linux Kconfig probe model used when allow_shell is set.",
        ),
        "probe_values": attr.string_dict(
            doc = "Overrides for the selected Linux Kconfig probe model.",
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
    },
    doc = "Resolves an imported Linux .config fragment into Kbuild config outputs.",
)

def _linux_perl_runtime(ctx):
    perl_runtime = ctx.toolchains[_PERL_TOOLCHAIN].perl_runtime
    return struct(
        files = perl_runtime.runtime,
        interpreter = perl_runtime.interpreter,
    )

def _linux_object_impl(ctx):
    if ctx.file.src:
        return _linux_real_object_impl(ctx)

    out = ctx.actions.declare_file(ctx.label.name + ".object.txt")
    lines = [
        "object=%s" % ctx.attr.object,
        "mode=%s" % ctx.attr.mode,
    ]
    for flag in ctx.attr.flags:
        lines.append("flag=%s" % flag)
    for flag in ctx.attr.remove_flags:
        lines.append("remove_flag=%s" % flag)
    for key in sorted(ctx.attr.config_fragment.keys()):
        lines.append("%s=%s" % (key, ctx.attr.config_fragment[key]))
    ctx.actions.write(out, "\n".join(lines) + "\n")
    info = LinuxObjectInfo(
        config_fragment = dict(ctx.attr.config_fragment),
        flags = list(ctx.attr.flags),
        mode = ctx.attr.mode,
        object = ctx.attr.object,
        output = out,
        real = False,
        generated_headers = depset(),
        generated_include_dirs = [],
        source = "",
    )
    return [
        DefaultInfo(files = depset([out])),
        info,
    ]

def _linux_real_object_impl(ctx):
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
    base_flags = _linux_compile_flags(ctx, cc_toolchain, feature_configuration)
    config = ctx.attr.config[LinuxConfigInfo] if ctx.attr.config else None
    config_values = _make_config_values(config, ctx.attr.config_fragment)

    out = ctx.actions.declare_file(ctx.label.name + ".o")
    cmd = ctx.actions.declare_file(ctx.label.name + ".cmd")
    compile_object = _linux_compile_object_name(ctx.attr.object)
    objcopy_flags = _linux_objcopy_flags_for_object(ctx.attr.object, ctx.attr.arch)
    needs_relacheck = _linux_object_needs_relacheck(ctx.attr.object)
    if needs_relacheck and not ctx.executable.relacheck:
        fail("linux_object %s builds %s and requires relacheck" % (ctx.label, ctx.attr.object))
    compile_out = out
    if objcopy_flags:
        compile_out = ctx.actions.declare_file(ctx.label.name + ".obj/" + compile_object)
    objcopy_out = out
    if objcopy_flags and needs_relacheck:
        objcopy_out = ctx.actions.declare_file(ctx.label.name + ".obj/" + ctx.attr.object)
    generated_object_headers = []
    exported_generated_headers = []
    exported_generated_include_dirs = []
    generated_sources = []
    utsversion_tmp = None
    src = ctx.file.src
    source_root_file = _linux_source_root_file(ctx)
    make_values = {
        "src": ctx.file.src.dirname,
    }
    if _is_shipped_c_source(ctx.file.src):
        src = ctx.actions.declare_file(ctx.label.name + ".obj/" + ctx.attr.object[:-len(".o")] + ".c")
        ctx.actions.expand_template(
            template = ctx.file.src,
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
        asn1_args.add(ctx.file.src)
        asn1_args.add(generated_c)
        asn1_args.add(generated_h)
        ctx.actions.run(
            executable = ctx.executable.asn1_compiler,
            inputs = [ctx.file.src],
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
            command = "\"$1\" \"$2\" void \"$3\""
        else:
            command = "\"$1\" \"$2\" > \"$3\""
        ctx.actions.run_shell(
            inputs = depset(
                [ctx.file.src],
                transitive = [
                    perl_runtime.files,
                ],
            ),
            outputs = [generated],
            command = command,
            arguments = [perl_runtime.interpreter.path, ctx.file.src.path, generated.path],
            mnemonic = "LinuxPerlAsm",
            progress_message = "Generating Linux perlasm source %{label}",
        )
        src = generated
        generated_sources.append(generated)
    if config and _flags_need_utsversion_tmp(ctx.attr.flags):
        utsversion_tmp = ctx.actions.declare_file(ctx.label.name + ".obj/utsversion-tmp.h")
        uts_args = ctx.actions.args()
        uts_args.add("-config", config.config)
        uts_args.add("-kernel_release", config.kernel_release)
        uts_args.add("-utsversion_out", utsversion_tmp)
        uts_args.add("-build_version=")
        uts_args.add("-build_timestamp=")
        ctx.actions.run(
            executable = ctx.executable._versionheaders,
            inputs = depset([], transitive = [config.files]),
            outputs = [utsversion_tmp],
            arguments = [uts_args],
            mnemonic = "LinuxObjectVersionHeader",
            progress_message = "Generating Linux object version header %{label}",
        )
        generated_object_headers.append(utsversion_tmp)
        make_values["obj"] = utsversion_tmp.dirname
    elif _flags_need_obj_dir(ctx.attr.flags):
        obj_marker = ctx.actions.declare_file(ctx.label.name + ".obj/.bazel-dir")
        ctx.actions.write(obj_marker, "")
        generated_object_headers.append(obj_marker)
        make_values["obj"] = obj_marker.dirname

    generated_headers = ctx.attr.generated_headers[LinuxGeneratedHeadersInfo] if ctx.attr.generated_headers else None
    source_root = source_root_file.dirname if source_root_file else ""
    if source_root:
        make_values["srctree"] = source_root
    if _is_dtb_source(ctx.file.src):
        if not source_root:
            fail("linux_object %s builds a devicetree blob and requires source_root" % ctx.label)
        generated = _linux_dtb_object_source(
            ctx,
            ctx.file.src,
            ctx.attr.object,
            _linux_rule_srcarch(ctx, generated_headers),
        )
        src = generated.src
        generated_sources.extend(generated.files)
    if ctx.attr.object == "arch/x86/kernel/cpu/capflags.o":
        generated = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/kernel/cpu/capflags.c")
        cap_args = ctx.actions.args()
        cpufeatures = _source_tree_file(ctx, "arch/x86/include/asm/cpufeatures.h")
        vmxfeatures = _source_tree_file(ctx, "arch/x86/include/asm/vmxfeatures.h")
        cap_args.add("-cpufeatures", cpufeatures)
        cap_args.add("-vmxfeatures", vmxfeatures)
        cap_args.add("-out", generated)
        ctx.actions.run(
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
        con_args.add("-in", ctx.file.src)
        con_args.add("-out", generated)
        ctx.actions.run(
            executable = ctx.executable._conmakehash,
            inputs = [ctx.file.src],
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
    expanded_remove_flags = _rewrite_source_root_flags(_expand_flag_refs(ctx.attr.remove_flags, config_values, make_values, ctx.attr.object), source_root)
    if ctx.attr.arch == "arm64" and ctx.attr.object.startswith("arch/arm64/kernel/pi/") and ctx.attr.object.endswith(".pi.o"):
        expanded_remove_flags = expanded_remove_flags + _linux_ftrace_remove_flags()
    if ctx.attr.arch == "x86" and ctx.attr.object.startswith("arch/x86/boot/startup/") and ctx.attr.object.endswith(".pi.o"):
        expanded_remove_flags = expanded_remove_flags + _linux_ftrace_remove_flags()
    config_flag_inputs = _linux_filtered_config_flags_for_source(ctx, config, src, expanded_remove_flags)

    args = ctx.actions.args()
    args.add_all(base_flags)
    args.add_all(config_flag_inputs.flags)
    args.add_all(_linux_generated_header_cflags(generated_headers))
    args.add_all(_linux_module_flags(ctx.attr.mode))
    args.add_all(_linux_object_name_flags(compile_object, ctx.attr.modname))
    if config and not source_root:
        args.add("-include")
        args.add(config.autoconf_h)
    args.add_all(_linux_source_preinclude_flags_for_root(source_root, _is_assembly_source(src)))
    if config:
        args.add("-I" + config.include_dir)
    if source_root:
        args.add("-fmacro-prefix-map=%s/=" % source_root)
    dep_generated_header_inputs = []
    for dep in ctx.attr.deps:
        dep_info = dep[LinuxObjectInfo]
        for include_dir in dep_info.generated_include_dirs:
            args.add("-I" + include_dir)
        dep_generated_header_inputs.append(dep_info.generated_headers)
    args.add("-I" + src.dirname)
    if src.dirname != ctx.file.src.dirname:
        args.add("-I" + ctx.file.src.dirname)
    for include_dir in generated_inputs.include_dirs:
        args.add("-I" + include_dir)
    args.add_all(_linux_source_include_flags(ctx, generated_headers))
    for include in ctx.attr.include_dirs:
        args.add("-I" + include)
    if _is_assembly_source(src):
        for include_root in generated_inputs.assembler_include_roots:
            args.add("-Wa,-I," + include_root)
    expanded_flags = _rewrite_source_root_flags(_expand_flag_refs(ctx.attr.flags, config_values, make_values, ctx.attr.object), source_root)
    if utsversion_tmp != None:
        expanded_flags = _rewrite_utsversion_tmp_flags(expanded_flags, ctx.attr.object, utsversion_tmp)
    args.add_all(expanded_flags)
    args.add("-c")
    args.add(src)
    args.add("-o")
    args.add(compile_out)

    generated_files = []
    for generated in ctx.attr.generated:
        generated_files.append(generated[LinuxGeneratedInfo].output)

    direct_inputs = _linux_source_tree_inputs(
        ctx,
        direct = [src] + generated_files + generated_object_headers + generated_sources + generated_inputs.files + config_flag_inputs.inputs,
    )
    if src != ctx.file.src:
        direct_inputs.append(ctx.file.src)
    transitive_inputs = [cc_toolchain.all_files]
    if config:
        transitive_inputs.append(config.files)
    if generated_headers:
        transitive_inputs.append(generated_headers.files)
    transitive_inputs.extend(dep_generated_header_inputs)

    ctx.actions.run(
        executable = compiler,
        inputs = depset(direct_inputs, transitive = transitive_inputs),
        outputs = [compile_out],
        arguments = [args],
        mnemonic = "LinuxObjectCompile",
        progress_message = "Compiling Linux object %{label}",
    )

    if objcopy_flags:
        objcopy_args = ctx.actions.args()
        objcopy_args.add_all(objcopy_flags)
        objcopy_args.add(compile_out)
        objcopy_args.add(objcopy_out)
        ctx.actions.run(
            executable = ctx.executable._llvm_objcopy,
            inputs = [compile_out, ctx.executable._llvm_objcopy],
            outputs = [objcopy_out],
            arguments = [objcopy_args],
            mnemonic = "LinuxObjectObjcopy",
            progress_message = "Objcopying Linux object %{label}",
        )
        if needs_relacheck:
            ctx.actions.run_shell(
                inputs = [objcopy_out, compile_out],
                tools = [ctx.executable.relacheck],
                outputs = [out],
                arguments = [
                    objcopy_out.path,
                    out.path,
                    ctx.executable.relacheck.path,
                    compile_out.path,
                ],
                command = "cp \"$1\" \"$2\" && chmod u+w \"$2\" && \"$3\" \"$2\" \"$4\"",
                mnemonic = "LinuxObjectRelacheck",
                progress_message = "Checking Linux arm64 PI relocations %{label}",
            )

    ctx.actions.write(
        output = cmd,
        content = "\n".join([
            "compiler=%s" % compiler,
            "source=%s" % ctx.file.src.short_path,
            "object=%s" % ctx.attr.object,
            "output=%s" % out.short_path,
        ] + ["flag=%s" % flag for flag in ctx.attr.flags] + ["remove_flag=%s" % flag for flag in ctx.attr.remove_flags]) + "\n",
    )

    info = LinuxObjectInfo(
        config_fragment = dict(ctx.attr.config_fragment),
        flags = list(ctx.attr.flags),
        mode = ctx.attr.mode,
        object = ctx.attr.object,
        output = out,
        real = True,
        generated_headers = depset(exported_generated_headers),
        generated_include_dirs = exported_generated_include_dirs,
        source = ctx.file.src.short_path,
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
        "_allowlist_function_transition": _linux_platform_transition_allowlist_attr(),
        "arch": attr.string(default = "x86"),
        "config_fragment": attr.string_dict(),
        "config": attr.label(providers = [LinuxConfigInfo]),
        "deps": attr.label_list(providers = [LinuxObjectInfo]),
        "flags": attr.string_list(),
        "remove_flags": attr.string_list(),
        "generated": attr.label_list(providers = [LinuxGeneratedInfo]),
        "generated_headers": attr.label(providers = [LinuxGeneratedHeadersInfo]),
        "include_dirs": attr.string_list(),
        "mode": attr.string(values = ["y", "m"], mandatory = True),
        "modname": attr.string(),
        "object": attr.string(mandatory = True),
        "src": attr.label(allow_single_file = True),
        "srcarch": attr.string(),
        "source_tree_info": attr.label(
            providers = [LinuxSourceTreeInfo],
            doc = "Shared Linux source tree provider required by source-backed objects.",
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
        "_certdata": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/certdata"),
            executable = True,
        ),
        "_capflags": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/capflags"),
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
        "_vdso2c": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/vdso2c"),
            executable = True,
        ),
    },
    cfg = _linux_platform_transition,
    fragments = ["cpp"],
    toolchains = use_cc_toolchain() + [_PERL_TOOLCHAIN],
    doc = "Placeholder for one Linux object variant keyed by a reduced Kconfig fragment.",
)

def _linux_composite_object_impl(ctx):
    object_infos = [obj[LinuxObjectInfo] for obj in ctx.attr.objects]
    if object_infos and all([info.real for info in object_infos]):
        return _linux_real_composite_object_impl(ctx, object_infos)

    out = ctx.actions.declare_file(ctx.label.name + ".object.txt")
    lines = [
        "composite=%s" % ctx.attr.object,
        "mode=%s" % ctx.attr.mode,
    ]
    for info in object_infos:
        lines.append("member=%s mode=%s output=%s" % (info.object, info.mode, info.output.short_path))
    for key in sorted(ctx.attr.config_fragment.keys()):
        lines.append("%s=%s" % (key, ctx.attr.config_fragment[key]))
    ctx.actions.write(out, "\n".join(lines) + "\n")
    info = LinuxObjectInfo(
        config_fragment = dict(ctx.attr.config_fragment),
        flags = [],
        mode = ctx.attr.mode,
        object = ctx.attr.object,
        output = out,
        real = False,
        generated_headers = depset(),
        generated_include_dirs = [],
        source = "",
    )
    return [
        DefaultInfo(files = depset([out])),
        info,
    ]

def _linux_real_composite_object_impl(ctx, object_infos):
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
    ctx.actions.run(
        executable = linker,
        inputs = depset([info.output for info in object_infos], transitive = [cc_toolchain.all_files]),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxCompositeObject",
        progress_message = "Linking Linux composite object %{label}",
    )
    info = LinuxObjectInfo(
        config_fragment = dict(ctx.attr.config_fragment),
        flags = [],
        mode = ctx.attr.mode,
        object = ctx.attr.object,
        output = out,
        real = True,
        generated_headers = depset(transitive = [info.generated_headers for info in object_infos]),
        generated_include_dirs = _unique_strings([include_dir for info in object_infos for include_dir in info.generated_include_dirs]),
        source = "",
    )
    return [
        DefaultInfo(files = depset([out])),
        info,
        OutputGroupInfo(object = depset([out])),
    ]

linux_composite_object = rule(
    implementation = _linux_composite_object_impl,
    attrs = {
        "_allowlist_function_transition": _linux_platform_transition_allowlist_attr(),
        "arch": attr.string(default = "x86"),
        "config_fragment": attr.string_dict(),
        "mode": attr.string(values = ["y", "m"], mandatory = True),
        "object": attr.string(mandatory = True),
        "objects": attr.label_list(providers = [LinuxObjectInfo], mandatory = True),
    },
    cfg = _linux_platform_transition,
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
    args.add("-I" + config.include_dir)
    args.add_all(_linux_source_include_flags_for_root(source_root, "arm64", generated_headers.include_dirs))
    args.add(src)
    args.add("-o")
    args.add(out)
    ctx.actions.run(
        executable = compiler,
        inputs = depset(
            _linux_source_tree_inputs(ctx, direct = [src]),
            transitive = [cc_toolchain.all_files, config.files, generated_headers.files],
        ),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxArm64NvheLinkerScript",
        progress_message = "Preprocessing Linux arm64 nVHE linker script %{label}",
    )
    return out

def _linux_link_relocatable(ctx, linker, cc_toolchain, feature_configuration, out_relpath, objects, flags = [], extra_inputs = []):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + out_relpath)
    args = ctx.actions.args()
    args.add_all(_cc_target_flags(ctx, cc_toolchain, feature_configuration))
    args.add("-fuse-ld=lld")
    args.add("-nostdlib")
    args.add("-r")
    args.add_all(flags)
    args.add("-o")
    args.add(out)
    args.add_all(objects)
    ctx.actions.run(
        executable = linker,
        inputs = depset(objects + extra_inputs, transitive = [cc_toolchain.all_files]),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxRelocatableLink",
        progress_message = "Linking Linux relocatable object %{label}",
    )
    return out

def _linux_arm64_nvhe_object_impl(ctx):
    object_infos = [obj[LinuxObjectInfo] for obj in ctx.attr.objects]
    missing = [info.object for info in object_infos if not info.real]
    if missing:
        fail("linux_arm64_nvhe_object %s requires real member objects:\n%s" % (ctx.label, "\n".join(missing)))
    if not ctx.attr.config:
        fail("linux_arm64_nvhe_object %s requires config" % ctx.label)
    if not ctx.attr.generated_headers:
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
    config = ctx.attr.config[LinuxConfigInfo]
    generated_headers = ctx.attr.generated_headers[LinuxGeneratedHeadersInfo]
    source_root = source_root_file.dirname
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
        flags = ["-Wl,-T," + linker_script.path],
        extra_inputs = [linker_script],
    )

    reloc_s = ctx.actions.declare_file(ctx.label.name + ".obj/arch/arm64/kvm/hyp/nvhe/hyp-reloc.S")
    ctx.actions.run_shell(
        inputs = [tmp, ctx.executable._genhyprel],
        outputs = [reloc_s],
        command = "\"$1\" \"$2\" > \"$3\"",
        arguments = [ctx.executable._genhyprel.path, tmp.path, reloc_s.path],
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
    ctx.actions.run(
        executable = ctx.executable._llvm_objcopy,
        inputs = [rel, ctx.executable._llvm_objcopy],
        outputs = [out],
        arguments = [objcopy_args],
        mnemonic = "LinuxArm64NvheObjcopy",
        progress_message = "Objcopying Linux arm64 nVHE object %{label}",
    )

    info = LinuxObjectInfo(
        config_fragment = dict(ctx.attr.config_fragment),
        flags = [],
        mode = ctx.attr.mode,
        object = ctx.attr.object,
        output = out,
        real = True,
        generated_headers = depset(transitive = [info.generated_headers for info in object_infos]),
        generated_include_dirs = _unique_strings([include_dir for info in object_infos for include_dir in info.generated_include_dirs]),
        source = "",
    )
    return [
        DefaultInfo(files = depset([out])),
        info,
        OutputGroupInfo(object = depset([out])),
    ]

linux_arm64_nvhe_object = rule(
    implementation = _linux_arm64_nvhe_object_impl,
    attrs = {
        "_allowlist_function_transition": _linux_platform_transition_allowlist_attr(),
        "arch": attr.string(default = "arm64"),
        "config_fragment": attr.string_dict(),
        "config": attr.label(providers = [LinuxConfigInfo]),
        "generated_headers": attr.label(providers = [LinuxGeneratedHeadersInfo]),
        "mode": attr.string(values = ["y", "m"], mandatory = True),
        "object": attr.string(mandatory = True),
        "objects": attr.label_list(providers = [LinuxObjectInfo]),
        "source_tree_info": attr.label(
            providers = [LinuxSourceTreeInfo],
            doc = "Shared Linux source tree provider.",
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
    },
    cfg = _linux_platform_transition,
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Builds the arm64 nVHE KVM custom composite object with hyp relocations.",
)

def _linux_generated_file_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name + ".generated.txt")
    lines = [
        "generated=%s" % ctx.attr.target,
    ]
    for src in ctx.files.srcs:
        lines.append("src=%s" % src.short_path)
    ctx.actions.write(out, "\n".join(lines) + "\n")
    info = LinuxGeneratedInfo(
        output = out,
        target = ctx.attr.target,
    )
    return [
        DefaultInfo(files = depset([out])),
        info,
    ]

linux_generated_file = rule(
    implementation = _linux_generated_file_impl,
    attrs = {
        "srcs": attr.label_list(allow_files = True),
        "target": attr.string(mandatory = True),
    },
    doc = "Placeholder native action for a generated Linux source or header target.",
)

def _linux_archive_impl(ctx):
    object_infos = [obj[LinuxObjectInfo] for obj in ctx.attr.objects]
    if object_infos and all([info.real for info in object_infos]):
        return _linux_real_archive_impl(ctx, object_infos)

    out = ctx.actions.declare_file(ctx.label.name + ".archive.txt")
    lines = [
        "archive=%s" % ctx.label.name,
        "kind=%s" % ctx.attr.kind,
    ]
    for info in object_infos:
        lines.append("object=%s mode=%s output=%s" % (info.object, info.mode, info.output.short_path))
    ctx.actions.write(out, "\n".join(lines) + "\n")
    info = LinuxArchiveInfo(
        kind = ctx.attr.kind,
        objects = object_infos,
        output = out,
        real = False,
    )
    return [
        DefaultInfo(files = depset([out])),
        info,
    ]

def _linux_real_archive_impl(ctx, object_infos):
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
    ctx.actions.run(
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
        real = True,
    )
    return [
        DefaultInfo(files = depset([out])),
        info,
    ]

linux_archive = rule(
    implementation = _linux_archive_impl,
    attrs = {
        "_allowlist_function_transition": _linux_platform_transition_allowlist_attr(),
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
    cfg = _linux_platform_transition,
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Placeholder native archive action for built-in.a, lib.a, or module object groups.",
)

def _linux_compact_image_impl(ctx):
    object_infos = [obj[LinuxObjectInfo] for obj in ctx.attr.objects]
    if object_infos and all([info.real for info in object_infos]):
        return _linux_real_compact_image_impl(ctx, object_infos)
    missing = [info.object for info in object_infos if not info.real]
    if ctx.attr.require_real and missing:
        fail("linux_compact_image %s still has non-real object inputs:\n%s" % (ctx.label, "\n".join(missing)))

    out = ctx.actions.declare_file(ctx.label.name + ".image.txt")
    lines = ["image=%s" % ctx.label.name]
    for info in object_infos:
        lines.append("object=%s mode=%s output=%s" % (info.object, info.mode, info.output.short_path))
    object_outputs = [info.output for info in object_infos]
    ctx.actions.run_shell(
        inputs = object_outputs,
        outputs = [out],
        command = "cat > \"$1\" <<'EOF'\n%s\nEOF\n" % ("\n".join(lines)),
        arguments = [out.path],
        mnemonic = "LinuxCompactImage",
        progress_message = "Collecting compact Linux image inputs %{label}",
    )
    info = LinuxImageInfo(
        archives = [],
        generated = [],
        objects = object_infos,
        output = out,
    )
    return [
        DefaultInfo(files = depset([out])),
        info,
    ]

def _linux_real_compact_image_impl(ctx, object_infos):
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
    ctx.actions.run(
        executable = archiver,
        inputs = depset(object_outputs, transitive = [cc_toolchain.all_files]),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxCompactImageArchive",
        progress_message = "Archiving compact Linux image %{label}",
    )
    info = LinuxImageInfo(
        archives = [],
        generated = [],
        objects = object_infos,
        output = out,
    )
    return [
        DefaultInfo(files = depset([out])),
        info,
    ]

linux_compact_image = rule(
    implementation = _linux_compact_image_impl,
    attrs = {
        "_allowlist_function_transition": _linux_platform_transition_allowlist_attr(),
        "arch": attr.string(default = "x86"),
        "objects": attr.label_list(providers = [LinuxObjectInfo]),
        "require_real": attr.bool(),
    },
    cfg = _linux_platform_transition,
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Links compact Linux object variants into one relocatable image object when all inputs are real.",
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
    args.add("-I" + config.include_dir)
    args.add_all(_linux_source_include_flags_for_root(source_root, srcarch, generated_headers.include_dirs))
    args.add(ctx.file.linker_script)
    args.add("-o")
    args.add(out)

    ctx.actions.run(
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

def _linux_vmlinux_compile_source(ctx, compiler, cc_toolchain, feature_configuration, config, generated_headers, source_root, src, out_relpath, object_name, extra_flags = []):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + out_relpath)
    assembly = _is_assembly_source(src)
    args = ctx.actions.args()
    args.add_all(_linux_compile_flags(ctx, cc_toolchain, feature_configuration))
    args.add_all(_linux_config_flags_for_source(config, src))
    args.add_all(_linux_generated_header_cflags(generated_headers))
    args.add_all(_linux_object_name_flags(object_name))
    args.add_all(_linux_source_preinclude_flags_for_root(source_root, assembly))
    args.add("-I" + config.include_dir)
    args.add("-I" + src.dirname)
    args.add_all(_linux_source_include_flags(ctx, generated_headers))
    args.add_all(extra_flags)
    args.add("-c")
    args.add(src)
    args.add("-o")
    args.add(out)

    ctx.actions.run(
        executable = compiler,
        inputs = depset(
            _linux_source_tree_inputs(ctx, direct = [src]),
            transitive = [cc_toolchain.all_files, config.files, generated_headers.files],
        ),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxVmlinuxCompile",
        progress_message = "Compiling Linux vmlinux support object %{label}",
    )
    return out

def _linux_vmlinux_export_object(ctx, compiler, cc_toolchain, feature_configuration, config, generated_headers, source_root):
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
    )

def _linux_system_map(ctx, input, name):
    nm_out = ctx.actions.declare_file(ctx.label.name + ".obj/" + name + ".nm")
    ctx.actions.run_shell(
        inputs = [input, ctx.executable._llvm_nm],
        outputs = [nm_out],
        command = "\"$1\" -n \"$2\" > \"$3\"",
        arguments = [ctx.executable._llvm_nm.path, input.path, nm_out.path],
        mnemonic = "LinuxVmlinuxNM",
        progress_message = "Generating Linux vmlinux nm output %{label}",
    )

    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + name + ".syms")
    map_args = ctx.actions.args()
    map_args.add("-in", nm_out)
    map_args.add("-out", out)
    ctx.actions.run(
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
    inputs = [input, config.config, ctx.executable._llvm_nm]
    sorttable_path = ""
    if ctx.executable.sorttable_tool:
        inputs.append(ctx.executable.sorttable_tool)
        sorttable_path = ctx.executable.sorttable_tool.path
    ctx.actions.run_shell(
        inputs = inputs,
        outputs = [out, nm_out],
        command = """\
if grep -qx 'CONFIG_BUILDTIME_TABLE_SORT=y' "$2"; then
  if [ -z "$4" ]; then
    echo "CONFIG_BUILDTIME_TABLE_SORT=y requires sorttable_tool" >&2
    exit 1
  fi
  "$3" -S "$1" > "$5"
  cp "$1" "$6"
  chmod u+w "$6"
  "$4" -s "$5" "$6"
else
  : > "$5"
  cp "$1" "$6"
fi
""",
        arguments = [input.path, config.config.path, ctx.executable._llvm_nm.path, sorttable_path, nm_out.path, out.path],
        mnemonic = "LinuxVmlinuxSortTable",
        progress_message = "Sorting Linux kernel tables %{label}",
    )
    return out

def _shell_quote(value):
    return "'" + value.replace("'", "'\\''") + "'"

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

    set_flags_shell = " ".join([_shell_quote(flag) for flag in set_flags])
    remove_flags_shell = " ".join([_shell_quote(flag) for flag in remove_flags])
    ctx.actions.run_shell(
        inputs = [input, ctx.executable._llvm_objcopy],
        outputs = [out],
        command = "\"$1\" %s \"$2\" \"$3\" && \"$1\" %s \"$3\"" % (set_flags_shell, remove_flags_shell),
        arguments = [ctx.executable._llvm_objcopy.path, input.path, out.path],
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
    ctx.actions.run_shell(
        inputs = [kallsyms_tool, system_map],
        outputs = [asm],
        command = "\"$1\" %s \"$2\" > \"$3\"" % " ".join(kallsyms_flags),
        arguments = [kallsyms_tool.path, system_map.path, asm.path],
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

    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + input.basename + ".btf.o")
    btf_vmlinux = ctx.actions.declare_file(ctx.label.name + ".obj/" + input.basename + ".btf")
    pahole_flags = [
        "--btf_features=encode_force,var,float,enum64,decl_tag,type_tag,optimized_func,consistent_func,decl_tag_kfuncs",
        "--btf_features=attributes",
        "--lang_exclude=rust",
    ]
    et_rel = "\\0\\1" if config.config_flags.get("CONFIG_CPU_BIG_ENDIAN") == "y" else "\\1\\0"
    ctx.actions.run_shell(
        inputs = [input],
        tools = [ctx.executable.pahole, ctx.executable._llvm_objcopy],
        outputs = [btf_vmlinux, out],
        command = """\
set -euo pipefail
cp "$1" "$2"
chmod u+w "$2"
LLVM_OBJCOPY="$3" "$4" -J {pahole_flags} "$2"
"$3" --only-section=.BTF --set-section-flags .BTF=alloc,readonly --strip-all "$2" "$5" 2>/dev/null
printf '{et_rel}' | dd of="$5" conv=notrunc bs=1 seek=16 status=none
""".format(
            et_rel = et_rel,
            pahole_flags = " ".join([_shell_quote(flag) for flag in pahole_flags]),
        ),
        arguments = [
            input.path,
            btf_vmlinux.path,
            ctx.executable._llvm_objcopy.path,
            ctx.executable.pahole.path,
            out.path,
        ],
        mnemonic = "LinuxBTF",
        progress_message = "Generating Linux BTF %{label}",
    )
    return out

def _linux_resolve_btfids(ctx, config, input):
    if config.config_flags.get("CONFIG_DEBUG_INFO_BTF") != "y":
        return input
    if not ctx.executable.resolve_btfids_tool:
        fail("linux_vmlinux %s has DEBUG_INFO_BTF enabled and requires resolve_btfids_tool" % ctx.label)

    out = ctx.actions.declare_file(ctx.label.name + ".btfids.vmlinux.unstripped")
    args = []
    if config.config_flags.get("CONFIG_WERROR") == "y":
        args.append("--fatal_warnings")
    ctx.actions.run_shell(
        inputs = [input],
        tools = [ctx.executable.resolve_btfids_tool],
        outputs = [out],
        command = """\
set -euo pipefail
cp "$1" "$2"
chmod u+w "$2"
"$3" {args} "$2"
""".format(args = " ".join([_shell_quote(arg) for arg in args])),
        arguments = [
            input.path,
            out.path,
            ctx.executable.resolve_btfids_tool.path,
        ],
        mnemonic = "LinuxResolveBTFIDs",
        progress_message = "Resolving Linux BTF IDs %{label}",
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
        args.add("--script=" + linker_script.path)
        if strip_debug:
            args.add("--strip-debug")
        whole_archive = "--whole-archive"
        no_whole_archive = "--no-whole-archive"
    else:
        args.add_all(_cc_target_flags(ctx, cc_toolchain, feature_configuration))
        args.add_all(_linux_vmlinux_link_flags(ctx, ctx.attr.config[LinuxConfigInfo]))
        args.add("-Wl,--script=" + linker_script.path)
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

    ctx.actions.run(
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
    if config.config_flags.get("CONFIG_ARCH_VMLINUX_NEEDS_RELOCS") == "y":
        flags.extend([
            "-Wl,--emit-relocs",
            "-Wl,--discard-none",
        ])
    return flags

def _linux_vmlinux_relocatable_object(ctx, linker, cc_toolchain, feature_configuration, image_object, image_object_inputs):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/vmlinux.o")
    args = ctx.actions.args()
    args.add_all(_cc_target_flags(ctx, cc_toolchain, feature_configuration))
    args.add_all([
        "-fuse-ld=lld",
        "-nostdlib",
        "-no-pie",
        "-Wl,-r",
        "-Wl,-m,elf_x86_64",
        "-o",
        out,
        "-Wl,--whole-archive",
    ])
    args.add(image_object)
    args.add("-Wl,--no-whole-archive")

    ctx.actions.run(
        executable = linker,
        inputs = depset([image_object], transitive = [image_object_inputs, cc_toolchain.all_files]),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxVmlinuxRelocatable",
        progress_message = "Linking Linux relocatable vmlinux.o %{label}",
    )
    return out

def _linux_vmlinux_objtool(ctx, config, linker, cc_toolchain, feature_configuration, image_object, image_object_inputs):
    if not ctx.executable.objtool:
        return image_object

    image_object = _linux_vmlinux_relocatable_object(ctx, linker, cc_toolchain, feature_configuration, image_object, image_object_inputs)
    out = ctx.actions.declare_file(ctx.label.name + ".obj/vmlinux.o.objtool")
    args = ctx.actions.args()
    args.add("-config", config.config)
    args.add("-objtool", ctx.executable.objtool)
    args.add("-in", image_object)
    args.add("-out", out)
    ctx.actions.run(
        executable = ctx.executable._objtoolrun,
        inputs = [config.config, image_object, ctx.executable.objtool],
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxObjtool",
        progress_message = "Processing Linux vmlinux.o with objtool %{label}",
    )
    return out

def _linux_real_vmlinux_impl(ctx):
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
    source_root = ctx.file.source_root.dirname
    linker_script = _linux_vmlinux_linker_script(ctx, compiler, cc_toolchain, feature_configuration, config, generated_headers, source_root)
    export_object = _linux_vmlinux_export_object(ctx, compiler, cc_toolchain, feature_configuration, config, generated_headers, source_root)
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
        ["-fno-function-sections", "-fno-data-sections", "-include", "generated/utsversion.h"],
    )
    image_object_inputs = depset([info.output for info in image.objects])
    image_object = _linux_vmlinux_objtool(ctx, config, linker, cc_toolchain, feature_configuration, image.output, image_object_inputs)

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
        _linux_vmlinux_link(ctx, linker, cc_toolchain, feature_configuration, image_object, image_object_inputs, export_object, version_object, linker_script, kallsyms_object, None, btf_base, False)
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
        generated = image.generated,
        objects = image.objects,
        output = out,
    )
    return [
        DefaultInfo(files = depset([out, system_map])),
        info,
        OutputGroupInfo(system_map = depset([system_map]), vmlinux = depset([out])),
    ]

def _linux_vmlinux_impl(ctx):
    if ctx.attr.image:
        return _linux_real_vmlinux_impl(ctx)

    out = ctx.actions.declare_file(ctx.label.name + ".vmlinux.txt")
    archive_infos = [archive[LinuxArchiveInfo] for archive in ctx.attr.archives]
    generated_infos = [generated[LinuxGeneratedInfo] for generated in ctx.attr.generated]
    lines = ["vmlinux=%s" % ctx.label.name]
    for info in archive_infos:
        lines.append("archive=%s kind=%s" % (info.output.short_path, info.kind))
    for info in generated_infos:
        lines.append("generated=%s output=%s" % (info.target, info.output.short_path))
    archive_outputs = [info.output for info in archive_infos]
    generated_outputs = [info.output for info in generated_infos]
    ctx.actions.run_shell(
        inputs = archive_outputs + generated_outputs,
        outputs = [out],
        command = "cat > \"$1\" <<'EOF'\n%s\nEOF\n" % ("\n".join(lines)),
        arguments = [out.path],
        mnemonic = "LinuxVmlinux",
        progress_message = "Collecting Linux vmlinux inputs %{label}",
    )
    info = LinuxImageInfo(
        archives = archive_infos,
        generated = generated_infos,
        objects = [],
        output = out,
    )
    return [
        DefaultInfo(files = depset([out], transitive = [depset(archive_outputs + generated_outputs)])),
        info,
    ]

linux_vmlinux = rule(
    implementation = _linux_vmlinux_impl,
    attrs = {
        "_allowlist_function_transition": _linux_platform_transition_allowlist_attr(),
        "arch": attr.string(default = "x86"),
        "archives": attr.label_list(providers = [LinuxArchiveInfo]),
        "config": attr.label(providers = [LinuxConfigInfo]),
        "format": attr.string(
            default = "x86_64",
            values = [
                "arm64",
                "x86_64",
            ],
        ),
        "generated": attr.label_list(providers = [LinuxGeneratedInfo]),
        "generated_headers": attr.label(providers = [LinuxGeneratedHeadersInfo]),
        "image": attr.label(providers = [LinuxImageInfo]),
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
        "source_root": attr.label(allow_single_file = True),
        "source_tree": attr.label_list(allow_files = True),
        "srcarch": attr.string(),
        "kallsyms_tool": attr.label(
            cfg = "exec",
            doc = "Kernel-source-specific scripts/kallsyms executable. Required when kallsyms is enabled for a real vmlinux link.",
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
        "_llvm_objcopy": attr.label(
            allow_single_file = True,
            cfg = "exec",
            default = Label("@llvm//tools:llvm-objcopy"),
            executable = True,
        ),
        "_mksysmap": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/mksysmap"),
            executable = True,
        ),
        "_objtoolrun": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/objtoolrun"),
            executable = True,
        ),
    },
    cfg = _linux_platform_transition,
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Links a native vmlinux ELF when image is set, otherwise emits placeholder metadata.",
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

def _linux_x86_add_include_flags(args, config, generated_headers, source_root, extra = []):
    args.add("-I" + config.include_dir)
    args.add_all(_linux_source_include_flags_for_root(source_root, "x86", generated_headers.include_dirs))
    for include_dir in extra:
        args.add("-I" + include_dir)

def _linux_x86_run_x86boot(ctx, outputs, arguments, inputs = []):
    args = ctx.actions.args()
    args.add_all(arguments)
    ctx.actions.run(
        executable = ctx.executable._x86boot,
        inputs = inputs,
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
    ctx.actions.run(
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
    ctx.actions.run_shell(
        inputs = [ctx.executable.x86_relocs_tool, vmlinux],
        outputs = [out],
        command = "\"$1\" \"$2\" > \"$3\"; \"$1\" --abs-relocs \"$2\"",
        arguments = [ctx.executable.x86_relocs_tool.path, vmlinux.path, out.path],
        mnemonic = "LinuxX86Relocs",
        progress_message = "Generating Linux x86 compressed relocation data %{label}",
    )
    return out

def _linux_x86_concat(ctx, inputs, out_relpath):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + out_relpath)
    args = ["concat"]
    for input in inputs:
        args.extend(["-in", input])
    args.extend(["-out", out])
    _linux_x86_run_x86boot(ctx, [out], args, inputs = [ctx.executable._x86boot] + inputs)
    return out

def _linux_x86_append_size(ctx, payload, size_inputs, out_relpath):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + out_relpath)
    args = ["append-size"]
    for input in payload:
        args.extend(["-in", input])
    for input in size_inputs:
        args.extend(["-size-in", input])
    args.extend(["-out", out])
    _linux_x86_run_x86boot(ctx, [out], args, inputs = [ctx.executable._x86boot] + payload + size_inputs)
    return out

def _linux_x86_lz4(ctx, input, out_relpath):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + out_relpath)
    ctx.actions.run_shell(
        inputs = [ctx.executable._lz4, input],
        outputs = [out],
        command = "\"$1\" -l -9 -c \"$2\" > \"$3\"",
        arguments = [ctx.executable._lz4.path, input.path, out.path],
        mnemonic = "LinuxX86LZ4",
        progress_message = "Compressing Linux x86 kernel payload %{label}",
    )
    return out

def _linux_x86_piggy(ctx, compressed):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/boot/compressed/piggy.S")
    _linux_x86_run_x86boot(
        ctx,
        [out],
        ["piggy", "-in", compressed, "-out", out],
        inputs = [ctx.executable._x86boot, compressed],
    )
    return out

def _linux_x86_offsets(ctx, input, kind, out_relpath):
    out = ctx.actions.declare_file(ctx.label.name + ".obj/" + out_relpath)
    _linux_x86_run_x86boot(
        ctx,
        [out],
        ["offsets", "-kind", kind, "-in", input, "-out", out],
        inputs = [ctx.executable._x86boot, input],
    )
    return out

def _linux_x86_bzimage(ctx, setup_bin, vmlinux_bin):
    out = ctx.actions.declare_file(ctx.label.name + "." + ctx.attr.extension)
    _linux_x86_run_x86boot(
        ctx,
        [out],
        ["bzimage", "-setup", setup_bin, "-kernel", vmlinux_bin, "-out", out],
        inputs = [ctx.executable._x86boot, setup_bin, vmlinux_bin],
    )
    return out

def _linux_x86_inat_tables(ctx):
    opcode_map = _source_tree_file(ctx, "arch/x86/lib/x86-opcode-map.txt")
    out = ctx.actions.declare_file(ctx.label.name + ".obj/arch/x86/lib/inat-tables.c")
    args = ctx.actions.args()
    args.add("-in", opcode_map)
    args.add("-out", out)
    ctx.actions.run(
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
    args.add(source_root + "/include/linux/hidden.h")
    _linux_x86_add_include_flags(
        args,
        config,
        generated_headers,
        source_root,
        extra = [
            source_root + "/arch/x86/boot/compressed",
            source_root + "/arch/x86/boot",
        ] + extra_include_dirs,
    )
    args.add("-c")
    args.add(src)
    args.add("-o")
    args.add(out)
    ctx.actions.run(
        executable = compiler,
        inputs = _linux_x86_boot_inputs(ctx, cc_toolchain, config, generated_headers, extra = [src] + extra_inputs),
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
    ctx.actions.run(
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
    args.add(source_root + "/include/linux/hidden.h")
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
    ctx.actions.run(
        executable = compiler,
        inputs = _linux_x86_boot_inputs(ctx, cc_toolchain, config, generated_headers, extra = [src]),
        outputs = [compile_out],
        arguments = [args],
        mnemonic = "LinuxX86EFIStubCompile",
        progress_message = "Compiling Linux x86 EFI stub object %{label}",
    )

    objcopy_args = ctx.actions.args()
    objcopy_args.add("--remove-section=.note.gnu.property")
    objcopy_args.add(compile_out)
    objcopy_args.add(out)
    ctx.actions.run(
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
    ctx.actions.run(
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
    compressed = _linux_x86_lz4(ctx, payload, "arch/x86/boot/compressed/vmlinux.bin.lz4.raw")
    compressed_with_size = _linux_x86_append_size(ctx, [compressed], [payload], "arch/x86/boot/compressed/vmlinux.bin.lz4")
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
    args.add("-Wl,-T," + linker_script.path)
    args.add("-o")
    args.add(out)
    args.add_all(objects)
    args.add_all(archives)
    ctx.actions.run(
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
    )
    args.add_all(extra_flags)
    args.add("-c")
    args.add(src)
    args.add("-o")
    args.add(out)
    ctx.actions.run(
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
        inputs = [ctx.executable._x86boot, cpufeatures, masks],
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
    ctx.actions.run(
        executable = ld,
        inputs = depset(objects + [setup_ld], transitive = [cc_toolchain.all_files]),
        outputs = [setup_elf],
        arguments = [args],
        mnemonic = "LinuxX86SetupLink",
        progress_message = "Linking Linux x86 setup ELF %{label}",
    )
    return _linux_x86_objcopy(ctx, setup_elf, "arch/x86/boot/setup.bin", ["-O", "binary"])

def _linux_real_x86_bzimage_impl(ctx):
    if not ctx.attr.config:
        fail("linux_compressed_image with real x86 output requires config")
    if not ctx.attr.generated_headers:
        fail("linux_compressed_image with real x86 output requires generated_headers")
    if not ctx.file.source_root:
        fail("linux_compressed_image with real x86 output requires source_root")

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
    source_root = ctx.file.source_root.dirname

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
        generated = image.generated,
        objects = image.objects,
        output = out,
    )
    module_outputs = _linux_empty_module_outputs(ctx)
    return [
        DefaultInfo(files = depset([out])),
        info,
        OutputGroupInfo(
            bzimage = depset([out]),
            modinfo = depset([module_outputs.modinfo]),
            modules = depset([module_outputs.modules]),
        ),
    ]

def _linux_empty_module_outputs(ctx):
    modinfo = ctx.actions.declare_file(ctx.label.name + ".modules.builtin.modinfo")
    modules = ctx.actions.declare_directory(ctx.label.name + ".modules")
    ctx.actions.write(modinfo, "")
    ctx.actions.run_shell(
        outputs = [modules],
        command = "mkdir -p \"$1\"",
        arguments = [modules.path],
        mnemonic = "LinuxEmptyModules",
        progress_message = "Creating empty Linux module output group %{label}",
    )
    return struct(
        modinfo = modinfo,
        modules = modules,
    )

def _linux_packaged_output_impl(ctx):
    if getattr(ctx.attr, "format", "placeholder") == "x86_bzimage":
        return _linux_real_x86_bzimage_impl(ctx)
    if getattr(ctx.attr, "format", "placeholder") == "arm64_image":
        return _linux_real_objcopy_image_impl(ctx, [
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

    image = ctx.attr.image[LinuxImageInfo]
    out = ctx.actions.declare_file(ctx.label.name + "." + ctx.attr.extension + ".txt")
    lines = [
        "%s=%s" % (ctx.attr.kind, ctx.label.name),
        "input=%s" % image.output.short_path,
    ]
    ctx.actions.run_shell(
        inputs = [image.output],
        outputs = [out],
        command = "cat > \"$1\" <<'EOF'\n%s\nEOF\n" % ("\n".join(lines)),
        arguments = [out.path],
        mnemonic = "LinuxPackagedOutput",
        progress_message = "Collecting Linux packaged output inputs %{label}",
    )
    info = LinuxImageInfo(
        archives = image.archives,
        generated = image.generated,
        objects = image.objects,
        output = out,
    )
    module_outputs = _linux_empty_module_outputs(ctx)
    return [
        DefaultInfo(files = depset([out])),
        info,
        OutputGroupInfo(
            modinfo = depset([module_outputs.modinfo]),
            modules = depset([module_outputs.modules]),
        ),
    ]

def _linux_real_objcopy_image_impl(ctx, objcopy_flags):
    image = ctx.attr.image[LinuxImageInfo]
    out = ctx.actions.declare_file(ctx.label.name + "." + ctx.attr.extension)
    args = ctx.actions.args()
    args.add_all(objcopy_flags)
    args.add(image.output)
    args.add(out)
    ctx.actions.run(
        executable = ctx.executable._llvm_objcopy,
        inputs = [image.output, ctx.executable._llvm_objcopy],
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxKernelObjcopyImage",
        progress_message = "Objcopying Linux kernel image %{label}",
    )
    info = LinuxImageInfo(
        archives = image.archives,
        generated = image.generated,
        objects = image.objects,
        output = out,
    )
    module_outputs = _linux_empty_module_outputs(ctx)
    return [
        DefaultInfo(files = depset([out])),
        info,
        OutputGroupInfo(
            image = depset([out]),
            modinfo = depset([module_outputs.modinfo]),
            modules = depset([module_outputs.modules]),
        ),
    ]

linux_compressed_image = rule(
    implementation = _linux_packaged_output_impl,
    attrs = {
        "_allowlist_function_transition": _linux_platform_transition_allowlist_attr(),
        "arch": attr.string(default = "x86"),
        "config": attr.label(providers = [LinuxConfigInfo]),
        "extension": attr.string(default = "image"),
        "format": attr.string(
            default = "placeholder",
            values = [
                "arm64_image",
                "placeholder",
                "x86_bzimage",
            ],
        ),
        "generated_headers": attr.label(providers = [LinuxGeneratedHeadersInfo]),
        "image": attr.label(providers = [LinuxImageInfo], mandatory = True),
        "kind": attr.string(default = "compressed_image"),
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
    cfg = _linux_platform_transition,
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    doc = "Placeholder native compressed kernel image action.",
)

def _linux_module_impl(ctx):
    image = ctx.attr.image[LinuxImageInfo]
    dep_infos = [dep[LinuxImageInfo] for dep in ctx.attr.deps]
    out = ctx.actions.declare_file(ctx.label.name + "." + ctx.attr.extension + ".txt")
    lines = [
        "module=%s" % ctx.label.name,
        "input=%s" % image.output.short_path,
    ]
    if ctx.attr.module_name:
        lines.append("module_name=%s" % ctx.attr.module_name)
    if ctx.attr.target:
        lines.append("target=%s" % ctx.attr.target)
    for src in ctx.files.srcs:
        lines.append("src=%s" % src.short_path)
    for dep in dep_infos:
        lines.append("dep=%s" % dep.output.short_path)
    ctx.actions.write(out, "\n".join(lines) + "\n")
    info = LinuxImageInfo(
        archives = image.archives,
        generated = image.generated,
        objects = image.objects,
        output = out,
    )
    return [
        DefaultInfo(files = depset([out])),
        info,
    ]

linux_module = rule(
    implementation = _linux_module_impl,
    attrs = {
        "deps": attr.label_list(providers = [LinuxImageInfo]),
        "extension": attr.string(default = "ko"),
        "image": attr.label(providers = [LinuxImageInfo], mandatory = True),
        "module_name": attr.string(),
        "srcs": attr.label_list(allow_files = True),
        "target": attr.string(),
    },
    doc = "Placeholder native module packaging action.",
)

linux_modpost = rule(
    implementation = _linux_packaged_output_impl,
    attrs = {
        "extension": attr.string(default = "modpost"),
        "image": attr.label(providers = [LinuxImageInfo], mandatory = True),
        "kind": attr.string(default = "modpost"),
    },
    doc = "Placeholder native modpost action.",
)

linux_dtb = rule(
    implementation = _linux_packaged_output_impl,
    attrs = {
        "extension": attr.string(default = "dtb"),
        "image": attr.label(providers = [LinuxImageInfo], mandatory = True),
        "kind": attr.string(default = "dtb"),
    },
    doc = "Placeholder native device-tree blob action.",
)

linux_install = rule(
    implementation = _linux_packaged_output_impl,
    attrs = {
        "extension": attr.string(default = "install"),
        "image": attr.label(providers = [LinuxImageInfo], mandatory = True),
        "kind": attr.string(default = "install"),
    },
    doc = "Placeholder native install/package output action.",
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
