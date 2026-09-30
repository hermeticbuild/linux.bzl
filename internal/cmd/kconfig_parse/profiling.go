package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

const cpuProfileDurationExitCode = 124

// Only the separate diagnostic binary installs this callback. Do not reference
// pprof.WriteHeapProfile here: doing so can enable Go's allocation sampling in
// the ordinary planner even when no profiling flags were supplied.
var diagnosticHeapWriter func(io.Writer) error

func validateDiagnosticProfileOptions(cpu, heap string, duration time.Duration) error {
	if err := validateCPUProfileOptions(cpu, duration); err != nil {
		return err
	}
	if heap == "" {
		return nil
	}
	if cpu == "" || diagnosticHeapWriter == nil {
		return fmt.Errorf("-heap_profile requires the heap diagnostic binary and bounded CPU profiling")
	}
	cpuPath, cpuErr := filepath.Abs(cpu)
	heapPath, heapErr := filepath.Abs(heap)
	if cpuErr != nil || heapErr != nil {
		return errors.Join(cpuErr, heapErr)
	}
	if cpuPath == heapPath {
		return fmt.Errorf("CPU and heap profile outputs must be distinct")
	}
	return nil
}

// CPU profiling is an opt-in diagnostic of this process, not a planner input.
// A positive duration is required so even a stuck planner finishes its profile
// and exits before the remote worker's independent timeout can kill it.
func validateCPUProfileOptions(filename string, duration time.Duration) error {
	if filename == "" && duration == 0 {
		return nil
	}
	if filename == "" || duration <= 0 {
		return fmt.Errorf("-cpu_profile and a positive -profile_duration must be supplied together")
	}
	return nil
}

type cpuProfileWriter struct {
	writer io.WriteCloser
	mu     sync.Mutex
	err    error
}

// pprof does not return asynchronous writer errors from StopCPUProfile.
func (w *cpuProfileWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.writer.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if w.err == nil {
		w.err = err
	}
	return n, err
}

type cpuProfileSession struct {
	mu         sync.Mutex
	output     *cpuProfileWriter
	heapOutput *cpuProfileWriter
	writeHeap  func(io.Writer) error
	stderr     io.Writer
	exit       func(int)
	timer      *time.Timer
	started    time.Time
	duration   time.Duration
	finished   bool
	stopErr    error

	// labelParent belongs only to the planning goroutine. The timer reads
	// value-only progress snapshots, never this context or mutable planner state.
	labelParent context.Context
	progress    atomic.Pointer[kconfig.ConfigDependencyDiagnosticSnapshot]
}

func startCPUProfile(filename string, duration time.Duration, stderr io.Writer, exit func(int)) (*cpuProfileSession, error) {
	return startDiagnosticProfile(filename, "", duration, stderr, exit)
}

func startDiagnosticProfile(filename, heapFilename string, duration time.Duration, stderr io.Writer, exit func(int)) (*cpuProfileSession, error) {
	if err := validateDiagnosticProfileOptions(filename, heapFilename, duration); err != nil {
		return nil, err
	}
	if filename == "" {
		return nil, nil
	}
	output, err := os.Create(filename)
	if err != nil {
		return nil, fmt.Errorf("create CPU profile: %w", err)
	}
	var heap io.WriteCloser
	if heapFilename != "" {
		heapFile, err := os.OpenFile(heapFilename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("create heap profile: %w", err), output.Close())
		}
		heap = heapFile
	}
	return startDiagnosticProfileWriter(output, heap, diagnosticHeapWriter, duration, stderr, exit)
}

// The writer/exit parameters allow lifecycle tests without terminating the
// test process. In production exit is os.Exit and does not return.
func startCPUProfileWriter(output io.WriteCloser, duration time.Duration, stderr io.Writer, exit func(int)) (*cpuProfileSession, error) {
	return startDiagnosticProfileWriter(output, nil, nil, duration, stderr, exit)
}

func startDiagnosticProfileWriter(output, heap io.WriteCloser, writeHeap func(io.Writer) error, duration time.Duration, stderr io.Writer, exit func(int)) (*cpuProfileSession, error) {
	if heap != nil && writeHeap == nil {
		return nil, errors.Join(fmt.Errorf("heap profiling writer is unavailable"), output.Close(), heap.Close())
	}
	session := &cpuProfileSession{
		output: &cpuProfileWriter{writer: output}, stderr: stderr, exit: exit,
		started: time.Now(), duration: duration,
		labelParent: context.Background(),
	}
	if heap != nil {
		session.heapOutput = &cpuProfileWriter{writer: heap}
		session.writeHeap = writeHeap
	}
	if err := pprof.StartCPUProfile(session.output); err != nil {
		var heapErr error
		if heap != nil {
			heapErr = heap.Close()
		}
		return nil, errors.Join(fmt.Errorf("start CPU profile: %w", err), output.Close(), heapErr)
	}
	// Lock through timer assignment: even an immediately expiring timer must
	// observe the initialized timer, and cannot race normal finalization.
	session.mu.Lock()
	session.timer = time.AfterFunc(duration, session.expire)
	session.mu.Unlock()
	return session, nil
}

