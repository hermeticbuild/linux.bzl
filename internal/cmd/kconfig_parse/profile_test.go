package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func checkCPUProfileForTest(filename string) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("profile is not gzip: %w", err)
	}
	decoded, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if len(decoded) == 0 {
		return fmt.Errorf("profile is empty")
	}
	return nil
}

func TestCPUProfileOptionsAndDisabled(t *testing.T) {
	for _, test := range []struct {
		path     string
		duration time.Duration
		valid    bool
	}{
		{"", 0, true}, {"", time.Second, false}, {"", -time.Second, false},
		{"cpu.pprof", 0, false}, {"cpu.pprof", -time.Second, false},
		{"cpu.pprof", time.Second, true},
	} {
		if err := validateCPUProfileOptions(test.path, test.duration); (err == nil) != test.valid {
			t.Errorf("validate(%q, %s) = %v", test.path, test.duration, err)
		}
	}
	profile, err := startCPUProfile("", 0, nil, nil)
	if err != nil || profile != nil {
		t.Fatalf("disabled profile = %v, %v", profile, err)
	}
	profile.phase("disabled")
	if err := profile.stop(); err != nil || profile.finishExitCode(7) != 7 {
		t.Fatal("disabled profiling changed execution")
	}
	if _, err := startCPUProfile(t.TempDir(), time.Hour, io.Discard, func(int) {}); err == nil {
		t.Fatal("directory accepted as profile file")
	}
}

func TestCPUProfileNormalStopFlushesAndCancelsDeadline(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "cpu.pprof")
	var stderr bytes.Buffer
	exits := make(chan int, 1)
	profile, err := startCPUProfile(filename, time.Hour, &stderr, func(code int) { exits <- code })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = profile.stop() })
	profile.phase("variant base evaluation")
	if got, ok := pprof.Label(profile.labelParent, "phase"); !ok || got != "variant base evaluation" {
		t.Fatalf("profile phase label = %q/%t", got, ok)
	}
	if got := profile.finishExitCode(0); got != 0 {
		t.Fatalf("successful stop returned %d", got)
	}
	if err := profile.stop(); err != nil {
		t.Fatal(err)
	}
	profile.expire() // Simulate an already-queued timer after normal completion.
	select {
	case code := <-exits:
		t.Fatalf("normal completion left an active deadline: %d", code)
	default:
	}
	if err := checkCPUProfileForTest(filename); err != nil {
		t.Fatal(err)
	}
	if _, err := profile.output.writer.Write([]byte("closed")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("profile writer remains open: %v", err)
	}
	if !strings.Contains(stderr.String(), `phase="variant base evaluation"`) || !strings.Contains(stderr.String(), "heap_bytes=") {
		t.Fatalf("missing opt-in phase diagnostics: %q", stderr.String())
	}
}

func TestCPUProfileDeadlineFlushesBeforeExit(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "cpu.pprof")
	type exitResult struct {
		code int
		err  error
	}
	exits := make(chan exitResult, 1)
	profile, err := startCPUProfile(filename, time.Nanosecond, io.Discard, func(code int) {
		exits <- exitResult{code: code, err: checkCPUProfileForTest(filename)}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = profile.stop() })
	select {
	case result := <-exits:
		if result.code != cpuProfileDurationExitCode || result.err != nil {
			t.Fatalf("deadline exit = %d, profile = %v", result.code, result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("profile deadline did not terminate")
	}
	if err := profile.stop(); err != nil {
		t.Fatal(err)
	}
	profile.expire()
	select {
	case result := <-exits:
		t.Fatalf("deadline exited twice: %#v", result)
	default:
	}
}

type failingCPUProfileWriterForTest struct {
	writeErr, closeErr error
	short              bool
	closes             int
}

func (w *failingCPUProfileWriterForTest) Write(data []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if w.short && len(data) != 0 {
		return len(data) - 1, nil
	}
	return len(data), nil
}

func (w *failingCPUProfileWriterForTest) Close() error {
	w.closes++
	return w.closeErr
}

func TestCPUProfileWriterErrorsPreserveFailure(t *testing.T) {
	sentinel := errors.New("profile output failure")
	for _, test := range []struct {
		name   string
		writer failingCPUProfileWriterForTest
		want   error
	}{
		{"write", failingCPUProfileWriterForTest{writeErr: sentinel}, sentinel},
		{"close", failingCPUProfileWriterForTest{closeErr: sentinel}, sentinel},
		{"short_write", failingCPUProfileWriterForTest{short: true}, io.ErrShortWrite},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile, err := startCPUProfileWriter(&test.writer, time.Hour, io.Discard, func(int) {})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = profile.stop() })
			if code := profile.finishExitCode(0); code != 1 {
				t.Fatalf("profile failure returned success: %d", code)
			}
			if code := profile.finishExitCode(7); code != 7 {
				t.Fatalf("profile failure replaced prior failure: %d", code)
			}
			if err := profile.stop(); !errors.Is(err, test.want) || test.writer.closes != 1 {
				t.Fatalf("stop error = %v; closes = %d", err, test.writer.closes)
			}
		})
	}
}

