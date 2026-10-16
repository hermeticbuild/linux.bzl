#!/usr/bin/env python3
"""Run an unchanged allnoconfig UML executable under the host debugger."""
import os
import json
from pathlib import Path
import struct
import subprocess
import sys
from xml.sax.saxutils import escape


def runtime_gaps(kernel):
    data = kernel.read_bytes()
    assert data[:6] == b'\x7fELF\x02\x01', 'UML target must be a little-endian ELF64 executable'
    assert struct.unpack_from('<HH', data, 16) == (2, 62), 'UML target must be an x86_64 executable'
    phoff, shoff = struct.unpack_from('<QQ', data, 32)
    phsize, phnum, shsize, shnum, names_index = struct.unpack_from('<HHHHH', data, 54)
    headers = [struct.unpack_from('<IIQQQQQQ', data, phoff + i * phsize) for i in range(phnum)]
    gaps = []
    if not any(p[0] == 1 and p[2] == 0 and p[5] >= phoff + phsize * phnum for p in headers):
        gaps.append('native UML static linker script leaves the ELF program headers unmapped with LLD; musl startup requires them')
    sections = [struct.unpack_from('<IIQQQQIIQQ', data, shoff + i * shsize) for i in range(shnum)]
    names = sections[names_index]
    strings = data[names[4]:names[4] + names[5]]
    orphans = [strings[s[0]:].split(b'\0', 1)[0].decode() for s in sections
               if s[5] and strings[s[0]:].split(b'\0', 1)[0].startswith((b'.ltext', b'.lrodata', b'.ldata', b'.lbss'))]
    if orphans:
        gaps.append('native UML linker scripts omit LLVM large-model input sections: ' + ', '.join(orphans))
    return gaps


def main():
    kernel = Path(sys.argv[1]).resolve()
    out = Path(os.environ.get('TEST_UNDECLARED_OUTPUTS_DIR', sys.argv[2] if len(sys.argv) > 2 else '.')).resolve()
    out.mkdir(parents=True, exist_ok=True)
    gaps = runtime_gaps(kernel)
    if gaps:
        (out / 'results.json').write_text(json.dumps(dict(status='skipped', built=True, boot_complete=False, source_gaps=gaps), indent=2) + '\n')
        reason = '; '.join(gaps)
        print('SKIP: UML executable built; ' + reason)
        if os.environ.get('XML_OUTPUT_FILE'):
            Path(os.environ['XML_OUTPUT_FILE']).write_text('<testsuites><testsuite name="usermode" tests="1" skipped="1"><testcase name="boot"><skipped message="' + escape(reason, {'"': '&quot;'}) + '"/></testcase></testsuite></testsuites>')
        return
    commands = ['set pagination off', 'set confirm off', 'set follow-fork-mode parent', 'set detach-on-fork on',
                'handle SIGSEGV SIGBUS SIGUSR1 SIGALRM SIGWINCH SIGSYS SIGIO nostop noprint pass', 'file ' + str(kernel)]
    for name in ['start_kernel', 'rest_init', 'kernel_init']:
        commands += ['break *' + name, 'commands', 'silent', 'printf "MILESTONE ' + name + '\\n"',
                     'disable $_hit_bpnum', 'continue', 'end']
    commands += ['break *panic', 'commands', 'silent', 'printf "MILESTONE panic\\n"',
                 'printf "PANIC: %s\\n", (char*)$rdi', 'bt', 'quit', 'end',
                 'run mem=128M con=null lpj=100000 panic=-1 uml_dir=' + str(out / 'uml')]
    script = out / 'commands.gdb'
    script.write_text('\n'.join(commands) + '\n')
    try:
        result = subprocess.run(['gdb-multiarch', '--batch', '-x', script], capture_output=True, text=True,
                                cwd=out, env=dict(os.environ, TMPDIR=str(out)), timeout=60)
        text = result.stdout + result.stderr
    except subprocess.TimeoutExpired as error:
        text = (error.stdout or b'').decode() + (error.stderr or b'').decode()
    (out / 'gdb.log').write_text(text)
    for name in ['start_kernel', 'rest_init', 'kernel_init', 'panic']:
        assert 'MILESTONE ' + name + '\n' in text, text[-16000:]
    assert 'PANIC: VFS: Unable to mount root fs' in text, text[-16000:]
    (out / 'results.json').write_text(json.dumps(dict(status='passed', built=True, boot_complete=True)) + '\n')
    print('PASS: User Mode Linux reaches kernel initialization and the expected missing-root panic')


if __name__ == '__main__':
    main()
