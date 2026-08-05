#include <linux/init.h>
#include <linux/module.h>

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
