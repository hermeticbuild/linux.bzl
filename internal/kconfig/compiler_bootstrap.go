package kconfig

// This file describes and decodes the pre-source compiler bootstrap. The
// request runs before the Linux source tree and source architecture are
// available, but the configured driver's default target can still make its
// raw identity architecture-sensitive. The subsequent source-backed
// Make/Kconfig evaluation owns every interpretation of those values. This
// bootstrap deliberately does not classify a compiler family or synthesize
// compiler/assembler policy.

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	bootstrapCompilerMachine    = "compiler-machine"
	bootstrapCompilerVersion    = "compiler-version"
	bootstrapCompilerPredefines = "compiler-predefines"

	// proberun bounds every process stream to 64 KiB. Repeat that invariant at
	// the trust boundary so callers which construct ProbeResults directly
	// cannot smuggle a larger bootstrap fact into source evaluation.
	maxLinuxCompilerPredefinesBytes = 64 << 10
)

var (
	compilerMachinePattern = regexp.MustCompile(`^[A-Za-z0-9_.+-]+$`)
)

// LinuxCompilerBootstrapRequest returns the minimal pre-source compiler
// identity request. The selected source tree, rather than this adapter,
// interprets the driver's version text and raw default-target predefines and
// chooses all compiler policy.
func LinuxCompilerBootstrapRequest() ProbeRequest {
	exitZero := func(step string) ProbePredicate {
		return ProbePredicate{Operator: "exit-zero", Step: step}
	}
	return ProbeRequest{
		Schema: LinuxProbeRequestSchema,
		Steps: []ProbeStep{
			{Name: bootstrapCompilerMachine, Tool: "cc", Arguments: []string{"-dumpmachine"}},
			{
				Name:        bootstrapCompilerVersion,
				Tool:        "cc",
				Arguments:   []string{"--version"},
				Environment: map[string]string{"LC_ALL": "C"},
			},
			{
				Name:      bootstrapCompilerPredefines,
				Tool:      "cc",
				Arguments: []string{"-dM", "-E", "-x", "c", "/dev/null"},
			},
		},
		Outcome: ProbeOutcome{
			Kind: "boolean",
			Predicate: &ProbePredicate{Operator: "all", Operands: []ProbePredicate{
				exitZero(bootstrapCompilerMachine),
				exitZero(bootstrapCompilerVersion),
				exitZero(bootstrapCompilerPredefines),
			}},
		},
	}
}

// AddLinuxCompilerBootstrap registers the bootstrap as a root node in a
// symbolic probe DAG. Its identity depends on scope and canonical request
// content, but never on an architecture selected after this probe runs.
func AddLinuxCompilerBootstrap(discovery ProbeDiscovery, scope string) (ProbeReference, error) {
	if discovery == nil {
		return ProbeReference{}, fmt.Errorf("Linux compiler bootstrap discovery is nil")
	}
	return discovery.Request(scope, LinuxCompilerBootstrapRequest())
}

// LinuxCompilerFacts is the immutable pre-source result. Machine and version
// text identify the configured driver. Predefines are the raw output for the
// configured driver's default target and can therefore be architecture
// sensitive even though the bootstrap has no Linux source architecture yet.
// Resource paths and policy flags are not bootstrap facts; declared Linux
// source probes measure those when needed.
type LinuxCompilerFacts struct {
	scope           string
	toolsetIdentity string
	machine         string
	versionText     string
	predefines      string
}

func (f *LinuxCompilerFacts) Scope() string { return f.scope }

func (f *LinuxCompilerFacts) ToolsetIdentity() string { return f.toolsetIdentity }

func (f *LinuxCompilerFacts) Machine() string { return f.machine }

// VersionText is the first line of the selected compiler's raw --version
// output, matching the value Linux obtains with `head -n 1`. Source Makefiles
// own every interpretation of this value.
func (f *LinuxCompilerFacts) VersionText() string { return f.versionText }

// Predefines returns the exact stdout produced by
// `cc -dM -E -x c /dev/null`. It is intentionally not parsed into a macro map:
// consumers reusing it must reproduce the source-owned fixed-substring stream
// operation exactly, including matches outside macro names or values.
func (f *LinuxCompilerFacts) Predefines() string { return f.predefines }

