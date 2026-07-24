"""Public providers returned by Bazel-native Linux build rules."""

visibility("//...")

LinuxKernelInfo = provider(
    doc = "Outputs and metadata for one configured Linux kernel.",
    fields = {
        "arch": "Canonical Linux target architecture: x86_64 or aarch64.",
        "version": "Upstream Linux source version.",
        "kernel_release": "File containing the resolved kernel release.",
        "image": "Architecture boot image File.",
        "vmlinux": "Uncompressed vmlinux File.",
        "config": "Resolved kernel configuration File.",
        "system_map": "System.map File.",
    },
)
