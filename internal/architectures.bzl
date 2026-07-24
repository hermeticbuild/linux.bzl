"""Supported Linux architecture descriptors."""

load(":arm64_host_tools.bzl", "linux_arm64_host_tools")
load(":x86_host_tools.bzl", "linux_x86_host_tools")

visibility("//...")

def linux_architectures():
    return [
        struct(
            arch = "x86",
            compact_vars = {
                "ARCH_CORE": "",
                "ARCH_DRIVERS": "arch/x86/pci/ arch/x86/power/",
                "ARCH_LIB": "lib/ arch/x86/lib/",
                "BITS": "64",
                "PROFILING": "",
            },
            compressed_format = "x86_bzimage",
            config_name = "x86_64",
            extension = "bzImage",
            final_suffix = "bzimage",
            host_tools = linux_x86_host_tools,
            platform = Label("@platforms//cpu:x86_64"),
            srcarch = "x86",
            uts_machine = "x86_64",
            vmlinux_format = "x86_64",
            vmlinux_linker_script = "arch/x86/kernel/vmlinux.lds.S",
        ),
        struct(
            arch = "arm64",
            compact_vars = {
                "ARCH_CORE": "",
                "ARCH_DRIVERS": "",
                "ARCH_LIB": "arch/arm64/lib/ lib/",
                "BITS": "64",
                "CC_FLAGS_FPU": "-ffreestanding -D_LINUX_FPU_COMPILATION_UNIT",
                "CC_FLAGS_FTRACE": "",
                "CC_FLAGS_LTO": "",
                "CC_FLAGS_NO_FPU": "-mgeneral-regs-only",
                "CC_FLAGS_SCS": "",
                "DISABLE_KSTACK_ERASE": "",
                "DISABLE_LATENT_ENTROPY_PLUGIN": "",
                "PROFILING": "",
            },
            compressed_format = "arm64_image",
            config_name = "aarch64",
            extension = "Image",
            final_suffix = "image",
            host_tools = linux_arm64_host_tools,
            platform = Label("@platforms//cpu:aarch64"),
            srcarch = "arm64",
            uts_machine = "aarch64",
            vmlinux_format = "arm64",
            vmlinux_linker_script = "arch/arm64/kernel/vmlinux.lds.S",
        ),
    ]
