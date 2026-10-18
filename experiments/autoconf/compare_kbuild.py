"""Cross-check resolved config and global flags using native GNU Make."""
import json
import os
from pathlib import Path
import shlex
import subprocess

root=Path(__file__).resolve().parent
linux=Path(json.loads((root/'.generated/inputs.json').read_text())['linux'])
output=root/'validation/kbuild';output.mkdir(parents=True,exist_ok=True)
(output/'.config').write_bytes((root/'validation/minimal/requested.config').read_bytes())
variables=['KBUILD_CFLAGS','KBUILD_AFLAGS','KBUILD_CPPFLAGS','KBUILD_LDFLAGS','KBUILD_HOSTCFLAGS','KBUILD_HOSTLDFLAGS']
helper='.PHONY: autoconf_flags\nautoconf_flags:\n'
for i,name in enumerate(variables):helper+='\t$(file '+('>' if i==0 else '>>')+'$(DUMP_FILE),'+name+'=$('+name+'))\n'
helper+='\t@:\n';(output/'print-flags.mk').write_text(helper)
command=['make','--no-print-directory','-f',str(linux/'Makefile'),'ARCH=x86_64','CC=/usr/bin/gcc','LD=/usr/bin/ld','HOSTCC=/usr/bin/gcc','OBJCOPY=/usr/bin/objcopy','RUSTC=/bin/false','HOSTRUSTC=/bin/false','BINDGEN=/bin/false','PAHOLE=/bin/false','PAHOLE_VERSION=0']
with (output/'native-make.log').open('w') as log:
 subprocess.run([*command,'olddefconfig'],cwd=output,stdout=log,stderr=log,check=True)
 subprocess.run([*command,'-f','print-flags.mk','DUMP_FILE=flags.txt','autoconf_flags'],cwd=output,stdout=log,stderr=log,check=True)
flags=dict(line.split('=',1) for line in (output/'flags.txt').read_text().splitlines())
actual=json.loads((root/'bazel-bin/kernel.tree/setup.json').read_text())['flags']
def normalize(text):
 return shlex.split(text.replace(str(linux),'{source}').replace('bazel-out/k8-fastbuild/bin/kernel.sources','{source}'))
differences={name:{'starlark':normalize(actual[name]),'upstream':normalize(flags[name])} for name in variables if normalize(actual[name])!=normalize(flags[name])}
(output/'flag-differences.json').write_text(json.dumps(differences,indent=2)+'\n')
def config(path):
 values={}
 for line in path.read_text().splitlines():
  if line.startswith('CONFIG_'):
   name,value=line.split('=',1);values[name]=value
  elif line.startswith('# CONFIG_') and line.endswith(' is not set'):
   values[line[2:-11]]='n'
 return values
left=config(root/'bazel-bin/kernel.tree/.config');right=config(output/'.config')
config_diff={k:{'starlark':left.get(k),'upstream':right.get(k)} for k in sorted(set(left)|set(right)) if left.get(k)!=right.get(k)}
(output/'config-differences.json').write_text(json.dumps(config_diff,indent=2)+'\n')
profile_differences={'minimal':len(config_diff)}
for mode,target in [('allno','allno_config'),('debug','debug_config')]:
 profile_output=output/mode;profile_output.mkdir(exist_ok=True)
 (profile_output/'.config').write_bytes((root/'validation'/mode/'requested.config').read_bytes())
 with (profile_output/'native-make.log').open('w') as log:
  subprocess.run([*command,'olddefconfig'],cwd=profile_output,stdout=log,stderr=log,check=True)
 expected=config(profile_output/'.config');observed=config(root/('bazel-bin/'+target+'.tree/.config'))
 delta={k:{'starlark':observed.get(k),'upstream':expected.get(k)} for k in sorted(set(observed)|set(expected)) if observed.get(k)!=expected.get(k)}
 (profile_output/'config-differences.json').write_text(json.dumps(delta,indent=2)+'\n')
 profile_differences[mode]=len(delta)
summary=dict(global_flag_groups=len(variables),flag_differences=len(differences),config_differences=sum(profile_differences.values()),profiles=profile_differences)
(output/'summary.json').write_text(json.dumps(summary,indent=2)+'\n');print(json.dumps(summary,indent=2))
raise SystemExit(bool(differences or summary['config_differences']))
