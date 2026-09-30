package e2e

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/hermeticbuild/linux.bzl/e2e/internal/modversions"
)

func TestBuiltModuleVersionMetadata(t *testing.T) {
	resolve := func(t *testing.T, name string) string {
		t.Helper()
		if name == "" {
			t.Fatal("missing required MODVERSIONS runfile")
		}
		path, err := runfiles.Rlocation(name)
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
	readBounded := func(t *testing.T, name string, limit int64) []byte {
		t.Helper()
		f, err := os.Open(resolve(t, name))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, limit+1))
		if err != nil || int64(len(data)) > limit {
			t.Fatalf("read %s: size/error %d, %v", name, len(data), err)
		}
		return data
	}
	config := "\n" + string(readBounded(t, os.Getenv("MODVERSIONS_CONFIG"), 4<<20))
	if !strings.Contains(config, "\nCONFIG_MODVERSIONS=y\n") {
		t.Fatal("semantic test requires actual CONFIG_MODVERSIONS=y")
	}
	wantExtended := strings.Contains(config, "\nCONFIG_EXTENDED_MODVERSIONS=y\n")
	wantBasic := strings.Contains(config, "\nCONFIG_BASIC_MODVERSIONS=y\n") || !wantExtended
	symversFiles := strings.Fields(os.Getenv("MODVERSIONS_SYMVERS"))
	if len(symversFiles) == 0 {
		t.Fatal("no kernel symvers input")
	}
	tables := make([]map[string]uint32, 0, len(symversFiles))
	for _, name := range symversFiles {
		table, err := modversions.ParseSymvers(strings.NewReader(string(readBounded(t, name, 16<<20))))
		if err != nil {
			t.Fatalf("symvers %s: %v", name, err)
		}
		tables = append(tables, table)
	}
	if _, exists := tables[0]["module_layout"]; !exists {
		t.Fatal("kernel symvers lacks module_layout")
	}
	producers, err := modversions.MergeSymvers(tables...)
	if err != nil {
		t.Fatal(err)
	}
	moduleFiles := strings.Fields(os.Getenv("MODVERSIONS_MODULES"))
	if len(moduleFiles) < 2 {
		t.Fatal("must validate both actual in-tree and external modules")
	}
	consumer := os.Getenv("MODVERSIONS_DEPENDENCY_CONSUMER")
	required := strings.Fields(os.Getenv("MODVERSIONS_REQUIRED_IMPORTS"))
	consumerSeen := false
	for _, name := range moduleFiles {
		t.Run(fmt.Sprintf("module_%s", name), func(t *testing.T) {
			module, err := modversions.ReadModule(readBounded(t, name, modversions.MaxImageBytes))
			if err != nil {
				t.Fatalf("module %s: %v", name, err)
			}
			if module.Basic != wantBasic || module.Extended != wantExtended {
				t.Fatalf("version formats basic=%v extended=%v; config requires %v/%v", module.Basic, module.Extended, wantBasic, wantExtended)
			}
			needed := []string(nil)
			if name == consumer {
				consumerSeen = true
				needed = required
			}
			if err := modversions.Validate(module, producers, needed...); err != nil {
				t.Fatal(err)
			}
		})
	}
	if consumer != "" && !consumerSeen {
		t.Fatal("dependency consumer was not validated")
	}
}
