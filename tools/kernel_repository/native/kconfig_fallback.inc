/* SPDX-License-Identifier: GPL-2.0-only */
/* Prototype-only hooks: export toolchain inputs, never execute target tools. */
static char *prototype_input(const char *slot)
{
	const char *value = getenv(slot);
	return xstrdup(value ? value : slot);
}

static char *prototype_probe_fallback(const char *name, int argc, char *argv[])
{
	const char *fallback = NULL;
	char actual[64], expected[32], extra;
	const char *slot = NULL;

	if (!strcmp(name, "cc-name")) slot = "__KCONFIG_CC_NAME";
	else if (!strcmp(name, "cc-version")) slot = "__KCONFIG_CC_VERSION";
	else if (!strcmp(name, "as-name")) slot = "__KCONFIG_AS_NAME";
	else if (!strcmp(name, "as-version")) slot = "__KCONFIG_AS_VERSION";
	else if (!strcmp(name, "ld-name")) slot = "__KCONFIG_LD_NAME";
	else if (!strcmp(name, "ld-version")) slot = "__KCONFIG_LD_VERSION";
	else if (!strcmp(name, "CC_VERSION_TEXT")) slot = "__KCONFIG_CC_VERSION_TEXT";
	if (slot) return prototype_input(slot);
	if (!strcmp(name, "rustc-version") || !strcmp(name, "rustc-llvm-version"))
		return xstrdup("0");

	if ((!strcmp(name, "success") || !strcmp(name, "failure")) && argc &&
	    sscanf(argv[0], "test \"%63[^\"]\" = %31s %c", actual, expected, &extra) == 2) {
		if (!strcmp(actual, "__KCONFIG_CC_NAME")) {
			if (!strcmp(expected, "GCC")) slot = "__KCONFIG_CC_IS_GCC";
			if (!strcmp(expected, "Clang")) slot = "__KCONFIG_CC_IS_CLANG";
		} else if (!strcmp(actual, "__KCONFIG_AS_NAME")) {
			if (!strcmp(expected, "GNU")) slot = "__KCONFIG_AS_IS_GNU";
			if (!strcmp(expected, "LLVM")) slot = "__KCONFIG_AS_IS_LLVM";
		} else if (!strcmp(actual, "__KCONFIG_LD_NAME")) {
			if (!strcmp(expected, "BFD")) slot = "__KCONFIG_LD_IS_BFD";
			if (!strcmp(expected, "LLD")) slot = "__KCONFIG_LD_IS_LLD";
		}
		if (slot) {
			char *value = prototype_input(slot);
			if (!strcmp(name, "failure")) {
				char *negated = xmalloc(strlen(value) + 2);
				sprintf(negated, "!%s", value);
				free(value);
				return negated;
			}
			return value;
		}
		if (!strcmp(expected, "GCC") || !strcmp(expected, "Clang") ||
		    !strcmp(expected, "GNU") || !strcmp(expected, "LLVM") ||
		    !strcmp(expected, "BFD") || !strcmp(expected, "LLD"))
			return xstrdup((!strcmp(actual, expected)) == !strcmp(name, "success") ? "y" : "n");
	}

	/* Capability queries stay symbolic until toolchain/platform analysis. */
	/* PROTOTYPE_COMPILER_CAPABILITIES */
	if (!strcmp(name, "cc-option") || !strcmp(name, "ld-option") ||
	    !strcmp(name, "as-instr") || !strcmp(name, "as-instr64") ||
	    !strcmp(name, "rustc-option") || !strcmp(name, "cc_can_link_user"))
		fallback = "n";
	else if (!strcmp(name, "cc-option-bit")) fallback = "";
	else if (!strcmp(name, "success")) fallback = "n";
	else if (!strcmp(name, "failure"))
		/* Existence checks belong to toolchain selection, not generation. */
		fallback = strstr(cur_filename, "scripts/Kconfig.include") ? "n" : "y";
	else if (!strcmp(name, "if-success")) fallback = argc > 2 ? argv[2] : "";
	if (!fallback) return NULL;
	fprintf(stderr, "%s:%d: WARNING: unresolved Kconfig compiler capability %s(%s); using fallback '%s'\n",
		cur_filename, yylineno, name, argc ? argv[0] : "", fallback);
	return xstrdup(fallback);
}

static char *prototype_shell_fallback(int argc, char *argv[])
{
	unsigned int version;
	int end = 0;
	char major[32];
	(void)argc;
	if (strstr(argv[0], "__KCONFIG_CC_VERSION"))
		return prototype_input("__KCONFIG_CC_MAJOR_VERSION");
	/* During reference resolution cc-version has already been bound to its
	 * provider value, rather than the symbolic leaf used during export. */
	if (sscanf(argv[0], "expr %u / 10000 %n", &version, &end) == 1 &&
	    end && !argv[0][end]) {
		snprintf(major, sizeof(major), "%u", version / 10000);
		return xstrdup(major);
	}
	fprintf(stderr, "%s:%d: WARNING: Kconfig shell operation deferred/unsupported; using fallback 0: %s\n",
		cur_filename, yylineno, argv[0]);
	return xstrdup("0");
}
