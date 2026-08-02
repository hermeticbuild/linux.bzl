"""Canonical platform-derived Linux target profiles."""

visibility("//...")

_PROFILES = [
    struct(
        config_symbol = "CONFIG_X86_64",
        cpu = Label("@platforms//cpu:x86_64"),
        linux_arch = "x86",
        name = "x86_64",
        srcarch = "x86",
        target_triple = "x86_64-linux-gnu",
        uts_machine = "x86_64",
    ),
    struct(
        config_symbol = "CONFIG_ARM64",
        cpu = Label("@platforms//cpu:aarch64"),
        linux_arch = "arm64",
        name = "aarch64",
        srcarch = "arm64",
        target_triple = "aarch64-linux-gnu",
        uts_machine = "aarch64",
    ),
    struct(
        config_symbol = "CONFIG_ARM",
        cpu = Label("@platforms//cpu:armv7"),
        linux_arch = "arm",
        name = "armv7",
        srcarch = "arm",
        target_triple = "arm-linux-gnueabi",
        uts_machine = "armv7l",
    ),
    struct(
        config_symbol = "CONFIG_RISCV",
        cpu = Label("@platforms//cpu:riscv64"),
        linux_arch = "riscv",
        name = "riscv64",
        srcarch = "riscv",
        target_triple = "riscv64-linux-gnu",
        uts_machine = "riscv64",
    ),
    struct(
        config_symbol = "CONFIG_PPC64",
        cpu = Label("@platforms//cpu:ppc64le"),
        linux_arch = "powerpc",
        name = "ppc64le",
        srcarch = "powerpc",
        target_triple = "powerpc64le-linux-gnu",
        uts_machine = "ppc64le",
    ),
]

def linux_architecture_profiles():
    """Returns canonical profiles in stable public-selection order."""
    return list(_PROFILES)

def linux_architecture_profile(name):
    """Returns one canonical profile or fails with the supported set."""
    for profile in _PROFILES:
        if profile.name == name:
            return profile
    fail(
        "unsupported Linux target profile %r; expected one of %s" %
        (name, [profile.name for profile in _PROFILES]),
    )

def linux_architecture_profile_for_arch(linux_arch):
    """Returns the canonical profile for a Linux ARCH value."""
    for profile in _PROFILES:
        if profile.linux_arch == linux_arch:
            return profile
    fail(
        "unsupported Linux ARCH %r; expected one of %s" %
        (linux_arch, [profile.linux_arch for profile in _PROFILES]),
    )

def linux_arch_values():
    return [profile.linux_arch for profile in _PROFILES]
