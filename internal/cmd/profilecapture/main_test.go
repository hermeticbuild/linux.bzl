package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/pprof"
	"slices"
	"strings"
	"testing"
	"time"
)

const helperModeEnvironment = "LINUX_BZL_PROFILECAPTURE_TEST_HELPER"
const helperRecordEnvironment = "LINUX_BZL_PROFILECAPTURE_TEST_RECORD"

type helperRecord struct {
	Arguments         []string
	Executable        string
	EnvironmentDigest [sha256.Size]byte
	Directory         string
}

func environmentDigest() [sha256.Size]byte {
	// JSON preserves ordered entries and their boundaries, without persisting
	// inherited values in the helper record or a failed assertion's output.
	encoded, _ := json.Marshal(os.Environ())
	return sha256.Sum256(encoded)
}

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperModeEnvironment); mode != "" {
		if err := captureHelper(mode); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(93)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helperOperand(arguments []string, name string) string {
	for index, argument := range arguments {
		for _, flag := range []string{"-" + name, "--" + name} {
			if argument == flag && index+1 < len(arguments) {
				return arguments[index+1]
			}
			if value, found := strings.CutPrefix(argument, flag+"="); found {
				return value
			}
		}
	}
	return ""
}

func fixtureGzip(contents string) []byte {
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	_, _ = io.WriteString(writer, contents)
	_ = writer.Close()
	return buffer.Bytes()
}

func captureHelper(mode string) error {
	arguments := os.Args[1:]
	directory, err := os.Getwd()
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(helperRecord{
		Arguments: arguments, Executable: os.Args[0],
		EnvironmentDigest: environmentDigest(), Directory: directory,
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(os.Getenv(helperRecordEnvironment), encoded, 0o600); err != nil {
		return err
	}
	if mode == "deadline" {
		time.Sleep(time.Minute)
		return fmt.Errorf("outer deadline did not terminate helper")
	}
	if mode == "signal" {
		process, err := os.FindProcess(os.Getpid())
		if err != nil {
			return err
		}
		return process.Kill()
	}
	manifest := helperOperand(arguments, "family_compiler_guard_manifest_out")
	plan := helperOperand(arguments, "family_compiler_guard_plan_out")
	if manifest != "" {
		if err := os.WriteFile(manifest, []byte("disposable manifest"), 0o600); err != nil {
			return err
		}
	} else {
		plan = helperOperand(arguments, "kbuild_probe_plan_out")
	}
	if err := os.Mkdir(plan, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(plan, "discarded.json"), []byte("not a receipt"), 0o600); err != nil {
		return err
	}
	profile := helperOperand(arguments, "cpu_profile")
	heapMode := strings.HasPrefix(mode, "heap-")
	if heapMode {
		mode = strings.TrimPrefix(mode, "heap-")
		cpu, err := os.Create(profile)
		if err != nil {
			return err
		}
		if err := pprof.StartCPUProfile(cpu); err != nil {
			_ = cpu.Close()
			return err
		}
		pprof.StopCPUProfile()
		if err := cpu.Close(); err != nil {
			return err
		}
		if mode == "bad-cpu" {
			if err := os.WriteFile(profile, []byte("broken CPU profile"), 0o600); err != nil {
				return err
			}
			mode = "zero"
		}
		profile = helperOperand(arguments, "heap_profile")
	}
	if mode == "missing" {
		return nil
	}
	if mode == "directory" {
		return os.Mkdir(profile, 0o700)
	}
	if mode == "zero" || mode == "expired" {
		// Successful lifecycle fixtures use real runtime/pprof bytes. Capturing
		// is still not the consumer-side semantic/sample-validity gate.
		file, err := os.Create(profile)
		if err != nil {
			return err
		}
		if heapMode {
			if err := pprof.WriteHeapProfile(file); err != nil {
				_ = file.Close()
				return err
			}
		} else {
			if err := pprof.StartCPUProfile(file); err != nil {
				_ = file.Close()
				return err
			}
			pprof.StopCPUProfile()
		}
		if err := file.Close(); err != nil {
			return err
		}
	} else {
		data := fixtureGzip("bounded gzip fixture; not a semantic pprof proof")
		switch mode {
		case "empty-file":
			data = nil
		case "empty-gzip":
			data = fixtureGzip("")
		case "corrupt":
			data[len(data)-8] ^= 1
		case "truncated":
			data = data[:len(data)-3]
		case "trailing":
			data = append(data, 'x')
		case "multistream":
			data = append(data, data...)
		case "symlink":
			if err := os.WriteFile(profile+".other", data, 0o600); err != nil {
				return err
			}
			return os.Symlink(profile+".other", profile)
		}
		if err := os.WriteFile(profile, data, 0o600); err != nil {
			return err
		}
	}
	if mode == "expired" || mode == "zero-with-deadline" || mode == "failed-finish-124" {
		duration, err := time.ParseDuration(helperOperand(arguments, "profile_duration"))
		if err != nil {
			return err
		}
		fmt.Fprint(os.Stderr, profileDeadlineMessage(duration))
	}
	if mode == "failed-finish" || mode == "failed-finish-124" {
		fmt.Fprint(os.Stderr, "kconfig_parse: failed to fin")
		if heapMode {
			fmt.Fprintln(os.Stderr, "ish heap profile: simulated close error")
		} else {
			fmt.Fprintln(os.Stderr, "ish CPU profile: simulated close error")
		}
	}
	switch mode {
	case "expired", "arbitrary-124", "failed-finish-124":
		os.Exit(124)
	case "failure":
		os.Exit(7)
	}
	return nil
}

func captureFixture(t *testing.T, mode string) (captureOptions, string) {
	t.Helper()
	planner, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	record := filepath.Join(directory, "child.json")
	t.Setenv(helperModeEnvironment, mode)
	t.Setenv(helperRecordEnvironment, record)
	return captureOptions{
		planner: planner, out: filepath.Join(directory, "captured.pprof"), duration: time.Second,
		arguments: []string{
			"-root", "source/Kconfig", "-var", "ORDER=first then second $literal",
			"-family_execution_mode", "guards", "-family_compiler_guard_manifest_out", "ordinary.json",
			"-family_compiler_guard_plan_out=ordinary.plan", "-family_compiler_guard_manifest", "0=prior.json",
			"-family_compiler_guard_plan", "0=prior.plan", "-family_compiler_guard_host_results", "0=prior.host",
			"-family_compiler_guard_target_results", "0=prior.target", "-var", "ORDER=third",
		},
	}, record
}

func TestCaptureProfilePreservesOriginalInvocation(t *testing.T) {
	for _, mode := range []string{"zero", "expired", "heap-zero", "heap-expired"} {
		t.Run(mode, func(t *testing.T) {
			options, recordPath := captureFixture(t, mode)
			if strings.HasPrefix(mode, "heap-") {
				options.kind = "heap"
			}
			t.Chdir(t.TempDir())
			t.Setenv("LINUX_BZL_PROFILECAPTURE_TEST_EXACT", " whitespace\nwith=values ")
			wantEnvironment := environmentDigest()
			wantDirectory, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			relativePlanner, err := filepath.Rel(wantDirectory, options.planner)
			if err != nil {
				t.Fatal(err)
			}
			options.planner = "./" + relativePlanner
			original := slices.Clone(options.arguments)
			var stderr bytes.Buffer
			if err := captureProfile(context.Background(), options, io.Discard, &stderr); err != nil {
				t.Fatalf("capture: %v, stderr=%s", err, &stderr)
			}
			if _, err := validateCapturedGzip(options.out, maxCompressedProfile, maxDecompressedProfile); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatal(err)
			}
			var record helperRecord
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
			if record.Directory != wantDirectory || record.EnvironmentDigest != wantEnvironment || record.Executable != options.planner || !slices.Equal(options.arguments, original) {
				t.Fatal("capture changed child cwd/environment, executable argv[0], or caller-owned argv")
			}
			scratch := filepath.Dir(helperOperand(record.Arguments, "cpu_profile"))
			want := slices.Clone(original)
			want[7] = filepath.Join(scratch, "manifest.json")
			want[8] = "-family_compiler_guard_plan_out=" + filepath.Join(scratch, "plan")
			want = append(want, "-cpu_profile", filepath.Join(scratch, "cpu.pprof"), "-profile_duration", options.duration.String())
			if options.kind == "heap" {
				want = append(want, "-heap_profile", filepath.Join(scratch, "heap.pprof"))
			}
			if !slices.Equal(record.Arguments, want) {
				t.Fatalf("child argv = %q, want %q", record.Arguments, want)
			}
			for _, filename := range []string{scratch, "ordinary.json", "ordinary.plan"} {
				if _, err := os.Lstat(filename); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("disposable/original output escaped capture: %s, %v", filename, err)
				}
			}
			if !strings.Contains(stderr.String(), "validate profile format before interpreting samples") {
				t.Fatal("capture omitted the mandatory consumer validation warning")
			}
		})
	}
}

func TestCaptureProfileRejectsFailureAndInvalidArtifacts(t *testing.T) {
	for _, mode := range []string{
		"failure", "arbitrary-124", "zero-with-deadline", "signal", "missing", "directory",
		"empty-file", "empty-gzip", "corrupt", "truncated", "trailing", "multistream", "symlink",
		"failed-finish", "failed-finish-124",
	} {
		t.Run(mode, func(t *testing.T) {
			options, _ := captureFixture(t, mode)
			if err := captureProfile(context.Background(), options, io.Discard, io.Discard); err == nil {
				t.Fatal("invalid capture accepted")
			}
			if _, err := os.Lstat(options.out); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed capture published an output: %v", err)
			}
		})
	}
}