func (s *cpuProfileSession) finishLocked() {
	if s.finished {
		return
	}
	s.finished = true
	s.timer.Stop()
	// Capture at the sampling boundary, before the profiler drains its writer.
	// Node work may advance during StopCPUProfile, but must not change this view.
	progress := s.progress.Load()
	capturedAt := time.Now()
	pprof.StopCPUProfile() // waits until every buffered profile write completes
	s.output.mu.Lock()
	s.stopErr = errors.Join(s.output.err, s.output.writer.Close())
	s.output.mu.Unlock()
	if s.heapOutput != nil {
		// No forced GC: this is Go's sampled, most recently published heap
		// profile, not a dump of the planner's mutable object graph. Planning
		// may advance while profiles flush; timestamps make that boundary clear.
		began := time.Now()
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		fmt.Fprintf(s.stderr, "kconfig_parse profile: heap_capture_begin elapsed=%s memprofile_rate=%d gc=%d last_gc_unix_ns=%d forced_gc=false\n",
			began.Sub(s.started).Round(time.Millisecond), runtime.MemProfileRate, memory.NumGC, memory.LastGC)
		writeErr := s.writeHeap(s.heapOutput)
		s.heapOutput.mu.Lock()
		heapErr := errors.Join(writeErr, s.heapOutput.err, s.heapOutput.writer.Close())
		s.heapOutput.mu.Unlock()
		if heapErr != nil {
			fmt.Fprintf(s.stderr, "kconfig_parse: failed to finish heap profile: %v\n", heapErr)
			s.stopErr = errors.Join(s.stopErr, heapErr)
		}
		fmt.Fprintf(s.stderr, "kconfig_parse profile: heap_capture_end elapsed=%s write_duration=%s\n",
			time.Since(s.started).Round(time.Millisecond), time.Since(began).Round(time.Millisecond))
	}
	if progress != nil {
		if encoded, err := json.Marshal(progress); err != nil {
			fmt.Fprintf(s.stderr, "kconfig_parse profile: failed to encode config dependency progress: %v\n", err)
		} else {
			fmt.Fprintf(s.stderr, "kconfig_parse profile: config_dependencies=%s snapshot_age=%s\n",
				encoded, capturedAt.Sub(progress.PublishedAt).Round(time.Millisecond))
		}
	}
}

func (s *cpuProfileSession) expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.finishLocked()
	fmt.Fprintf(s.stderr, "kconfig_parse: CPU profile duration %s reached; profiling stopped; exiting %d\n", s.duration, cpuProfileDurationExitCode)
	if s.stopErr != nil {
		fmt.Fprintf(s.stderr, "kconfig_parse: failed to finish CPU profile: %v\n", s.stopErr)
	}
	// Keep the lock until process exit: a racing normal return must not win
	// with status zero after the timer has selected diagnostic termination.
	s.exit(cpuProfileDurationExitCode)
}

func (s *cpuProfileSession) stop() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishLocked()
	return s.stopErr
}

func (s *cpuProfileSession) finishExitCode(code int) int {
	if err := s.stop(); err != nil {
		fmt.Fprintf(s.stderr, "kconfig_parse: failed to finish CPU profile: %v\n", err)
		if code == 0 {
			return 1
		}
	}
	return code
}

// Markers are coarse, opt-in, and serialized with timer finalization. Never
// traverse planner state here: timer diagnostics must not race mutable plans.
func (s *cpuProfileSession) phase(name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.labelParent = pprof.WithLabels(context.Background(), pprof.Labels("phase", name))
	pprof.SetGoroutineLabels(s.labelParent)
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	fmt.Fprintf(s.stderr, "kconfig_parse profile: phase=%q go_version=%q elapsed=%s heap_bytes=%d total_alloc_bytes=%d gc=%d memprofile_rate=%d\n",
		name, runtime.Version(), time.Since(s.started).Round(time.Millisecond), memory.HeapAlloc, memory.TotalAlloc, memory.NumGC, runtime.MemProfileRate)
}

// These labels describe a compiler node, which may contain multiple selected
// profiles or translation units. They survive truncated deep include stacks in
// CPU samples, but do not attribute background GC or measure wall-clock time.
func configDependencyProfileLabels(parent context.Context, snapshot kconfig.ConfigDependencyDiagnosticSnapshot) context.Context {
	if parent == nil {
		parent = context.Background()
	}
	if snapshot.Event == "analysis_end" {
		return parent
	}
	labels := []string{"analysis", strconv.FormatUint(snapshot.AnalysisSequence, 10)}
	if snapshot.Event == "compiler_start" && snapshot.CurrentOutput != "" {
		labels = append(labels, "compile_output", snapshot.CurrentOutput)
	}
	return pprof.WithLabels(parent, pprof.Labels(labels...))
}

// Called synchronously by the planning goroutine only. Publication never takes
// the profile mutex: deadline expiry must not wait for a planner callback.
func (s *cpuProfileSession) observeConfigDependencies(snapshot kconfig.ConfigDependencyDiagnosticSnapshot) {
	if s == nil {
		return
	}
	pprof.SetGoroutineLabels(configDependencyProfileLabels(s.labelParent, snapshot))
	s.progress.Store(&snapshot)
}
