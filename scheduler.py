"""Preprocess compiler dependencies, execute narrow-input jobs, and export results."""
import json
import os
from pathlib import Path
import posixpath
import shlex
import shutil
import subprocess
import sys
import tempfile
from typing import TypedDict

from execute import execute
from payload import publish


class CompilerDependencies(TypedDict):
    sources: list[str]
    generated: list[str]


class DependencyScan:
    def __init__(self, request):
        self.request = request
        self.depfile: Path | None = None

    def __call__(self, spec, work):
        start = self.request['scan']['argument_start']
        self.depfile = work / '.compiler-dependencies.d'
        flags, skip = [], False
        for flag in spec['arguments'][start:]:
            if skip:
                skip = False
            elif flag in ('-o', '-MF', '-MT', '-MQ'):
                skip = True
            elif flag in ('-c', '-S', '-E', '-MD', '-MMD', '-M', '-MM', '-MP') or flag.startswith(('-Wp,-MD,', '-Wp,-MMD,')):
                continue
            else:
                flags.append(flag)
        return spec['arguments'][:start] + flags + ['-M', '-MF', str(self.depfile), '-MT', 'kernel_dependency_scan']

    def dependencies(self, work: Path) -> CompilerDependencies:
        if self.depfile is None:
            raise ValueError('Compiler dependency scan has not run')
        text = self.depfile.read_text().replace('\\\n', '')
        if ':' not in text:
            raise ValueError('Compiler did not emit a valid dependency file')
        paths = shlex.split(text.split(':', 1)[1])
        prefixes = [self.request['source_prefix'], self.request['original_prefix']]
        scan = self.request['scan']
        generated = scan.get('generated', {})
        header_tree = scan.get('header_tree')
        generated_paths: dict[str, set[str]] = {}
        for logical, path in generated.items():
            if not logical or logical == '.' or Path(logical).is_absolute() or '..' in Path(logical).parts:
                raise ValueError('Invalid generated dependency name: ' + logical)
            for spelling in (path, self.request['aliases'].get(path, path)):
                generated_paths.setdefault(posixpath.normpath(spelling), set()).add(logical)
        source_names: set[str] = set()
        generated_names: set[str] = set()
        for path in paths:
            path = posixpath.normpath(path)
            for root in (work, Path.cwd()):
                if path.startswith(str(root) + '/'):
                    path = path[len(str(root)) + 1:]
                    break
            if header_tree and path.startswith(header_tree.rstrip('/') + '/'):
                logical = path[len(header_tree.rstrip('/')) + 1:]
                if logical not in generated:
                    raise ValueError('Compiler read an undeclared generated dependency: ' + logical)
                generated_names.add(logical)
                continue
            if path in generated_paths:
                generated_names.update(generated_paths[path])
                # A source File can also back a generated-tree alias. Keep
                # its measured source spelling for the compiler's -I paths.
            # The native wrapper also copies the header tree into its working
            # directory. Quoted includes can therefore use its logical names.
            if path in generated:
                generated_names.add(path)
                continue
            for prefix in prefixes:
                needle = prefix.rstrip('/') + '/'
                if path.startswith(needle):
                    relative = path[len(needle):]
                    if '..' in Path(relative).parts or Path(relative).is_absolute():
                        raise ValueError('Invalid source dependency: ' + relative)
                    source_names.add(relative)
                    break
        return dict(sources=sorted(source_names), generated=sorted(generated_names))


def scan(request, dependencies: Path, used_inputs: Path) -> None:
    spec = json.loads(Path(request['manifest']).read_text())
    spec['aliases'] = dict(request['aliases'])
    spec['inputs'] = list(request['aliases'])
    spec['empty_directories'] = request['empty_directories']
    spec['directory_inputs'] = [name for name in spec['directory_inputs'] if name not in request['empty_directories']]
    answer = execute(spec, DependencyScan(request))
    selected = {request['source_prefix'] + '/' + name for name in answer['sources']}
    generated = request['scan'].get('generated', {})
    selected.update(request['aliases'][generated[name]] for name in answer['generated'])
    dependencies.parent.mkdir(parents=True, exist_ok=True)
    dependencies.write_text(json.dumps(answer, separators=(',', ':')) + '\n')
    used_inputs.write_text(''.join(path + '\n' for path in sorted(selected)))


