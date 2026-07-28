package kconfig

// objtoolConfigSymbols is the set of CONFIG_* symbols consulted by
// //internal/cmd/objtoolrun. These values are action inputs for every x86
// source-backed object whose Kbuild metadata enables objtool.
var objtoolConfigSymbols = []string{
	"CONFIG_FINEIBT",
	"CONFIG_FTRACE_MCOUNT_USE_OBJTOOL",
	"CONFIG_FUNCTION_PADDING_BYTES",
	"CONFIG_GCOV_KERNEL",
	"CONFIG_HAVE_JUMP_LABEL_HACK",
	"CONFIG_HAVE_NOINSTR_HACK",
	"CONFIG_HAVE_OBJTOOL_NOP_MCOUNT",
	"CONFIG_HAVE_STATIC_CALL_INLINE",
	"CONFIG_HAVE_UACCESS_VALIDATION",
	"CONFIG_KCOV",
	"CONFIG_LTO_CLANG",
	"CONFIG_MITIGATION_CALL_DEPTH_TRACKING",
	"CONFIG_MITIGATION_RETHUNK",
	"CONFIG_MITIGATION_RETPOLINE",
	"CONFIG_MITIGATION_SLS",
	"CONFIG_MITIGATION_SRSO",
	"CONFIG_MITIGATION_UNRET_ENTRY",
	"CONFIG_NOINSTR_VALIDATION",
	"CONFIG_OBJTOOL",
	"CONFIG_OBJTOOL_WERROR",
	"CONFIG_PREFIX_SYMBOLS",
	"CONFIG_STACK_VALIDATION",
	"CONFIG_UNWINDER_ORC",
	"CONFIG_X86_KERNEL_IBT",
}

// ObjtoolConfigSymbols returns a copy of the objtool action footprint.
func ObjtoolConfigSymbols() []string {
	out := make([]string, len(objtoolConfigSymbols))
	copy(out, objtoolConfigSymbols)
	return out
}
