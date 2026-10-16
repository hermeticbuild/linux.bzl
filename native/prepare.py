from pathlib import Path
import subprocess
from compiler_capabilities import native_capabilities
HERE = Path(__file__).resolve().parent

def prepare_sources(linux, output):
    output.mkdir(parents=True, exist_ok=True)
    src = linux / 'scripts/kconfig'
    parser = (src / 'parser.y').read_text()
    source_hook = 'zconf_nextfile($2);'
    assert parser.count(source_hook) == 1
    parser = parser.replace(source_hook, 'menu_add_entry(NULL, M_COMMENT); current_entry->help = xstrdup($2);')
    finalize = '\tmenu_finalize();'
    assert parser.count(finalize) == 1
    parser = parser.replace(finalize, '\treturn; /* Link and finalize the syntax in Bazel analysis. */')
    parser = parser.replace('\tyyparse();', '\t/* Preprocessor definitions shared by the kernel Kconfig files. */\n\tif (strcmp(name, "scripts/Kconfig.include")) {\n\t\tzconf_initscan("scripts/Kconfig.include");\n\t\tyyparse();\n\t\tzconf_initscan(name);\n\t}\n\tyyparse();', 1)
    (output / 'parser.y').write_text(parser)
    subprocess.run(['bison', '-t', '-d', '-o', output / 'parser.tab.c', output / 'parser.y'], check=True)
    lexer = (src / 'lexer.l').read_text().replace('cur_filename = file_lookup(name, NULL, 0);', 'cur_filename = file_lookup(name, NULL, 0);\n\tyyrestart(yyin);\n\tBEGIN(INITIAL);\n\tprev_token = prev_prev_token = T_EOL;\n\tcur_lineno = 1;')
    (output / 'lexer.l').write_text(lexer)
    subprocess.run(['flex', '-o', output / 'lexer.lex.c', output / 'lexer.l'], check=True)
    preprocess = (src / 'preprocess.c').read_text()
    marker = 'static LIST_HEAD(env_list);'
    assert preprocess.count(marker) == 1
    preprocess = preprocess.replace(marker, (HERE / 'kconfig_fallback.inc').read_text().replace('/* PROTOTYPE_COMPILER_CAPABILITIES */', native_capabilities()) + '\n' + marker)
    start = preprocess.index('static char *variable_expand(')
    before, after = preprocess[:start], preprocess[start:]
    after = after.replace('\tv = variable_lookup(name);', '\tres = prototype_probe_fallback(name, argc, argv);\n\tif (res) return res;\n\tv = variable_lookup(name);', 1)
    preprocess = (before + after).replace('\tcmd = argv[0];', '\treturn prototype_shell_fallback(argc, argv);\n\tcmd = argv[0];', 1)
    (output / 'preprocess.c').write_text(preprocess)
    dump = (HERE / 'dump.c').read_text()
    dump = dump[:dump.index('int main(')] + (HERE / 'kconfig_syntax.inc').read_text()
    (output / 'dump.c').write_text(dump)
    sources = [src / (n + '.c') for n in ('confdata', 'expr', 'menu', 'symbol', 'util')]
    sources += [output / n for n in ('lexer.lex.c', 'parser.tab.c', 'preprocess.c', 'dump.c')]
    return sources