def collect_scans(source: Path, output: Path) -> None:
    """Keep audit answers intact and share logical names in callback metadata."""
    answers: dict[str, CompilerDependencies] = {}
    paths: dict[str, int] = {}
    for path in sorted(source.glob('*/dependencies.json')):
        identity = path.parent.name
        if not identity.isdecimal() or not (path.parent / 'used-inputs').is_file():
            raise ValueError('Invalid dependency scan outputs: ' + str(path))
        match json.loads(path.read_text()):
            case {'sources': list(sources), 'generated': list(generated)} as answer if len(answer) == 2:
                if not all(isinstance(name, str) for name in sources + generated):
                    raise ValueError('Invalid dependency scan names: ' + str(path))
            case _:
                raise ValueError('Invalid dependency scan result: ' + str(path))
        answers[identity] = answer
        indexed = {}
        for kind, names in answer.items():
            indices = []
            for name in names:
                indices.append(paths.setdefault(name, len(paths)))
            indexed[kind] = indices
        publish(output / 'jobs' / identity, indexed)
    publish(output / 'paths', list(paths))
    (output / 'report.json').write_text(json.dumps(answers, indent=2) + '\n')


def run_job(mapping):
    spec = json.loads(Path(mapping['manifest']).read_text())
    aliases = dict(mapping['aliases'])
    aliases.update({name: mapping['aliases'][path] for name, path in spec.get('aliases', {}).items()})
    spec['aliases'] = aliases
    spec['inputs'] = list(aliases) + mapping['source_inputs']
    spec['output_aliases'] = mapping['outputs']
    spec['empty_directories'] = spec.get('empty_directories', []) + mapping.get('empty_directories', [])
    execute(spec)


def export_manifest(source: Path, mappings: Path, output: Path) -> None:
    manifest = json.loads(source.read_text())
    scheduled = json.loads(mappings.read_text())
    paths = scheduled["paths"]

    def artifact(path: str) -> str:
        if path not in paths:
            raise ValueError("Manifest artifact has no scheduled producer/input: " + path)
        return paths[path]

    manifest["config"] = artifact(manifest["config"])
    manifest["config_owner"] = artifact(manifest["config_owner"])
    manifest["setup"] = artifact(manifest["setup"])
    manifest["artifacts"] = {target: artifact(path) for target, path in manifest["artifacts"].items()}
    for record in manifest["compilers"].values():
        for role in ("source", "object", "frontend", "bitcode", "integrated"):
            if role in record:
                record[role] = artifact(record[role])
    for descriptor in manifest["compiler_inventory"].values():
        descriptor["source_file"] = artifact(descriptor["source_file"])
    for member in manifest["members"]:
        member["outputs"] = {target: artifact(path) for target, path in member["outputs"].items()}
    manifest["compiler_diagnostics"] = [artifact(path) for path in manifest.get("compiler_diagnostics", [])]
    manifest["compiler_diagnostic_cases"] = {planned: artifact(path) for planned, path in manifest.get("compiler_diagnostic_cases", {}).items()}
    manifest["native_recipe_checks"] = {role: artifact(path) for role, path in manifest.get("native_recipe_checks", {}).items()}
    manifest["compiler_jobs"] = [
        dict(job["compiler"], source=artifact(job["compiler"]["source"]), outputs=[artifact(path) for path in job["outputs"]])
        for job in scheduled["compiler_jobs"]
    ]
    manifest["compiler_scans"] = scheduled["compiler_scans"]
    output.write_text(json.dumps(manifest, indent=2) + "\n")


def main(arguments):
    if arguments[0] == 'scan':
        scan(json.loads(Path(arguments[1]).read_text()), Path(arguments[2]), Path(arguments[3]))
    elif arguments[0] == 'collect-scans':
        collect_scans(Path(arguments[1]), Path(arguments[2]))
    elif arguments[0] == 'execute':
        run_job(json.loads(Path(arguments[1]).read_text()))
    elif arguments[0] == 'manifest':
        export_manifest(Path(arguments[1]), Path(arguments[2]), Path(arguments[3]))
    elif arguments[0] == 'copy':
        dest = Path(arguments[2])
        dest.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(arguments[1], dest)
    else:
        raise ValueError('Unknown scheduler operation ' + arguments[0])


if __name__ == '__main__':
    main(sys.argv[1:])