func TestCPUProfileStartFailureClosesWriter(t *testing.T) {
	active, err := startCPUProfile(filepath.Join(t.TempDir(), "cpu.pprof"), time.Hour, io.Discard, func(int) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = active.stop() })
	output := &failingCPUProfileWriterForTest{}
	second, err := startCPUProfileWriter(output, time.Hour, io.Discard, func(int) {})
	if err == nil || second != nil || output.closes != 1 {
		t.Fatalf("concurrent profile = %v, %v; closes = %d", second, err, output.closes)
	}
}

func TestRunCPUProfileValidationAndErrorCleanup(t *testing.T) {
	for _, arguments := range [][]string{
		{"-cpu_profile=unused.pprof"}, {"-profile_duration=1s"},
		{"-cpu_profile=unused.pprof", "-profile_duration=-1s"},
	} {
		if code, stderr := runKconfigParseForTest(t, arguments...); code != 2 || !strings.Contains(stderr, "must be supplied together") {
			t.Fatalf("invalid profile options = %d, %q", code, stderr)
		}
	}
	filename := filepath.Join(t.TempDir(), "cpu.pprof")
	code, stderr := runKconfigParseForTest(t, "-cpu_profile="+filename, "-profile_duration=1h")
	if code != 2 || !strings.Contains(stderr, "Kconfig evaluation requires") {
		t.Fatalf("profiling changed existing validation failure: %d, %q", code, stderr)
	}
	if err := checkCPUProfileForTest(filename); err != nil {
		t.Fatalf("validation return did not flush profile: %v", err)
	}
	if code, stderr := runKconfigParseForTest(t); code != 2 || strings.Contains(stderr, "profile:") {
		t.Fatalf("default profiling is not inert: %d, %q", code, stderr)
	}
}

func TestConfigDependencyProfileLabelsPreservePhase(t *testing.T) {
	parent := pprof.WithLabels(context.Background(), pprof.Labels("phase", "variant base evaluation", "retained", "caller"))
	for _, event := range []string{"analysis_start", "compiler_start", "compiler_complete", "analysis_end"} {
		snapshot := kconfig.ConfigDependencyDiagnosticSnapshot{
			AnalysisSequence: 17, Event: event, CurrentOutput: "drivers/example/driver.o",
		}
		labeled := configDependencyProfileLabels(parent, snapshot)
		for key, want := range map[string]string{"phase": "variant base evaluation", "retained": "caller"} {
			if got, ok := pprof.Label(labeled, key); !ok || got != want {
				t.Fatalf("%s lost parent label %s: %q/%t", event, key, got, ok)
			}
		}
		analysis, _ := pprof.Label(labeled, "analysis")
		output, _ := pprof.Label(labeled, "compile_output")
		if event == "analysis_end" {
			if labeled != parent || analysis != "" || output != "" {
				t.Fatalf("analysis end did not restore the parent context: analysis=%q output=%q", analysis, output)
			}
		} else {
			if analysis != "17" {
				t.Fatalf("%s analysis label = %q, want 17", event, analysis)
			}
			wantOutput := ""
			if event == "compiler_start" {
				wantOutput = snapshot.CurrentOutput
			}
			if output != wantOutput {
				t.Fatalf("%s compile_output = %q, want %q", event, output, wantOutput)
			}
		}
		if _, exists := pprof.Label(parent, "analysis"); exists {
			t.Fatal("label construction mutated the caller's phase context")
		}
	}
}

