package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

// Measure the actual executable, not this test process: testing itself retains
// heap-profile entry points and would hide accidental ordinary-binary sampling.
func TestPlannerBinaryHeapSamplingIsolation(t *testing.T) {
	for _, test := range []struct {
		environment, rate string
		heap              bool
	}{
		{"LINUX_BZL_TEST_NORMAL_PLANNER", "0", false},
		{"LINUX_BZL_TEST_HEAP_PLANNER", "1048576", true},
	} {
		t.Run(test.environment, func(t *testing.T) {
			planner, err := runfiles.Rlocation(os.Getenv(test.environment))
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			cpu, heap := filepath.Join(dir, "cpu.pprof"), filepath.Join(dir, "heap.pprof")
			arguments := []string{"-cpu_profile", cpu, "-profile_duration", "1h"}
			if test.heap {
				arguments = append(arguments, "-heap_profile", heap)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, planner, arguments...)
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "GODEBUG=") {
					command.Env = append(command.Env, entry)
				}
			}
			// The linker-disabled ordinary binary must override even this explicit
			// runtime setting. The diagnostic binary must honor it, unchanged.
			command.Env = append(command.Env, "GODEBUG=memprofilerate=1048576")
			output, err := command.CombinedOutput()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != 2 || ctx.Err() != nil ||
				!strings.Contains(string(output), "Kconfig evaluation requires") {
				t.Fatalf("unexpected diagnostic-only validation result: %v, %s", err, output)
			}
			if !strings.Contains(string(output), "memprofile_rate="+test.rate+"\n") {
				t.Fatalf("actual binary sampling activation changed: %s", output)
			}
			if err := checkCPUProfileForTest(cpu); err != nil {
				t.Fatal(err)
			}
			if test.heap {
				if err := checkCPUProfileForTest(heap); err != nil {
					t.Fatal(err)
				}
			} else if _, err := os.Lstat(heap); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("ordinary binary emitted heap output")
			}
		})
	}
}
