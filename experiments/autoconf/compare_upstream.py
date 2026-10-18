"""Compare all resolved symbols and written configs against unmodified Kconfig."""
import argparse
import json
import os
from pathlib import Path
import subprocess

parser=argparse.ArgumentParser();parser.add_argument('--tree',type=Path,default=Path('bazel-bin/kernel.tree'))
parser.add_argument('--mode', choices=['minimal','allno','debug'],default='minimal')
parser.add_argument('--out',type=Path,default=Path('validation/upstream'))
args=parser.parse_args();root=Path(__file__).resolve().parent;linux=Path(json.loads((root/'.generated/inputs.json').read_text())['linux'])
output=args.out.resolve();output.mkdir(parents=True,exist_ok=True)
profile=json.loads((root/'.generated/profile.json').read_text());requested=dict(profile['base']);requested.update(profile['requested'] if args.mode!='allno' else {'64BIT':'y'})
if args.mode=='debug':requested.update({'DEBUG_KERNEL':'y','DEBUG_INFO_DWARF5':'y','DEBUG_INFO_NONE':'n','PRINTK_TIME':'y'})
seed='\n'.join('# CONFIG_'+k+' is not set' if v=='n' else 'CONFIG_'+k+'='+v for k,v in requested.items())+'\n'
(output/'.config').write_text(seed);(output/'requested.config').write_text(seed)
env=dict(os.environ,srctree=str(linux),ARCH='x86_64',SRCARCH='x86',SUBARCH='x86',HEADER_ARCH='x86',CC='/usr/bin/gcc',LD='/usr/bin/ld',
         HOSTCC='/usr/bin/gcc',RUSTC='/bin/false',PAHOLE='/bin/false',PAHOLE_VERSION='0',KERNELVERSION='prototype',KCONFIG_CONFIG='.config',LC_ALL='C')
env.update(AS='/usr/bin/as',AR='/usr/bin/ar',NM='/usr/bin/nm',OBJCOPY='/usr/bin/objcopy',OBJDUMP='/usr/bin/objdump',READELF='/usr/bin/readelf',STRIP='/usr/bin/strip',BINDGEN='/bin/false',HOSTRUSTC='/bin/false')
env['CC_VERSION_TEXT']=subprocess.check_output(['/usr/bin/gcc','--version'],text=True).splitlines()[0]
with (output/'upstream.log').open('w') as log:
 subprocess.run([str(root/'.generated/native/upstream/conf'),'--olddefconfig',str(linux/'Kconfig')],cwd=output,env=env,stdout=log,stderr=log,check=True)
 native=subprocess.run([str(root/'.generated/native/upstream/values'),str(linux/'Kconfig'),'.config'],cwd=output,env=env,text=True,capture_output=True,check=True)
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
summary=dict(symbols_starlark=len(actual),symbols_upstream=len(resolved),symbol_differences=len(differences),written_differences=len(written_differences),tree=str(args.tree.resolve()),compiler=env['CC_VERSION_TEXT'])
(output/'summary.json').write_text(json.dumps(summary,indent=2)+'\n');print(json.dumps(summary,indent=2))
raise SystemExit(bool(differences or written_differences))
