"""Behavioral checks for artifact transport used by the kernel action adapter."""
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from urllib.parse import unquote

from payload import chunk_path, publish
from scheduler import collect_scans, DependencyScan

EXECUTOR=Path(__file__).absolute().parent/'execute.py'
SCHEDULER = Path(__file__).absolute().parent / 'scheduler.py'

class ExecuteTest(unittest.TestCase):
 def run_action(self,root,inputs,outputs,program,directories=(),directory_outputs=(),tool_runfiles=(),path_environment=None):
  manifest=root/'action.json'
  manifest.write_text(json.dumps(dict(executable=sys.executable,arguments=['-c',program],inputs=inputs,outputs=outputs,
      directory_inputs=list(directories),directory_outputs=list(directory_outputs),environment={},
      tool_runfiles=list(tool_runfiles),path_environment=path_environment or {})))
  return subprocess.run([sys.executable,str(EXECUTOR),str(manifest)],cwd=root,capture_output=True,text=True)

 def test_regular_declared_input_and_output(self):
  with tempfile.TemporaryDirectory() as tmp:
   root=Path(tmp);(root/'input').write_text('kernel')
   result=self.run_action(root,['input'],['out/result'],"from pathlib import Path; Path('out/result').write_text(Path('input').read_text()+'-built')")
   self.assertEqual(result.returncode,0,result.stderr)
   self.assertEqual((root/'out/result').read_text(),'kernel-built')

 def test_directory_intermediate_round_trip(self):
  with tempfile.TemporaryDirectory() as tmp:
   root=Path(tmp)
   result=self.run_action(root,[],['headers'],"from pathlib import Path; Path('headers').mkdir(); Path('headers/config.h').write_text('#define CONFIG_TEST 1')",directory_outputs=['headers'])
   self.assertEqual(result.returncode,0,result.stderr)
   result=self.run_action(root,['headers'],['result'],"from pathlib import Path; Path('result').write_text(Path('headers/config.h').read_text())",directories=['headers'])
   self.assertEqual(result.returncode,0,result.stderr)
   self.assertEqual((root/'result').read_text(),'#define CONFIG_TEST 1')

 def test_directory_transport_ignores_timestamps(self):
  with tempfile.TemporaryDirectory() as tmp:
   root=Path(tmp)
   outputs=[]
   for stamp in [100,200]:
    result=self.run_action(root,[],['headers'],"from pathlib import Path; import os; Path('headers').mkdir(); Path('headers/config.h').write_text('constant'); os.utime('headers/config.h', (%d,%d))"%(stamp,stamp),directory_outputs=['headers'])
    self.assertEqual(result.returncode,0,result.stderr)
    outputs.append((root/'headers').read_bytes())
   self.assertEqual(outputs[0],outputs[1])

 def test_failed_command_is_a_build_failure(self):
  with tempfile.TemporaryDirectory() as tmp:
   result=self.run_action(Path(tmp),[],['result'],'raise SystemExit(42)')
   self.assertNotEqual(result.returncode,0)

 def test_provider_runfiles_aliases_survive_private_staging(self):
  with tempfile.TemporaryDirectory() as tmp:
   root=Path(tmp)
   (root/'data').write_text('declared tool data')
   runfiles=root/'bin/tool.runfiles/owner'
   runfiles.mkdir(parents=True)
   (runfiles/'alias').symlink_to(root/'data')
   result=self.run_action(root,['data'],['result'],
       "from pathlib import Path; import os; Path('result').write_text(Path(os.environ['TOOL_DATA']).read_text())",
       tool_runfiles=['bin/tool.runfiles'],path_environment={'TOOL_DATA':'bin/tool.runfiles/owner/alias'})
   self.assertEqual(result.returncode,0,result.stderr)
   self.assertEqual((root/'result').read_text(),'declared tool data')

 def test_missing_provider_runfiles_is_an_action_failure(self):
  with tempfile.TemporaryDirectory() as tmp:
   result=self.run_action(Path(tmp),[],['result'],"raise AssertionError('must not execute')",tool_runfiles=['bin/tool.runfiles'])
   self.assertNotEqual(result.returncode,0)
   self.assertIn('Declared tool runfiles directory is missing: bin/tool.runfiles',result.stderr)


