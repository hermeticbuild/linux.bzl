"""Architecture-specific ELF conventions for native Linux links."""

visibility("//...")

_SPECS = {
    "arm": struct(
        direct_lld = True,
        emulation = "armelf_linux_eabi",
        endian_flags = ["-EL"],
        relocatable_flags = [],
        vmlinux_flags = [
            "--no-undefined",
            "-X",
            "--pic-veneer",
            "-z",
            "norelro",
        ],
    ),
    "arm64": struct(
        direct_lld = True,
        emulation = "aarch64elf",
        endian_flags = ["-EL"],
        relocatable_flags = [],
        vmlinux_flags = [
            "--no-undefined",
            "-X",
            "--pic-veneer",
            "-z",
            "norelro",
        ],
    ),
    "powerpc": struct(
        direct_lld = True,
        emulation = "elf64lppc",
        endian_flags = ["-EL"],
        relocatable_flags = [],
        vmlinux_flags = ["-Bstatic"],
    ),
    "riscv": struct(
        direct_lld = True,
        emulation = "elf64lriscv",
        endian_flags = [],
        relocatable_flags = [],
        vmlinux_flags = ["-z", "norelro"],
    ),
    "x86": struct(
        direct_lld = False,
        emulation = "elf_x86_64",
        endian_flags = [],
        relocatable_flags = [],
        vmlinux_flags = ["-z", "max-page-size=0x200000"],
    ),
}

def linux_vmlinux_link_spec(arch):
    """Returns the ELF link convention for a Linux ARCH value."""
    if arch not in _SPECS:
        fail(
            "unsupported Linux vmlinux link architecture %r; expected one of %s" %
            (arch, sorted(_SPECS.keys())),
        )
    return _SPECS[arch]
