"""Compare measured Kbuild flags and linked ELF with declared native tools."""

import json
from pathlib import Path
import shlex
import shutil
import subprocess
import sys
import tempfile

from native.configure import check_infrastructure, compiler_wrappers, prepare_sources
from native.spec import mapping, strings, text_field
from probe_runtime import absolute_environment


FLAGS = ("KBUILD_CFLAGS", "KBUILD_AFLAGS", "KBUILD_CPPFLAGS", "KBUILD_LDFLAGS", "KBUILD_HOSTCFLAGS", "KBUILD_HOSTLDFLAGS")


def check(spec: dict[str, object], output: Path) -> None:
    root = Path.cwd()
    output = output.absolute()
    output.mkdir(parents=True, exist_ok=True)
    kernel = root / text_field(spec, "kernel")
    config = root / text_field(spec, "config")
    semantic = mapping(json.loads((config / "values/report.json").read_text()))
    values = strings(semantic["values"])
    observed = mapping(json.loads((kernel / "setup.json").read_text()))
    actual = strings(observed["flags"])
    tools = {name: str(root / path) for name, path in strings(spec["tools"]).items()}
    environment = absolute_environment(strings(spec["environment"]), root)
    environment.update({name: str(root / path) for name, path in strings(spec["path_environment"]).items()})
    driver = root / text_field(spec, "configure")
    environment.update(PATH=str(root / text_field(spec, "bin")), LC_ALL="C", KBUILD_EXEC_ROOT=str(root),
                       PYTHONPATH=str(driver.parent.parent), PYTHONDONTWRITEBYTECODE="1", NATIVE_CONFIG_ERROR=str(output / "infrastructure-error"))
    with tempfile.TemporaryDirectory() as directory:
        work = Path(directory)
        source = work / "source"
        prepare_sources(strings(spec["sources"]), source, tools, driver)
        obj = work / "object"
        for relative in (".config", "include/config/auto.conf", "include/config/auto.conf.cmd", "include/generated/autoconf.h"):
            destination = obj / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(config / relative, destination)
        roles = strings(spec["roles"])
        roles.update({"HOST" + name: path for name, path in strings(spec["host_roles"]).items() if name in {"CC", "CXX"}})
        roles = compiler_wrappers(roles, mapping(spec["compiler"]), mapping(spec["host_compiler"]), tools, work, root)
        roles.update(CONFIG_SHELL=tools["sh"], SHELL=tools["sh"], BASH=tools["bash"], AWK=tools["awk"],
                     PAHOLE=str(root / text_field(spec, "pahole")), ARCH=text_field(spec, "architecture"),
                     LLVM="1" if values.get("CC_IS_CLANG") == "y" else "",
                     LLVM_IAS="1" if values.get("AS_IS_LLVM") == "y" else "0")
        native_path = output / "native-flags.json"
        environment["LINUX_BZL_MAKE_SETUP"] = str(native_path)
        # Setup mode exits after reading and expanding the original Makefiles,
        # before remaking config files or building targets.
        with (output / "native-make.log").open("w") as log:
            subprocess.run([str(root / text_field(spec, "make")), "-rR", "--no-print-directory", "-f", str(source / "Makefile"),
                            *[f"{name}={value}" for name, value in roles.items()], "vmlinux"],
                           cwd=obj, env=environment, stdout=log, stderr=log, check=True)
        check_infrastructure(output)
        native = strings(json.loads(native_path.read_text()))
        prepared = text_field(spec, "kernel").removesuffix(".tree") + ".sources"

        def normalize(value: str) -> list[str]:
            return shlex.split(value.replace(str(source), "{source}").replace(prepared, "{source}"))

        differences = {name: dict(measured=normalize(actual[name]), native=normalize(native[name]))
                       for name in FLAGS if normalize(actual[name]) != normalize(native[name])}
        (output / "flag-differences.json").write_text(json.dumps(differences, indent=2))
        if differences:
            raise AssertionError(f"Native Make disagrees with measured flags: {differences}")
        elf = kernel / "kernel.artifacts/vmlinux"
        header = subprocess.check_output([roles["READELF"], "-h", str(elf)], env=environment, text=True)
        (output / "elf-header.txt").write_text(header)
        machine = "AArch64" if text_field(spec, "architecture") == "arm64" else "Advanced Micro Devices X86-64"
        assert "ELF64" in header and machine in header, header
        symbols = subprocess.check_output([roles["NM"], str(elf)], env=environment, text=True)
        (output / "kernel-symbols.txt").write_text(symbols)
        required = ["start_kernel", "_text"] + (["x86_64_start_kernel"] if machine != "AArch64" else [])
        present = {line.rsplit(None, 1)[-1] for line in symbols.splitlines() if line.strip()}
        assert set(required) <= present, {"missing": sorted(set(required) - present)}
        (output / "summary.json").write_text(json.dumps(dict(global_flag_groups=len(FLAGS), flag_differences=0,
                                                             kernel_bytes=elf.stat().st_size, entry_symbols=required), indent=2))


if __name__ == "__main__":
    match sys.argv[1:]:
        case [spec, output]:
            check(mapping(json.loads(Path(spec).read_text())), Path(output))
        case _:
            raise ValueError("Expected a declared validation spec and output directory")
