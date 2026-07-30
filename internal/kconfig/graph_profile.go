package kconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/hermeticbuild/linux.bzl/internal/ccprofile"
)

const graphProfileSourceRoot = "__LINUX_BZL_SRCTREE__"

var graphProfileSourceInputPattern = regexp.MustCompile(
	regexp.QuoteMeta(graphProfileSourceRoot) + `/[A-Za-z0-9_./+@-]+`,
)

var graphProfileSiblingInputPattern = regexp.MustCompile(
	`\$\(dirname\s+(?:"\$0"|'\$0'|\$0)\)/([A-Za-z0-9_./+@-]+)`,
)

// GraphProfileShell resolves Kconfig's tool-dependent shell calls from an
// exact checked-in profile and records the subset consumed by one parse.
type GraphProfileShell struct {
	sourceRoot  string
	environment map[string]string
	resolver    *ccprofile.GraphProfileResolver
	baseProfile *ccprofile.GraphProfile

	recordIdentity ccprofile.GraphProfileIdentity
	fallback       func(context.Context, string) (string, error)
	probeFallback  GraphProfileKbuildProbeEvaluator
	extend         bool
	recordMu       sync.Mutex
	recorded       map[string]ccprofile.KconfigCommand
	recordedProbes map[string]ccprofile.KbuildGraphProbe
}

// GraphProfileKbuildProbeEvaluator evaluates one exact probe missing from a
// seeded profile. The request has already been validated and canonicalized.
type GraphProfileKbuildProbeEvaluator func(
	ccprofile.KbuildGraphProbeIdentity,
) (bool, error)

func NewGraphProfileShell(
	profile ccprofile.GraphProfile,
	sourceRoot string,
	environment map[string]string,
) (*GraphProfileShell, error) {
	resolver, err := ccprofile.NewGraphProfileResolver(profile)
	if err != nil {
		return nil, err
	}
	root, err := canonicalGraphProfileSourceRoot(sourceRoot)
	if err != nil {
		return nil, err
	}
	return &GraphProfileShell{
		sourceRoot:  root,
		environment: cloneGraphProfileStrings(environment),
		resolver:    resolver,
	}, nil
}

// NewGraphProfileExtensionShell resolves exact hits from profile and records
// only missing identities through explicit fallbacks. It never falls back for
// profile validation or semantic errors.
func NewGraphProfileExtensionShell(
	profile ccprofile.GraphProfile,
	sourceRoot string,
	environment map[string]string,
	fallback func(context.Context, string) (string, error),
	probeFallback GraphProfileKbuildProbeEvaluator,
) (*GraphProfileShell, error) {
	if fallback == nil {
		return nil, fmt.Errorf("graph profile extension shell requires a fallback")
	}
	resolver, err := ccprofile.NewGraphProfileResolver(profile)
	if err != nil {
		return nil, err
	}
	baseProfile, err := ccprofile.MergeGraphProfiles(profile)
	if err != nil {
		return nil, err
	}
	root, err := canonicalGraphProfileSourceRoot(sourceRoot)
	if err != nil {
		return nil, err
	}
	return &GraphProfileShell{
		sourceRoot:     root,
		environment:    cloneGraphProfileStrings(environment),
		resolver:       resolver,
		baseProfile:    &baseProfile,
		recordIdentity: baseProfile.Identity(),
		fallback:       fallback,
		probeFallback:  probeFallback,
		extend:         true,
		recorded:       map[string]ccprofile.KconfigCommand{},
		recordedProbes: map[string]ccprofile.KbuildGraphProbe{},
	}, nil
}

// NewGraphProfileRecordingShell captures exact command results returned by
// fallback. It is used only to maintain reviewed checked-in profile supersets.
func NewGraphProfileRecordingShell(
	identity ccprofile.GraphProfileIdentity,
	sourceRoot string,
	environment map[string]string,
	fallback func(context.Context, string) (string, error),
) (*GraphProfileShell, error) {
	if fallback == nil {
		return nil, fmt.Errorf("graph profile recording shell requires a fallback")
	}
	root, err := canonicalGraphProfileSourceRoot(sourceRoot)
	if err != nil {
		return nil, err
	}
	return &GraphProfileShell{
		sourceRoot:     root,
		environment:    cloneGraphProfileStrings(environment),
		recordIdentity: identity,
		fallback:       fallback,
		recorded:       map[string]ccprofile.KconfigCommand{},
		recordedProbes: map[string]ccprofile.KbuildGraphProbe{},
	}, nil
}

