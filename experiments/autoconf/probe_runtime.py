"""Measured Kconfig syntax and staged Kbuild probes for the full prototype."""
import concurrent.futures
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile


def publish(output, value):
    output = Path(output)
    output.mkdir(parents=True, exist_ok=True)
    encoded = json.dumps(value, ensure_ascii=True, separators=(',', ':')).encode().hex()
    for start in range(0, len(encoded), 180):
        (output / ('%08d=' % (start // 180) + encoded[start:start+180])).touch()
    (output/'report.json').write_text(json.dumps(value,indent=2))


def kconfig(spec, output):
    root=Path(spec['source']).resolve()
    baseline=json.loads(Path(spec['baseline']).read_text())
    sources={k:json.loads(v) if isinstance(v,str) else v for k,v in baseline.items()}
    reachable=set();pending=['Kconfig']
    while pending:
        name=pending.pop()
        if name in reachable:continue
        reachable.add(name)
        for node in sources[name]:
            if node['kind']=='source':
                path=node['path']
                for k,v in {'SRCARCH':'x86','ARCH':'x86_64','SUBARCH':'x86','HEADER_ARCH':'x86'}.items():
                    path=path.replace('__KCONFIG_'+k,v)
                pending.append(path)
    env=dict(os.environ, srctree=str(root), ARCH='x86_64', SRCARCH='x86', SUBARCH='x86', HEADER_ARCH='x86',
             KERNELVERSION='prototype', CC=spec['cc'], LD=spec['ld'], RUSTC='/bin/false', PAHOLE='/bin/false',
             PAHOLE_VERSION='0', HOSTCC=spec['cc'], LC_ALL='C')
    env.update(spec['tool_environment'])
    env['CC_VERSION_TEXT']=subprocess.check_output([spec['cc'],'--version'],text=True).splitlines()[0]
    binary=str(Path(spec['syntax']).resolve())
    def export(name):
        result=subprocess.run([binary,name],cwd=root,env=env,capture_output=True,text=True)
        if result.returncode:raise RuntimeError(name+'\n'+result.stderr)
        nodes=json.loads(result.stdout)
        if len(nodes)!=len(sources[name]):
            return [{'file':name,'nodes':nodes}]
        return [{'file':name,'index':i,'node':n} for i,n in enumerate(nodes) if n!=sources[name][i]]
    with concurrent.futures.ThreadPoolExecutor(max_workers=12) as pool:
        patches=[p for group in pool.map(export,sorted(reachable)) for p in group]
    host_capabilities = {}
    for program in json.loads(Path(spec['host_programs']).read_text()):
        command=program.replace('$(pound)','#').replace('$(HOSTCC)',shlex.quote(spec['cc'])).replace('$(HOSTCFLAGS)',shlex.join(spec['compile_flags'])).replace('$(HOSTPKG_CONFIG)','pkg-config').replace('$$','$')
        result=subprocess.run(['/bin/sh','-c',command],env=env,text=True,capture_output=True,timeout=30)
        if result.returncode not in (0,1):raise RuntimeError(command+'\n'+result.stderr)
        host_capabilities[program]=result.stdout.strip()
    result=subprocess.run([spec['cc'],'-Werror','-x','c','-','-c','-o','/dev/null'],input='#include <libelf.h>\nvoid *p = elf_getshdrnum;\n',text=True,capture_output=True)
    host_capabilities['__libelf_modern_api__']=result.returncode==0
    publish(output,dict(patches=patches,capabilities={},host_capabilities=host_capabilities,history=[],files=len(reachable),compiler=env['CC_VERSION_TEXT']))


def probes(spec, output):
    previous=json.loads((Path(spec['previous'])/'report.json').read_text())
    if not spec['requests']:
        previous["converged"]=True
        publish(output,previous);return
    cc=spec['cc'];ld=spec['ld'];answers=dict(previous['capabilities']);records=[]
    for key,request in sorted(spec['requests'].items()):
        kind=request['kind'];arg=request['argument']
        with tempfile.TemporaryDirectory() as work:
            source=Path(work)/'test.c';obj=Path(work)/'test.o';source.write_text('int f(void) { return 0; }\n')
            if kind=='ld-option':
                command=[ld,*shlex.split(arg),'-v']
            elif kind=='as-instr':
                source=Path(work)/'test.S';source.write_text(arg.replace('\\n','\n').replace('\\$','$')+'\n')
                command=[cc,'-Werror','-c','-x','assembler',str(source),'-o',str(obj)]
            elif kind=='try-run':
                command=[spec['shell'],'-c',request['command']]
            elif kind.startswith('rustc'):
                raise RuntimeError('Rust probes require a declared Rust toolchain')
            else:
                flags=[f.replace('-Wno-','-W',1) if f.startswith('-Wno-') else f for f in shlex.split(arg)]
                if kind=='cc-disable-warning':flags=['-Werror='+arg,'-Wno-'+arg]
                command=[cc,'-Werror',*flags,'-c',str(source),'-o',str(obj)]
            result=subprocess.run(command,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,timeout=30,
                                  env=dict(os.environ,LC_ALL='C',TMP=work,TMPDIR=work))
            if result.returncode not in (0,1):raise RuntimeError(f'{key}: unexpected exit {result.returncode}\n{result.stderr}')
            answers[key]=result.returncode==0
            records.append(dict(key=key,argv=command,status=result.returncode,stderr=result.stderr))
    previous['capabilities']=answers;previous['history'].append(records)
    publish(output,previous)

if __name__=='__main__':
    spec=json.loads(Path(sys.argv[2]).read_text())
    (kconfig if sys.argv[1]=='kconfig' else probes)(spec,sys.argv[3])
