"""Compare all resolved symbols and written configs against unmodified Kconfig."""
import argparse
import json
import os
from pathlib import Path
import subprocess

from probe_runtime import compiler_environment

parser=argparse.ArgumentParser();parser.add_argument('--tree',type=Path,default=Path('bazel-bin/kernel.tree'))
parser.add_argument('--mode', choices=['minimal','allno','debug'],default='minimal')
parser.add_argument('--out',type=Path,default=Path('validation/upstream'))
parser.add_argument('--linux',type=Path)
parser.add_argument('--execution-root',type=Path)
parser.add_argument('--conf',type=Path)
parser.add_argument('--values',type=Path)
parser.add_argument('--report-only',action='store_true')
args=parser.parse_args();root=Path(__file__).resolve().parent;linux=(args.linux or Path(json.loads((root/'.generated/inputs.json').read_text())['linux'])).resolve()
output=args.out.resolve();output.mkdir(parents=True,exist_ok=True)
report=json.loads((args.tree/'config-report.json').read_text())
qualification=report.get('mode') == 'qualification'
requested=report['raw_requested'] if qualification else report['requested']
seed='\n'.join('# CONFIG_'+k+' is not set' if v=='n' else 'CONFIG_'+k+'='+v for k in (list(requested) if qualification else report['request_order']) for v in [requested[k]])+'\n'
(output/'.config').write_text(seed);(output/'requested.config').write_text(seed)
execution_root=(args.execution_root or Path(subprocess.check_output(['bazel','info','execution_root'], cwd=root, text=True).strip())).resolve()
conf=(args.conf or root/'.generated/native/upstream/conf').resolve()
values=(args.values or root/'.generated/native/upstream/values').resolve()
spec=dict(report['toolchain'], source=str(linux))
env=compiler_environment(spec, execution_root)
env.update(KCONFIG_CONFIG='.config')
if qualification: env['KCONFIG_ALLCONFIG']=str(output/'requested.config')
with (output/'upstream.log').open('w') as log:
 subprocess.run([str(conf),'--allnoconfig' if qualification else '--olddefconfig',str(linux/'Kconfig')],cwd=output,env=env,stdout=log,stderr=log,check=True)
 native=subprocess.run([str(values),str(linux/'Kconfig'),'.config'],cwd=output,env=env,text=True,capture_output=True,check=True)
(output/'upstream-values.tsv').write_text(native.stdout)
resolved={}
for line in native.stdout.splitlines():
 name,kind,value=line.split('\t',2)
 resolved[name]=json.dumps(value,ensure_ascii=False) if kind=='5' else value
actual=json.loads((args.tree/'config-report.json').read_text())['values']
differences={name:{'starlark':actual.get(name),'upstream':resolved.get(name)} for name in sorted(set(actual)|set(resolved)) if actual.get(name)!=resolved.get(name)}
(output/'symbol-differences.json').write_text(json.dumps(differences,indent=2)+'\n')
def written(path):
 values={}
 for line in path.read_text().splitlines():
  if line.startswith('CONFIG_') and '=' in line:
   key,value=line.split('=',1);values[key.removeprefix('CONFIG_')]=value
  elif line.startswith('# CONFIG_') and line.endswith(' is not set'):
   values[line[9:-11]]='n'
 return values
left=written(args.tree/'.config');right=written(output/'.config')
written_differences={name:{'starlark':left.get(name),'upstream':right.get(name)} for name in sorted(set(left)|set(right)) if left.get(name)!=right.get(name)}
(output/'written-differences.json').write_text(json.dumps(written_differences,indent=2)+'\n')
summary=dict(symbols_starlark=len(actual),symbols_upstream=len(resolved),symbol_differences=len(differences),written_differences=len(written_differences),tree=str(args.tree.resolve()),compiler=env['CC_VERSION_TEXT'],compiler_command=env['CC'],linker_command=env['LD'],architecture=spec['architecture']['arch'])
(output/'summary.json').write_text(json.dumps(summary,indent=2)+'\n');print(json.dumps(summary,indent=2))
raise SystemExit(bool(differences or written_differences) and not args.report_only)