func canonicalGraphProfileSourceRoot(sourceRoot string) (string, error) {
	if strings.TrimSpace(sourceRoot) == "" {
		return "", fmt.Errorf("graph profile source root must not be empty")
	}
	root, err := filepath.Abs(sourceRoot)
	if err != nil {
		return "", fmt.Errorf("resolve graph profile source root: %w", err)
	}
	return filepath.Clean(root), nil
}

func (shell *GraphProfileShell) Run(ctx context.Context, command string) (string, error) {
	if shell == nil {
		return "", fmt.Errorf("graph profile shell is nil")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(command)
	if match := ifSuccessPattern.FindStringSubmatch(trimmed); match != nil {
		success, err := shell.resolveSuccess(ctx, strings.TrimSpace(match[1]), trimmed, match[2], match[3])
		if err != nil {
			return "", err
		}
		if success {
			return match[2], nil
		}
		return match[3], nil
	}
	return shell.resolveStdout(ctx, trimmed)
}

func (shell *GraphProfileShell) resolveStdout(
	ctx context.Context,
	command string,
) (string, error) {
	canonical, inputs, err := shell.commandIdentity(command)
	if err != nil {
		return "", err
	}
	if shell.resolver != nil {
		result, err := shell.resolver.LookupStdout(
			canonical,
			shell.environment,
			inputs,
		)
		if err == nil {
			return result, nil
		}
		if !shell.extend || !ccprofile.IsMissingGraphProfileEntry(err) {
			return "", fmt.Errorf("resolve Kconfig stdout command %q: %w", canonical, err)
		}
		if recorded, ok, lookupErr := shell.recordedCommand(
			ccprofile.KconfigCommandKindStdout,
			canonical,
			inputs,
		); lookupErr != nil {
			return "", lookupErr
		} else if ok {
			return *recorded.Stdout, nil
		}
	}
	result, err := shell.runFallback(ctx, command)
	if err != nil {
		return "", err
	}
	value := result
	if err := shell.record(ccprofile.KconfigCommand{
		Kind:        ccprofile.KconfigCommandKindStdout,
		Command:     canonical,
		Environment: cloneGraphProfileStrings(shell.environment),
		Inputs:      inputs,
		Stdout:      &value,
	}); err != nil {
		return "", err
	}
	return result, nil
}

func (shell *GraphProfileShell) resolveSuccess(
	ctx context.Context,
	command string,
	outerCommand string,
	whenTrue string,
	whenFalse string,
) (bool, error) {
	canonical, inputs, err := shell.commandIdentity(command)
	if err != nil {
		return false, err
	}
	if shell.resolver != nil {
		result, err := shell.resolver.LookupSuccess(
			canonical,
			shell.environment,
			inputs,
		)
		if err == nil {
			return result, nil
		}
		if !shell.extend || !ccprofile.IsMissingGraphProfileEntry(err) {
			return false, fmt.Errorf("resolve Kconfig success command %q: %w", canonical, err)
		}
		if recorded, ok, lookupErr := shell.recordedCommand(
			ccprofile.KconfigCommandKindSuccess,
			canonical,
			inputs,
		); lookupErr != nil {
			return false, lookupErr
		} else if ok {
			return *recorded.Success, nil
		}
	}
	output, err := shell.runFallback(ctx, outerCommand)
	if err != nil {
		return false, err
	}
	success := true
	switch {
	case whenTrue != whenFalse && output == whenTrue:
		success = true
	case whenTrue != whenFalse && output == whenFalse:
		success = false
	case whenTrue == whenFalse:
		// The result is unobservable; choose the reduced true branch.
		success = true
	default:
		return false, fmt.Errorf(
			"record Kconfig success command %q: fallback returned %q, want %q or %q",
			canonical,
			output,
			whenTrue,
			whenFalse,
		)
	}
	if err := shell.record(ccprofile.KconfigCommand{
		Kind:        ccprofile.KconfigCommandKindSuccess,
		Command:     canonical,
		Environment: cloneGraphProfileStrings(shell.environment),
		Inputs:      inputs,
		Success:     &success,
	}); err != nil {
		return false, err
	}
	return success, nil
}

func (shell *GraphProfileShell) runFallback(
	ctx context.Context,
	command string,
) (string, error) {
	if shell.fallback == nil {
		return "", fmt.Errorf("graph profile shell has no command fallback")
	}
	result, err := shell.fallback(ctx, command)
	if err != nil {
		return "", fmt.Errorf("record Kconfig command %q: %w", command, err)
	}
	return result, nil
}

func (shell *GraphProfileShell) record(command ccprofile.KconfigCommand) error {
	identity, err := ccprofile.NewKconfigCommandIdentity(
		command.Kind,
		command.Command,
		command.Environment,
		command.Inputs,
	)
	if err != nil {
		return err
	}
	command.ID = identity.ID
	shell.recordMu.Lock()
	defer shell.recordMu.Unlock()
	if existing, ok := shell.recorded[command.ID]; ok {
		if !sameGraphProfileResult(existing, command) {
			return fmt.Errorf("Kconfig command %s produced conflicting results", command.ID)
		}
		return nil
	}
	shell.recorded[command.ID] = command
	return nil
}

func (shell *GraphProfileShell) recordedCommand(
	kind string,
	command string,
	inputs map[string]string,
) (ccprofile.KconfigCommand, bool, error) {
	identity, err := ccprofile.NewKconfigCommandIdentity(
		kind,
		command,
		shell.environment,
		inputs,
	)
	if err != nil {
		return ccprofile.KconfigCommand{}, false, err
	}
	shell.recordMu.Lock()
	recorded, ok := shell.recorded[identity.ID]
	shell.recordMu.Unlock()
	return recorded, ok, nil
}

func (shell *GraphProfileShell) resolveKbuildGraphProbe(
	kind string,
	language string,
	contextArgv []string,
	candidateArgv []string,
	inputs map[string]string,
	fallbackResult bool,
) (bool, error) {
	if shell == nil || shell.resolver == nil {
		return false, fmt.Errorf("graph profile shell is not resolving a profile")
	}
	supported, err := shell.resolver.LookupKbuildGraphProbe(
		kind,
		language,
		contextArgv,
		candidateArgv,
		inputs,
	)
	if err == nil {
		return supported, nil
	}
	if !shell.extend || !ccprofile.IsMissingGraphProfileEntry(err) {
		return false, err
	}
	identity, identityErr := ccprofile.NewKbuildGraphProbeIdentity(
		kind,
		language,
		contextArgv,
		candidateArgv,
		inputs,
	)
	if identityErr != nil {
		return false, identityErr
	}
	shell.recordMu.Lock()
	recorded, ok := shell.recordedProbes[identity.ID]
	shell.recordMu.Unlock()
	if ok {
		return recorded.Supported, nil
	}
	supported = fallbackResult
	if shell.probeFallback != nil {
		supported, err = shell.probeFallback(identity)
		if err != nil {
			return false, fmt.Errorf(
				"evaluate missing Kbuild graph probe %s: %w",
				identity.ID,
				err,
			)
		}
	}
	if err := shell.recordKbuildGraphProbe(
		kind,
		language,
		contextArgv,
		candidateArgv,
		inputs,
		supported,
	); err != nil {
		return false, err
	}
	return supported, nil
}

func (shell *GraphProfileShell) recordKbuildGraphProbe(
	kind string,
	language string,
	contextArgv []string,
	candidateArgv []string,
	inputs map[string]string,
	supported bool,
) error {
	if shell == nil || shell.fallback == nil {
		return fmt.Errorf("graph profile shell is not recording a profile")
	}
	identity, err := ccprofile.NewKbuildGraphProbeIdentity(
		kind,
		language,
		contextArgv,
		candidateArgv,
		inputs,
	)
	if err != nil {
		return err
	}
	probe := ccprofile.KbuildGraphProbe{
		ID:            identity.ID,
		Kind:          identity.Kind,
		Language:      identity.Language,
		ContextArgv:   identity.ContextArgv,
		CandidateArgv: identity.CandidateArgv,
		Inputs:        identity.Inputs,
		Supported:     supported,
	}
	shell.recordMu.Lock()
	defer shell.recordMu.Unlock()
	if existing, ok := shell.recordedProbes[probe.ID]; ok {
		if existing.Supported != probe.Supported {
			return fmt.Errorf(
				"Kbuild graph probe %s produced conflicting results",
				probe.ID,
			)
		}
		return nil
	}
	shell.recordedProbes[probe.ID] = probe
	return nil
}

func sameGraphProfileResult(
	left ccprofile.KconfigCommand,
	right ccprofile.KconfigCommand,
) bool {
	switch left.Kind {
	case ccprofile.KconfigCommandKindStdout:
		return left.Stdout != nil && right.Stdout != nil && *left.Stdout == *right.Stdout
	case ccprofile.KconfigCommandKindSuccess:
		return left.Success != nil && right.Success != nil && *left.Success == *right.Success
	default:
		return false
	}
}

func (shell *GraphProfileShell) commandIdentity(
	command string,
) (string, map[string]string, error) {
	canonical := filepath.ToSlash(command)
	root := filepath.ToSlash(shell.sourceRoot)
	canonical = strings.ReplaceAll(canonical, root, graphProfileSourceRoot)
	inputs := map[string]string{}
	for _, match := range graphProfileSourceInputPattern.FindAllString(canonical, -1) {
		relative := strings.TrimPrefix(match, graphProfileSourceRoot+"/")
		relative = filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative)))
		if err := shell.addGraphProfileInputClosure(relative, inputs); err != nil {
			return "", nil, err
		}
	}
	return canonical, inputs, nil
}