class PayloadTest(unittest.TestCase):
    def test_nested_chunks_preserve_full_payload_and_bound_directory_fanout(self):
        value = dict(
            capabilities={'native': True, 'empty': '', 'unicode': 'λ', 'escapes': '/\\%2F%5C%25'},
            history=[[dict(argv=['compiler', 'x' * 20_000], status=1, stdout='first\nsecond\n', stderr='warning')]],
        )
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            publish(root, value)
            chunks = []
            for name, directories, files in os.walk(root):
                directory = Path(name)
                self.assertLessEqual(len(directories) + len(files), 101 if directory == root else 100)
                chunks.extend(directory / name for name in files if '=' in name)
            self.assertGreater(len(chunks), 100)
            self.assertTrue(all(path.parent != root for path in chunks))
            self.assertTrue(all(len(path.name.encode()) <= 189 and '\\' not in path.name for path in chunks))
            encoded = ''.join(path.name.split('=', 1)[1] for path in sorted(chunks, key=lambda path: path.name))
            self.assertEqual(json.loads(unquote(encoded)), value)
            self.assertEqual(json.loads((root / 'report.json').read_text()), value)

    def test_chunk_index_boundary_rejects_overflow(self):
        self.assertEqual(chunk_path(99_999_999, 'ab'), Path('99/99/99/99999999=ab'))
        for index in [-1, 100_000_000]:
            with self.subTest(index=index), self.assertRaisesRegex(ValueError, 'eight-digit'):
                chunk_path(index, 'ab')


