"""Build source-native libbpf with the selected host compiler and declared tools."""

import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile

from native import configure
from native.spec import mapping, string_list, strings, text_field
from probe_runtime import absolute_arguments, absolute_environment


def stage_source(source: Path, destination: Path, tools: dict[str, str]) -> None:
    """Rebind executable scripts without copying the kernel's source/header tree."""
    # These are the native libbpf build and helper directories. Keep all other
    # source subtrees as directory symlinks; native Make still owns its inventory.
    expanded = {"scripts", "tools/scripts", "tools/build", "tools/lib/bpf"}
    parents = {"", "tools", "tools/lib"}
    sources: dict[str, str] = {}

    def visit(relative: str) -> None:
        original = source / relative
        output = destination / relative
        output.mkdir(parents=True, exist_ok=True)
        for entry in original.iterdir():
            name = (Path(relative) / entry.name).as_posix()
            if name in parents or name in expanded:
                visit(name)
            elif any(name.startswith(prefix + "/") for prefix in expanded):
                if entry.is_dir():
                    visit(name)
                else:
                    sources[name] = str(entry)
            else:
                (destination / name).symlink_to(entry, target_is_directory=entry.is_dir())

    visit("")
    configure.prepare_sources(sources, destination, tools, Path(configure.__file__).absolute())


def tool(spec: dict[str, object], role: str, arguments: list[str]) -> int:
    """Preserve provider flags when native Make clears or replaces CFLAGS."""
    roles = strings(spec["roles"])
    environment = dict(os.environ)
    environment.update(strings(spec["compile_env"]))
    flags: list[str] = []
    trailing: list[str] = []
    if role in {"CC", "CXX"}:
        flags = string_list(spec["compile_flags"]) + string_list(spec["library_cflags"])
        # Native feature tests compile and link in one compiler invocation;
        # ordinary objects and preprocessing must not receive linker options.
        compile_only = any(argument in {"-c", "-S", "-E", "-M", "-MM", "--version", "-dumpversion", "-dumpmachine"}
                           or argument.startswith("-print-") for argument in arguments)
        if not compile_only:
            environment.update(strings(spec["link_env"]))
            flags += string_list(spec["link_flags"]) + string_list(spec["library_search"])
            trailing = string_list(spec["library_flags"])
    elif role == "LD":
        environment.update(strings(spec["link_env"]))
    try:
        result = subprocess.run([roles[role], *flags, *arguments, *trailing], env=environment)
    except OSError as error:
        marker = environment.get("KBUILD_PROBE_ERROR") or environment.get("NATIVE_CONFIG_ERROR")
        if marker is None:
            raise RuntimeError("Native tool launch failed without an infrastructure marker") from error
        Path(marker).write_text(f"Cannot execute selected {role}: {error}\n")
        raise
    return result.returncode if result.returncode >= 0 else 128 - result.returncode