func TestCaptureProfileIndependentDeadline(t *testing.T) {
	options, _ := captureFixture(t, "deadline")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := captureProfile(ctx, options, io.Discard, io.Discard); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("outer deadline became accepted profile expiry: %v", err)
	}
	if _, err := os.Lstat(options.out); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("outer timeout published an artifact")
	}
}

func TestCaptureProfileRefusesExistingOutput(t *testing.T) {
	options, _ := captureFixture(t, "zero")
	if err := os.WriteFile(options.out, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := captureProfile(context.Background(), options, io.Discard, io.Discard); err == nil {
		t.Fatal("existing output accepted")
	}
	data, err := os.ReadFile(options.out)
	if err != nil || string(data) != "existing" {
		t.Fatal("capture overwrote existing output")
	}
}

func TestCapturePlannerArgumentsRejectsUnsupportedForms(t *testing.T) {
	base := []string{"-family_execution_mode", "guards", "-family_compiler_guard_manifest_out", "original.json", "-family_compiler_guard_plan_out", "original.plan"}
	for _, arguments := range [][]string{
		nil, base[:len(base)-1], {"-family_execution_mode", "replay"},
		append(slices.Clone(base), "-family_execution_mode=guards"),
		append(slices.Clone(base), "-family_compiler_guard_manifest_out=duplicate"),
		append(slices.Clone(base), "--family_compiler_guard_plan_out=duplicate"),
		append(slices.Clone(base), "-cpu_profile", "profile"),
		append(slices.Clone(base), "--cpu_profile=profile"),
		append(slices.Clone(base), "-heap_profile", "profile"),
		append(slices.Clone(base), "--heap_profile=profile"),
		append(slices.Clone(base), "-profile_duration=1s"),
		append(slices.Clone(base), "@arguments"),
		append(slices.Clone(base), "--", "ignored"),
		{"-family_execution_mode=guards", "-family_compiler_guard_manifest_out=", "-family_compiler_guard_plan_out=plan"},
	} {
		if _, err := capturePlannerArguments(arguments, "/scratch", time.Second); err == nil {
			t.Fatalf("unsupported argv accepted: %q", arguments)
		}
	}
}

func TestCaptureDiagnosticsRetainsSplitMarkersAfterForwardingLimit(t *testing.T) {
	var output bytes.Buffer
	diagnostics := &captureDiagnostics{output: &output, remaining: 13, deadline: profileDeadlineMessage(time.Second)}
	text := strings.Repeat("prefix", 1000) + diagnostics.deadline + profileFinalizationError + " failed\n"
	for index := range len(text) {
		if n, err := diagnostics.Write([]byte(text[index : index+1])); n != 1 || err != nil {
			t.Fatal(n, err)
		}
		if len(diagnostics.tail) >= max(len(diagnostics.deadline), len(profileFinalizationError)) {
			t.Fatal("diagnostic overlap grew without bound")
		}
	}
	if !diagnostics.expired || !diagnostics.finishFailed || output.Len() != 13 {
		t.Fatalf("markers lost after bounded forwarding: %#v / %d", diagnostics, output.Len())
	}
}

func TestValidateCapturedGzipBoundsAndConsumerBoundary(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "capture")
	data := fixtureGzip(strings.Repeat("x", 1024))
	if err := os.WriteFile(filename, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, limits := range [][2]int64{{int64(len(data) - 1), 2048}, {2048, 1023}} {
		if _, err := validateCapturedGzip(filename, limits[0], limits[1]); err == nil {
			t.Fatalf("capture exceeded budget: %v", limits)
		}
	}
	// The supervisor verifies transport, not the protobuf/profile semantics.
	// Even this gzip fixture needs the mandatory consumer-side pprof parse.
	got, err := validateCapturedGzip(filename, int64(len(data)), 1024)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("exact gzip budget rejected: %v", err)
	}
}