class CompilerDependenciesTest(unittest.TestCase):
    def test_scan_reuses_declared_source_namespace_and_reports_used_files(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / 'prepared/include').mkdir(parents=True)
            (root / 'prepared/main.c').write_text('source')
            (root / 'prepared/include/chosen.h').write_text('prepared header')
            (root / 'prepared/include/unused.h').write_text('unused')
            (root / 'stage').mkdir()
            (root / 'stage/generated.h').write_text('generated header')
            program = '\n'.join([
                'from pathlib import Path',
                'import sys',
                "assert Path('original/include/chosen.h').read_text() == 'prepared header'",
                "assert Path('prepared/include/chosen.h').read_text() == 'prepared header'",
                "assert Path('plan/headers/generated.h').read_text() == 'generated header'",
                "assert Path('plan/headers/main.c').read_text() == 'source'",
                "Path(sys.argv[sys.argv.index('-MF') + 1]).write_text('scan: original/main.c prepared/include/chosen.h plan/headers/generated.h\\n')",
            ])
            manifest = root / 'compile.json'
            manifest.write_text(json.dumps(dict(
                executable=sys.executable, arguments=['-c', program, 'scan'],
                inputs=['prepared', 'plan/headers'], outputs=['plan/result'],
                directory_inputs=['plan/headers'], directory_outputs=[], environment={},
            )))
            request = root / 'request.json'
            request.write_text(json.dumps(dict(
                manifest=str(manifest), source_prefix='prepared', original_prefix='original',
                aliases={'prepared': 'prepared', 'original': 'prepared',
                         'original/main.c': 'prepared/main.c',
                         'plan/headers/main.c': 'prepared/main.c',
                         'plan/generated.h': 'stage/generated.h',
                         'plan/headers/generated.h': 'stage/generated.h'},
                empty_directories=['plan/headers'],
                scan=dict(argument_start=3, header_tree='plan/headers', generated={'generated.h': 'plan/generated.h', 'main.c': 'original/main.c'}, mandatory=['original/main.c']),
            )))
            result = subprocess.run(
                [sys.executable, str(SCHEDULER), 'scan', str(request), 'outputs/dependencies.json', 'outputs/used-inputs'],
                cwd=root, capture_output=True, text=True,
                env=dict(os.environ, PYTHONPATH=str(SCHEDULER.parent)),
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(json.loads((root / 'outputs/dependencies.json').read_text()), dict(
                sources=['include/chosen.h', 'main.c'], generated=['generated.h', 'main.c'],
            ))
            self.assertEqual((root / 'outputs/used-inputs').read_text().splitlines(), [
                'prepared/include/chosen.h', 'prepared/main.c', 'stage/generated.h',
            ])

    def test_collector_shares_names_and_preserves_each_job_and_full_report(self):
        def decoded(directory):
            chunks = sorted((p for p in directory.rglob('*') if '=' in p.name), key=lambda p: p.name)
            return json.loads(unquote(''.join(p.name.split('=', 1)[1] for p in chunks)))

        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            names = [f'include/{index:04d}/' + 'long_native_header_name_' * 3 + '.h' for index in range(200)]
            answers = {'2': dict(sources=names, generated=['include/generated/config.h']),
                       '11': dict(sources=list(reversed(names)), generated=[names[0]])}
            for identity, answer in answers.items():
                directory = root / 'scans' / identity
                directory.mkdir(parents=True)
                (directory / 'dependencies.json').write_text(json.dumps(answer))
                (directory / 'used-inputs').write_text('')
            collect_scans(root / 'scans', root / 'metadata')
            paths = decoded(root / 'metadata/paths')
            self.assertEqual(len(paths), len(names) + 1)
            for identity, expected in answers.items():
                indices = decoded(root / 'metadata/jobs' / identity)
                self.assertEqual({kind: [paths[index] for index in values] for kind, values in indices.items()}, expected)
            self.assertEqual(json.loads((root / 'metadata/report.json').read_text()), answers)
            self.assertGreater(len(list((root / 'metadata/paths').rglob('*=*'))), 100)

    def test_source_and_generated_dependencies_keep_distinct_names(self):
        with tempfile.TemporaryDirectory() as temporary:
            work = Path(temporary)
            scan = DependencyScan(dict(
                source_prefix='prepared',
                original_prefix='original',
                aliases={'plan/generated.c': 'stage/artifacts/generated.c',
                         'original/include/aliased.h': 'prepared/include/aliased.h'},
                scan=dict(
                    argument_start=0,
                    header_tree='plan/headers',
                    generated={
                        'include/shared.h': 'plan/shared.h',
                        'generated.c': 'plan/generated.c',
                        'include/quoted name.h': 'plan/quoted.h',
                        'vmlinux.o': 'plan/vmlinux.o',
                        '__source__/include/aliased.h': 'original/include/aliased.h',
                    },
                ),
            ))
            scan.depfile = work / 'dependencies.d'
            scan.depfile.write_text(
                'object: original/source.c prepared/include/shared.h \\\n'
                f' {work}/plan/headers/include/shared.h '
                'stage/artifacts/generated.c include/quoted\\ name.h prepared/include/aliased.h\n'
            )
            self.assertEqual(scan.dependencies(work), dict(
                sources=['include/aliased.h', 'include/shared.h', 'source.c'],
                generated=['__source__/include/aliased.h', 'generated.c', 'include/quoted name.h', 'include/shared.h'],
            ))

    def test_undeclared_generated_header_is_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            work = Path(temporary)
            scan = DependencyScan(dict(
                source_prefix='prepared',
                original_prefix='original',
                aliases={},
                scan=dict(argument_start=0, header_tree='plan/headers', generated={}),
            ))
            scan.depfile = work / 'dependencies.d'
            scan.depfile.write_text('object: plan/headers/include/missing.h\n')
            with self.assertRaisesRegex(ValueError, 'undeclared generated dependency: include/missing.h'):
                scan.dependencies(work)

    def test_compile_reconstructs_only_measured_and_mandatory_inputs(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / 'selected.h').write_text('selected header')
            (root / 'generated.c').write_text('generated source')
            (root / 'initramfs').write_bytes(b'native payload')
            (root / 'tools.json').write_text('[]')
            # The unused vmlinux.o does not exist during execution. There is no
            # aggregate archive: the namespace is made from selected Files.
            manifest = root / 'compile.json'
            program = '\n'.join([
                'from pathlib import Path',
                'import shutil',
                "assert not Path('plan/vmlinux.o').exists()",
                "assert not Path('plan/headers/vmlinux.o').exists()",
                "assert Path('plan/headers/arch/x86').is_dir()",
                "shutil.copytree('plan/headers', 'native-work', symlinks=True)",
                "header = Path('native-work/include/generated/selected.h').read_text()",
                "payload = Path('native-work/usr/initramfs_data.cpio').read_bytes()",
                "source = Path('plan/generated.c').read_text()",
                "Path('plan/result').write_bytes((header + ':' + source).encode() + b':' + payload)",
            ])
            manifest.write_text(json.dumps(dict(
                executable=sys.executable,
                arguments=['-c', program],
                inputs=['plan/vmlinux.o', 'plan/generated.c'],
                outputs=['plan/result'],
                directory_inputs=[],
                directory_outputs=[],
                common_inputs='plan/tools.json',
                environment={},
            )))
            mapping = root / 'mapping.json'
            mapping.write_text(json.dumps(dict(
                manifest=str(manifest),
                aliases={
                    'plan/tools.json': 'tools.json',
                    'plan/generated.c': 'generated.c',
                    'plan/headers/include/generated/selected.h': 'selected.h',
                    'plan/headers/usr/initramfs_data.cpio': 'initramfs',
                },
                source_inputs=[],
                outputs={'plan/result': 'result'},
                empty_directories=['plan/headers', 'plan/headers/arch/x86'],
            )))
            result = subprocess.run(
                [sys.executable, str(SCHEDULER), 'execute', str(mapping)],
                cwd=root, capture_output=True, text=True,
                env=dict(os.environ, PYTHONPATH=str(SCHEDULER.parent)),
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((root / 'result').read_bytes(), b'selected header:generated source:native payload')

    def test_native_namespace_uses_scheduled_files_and_writable_private_copies(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / 'scheduled-header').write_text('original header')
            (root / 'scheduled-header').chmod(0o444)
            (root / 'scheduled-payload').write_bytes(b'native payload')
            manifest = root / 'native.json'
            program = '\n'.join([
                'from pathlib import Path',
                'import shutil',
                "assert Path('plan/headers/arch/x86').is_dir()",
                "assert Path('plan/headers/include/header.h').is_symlink()",
                "shutil.copytree('plan/headers', 'private-work', symlinks=False)",
                "header = Path('private-work/include/header.h')",
                "assert not header.is_symlink()",
                "header.chmod(header.stat().st_mode | 0o200)",
                "header.write_text('modified private header')",
                "Path('plan/result').write_bytes(header.read_bytes() + b':' + Path('private-work/payload').read_bytes())",
            ])
            manifest.write_text(json.dumps(dict(
                executable=sys.executable,
                arguments=['-c', program],
                inputs=['plan/header', 'plan/payload'],
                aliases={'plan/headers/include/header.h': 'plan/header', 'plan/headers/payload': 'plan/payload'},
                empty_directories=['plan/headers', 'plan/headers/arch/x86'],
                outputs=['plan/result'],
                directory_inputs=[],
                directory_outputs=[],
                environment={},
            )))
            mapping = root / 'mapping.json'
            mapping.write_text(json.dumps(dict(
                manifest=str(manifest),
                aliases={'plan/header': 'scheduled-header', 'plan/payload': 'scheduled-payload'},
                source_inputs=[],
                outputs={'plan/result': 'result'},
            )))
            result = subprocess.run(
                [sys.executable, str(SCHEDULER), 'execute', str(mapping)],
                cwd=root, capture_output=True, text=True,
                env=dict(os.environ, PYTHONPATH=str(SCHEDULER.parent)),
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((root / 'result').read_bytes(), b'modified private header:native payload')
            self.assertEqual((root / 'scheduled-header').read_text(), 'original header')
            self.assertEqual((root / 'scheduled-header').stat().st_mode & 0o777, 0o444)
            self.assertEqual((root / 'scheduled-payload').read_bytes(), b'native payload')


if __name__=='__main__':unittest.main()