func (shell *GraphProfileShell) addGraphProfileInputClosure(
	relative string,
	inputs map[string]string,
) error {
	relative = filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative)))
	if relative == "." || relative == ".." || strings.HasPrefix(relative, "../") {
		return fmt.Errorf("Kconfig command references invalid source input %q", relative)
	}
	if _, ok := inputs[relative]; ok {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(shell.sourceRoot, filepath.FromSlash(relative)))
	if err != nil {
		return fmt.Errorf("read Kconfig command input %s: %w", relative, err)
	}
	sum := sha256.Sum256(data)
	inputs[relative] = hex.EncodeToString(sum[:])

	// Linux's Kconfig probe scripts use $(dirname $0)/... for source-tree
	// helpers and fixtures. Those files are part of the exact command input,
	// even though the outer Kconfig command names only the entry script.
	directory := filepath.ToSlash(filepath.Dir(relative))
	for _, match := range graphProfileSiblingInputPattern.FindAllSubmatch(data, -1) {
		sibling := filepath.ToSlash(filepath.Join(directory, string(match[1])))
		if err := shell.addGraphProfileInputClosure(sibling, inputs); err != nil {
			return err
		}
	}
	return nil
}

func (shell *GraphProfileShell) kbuildGraphProbeInputs(
	contextArgv []string,
	candidateArgv []string,
) (map[string]string, error) {
	inputs := map[string]string{}
	for _, argv := range [][]string{contextArgv, candidateArgv} {
		references, err := kbuildGraphProbeInputReferences(argv)
		if err != nil {
			return nil, err
		}
		for _, reference := range references {
			if err := shell.addKbuildGraphProbeInput(reference, inputs); err != nil {
				return nil, err
			}
		}
	}
	return inputs, nil
}

