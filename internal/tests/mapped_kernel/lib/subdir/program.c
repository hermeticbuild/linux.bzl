#include <stdio.h>
#include <string.h>
#include "path_probe.h"
#include "../../include/linux/family_host_value.h"
#include "../../include/generated/autoconf.h"

static const char mapped_program_file[] = __FILE__;
static const char mapped_program_base_file[] = __BASE_FILE__;

#ifdef CONFIG_USED_FAMILY_OPTION
#define MAPPED_FAMILY_VARIANT_VALUE 1
#define MAPPED_FAMILY_VARIANT_TEXT "1"
#else
#define MAPPED_FAMILY_VARIANT_VALUE 0
#define MAPPED_FAMILY_VARIANT_TEXT "0"
#endif

int main(int argc, char **argv) {
	volatile int header_value = 73;
	if (mapped_header_line_probe(&header_value) != 73) {
		fputs("header code observation mismatch\n", stderr);
		return 4;
	}
	if (strcmp(mapped_program_file, "/mapped-kernel-source/lib/subdir/program.c") != 0 ||
	    strcmp(mapped_program_base_file, "/mapped-kernel-source/lib/subdir/program.c") != 0 ||
	    strcmp(mapped_header_file, "/mapped-kernel-source/lib/subdir/path_probe.h") != 0 ||
	    strcmp(mapped_header_base_file, "/mapped-kernel-source/lib/subdir/program.c") != 0) {
		fprintf(stderr, "source path mismatch: program=%s base=%s header=%s header-base=%s\n",
			mapped_program_file, mapped_program_base_file,
			mapped_header_file, mapped_header_base_file);
		return 3;
	}
	if (argc == 2 && strcmp(argv[1], "--header") == 0)
		return printf("#ifndef MAPPED_FAMILY_GENERATED_VALUE_H\n"
			      "#define MAPPED_FAMILY_GENERATED_VALUE_H\n"
			      "#define MAPPED_FAMILY_GENERATED_VALUE %d\n"
			      "#endif\n", MAPPED_FAMILY_VARIANT_VALUE) < 0;
	if (argc != 1)
		return 2;
	return puts(MAPPED_FAMILY_HOST_VALUE ":" MAPPED_FAMILY_VARIANT_TEXT) < 0;
}
