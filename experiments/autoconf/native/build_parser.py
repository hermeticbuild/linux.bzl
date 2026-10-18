"""Build a file-local syntax exporter and an unmodified upstream conf binary."""
import argparse
from pathlib import Path
import subprocess

workspace=Path(__file__).resolve().parents[1]
parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument('--linux', type=Path, required=True)
args=parser.parse_args()
root=workspace/'.generated/native';root.mkdir(parents=True,exist_ok=True)
linux=args.linux.resolve()
src=linux/'scripts/kconfig'
parser=(src/'parser.y').read_text().replace('zconf_nextfile($2);','menu_add_entry(NULL, M_COMMENT); current_entry->help = xstrdup($2);')
parser=parser.replace('\tmenu_finalize();','\treturn; /* Source scopes are linked by Starlark. */')
parser=parser.replace('\tyyparse();','\tif (strcmp(name, "scripts/Kconfig.include")) { zconf_initscan("scripts/Kconfig.include"); yyparse(); zconf_initscan(name); }\n\tyyparse();',1)
(root/'parser.y').write_text(parser)
lexer=(src/'lexer.l').read_text().replace('cur_filename = file_lookup(name, NULL, 0);','cur_filename = file_lookup(name, NULL, 0);\n yyrestart(yyin); BEGIN(INITIAL); prev_token = prev_prev_token = T_EOL; cur_lineno = 1;')
(root/'lexer.l').write_text(lexer)
subprocess.run(['bison','-t','-d','-o',str(root/'parser.tab.c'),str(root/'parser.y')],check=True)
subprocess.run(['flex','-o',str(root/'lexer.lex.c'),str(root/'lexer.l')],check=True)
dump=(workspace.parents[1]/'native/dump.c').read_text();dump=dump[:dump.index('int main(')] + (workspace.parents[1]/'native/kconfig_syntax.inc').read_text()
(root/'dump.c').write_text(dump)
sources=[str(src/(n+'.c')) for n in ['confdata','expr','menu','symbol','util','preprocess']]+[str(root/n) for n in ['lexer.lex.c','parser.tab.c','dump.c']]
subprocess.run(['gcc','-O2','-DYYDEBUG=1','-I'+str(src),'-I'+str(linux/'scripts/include'),'-I'+str(root),*sources,'-o',str(root/'syntax')],check=True)
print(root/'syntax')
