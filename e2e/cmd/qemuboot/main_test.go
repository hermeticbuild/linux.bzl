package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMarkerWriterFindsSplitMarker(t *testing.T) {
	var output bytes.Buffer
	writer := newMarkerWriter(&output, "LINUX_BZL_BOOT_OK")
	for _, part := range []string{"kernel log\nLINUX_BZL_", "BOOT", "_OK\n"} {
		if _, err := writer.Write([]byte(part)); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}
	}
	select {
	case <-writer.found:
	default:
		t.Fatal("marker was not detected")
	}
	if got, want := writer.String(), output.String(); got != want {
		t.Fatalf("capture = %q, output = %q", got, want)
	}
}

func TestMarkerWriterFindsOverlappingSplitMarker(t *testing.T) {
	writer := newMarkerWriter(&bytes.Buffer{}, "ababaca")
	for _, part := range []string{"ababab", "aca"} {
		if _, err := writer.Write([]byte(part)); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}
	}
	if !writer.MarkerFound() {
		t.Fatal("overlapping marker was not detected")
	}
}

func TestMarkerWriterFindsMarkerAfterCaptureTruncation(t *testing.T) {
	writer := newMarkerWriter(&bytes.Buffer{}, "LINUX_BZL_BOOT_OK")
	if _, err := writer.Write(bytes.Repeat([]byte{'x'}, serialCaptureLimit+1)); err != nil {
		t.Fatalf("Write() failed: %v", err)
	}
	for _, part := range []string{"LINUX_BZL_", "BOOT_OK"} {
		if _, err := writer.Write([]byte(part)); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}
	}
	if !writer.MarkerFound() {
		t.Fatal("marker after capture truncation was not detected")
	}
}

func TestMarkerWriterBoundsCapture(t *testing.T) {
	writer := newMarkerWriter(&bytes.Buffer{}, "not present")
	prefix := bytes.Repeat([]byte{'a'}, 32)
	suffix := bytes.Repeat([]byte{'z'}, serialCaptureLimit)
	if _, err := writer.Write(append(prefix, suffix...)); err != nil {
		t.Fatalf("Write() failed: %v", err)
	}

	got := writer.String()
	if !strings.HasPrefix(got, serialTruncatedNote) {
		t.Fatalf("capture does not start with truncation notice: %q", got[:min(len(got), 80)])
	}
	if strings.Contains(got, string(prefix)) {
		t.Fatal("capture retained discarded prefix")
	}
	if tail := strings.TrimPrefix(got, serialTruncatedNote); tail != string(suffix) {
		t.Fatalf("capture tail length = %d, want %d", len(tail), len(suffix))
	}
	if writer.MarkerFound() {
		t.Fatal("absent marker was reported as found")
	}
}

func TestQEMUCommandX8664(t *testing.T) {
	config := validConfig()
	command, args, err := qemuCommand(config)
	if err != nil {
		t.Fatalf("qemuCommand() failed: %v", err)
	}
	if command != "/qemu/bin/qemu-system-x86_64" {
		t.Fatalf("command = %q", command)
	}
	want := []string{
		"-L", "/qemu/share/qemu",
		"-machine", "pc",
		"-accel", "tcg",
		"-m", "512M",
		"-smp", "1",
		"-display", "none",
		"-monitor", "none",
		"-serial", "stdio",
		"-nic", "none",
		"-no-reboot",
		"-no-shutdown",
		"-kernel", "/kernel",
		"-initrd", "/initramfs",
		"-append", "console=ttyS0 rdinit=/init",
		"-d", "guest_errors",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}

func TestQEMUCommandAarch64AddsCPU(t *testing.T) {
	config := validConfig()
	config.Arch = "aarch64"
	_, args, err := qemuCommand(config)
	if err != nil {
		t.Fatalf("qemuCommand() failed: %v", err)
	}
	if got := strings.Join(args, " "); !strings.Contains(got, "-cpu max") {
		t.Fatalf("args do not contain arm CPU selection: %s", got)
	}
}

func TestFormatCommand(t *testing.T) {
	got := formatCommand("/qemu system", []string{"-append", "console=ttyS0 rdinit=/init"})
	want := `"/qemu system" "-append" "console=ttyS0 rdinit=/init"`
	if got != want {
		t.Fatalf("formatCommand() = %q, want %q", got, want)
	}
}

func TestResolveRunfileDirectory(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "repo", "file")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := resolveRunfile("repo/file", getenvMap(map[string]string{"RUNFILES_DIR": root}))
	if err != nil {
		t.Fatalf("resolveRunfile() failed: %v", err)
	}
	if got != path {
		t.Fatalf("resolveRunfile() = %q, want %q", got, path)
	}
}

func TestResolveRunfileManifest(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "actual file")
	manifest := filepath.Join(root, "MANIFEST")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("repo/file "+file+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := resolveRunfile("repo/file", getenvMap(map[string]string{"RUNFILES_MANIFEST_FILE": manifest}))
	if err != nil {
		t.Fatalf("resolveRunfile() failed: %v", err)
	}
	if got != file {
		t.Fatalf("resolveRunfile() = %q, want %q", got, file)
	}
}

func TestFindConfigPath(t *testing.T) {
	getenv := getenvMap(map[string]string{configEnvironment: "from-env.json"})
	if got, err := findConfigPath(nil, getenv); err != nil || got != "from-env.json" {
		t.Fatalf("findConfigPath(nil) = %q, %v", got, err)
	}
	if got, err := findConfigPath([]string{"from-arg.json"}, getenv); err != nil || got != "from-arg.json" {
		t.Fatalf("findConfigPath(arg) = %q, %v", got, err)
	}
	if _, err := findConfigPath([]string{"one", "two"}, getenv); err == nil {
		t.Fatal("findConfigPath(two args) succeeded")
	}
}

func validConfig() bootConfig {
	return bootConfig{
		Arch:             "x86_64",
		QEMUSystem:       "/qemu/bin/qemu-system-x86_64",
		SystemDataAnchor: "/qemu/share/qemu",
		Machine:          "pc",
		Accel:            "tcg",
		Kernel:           "/kernel",
		Initramfs:        "/initramfs",
		KernelArgs:       []string{"console=ttyS0", "rdinit=/init"},
		QEMUArgs:         []string{"-d", "guest_errors"},
		Expect:           "LINUX_BZL_BOOT_OK",
		TimeoutSeconds:   60,
	}
}
