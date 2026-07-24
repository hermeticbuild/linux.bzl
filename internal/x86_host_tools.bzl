"""Macros for source-tree-specific x86 Linux host tools."""

load("@bazel_lib//lib:run_binary.bzl", "run_binary")
load("@bazel_skylib//rules:write_file.bzl", "write_file")
load("@rules_cc//cc:defs.bzl", "cc_binary", "cc_library")
load(":common_host_tools.bzl", "linux_common_host_tools")
load(":linux_objects.bzl", "linux_x86_generated_headers")
load(":source_utils.bzl", "package_label", "source_label", "source_labels")

_X86_OBJTOOL_TEXTUAL_SRCS = [
    "tools/arch/x86/lib/inat.c",
    "tools/arch/x86/lib/insn.c",
]

_X86_OBJTOOL_SRCS = [
    "tools/lib/ctype.c",
    "tools/lib/rbtree.c",
    "tools/lib/str_error_r.c",
    "tools/lib/string.c",
    "tools/lib/subcmd/exec-cmd.c",
    "tools/lib/subcmd/help.c",
    "tools/lib/subcmd/pager.c",
    "tools/lib/subcmd/parse-options.c",
    "tools/lib/subcmd/run-command.c",
    "tools/lib/subcmd/sigchain.c",
    "tools/lib/subcmd/subcmd-config.c",
    "tools/objtool/arch/x86/decode.c",
    "tools/objtool/arch/x86/orc.c",
    "tools/objtool/arch/x86/special.c",
    "tools/objtool/builtin-check.c",
    "tools/objtool/check.c",
    "tools/objtool/elf.c",
    "tools/objtool/objtool.c",
    "tools/objtool/orc_dump.c",
    "tools/objtool/orc_gen.c",
    "tools/objtool/special.c",
    "tools/objtool/weak.c",
]

