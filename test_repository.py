"""Check the declared GNU Make export boundary and symbolic source ownership."""

import argparse
import json
import os
import platform
import re
import shutil
import subprocess
import tempfile
from pathlib import Path

from build import assert_self_contained


def nodes(value):
    if isinstance(value, dict):
        if 'kind' in value:
            yield value
        for child in value.values():
            yield from nodes(child)
    elif isinstance(value, list):
        for child in value:
            yield from nodes(child)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--tool', type=Path, required=True)
    parser.add_argument('--source-anchor', type=Path, required=True)
    parser.add_argument('--make', type=Path, required=True)
    parser.add_argument('--strace', action='store_true', help='Require Linux exec tracing for the standalone tool')
    args = parser.parse_args()
    tool = args.tool.resolve()
    make = args.make.resolve()
    architecture = platform.machine()
    if architecture == 'arm64':
        architecture = 'aarch64'
    assert_self_contained(tool, architecture)
    # The launcher ignores ambient Python/compiler settings and runs only its
    # embedded native syntax parser; Make evaluation and recipes are forbidden.
    environment = {'PATH': '', 'CC': '/absent/cc', 'MAKEFLAGS': '--invalid',
                   'PYTHONHOME': '/absent/python', 'PYTHONPATH': '/absent/python'}
    def generate(output: Path, trace: Path | None = None) -> subprocess.CompletedProcess[str]:
        command = [str(tool), '--linux', str(linux), '--out', str(output)]
        if trace is not None:
            tracer = shutil.which('strace')
            assert tracer, 'strace is required for --strace'
            command = [tracer, '-f', '-s', '4096', '-e', 'trace=execve,execveat', '-o', str(trace), *command]
        return subprocess.run(command, env=environment, capture_output=True, text=True)

    linux = args.source_anchor.resolve().parent
    with tempfile.TemporaryDirectory() as temporary:
        output = Path(temporary) / 'export'
        diagnostics = Path(os.environ.get('TEST_UNDECLARED_OUTPUTS_DIR', temporary))
        trace = diagnostics / 'exec.trace' if args.strace else None
        result = generate(output, trace)
        assert result.returncode == 0, result.stderr
        if trace is not None:
            text = trace.read_text()
            paths = re.findall(r'\bexecve\("([^"\n]+)"', text)
            assert len(paths) > 1 and set(paths) == {str(tool)}, paths
            assert 'execveat(' not in text, text
            print(f'PASS: {len(paths)} execs, all of the same static exporter')
        data = json.loads((output / 'report.json').read_text())
        print(f'Kbuild payload: {(output / "report.json").stat().st_size} report bytes; '
              f'{sum(1 for _ in output.iterdir())} files', flush=True)
        assert sorted(p.name for p in output.iterdir()) == ['program.bzl', 'report.json']
        assert (output / 'program.bzl').read_text().startswith('KBUILD = json.decode(')
        program = data['program']
        encoded = json.dumps(program)
        assert 'CONFIG_' in encoded and 'SRCARCH' in encoded
        assert '.generation-' not in encoded and str(linux) not in encoded
        assert data['actions'], 'Native producer catalogue is empty'
        native_initramfs = (linux / 'usr/Makefile').read_text().splitlines()
        for view in ('image_programs', 'image_flag_programs'):
            assignments = [node for node in nodes(program[view]['usr/Makefile'])
                           if node['kind'] in ('assignment', 'define') and node.get('name') == 'ramfs-input']
            assert assignments, view
            for node in assignments:
                assert node['file'] == 'usr/Makefile', node
                assert node['value'] in native_initramfs[node['line'] - 1], node
                expression = json.dumps(node['value_expression'])
                assert '"name": "shell"' not in expression, node
                if '$(shell ' in node['value']:
                    assert '"name": "kbuild_host_shell"' in expression, node
                    assert '"name": "strip"' in expression, node
                    assert '"op": "config", "name": "CONFIG_INITRAMFS_SOURCE"' in expression, node
        original_link = (linux / 'scripts/link-vmlinux.sh').read_text()
        init_goal = 'need-builtin=1' if program['image_script_link'] else 'init/version-timestamp.o'
        init_call = '${MAKE} -f "${srctree}/scripts/Makefile.build" obj=init ' + init_goal
        assert original_link.count(init_call) == 1, init_call
        expected_link = original_link.replace(init_call, ': # init inputs are declared Bazel prerequisites')
        modpost_call = '${MAKE} -f "${srctree}/scripts/Makefile.modpost" MODPOST_VMLINUX=1'
        if program['image_script_link']:
            assert expected_link.count(modpost_call) == 1, modpost_call
            expected_link = expected_link.replace(modpost_call, '__KBUILD_VMLINUX_MODPOST__')
        assert data['link_script'] == expected_link, 'Export changed native link commands outside declared Make boundaries'
        assert '${MAKE}' not in data['link_script'], 'Unowned recursive Make remains in the link script'

        version = program['image_version']
        literals = {'VERSION', 'PATCHLEVEL', 'SUBLEVEL', 'EXTRAVERSION'}
        assert {node.get('name') for node in version if node['kind'] == 'assignment'} >= literals, version
        version_nodes = list(nodes(version))
        version_assignments = [node for node in version_nodes if node['kind'] == 'assignment']
        assert {node['name'] for node in version_assignments} == literals | {'KERNELVERSION'}, version
        assert all(node['file'] == 'Makefile' and not node.get('targets') for node in version_assignments), version
        assert all(node['kind'] in ('assignment', 'conditional') or
                   (node['kind'] == 'directive' and node['directive'] in ('ifeq', 'ifneq', 'ifdef', 'ifndef', 'else'))
                   for node in version_nodes), version
        native_makefile = args.source_anchor.read_text()
        exports = list(nodes(program['image_exports']))
        assert all(any(branch['body'] for branch in node['branches'])
                   for node in exports if node['kind'] == 'conditional'), 'Export slice retained an irrelevant conditional guard'
        if not re.search(r'^\s*(?:export\s+)?ARCH_CORE\s*[:+?]?=', native_makefile, re.MULTILINE):
            descent = list(nodes(program['image_root_descent']))
            assert any(node['kind'] == 'rule' and node['targets'] == 'descend' for node in descent), descent
            assert any(node['kind'] == 'recipe' for node in descent), descent
        if re.search(r'^\s*cmd_ar_vmlinux\.a\s*[:+?]?=', native_makefile, re.MULTILINE):
            assert any(node.get('name') == 'cmd_ar_vmlinux.a' and node['file'] == 'Makefile'
                       for node in nodes(program['image_archive'])), program['image_archive']
        native_object_link = linux / 'scripts/Makefile.vmlinux_o'
        if native_object_link.exists():
            body = native_object_link.read_text()
            link_nodes = list(nodes(program['image_link']['vmlinux_o']))
            initcalls = {'op': 'reference', 'name': [{'op': 'literal', 'value': 'initcalls-lds'}]}
            assert any(node['kind'] == 'rule' and node['targets'] == 'vmlinux.o' and initcalls in node['tail_expression'] for node in link_nodes), link_nodes
            if 'cmd_check_function_names' in body:
                assert 'check_function_names' in program['image_link_postprocess']['ld_vmlinux.o'], program['image_link_postprocess']
                assert any(node.get('name') == 'cmd_check_function_names' for node in link_nodes), link_nodes
        graph = {(node['file'], node['line']) for node in nodes(program['graph_helpers'])
                 if node['kind'] in ('assignment', 'define')}
        helpers = {(node['file'], node['line']) for node in nodes(program['image_helpers'])
                   if node['kind'] in ('assignment', 'define')}
        assert graph and not graph.intersection(helpers), 'Graph transformations were dropped or replayed by compiler helpers'
        assert program['image_modules_order'], 'Native selected-module list writer is missing'
        module_variables = {node['name']: node for node in nodes(program['image_module_variables'])
                            if node['kind'] in ('assignment', 'define')}
        native_modorder = re.search(r'^(?:export\s+)?MODORDER\s*(?::=|=)', (linux / 'Makefile').read_text(), re.MULTILINE)
        assert ('MODORDER' in module_variables) == bool(native_modorder), 'Native MODORDER assignment was lost or invented'
        if 'MODORDER' in module_variables:
            prefix = module_variables['MODORDER']['value'].removesuffix('modules.order')
            assert prefix.startswith('$(') and prefix.endswith(')'), prefix
            assert prefix[2:-1] in module_variables, 'Native module-order prefix assignment is missing'
            assert 'KBUILD_EXTMOD' in module_variables, 'Native external-module selection is missing'
        assert any(node.get('name') == 'cmd_ld_ko_o' for node in nodes(program['image_modfinal'])), 'Native module link command is missing'
        assert program['image_host_objtool_final'] and program['image_host_subcmd_final'], 'Native host final link/archive recipes are missing'
        resolver_makefile = (linux / 'tools/bpf/resolve_btfids/Makefile').read_text()
        resolver_call = program['image_host_btfids_subcmd_call']
        assert resolver_call, 'Native resolver subcmd invocation is missing'
        native_call = re.search(r'^\$\(SUBCMDOBJ\):[^\n]*\n((?:\t[^\n]*\n)+)', resolver_makefile, re.MULTILINE)
        assert native_call is not None, 'Native resolver subcmd rule is missing'
        assert ('install_headers' in json.dumps(resolver_call)) == ('install_headers' in native_call[1]), resolver_call
        assert {'cmd_ld_multi', 'cmd_host_ld_multi'} <= {node.get('name') for node in nodes(program['image_host_link_commands'])}, 'Native target/host aggregate commands are missing'
        if '$(LIBSUBCMD_OUTPUT):' in (linux / 'tools/objtool/Makefile').read_text():
            prerequisites = list(nodes(program['image_host_subcmd_directories']))
            assert any(node['kind'] == 'rule' and node['targets'] == '$(LIBSUBCMD_OUTPUT)' for node in prerequisites), prerequisites
            assert any(node['kind'] == 'recipe' for node in prerequisites), prerequisites
        if program['image_recordmcount'] is not None:
            assert program['image_recordmcount']['excluded'] == 'scripts/mod/empty.o', program['image_recordmcount']
            assert 'record_mcount' in program['image_compile_postprocess']['cc_o_c'], program['image_compile_postprocess']

        host = list(nodes(program['image_host_objtool']))
        pound = [node for node in host if node.get('name') == 'pound']
        assert len(pound) == 1 and pound[0]['value_expression'] == [{'op': 'literal', 'value': '#'}], pound
        elfshdr = [node for node in host if node.get('name') == 'elfshdr']
        assert len(elfshdr) == 1 and 'kbuild_host_shell' in json.dumps(elfshdr), elfshdr
        assert not list(output.parent.glob('.generation-*'))
        sentinel = output / 'keep.txt'
        sentinel.write_text('keep\n')
        result = generate(output)
        assert result.returncode != 0 and 'must be empty' in result.stderr, result.stderr
        assert sentinel.read_text() == 'keep\n'
        result = generate(linux / 'generated')
        assert result.returncode != 0 and 'outside' in result.stderr, result.stderr
        fixture = Path(temporary) / 'Makefile'
        fixture.write_text('VALUE := $(error must not evaluate)\nall:\n\t$(error must not execute)\n')
        events = Path(temporary) / 'events.jsonl'
        result = subprocess.run([str(tool), '_make', '-rR', '-f', str(fixture)],
                                env={'PATH': '', 'LINUX_BZL_MAKE_AST': str(events)},
                                capture_output=True, text=True)
        assert result.returncode == 0, result.stderr
        assert 'must not evaluate' in events.read_text() and 'must not execute' in events.read_text()
        result = subprocess.run([str(tool), '_make', '-f', str(fixture)], env=environment,
                                capture_output=True, text=True)
        assert result.returncode != 0 and 'Only GNU Make syntax export' in result.stderr

        # Exercise the actual parent's recursive producer under parallel Make.
        # Its child owns both files; the parent must dispatch that child once
        # and expose both inputs to rmpiggy.o, even if only relocs is missing.
        realmode = Path(temporary) / 'arch/x86/realmode/rm'
        realmode.mkdir(parents=True)
        child = Path(temporary) / 'child.mk'
        child.write_text(
            'invoked := $(file >>invocations,child)\n'
            '$(directory)/realmode.bin: $(directory)/realmode.relocs\n'
            '\t$(file >$@,binary)\n'
            '$(directory)/realmode.relocs:\n'
            '\t$(file >$@,relocations)\n')
        fixture.write_text(
            'obj := arch/x86/realmode\n'
            'build := directory\n'
            f'MAKE := {make} -rR -f {child}\n'
            f'include {linux}/arch/x86/realmode/Makefile\n'
            '.PHONY: FORCE\nFORCE:\n'
            '$(obj)/rmpiggy.o:\n\t$(file >$@,$^)\n')
        for state in ['missing-both', 'missing-relocations']:
            if state == 'missing-relocations':
                (realmode / 'realmode.relocs').unlink()
                (realmode.parent / 'rmpiggy.o').unlink()
            invocations = Path(temporary) / 'invocations'
            invocations.write_text('')
            result = subprocess.run([str(make), '-rR', '-j8', '-f', str(fixture),
                                     'arch/x86/realmode/rmpiggy.o'], cwd=temporary,
                                    env={'PATH': '', 'SHELL': '/absent/shell'},
                                    capture_output=True, text=True)
            assert result.returncode == 0, (state, result.stdout, result.stderr)
            assert invocations.read_text() == 'child\n', (state, invocations.read_text())
            assert (realmode / 'realmode.bin').read_text() == 'binary\n'
            assert (realmode / 'realmode.relocs').read_text() == 'relocations\n'
            prerequisites = (realmode.parent / 'rmpiggy.o').read_text().split()
            assert prerequisites == ['arch/x86/realmode/rm/realmode.bin',
                                     'arch/x86/realmode/rm/realmode.relocs'], prerequisites
    print('PASS: declared Make, symbolic inputs, host helpers, and source/output guards')


if __name__ == '__main__':
    main()
