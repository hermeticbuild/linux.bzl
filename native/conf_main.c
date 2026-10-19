/* Preserve native configuration semantics while exposing its resolved inventory. */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "lkc.h"

#define main native_conf_main
#include "native_conf.c"
#undef main

static void json_string(const char *value)
{
	const unsigned char *p = (const unsigned char *)(value ? value : "");
	putchar('"');
	for (; *p; p++) {
		if (*p == '"' || *p == '\\')
			printf("\\%c", *p);
		else if (*p < 32)
			printf("\\u%04x", *p);
		else
			putchar(*p);
	}
	putchar('"');
}

int main(int argc, char **argv)
{
	struct symbol *sym;
	int first = 1;
	NATIVE_SYMBOL_ITERATOR_DECL

	if (argc != 4 || strcmp(argv[1], "--native-values"))
		return native_conf_main(argc, argv);
	conf_parse(argv[2]);
	if (conf_read(argv[3]))
		return 1;
	putchar('{');
	NATIVE_FOR_ALL_SYMBOLS {
		if (!sym->name || (sym->flags & SYMBOL_CONST) || sym->type == S_UNKNOWN)
			continue;
		sym_calc_value(sym);
		if (!first)
			putchar(',');
		first = 0;
		json_string(sym->name);
		fputs(":{\"type\":", stdout);
		json_string(sym_type_name(sym->type));
		fputs(",\"value\":", stdout);
		json_string(sym_get_string_value(sym));
		putchar('}');
	}
	puts("}");
	return ferror(stdout) ? 1 : 0;
}
