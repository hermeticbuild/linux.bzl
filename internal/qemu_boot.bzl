"""QEMU system-mode boot test helpers."""

def linux_qemu_boot_test(
        name,
        kernel,
        qemu,
        expect = "Linux version",
        kernel_args = "console=ttyS0 panic=-1",
        qemu_args = [],
        timeout_seconds = 30,
        tags = None,
        **kwargs):
    """Declare a smoke test that boots a kernel under qemu-system.

    Args:
      name: Test target name.
      kernel: Label of a single-file Linux kernel image, such as a bzImage.
      qemu: Label of a qemu-system executable.
      expect: Text that must appear on the serial console before the timeout.
      kernel_args: Kernel command-line arguments passed with `-append`.
      qemu_args: Additional QEMU arguments.
      timeout_seconds: Wall-clock timeout for the boot probe.
      tags: Additional test tags.
      **kwargs: Additional `sh_test` keyword arguments.
    """
    if tags == None:
        tags = []

    # buildifier: disable=native-sh-test
    native.sh_test(
        name = name,
        srcs = ["@linux.bzl//internal:qemu_boot_test.sh"],
        args = [
            "$(location %s)" % qemu,
            "$(location %s)" % kernel,
            str(timeout_seconds),
            expect,
            kernel_args,
        ] + qemu_args,
        data = [
            kernel,
            qemu,
        ],
        tags = tags + ["requires-qemu-system"],
        **kwargs
    )
