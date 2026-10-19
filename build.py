"""Bazel action helpers for the standalone Kbuild syntax exporter."""

import argparse
import gzip
import io
import json
from pathlib import Path
import re
import shutil
import struct
import subprocess
import tarfile
import tempfile
from typing import Literal, TypedDict
import zipfile


# bootstrap.bzl writes these action specifications.
class NamespaceSpec(TypedDict):
    nm: str
    objcopy: str
    namespace: str
    symbol_prefix: str
    objects: list[tuple[str, str]]


class PayloadSpec(TypedDict):
    output: str
    python_metadata: str
    make_module: str
    stdlib: list[str]
    renderer: list[str]
    license_files: list[str]


class BundleSpec(TypedDict):
    binary: str
    output: str
    payload: str
    python_metadata: str


class ArchiveSpec(TypedDict):
    tool: str
    repository: str
    output: str


def namespace(spec: NamespaceSpec) -> None:
    """Rename parser-owned globals, preserving references to libc."""
    symbols = set()
    for source, _ in spec['objects']:
        result = subprocess.run(
            [spec['nm'], '--defined-only', '--extern-only', '--format=just-symbols', source],
            check=True, capture_output=True, text=True,
        )
        symbols.update(result.stdout.splitlines())
    prefix = spec['symbol_prefix']
    if any(not name.startswith(prefix) for name in symbols):
        raise ValueError(f'Parser symbols do not use the expected ABI prefix {prefix!r}: {sorted(symbols)}')
    mapping = ''.join(f'{name} {prefix}{spec["namespace"]}{name[len(prefix):]}\n' for name in sorted(symbols))
    with tempfile.NamedTemporaryFile(mode='w', dir=Path(spec['objects'][0][1]).parent) as stream:
        stream.write(mapping)
        stream.flush()
        for source, output in spec['objects']:
            subprocess.run([spec['objcopy'], '--redefine-syms=' + stream.name, source, output], check=True)


def assert_self_contained(binary: Path, architecture: str) -> Literal['elf', 'mach-o']:
    """Reject external runtimes, allowing only the macOS system libraries."""
    machines = {'x86_64': (62, 0x01000007), 'aarch64': (183, 0x0100000c)}
    if architecture not in machines:
        raise ValueError(f'Unsupported generator architecture: {architecture}')
    elf_machine, macho_cpu = machines[architecture]
    with binary.open('rb') as stream:
        header = stream.read(64)
        if header[:4] == b'\xcf\xfa\xed\xfe':
            _, cpu, _, kind, count, command_size, _, _ = struct.unpack('<8I', header[:32])
            if cpu != macho_cpu or kind != 2:
                raise ValueError(f'Generator must be an {architecture} Mach-O executable')
            stream.seek(32)
            end = 32 + command_size
            libraries = set()
            signature = None
            for _ in range(count):
                start = stream.tell()
                command, size = struct.unpack('<II', stream.read(8))
                if size < 8 or start + size > end:
                    raise ValueError('Invalid Mach-O load-command bounds')
                data = stream.read(size - 8)
                if command in (0xc, 0x80000018, 0x8000001f, 0x80000023):
                    name_offset = struct.unpack_from('<I', data)[0]
                    libraries.add(data[name_offset - 8:].split(b'\0', 1)[0].decode())
                elif command == 0x8000001c:
                    raise ValueError('Generator must not contain runtime library search paths')
                elif command == 0x1d:
                    signature = struct.unpack('<II', data)
            allowed = {'/usr/lib/libSystem.B.dylib',
                       '/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation'}
            if not libraries <= allowed or '/usr/lib/libSystem.B.dylib' not in libraries:
                raise ValueError(f'Unexpected generator library dependencies: {sorted(libraries)}')
            if signature is None or sum(signature) != binary.stat().st_size:
                raise ValueError('Generator must end with its Mach-O code signature')
            return 'mach-o'
        if header[:6] != b'\x7fELF\x02\x01':
            raise ValueError('Generator must be a supported ELF or Mach-O executable')
        if struct.unpack_from('<H', header, 18)[0] != elf_machine:
            raise ValueError(f'Generator must be an {architecture} ELF executable')
        program_offset = struct.unpack_from('<Q', header, 32)[0]
        entry_size, count = struct.unpack_from('<HH', header, 54)
        for index in range(count):
            stream.seek(program_offset + index * entry_size)
            kind = struct.unpack('<I', stream.read(4))[0]
            if kind in (2, 3):
                raise ValueError('Generator must have neither an interpreter nor dynamic dependencies')
        return 'elf'


