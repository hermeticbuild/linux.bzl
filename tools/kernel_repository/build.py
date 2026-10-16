#!/usr/bin/env python3
"""Bootstrap the single-file Linux/x86_64 repository generator.

Host build dependencies: Python3, musl-tools, GCC/binutils, Make, patch, Bison,
Flex. None is required when invoking the resulting executable.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import urllib.request
import zipfile

HERE = Path(__file__).resolve().parent
PYTHON_VERSION = '3.13.9'
PYTHON_SHA256 = 'ed5ef34cda36cfa2f3a340f07cac7e7814f91c7f3c411f6d3562323a866c5c66'
MAKE_SHA256 = 'dd16fb1d67bfab79a72f5e8390735c49e3e8e70b4945a15ab1f81ddb78658fb3'
STATIC_MODULES = '''*static*
array arraymodule.c
_bisect _bisectmodule.c
_contextvars _contextvarsmodule.c
_json _json.c
_random _randommodule.c
_struct _struct.c
math mathmodule.c
binascii binascii.c -UUSE_ZLIB_CRC32
_posixsubprocess _posixsubprocess.c
select selectmodule.c
fcntl fcntlmodule.c
grp grpmodule.c
_md5 md5module.c -I$(srcdir)/Modules/_hacl/include _hacl/Hacl_Hash_MD5.c -D_BSD_SOURCE -D_DEFAULT_SOURCE
_sha1 sha1module.c -I$(srcdir)/Modules/_hacl/include _hacl/Hacl_Hash_SHA1.c -D_BSD_SOURCE -D_DEFAULT_SOURCE
_sha2 sha2module.c -I$(srcdir)/Modules/_hacl/include Modules/_hacl/libHacl_Hash_SHA2.a
_sha3 sha3module.c -I$(srcdir)/Modules/_hacl/include _hacl/Hacl_Hash_SHA3.c -D_BSD_SOURCE -D_DEFAULT_SOURCE
_blake2 _blake2/blake2module.c _blake2/blake2b_impl.c _blake2/blake2s_impl.c
*disabled*
_ssl _hashlib _ctypes _curses _curses_panel _tkinter readline _bz2 _lzma zlib
'''


def run(command, cwd, log):
    with log.open('w') as stream:
        result = subprocess.run([str(x) for x in command], cwd=cwd, stdout=stream, stderr=subprocess.STDOUT)
    if result.returncode:
        raise RuntimeError(f'{command[0]} failed; see {log}\n{log.read_text()[-6000:]}')


def archive(url, path, digest):
    if not path.exists():
        urllib.request.urlretrieve(url, path)
    if hashlib.sha256(path.read_bytes()).hexdigest() != digest:
        raise ValueError(f'Archive checksum mismatch: {path}')
    return path


def namespaced_object(objects, out, namespace, main):
    """Isolate upstream C globals, preserving unresolved libc references."""
    combined = out.with_suffix('.combined.o')
    run(['ld', '-r', '-o', combined, *objects], out.parent, out.with_suffix('.ld.log'))
    symbols = subprocess.check_output(['nm', '--defined-only', '--extern-only', str(combined)], text=True)
    names = sorted({line.split()[-1] for line in symbols.splitlines() if line.split()})
    mapping = out.with_suffix('.symbols')
    mapping.write_text(''.join(n + ' ' + (main if n == 'main' else namespace + n) + '\n' for n in names))
    subprocess.run(['objcopy', '--redefine-syms=' + str(mapping), str(combined), str(out)], check=True)


def build(linux, work, output, jobs):
    work.mkdir(parents=True, exist_ok=True)
    cc = work / 'musl-cc'
    # Debian's musl-gcc delegates --print-multiarch to its glibc GCC. CPython
    # requires this query to match the compiler's actual libc headers.
    cc.write_text('#!/bin/sh\nif [ "$1" = --print-multiarch ]; then\n echo x86_64-linux-musl\nelse\n exec musl-gcc "$@"\nfi\n')
    cc.chmod(0o755)
    source = work / ('Python-' + PYTHON_VERSION)
    if not source.exists():
        path = archive(f'https://www.python.org/ftp/python/{PYTHON_VERSION}/Python-{PYTHON_VERSION}.tar.xz',
                       work / ('Python-' + PYTHON_VERSION + '.tar.xz'), PYTHON_SHA256)
        with tarfile.open(path) as stream:
            stream.extractall(work, filter='data')
    python = work / 'python-build'
    python.mkdir(exist_ok=True)
    if not (python / 'Makefile').exists():
        run(['env', 'CC=' + str(cc), 'LDFLAGS=-static', source / 'configure',
             '--disable-shared', '--without-ensurepip', '--disable-test-modules'], python, python / 'configure.log')
    setup = python / 'Modules/Setup.local'
    if not setup.exists() or setup.read_text() != STATIC_MODULES:
        setup.write_text(STATIC_MODULES)
    print('Building static CPython', flush=True)
    run(['make', '-j' + str(jobs), 'libpython3.13.a', 'Modules/_hacl/libHacl_Hash_SHA2.a'], python, python / 'build.log')

    patch_digest = hashlib.sha256((HERE / 'native/gnu-make-4.4.1.patch').read_bytes()).hexdigest()[:12]
    make_source, make_build = work / ('make-source-' + patch_digest), work / ('make-build-' + patch_digest)
    if not make_source.exists():
        path = archive('https://ftp.gnu.org/gnu/make/make-4.4.1.tar.gz', work / 'make-4.4.1.tar.gz', MAKE_SHA256)
        with tarfile.open(path) as stream:
            stream.extractall(work, filter='data')
        (work / 'make-4.4.1').rename(make_source)
        run(['patch', '-p1', '-i', HERE / 'native/gnu-make-4.4.1.patch'], make_source, work / 'make-patch.log')
    for name in ('ast_export.inc', 'expr_export.inc', 'setup_export.inc'):
        shutil.copyfile(HERE / 'native' / name, make_source / 'src' / name)
    make_build.mkdir(exist_ok=True)
    if not (make_build / 'Makefile').exists():
        run(['env', 'CC=' + str(cc), 'LDFLAGS=-static', make_source / 'configure', '--without-guile', '--disable-nls'],
            make_build, make_build / 'configure.log')
    print('Building native GNU Make parser', flush=True)
    run(['make', '-j' + str(jobs)], make_build, make_build / 'build.log')
    make_object = work / 'make.o'
    namespaced_object([*sorted((make_build / 'src').glob('*.o')), make_build / 'lib/libgnu.a'], make_object,
                      'kernel_make_', 'kernel_make_main')

    sys.path.insert(0, str(HERE / 'renderer'))
    sys.path.insert(0, str(HERE / 'native'))
    from prepare import prepare_sources
    kconfig = work / 'kconfig'
    sources = prepare_sources(linux, kconfig)
    print('Building native Kconfig parser', flush=True)
    objects = []
    for source_file in sources:
        obj = kconfig / (source_file.stem + '.o')
        run([cc, '-O2', '-I', linux / 'scripts/kconfig', '-I', linux / 'scripts/include', '-I', kconfig,
             '-c', source_file, '-o', obj], kconfig, obj.with_suffix('.log'))
        objects.append(obj)
    kconfig_object = work / 'kconfig.o'
    namespaced_object(objects, kconfig_object, 'kernel_kconfig_', 'kernel_kconfig_main')
    identity = {str(p.relative_to(linux)): hashlib.sha256(p.read_bytes()).hexdigest()
                for p in sorted((linux / 'scripts/kconfig').iterdir()) if p.suffix in ('.c', '.h', '.l', '.y')}
    identity.update({str(p.relative_to(linux)): hashlib.sha256(p.read_bytes()).hexdigest()
                     for p in sorted((linux / 'scripts/include').rglob('*.h'))})
    output.parent.mkdir(parents=True, exist_ok=True)
    linked = output.with_name('.' + output.name + '.linking')
    print('Linking one static executable', flush=True)
    run([cc, '-O2', '-static', '-I', python, '-I', work / ('Python-' + PYTHON_VERSION) / 'Include',
         HERE / 'launcher.c', make_object, kconfig_object, python / 'libpython3.13.a', python / 'Modules/_hacl/libHacl_Hash_SHA2.a', '-lm', '-ldl', '-lpthread', '-o', linked],
        work, work / 'link.log')
    subprocess.run(['strip', str(linked)], check=True)
    # ZIP_STORED needs no decompressor/DSO at runtime. CPython imports stdlib
    # straight from this ELF's appended zip. Only renderer data is materialized.
    with zipfile.ZipFile(linked, 'a', compression=zipfile.ZIP_STORED) as bundle:
        for path in sorted((work / ('Python-' + PYTHON_VERSION) / 'Lib').rglob('*.py')):
            relative = path.relative_to(work / ('Python-' + PYTHON_VERSION) / 'Lib')
            if not {'test', 'tests', 'idlelib', 'tkinter', 'ensurepip', '__pycache__'} & set(relative.parts):
                bundle.write(path, relative.as_posix())
        for path in sorted((HERE / 'renderer').iterdir()):
            if path.is_file():
                bundle.write(path, 'resources/renderer/' + path.name)
        bundle.writestr('resources/renderer/parser-sources.json', json.dumps(identity, sort_keys=True))
        bundle.writestr('resources/renderer/tool-versions.json', json.dumps({'python': PYTHON_VERSION, 'make': '4.4.1'}))
        for name, path in [('Linux-GPL-2.0', linux / 'LICENSES/preferred/GPL-2.0'), ('CPython', work / ('Python-' + PYTHON_VERSION) / 'LICENSE'),
                           ('GNU-Make', make_source / 'COPYING'), ('Linux', linux / 'COPYING')]:
            bundle.write(path, 'resources/licenses/' + name)
    dynamic = subprocess.check_output(['readelf', '-l', '-d', str(linked)], text=True)
    if 'INTERP' in dynamic or '(NEEDED)' in dynamic:
        raise RuntimeError('Generator must have neither an ELF interpreter nor dynamic dependencies')
    linked.replace(output)
    print(output)
    print('sha256: ' + hashlib.sha256(output.read_bytes()).hexdigest())


if __name__ == '__main__':
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--linux', type=Path, default=Path('_tmp/linux'))
    p.add_argument('--work', type=Path, default=Path('_tmp/kernel-repository/bootstrap'))
    p.add_argument('--out', type=Path, default=Path('_tmp/kernel-repository/kernel-repository'))
    p.add_argument('--jobs', type=int, default=8)
    args = p.parse_args()
    build(args.linux.resolve(), args.work.resolve(), args.out.resolve(), args.jobs)