func TestCPUProfileDisabledObservationPreservesGoroutineLabels(t *testing.T) {
	parent := pprof.WithLabels(context.Background(), pprof.Labels("profile_disabled_test", "unchanged", "phase", "caller phase"))
	pprof.SetGoroutineLabels(parent)
	t.Cleanup(func() { pprof.SetGoroutineLabels(context.Background()) })
	var profile *cpuProfileSession
	profile.phase("must stay disabled")
	profile.observeConfigDependencies(kconfig.ConfigDependencyDiagnosticSnapshot{
		AnalysisSequence: 999, Event: "compiler_start", CurrentOutput: "must-not-label.o",
	})
	var dump bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&dump, 1); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range strings.Split(dump.String(), "\n") {
		if !strings.HasPrefix(line, "# labels: ") {
			continue
		}
		labels := map[string]string{}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "# labels: ")), &labels); err != nil {
			t.Fatal(err)
		}
		if labels["profile_disabled_test"] != "unchanged" {
			continue
		}
		found = true
		if labels["phase"] != "caller phase" || labels["analysis"] != "" || labels["compile_output"] != "" {
			t.Fatalf("disabled profiling changed goroutine labels: %#v", labels)
		}
	}
	if !found {
		t.Fatal("disabled observation replaced the caller's goroutine labels")
	}
}

type gatedCPUProfileCloseForTest struct {
	file         *os.File
	entered      chan struct{}
	release      chan struct{}
	releaseOnce  sync.Once
	armed        atomic.Bool
	closed       atomic.Bool
	closeStarted atomic.Bool
}

func (w *gatedCPUProfileCloseForTest) Write(data []byte) (int, error) {
	return w.file.Write(data)
}

func (w *gatedCPUProfileCloseForTest) unblock() {
	w.releaseOnce.Do(func() { close(w.release) })
}

func (w *gatedCPUProfileCloseForTest) Close() error {
	if w.armed.Load() && w.closeStarted.CompareAndSwap(false, true) {
		close(w.entered)
		<-w.release
	}
	err := w.file.Close()
	w.closed.Store(true)
	return err
}

type afterCloseCPUProfileLogForTest struct {
	bytes.Buffer
	profile *gatedCPUProfileCloseForTest
	early   bool
}

func (w *afterCloseCPUProfileLogForTest) Write(data []byte) (int, error) {
	if !w.profile.closed.Load() {
		w.early = true
	}
	return w.Buffer.Write(data)
}

