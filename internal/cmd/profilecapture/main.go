// profilecapture runs a diagnostic copy of one compiler-guard planner action.
// It publishes only a checked gzip capture, never a planner result or receipt.
// A successful go tool pprof parse is still required before interpreting it.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	profileFlushMargin       = 15 * time.Second
	maxCaptureDuration       = time.Hour
	maxCompressedProfile     = 16 << 20
	maxDecompressedProfile   = 128 << 20
	maxForwardedDiagnostics  = 1 << 20
	profileFinalizationError = "kconfig_parse: failed to finish CPU profile:"
	heapFinalizationError    = "kconfig_parse: failed to finish heap profile:"
)

type captureOptions struct {
	planner   string
	kind      string
	out       string
	duration  time.Duration
	arguments []string
}

// Only output destinations change. This is deliberately not a parser or
// reserializer for arbitrary planner flags, compiler arguments or manifests.
func capturePlannerArguments(original []string, scratch string, duration time.Duration) ([]string, error) {
	return capturePlannerArgumentsForKind(original, scratch, duration, "cpu")
}

func capturePlannerArgumentsForKind(original []string, scratch string, duration time.Duration, kind string) ([]string, error) {
	if kind != "cpu" && kind != "heap" {
		return nil, fmt.Errorf("profile kind must be cpu or heap")
	}
	arguments := append([]string(nil), original...)
	seen := map[string]bool{}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if strings.HasPrefix(argument, "@") || argument == "--" {
			return nil, fmt.Errorf("response files and positional-argument separators are unsupported")
		}
		name, value, joined := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(argument, "-"), "-"), "=")
		if !strings.HasPrefix(argument, "-") {
			continue
		}
		switch name {
		case "cpu_profile", "heap_profile", "profile_duration":
			return nil, fmt.Errorf("original planner argv already contains -%s", name)
		case "family_execution_mode", "family_compiler_guard_manifest_out", "family_compiler_guard_plan_out":
			if seen[name] {
				return nil, fmt.Errorf("duplicate -%s", name)
			}
			seen[name] = true
			if !joined {
				if index+1 == len(arguments) || arguments[index+1] == "" || strings.HasPrefix(arguments[index+1], "-") || strings.HasPrefix(arguments[index+1], "@") {
					return nil, fmt.Errorf("missing operand for -%s", name)
				}
				index++
				value = arguments[index]
			}
			if value == "" || strings.HasPrefix(value, "@") {
				return nil, fmt.Errorf("invalid operand for -%s", name)
			}
			if name == "family_execution_mode" {
				if value != "guards" {
					return nil, fmt.Errorf("profile capture requires -family_execution_mode guards")
				}
				continue
			}
			replacement := filepath.Join(scratch, "manifest.json")
			if name == "family_compiler_guard_plan_out" {
				replacement = filepath.Join(scratch, "plan")
			}
			if joined {
				prefix, _, _ := strings.Cut(argument, "=")
				arguments[index] = prefix + "=" + replacement
			} else {
				arguments[index] = replacement
			}
		}
	}
	for _, name := range []string{"family_execution_mode", "family_compiler_guard_manifest_out", "family_compiler_guard_plan_out"} {
		if !seen[name] {
			return nil, fmt.Errorf("missing -%s", name)
		}
	}
	arguments = append(arguments, "-cpu_profile", filepath.Join(scratch, "cpu.pprof"), "-profile_duration", duration.String())
	if kind == "heap" {
		arguments = append(arguments, "-heap_profile", filepath.Join(scratch, "heap.pprof"))
	}
	return arguments, nil
}

// Keep only a short overlap for diagnostic markers, even when forwarding has
// reached its bound. A finalization error after that bound must still fail.
type captureDiagnostics struct {
	output       io.Writer
	remaining    int
	deadline     string
	tail         string
	expired      bool
	finishFailed bool
	writeErr     error
}

func (d *captureDiagnostics) Write(data []byte) (int, error) {
	text := d.tail + string(data)
	d.expired = d.expired || d.deadline != "" && strings.Contains(text, d.deadline)
	d.finishFailed = d.finishFailed || strings.Contains(text, profileFinalizationError) || strings.Contains(text, heapFinalizationError)
	keep := max(len(d.deadline), len(profileFinalizationError), len(heapFinalizationError)) - 1
	if keep > 0 {
		d.tail = strings.Clone(text[max(0, len(text)-keep):])
	}
	if d.writeErr == nil && d.remaining > 0 {
		part := data[:min(len(data), d.remaining)]
		n, err := d.output.Write(part)
		d.remaining -= n
		if err == nil && n != len(part) {
			err = io.ErrShortWrite
		}
		d.writeErr = err
	}
	// Always drain the child pipe; log truncation must not change its behavior.
	return len(data), nil
}

func profileDeadlineMessage(duration time.Duration) string {
	return fmt.Sprintf("kconfig_parse: CPU profile duration %s reached; profiling stopped; exiting 124\n", duration)
}