func kbuildGraphProbeInputReferences(argv []string) ([]string, error) {
	unwrapped := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		if argv[i] != "-Xlinker" {
			unwrapped = append(unwrapped, argv[i])
			continue
		}
		if i+1 == len(argv) {
			return nil, fmt.Errorf("Kbuild graph probe has -Xlinker without an argument")
		}
		i++
		unwrapped = append(unwrapped, argv[i])
	}

	references, err := scanKbuildGraphProbeInputArguments(unwrapped)
	if err != nil {
		return nil, err
	}
	for _, arg := range unwrapped {
		var wrapped string
		switch {
		case strings.HasPrefix(arg, "-Wl,"):
			wrapped = strings.TrimPrefix(arg, "-Wl,")
		case strings.HasPrefix(arg, "-Wp,"):
			wrapped = strings.TrimPrefix(arg, "-Wp,")
		default:
			continue
		}
		nested, err := scanKbuildGraphProbeInputArguments(strings.Split(wrapped, ","))
		if err != nil {
			return nil, fmt.Errorf("scan wrapped Kbuild graph probe argument %q: %w", arg, err)
		}
		references = append(references, nested...)
	}
	return references, nil
}

func scanKbuildGraphProbeInputArguments(argv []string) ([]string, error) {
	var references []string
	separate := map[string]bool{
		"-include":              true,
		"-include-pch":          true,
		"-imacros":              true,
		"-T":                    true,
		"--script":              true,
		"--version-script":      true,
		"--dynamic-list":        true,
		"--retain-symbols-file": true,
	}
	for i := 0; i < len(argv); i++ {
		arg := strings.TrimSpace(argv[i])
		if strings.HasPrefix(arg, "@") && len(arg) > 1 {
			references = append(references, strings.TrimPrefix(arg, "@"))
			continue
		}
		if separate[arg] {
			if i+1 == len(argv) {
				return nil, fmt.Errorf("Kbuild graph probe option %q has no input path", arg)
			}
			i++
			references = append(references, argv[i])
			continue
		}
		for _, prefix := range []string{
			"--retain-symbols-file=",
			"--version-script=",
			"--dynamic-list=",
			"--script=",
			"-include=",
			"-imacros=",
		} {
			if strings.HasPrefix(arg, prefix) && len(arg) > len(prefix) {
				references = append(references, strings.TrimPrefix(arg, prefix))
				arg = ""
				break
			}
		}
		if arg == "" {
			continue
		}
		for _, prefix := range []string{"-include", "-imacros"} {
			if strings.HasPrefix(arg, prefix) && len(arg) > len(prefix) {
				value := strings.TrimPrefix(arg, prefix)
				if !strings.HasPrefix(value, "-") {
					references = append(references, value)
				}
			}
		}
		if strings.HasPrefix(arg, "-T") && len(arg) > 2 &&
			!strings.HasPrefix(arg, "-Ttext") &&
			!strings.HasPrefix(arg, "-Tdata") &&
			!strings.HasPrefix(arg, "-Tbss") {
			references = append(
				references,
				strings.TrimPrefix(strings.TrimPrefix(arg, "-T"), "="),
			)
		}
	}
	return references, nil
}

