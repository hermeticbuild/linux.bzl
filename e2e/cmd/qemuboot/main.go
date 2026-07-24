package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	configEnvironment   = "LINUX_BZL_QEMU_CONFIG"
	serialCaptureLimit  = 1 << 20
	serialTruncatedNote = "[... serial output truncated; showing last 1048576 bytes ...]\n"
)

type bootConfig struct {
	Arch             string   `json:"arch"`
	QEMUSystem       string   `json:"qemu_system"`
	SystemDataAnchor string   `json:"system_data_anchor"`
	Machine          string   `json:"machine"`
	Accel            string   `json:"accel"`
	Kernel           string   `json:"kernel"`
	Initramfs        string   `json:"initramfs"`
	KernelArgs       []string `json:"kernel_args"`
	QEMUArgs         []string `json:"qemu_args"`
	Expect           string   `json:"expect"`
	TimeoutSeconds   int      `json:"timeout_seconds"`
}

type markerWriter struct {
	mu         sync.Mutex
	output     io.Writer
	marker     []byte
	markerTail []byte
	matched    bool
	found      chan struct{}
	once       sync.Once
	capture    tailBuffer
}

type tailBuffer struct {
	data      []byte
	start     int
	size      int
	truncated bool
}

func newMarkerWriter(output io.Writer, marker string) *markerWriter {
	return &markerWriter{
		output: output,
		marker: []byte(marker),
		found:  make(chan struct{}),
	}
}

func (w *markerWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err := w.output.Write(p)
	if n > 0 {
		chunk := p[:n]
		w.capture.Write(chunk)
		if w.match(chunk) {
			w.matched = true
			w.once.Do(func() { close(w.found) })
		}
	}
	return n, err
}

func (w *markerWriter) match(chunk []byte) bool {
	if bytes.Contains(chunk, w.marker) {
		w.updateMarkerTail(chunk)
		return true
	}

	tailLimit := len(w.marker) - 1
	if len(w.markerTail) > 0 && tailLimit > 0 {
		prefixLength := min(len(chunk), tailLimit)
		boundary := make([]byte, 0, len(w.markerTail)+prefixLength)
		boundary = append(boundary, w.markerTail...)
		boundary = append(boundary, chunk[:prefixLength]...)
		if bytes.Contains(boundary, w.marker) {
			w.updateMarkerTail(chunk)
			return true
		}
	}

	w.updateMarkerTail(chunk)
	return false
}

func (w *markerWriter) updateMarkerTail(chunk []byte) {
	tailLimit := len(w.marker) - 1
	if tailLimit <= 0 {
		w.markerTail = nil
		return
	}
	if len(chunk) >= tailLimit {
		w.markerTail = append(w.markerTail[:0], chunk[len(chunk)-tailLimit:]...)
		return
	}
	w.markerTail = append(w.markerTail, chunk...)
	if len(w.markerTail) > tailLimit {
		copy(w.markerTail, w.markerTail[len(w.markerTail)-tailLimit:])
		w.markerTail = w.markerTail[:tailLimit]
	}
}

func (w *markerWriter) MarkerFound() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.matched
}

func (w *markerWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.capture.String()
}

func (b *tailBuffer) Write(p []byte) {
	if len(p) == 0 {
		return
	}
	if b.data == nil {
		b.data = make([]byte, serialCaptureLimit)
	}
	if len(p) >= len(b.data) {
		b.truncated = b.truncated || b.size > 0 || len(p) > len(b.data)
		copy(b.data, p[len(p)-len(b.data):])
		b.start = 0
		b.size = len(b.data)
		return
	}

	free := len(b.data) - b.size
	if len(p) > free {
		dropped := len(p) - free
		b.start = (b.start + dropped) % len(b.data)
		b.size -= dropped
		b.truncated = true
	}

	end := (b.start + b.size) % len(b.data)
	first := min(len(p), len(b.data)-end)
	copy(b.data[end:], p[:first])
	copy(b.data, p[first:])
	b.size += len(p)
}

func (b *tailBuffer) String() string {
	if b.size == 0 {
		return ""
	}
	ordered := make([]byte, b.size)
	first := min(b.size, len(b.data)-b.start)
	copy(ordered, b.data[b.start:b.start+first])
	copy(ordered[first:], b.data[:b.size-first])
	if b.truncated {
		return serialTruncatedNote + string(ordered)
	}
	return string(ordered)
}

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "qemuboot: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	configPath, err := findConfigPath(args, os.Getenv)
	if err != nil {
		return err
	}
	configPath, err = resolveRunfile(configPath, os.Getenv)
	if err != nil {
		return fmt.Errorf("resolve config: %w", err)
	}

	config, err := readConfig(configPath)
	if err != nil {
		return err
	}
	if err := resolveConfigPaths(&config, os.Getenv); err != nil {
		return err
	}

	command, commandArgs, err := qemuCommand(config)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "qemuboot: command: %s\n", formatCommand(command, commandArgs))
	cmd := exec.Command(command, commandArgs...)
	cmd.Env = []string{
		"HOME=" + os.Getenv("TEST_TMPDIR"),
		"LC_ALL=C",
		"TMPDIR=" + os.Getenv("TEST_TMPDIR"),
		"TZ=UTC",
	}

	serial := newMarkerWriter(output, config.Expect)
	cmd.Stdout = serial
	cmd.Stderr = serial
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start QEMU: %w", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	timer := time.NewTimer(time.Duration(config.TimeoutSeconds) * time.Second)
	defer timer.Stop()

	select {
	case <-serial.found:
		if err := stopProcess(cmd, done); err != nil {
			return fmt.Errorf("stop QEMU after marker: %w", err)
		}
		return nil
	case err := <-done:
		if serial.MarkerFound() {
			return nil
		}
		if err == nil {
			return fmt.Errorf("QEMU exited before marker %q\nserial output:\n%s", config.Expect, serial.String())
		}
		return fmt.Errorf("QEMU exited before marker %q: %w\nserial output:\n%s", config.Expect, err, serial.String())
	case <-timer.C:
		stopErr := stopProcess(cmd, done)
		if stopErr != nil {
			return fmt.Errorf("timed out after %ds waiting for marker %q; stopping QEMU: %w\nserial output:\n%s", config.TimeoutSeconds, config.Expect, stopErr, serial.String())
		}
		return fmt.Errorf("timed out after %ds waiting for marker %q\nserial output:\n%s", config.TimeoutSeconds, config.Expect, serial.String())
	}
}

