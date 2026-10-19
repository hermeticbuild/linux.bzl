"""Build the source-selected native conf with declared execution tools."""

import json
from pathlib import Path
import re
import subprocess
import sys
import tempfile

from probe_runtime import absolute_arguments, absolute_environment
from native.spec import mapping, string_list, strings, text_field


def build(spec: dict[str, object], output: Path) -> None:
    execution_root = Path.cwd()
    source = Path(text_field(spec, "source")).absolute()
    tools = {name: str(execution_root / path) for name, path in strings(spec["tools"]).items()}
    environment = absolute_environment(strings(spec["environment"]), execution_root)
    environment.update(
        PATH=str(Path(text_field(spec, "bin")).absolute()), LC_ALL="C", LANG="C",
        KBUILD_EXEC_ROOT=str(execution_root),
    )
    environment.update({name: str(execution_root / path) for name, path in strings(spec["path_environment"]).items()})
    compiler = mapping(spec["compiler"])
    compile_flags = absolute_arguments(string_list(compiler["compile_flags"]), execution_root)
    link_flags = absolute_arguments(string_list(compiler["link_flags"]), execution_root)
    environment.update(absolute_environment(strings(compiler["compile_env"]), execution_root))
    output = output.resolve()
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(dir=output.parent) as temporary:
        work = Path(temporary)
        # Ask GNU Make for the inventory selected by the original Makefile. The
        # target executes no native build recipe or configuration operation.
        inventory = work / "inventory.mk"
        inventory.write_text(
            "include " + str(source / "scripts/kconfig/Makefile") + "\n"
            "__linux_bzl_conf_objects:\n\t@printf '%s\\n' '$(conf-objs)'\n"
        )
        objects = subprocess.check_output(
            [tools["make"], "-rR", "--no-print-directory", "-f", str(inventory),
             "SHELL=" + tools["sh"], "srctree=" + str(source),
             "src=" + str(source / "scripts/kconfig"), "obj=unused", "cross_compiling=1",
             "__linux_bzl_conf_objects"],
            cwd=work, env=environment, text=True,
        ).split()
        if not objects or len(set(objects)) != len(objects) or any(not re.fullmatch(r"[\w.-]+\.o", item) for item in objects):
            raise ValueError(f"Unsupported native conf object inventory: {objects!r}")
        if not {"conf.o", "parser.tab.o", "lexer.lex.o", "preprocess.o"} <= set(objects):
            raise ValueError(f"Native conf lacks its expected entry/parser/preprocessor: {objects!r}")
        subprocess.run([tools["bison"], "-t", "-l", "-o", str(work / "parser.tab.c"),
                        "--defines=" + str(work / "parser.tab.h"), str(source / "scripts/kconfig/parser.y")],
                       env=environment, check=True)
        subprocess.run([tools["flex"], "-L", "-o", str(work / "lexer.lex.c"),
                        str(source / "scripts/kconfig/lexer.l")], env=environment, check=True)
        header = "\n".join(path.read_text() for path in sorted((source / "scripts/kconfig").glob("*.h")))
        match = re.search(r"#define\s+for_all_symbols\s*\(([^)]*)\)", header)
        if match is None:
            raise ValueError("Native Kconfig headers do not define for_all_symbols")
        parameters = [name.strip() for name in match[1].split(",")]
        match parameters:
            case [symbol]:
                iterator, declaration = "for_all_symbols(sym)", ""
            case [index, symbol]:
                iterator, declaration = "for_all_symbols(i, sym)", "int i;"
            case _:
                raise ValueError(f"Unsupported native symbol iterator: {parameters!r}")
        adapter = Path(text_field(spec, "main")).read_text().replace("NATIVE_SYMBOL_ITERATOR_DECL", declaration).replace("NATIVE_FOR_ALL_SYMBOLS", iterator)
        # conf.c remains byte-for-byte native; the adapter only dispatches a
        # read-only inventory mode before its original main.
        (work / "native_conf.c").write_bytes((source / "scripts/kconfig/conf.c").read_bytes())
        (work / "conf.c").write_text(adapter)
        includes = ["-I" + str(source / "scripts/kconfig"), "-I" + str(source / "scripts/include"), "-I" + str(work)]
        compiled = []
        for name in objects + ["native-process.o"]:
            stem = name.removesuffix(".o")
            src = (work if stem in {"conf", "parser.tab", "lexer.lex"} else source / "scripts/kconfig") / (stem + ".c")
            flags = list(compile_flags)
            if stem == "preprocess":
                flags += ["-Dpopen=native_config_popen", "-Dpclose=native_config_pclose"]
            elif stem == "native-process":
                src = Path(text_field(spec, "process")).absolute()
            obj = work / name
            subprocess.run([str(execution_root / text_field(compiler, "cc")), *flags, *includes, "-c", str(src), "-o", str(obj)],
                           env=environment, check=True)
            compiled.append(str(obj))
        link_environment = dict(environment, **absolute_environment(strings(compiler["link_env"]), execution_root))
        subprocess.run([str(execution_root / text_field(compiler, "ld")), *link_flags, *compiled, "-o", str(output)],
                       env=link_environment, check=True)


if __name__ == "__main__":
    build(mapping(json.loads(Path(sys.argv[1]).read_text())), Path(sys.argv[2]))
