package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

type probeCandidatePathRewrite struct {
	start int
	end   int
	value string
}

// validateAndRewriteProbeCandidateArguments applies the security grammar to
// the fully rendered source-owned argv and then binds every filesystem operand
// to an input of this action. candidateIndexes maps each source-owned argv word
// back to its position in arguments; managed probe arguments never enter the
// validator and therefore cannot be mistaken for source-controlled compiler
// mode or output selection.
func validateAndRewriteProbeCandidateArguments(
	stepName, policy string,
	arguments, candidates []string,
	candidateIndexes []int,
	scratchRoot, workingDirectory, execroot string,
	sources, sourceRoots map[string]string,
	toolsetPaths *toolsetPathResolver,
) ([]string, error) {
	if len(candidates) != len(candidateIndexes) {
		return nil, fmt.Errorf("step %s candidate provenance has %d words and %d argument indexes", stepName, len(candidates), len(candidateIndexes))
	}
	operands, err := kconfig.ValidateProbeCandidateArguments(policy, candidates)
	if err != nil {
		return nil, fmt.Errorf("step %s candidate arguments: %w", stepName, err)
	}
	for _, candidate := range candidates {
		if candidate == "-gsplit-dwarf" && filepath.Clean(workingDirectory) != filepath.Clean(scratchRoot) {
			return nil, fmt.Errorf("step %s split-DWARF candidate requires the private scratch working directory", stepName)
		}
	}
	rewrites := make(map[int][]probeCandidatePathRewrite)
	for operandIndex, operand := range operands {
		if operand.Argument < 0 || operand.Argument >= len(candidates) {
			return nil, fmt.Errorf("step %s candidate path operand %d references argument %d", stepName, operandIndex, operand.Argument)
		}
		argument := candidates[operand.Argument]
		if operand.Start < 0 || operand.End <= operand.Start || operand.End > len(argument) {
			return nil, fmt.Errorf("step %s candidate path operand %d has invalid byte range [%d:%d] for argument %d", stepName, operandIndex, operand.Start, operand.End, operand.Argument)
		}
		resolved, err := resolveProbeCandidatePath(
			operand.Kind,
			argument[operand.Start:operand.End],
			scratchRoot,
			workingDirectory,
			execroot,
			sources,
			sourceRoots,
			toolsetPaths,
		)
		if err != nil {
			return nil, fmt.Errorf("step %s candidate path operand %d: %w", stepName, operandIndex, err)
		}
		if probeCandidateForwardedArgument(argument) && strings.ContainsRune(resolved, ',') {
			return nil, fmt.Errorf("step %s candidate path operand %d resolves to comma-bearing path %q inside forwarded argument %q", stepName, operandIndex, resolved, argument)
		}
		rewrites[operand.Argument] = append(rewrites[operand.Argument], probeCandidatePathRewrite{
			start: operand.Start,
			end:   operand.End,
			value: resolved,
		})
	}

	out := append([]string(nil), arguments...)
	for candidateIndex, edits := range rewrites {
		argumentIndex := candidateIndexes[candidateIndex]
		if argumentIndex < 0 || argumentIndex >= len(out) {
			return nil, fmt.Errorf("step %s candidate argument %d maps outside rendered argv", stepName, candidateIndex)
		}
		sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
		value := out[argumentIndex]
		priorStart := len(value)
		for _, edit := range edits {
			if edit.end > priorStart || edit.end > len(value) {
				return nil, fmt.Errorf("step %s candidate argument %d has overlapping path operands", stepName, candidateIndex)
			}
			value = value[:edit.start] + edit.value + value[edit.end:]
			priorStart = edit.start
		}
		out[argumentIndex] = value
	}
	return out, nil
}

func resolveProbeCandidatePath(
	kind kconfig.ProbeCandidatePathKind,
	value, scratchRoot, workingDirectory, execroot string,
	sources, sourceRoots map[string]string,
	toolsetPaths *toolsetPathResolver,
) (string, error) {
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("empty or control-character path")
	}
	if strings.HasPrefix(value, "=") {
		return "", fmt.Errorf("sysroot-relative candidate path %q is not bound to a typed input", value)
	}
	if kind == kconfig.ProbeCandidatePathRegularFile && value == "/dev/null" {
		empty := filepath.Join(scratchRoot, ".linux-bzl-empty-candidate-input")
		file, err := os.OpenFile(empty, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			err = file.Close()
		} else if os.IsExist(err) {
			info, statErr := os.Stat(empty)
			if statErr != nil || !info.Mode().IsRegular() || info.Size() != 0 {
				return "", fmt.Errorf("inspect private empty candidate input %q: %v", empty, statErr)
			}
			err = nil
		}
		if err != nil {
			return "", fmt.Errorf("create private empty candidate input: %w", err)
		}
		return empty, nil
	}

	// Durable compiler-query results use the canonical toolset namespace. Try
	// that binding before interpreting a relative spelling against the process
	// cwd; this is what lets producer and consumer actions use different Bazel
	// output-path mappings for the same resource.
	if !filepath.IsAbs(value) {
		if canonical, err := toolaction.CanonicalArtifactPath(filepath.ToSlash(value)); err == nil {
			if resolved, resolveErr := toolsetPaths.resolve(canonical); resolveErr == nil {
				physical, err := filepath.EvalSymlinks(resolved)
				if err != nil {
					return "", fmt.Errorf("resolve canonical candidate path %q: %w", canonical, err)
				}
				physical = filepath.Clean(physical)
				if err := validateProbeCandidatePathKind(kind, physical, false); err != nil {
					return "", err
				}
				return physical, nil
			}
		}
	}

	physical := filepath.Clean(value)
	if !filepath.IsAbs(physical) {
		physical = filepath.Join(workingDirectory, filepath.FromSlash(value))
	}
	allowMissing := kind == kconfig.ProbeCandidatePathInclude
	physical, err := authorizeProbeCandidatePhysicalPath(
		physical, scratchRoot, execroot, sources, sourceRoots, toolsetPaths, allowMissing,
	)
	if err != nil {
		return "", err
	}
	if err := validateProbeCandidatePathKind(kind, physical, allowMissing); err != nil {
		return "", err
	}
	return physical, nil
}