def build(spec: dict[str, object], archive: Path, headers: Path) -> None:
    execution_root = Path.cwd()
    original_source = (execution_root / text_field(spec, "source")).absolute()
    tools = {name: str(execution_root / path) for name, path in strings(spec["tools"]).items()}
    compiler = mapping(spec["compiler"])
    roles = {name: str(execution_root / path) for name, path in strings(spec["roles"]).items()}
    library_files = {path: str(execution_root / path) for path in string_list(spec["library_files"])}
    library_flags = [library_files.get(flag, flag) for flag in string_list(spec["library_flags"])]
    # Native libelf/zlib feature recipes append -lelf/-lz themselves. Search
    # only directories containing the declared static archives, then retain
    # the complete ordered CcInfo link closure after the native arguments.
    library_search = list(dict.fromkeys("-L" + str(Path(flag).parent) for flag in library_flags if flag.endswith(".a") and Path(flag).is_absolute()))
    environment = absolute_environment(strings(spec["environment"]), execution_root)
    environment.update({name: str(execution_root / path) for name, path in strings(spec["path_environment"]).items()})
    environment.update(PATH=str(execution_root / text_field(spec, "bin")), LC_ALL="C", LANG="C", TZ="UTC",
                       PYTHONDONTWRITEBYTECODE="1", PYTHONNOUSERSITE="1",
                       PYTHONPATH=str(Path(__file__).absolute().parent.parent), KBUILD_EXEC_ROOT=str(execution_root))
    archive, headers = archive.absolute(), headers.absolute()
    archive.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(dir=archive.parent) as temporary:
        work = Path(temporary)
        source, objects, installed = work / "source", work / "objects", work / "install"
        stage_source(original_source, source, tools)
        objects.mkdir()
        environment.update(TMPDIR=str(work), NATIVE_CONFIG_ERROR=str(work / "infrastructure-error"),
                           KBUILD_PROBE_ERROR=str(work / "infrastructure-error"))
        selected = work / "compiler.json"
        selected.write_text(json.dumps(dict(
            roles=roles,
            compile_flags=absolute_arguments(string_list(compiler["compile_flags"]), execution_root),
            link_flags=absolute_arguments(string_list(compiler["link_flags"]), execution_root),
            compile_env=absolute_environment(strings(compiler["compile_env"]), execution_root),
            link_env=absolute_environment(strings(compiler["link_env"]), execution_root),
            library_cflags=absolute_arguments(string_list(spec["library_cflags"]), execution_root),
            library_flags=library_flags, library_search=library_search,
        )))
        commands = {}
        for role in roles:
            wrapper = work / role.lower()
            configure.write_executable(wrapper, "#!" + tools["sh"] + "\nexec " + shlex.join([
                tools["python3"], "-B", str(Path(__file__).absolute()), "tool", str(selected), role,
            ]) + ' "$@"\n')
            commands[role] = str(wrapper)
        commands.update({"HOST" + name: commands[name] for name in ("CC", "CXX", "LD", "AR")})
        commands.update(CONFIG_SHELL=tools["sh"], BASH=tools["bash"], PYTHON3=tools["python3"],
                        PYTHON=tools["python3"], AWK=tools["awk"],
                        PKG_CONFIG=shlex.join([tools["sh"], str(execution_root / text_field(spec, "package_command"))]))
        commands["HOSTPKG_CONFIG"] = commands["PKG_CONFIG"]
        environment.update(commands)
        # libbpf deliberately clears MAKEOVERRIDES. Bind the selected shell on
        # every recursive Make, including feature checks and fixdep, without
        # overriding the native CFLAGS or supplying any feature result.
        make = work / "make"
        configure.write_executable(make, "#!" + tools["sh"] + "\nexec " + shlex.join([
            tools["make"], "SHELL=" + tools["sh"], "MAKE=" + str(make),
        ]) + ' "$@"\n')
        command = [str(make), "--no-print-directory", "-j1", *[f"{name}={value}" for name, value in commands.items()],
                   "srctree=" + str(source), "OUTPUT=" + str(objects) + "/", "DESTDIR=" + str(installed), "prefix=", "V=1"]
        # Old libbpf's exact archive target does not order fixdep itself.
        for goals in [["fixdep"], [str(objects / "libbpf.a"), "install_headers"]]:
            try:
                subprocess.run([*command, *goals], cwd=source / "tools/lib/bpf", env=environment, check=True)
            except subprocess.CalledProcessError:
                # Native feature checks hide compiler diagnostics in these
                # files. Preserve them before the private build tree is removed.
                for diagnostic in sorted((objects / "feature").glob("*.make.output")):
                    print(f"Native feature diagnostics: {diagnostic.name}", file=sys.stderr, flush=True)
                    sys.stderr.buffer.write(diagnostic.read_bytes())
                    sys.stderr.flush()
                raise
            configure.check_infrastructure(work)
        for path in [objects / "libbpf.a", *[installed / "include/bpf" / name for name in ("bpf.h", "libbpf.h", "btf.h")]]:
            if not path.is_file():
                raise ValueError(f"Native libbpf build/install did not produce {path}")
        # Publish only after the complete native build/install succeeds. The
        # scheduler transports the public header directory as one output.
        headers.parent.mkdir(parents=True, exist_ok=True)
        (objects / "libbpf.a").replace(archive)
        installed.replace(headers)


if __name__ == "__main__":
    match sys.argv[1:]:
        case ["tool", spec, role, *arguments]:
            sys.exit(tool(mapping(json.loads(Path(spec).read_text())), role, arguments))
        case [spec, archive, headers]:
            build(mapping(json.loads(Path(spec).read_text())), Path(archive), Path(headers))
        case _:
            raise ValueError(f"Unsupported libbpf builder arguments: {sys.argv[1:]!r}")
