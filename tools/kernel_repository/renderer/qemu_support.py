"""Generate optional QEMU toolchains; kernel compilation never depends on them."""
from pathlib import Path
import shutil

GUESTS = {
    'aarch64': {'system_target': 'aarch64-softmmu', 'machine': 'virt'},
    'armv7': {'system_target': 'arm-softmmu', 'machine': 'mps2-an511'},
    # The 64-bit system emulator also supports the Malta 24Kf CPU fixture.
    'mips32': {'system_target': 'mips64-softmmu', 'machine': 'malta'},
    'ppc32': {'system_target': 'ppc-softmmu', 'machine': 'mac99'},
    'riscv64': {'system_target': 'riscv64-softmmu', 'machine': 'virt'},
    's390x': {'system_target': 's390x-softmmu', 'machine': 's390-ccw-virtio'},
    'x86_64': {'system_target': 'x86_64-softmmu', 'machine': 'pc'},
}

MODULE = '''
bazel_dep(name="rules_qemu", version="0.3.0")
kernel_qemu = use_extension("@rules_qemu//qemu/extension:qemu.bzl", "qemu")
''' + ''.join('kernel_qemu.system_toolchain(system_target=' + repr(spec['system_target']) + ', machine=' + repr(spec['machine']) + ', target_settings=["//qemu_tools:guest_' + guest + '"])\n'
              for guest, spec in GUESTS.items()) + '''use_repo(kernel_qemu, "qemu_system_toolchains", "qemu_user_toolchains")
register_toolchains("@qemu_system_toolchains//:all")
'''



def generate(repo):
    here = Path(__file__).resolve().parent
    shutil.copyfile(here / 'qemu_tools.bzl', repo / 'rules/qemu_tools.bzl')
    (repo / 'rules/qemu_guests.bzl').write_text('QEMU_GUESTS = ' + repr(GUESTS) + '\n')
    (repo / 'qemu_tools').mkdir(exist_ok=True)
    (repo / 'qemu_tools/BUILD.bazel').write_text('load("//rules:qemu_tools.bzl", "qemu_tools")\npackage(default_visibility=["//visibility:public"])\nqemu_tools()\n')