func authorizeProbeCandidatePhysicalPath(
	filename, scratchRoot, execroot string,
	sources, sourceRoots map[string]string,
	toolsetPaths *toolsetPathResolver,
	allowMissing bool,
) (string, error) {
	resolved, err := resolveProbeCandidatePhysicalPath(filename, allowMissing)
	if err != nil {
		return "", fmt.Errorf("resolve candidate path %q: %w", filename, err)
	}
	for _, root := range append([]string{scratchRoot}, sortedStringValues(sourceRoots)...) {
		if root == "" {
			continue
		}
		rootResolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return "", fmt.Errorf("resolve declared candidate root %q: %w", root, err)
		}
		if probePhysicalPathWithin(rootResolved, resolved) {
			return resolved, nil
		}
	}
	for _, source := range sources {
		sourceResolved, err := filepath.EvalSymlinks(source)
		if err == nil && filepath.Clean(sourceResolved) == resolved {
			return resolved, nil
		}
	}
	canonical, err := canonicalActionArtifactPath(execroot, filename)
	if err == nil {
		bound, resolveErr := toolsetPaths.resolve(canonical)
		if resolveErr == nil {
			boundResolved, evalErr := filepath.EvalSymlinks(bound)
			if evalErr == nil && filepath.Clean(boundResolved) == resolved {
				return resolved, nil
			}
		}
	}
	return "", fmt.Errorf("candidate path %q is outside declared source, scratch, and identity-bound toolset inputs", filename)
}

// resolveProbeCandidatePhysicalPath resolves every existing path component.
// A compiler include search directory may legitimately not exist: upstream
// Kbuild commonly puts a future output directory on -I while evaluating a
// parse-time capability probe. Missing paths confer no read authority, but
// their deepest existing ancestor still must resolve inside an authorized
// root. Forced includes and other regular-file operands keep requiring an
// existing file.
func resolveProbeCandidatePhysicalPath(filename string, allowMissing bool) (string, error) {
	resolved, err := filepath.EvalSymlinks(filename)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if !allowMissing || !os.IsNotExist(err) {
		return "", err
	}

	current := filepath.Clean(filename)
	missing := []string{}
	for {
		_, statErr := os.Lstat(current)
		if statErr == nil {
			ancestor, resolveErr := filepath.EvalSymlinks(current)
			if resolveErr != nil {
				return "", resolveErr
			}
			ancestorInfo, inspectErr := os.Stat(ancestor)
			if inspectErr != nil {
				return "", inspectErr
			}
			if !ancestorInfo.IsDir() {
				return "", fmt.Errorf("existing ancestor %q is not a directory", current)
			}
			resolved = filepath.Clean(ancestor)
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(statErr) {
			return "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func probeCandidateForwardedArgument(argument string) bool {
	return strings.HasPrefix(argument, "-Wa,") || strings.HasPrefix(argument, "-Wp,") || strings.HasPrefix(argument, "-Wl,")
}

func validateProbeCandidatePathKind(kind kconfig.ProbeCandidatePathKind, filename string, allowMissing bool) error {
	info, err := os.Stat(filename)
	if err != nil {
		if kind == kconfig.ProbeCandidatePathInclude && allowMissing && os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect candidate path %q: %w", filename, err)
	}
	switch kind {
	case kconfig.ProbeCandidatePathInclude:
		if !info.IsDir() {
			return fmt.Errorf("candidate include path %q is not a directory", filename)
		}
	case kconfig.ProbeCandidatePathForcedInclude:
		if !info.Mode().IsRegular() {
			return fmt.Errorf("candidate forced-include path %q is not a regular file", filename)
		}
	case kconfig.ProbeCandidatePathRegularFile:
		if !info.Mode().IsRegular() {
			return fmt.Errorf("candidate read-only path %q is not a regular file", filename)
		}
	default:
		return fmt.Errorf("candidate path %q has unsupported kind %q", filename, kind)
	}
	return nil
}

func probePhysicalPathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func sortedStringValues(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, values[key])
	}
	return out
}
