#include <linux/errno.h>
#include <linux/init.h>
#include <linux/module.h>

extern int linux_bzl_e2e_dependency_value(void);
extern int linux_bzl_e2e_second_dependency_value(void);

static int __init exported_symbol_consumer_init(void)
{
	if (linux_bzl_e2e_dependency_value() != 42 ||
	    linux_bzl_e2e_second_dependency_value() != 7)
		return -EINVAL;
	return 0;
}

static void __exit exported_symbol_consumer_exit(void)
{
}

module_init(exported_symbol_consumer_init);
module_exit(exported_symbol_consumer_exit);

MODULE_DESCRIPTION("linux.bzl external-module dependency consumer");
MODULE_LICENSE("GPL");