func (shell *GraphProfileShell) addKbuildGraphProbeInput(
	reference string,
	inputs map[string]string,
) error {
	reference = strings.TrimSpace(reference)
	if reference == "" || reference == "-" {
		return fmt.Errorf("Kbuild graph probe has unsupported input path %q", reference)
	}
	reference = filepath.ToSlash(reference)
	switch {
	case reference == KbuildGraphProbeObjectRoot ||
		strings.HasPrefix(reference, KbuildGraphProbeObjectRoot+"/"):
		return fmt.Errorf(
			"Kbuild graph probe input %q is generated in the object tree",
			reference,
		)
	case reference == graphProfileSourceRoot:
		return fmt.Errorf("Kbuild graph probe input names the source tree, not a file")
	case strings.HasPrefix(reference, graphProfileSourceRoot+"/"):
		return shell.addGraphProfileInputClosure(
			strings.TrimPrefix(reference, graphProfileSourceRoot+"/"),
			inputs,
		)
	}

	native := filepath.Clean(filepath.FromSlash(reference))
	if filepath.IsAbs(native) {
		relative, err := filepath.Rel(shell.sourceRoot, native)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf(
				"Kbuild graph probe input %q is outside source root %s",
				reference,
				shell.sourceRoot,
			)
		}
		return shell.addGraphProfileInputClosure(filepath.ToSlash(relative), inputs)
	}
	if len(reference) >= 3 &&
		((reference[0] >= 'A' && reference[0] <= 'Z') ||
			(reference[0] >= 'a' && reference[0] <= 'z')) &&
		reference[1] == ':' && reference[2] == '/' {
		return fmt.Errorf(
			"Kbuild graph probe input %q is outside source root %s",
			reference,
			shell.sourceRoot,
		)
	}
	return shell.addGraphProfileInputClosure(filepath.ToSlash(native), inputs)
}

