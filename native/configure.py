"""Run source-selected native Kconfig with declared compiler and process tools."""

import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import time

from native.spec import mapping, string_list, strings, text_field


def probe(arguments: list[str]) -> int:
    """Keep native shell status and pipeline semantics; report adapter failures."""
    try:
        result = subprocess.run([os.environ["NATIVE_CONFIG_SHELL"], *arguments], capture_output=True)
        with Path(os.environ["NATIVE_CONFIG_TRACE"]).open("a") as stream:
            stream.write(json.dumps(dict(arguments=arguments, directory=os.getcwd(), status=result.returncode,
                                         stdout=result.stdout.hex(), stderr=result.stderr.hex())) + "\n")
        sys.stdout.buffer.write(result.stdout)
        sys.stderr.buffer.write(result.stderr)
        return result.returncode if result.returncode >= 0 else 128 - result.returncode
    except (OSError, KeyError) as error:
        Path(os.environ["NATIVE_CONFIG_ERROR"]).write_text(str(error))
        raise


def merge_fragments(fragments: list[str], requested: dict[str, str]) -> str:
    """Apply native merge_config ordering before conf performs any resolution."""
    assignments: dict[str, str] = {}
    pattern = re.compile(r"(?:CONFIG_([A-Za-z0-9_]+)=.*|# CONFIG_([A-Za-z0-9_]+) is not set)")
    for content in fragments:
        for line in content.splitlines():
            if match := pattern.fullmatch(line):
                name = match[1] or match[2]
                assignments.pop(name, None)
                assignments[name] = line
            elif line.strip() and not line.startswith("#"):
                raise ValueError(f"Invalid configuration fragment line: {line!r}")
    for name, value in requested.items():
        if not re.fullmatch(r"[A-Za-z0-9_]+", name) or "\n" in value:
            raise ValueError(f"Invalid requested configuration: {name}={value!r}")
        assignments.pop(name, None)
        assignments[name] = f"# CONFIG_{name} is not set" if value == "n" else f"CONFIG_{name}={value}"
    return "".join(line + "\n" for line in assignments.values())


def write_executable(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content)
    path.chmod(0o755)


def compiler_wrappers(roles: dict[str, str], target: dict[str, object], host: dict[str, object],
                      tools: dict[str, str], work: Path, execution_root: Path) -> dict[str, str]:
    """Bind each declared compiler role to the selected flags and environment."""
    from probe_runtime import absolute_arguments, absolute_environment

    wrappers = {}
    for role, path in roles.items():
        context = host if role.startswith("HOST") else target
        command = [str(execution_root / path)]
        if role in {"CC", "HOSTCC"}:
            command += absolute_arguments(string_list(context["compile_flags"]), execution_root)
        tool_environment = absolute_environment(strings(context["compile_env"]), execution_root)
        wrapper = work / "bin" / role.lower()
        write_executable(wrapper, "#!" + tools["sh"] + "\nexec " + shlex.join([
            tools["env"], *[f"{key}={value}" for key, value in tool_environment.items()], *command,
        ]) + ' "$@"\n')
        wrappers[role] = str(wrapper)
    return wrappers


def check_infrastructure(output: Path) -> None:
    if (error := output / "infrastructure-error").exists():
        raise RuntimeError("Native probe infrastructure failed: " + error.read_text())


def prepare_sources(sources: dict[str, str], root: Path, tools: dict[str, str], driver: Path) -> dict[str, str]:
    """Preserve source bytes apart from binding executable shebangs to tools."""
    identities = {}
    for logical, original in sources.items():
        source = Path(original).absolute()
        destination = root / logical
        destination.parent.mkdir(parents=True, exist_ok=True)
        with source.open("rb") as stream:
            first = stream.readline(512)
        if first.startswith(b"#!"):
            data = source.read_bytes()
            identities[logical] = hashlib.sha256(data).hexdigest()
            words = shlex.split(first[2:].decode().strip())
            if words and Path(words[0]).name == "env":
                words = words[1:]
            match words:
                case [command, *flags] if Path(command).name in tools and len(flags) <= 1:
                    interpreter = tools[Path(command).name]
                    destination.write_bytes(("#!" + interpreter + (" " + flags[0] if flags else "") + "\n").encode() + data[len(first):])
                    destination.chmod(source.stat().st_mode & 0o777)
                case _:
                    # Unsupported source interpreters must fail the action even
                    # if native Kconfig treats a failed shell probe as false.
                    write_executable(destination, "#!" + tools["sh"] + "\nexec " + shlex.join([
                        tools["python3"], "-B", str(driver), "unsupported-interpreter", logical,
                        first.decode().strip(),
                    ]) + "\n")
        else:
            destination.symlink_to(source)
            if logical == "Makefile" or "Kconfig" in source.name:
                identities[logical] = hashlib.sha256(source.read_bytes()).hexdigest()
    return identities


