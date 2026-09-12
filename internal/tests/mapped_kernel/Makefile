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

ifneq ($(M),)
src := $(M)
obj := $(M)
include $(src)/Kbuild
else
include Kbuild
endif

CONFIG_SHELL := sh
if_changed = $(cmd_$(1))
if_changed_dep = $(cmd_and_fixdep)
# Keep the source-owned compiler/helper/cleanup envelope intact. The helper
# consumes the actual compiler depfile and emits the surviving command record.
squote := '
pound := \#
escsq = $(subst $(squote),'\$(squote)',$1)
cmd = set -e; $(cmd_$(1))
make-cmd = $(call escsq,$(subst $(pound),$$(pound),$(subst $$,$$$$,$(cmd_$(1)))))
dot-target = $(dir $@).$(notdir $@)
depfile = $(dot-target).d
cmd_and_fixdep = $(cmd); $(objtree)/scripts/basic/fixdep $(depfile) $@ '$(make-cmd)' > $(dot-target).cmd; rm -f $(depfile)
cmd_link_vmlinux = $< smoke.o host-result .smoke.o.cmd $@

ifneq ($(M),)
external_module := $(patsubst %.o,%,$(firstword $(obj-m)))
external_member_objects := $(addprefix $(obj)/,$($(external_module)-y))
external_module_object := $(obj)/$(external_module).o
external_module_output := $(obj)/$(external_module).ko
external_modules_order := $(obj)/modules.order
external_module_symvers := $(obj)/Module.symvers

.PHONY: modules
modules: $(external_module_output) $(external_modules_order) $(external_module_symvers)

$(obj)/%.o: $(src)/%.c
	$(CC) $(KBUILD_CPPFLAGS) $(KBUILD_CFLAGS) $(ccflags-y) -c -o $@ $<

$(external_module_object): $(external_member_objects)
	$(LD) -r -o $@ $^

$(external_module_output): $(external_module_object)
	$(LD) -r -o $@ $<

$(external_modules_order): $(external_module_object)
	printf '%s\n' '$(external_module).o' > $@

$(external_module_symvers): $(external_module_output)
	: > $@
else
.PHONY: all
all: vmlinux modules.builtin modules.builtin.modinfo kernel

.vmlinux.unstripped: scripts/link-vmlinux.sh smoke.o dollar_smoke.o numeric_smoke.o pending_left.o pending_right.o literal_hint.o host-result host-path-probe include/generated/mapped_offsets.h
	+$(call if_changed,link_vmlinux)

vmlinux: .vmlinux.unstripped
	cp $< $@

modules.builtin modules.builtin.modinfo:
	touch $@

kernel: vmlinux
	cp $< $@
endif