func TestProfileCaptureCLI(t *testing.T) {
	for _, arguments := range [][]string{nil, {"-planner", "unused"}, {"-unknown", "--"}, {"operand", "--"}, {"-duration=-1s", "--"}} {
		if code := run(arguments, io.Discard, io.Discard); code == 0 {
			t.Fatalf("invalid supervisor argv accepted: %q", arguments)
		}
	}
	options, _ := captureFixture(t, "failure")
	arguments := append([]string{"-planner", options.planner, "-out", options.out, "-duration", "1s", "--"}, options.arguments...)
	if code := run(arguments, io.Discard, io.Discard); code != 7 {
		t.Fatalf("planner failure code = %d, want 7", code)
	}
}

func TestCaptureHeapRejectsFailuresBeforePublication(t *testing.T) {
	for _, mode := range []string{"missing", "directory", "empty-file", "empty-gzip", "corrupt", "truncated", "trailing", "multistream", "symlink", "failed-finish", "failed-finish-124", "bad-cpu", "failure", "arbitrary-124", "zero-with-deadline"} {
		t.Run(mode, func(t *testing.T) {
			options, _ := captureFixture(t, "heap-"+mode)
			options.kind = "heap"
			if err := captureProfile(context.Background(), options, io.Discard, io.Discard); err == nil {
				t.Fatal("invalid heap capture accepted")
			}
			if _, err := os.Lstat(options.out); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed heap capture published an output")
			}
		})
	}
	for _, mode := range []string{"signal", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			options, _ := captureFixture(t, mode)
			options.kind = "heap"
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if err := captureProfile(ctx, options, io.Discard, io.Discard); err == nil {
				t.Fatal("heap capture accepted signal/cancellation")
			}
			if _, err := os.Lstat(options.out); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed heap capture published an output")
			}
		})
	}
}

func TestCaptureHeapFinalizationMarkerRemainsBounded(t *testing.T) {
	diagnostics := &captureDiagnostics{output: io.Discard, remaining: 0}
	for _, value := range []string{strings.Repeat("x", 2048), "kconfig_parse: failed to fin", "ish heap profile:", " synthetic failure"} {
		_, _ = diagnostics.Write([]byte(value))
		if len(diagnostics.tail) >= max(len(profileFinalizationError), len(heapFinalizationError)) {
			t.Fatal("heap marker overlap grew without bound")
		}
	}
	if !diagnostics.finishFailed {
		t.Fatal("split heap failure lost after forwarding limit")
	}
}

func TestCaptureKindValidation(t *testing.T) {
	options, _ := captureFixture(t, "zero")
	options.kind = "unknown"
	if err := captureProfile(context.Background(), options, io.Discard, io.Discard); err == nil {
		t.Fatal("unknown capture kind accepted")
	}
	if _, err := capturePlannerArgumentsForKind(options.arguments, "/scratch", time.Second, "unknown"); err == nil {
		t.Fatal("unknown argument projection kind accepted")
	}
}
