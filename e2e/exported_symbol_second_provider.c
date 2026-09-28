#include <linux/init.h>
#include <linux/module.h>

int linux_bzl_e2e_second_dependency_value(void);

int linux_bzl_e2e_second_dependency_value(void)
{
	return 7;
}
EXPORT_SYMBOL_GPL(linux_bzl_e2e_second_dependency_value);

static int __init exported_symbol_second_provider_init(void)
{
	return 0;
}

static void __exit exported_symbol_second_provider_exit(void)
{
}

module_init(exported_symbol_second_provider_init);
module_exit(exported_symbol_second_provider_exit);

MODULE_DESCRIPTION("linux.bzl second external-module dependency provider");
MODULE_LICENSE("GPL");
