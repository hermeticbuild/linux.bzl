package sonic

import (
	"debug/elf"
	"os"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

func TestLinuxNgbdeModule(t *testing.T) {
	path, err := runfiles.Rlocation(os.Getenv("LINUX_NGBDE_MODULE"))
	if err != nil {
		t.Fatalf("resolve linux_ngbde module: %v", err)
	}
	module, err := elf.Open(path)
	if err != nil {
		t.Fatalf("open linux_ngbde ELF: %v", err)
	}
	defer module.Close()

	if module.Class != elf.ELFCLASS64 || module.Data != elf.ELFDATA2LSB || module.Machine != elf.EM_X86_64 || module.Type != elf.ET_REL {
		t.Fatalf("linux_ngbde ELF identity is class=%s data=%s machine=%s type=%s", module.Class, module.Data, module.Machine, module.Type)
	}
	section := module.Section(".modinfo")
	if section == nil {
		t.Fatal("linux_ngbde has no .modinfo section")
	}
	contents, err := section.Data()
	if err != nil {
		t.Fatalf("read linux_ngbde .modinfo: %v", err)
	}
	if !strings.Contains(string(contents), "name=linux_ngbde") {
		t.Fatalf("linux_ngbde .modinfo does not contain the canonical module name: %q", contents)
	}
}

func TestX86EnablesVendorKconfig(t *testing.T) {
	path, err := runfiles.Rlocation(os.Getenv("SONIC_X86_CONFIG"))
	if err != nil {
		t.Fatalf("resolve x86 config: %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read x86 config: %v", err)
	}
	if !strings.Contains(string(contents), "CONFIG_LINUX_NGBDE=m\n") {
		t.Fatalf("vendor Kconfig did not enable linux_ngbde as a module:\n%s", contents)
	}
}
