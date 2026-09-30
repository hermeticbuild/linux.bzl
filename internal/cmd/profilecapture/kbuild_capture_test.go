package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestCaptureKbuildOriginalInvocationAndDisposablePlan(t *testing.T) {
	for _, mode := range []string{"zero", "expired"} {
		for _, joined := range []bool{false, true} {
			options, recordPath := captureFixture(t, mode)
			t.Chdir(t.TempDir())
			t.Setenv("LINUX_BZL_PROFILECAPTURE_TEST_EXACT", " whitespace\nwith=values ")
			environment := environmentDigest()
			directory, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			options.arguments = []string{"-root", "source/Kconfig", "-resolve_config", "input.config", "-var", "ORDER=first then second $literal", "-target_probe_results", "target-results", "-var", "ORDER=third"}
			want := slices.Clone(options.arguments)
			if joined {
				options.arguments = append(options.arguments, "--kbuild_probe_plan_out=ordinary.plan")
			} else {
				options.arguments = append(options.arguments, "-kbuild_probe_plan_out", "ordinary.plan")
			}
			original := slices.Clone(options.arguments)
			var stderr bytes.Buffer
			if err := captureProfile(context.Background(), options, io.Discard, &stderr); err != nil {
				t.Fatalf("capture: %v, stderr=%s", err, &stderr)
			}
			data, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatal(err)
			}
			var record helperRecord
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
			if record.Directory != directory || record.EnvironmentDigest != environment || record.Executable != options.planner || !slices.Equal(original, options.arguments) {
				t.Fatal("Kbuild capture changed original invocation")
			}
			scratch := filepath.Dir(helperOperand(record.Arguments, "cpu_profile"))
			if joined {
				want = append(want, "--kbuild_probe_plan_out="+filepath.Join(scratch, "kbuild-plan"))
			} else {
				want = append(want, "-kbuild_probe_plan_out", filepath.Join(scratch, "kbuild-plan"))
			}
			want = append(want, "-cpu_profile", filepath.Join(scratch, "cpu.pprof"), "-profile_duration", options.duration.String())
			if !slices.Equal(record.Arguments, want) {
				t.Fatal("Kbuild capture changed non-output argument bytes/order")
			}
			if _, err := validateCapturedGzip(options.out, maxCompressedProfile, maxDecompressedProfile); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{scratch, "ordinary.plan"} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("diagnostic published or retained an ordinary plan")
				}
			}
		}
	}
}

func TestCaptureKbuildRejectsMixedOrMalformedModes(t *testing.T) {
	for _, args := range [][]string{
		{"-kbuild_probe_plan_out"}, {"-kbuild_probe_plan_out="},
		{"-kbuild_probe_plan_out", "@response"},
		{"-kbuild_probe_plan_out=a", "--kbuild_probe_plan_out=b"},
		{"-kbuild_probe_plan_out=a", "-family_execution_mode=guards"},
		{"-kbuild_probe_plan_out=a", "-family_compiler_guard_plan_out=b"},
		{"-kbuild_probe_plan_out=a", "-family_compiler_guard_manifest_out=b"},
		{"-kbuild_probe_plan_out=a", "-cpu_profile=p"},
		{"-kbuild_probe_plan_out=a", "-heap_profile=p"},
		{"-kbuild_probe_plan_out=a", "-profile_duration=1s"},
		{"-kbuild_probe_plan_out=a", "@response"},
	} {
		if _, err := capturePlannerArguments(args, "/scratch", time.Second); err == nil {
			t.Fatal("accepted mixed/malformed Kbuild profiling invocation")
		}
	}
}
