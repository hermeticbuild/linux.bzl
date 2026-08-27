#include <linux/init.h>
#include <linux/module.h>

#ifdef LINUX_BZL_EXTERNAL_E2E
#include <linux_bzl_external_closure.h>

#ifndef LINUX_BZL_EXTERNAL_HEADER_CLOSURE_OK
#error "linux_cc_module hdrs closure did not provide the nested header"
#endif
#ifndef LINUX_BZL_EXTERNAL_FROM_M_OK
#error "a compiler flag derived from M did not resolve in the staged module"
#endif
#ifndef LINUX_BZL_EXTERNAL_FROM_KBUILD_EXTMOD_OK
#error "a compiler flag derived from KBUILD_EXTMOD did not resolve in the staged module"
#endif
#ifndef LINUX_BZL_M_ORIGIN_OK
#error "M did not retain command-line provenance in the external compiler"
#endif
#ifdef LINUX_BZL_M_ORIGIN_BAD
#error "M has the wrong provenance in the external compiler"
#endif
#ifndef LINUX_BZL_KBUILD_EXTMOD_ORIGIN_OK
#error "KBUILD_EXTMOD did not retain environment provenance in the external compiler"
#endif
#ifdef LINUX_BZL_KBUILD_EXTMOD_ORIGIN_BAD
#error "KBUILD_EXTMOD has the wrong provenance in the external compiler"
#endif
#endif

static int __init hello_init(void)
{
	pr_info("hello from an out-of-tree linux.bzl C module\n");
	return 0;
}

static void __exit hello_exit(void)
{
	pr_info("goodbye from an out-of-tree linux.bzl C module\n");
}

module_init(hello_init);
module_exit(hello_exit);

MODULE_AUTHOR("linux.bzl contributors");
MODULE_DESCRIPTION("Example out-of-tree C module built with linux.bzl");
MODULE_LICENSE("GPL");
MODULE_VERSION("1.0");