func TestCPUProfileDeadlinePublishesImmutableConfigSnapshotAfterFlush(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "cpu.pprof")
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	output := &gatedCPUProfileCloseForTest{file: file, entered: make(chan struct{}), release: make(chan struct{})}
	stderr := &afterCloseCPUProfileLogForTest{profile: output}
	type exitResult struct {
		code int
		err  error
	}
	exits := make(chan exitResult, 1)
	profile, err := startCPUProfileWriter(output, time.Hour, stderr, func(code int) {
		profileErr := checkCPUProfileForTest(filename)
		if !output.closed.Load() {
			profileErr = errors.Join(profileErr, errors.New("exit before output close"))
		}
		exits <- exitResult{code: code, err: profileErr}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		output.unblock()
		_ = profile.stop()
		pprof.SetGoroutineLabels(context.Background())
	})
	// The parent context is fixed before the observer and timer run together.
	// No live planner state or mutable phase field is read by the timer.
	profile.phase("variant base evaluation")
	stderr.Reset()
	stderr.early = false
	snapshot := kconfig.ConfigDependencyDiagnosticSnapshot{
		AnalysisSequence: 7, Event: "compiler_start", TotalCompileNodes: 101,
		StartedCompileNodes: 9, CompletedCompileNodes: 8, CurrentOutput: "drivers/example/current.o",
		PublishedAt: time.Now().Add(-2 * time.Second), ForcedCacheHits: 11, ForcedCacheMisses: 12,
		CompletedCacheHits: 13, CompletedCacheMisses: 14,
	}
	wantSnapshot := snapshot
	profile.observeConfigDependencies(snapshot)
	snapshot.CurrentOutput = "mutated-after-publication.o"
	if got := profile.progress.Load(); got == nil || *got != wantSnapshot {
		t.Fatalf("observer did not publish an immutable value copy: %#v", got)
	}
	output.armed.Store(true)
	go profile.expire()
	select {
	case <-output.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("deadline did not reach profile close")
	}
	// Closing is blocked while the deadline owns the session mutex. Publication
	// must remain lock-free, and every observed pointer must hold a whole value.
	published := make(chan struct{})
	go func() {
		defer close(published)
		for index := 1; index <= 4096; index++ {
			profile.observeConfigDependencies(kconfig.ConfigDependencyDiagnosticSnapshot{
				AnalysisSequence: uint64(index + 100), Event: "compiler_start", TotalCompileNodes: index,
				StartedCompileNodes: index, CompletedCompileNodes: index, CurrentOutput: strconv.Itoa(index), PublishedAt: time.Now(),
			})
		}
	}()
	for index := 0; index < 4096; index++ {
		current := profile.progress.Load()
		if current.AnalysisSequence == wantSnapshot.AnalysisSequence {
			continue
		}
		if current.AnalysisSequence != uint64(current.TotalCompileNodes+100) ||
			current.StartedCompileNodes != current.TotalCompileNodes || current.CompletedCompileNodes != current.TotalCompileNodes ||
			current.CurrentOutput != strconv.Itoa(current.TotalCompileNodes) {
			t.Fatalf("torn published diagnostic snapshot: %#v", current)
		}
	}
	select {
	case <-published:
	case <-time.After(10 * time.Second):
		t.Fatal("diagnostic publication blocked behind profile finalization")
	}
	output.unblock()
	select {
	case result := <-exits:
		if result.code != cpuProfileDurationExitCode || result.err != nil {
			t.Fatalf("deadline exit = %d, profile = %v", result.code, result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("deadline did not finish after releasing profile close")
	}
	if stderr.early {
		t.Fatal("deadline diagnostics were printed before profile output closed")
	}
	const prefix = "kconfig_parse profile: config_dependencies="
	found := false
	for _, line := range strings.Split(stderr.String(), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		encoded, ageText, ok := strings.Cut(strings.TrimPrefix(line, prefix), " snapshot_age=")
		if !ok {
			t.Fatalf("diagnostic snapshot has no age: %q", line)
		}
		var got kconfig.ConfigDependencyDiagnosticSnapshot
		if err := json.Unmarshal([]byte(encoded), &got); err != nil {
			t.Fatal(err)
		}
		if got.AnalysisSequence != wantSnapshot.AnalysisSequence || got.Event != wantSnapshot.Event ||
			got.TotalCompileNodes != wantSnapshot.TotalCompileNodes || got.StartedCompileNodes != wantSnapshot.StartedCompileNodes ||
			got.CompletedCompileNodes != wantSnapshot.CompletedCompileNodes || got.CurrentOutput != wantSnapshot.CurrentOutput ||
			!got.PublishedAt.Equal(wantSnapshot.PublishedAt) || got.ForcedCacheHits != wantSnapshot.ForcedCacheHits ||
			got.ForcedCacheMisses != wantSnapshot.ForcedCacheMisses || got.CompletedCacheHits != wantSnapshot.CompletedCacheHits ||
			got.CompletedCacheMisses != wantSnapshot.CompletedCacheMisses {
			t.Fatalf("deadline did not retain the snapshot captured before flushing: got %#v, want %#v", got, wantSnapshot)
		}
		age, err := time.ParseDuration(ageText)
		if err != nil || age < 2*time.Second || age > time.Since(wantSnapshot.PublishedAt)+time.Second {
			t.Fatalf("invalid diagnostic snapshot age %q: %v", ageText, err)
		}
		found = true
	}
	if !found {
		t.Fatalf("deadline omitted the last published diagnostic snapshot: %q", stderr.String())
	}
}

func TestHeapProfileDisabledAndOptions(t *testing.T) {
	if diagnosticHeapWriter != nil {
		t.Fatal("ordinary test library must not install the diagnostic heap writer")
	}
	dir := t.TempDir()
	cpu, heap := filepath.Join(dir, "cpu.pprof"), filepath.Join(dir, "heap.pprof")
	if _, err := startDiagnosticProfile(cpu, heap, time.Hour, io.Discard, func(int) {}); err == nil {
		t.Fatal("ordinary planner accepted heap capture")
	}
	if _, err := os.Lstat(cpu); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected heap options created CPU output")
	}
	calls := 0
	diagnosticHeapWriter = func(io.Writer) error { calls++; return nil }
	t.Cleanup(func() { diagnosticHeapWriter = nil })
	for _, test := range []struct {
		cpu, heap string
		duration  time.Duration
	}{
		{"", heap, time.Hour}, {cpu, heap, 0}, {cpu, cpu, time.Hour},
		{cpu, filepath.Join(dir, ".", "cpu.pprof"), time.Hour},
	} {
		if err := validateDiagnosticProfileOptions(test.cpu, test.heap, test.duration); err == nil {
			t.Fatal("invalid paired heap options accepted")
		}
	}
	profile, err := startDiagnosticProfile(cpu, "", time.Hour, io.Discard, func(int) {})
	if err != nil {
		t.Fatal(err)
	}
	if err := profile.stop(); err != nil || calls != 0 {
		t.Fatalf("CPU-only mode invoked heap hook: %v, calls=%d", err, calls)
	}
}