def linux_x86_host_tools(
        name,
        config,
        source_repo,
        target_prefix = None,
        source_root = None,
        source_tree = None,
        env = None,
        visibility = None):
    """Defines the source-tree-specific host tools required by x86 native builds."""
    if env == None:
        env = {
            "ARCH": "x86",
            "SRCARCH": "x86",
        }
    common = linux_common_host_tools(
        name = name,
        env = env,
        source_repo = source_repo,
        source_root = source_root,
        source_tree = source_tree,
        target_prefix = target_prefix,
        visibility = visibility,
    )
    if target_prefix == None:
        target_prefix = name

    generated_headers = target_prefix + "_x86_generated_headers"
    objtool_inat_tables = target_prefix + "_x86_objtool_inat_tables"
    objtool_inputs = target_prefix + "_x86_objtool_inputs"
    objtool = target_prefix + "_x86_objtool"
    relocs_sources = target_prefix + "_x86_relocs_sources"
    relocs_tool = target_prefix + "_x86_relocs_tool"

    linux_x86_generated_headers(
        name = generated_headers,
        asm_offsets_c = source_label(source_repo, "arch/x86/kernel/asm-offsets.c"),
        bounds_c = source_label(source_repo, "kernel/bounds.c"),
        config = config,
        cpufeatures_h = source_label(source_repo, "arch/x86/include/asm/cpufeatures.h"),
        kvm_asm_offsets_c = source_label(source_repo, "arch/x86/kvm/kvm-asm-offsets.c"),
        orc_types_h = source_label(source_repo, "arch/x86/include/asm/orc_types.h"),
        required_features_h = source_label(source_repo, "x86_required_features_h"),
        rq_offsets_c = source_label(source_repo, "rq_offsets_c"),
        source_root = common.source_root,
        source_tree = common.source_tree,
        syscall_32_tbl = source_label(source_repo, "x86_syscall_32_tbl"),
        syscall_64_tbl = source_label(source_repo, "x86_syscall_64_tbl"),
        visibility = visibility,
        xen_interface_headers = source_labels(source_repo, [
            "include/xen/interface/xen-mca.h",
            "include/xen/interface/xen.h",
            "include/xen/interface/xenpmu.h",
        ]),
    )

    genrule_inat_out = target_prefix + "_x86_objtool_inat/arch/x86/lib/inat-tables.c"
    opcode_map = source_label(source_repo, "arch/x86/lib/x86-opcode-map.txt")
    objtool_inat_h = source_label(source_repo, "tools/arch/x86/include/asm/inat.h")
    run_binary(
        name = objtool_inat_tables,
        srcs = [
            objtool_inat_h,
            opcode_map,
        ],
        outs = [genrule_inat_out],
        args = [
            "-in",
            "$(location %s)" % opcode_map,
            "-inat_h",
            "$(location %s)" % objtool_inat_h,
            "-out",
            "$(location %s)" % genrule_inat_out,
        ],
        tool = Label("//tools:insnattr"),
        visibility = visibility,
    )

    cc_library(
        name = objtool_inputs,
        includes = [genrule_inat_out[:-len("/inat-tables.c")]],
        textual_hdrs = [":" + objtool_inat_tables] + source_labels(source_repo, _X86_OBJTOOL_TEXTUAL_SRCS),
        visibility = visibility,
    )

    cc_binary(
        name = objtool,
        srcs = source_labels(source_repo, _X86_OBJTOOL_SRCS),
        copts = [
            "-D_GNU_SOURCE",
            "-Dbswap_16=__bswap_16",
            "-Dbswap_32=__bswap_32",
            "-Dbswap_64=__bswap_64",
            "-Wno-missing-field-initializers",
            "-Wno-nested-externs",
            "-Wno-packed",
            "-Wno-switch-default",
            "-Wno-switch-enum",
            "-Wno-unused-parameter",
        ],
        linkstatic = True,
        visibility = visibility,
        deps = [
            ":" + objtool_inputs,
            Label("@libelf//:elf"),
            source_label(source_repo, "tools_headers_cc"),
        ],
    )

    relocs_32_source = target_prefix + "_x86_relocs_32_source"
    relocs_64_source = target_prefix + "_x86_relocs_64_source"
    relocs_inputs = target_prefix + "_x86_relocs_inputs"

    write_file(
        name = relocs_32_source,
        out = target_prefix + "_x86_relocs_32.c",
        content = [
            "#include \"relocs.h\"",
            "#define ELF_BITS 32",
            "#define ELF_MACHINE EM_386",
            "#define ELF_MACHINE_NAME \"i386\"",
            "#define SHT_REL_TYPE SHT_REL",
            "#define Elf_Rel Elf32_Rel",
            "#define ELF_CLASS ELFCLASS32",
            "#define ELF_R_SYM(val) ELF32_R_SYM(val)",
            "#define ELF_R_TYPE(val) ELF32_R_TYPE(val)",
            "#define ELF_ST_TYPE(o) ELF32_ST_TYPE(o)",
            "#define ELF_ST_BIND(o) ELF32_ST_BIND(o)",
            "#define ELF_ST_VISIBILITY(o) ELF32_ST_VISIBILITY(o)",
            "#ifndef R_386_NONE",
            "#define R_386_NONE 0",
            "#endif",
            "#ifndef R_386_32",
            "#define R_386_32 1",
            "#endif",
            "#ifndef R_386_PC32",
            "#define R_386_PC32 2",
            "#endif",
            "#ifndef R_386_GOT32",
            "#define R_386_GOT32 3",
            "#endif",
            "#ifndef R_386_PLT32",
            "#define R_386_PLT32 4",
            "#endif",
            "#ifndef R_386_COPY",
            "#define R_386_COPY 5",
            "#endif",
            "#ifndef R_386_GLOB_DAT",
            "#define R_386_GLOB_DAT 6",
            "#endif",
            "#ifndef R_386_JMP_SLOT",
            "#define R_386_JMP_SLOT 7",
            "#endif",
            "#ifndef R_386_RELATIVE",
            "#define R_386_RELATIVE 8",
            "#endif",
            "#ifndef R_386_GOTOFF",
            "#define R_386_GOTOFF 9",
            "#endif",
            "#ifndef R_386_GOTPC",
            "#define R_386_GOTPC 10",
            "#endif",
            "#ifndef R_386_16",
            "#define R_386_16 20",
            "#endif",
            "#ifndef R_386_PC16",
            "#define R_386_PC16 21",
            "#endif",
            "#ifndef R_386_8",
            "#define R_386_8 22",
            "#endif",
            "#ifndef R_386_PC8",
            "#define R_386_PC8 23",
            "#endif",
            "#include \"relocs.c\"",
        ],
        visibility = visibility,
    )

    write_file(
        name = relocs_64_source,
        out = target_prefix + "_x86_relocs_64.c",
        content = [
            "#include \"relocs.h\"",
            "#define ELF_BITS 64",
            "#define ELF_MACHINE EM_X86_64",
            "#define ELF_MACHINE_NAME \"x86_64\"",
            "#define SHT_REL_TYPE SHT_RELA",
            "#define Elf_Rel Elf64_Rela",
            "#define ELF_CLASS ELFCLASS64",
            "#define ELF_R_SYM(val) ELF64_R_SYM(val)",
            "#define ELF_R_TYPE(val) ELF64_R_TYPE(val)",
            "#define ELF_ST_TYPE(o) ELF64_ST_TYPE(o)",
            "#define ELF_ST_BIND(o) ELF64_ST_BIND(o)",
            "#define ELF_ST_VISIBILITY(o) ELF64_ST_VISIBILITY(o)",
            "#ifndef R_X86_64_NONE",
            "#define R_X86_64_NONE 0",
            "#endif",
            "#ifndef R_X86_64_64",
            "#define R_X86_64_64 1",
            "#endif",
            "#ifndef R_X86_64_PC32",
            "#define R_X86_64_PC32 2",
            "#endif",
            "#ifndef R_X86_64_GOT32",
            "#define R_X86_64_GOT32 3",
            "#endif",
            "#ifndef R_X86_64_PLT32",
            "#define R_X86_64_PLT32 4",
            "#endif",
            "#ifndef R_X86_64_COPY",
            "#define R_X86_64_COPY 5",
            "#endif",
            "#ifndef R_X86_64_GLOB_DAT",
            "#define R_X86_64_GLOB_DAT 6",
            "#endif",
            "#ifndef R_X86_64_JUMP_SLOT",
            "#define R_X86_64_JUMP_SLOT 7",
            "#endif",
            "#ifndef R_X86_64_RELATIVE",
            "#define R_X86_64_RELATIVE 8",
            "#endif",
            "#ifndef R_X86_64_GOTPCREL",
            "#define R_X86_64_GOTPCREL 9",
            "#endif",
            "#ifndef R_X86_64_32",
            "#define R_X86_64_32 10",
            "#endif",
            "#ifndef R_X86_64_32S",
            "#define R_X86_64_32S 11",
            "#endif",
            "#ifndef R_X86_64_16",
            "#define R_X86_64_16 12",
            "#endif",
            "#ifndef R_X86_64_PC16",
            "#define R_X86_64_PC16 13",
            "#endif",
            "#ifndef R_X86_64_8",
            "#define R_X86_64_8 14",
            "#endif",
            "#ifndef R_X86_64_PC8",
            "#define R_X86_64_PC8 15",
            "#endif",
            "#ifndef R_X86_64_PC64",
            "#define R_X86_64_PC64 24",
            "#endif",
            "#include \"relocs.c\"",
        ],
        visibility = visibility,
    )

    native.filegroup(
        name = relocs_sources,
        srcs = [
            ":" + relocs_32_source,
            ":" + relocs_64_source,
        ],
        visibility = visibility,
    )

    cc_library(
        name = relocs_inputs,
        textual_hdrs = [source_label(source_repo, "arch/x86/tools/relocs.c")],
        visibility = ["//visibility:private"],
        deps = [source_label(source_repo, "tools_headers_cc")],
    )

    cc_binary(
        name = relocs_tool,
        srcs = [
            ":" + relocs_sources,
            source_label(source_repo, "arch/x86/tools/relocs_common.c"),
        ],
        visibility = visibility,
        deps = [
            ":" + relocs_inputs,
            Label("@libelf//:elf_headers"),
            source_label(source_repo, "tools_headers_cc"),
        ],
    )

    return struct(
        asn1_compiler = common.asn1_compiler,
        generated_headers = package_label(generated_headers),
        kallsyms_tool = common.kallsyms_tool,
        objtool = package_label(objtool),
        probe_config = common.probe_config,
        source_asn1_compiler = common.source_asn1_compiler,
        source_label_package = common.source_label_package,
        source_repo = source_repo,
        source_root = common.source_root,
        source_tree = common.source_tree,
        sorttable_tool = common.sorttable_tool,
        x86_relocs_tool = package_label(relocs_tool),
    )