func stopProcess(cmd *exec.Cmd, done <-chan error) error {
	killErr := cmd.Process.Kill()
	waitErr := <-done
	if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
		return killErr
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return waitErr
	}
	return nil
}

func findConfigPath(args []string, getenv func(string) string) (string, error) {
	if len(args) > 1 {
		return "", errors.New("usage: qemuboot [CONFIG]")
	}
	if len(args) == 1 {
		if args[0] == "" {
			return "", errors.New("config path must not be empty")
		}
		return args[0], nil
	}
	if path := getenv(configEnvironment); path != "" {
		return path, nil
	}
	return "", fmt.Errorf("pass CONFIG or set %s", configEnvironment)
}

func readConfig(path string) (bootConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return bootConfig{}, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()

	var config bootConfig
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return bootConfig{}, fmt.Errorf("decode config: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return bootConfig{}, errors.New("decode config: trailing JSON value")
		}
		return bootConfig{}, fmt.Errorf("decode config: %w", err)
	}
	if err := validateConfig(config); err != nil {
		return bootConfig{}, fmt.Errorf("invalid config: %w", err)
	}
	return config, nil
}

func validateConfig(config bootConfig) error {
	if config.Arch != "x86_64" && config.Arch != "aarch64" {
		return fmt.Errorf("arch must be x86_64 or aarch64, got %q", config.Arch)
	}
	for name, value := range map[string]string{
		"qemu_system":        config.QEMUSystem,
		"system_data_anchor": config.SystemDataAnchor,
		"machine":            config.Machine,
		"accel":              config.Accel,
		"kernel":             config.Kernel,
		"initramfs":          config.Initramfs,
		"expect":             config.Expect,
	} {
		if value == "" {
			return fmt.Errorf("%s must not be empty", name)
		}
	}
	if config.TimeoutSeconds <= 0 {
		return fmt.Errorf("timeout_seconds must be positive, got %d", config.TimeoutSeconds)
	}
	return nil
}

func resolveConfigPaths(config *bootConfig, getenv func(string) string) error {
	for name, path := range map[string]*string{
		"qemu_system":        &config.QEMUSystem,
		"system_data_anchor": &config.SystemDataAnchor,
		"kernel":             &config.Kernel,
		"initramfs":          &config.Initramfs,
	} {
		resolved, err := resolveRunfile(*path, getenv)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", name, err)
		}
		*path = resolved
	}
	return nil
}

func resolveRunfile(path string, getenv func(string) string) (string, error) {
	if path == "" {
		return "", errors.New("empty runfile path")
	}
	if filepath.IsAbs(path) {
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		return path, nil
	}

	candidates := []string{path}
	for _, variable := range []string{"RUNFILES_DIR", "TEST_SRCDIR"} {
		if root := getenv(variable); root != "" {
			candidates = append(candidates, filepath.Join(root, filepath.FromSlash(path)))
			if workspace := getenv("TEST_WORKSPACE"); workspace != "" {
				candidates = append(candidates, filepath.Join(root, workspace, filepath.FromSlash(path)))
			}
		}
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			absolute, err := filepath.Abs(candidate)
			if err != nil {
				return "", err
			}
			return absolute, nil
		}
	}

	if manifest := getenv("RUNFILES_MANIFEST_FILE"); manifest != "" {
		resolved, err := resolveManifestEntry(manifest, path)
		if err == nil {
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("%s: %w", path, os.ErrNotExist)
}

func resolveManifestEntry(manifest, path string) (string, error) {
	data, err := os.ReadFile(manifest)
	if err != nil {
		return "", err
	}
	prefix := path + " "
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			value := strings.TrimPrefix(line, prefix)
			if value == "" {
				return "", fmt.Errorf("empty manifest value for %s", path)
			}
			return value, nil
		}
		if line == path {
			return path, nil
		}
	}
	return "", os.ErrNotExist
}

func qemuCommand(config bootConfig) (string, []string, error) {
	if err := validateConfig(config); err != nil {
		return "", nil, fmt.Errorf("invalid config: %w", err)
	}
	args := []string{
		"-L", config.SystemDataAnchor,
		"-machine", config.Machine,
		"-accel", config.Accel,
		"-m", "512M",
		"-smp", "1",
		"-display", "none",
		"-monitor", "none",
		"-serial", "stdio",
		"-nic", "none",
		"-no-reboot",
		"-no-shutdown",
	}
	if config.Arch == "aarch64" {
		args = append(args, "-cpu", "max")
	}
	args = append(args,
		"-kernel", config.Kernel,
		"-initrd", config.Initramfs,
		"-append", strings.Join(config.KernelArgs, " "),
	)
	args = append(args, config.QEMUArgs...)
	return config.QEMUSystem, args, nil
}

func formatCommand(command string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, strconv.Quote(command))
	for _, arg := range args {
		parts = append(parts, strconv.Quote(arg))
	}
	return strings.Join(parts, " ")
}

func getenvMap(values map[string]string) func(string) string {
	return func(key string) string {
		return values[key]
	}
}
