package artifacts

import (
	"bufio"
	"debug/elf"
	"encoding/binary"
	"os"
	"strings"
	"testing"
)

type artifactIdentity struct {
	class   elf.Class
	machine elf.Machine
}

func requiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is not set", name)
	}
	return value
}

func expectedIdentity(t *testing.T) artifactIdentity {
	t.Helper()
	classes := map[string]elf.Class{
		"32": elf.ELFCLASS32,
		"64": elf.ELFCLASS64,
	}
	machines := map[string]elf.Machine{
		"arm":   elf.EM_ARM,
		"ppc64": elf.EM_PPC64,
		"riscv": elf.EM_RISCV,
	}
	className := requiredEnv(t, "ELF_CLASS")
	machineName := requiredEnv(t, "ELF_MACHINE")
	class, ok := classes[className]
	if !ok {
		t.Fatalf("unsupported ELF_CLASS %q", className)
	}
	machine, ok := machines[machineName]
	if !ok {
		t.Fatalf("unsupported ELF_MACHINE %q", machineName)
	}
	return artifactIdentity{class: class, machine: machine}
}

func checkELF(t *testing.T, path string, identity artifactIdentity, wantType elf.Type) {
	t.Helper()
	file, err := elf.Open(path)
	if err != nil {
		t.Fatalf("open ELF %s: %v", path, err)
	}
	defer file.Close()
	if file.Class != identity.class {
		t.Errorf("%s: ELF class is %s, want %s", path, file.Class, identity.class)
	}
	if file.Data != elf.ELFDATA2LSB {
		t.Errorf("%s: ELF data encoding is %s, want little endian", path, file.Data)
	}
	if file.Machine != identity.machine {
		t.Errorf("%s: ELF machine is %s, want %s", path, file.Machine, identity.machine)
	}
	if wantType != elf.ET_NONE && file.Type != wantType {
		t.Errorf("%s: ELF type is %s, want %s", path, file.Type, wantType)
	}
}

func readConfig(t *testing.T, path string) map[string]string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open config %s: %v", path, err)
	}
	defer file.Close()
	values := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "CONFIG_") {
			continue
		}
		name, value, found := strings.Cut(line, "=")
		if found {
			values[name] = value
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan config %s: %v", path, err)
	}
	return values
}

func checkConfig(t *testing.T) {
	t.Helper()
	inferred := requiredEnv(t, "INFERRED_CONFIG")
	input := readConfig(t, requiredEnv(t, "INPUT_CONFIG"))
	if _, found := input[inferred]; found {
		t.Fatalf("input config unexpectedly supplies inferred architecture symbol %s", inferred)
	}
	generated := readConfig(t, requiredEnv(t, "KERNEL_CONFIG"))
	if generated[inferred] != "y" {
		t.Errorf("generated config has %s=%q, want y", inferred, generated[inferred])
	}
	for _, name := range strings.Split(requiredEnv(t, "REQUIRED_CONFIGS"), ",") {
		if generated[name] != "y" {
			t.Errorf("generated config has %s=%q, want y", name, generated[name])
		}
	}
}

func checkImage(t *testing.T, identity artifactIdentity) {
	t.Helper()
	path := requiredEnv(t, "KERNEL_IMAGE")
	switch format := requiredEnv(t, "IMAGE_FORMAT"); format {
	case "elf":
		checkELF(t, path, identity, elf.ET_EXEC)
	case "arm-zimage":
		file, err := os.Open(path)
		if err != nil {
			t.Fatalf("open ARM zImage %s: %v", path, err)
		}
		defer file.Close()
		if _, err := file.Seek(0x24, 0); err != nil {
			t.Fatalf("seek ARM zImage %s: %v", path, err)
		}
		var magic uint32
		if err := binary.Read(file, binary.LittleEndian, &magic); err != nil {
			t.Fatalf("read ARM zImage magic from %s: %v", path, err)
		}
		if magic != 0x016f2818 {
			t.Errorf("%s: ARM zImage magic is %#08x, want %#08x", path, magic, uint32(0x016f2818))
		}
	default:
		t.Fatalf("unsupported IMAGE_FORMAT %q", format)
	}
}

func TestKernelArtifacts(t *testing.T) {
	identity := expectedIdentity(t)
	t.Run("config", checkConfig)
	t.Run("vmlinux", func(t *testing.T) {
		checkELF(t, requiredEnv(t, "KERNEL_VMLINUX"), identity, elf.ET_EXEC)
	})
	t.Run("module", func(t *testing.T) {
		checkELF(t, requiredEnv(t, "KERNEL_MODULE"), identity, elf.ET_REL)
	})
	t.Run("image", func(t *testing.T) {
		checkImage(t, identity)
	})
	if t.Failed() {
		t.Logf("artifact verification failed for %q", requiredEnv(t, "ARCH"))
	}
}
