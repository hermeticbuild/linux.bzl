VERSION = 6
PATCHLEVEL = 18
SUBLEVEL = 0

SUBARCH := $(shell uname -m | sed -e s/x86_64/x86/)
ARCH ?= $(SUBARCH)
UTS_MACHINE := $(ARCH)
SRCARCH := $(ARCH)
ifeq ($(ARCH),x86)
UTS_MACHINE := x86_64
endif
KCONFIG_CONFIG ?= .config

NOSTDINC_FLAGS := -nostdinc
LINUXINCLUDE := -I$(srctree)/include -include $(srctree)/include/linux/compiler-version.h -include $(srctree)/include/linux/kconfig.h
KBUILD_CPPFLAGS := -D__KERNEL__
KBUILD_CFLAGS := -std=gnu11 -fno-common -fno-PIE
KBUILD_AFLAGS := -D__ASSEMBLY__ -fno-PIE
KBUILD_LDFLAGS :=
cmd_cc_o_c = $(CC) $(KBUILD_CPPFLAGS) $(KBUILD_CFLAGS) -c -o $@ $<
KBUILD_IMAGE := kernel

export NOSTDINC_FLAGS LINUXINCLUDE KBUILD_CPPFLAGS KBUILD_CFLAGS KBUILD_AFLAGS KBUILD_LDFLAGS cmd_cc_o_c KBUILD_IMAGE

include Kbuild

CONFIG_SHELL := sh
if_changed_dep = $(cmd_$(1))
cmd_link_vmlinux = $< smoke.o $@

.PHONY: all
all: vmlinux modules.builtin modules.builtin.modinfo kernel

.vmlinux.unstripped: scripts/link-vmlinux.sh smoke.o
	+$(call if_changed_dep,link_vmlinux)

vmlinux: .vmlinux.unstripped
	cp $< $@

modules.builtin modules.builtin.modinfo:
	touch $@

kernel: vmlinux
	cp $< $@