func (shell *GraphProfileShell) Projection() (ccprofile.GraphProjection, error) {
	if shell == nil || shell.resolver == nil {
		return ccprofile.GraphProjection{}, fmt.Errorf("graph profile shell is not resolving a profile")
	}
	projection, err := shell.resolver.Projection()
	if err != nil {
		return ccprofile.GraphProjection{}, err
	}
	if !shell.extend {
		return projection, nil
	}
	commands, probes := shell.recordedEntries()
	projection.KconfigCommands = append(projection.KconfigCommands, commands...)
	projection.KbuildGraphProbes = append(projection.KbuildGraphProbes, probes...)
	sort.Slice(projection.KconfigCommands, func(i, j int) bool {
		return projection.KconfigCommands[i].ID < projection.KconfigCommands[j].ID
	})
	sort.Slice(projection.KbuildGraphProbes, func(i, j int) bool {
		return projection.KbuildGraphProbes[i].ID < projection.KbuildGraphProbes[j].ID
	})
	if err := ccprofile.ValidateGraphProjection(projection); err != nil {
		return ccprofile.GraphProjection{}, err
	}
	return projection, nil
}

func (shell *GraphProfileShell) IsResolving() bool {
	return shell != nil && shell.resolver != nil
}

func (shell *GraphProfileShell) IsRecording() bool {
	return shell != nil && shell.fallback != nil
}

func (shell *GraphProfileShell) ProjectionDigest() (string, error) {
	if shell == nil || shell.resolver == nil {
		return "", fmt.Errorf("graph profile shell is not resolving a profile")
	}
	projection, err := shell.Projection()
	if err != nil {
		return "", err
	}
	return ccprofile.GraphProjectionDigest(projection)
}

func (shell *GraphProfileShell) RecordedProfile() (ccprofile.GraphProfile, error) {
	if shell == nil || shell.fallback == nil {
		return ccprofile.GraphProfile{}, fmt.Errorf("graph profile shell is not recording commands")
	}
	commands, probes := shell.recordedEntries()
	profile := ccprofile.GraphProfile{
		Schema:            ccprofile.GraphProfileSchema,
		Architecture:      shell.recordIdentity.Architecture,
		DriverContract:    shell.recordIdentity.DriverContract,
		AnalysisIdentity:  shell.recordIdentity.AnalysisIdentity,
		KconfigCommands:   commands,
		KbuildGraphProbes: probes,
	}
	if err := ccprofile.ValidateGraphProfile(profile); err != nil {
		return ccprofile.GraphProfile{}, err
	}
	if shell.baseProfile != nil {
		return ccprofile.MergeGraphProfiles(*shell.baseProfile, profile)
	}
	return profile, nil
}

func (shell *GraphProfileShell) recordedEntries() (
	[]ccprofile.KconfigCommand,
	[]ccprofile.KbuildGraphProbe,
) {
	shell.recordMu.Lock()
	commands := make([]ccprofile.KconfigCommand, 0, len(shell.recorded))
	for _, command := range shell.recorded {
		commands = append(commands, command)
	}
	probes := make([]ccprofile.KbuildGraphProbe, 0, len(shell.recordedProbes))
	for _, probe := range shell.recordedProbes {
		probes = append(probes, probe)
	}
	shell.recordMu.Unlock()
	sort.Slice(commands, func(i, j int) bool {
		return commands[i].ID < commands[j].ID
	})
	sort.Slice(probes, func(i, j int) bool {
		return probes[i].ID < probes[j].ID
	})
	return commands, probes
}

func cloneGraphProfileStrings(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for name, value := range values {
		cloned[name] = value
	}
	return cloned
}