func validateCapturedGzip(filename string, compressedLimit, decompressedLimit int64) ([]byte, error) {
	before, err := os.Lstat(filename)
	if err != nil {
		return nil, fmt.Errorf("inspect captured profile: %w", err)
	}
	if !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > compressedLimit {
		return nil, fmt.Errorf("captured profile must be a nonempty bounded regular file, not a symlink")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	after, statErr := file.Stat()
	if statErr != nil || !os.SameFile(before, after) {
		return nil, errors.Join(fmt.Errorf("captured profile changed while opening"), statErr, file.Close())
	}
	data, readErr := io.ReadAll(io.LimitReader(file, compressedLimit+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if len(data) == 0 || int64(len(data)) > compressedLimit {
		return nil, fmt.Errorf("compressed profile exceeds capture budget")
	}
	input := bytes.NewReader(data)
	reader, err := gzip.NewReader(input)
	if err != nil {
		return nil, fmt.Errorf("captured profile is not gzip: %w", err)
	}
	reader.Multistream(false)
	size, readErr := io.Copy(io.Discard, io.LimitReader(reader, decompressedLimit+1))
	closeErr = reader.Close()
	if readErr != nil || closeErr != nil {
		return nil, fmt.Errorf("captured gzip is incomplete: %w", errors.Join(readErr, closeErr))
	}
	if size == 0 || size > decompressedLimit || input.Len() != 0 {
		return nil, fmt.Errorf("captured gzip is empty, oversized, or has trailing data")
	}
	return data, nil
}

func captureProfile(parent context.Context, options captureOptions, stdout, stderr io.Writer) error {
	if options.kind == "" {
		options.kind = "cpu"
	}
	if options.kind != "cpu" && options.kind != "heap" {
		return fmt.Errorf("profile kind must be cpu or heap")
	}
	if options.planner == "" || options.out == "" || options.duration <= 0 || options.duration > maxCaptureDuration {
		return fmt.Errorf("-planner, -out and a positive -duration no greater than %s are required", maxCaptureDuration)
	}
	planner, err := filepath.Abs(options.planner)
	if err != nil {
		return err
	}
	out, err := filepath.Abs(options.out)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(out); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("capture output must not already exist: %s (%v)", out, err)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	// A sibling scratch directory keeps the final rename on one filesystem.
	// Its paths are output operands only; the child's cwd/TMPDIR stay unchanged.
	scratch, err := os.MkdirTemp(filepath.Dir(out), ".profilecapture-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	arguments, err := capturePlannerArgumentsForKind(options.arguments, scratch, options.duration, options.kind)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, options.duration+profileFlushMargin)
	defer cancel()
	command := exec.CommandContext(ctx, planner, arguments...)
	command.Args[0] = options.planner
	command.Env = os.Environ()
	command.WaitDelay = 2 * time.Second
	output := &captureDiagnostics{output: stdout, remaining: maxForwardedDiagnostics}
	diagnostics := &captureDiagnostics{output: stderr, remaining: maxForwardedDiagnostics, deadline: profileDeadlineMessage(options.duration)}
	command.Stdout, command.Stderr = output, diagnostics
	runErr := command.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("profile capture outer deadline/cancellation: %w", ctx.Err())
	}
	if diagnostics.writeErr != nil || output.writeErr != nil {
		return fmt.Errorf("forward profile diagnostics: %w", errors.Join(diagnostics.writeErr, output.writeErr))
	}
	if runErr != nil {
		var exited *exec.ExitError
		if !errors.As(runErr, &exited) || exited.ExitCode() != 124 || !diagnostics.expired {
			return fmt.Errorf("profile planner failed: %w", runErr)
		}
	} else if diagnostics.expired {
		return fmt.Errorf("profile planner reported deadline expiry but exited zero")
	}
	if diagnostics.finishFailed {
		return fmt.Errorf("profile planner reported a profile finalization error")
	}
	data, err := validateCapturedGzip(filepath.Join(scratch, "cpu.pprof"), maxCompressedProfile, maxDecompressedProfile)
	if err != nil {
		return err
	}
	if options.kind == "heap" {
		// Require the original CPU lifecycle to flush too, but publish only the
		// heap artifact. Neither scratch output is a planner receipt.
		data, err = validateCapturedGzip(filepath.Join(scratch, "heap.pprof"), maxCompressedProfile, maxDecompressedProfile)
		if err != nil {
			return err
		}
	}
	// Publish the exact validated bytes, not a mutable child-owned pathname.
	validated := filepath.Join(scratch, "validated.pprof")
	if err := os.WriteFile(validated, data, 0o644); err != nil {
		return err
	}
	if _, err := os.Lstat(out); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("capture output appeared before publication: %s (%v)", out, err)
	}
	if err := os.Rename(validated, out); err != nil {
		return err
	}
	fmt.Fprintln(stderr, "profilecapture: gzip capture only; run go tool pprof -top before interpreting samples; no planner result published")
	return nil
}

func run(arguments []string, stdout, stderr io.Writer) int {
	separator := -1
	for index, argument := range arguments {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 {
		fmt.Fprintln(stderr, "profilecapture: -- must precede the exact original planner argv")
		return 2
	}
	flags := flag.NewFlagSet("profilecapture", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var options captureOptions
	flags.StringVar(&options.planner, "planner", "", "declared planner executable (heap mode uses the diagnostic-only binary)")
	flags.StringVar(&options.kind, "kind", "cpu", "published profile: cpu or heap")
	flags.StringVar(&options.out, "out", "", "declared diagnostic gzip output")
	flags.DurationVar(&options.duration, "duration", 0, "positive CPU-profile duration, at most 1h")
	if err := flags.Parse(arguments[:separator]); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "profilecapture: unexpected supervisor operands")
		return 2
	}
	options.arguments = arguments[separator+1:]
	if err := captureProfile(context.Background(), options, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "profilecapture: %v\n", err)
		var exited *exec.ExitError
		if errors.As(err, &exited) && exited.ExitCode() > 0 {
			return exited.ExitCode()
		}
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
