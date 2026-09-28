package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
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

func TestMarkerWriterRejectsReturnThunkAcrossWriteBoundaries(t *testing.T) {
	for split := 0; split <= len(returnThunkWarning); split++ {
		t.Run(fmt.Sprintf("split_%d", split), func(t *testing.T) {
			writer := newMarkerWriter(io.Discard, "LINUX_BZL_BOOT_OK")
			for _, part := range []string{returnThunkWarning[:split], "", returnThunkWarning[split:], "\nLINUX_BZL_BOOT_OK\n"} {
				if _, err := io.WriteString(writer, part); err != nil {
					t.Fatal(err)
				}
			}
			if !writer.MarkerFound() || writer.GuestError() == nil {
				t.Fatal("split return-thunk warning was overridden by success marker")
			}
			if len(writer.returnThunkTail) >= len(returnThunkWarning) {
				t.Fatal("warning matcher retained an unbounded stream")
			}
		})
	}
	writer := newMarkerWriter(io.Discard, "LINUX_BZL_BOOT_OK")
	for _, b := range []byte(returnThunkWarning) {
		if _, err := writer.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.GuestError(); err == nil {
		t.Fatal("byte-at-a-time warning was not detected before any marker")
	}
}

func TestMarkerWriterReturnThunkWinsBeforeAndAfterMarker(t *testing.T) {
	const marker = "LINUX_BZL_MODVERSIONS_OK"
	for name, chunks := range map[string][]string{
		"warning_then_marker":       {returnThunkWarning + "\n", marker + "\n"},
		"marker_then_warning":       {marker + "\n", returnThunkWarning + "\n"},
		"marker_warning_one_write":  {marker + "\n" + returnThunkWarning + "\n"},
		"warning_marker_one_write":  {returnThunkWarning + "\n" + marker + "\n"},
		"marker_then_split_warning": {marker + "\nUnpatched return thunk in use.", " This should not happen!\n"},
	} {
		t.Run(name, func(t *testing.T) {
			writer := newMarkerWriter(io.Discard, marker)
			for _, chunk := range chunks {
				if _, err := io.WriteString(writer, chunk); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-writer.found:
			default:
				t.Fatal("existing marker signal changed")
			}
			if err := writer.GuestError(); err == nil || !strings.Contains(err.Error(), returnThunkWarning) {
				t.Fatalf("drained guest error = %v", err)
			}
		})
	}
}

func TestMarkerWriterRetainsReturnThunkFailureAfterCaptureTruncation(t *testing.T) {
	writer := newMarkerWriter(io.Discard, "LINUX_BZL_BOOT_OK")
	if _, err := io.WriteString(writer, returnThunkWarning+"\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(bytes.Repeat([]byte{'x'}, serialCaptureLimit+1)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, "\nLINUX_BZL_BOOT_OK\n"); err != nil {
		t.Fatal(err)
	}
	if capture := writer.String(); !strings.HasPrefix(capture, serialTruncatedNote) || strings.Contains(capture, returnThunkWarning) {
		t.Fatal("test did not evict the warning from bounded capture")
	}
	if !writer.MarkerFound() {
		t.Fatal("success marker not detected")
	}
	if err := writer.GuestError(); err == nil || !strings.Contains(err.Error(), returnThunkWarning) {
		t.Fatalf("evicted warning lost permanent failure: %v", err)
	}
}

func TestMarkerWriterAllowsExpectedCRCMismatchAndUnrelatedWarnings(t *testing.T) {
	writer := newMarkerWriter(io.Discard, "LINUX_BZL_MODVERSIONS_OK")
	for _, part := range []string{
		"hello: disagrees about version of symbol module_layout\n",
		"module_layout: expected CRC mismatch for deliberately altered module\n",
		"WARNING: unrelated guest message\n",
		"Unpatched return thunk in use. This should not happen?\n",
		"LINUX_BZL_MODVERSIONS_OK\n",
	} {
		if _, err := io.WriteString(writer, part); err != nil {
			t.Fatal(err)
		}
	}
	if !writer.MarkerFound() || writer.GuestError() != nil {
		t.Fatal("expected CRC mismatch or unrelated warning rejected")
	}
}

func TestMarkerWriterReturnThunkConcurrentAccess(t *testing.T) {
	writer := newMarkerWriter(io.Discard, "LINUX_BZL_BOOT_OK")
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 16 {
				if _, err := io.WriteString(writer, "ordinary serial output\n"); err != nil {
					t.Error(err)
				}
				_ = writer.MarkerFound()
				_ = writer.GuestError()
				_ = writer.String()
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		if _, err := io.WriteString(writer, "LINUX_BZL_BOOT_OK\n"+returnThunkWarning+"\n"); err != nil {
			t.Error(err)
		}
	}()
	workers.Wait()
	if !writer.MarkerFound() || writer.GuestError() == nil {
		t.Fatal("concurrent writers/readers lost guest failure")
	}
}

type shortSerialWriter struct {
	n   int
	err error
}

func (w shortSerialWriter) Write([]byte) (int, error) { return w.n, w.err }

func TestMarkerWriterPreservesOutputWriteErrorBehavior(t *testing.T) {
	const marker = "LINUX_BZL_BOOT_OK"
	wantErr := errors.New("serial destination failure")
	writer := newMarkerWriter(shortSerialWriter{n: len(marker), err: wantErr}, marker)
	n, err := writer.Write([]byte(marker + returnThunkWarning))
	if n != len(marker) || err != wantErr || !writer.MarkerFound() || writer.GuestError() != nil || writer.String() != marker {
		t.Fatalf("partial output write changed: n=%d err=%v", n, err)
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