def zip_entry(bundle: zipfile.ZipFile, name: str, data: bytes) -> None:
    entry = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
    entry.external_attr = 0o100644 << 16
    bundle.writestr(entry, data)


def payload(spec: PayloadSpec) -> None:
    metadata_path = Path(spec['python_metadata'])
    metadata = json.loads(metadata_path.read_text())
    stdlib = metadata_path.parent / metadata['python_paths']['stdlib']
    make_module = Path(spec['make_module']).read_text()
    match = re.search(r'\bversion\s*=\s*"([^"]+)"', make_module)
    if match is None:
        raise ValueError('GNU Make module does not declare its version')
    versions = {'python': metadata['python_version'], 'make': match[1].split('.bcr.')[0]}
    with zipfile.ZipFile(spec['output'], 'w', compression=zipfile.ZIP_STORED) as archive:
        for path in sorted(map(Path, spec['stdlib'])):
            zip_entry(archive, path.relative_to(stdlib).as_posix(), path.read_bytes())
        for path in sorted(map(Path, spec['renderer'])):
            zip_entry(archive, 'resources/renderer/' + path.name, path.read_bytes())
        zip_entry(archive, 'resources/renderer/tool-versions.json', json.dumps(versions, sort_keys=True).encode())
        for index, path in enumerate(sorted(map(Path, spec['license_files']))):
            zip_entry(archive, f'resources/licenses/{index}-{path.name}', path.read_bytes())


def bundle(spec: BundleSpec) -> None:
    source, output = Path(spec['binary']), Path(spec['output'])
    metadata = json.loads(Path(spec['python_metadata']).read_text())
    architecture = metadata['target_triple'].split('-', 1)[0]
    binary_format = assert_self_contained(source, architecture)
    shutil.copyfile(source, output)
    if binary_format == 'elf':
        with output.open('ab') as destination, Path(spec['payload']).open('rb') as archive:
            shutil.copyfileobj(archive, destination)
    output.chmod(0o755)


def archive(spec: ArchiveSpec) -> None:
    """Package only the generic executable and its repository entry point."""
    repository = Path(spec['repository'])
    repository.mkdir(exist_ok=True)
    shutil.copyfile(spec['tool'], repository / 'kbuild-exporter')
    (repository / 'kbuild-exporter').chmod(0o755)
    (repository / 'BUILD.bazel').write_text('exports_files(["kbuild-exporter"], visibility = ["//visibility:public"])\n')
    (repository / 'MODULE.bazel').write_text('module(name = "kbuild_exporter")\n')
    with Path(spec['output']).open('wb') as raw, gzip.GzipFile(fileobj=raw, mode='wb', filename='', mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode='w') as output:
            for path in sorted(repository.iterdir()):
                data = path.read_bytes()
                entry = tarfile.TarInfo(path.name)
                entry.size = len(data)
                entry.mode = 0o755 if path.name == 'kbuild-exporter' else 0o644
                output.addfile(entry, io.BytesIO(data))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['namespace', 'payload', 'bundle', 'archive'])
    parser.add_argument('specification', type=Path)
    args = parser.parse_args()
    action = {'namespace': namespace, 'payload': payload, 'bundle': bundle, 'archive': archive}[args.action]
    action(json.loads(args.specification.read_text()))