// ParseLinuxCompilerBootstrapResult constructs immutable pre-source compiler
// facts from one canonical result. It performs no filesystem access and
// executes no subprocesses.
func ParseLinuxCompilerBootstrapResult(result ProbeResult, scope, toolsetIdentity string) (*LinuxCompilerFacts, error) {
	if err := result.Validate(); err != nil {
		return nil, fmt.Errorf("invalid Linux compiler bootstrap result: %w", err)
	}
	if scope != "target" && scope != "host" {
		return nil, fmt.Errorf("Linux compiler bootstrap scope %q is invalid", scope)
	}
	if err := validateProbeIdentity(toolsetIdentity); err != nil {
		return nil, fmt.Errorf("Linux compiler bootstrap toolset: %w", err)
	}
	request := LinuxCompilerBootstrapRequest()
	requestID, err := request.ID()
	if err != nil {
		return nil, err
	}
	expectedNode := ProbePlanNode{Scope: scope, RequestID: requestID}
	expectedNode.ID = expectedNode.ContentID()
	identityFields := []struct {
		name      string
		got, want string
	}{
		{name: "node ID", got: result.NodeID, want: expectedNode.ID},
		{name: "request ID", got: result.RequestID, want: requestID},
		{name: "scope", got: result.Scope, want: scope},
		{name: "toolset identity", got: result.ToolsetIdentity, want: toolsetIdentity},
		{name: "outcome kind", got: result.Kind, want: "boolean"},
	}
	for _, field := range identityFields {
		if field.got != field.want {
			return nil, fmt.Errorf("Linux compiler bootstrap %s = %q, want %q", field.name, field.got, field.want)
		}
	}
	if result.Boolean == nil || !*result.Boolean {
		return nil, fmt.Errorf("Linux compiler bootstrap outcome is not successful")
	}
	wantSteps := bootstrapStepNames()
	if len(result.Steps) != len(wantSteps) {
		return nil, fmt.Errorf("Linux compiler bootstrap has %d step results, want %d", len(result.Steps), len(wantSteps))
	}
	steps := make(map[string]ProbeStepResult, len(wantSteps))
	for index, want := range wantSteps {
		step := result.Steps[index]
		if step.Name != want {
			return nil, fmt.Errorf("Linux compiler bootstrap step %d = %q, want %q", index, step.Name, want)
		}
		if err := requireBootstrapStepSuccess(step); err != nil {
			return nil, err
		}
		steps[step.Name] = step
	}

	machineStep := steps[bootstrapCompilerMachine]
	if strings.TrimSpace(machineStep.Stderr) != "" {
		return nil, fmt.Errorf("Linux compiler bootstrap -dumpmachine wrote to stderr: %q", strings.TrimSpace(machineStep.Stderr))
	}
	machine := strings.TrimSpace(machineStep.Stdout)
	if !compilerMachinePattern.MatchString(machine) {
		return nil, fmt.Errorf("Linux compiler bootstrap returned invalid machine %q", machine)
	}

	versionOutput := strings.ReplaceAll(steps[bootstrapCompilerVersion].Stdout, "\r\n", "\n")
	versionText := strings.TrimSpace(strings.SplitN(versionOutput, "\n", 2)[0])
	if versionText == "" || strings.ContainsRune(versionText, 0) {
		return nil, fmt.Errorf("Linux compiler bootstrap returned invalid version text %q", versionText)
	}

	// Preserve this stream byte-for-byte. Linux source asks fixed-substring
	// questions of the complete -dM output; normalizing line endings, sorting
	// definitions, or parsing names would subtly change those semantics.
	predefines := steps[bootstrapCompilerPredefines].Stdout
	if len(predefines) > maxLinuxCompilerPredefinesBytes {
		return nil, fmt.Errorf(
			"Linux compiler bootstrap predefines are %d bytes, maximum is %d",
			len(predefines), maxLinuxCompilerPredefinesBytes,
		)
	}
	if !utf8.ValidString(predefines) || strings.ContainsRune(predefines, 0) {
		return nil, fmt.Errorf("Linux compiler bootstrap returned invalid predefines text")
	}

	return &LinuxCompilerFacts{
		scope: scope, toolsetIdentity: toolsetIdentity, machine: machine, versionText: versionText,
		predefines: predefines,
	}, nil
}

func bootstrapStepNames() []string {
	return []string{
		bootstrapCompilerMachine,
		bootstrapCompilerVersion,
		bootstrapCompilerPredefines,
	}
}

func requireBootstrapStepSuccess(step ProbeStepResult) error {
	if step.Status != "success" || step.ExitCode != 0 {
		return fmt.Errorf("Linux compiler bootstrap step %q did not succeed (status %s, exit %d)", step.Name, step.Status, step.ExitCode)
	}
	return nil
}