func TestHeapProfileClosesBeforeNormalOrDeadlineExit(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			dir := t.TempDir()
			cpuName, heapName := filepath.Join(dir, "cpu.pprof"), filepath.Join(dir, "heap.pprof")
			cpu, err := os.Create(cpuName)
			if err != nil {
				t.Fatal(err)
			}
			heap, err := os.Create(heapName)
			if err != nil {
				_ = cpu.Close()
				t.Fatal(err)
			}
			writes := 0
			duration := time.Hour
			if deadline {
				duration = time.Nanosecond
			}
			exits := make(chan error, 2)
			var stderr bytes.Buffer
			profile, err := startDiagnosticProfileWriter(cpu, heap, func(writer io.Writer) error {
				writes++
				if err := checkCPUProfileForTest(cpuName); err != nil {
					return err
				}
				return pprof.WriteHeapProfile(writer)
			}, duration, &stderr, func(code int) {
				var failure error
				if code != 124 {
					failure = fmt.Errorf("exit = %d", code)
				}
				_, cpuErr := cpu.Write([]byte("closed"))
				_, heapErr := heap.Write([]byte("closed"))
				if !errors.Is(cpuErr, os.ErrClosed) || !errors.Is(heapErr, os.ErrClosed) {
					failure = errors.Join(failure, fmt.Errorf("profile not closed before exit"))
				}
				exits <- errors.Join(failure, checkCPUProfileForTest(cpuName), checkCPUProfileForTest(heapName))
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = profile.stop() })
			if deadline {
				select {
				case err := <-exits:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("heap deadline did not finish")
				}
			} else if code := profile.finishExitCode(0); code != 0 {
				t.Fatalf("normal heap stop = %d", code)
			}
			if err := profile.stop(); err != nil {
				t.Fatal(err)
			}
			profile.expire()
			if writes != 1 {
				t.Fatalf("heap writes = %d", writes)
			}
			select {
			case <-exits:
				t.Fatal("duplicate deadline exit")
			default:
			}
			if err := checkCPUProfileForTest(heapName); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stderr.String(), "forced_gc=false") ||
				!strings.Contains(stderr.String(), "heap_capture_begin") ||
				!strings.Contains(stderr.String(), "heap_capture_end") {
				t.Fatal("heap snapshot semantics/timing were not recorded")
			}
		})
	}
}

func TestHeapProfileFailureIsNeverSuccess(t *testing.T) {
	sentinel := errors.New("heap fixture failure")
	for _, test := range []struct {
		name          string
		writer        failingCPUProfileWriterForTest
		callbackError error
		want          error
	}{
		{"callback", failingCPUProfileWriterForTest{}, sentinel, sentinel},
		{"write", failingCPUProfileWriterForTest{writeErr: sentinel}, nil, sentinel},
		{"short", failingCPUProfileWriterForTest{short: true}, nil, io.ErrShortWrite},
		{"close", failingCPUProfileWriterForTest{closeErr: sentinel}, nil, sentinel},
	} {
		t.Run(test.name, func(t *testing.T) {
			cpu := &failingCPUProfileWriterForTest{}
			var stderr bytes.Buffer
			profile, err := startDiagnosticProfileWriter(cpu, &test.writer, func(writer io.Writer) error {
				// A misbehaving serializer may swallow write errors; the wrapper
				// must still detect these, just as it does for CPU output.
				_, _ = writer.Write([]byte("heap"))
				return test.callbackError
			}, time.Hour, &stderr, func(int) {})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = profile.stop() })
			if profile.finishExitCode(0) != 1 || profile.finishExitCode(7) != 7 {
				t.Fatal("heap failure returned success or replaced original failure")
			}
			if err := profile.stop(); !errors.Is(err, test.want) || test.writer.closes != 1 || cpu.closes != 1 {
				t.Fatalf("failure/close contract: %v, heap closes=%d, CPU closes=%d", err, test.writer.closes, cpu.closes)
			}
			if !strings.Contains(stderr.String(), "kconfig_parse: failed to finish heap profile:") {
				t.Fatal("supervisor cannot recognize heap finalization error")
			}
		})
	}
}

func TestHeapProfileStartFailureClosesBothOutputs(t *testing.T) {
	active, err := startCPUProfile(filepath.Join(t.TempDir(), "active.pprof"), time.Hour, io.Discard, func(int) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = active.stop() })
	cpu, heap := &failingCPUProfileWriterForTest{}, &failingCPUProfileWriterForTest{}
	failed, err := startDiagnosticProfileWriter(cpu, heap, func(io.Writer) error { return nil }, time.Hour, io.Discard, func(int) {})
	if err == nil || failed != nil || cpu.closes != 1 || heap.closes != 1 {
		t.Fatal("concurrent start leaked CPU/heap output")
	}
}
