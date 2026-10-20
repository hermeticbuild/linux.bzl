#include <linux/build_bug.h>

void linux_bzl_ir_diagnostic(void);

void linux_bzl_ir_diagnostic(void)
{
#if defined(IR_ATTRIBUTE_ERROR)
	BUILD_BUG_ON_MSG(1, "linux-bzl-ir-attribute-error");
#elif defined(IR_ASSEMBLY_ERROR)
	asm volatile(".error \"linux-bzl-ir-inline-asm\"");
#else
#error Select an IR diagnostic case
#endif
}
