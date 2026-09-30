package mapped_kernel_test

import (
	"debug/dwarf"
	"debug/elf"
	"io"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceRunfilesPreservesAuthoredDebugPrefixMaps(t *testing.T) {
	// This is an explicitly declared objects-view output. Do not infer a host
	// store pathname from the plan or inspect files outside the test's runfiles.
	filename := filepath.Join(runfileFromEnv(t, "FAMILY_SMOKE_BASE_OBJECTS"), "host-path-probe")
	image, err := elf.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	data, err := image.DWARF()
	if err != nil {
		t.Fatalf("read host path probe DWARF: %v", err)
	}
	const (
		program = "/mapped-kernel-source/lib/subdir/program.c"
		header  = "/mapped-kernel-source/lib/subdir/path_probe.h"
	)
	lineFiles := map[string]bool{}
	programUnit := false
	inspectSourcePath := func(name, directory string) string {
		t.Helper()
		resolved := name
		if !path.IsAbs(resolved) {
			resolved = path.Join(directory, resolved)
		}
		base := path.Base(name)
		authored := base == "program.c" || base == "path_probe.h" ||
			strings.HasPrefix(resolved, "/mapped-kernel-source/") ||
			strings.Contains(name, ".runfiles/kernel/") ||
			strings.Contains(name, "/internal/tests/mapped_kernel/")
		// Compiler/CRT/system-header debug records are allowed to retain their
		// independently configured paths. The fixture's source paths are not.
		if authored {
			for _, spelling := range []string{name, resolved} {
				if strings.Contains(spelling, ".runfiles/") || strings.HasPrefix(spelling, "bazel-out/") ||
					strings.Contains(spelling, "/bazel-out/") {
					t.Errorf("authored source retains physical debug path %q", spelling)
				}
			}
			if base == "program.c" && resolved != program {
				t.Errorf("program debug path = %q, want %q", resolved, program)
			}
			if base == "path_probe.h" && resolved != header {
				t.Errorf("header debug path = %q, want %q", resolved, header)
			}
		}
		return resolved
	}
	reader := data.Reader()
	for {
		unit, err := reader.Next()
		if err != nil {
			t.Fatalf("read debug entry: %v", err)
		}
		if unit == nil {
			break
		}
		if unit.Tag != dwarf.TagCompileUnit {
			continue
		}
		name, _ := unit.Val(dwarf.AttrName).(string)
		directory, _ := unit.Val(dwarf.AttrCompDir).(string)
		if name != "" && inspectSourcePath(name, directory) == program {
			programUnit = true
			producer, _ := unit.Val(dwarf.AttrProducer).(string)
			if producer == "" {
				t.Error("authored compilation unit has no DW_AT_producer")
			}
			t.Logf("host path probe DW_AT_producer: %s", producer)
		}
		lines, err := data.LineReader(unit)
		if err != nil {
			t.Fatalf("read line table for %q: %v", name, err)
		}
		if lines != nil {
			// Read the complete table first: DW_LNE_define_file may add entries
			// after its initial file list. No referenced source file is opened.
			var entry dwarf.LineEntry
			for {
				err := lines.Next(&entry)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("read line entry for %q: %v", name, err)
				}
			}
			for _, file := range lines.Files() {
				if file != nil && file.Name != "" {
					lineFiles[inspectSourcePath(file.Name, directory)] = true
				}
			}
		}
		reader.SkipChildren()
	}
	if !programUnit {
		t.Errorf("no DW_AT_name identifies authored source %q", program)
	}
	for _, expected := range []string{program, header} {
		if !lineFiles[expected] {
			t.Errorf("line tables do not contain authored source %q", expected)
		}
	}
}