def configure(spec: dict[str, object], output: Path, measured: dict[str, object]) -> None:
    from probe_runtime import absolute_environment, publish

    started = time.monotonic()
    timings = {}
    execution_root = Path.cwd()
    output = output.absolute()
    output.mkdir(parents=True, exist_ok=True)
    tools = {name: str(execution_root / path) for name, path in strings(spec["tools"]).items()}
    driver = Path(__file__).absolute()
    environment = absolute_environment(strings(spec["environment"]), execution_root)
    environment.update({name: str(execution_root / path) for name, path in strings(spec["path_environment"]).items()})
    environment.update(PATH=str(execution_root / text_field(spec, "bin")), LC_ALL="C", LANG="C",
                       PYTHONDONTWRITEBYTECODE="1", PYTHONNOUSERSITE="1",
                       PYTHONPATH=str(driver.parent.parent), KBUILD_EXEC_ROOT=str(execution_root),
                       NATIVE_CONFIG_PYTHON=tools["python3"], NATIVE_CONFIG_RUNNER=str(driver),
                       NATIVE_CONFIG_SHELL=tools["sh"], NATIVE_CONFIG_TRACE=str(output / "probes.jsonl"),
                       NATIVE_CONFIG_ERROR=str(output / "infrastructure-error"))
    mode = text_field(spec, "mode")
    if mode not in {"allnoconfig", "olddefconfig"}:
        raise ValueError(f"Unsupported native configuration mode: {mode}")
    with tempfile.TemporaryDirectory(dir=output.parent) as temporary:
        work = Path(temporary)
        source = work / "source"
        provenance = prepare_sources(strings(spec["sources"]), source, tools, driver)
        timings["prepare_sources"] = time.monotonic() - started
        declared_roles = strings(spec["roles"])
        declared_roles.update({"HOST" + name: path for name, path in strings(spec["host_roles"]).items() if name in {"CC", "CXX"}})
        roles = compiler_wrappers(declared_roles, mapping(spec["compiler"]), mapping(spec["host_compiler"]), tools, work, execution_root)
        roles.update(CONFIG_SHELL=tools["sh"], BASH=tools["bash"], PYTHON3=tools["python3"], AWK=tools["awk"],
                     PAHOLE=str(execution_root / text_field(spec, "pahole")))
        roles.update(strings(spec["architecture"]))
        roles["LLVM_IAS"] = text_field(spec, "llvm_ias")
        shell = work / "make-shell"
        write_executable(shell, "#!" + tools["sh"] + "\n"
                         "exec " + shlex.join([tools["python3"], "-B", str(driver), "shell"]) + ' "$@"\n')
        query = work / "environment.mk"
        captured = work / "environment.json"
        query.write_text(".PHONY: native-config-environment\nnative-config-environment:\n\t@" +
                         shlex.join([tools["python3"], "-B", str(driver), "environment", str(captured)]) + "\n")
        assignments = [f"{name}={value}" for name, value in roles.items()]
        for makefile in [source / "Makefile", source / "scripts/kconfig/Makefile"]:
            begin = time.monotonic()
            subprocess.run([tools["make"], "-rR", "--no-print-directory", "-f", str(makefile), "-f", str(query),
                            "MAKECMDGOALS=" + mode, "SHELL=" + str(shell), *assignments,
                            "native-config-environment"], cwd=source, env=environment, check=True)
            check_infrastructure(output)
            environment = strings(json.loads(captured.read_text()))
            timings[makefile.relative_to(source).as_posix()] = time.monotonic() - begin
        environment.pop("MAKEFLAGS", None)
        environment.pop("MAKELEVEL", None)
        environment["srctree"] = str(source)
        seed = output / "requested.config"
        seed.write_text(merge_fragments([Path(path).read_text() for path in string_list(spec["fragments"])], strings(spec["requested"])))
        environment["KCONFIG_CONFIG"] = str(output / ".config")
        if mode == "allnoconfig":
            environment["KCONFIG_ALLCONFIG"] = str(seed)
        else:
            shutil.copyfile(seed, output / ".config")
        entry = source / "Kconfig"
        if measured or spec["extension"]:
            additions = work / "measured.Kconfig"
            declarations = []
            for name, value in measured.items():
                if not re.fullmatch(r"[A-Za-z0-9_]+", name):
                    raise ValueError(f"Invalid measured Kconfig name: {name!r}")
                match value:
                    case bool():
                        kind, default = "bool", "y" if value else "n"
                    case int():
                        kind, default = "int", str(value)
                    case str():
                        kind, default = "string", json.dumps(value)
                    case _:
                        raise ValueError(f"Unsupported measured Kconfig value: {value!r}")
                declarations.append(f"config {name}\n\t{kind}\n\tdefault {default}\n")
            additions.write_text("\n".join(declarations))
            entry = work / "Kconfig"
            paths = [additions]
            if extension := spec["extension"]:
                if not isinstance(extension, str):
                    raise ValueError(f"Invalid Kconfig extension: {extension!r}")
                paths.append(execution_root / extension)
            # Native grammar permits mainmenu only at the start of its root
            # input. Preserve that input before appending optional sources;
            # sourcing the root from a wrapper makes its mainmenu invalid.
            entry.write_bytes((source / "Kconfig").read_bytes() + b"\n" +
                              "".join("source " + json.dumps(str(path)) + "\n" for path in paths).encode())
        conf = str(execution_root / text_field(spec, "conf"))
        for option in ["--" + mode, "--syncconfig"]:
            begin = time.monotonic()
            subprocess.run([conf, option, str(entry)], cwd=output, env=environment, check=True)
            timings[option] = time.monotonic() - begin
            check_infrastructure(output)
        begin = time.monotonic()
        inventory = subprocess.check_output([conf, "--native-values", str(entry), str(output / ".config")],
                                            cwd=output, env=environment, text=True)
        timings["--native-values"] = time.monotonic() - begin
        check_infrastructure(output)
        symbols = json.loads(inventory)
        values = {name: record["value"] for name, record in symbols.items()}
        # Kbuild reads auto.conf as Make syntax. In particular, old native
        # serializers retain quotes around strings; semantic inventory is not
        # a substitute for this variable representation.
        auto_conf = output / "include/config/auto.conf"
        make_names = [match[1] for line in auto_conf.read_text().splitlines()
                      if (match := re.match(r"^(CONFIG_[A-Za-z0-9_]+)=", line))]
        query.write_text("include " + str(auto_conf) + "\nexport " + " ".join(make_names) + "\n"
                         ".PHONY: native-config-environment\nnative-config-environment:\n\t@" +
                         shlex.join([tools["python3"], "-B", str(driver), "environment", str(captured)]) + "\n")
        subprocess.run([tools["make"], "-rR", "--no-print-directory", "-f", str(query),
                        "SHELL=" + str(shell), "native-config-environment"], cwd=output, env=environment, check=True)
        check_infrastructure(output)
        exported = strings(json.loads(captured.read_text()))
        make_values = {name: exported[name] for name in make_names}
        report = dict(values=values, symbols=symbols, mode=mode, requested=seed.read_text(),
                      compiler=spec["compiler"], compiler_identity=spec["compiler_identity"], roles=spec["roles"], execution_environment=environment,
                      measured=measured, source=spec["source_label"], source_hashes=provenance, timings=timings)
        (output / "config-report.json").write_text(json.dumps(report, indent=2))
        # The callback reads native bytes and resolved values; it never solves
        # a second configuration or reconstructs auto.conf serialization.
        publish(output / "values", dict(values=values, symbols=symbols, make_values=make_values, config=(output / ".config").read_text(),
                                       header=(output / "include/generated/autoconf.h").read_text(),
                                       auto_conf=(output / "include/config/auto.conf").read_text()))


if __name__ == "__main__":
    match sys.argv[1:]:
        case ["probe", command]:
            sys.exit(probe(["-c", command]))
        case ["shell", *arguments]:
            sys.exit(probe(arguments))
        case ["environment", path]:
            Path(path).write_text(json.dumps(dict(os.environ)))
        case ["unsupported-interpreter", source, interpreter]:
            message = f"Source {source} requires undeclared interpreter {interpreter}"
            Path(os.environ["NATIVE_CONFIG_ERROR"]).write_text(message)
            raise RuntimeError(message)
        case [spec, output]:
            configure(mapping(json.loads(Path(spec).read_text())), Path(output), {})
        case [spec, output, "--measured", measured]:
            configure(mapping(json.loads(Path(spec).read_text())), Path(output), mapping(json.loads(Path(measured).read_text())))
        case _:
            raise ValueError(f"Unsupported native config arguments: {sys.argv[1:]!r}")
