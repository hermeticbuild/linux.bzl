#!/usr/bin/env python3
"""Build the kernel and verify three profiles against independent upstream conf."""
import argparse
import json
from pathlib import Path
import subprocess
import sys

parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument("--jobs", type=int, help="Override the workspace Bazel job limit")
args=parser.parse_args()
root=Path(__file__).resolve().parent
out=root/'validation';out.mkdir(exist_ok=True)
base=root.parents[1]/'_tmp/autoconf-integrated-output'
subprocess.run(['bazel','--output_base='+str(base),'build','//:kernel','//:config','//:allno_config','//:debug_config','//:custom_config']+(['--jobs='+str(args.jobs)] if args.jobs else []),cwd=root,check=True)
subprocess.run(['bazel','--output_base='+str(base),'test','//:execute_test','//:configure_test','--test_output=errors'],cwd=root,check=True)
summaries={}
for mode,target in [('minimal','config'),('allno','allno_config'),('debug','debug_config')]:
 subprocess.run([sys.executable,'compare_upstream.py','--mode',mode,'--tree','bazel-bin/'+target+'.tree','--out','validation/'+mode],cwd=root,check=True)
 summaries[mode]=json.loads((out/mode/'summary.json').read_text())
custom=json.loads((root/'bazel-bin/custom_config.tree/config-report.json').read_text())['values']
minimal=json.loads((root/'bazel-bin/config.tree/config-report.json').read_text())['values']
extras={'AUTOCONF_PROTOTYPE_CUSTOM','AUTOCONF_PROTOTYPE_DEPENDENT'}
assert set(custom)-set(minimal)==extras
assert all(custom[name]=='y' for name in extras)
assert all(custom[name]==value for name,value in minimal.items())
(out/'custom-probes.json').write_text(json.dumps({name:custom[name] for name in sorted(extras)},indent=2)+'\n')
assert json.loads((root/'bazel-bin/debug_config.tree/config-report.json').read_text())['values']['DEBUG_INFO_DWARF5']=='y'
# Compare the config actually used by the linked image as well.
subprocess.run([sys.executable,'compare_upstream.py','--tree','bazel-bin/kernel.tree','--out','validation/built-kernel'],cwd=root,check=True)
subprocess.run([sys.executable,'compare_kbuild.py'],cwd=root,check=True)
elf=root/'bazel-bin/kernel.tree/kernel.artifacts/vmlinux'
header=subprocess.check_output(['readelf','-h',str(elf)],text=True)
assert 'ELF64' in header and 'Advanced Micro Devices X86-64' in header,header
(out/'elf-header.txt').write_text(header)
symbols=subprocess.check_output(['nm',str(elf)],text=True)
for name in ['start_kernel','_text','x86_64_start_kernel']:
 assert any(line.endswith(' '+name) for line in symbols.splitlines()),name
(out/'kernel-symbols.txt').write_text(symbols)
manifest=json.loads((root/'bazel-bin/kernel.tree/kernel.json').read_text())
summary=dict(kbuild=json.loads((out/'kbuild/summary.json').read_text()),profiles=summaries,built_kernel_config=json.loads((out/'built-kernel/summary.json').read_text()),
             kernel=str(elf.resolve()),kernel_bytes=elf.stat().st_size,artifact_jobs=len(manifest['members']))
(out/'summary.json').write_text(json.dumps(summary,indent=2)+'\n')
print(json.dumps(summary,indent=2))
