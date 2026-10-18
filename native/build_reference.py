"""Build upstream's unmodified conf implementation for configuration comparisons."""
import argparse
from pathlib import Path
import subprocess

parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument('--linux',type=Path,required=True)
args=parser.parse_args()
root=Path(__file__).resolve().parents[1]/'.generated/native/upstream';root.mkdir(parents=True,exist_ok=True)
linux=args.linux.resolve();src=linux/'scripts/kconfig'
subprocess.run(['bison','-t','-d','-o',str(root/'parser.tab.c'),str(src/'parser.y')],check=True)
subprocess.run(['flex','-o',str(root/'lexer.lex.c'),str(src/'lexer.l')],check=True)
common=[str(src/(n+'.c')) for n in ['confdata','expr','menu','symbol','util','preprocess']]+[str(root/n) for n in ['lexer.lex.c','parser.tab.c']]
flags=['gcc','-O2','-DYYDEBUG=1','-I'+str(src),'-I'+str(linux/'scripts/include'),'-I'+str(root)]
subprocess.run([*flags,*common,str(src/'conf.c'),'-o',str(root/'conf')],check=True)
# Independently dump all evaluated symbol values after reading upstream's config.
main='''#include <stdio.h>
#include "lkc.h"
#include <list.h>
#include "internal.h"
int main(int argc,char **argv) {
 if(argc!=3)return 2;
 conf_parse(argv[1]);
 conf_read(argv[2]);
 struct symbol *sym; int i;
 for_all_symbols(sym) {
   if(!sym->name || (sym->flags & SYMBOL_CONST) || sym->type == S_UNKNOWN)continue;
   sym_calc_value(sym);
   printf("%s\\t%d\\t%s\\n",sym->name,sym->type,sym_get_string_value(sym));
 }
 return 0;
}
'''
(root/'values.c').write_text(main)
subprocess.run([*flags,*common,str(root/'values.c'),'-o',str(root/'values')],check=True)
print(root)
