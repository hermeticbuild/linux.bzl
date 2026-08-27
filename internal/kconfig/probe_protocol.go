package kconfig

// This file is the execution-time contract for compiler capability probes.
// The contract intentionally describes processes and reductions, not compiler
// families.  Linux's Kconfig/Kbuild evaluator owns the requests; map_directory
// only turns the path-encoded DAG into actions, and proberun executes it.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	LinuxProbePlanSchema    = "linux-probe-plan-v2"
	LinuxProbeRequestSchema = "linux-probe-request-v10"
	LinuxProbeResultSchema  = "linux-probe-result-v1"

	// Probe candidate policies select the security grammar used for arguments
	// originating in Kconfig or Kbuild. They describe the invoked interface,
	// independently of the configured tool role that implements it.
	ProbeCandidatePolicyCC     = "cc"
	ProbeCandidatePolicyLD     = "ld"
	ProbeCandidatePolicyCCLink = "cc-link"

	maxProbeSources         = 4096
	maxProbeSourceRoots     = 256
	maxProbeSourcePathBytes = 1024
	maxProbeSourceRootBytes = 128
	maxProbeSourceComponent = 254
	maxProbeValueFragments  = 4096
	// MaxProbeDynamicArgumentWords bounds the number of argv entries produced
	// by all dependency-backed argument-fragment modes at execution.
	MaxProbeDynamicArgumentWords = 65536
	// MaxProbeInterpolatedBytes bounds both the serialized fragment template
	// and the value rendered from dependency results at execution time.
	MaxProbeInterpolatedBytes = 1 << 20
)

// ProbeRequest is one content-addressed, data-driven capability query. Steps
// run in order in one private scratch directory. Arguments, environment values,
// and stdin may refer to ${scratch:NAME}, ${source:PATH},
// ${source_root:NAME}, and ${tool:ROLE}; no shell expansion or ambient tool
// lookup is performed.
type ProbeRequest struct {
	Schema      string         `json:"schema"`
	InputCount  int            `json:"input_count,omitempty"`
	Sources     []string       `json:"sources,omitempty"`
	SourceRoots []string       `json:"source_roots,omitempty"`
	Scratch     []ProbeScratch `json:"scratch,omitempty"`
	Steps       []ProbeStep    `json:"steps"`
	Outcome     ProbeOutcome   `json:"outcome"`
}

type ProbeScratch struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"` // file or directory
	Content string `json:"content,omitempty"`
}

// ProbeCandidateArguments identifies the argument structure supplied by
// Kconfig or Kbuild and therefore requiring execution-time validation. Base
// contains indexes into ProbeStep.Arguments. Owning a base slot also owns all
// argv words rendered by a ProbeArgumentFragments group at that slot.
// Conditional contains indexes into ProbeStep.ConditionalArguments and owns
// every argument rendered by each selected group. Both lists are strictly
// sorted so semantically identical ownership has one canonical encoding.
type ProbeCandidateArguments struct {
	Policy      string `json:"policy"`
	Base        []int  `json:"base,omitempty"`
	Conditional []int  `json:"conditional,omitempty"`
}

type ProbeStep struct {
	Name string `json:"name"`
	Tool string `json:"tool"`
	// AuxiliaryTools names configured tool action contracts forwarded to the
	// primary tool through toolaction.EnvironmentName. Each role remains an
	// explicit ${tool:ROLE} path argument/environment reference as well.
	AuxiliaryTools []string `json:"auxiliary_tools,omitempty"`
	// WorkingDirectory is either empty (the private scratch directory) or
	// exactly one declared ${source_root:NAME} placeholder.
	WorkingDirectory     string                      `json:"working_directory,omitempty"`
	Arguments            []string                    `json:"arguments,omitempty"`
	ConditionalArguments []ProbeConditionalArguments `json:"conditional_arguments,omitempty"`
	ArgumentFragments    []ProbeArgumentFragments    `json:"argument_fragments,omitempty"`
	Candidate            *ProbeCandidateArguments    `json:"candidate,omitempty"`
	Environment          map[string]string           `json:"environment,omitempty"`
	EnvironmentFragments []ProbeEnvironmentFragments `json:"environment_fragments,omitempty"`
	Stdin                string                      `json:"stdin,omitempty"`
	StdinFragments       []ProbeValueFragment        `json:"stdin_fragments,omitempty"`
	When                 *ProbePredicate             `json:"when,omitempty"`
	// DiscardStdout and DiscardStderr preserve source shell redirections for
	// status-only probes. The process still runs normally, but discarded bytes
	// cannot enter the content-addressed ProbeResult or its output limits.
	DiscardStdout bool `json:"discard_stdout,omitempty"`
	DiscardStderr bool `json:"discard_stderr,omitempty"`
	// StdoutExecrootRelative declares that a successful step's stdout is one
	// path. proberun strips surrounding whitespace and serializes the path
	// relative to its execroot, rejecting paths outside that root. This keeps
	// remote-worker absolute paths out of durable ProbeResults.
	StdoutExecrootRelative bool `json:"stdout_execroot_relative,omitempty"`
	// StdoutFallbackPath names the safe path-component sentinel for a
	// -print-file-name style lookup which did not produce an identity-bound
	// artifact. The runner preserves an exact sentinel or substitutes it for a
	// valid normalized path outside the declared toolset closure. It never
	// grants filesystem authority: consumers must short-circuit before trying
	// to resolve that spelling.
	StdoutFallbackPath string `json:"stdout_fallback_path,omitempty"`
}

// ProbeValueFragment is one ordered piece of a process string. When is nil
// for an unconditional literal; otherwise the expanded Value is appended only
// when the predicate holds. This preserves source-selected text inside stdin
// and environment values without exposing planner-only symbolic atoms to the
// executed process.
type ProbeValueFragment struct {
	Value      string                `json:"value"`
	When       *ProbePredicate       `json:"when,omitempty"`
	Transforms []ProbeValueTransform `json:"transforms,omitempty"`
	// Fragments is an aggregate value: its children are concatenated first,
	// then Transforms are applied to the complete result. Value and Fragments
	// are mutually exclusive. Aggregate fragments preserve exact whole-Make-
	// text operations without applying them independently to each conditional
	// child or to adjacent literal text owned by the containing value.
	Fragments []ProbeValueFragment `json:"fragments,omitempty"`
}

// ProbeValueTransformArgumentFragments supplies one dynamic, unsplit Make
// function argument. The backing Arguments slot is empty in the serialized
// transform; proberun concatenates Fragments into that slot before applying
// the transform. This is deliberately distinct from ProbeArgumentFragments:
// Make function arguments are text values and are never shell-word split.
type ProbeValueTransformArgumentFragments struct {
	Index     int                  `json:"index"`
	Fragments []ProbeValueFragment `json:"fragments"`
}

const MaxProbeValueFragmentDepth = 32

// ProbeEnvironmentFragments builds one environment value by concatenating
// its fragments in order. Entries are strictly sorted by Name and cannot also
// appear in ProbeStep.Environment.
type ProbeEnvironmentFragments struct {
	Name      string               `json:"name"`
	Fragments []ProbeValueFragment `json:"fragments"`
}

const (
	// ProbeArgumentFragmentsModeSignedDecimal requires the rendered fragment
	// value to be exactly one ASCII decimal argv word: an optional leading '-'
	// followed by one to nineteen digits. The runner validates the complete
	// value and appends it without field splitting.
	ProbeArgumentFragmentsModeSignedDecimal = "signed-decimal"
)

// ValidateProbeSignedDecimalArgument validates the complete runtime value for
// ProbeArgumentFragmentsModeSignedDecimal. It is intentionally lexical rather
// than machine-integer-specific: expr remains responsible for evaluating the
// bounded decimal, while the protocol proves it cannot become shell syntax or
// another argv element.
func ValidateProbeSignedDecimalArgument(value string) error {
	digits := value
	if strings.HasPrefix(digits, "-") {
		digits = digits[1:]
	}
	if len(digits) == 0 || len(digits) > 19 {
		return fmt.Errorf("signed-decimal argument must contain one to nineteen digits after an optional '-'")
	}
	for _, character := range []byte(digits) {
		if character < '0' || character > '9' {
			return fmt.Errorf("signed-decimal argument must contain only ASCII digits after an optional '-'")
		}
	}
	return nil
}

// ProbeArgumentFragments replaces one empty base argument with argv derived
// from concatenating Fragments. The default mode produces whitespace-delimited
// words using strings.Fields-style splitting. Signed-decimal mode validates and
// appends exactly one bounded scalar word. Keeping an explicit base slot makes
// its order unambiguous relative to both literal and conditional arguments;
// neither mode invokes a shell.
type ProbeArgumentFragments struct {
	Index     int                  `json:"index"`
	Mode      string               `json:"mode,omitempty"`
	Fragments []ProbeValueFragment `json:"fragments"`
}

// ProbeConditionalArguments inserts Arguments immediately before the base
// argument at Before (or at the end when Before == len(Arguments)).
type ProbeConditionalArguments struct {
	Before    int            `json:"before"`
	When      ProbePredicate `json:"when"`
	Arguments []string       `json:"arguments"`
}

// ProbePredicate is a small generic boolean algebra over completed process
// results and scratch artifacts. It is sufficient to express Linux's compound
// probes (including linker fallbacks) without teaching the runner about Linux,
// GCC, Clang, or any configuration symbol.
type ProbePredicate struct {
	Operator string           `json:"operator"`
	Step     string           `json:"step,omitempty"`
	Stream   string           `json:"stream,omitempty"`
	Value    string           `json:"value,omitempty"`
	Scratch  string           `json:"scratch,omitempty"`
	Result   string           `json:"result,omitempty"`
	Operands []ProbePredicate `json:"operands,omitempty"`
}

type ProbeOutcome struct {
	Kind      string          `json:"kind"` // boolean or text
	Predicate *ProbePredicate `json:"predicate,omitempty"`
	Step      string          `json:"step,omitempty"`
	Stream    string          `json:"stream,omitempty"`
	// Fragments materializes one derived text result directly from declared
	// dependency results and pure Make transforms. This keeps map_directory
	// topology decisions data-driven without starting a shell merely to copy a
	// protocol-lowered value into another content-addressed node.
	Fragments []ProbeValueFragment `json:"fragments,omitempty"`
	// Result and Word select one shell-style whitespace-delimited word from a
	// declared text dependency. This models Kconfig's `set -- VALUE && echo
	// $N` without executing a shell in the planner or interpreting the
	// producer script.
	Result    string `json:"result,omitempty"`
	Word      int    `json:"word,omitempty"`
	TrimSpace bool   `json:"trim_space,omitempty"`
	FirstLine bool   `json:"first_line,omitempty"`
	LastLine  bool   `json:"last_line,omitempty"`
	// GNUMakeShell applies the exact newline reduction used by $(shell ...) to
	// the selected stream before it becomes symbolic Make text.
	GNUMakeShell bool `json:"gnu_make_shell,omitempty"`
	// RequireSuccess makes a text result unavailable when its selected process
	// exits nonzero. Ordinary compiler diagnostic queries may intentionally
	// consume failed stdout; topology-selecting source queries may not silently
	// turn a missing runtime command into empty Make text.
	RequireSuccess bool `json:"require_success,omitempty"`
	// SingleMakeWord requires the reduced text to be nonempty and contain no
	// whitespace. This lets discovery preserve exact Make word boundaries when
	// a source-derived value participates in target or prerequisite topology.
	SingleMakeWord bool `json:"single_make_word,omitempty"`
	// PathComponent requires the reduced text to be one safe, non-special
	// filename component. It is used for source queries such as rustc
	// --print file-names before the value may participate in Make topology.
	PathComponent bool `json:"path_component,omitempty"`
}

type ProbeStepResult struct {
	Name     string `json:"name"`
	Status   string `json:"status"` // success, failure, or skipped
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

type ProbeResult struct {
	Schema          string            `json:"schema"`
	NodeID          string            `json:"node_id"`
	RequestID       string            `json:"request_id"`
	Scope           string            `json:"scope"`
	ToolsetIdentity string            `json:"toolset_identity"`
	Kind            string            `json:"kind"`
	Boolean         *bool             `json:"boolean,omitempty"`
	Text            string            `json:"text,omitempty"`
	Steps           []ProbeStepResult `json:"steps"`
}

var probePlaceholder = regexp.MustCompile(`\$\{(scratch|source|source_root|tool|result):([^}]+)\}`)
var probeResultReference = regexp.MustCompile(`^([0-9]{8})\.(text|boolean)$`)

func (r ProbeRequest) CanonicalJSON() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func (r ProbeRequest) ID() (string, error) {
	data, err := r.CanonicalJSON()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (r ProbeRequest) ToolRoles() []string {
	roles := map[string]bool{}
	for _, step := range r.Steps {
		roles[step.Tool] = true
		values := append(append([]string(nil), step.Arguments...), step.Stdin)
		for _, fragment := range step.StdinFragments {
			values = append(values, probeValueFragmentStrings(fragment)...)
		}
		for _, group := range step.ConditionalArguments {
			values = append(values, group.Arguments...)
		}
		for _, group := range step.ArgumentFragments {
			for _, fragment := range group.Fragments {
				values = append(values, probeValueFragmentStrings(fragment)...)
			}
		}
		for _, value := range values {
			for _, match := range probePlaceholder.FindAllStringSubmatch(value, -1) {
				if match[1] == "tool" {
					roles[match[2]] = true
				}
			}
		}
		for _, value := range step.Environment {
			for _, match := range probePlaceholder.FindAllStringSubmatch(value, -1) {
				if match[1] == "tool" {
					roles[match[2]] = true
				}
			}
		}
		for _, entry := range step.EnvironmentFragments {
			for _, fragment := range entry.Fragments {
				values := probeValueFragmentStrings(fragment)
				for _, value := range values {
					for _, match := range probePlaceholder.FindAllStringSubmatch(value, -1) {
						if match[1] == "tool" {
							roles[match[2]] = true
						}
					}
				}
			}
		}
	}
	for _, fragment := range r.Outcome.Fragments {
		for _, value := range probeValueFragmentStrings(fragment) {
			for _, match := range probePlaceholder.FindAllStringSubmatch(value, -1) {
				if match[1] == "tool" {
					roles[match[2]] = true
				}
			}
		}
	}
	out := make([]string, 0, len(roles))
	for role := range roles {
		out = append(out, role)
	}
	sort.Strings(out)
	return out
}

func (r ProbeRequest) Validate() error {
	if r.Schema != LinuxProbeRequestSchema {
		return fmt.Errorf("probe request schema %q, want %q", r.Schema, LinuxProbeRequestSchema)
	}
	if len(r.Steps) == 0 && r.InputCount == 0 {
		return errors.New("probe request has neither steps nor inputs")
	}
	if r.InputCount < 0 || r.InputCount > 99999999 {
		return fmt.Errorf("probe request input count %d is out of range", r.InputCount)
	}
	if len(r.Sources) > maxProbeSources {
		return fmt.Errorf("probe request declares %d sources, maximum is %d", len(r.Sources), maxProbeSources)
	}
	sources := make(map[string]bool, len(r.Sources))
	previous := ""
	for _, source := range r.Sources {
		if err := validateProbeSourcePath(source); err != nil {
			return err
		}
		if source <= previous {
			return errors.New("probe sources must be strictly sorted")
		}
		sources[source], previous = true, source
	}
	if len(r.SourceRoots) > maxProbeSourceRoots {
		return fmt.Errorf("probe request declares %d source roots, maximum is %d", len(r.SourceRoots), maxProbeSourceRoots)
	}
	sourceRoots := make(map[string]bool, len(r.SourceRoots))
	previous = ""
	for _, root := range r.SourceRoots {
		if len(root) > maxProbeSourceRootBytes {
			return fmt.Errorf("probe source root %q exceeds %d bytes", root, maxProbeSourceRootBytes)
		}
		if err := validatePlanName("probe source root", root); err != nil {
			return err
		}
		if root <= previous {
			return errors.New("probe source roots must be strictly sorted")
		}
		sourceRoots[root], previous = true, root
	}
	scratch := map[string]bool{}
	previous = ""
	for _, item := range r.Scratch {
		if err := validatePlanName("probe scratch", item.Name); err != nil {
			return err
		}
		if item.Name <= previous {
			return errors.New("probe scratch entries must be strictly sorted by name")
		}
		if item.Kind != "file" && item.Kind != "directory" {
			return fmt.Errorf("probe scratch %q has unsupported kind %q", item.Name, item.Kind)
		}
		if len(item.Content) > 1<<20 || strings.ContainsRune(item.Content, 0) {
			return fmt.Errorf("probe scratch %q content is invalid or exceeds 1 MiB", item.Name)
		}
		if item.Kind != "file" && item.Content != "" {
			return fmt.Errorf("probe scratch directory %q has content", item.Name)
		}
		scratch[item.Name], previous = true, item.Name
	}
	steps := map[string]int{}
	for index, step := range r.Steps {
		if err := validatePlanName("probe step", step.Name); err != nil {
			return err
		}
		if _, exists := steps[step.Name]; exists {
			return fmt.Errorf("probe request repeats step %q", step.Name)
		}
		if err := validatePlanName("probe tool role", step.Tool); err != nil {
			return err
		}
		if step.WorkingDirectory != "" {
			if err := validateProbeWorkingDirectory(step.WorkingDirectory, sourceRoots); err != nil {
				return fmt.Errorf("probe step %q working directory: %w", step.Name, err)
			}
		}
		if step.DiscardStdout && step.StdoutExecrootRelative {
			return fmt.Errorf("probe step %q cannot discard and normalize stdout", step.Name)
		}
		if step.StdoutFallbackPath != "" {
			if !step.StdoutExecrootRelative {
				return fmt.Errorf("probe step %q stdout fallback requires execroot-relative stdout", step.Name)
			}
			if err := ValidateProbePathComponent(step.StdoutFallbackPath); err != nil {
				return fmt.Errorf("probe step %q stdout fallback: %w", step.Name, err)
			}
		}
		previousAuxiliary := ""
		for _, role := range step.AuxiliaryTools {
			if err := validatePlanName("probe auxiliary tool role", role); err != nil {
				return err
			}
			if role <= previousAuxiliary {
				return fmt.Errorf("probe step %q auxiliary tools must be strictly sorted", step.Name)
			}
			if role == step.Tool {
				return fmt.Errorf("probe step %q auxiliary tool %q is also the primary tool", step.Name, role)
			}
			previousAuxiliary = role
		}
		valueTransformCount := 0
		for key, value := range step.Environment {
			if key == "" || strings.ContainsAny(key, "=\x00") {
				return fmt.Errorf("probe step %q has invalid environment name %q", step.Name, key)
			}
			if err := validateProbeTemplate(value, scratch, sources, sourceRoots, r.InputCount); err != nil {
				return fmt.Errorf("probe step %q environment %s: %w", step.Name, key, err)
			}
		}
		previousEnvironmentName := ""
		environmentFragmentCount := 0
		environmentFragmentBytes := 0
		for entryIndex, entry := range step.EnvironmentFragments {
			if entry.Name == "" || strings.ContainsAny(entry.Name, "=\x00") {
				return fmt.Errorf("probe step %q environment fragments %d have invalid name %q", step.Name, entryIndex, entry.Name)
			}
			if entry.Name <= previousEnvironmentName {
				return fmt.Errorf("probe step %q environment fragments must be strictly sorted by name", step.Name)
			}
			if _, exists := step.Environment[entry.Name]; exists {
				return fmt.Errorf("probe step %q environment %s has both a literal and fragmented value", step.Name, entry.Name)
			}
			if err := validateProbeValueFragments(
				fmt.Sprintf("probe step %q environment %s", step.Name, entry.Name),
				entry.Fragments, 0, steps, scratch, sources, sourceRoots, r.InputCount,
				&environmentFragmentCount, &environmentFragmentBytes, &valueTransformCount,
			); err != nil {
				return err
			}
			previousEnvironmentName = entry.Name
		}
		if environmentFragmentCount > maxProbeValueFragments || environmentFragmentBytes > MaxProbeInterpolatedBytes {
			return fmt.Errorf("probe step %q environment fragments exceed protocol bounds", step.Name)
		}
		for argumentIndex, value := range step.Arguments {
			if strings.ContainsRune(value, 0) {
				return fmt.Errorf("probe step %q argument %d contains NUL", step.Name, argumentIndex)
			}
			if err := validateProbeTemplate(value, scratch, sources, sourceRoots, r.InputCount); err != nil {
				return fmt.Errorf("probe step %q argument %d: %w", step.Name, argumentIndex, err)
			}
		}
		previousArgumentFragmentIndex := -1
		argumentFragmentCount := 0
		argumentFragmentBytes := 0
		for groupIndex, group := range step.ArgumentFragments {
			if group.Index < 0 || group.Index >= len(step.Arguments) || group.Index <= previousArgumentFragmentIndex {
				return fmt.Errorf("probe step %q argument fragment group %d has an invalid or unsorted index", step.Name, groupIndex)
			}
			if group.Mode != "" && group.Mode != ProbeArgumentFragmentsModeSignedDecimal {
				return fmt.Errorf("probe step %q argument fragment group %d has unsupported mode %q", step.Name, groupIndex, group.Mode)
			}
			if step.Arguments[group.Index] != "" || len(group.Fragments) == 0 {
				return fmt.Errorf("probe step %q argument fragment group %d must replace one empty base argument", step.Name, groupIndex)
			}
			if err := validateProbeValueFragments(
				fmt.Sprintf("probe step %q argument group %d", step.Name, groupIndex),
				group.Fragments, 0, steps, scratch, sources, sourceRoots, r.InputCount,
				&argumentFragmentCount, &argumentFragmentBytes, &valueTransformCount,
			); err != nil {
				return err
			}
			previousArgumentFragmentIndex = group.Index
		}
		if argumentFragmentCount > maxProbeValueFragments || argumentFragmentBytes > MaxProbeInterpolatedBytes {
			return fmt.Errorf("probe step %q argument fragments exceed protocol bounds", step.Name)
		}
		if len(step.Stdin) > 1<<20 || strings.ContainsRune(step.Stdin, 0) {
			return fmt.Errorf("probe step %q stdin is invalid or exceeds 1 MiB", step.Name)
		}
		if step.Stdin != "" && len(step.StdinFragments) != 0 {
			return fmt.Errorf("probe step %q has both literal and fragmented stdin", step.Name)
		}
		stdinFragmentBytes := 0
		if len(step.StdinFragments) > maxProbeValueFragments {
			return fmt.Errorf("probe step %q stdin has too many fragments", step.Name)
		}
		stdinFragmentCount := 0
		if err := validateProbeValueFragments(
			fmt.Sprintf("probe step %q stdin", step.Name),
			step.StdinFragments, 0, steps, scratch, sources, sourceRoots, r.InputCount,
			&stdinFragmentCount, &stdinFragmentBytes, &valueTransformCount,
		); err != nil {
			return err
		}
		if stdinFragmentBytes > MaxProbeInterpolatedBytes {
			return fmt.Errorf("probe step %q fragmented stdin exceeds 1 MiB", step.Name)
		}
		if valueTransformCount > maxProbeValueTransforms {
			return fmt.Errorf("probe step %q has %d value transforms, maximum is %d", step.Name, valueTransformCount, maxProbeValueTransforms)
		}
		previousBefore := -1
		for groupIndex, group := range step.ConditionalArguments {
			if len(group.Arguments) == 0 || group.Before < 0 || group.Before > len(step.Arguments) || group.Before < previousBefore {
				return fmt.Errorf("probe step %q conditional argument group %d has invalid insertion point or no arguments", step.Name, groupIndex)
			}
			if err := group.When.validate(steps, scratch, sources, sourceRoots, r.InputCount); err != nil {
				return fmt.Errorf("probe step %q conditional argument group %d: %w", step.Name, groupIndex, err)
			}
			for argumentIndex, value := range group.Arguments {
				if strings.ContainsRune(value, 0) {
					return fmt.Errorf("probe step %q conditional argument %d:%d contains NUL", step.Name, groupIndex, argumentIndex)
				}
				if err := validateProbeTemplate(value, scratch, sources, sourceRoots, r.InputCount); err != nil {
					return fmt.Errorf("probe step %q conditional argument %d:%d: %w", step.Name, groupIndex, argumentIndex, err)
				}
			}
			previousBefore = group.Before
		}
		if err := validateProbeCandidateArguments(step); err != nil {
			return fmt.Errorf("probe step %q candidate arguments: %w", step.Name, err)
		}
		if err := validateProbeTemplate(step.Stdin, scratch, sources, sourceRoots, r.InputCount); err != nil {
			return fmt.Errorf("probe step %q stdin: %w", step.Name, err)
		}
		if step.When != nil {
			if err := step.When.validate(steps, scratch, sources, sourceRoots, r.InputCount); err != nil {
				return fmt.Errorf("probe step %q condition: %w", step.Name, err)
			}
		}
		referencedTools := probeStepTemplateToolRoles(step)
		for _, role := range step.AuxiliaryTools {
			if !referencedTools[role] {
				return fmt.Errorf("probe step %q auxiliary tool %q is not referenced via ${tool:%s}", step.Name, role, role)
			}
		}
		steps[step.Name] = index
	}
	if len(r.Outcome.Fragments) != 0 {
		if r.InputCount == 0 {
			return errors.New("derived text probe outcome requires at least one input")
		}
		if len(r.Steps) != 0 || len(r.Scratch) != 0 || len(r.Sources) != 0 || len(r.SourceRoots) != 0 {
			return errors.New("derived text probe outcome must be a dependency-only, zero-step request")
		}
	}
	if err := r.Outcome.validate(steps, scratch, sources, sourceRoots, r.InputCount); err != nil {
		return err
	}
	return nil
}

func validateProbeCandidateArguments(step ProbeStep) error {
	candidate := step.Candidate
	if candidate == nil {
		return nil
	}
	switch candidate.Policy {
	case ProbeCandidatePolicyCC, ProbeCandidatePolicyLD, ProbeCandidatePolicyCCLink:
	default:
		return fmt.Errorf("unsupported policy %q", candidate.Policy)
	}
	if len(candidate.Base) == 0 && len(candidate.Conditional) == 0 {
		return errors.New("must own at least one base or conditional argument")
	}
	previous := -1
	for ownershipIndex, argumentIndex := range candidate.Base {
		if argumentIndex < 0 || argumentIndex >= len(step.Arguments) || argumentIndex <= previous {
			return fmt.Errorf("base ownership %d has invalid or unsorted argument index %d", ownershipIndex, argumentIndex)
		}
		previous = argumentIndex
	}
	previous = -1
	for ownershipIndex, groupIndex := range candidate.Conditional {
		if groupIndex < 0 || groupIndex >= len(step.ConditionalArguments) || groupIndex <= previous {
			return fmt.Errorf("conditional ownership %d has invalid or unsorted group index %d", ownershipIndex, groupIndex)
		}
		previous = groupIndex
	}
	return nil
}

// validateProbeWorkingDirectory admits one declared source root plus an
// optional canonical subdirectory. Source-only Kbuild shell queries run from
// the exact recursive-Make cwd, while the root binding and bounded suffix keep
// the process inside an input tree. No scratch, tool, result, or second source
// placeholder can influence cwd selection.
func validateProbeWorkingDirectory(value string, sourceRoots map[string]bool) error {
	const prefix = "${source_root:"
	if !strings.HasPrefix(value, prefix) {
		return fmt.Errorf("must start with one declared ${source_root:NAME}")
	}
	end := strings.IndexByte(value[len(prefix):], '}')
	if end < 0 {
		return fmt.Errorf("has an unterminated source-root placeholder")
	}
	end += len(prefix)
	name := value[len(prefix):end]
	if !sourceRoots[name] {
		return fmt.Errorf("references undeclared source root %q", name)
	}
	suffix := value[end+1:]
	if suffix == "" {
		return nil
	}
	if !strings.HasPrefix(suffix, "/") {
		return fmt.Errorf("source-root subdirectory must begin with /")
	}
	relative := strings.TrimPrefix(suffix, "/")
	if strings.Contains(relative, "${") {
		return fmt.Errorf("source-root subdirectory cannot contain another placeholder")
	}
	if err := validateProbeSourcePath(relative); err != nil {
		return fmt.Errorf("source-root subdirectory: %w", err)
	}
	return nil
}

func validateProbeTemplate(value string, scratch, sources, sourceRoots map[string]bool, inputCount int) error {
	for _, match := range probePlaceholder.FindAllStringSubmatch(value, -1) {
		switch match[1] {
		case "scratch":
			if !scratch[match[2]] {
				return fmt.Errorf("references undeclared scratch %q", match[2])
			}
		case "source":
			if !sources[match[2]] {
				return fmt.Errorf("references undeclared source %q", match[2])
			}
		case "source_root":
			if !sourceRoots[match[2]] {
				return fmt.Errorf("references undeclared source root %q", match[2])
			}
		case "tool":
			if err := validatePlanName("probe tool role", match[2]); err != nil {
				return err
			}
		case "result":
			parts := probeResultReference.FindStringSubmatch(match[2])
			if parts == nil || !validProbeResultOrdinal(parts[1], inputCount) {
				return fmt.Errorf("references unavailable result %q", match[2])
			}
		default:
			return fmt.Errorf("uses unsupported placeholder %q", match[0])
		}
	}
	if strings.Contains(probePlaceholder.ReplaceAllString(value, ""), "${") {
		return fmt.Errorf("contains malformed or unsupported placeholder")
	}
	return nil
}

func validateProbeSourcePath(value string) error {
	if value == "" || len(value) > maxProbeSourcePathBytes || strings.ContainsAny(value, "\\\x00") || path.IsAbs(value) || path.Clean(value) != value || value == "." || value == ".." || strings.HasPrefix(value, "../") {
		return fmt.Errorf("probe source path %q is not a canonical source-relative path of at most %d bytes", value, maxProbeSourcePathBytes)
	}
	for _, component := range strings.Split(value, "/") {
		if len(component) > maxProbeSourceComponent {
			return fmt.Errorf("probe source path %q has a component exceeding %d bytes", value, maxProbeSourceComponent)
		}
	}
	return nil
}

func probeStepTemplateToolRoles(step ProbeStep) map[string]bool {
	roles := map[string]bool{}
	values := append(append([]string(nil), step.Arguments...), step.Stdin)
	for _, fragment := range step.StdinFragments {
		values = append(values, probeValueFragmentStrings(fragment)...)
	}
	for _, group := range step.ConditionalArguments {
		values = append(values, group.Arguments...)
	}
	for _, group := range step.ArgumentFragments {
		for _, fragment := range group.Fragments {
			values = append(values, probeValueFragmentStrings(fragment)...)
		}
	}
	for _, value := range step.Environment {
		values = append(values, value)
	}
	for _, entry := range step.EnvironmentFragments {
		for _, fragment := range entry.Fragments {
			values = append(values, probeValueFragmentStrings(fragment)...)
		}
	}
	for _, value := range values {
		for _, match := range probePlaceholder.FindAllStringSubmatch(value, -1) {
			if match[1] == "tool" {
				roles[match[2]] = true
			}
		}
	}
	return roles
}

func probeValueFragmentStrings(fragment ProbeValueFragment) []string {
	values := []string{fragment.Value}
	for _, transform := range fragment.Transforms {
		values = append(values, transform.Arguments...)
		for _, group := range transform.ArgumentFragments {
			for _, nested := range group.Fragments {
				values = append(values, probeValueFragmentStrings(nested)...)
			}
		}
	}
	for _, nested := range fragment.Fragments {
		values = append(values, probeValueFragmentStrings(nested)...)
	}
	return values
}

func validateProbeValueFragments(
	owner string,
	fragments []ProbeValueFragment,
	depth int,
	steps map[string]int,
	scratch, sources, sourceRoots map[string]bool,
	inputCount int,
	fragmentCount, fragmentBytes, transformCount *int,
) error {
	if depth > MaxProbeValueFragmentDepth {
		return fmt.Errorf("%s exceeds aggregate depth %d", owner, MaxProbeValueFragmentDepth)
	}
	for fragmentIndex, fragment := range fragments {
		location := fmt.Sprintf("%s fragment %d", owner, fragmentIndex)
		if *fragmentCount >= maxProbeValueFragments {
			return fmt.Errorf("%s has more than %d fragments", owner, maxProbeValueFragments)
		}
		*fragmentCount++
		hasValue := fragment.Value != ""
		hasChildren := len(fragment.Fragments) != 0
		if hasValue == hasChildren || strings.ContainsRune(fragment.Value, 0) {
			return fmt.Errorf("%s must contain exactly one nonempty value or aggregate", location)
		}
		if hasValue {
			if err := validateProbeTemplate(fragment.Value, scratch, sources, sourceRoots, inputCount); err != nil {
				return fmt.Errorf("%s: %w", location, err)
			}
			if len(fragment.Value) > MaxProbeInterpolatedBytes-*fragmentBytes {
				return fmt.Errorf("%s exceeds %d literal bytes", owner, MaxProbeInterpolatedBytes)
			}
			*fragmentBytes += len(fragment.Value)
		} else if err := validateProbeValueFragments(
			location+" aggregate", fragment.Fragments, depth+1,
			steps, scratch, sources, sourceRoots, inputCount,
			fragmentCount, fragmentBytes, transformCount,
		); err != nil {
			return err
		}
		if fragment.When != nil {
			if err := fragment.When.validate(steps, scratch, sources, sourceRoots, inputCount); err != nil {
				return fmt.Errorf("%s condition: %w", location, err)
			}
		}
		if len(fragment.Transforms) > maxProbeValueTransforms {
			return fmt.Errorf("%s has %d value transforms, maximum is %d", location, len(fragment.Transforms), maxProbeValueTransforms)
		}
		for transformIndex, transform := range fragment.Transforms {
			if *transformCount >= maxProbeValueTransforms {
				return fmt.Errorf("probe step has more than %d value transforms", maxProbeValueTransforms)
			}
			if err := validateProbeValueTransformShape(transform); err != nil {
				return fmt.Errorf("%s transform %d: %w", location, transformIndex, err)
			}
			transformBytes := len(transform.Function)
			for argumentIndex, argument := range transform.Arguments {
				if err := validateProbeTemplate(argument, scratch, sources, sourceRoots, inputCount); err != nil {
					return fmt.Errorf("%s transform %d argument %d: %w", location, transformIndex, argumentIndex, err)
				}
				if len(argument) > MaxProbeInterpolatedBytes-transformBytes {
					return fmt.Errorf("%s transform %d exceeds %d literal bytes", location, transformIndex, MaxProbeInterpolatedBytes)
				}
				transformBytes += len(argument)
			}
			if transformBytes > MaxProbeInterpolatedBytes-*fragmentBytes {
				return fmt.Errorf("%s exceeds %d literal bytes", owner, MaxProbeInterpolatedBytes)
			}
			*fragmentBytes += transformBytes
			*transformCount++
			for groupIndex, group := range transform.ArgumentFragments {
				if err := validateProbeValueFragments(
					fmt.Sprintf("%s transform %d argument group %d", location, transformIndex, groupIndex),
					group.Fragments, depth+1,
					steps, scratch, sources, sourceRoots, inputCount,
					fragmentCount, fragmentBytes, transformCount,
				); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (p ProbePredicate) validate(steps map[string]int, scratch, sources, sourceRoots map[string]bool, inputCount int) error {
	leaf := p.Operator == "exit-zero" || p.Operator == "stream-contains" || p.Operator == "stream-matches" || p.Operator == "stream-empty" || p.Operator == "stream-trimmed-empty" || p.Operator == "regular-file" || p.Operator == "execroot-exists" || p.Operator == "execroot-regular-file"
	resultLeaf := p.Operator == "result-true" || p.Operator == "result-false" || p.Operator == "result-text-empty" || p.Operator == "result-text-equals" || p.Operator == "result-text-contains"
	if !leaf && !resultLeaf && p.Operator != "all" && p.Operator != "any" && p.Operator != "not" {
		return fmt.Errorf("unsupported predicate operator %q", p.Operator)
	}
	if p.Operator == "all" || p.Operator == "any" || p.Operator == "not" {
		if p.Step != "" || p.Stream != "" || p.Value != "" || p.Scratch != "" || p.Result != "" {
			return fmt.Errorf("composite predicate %q has leaf fields", p.Operator)
		}
		if len(p.Operands) == 0 || p.Operator == "not" && len(p.Operands) != 1 {
			return fmt.Errorf("predicate %q has invalid operand count %d", p.Operator, len(p.Operands))
		}
		for _, operand := range p.Operands {
			if err := operand.validate(steps, scratch, sources, sourceRoots, inputCount); err != nil {
				return err
			}
		}
		return nil
	}
	if len(p.Operands) != 0 {
		return fmt.Errorf("leaf predicate %q has operands", p.Operator)
	}
	if resultLeaf {
		if !validProbeResultOrdinal(p.Result, inputCount) || p.Step != "" || p.Stream != "" || p.Scratch != "" {
			return fmt.Errorf("result predicate %q has invalid result/step fields", p.Operator)
		}
		if (p.Operator == "result-true" || p.Operator == "result-false") && p.Value != "" {
			return fmt.Errorf("boolean result predicate has text value")
		}
		if p.Operator == "result-text-empty" && p.Value != "" {
			return fmt.Errorf("empty-text result predicate has a value")
		}
		if (p.Operator == "result-text-equals" || p.Operator == "result-text-contains") && p.Value == "" {
			return fmt.Errorf("text result predicate has no value")
		}
		return nil
	}
	if p.Operator == "regular-file" {
		if !scratch[p.Scratch] || p.Step != "" || p.Stream != "" || p.Value != "" || p.Result != "" {
			return fmt.Errorf("regular-file predicate has invalid scratch/step fields")
		}
		return nil
	}
	if p.Operator == "execroot-exists" || p.Operator == "execroot-regular-file" {
		if p.Value == "" || p.Step != "" || p.Stream != "" || p.Scratch != "" || p.Result != "" {
			return fmt.Errorf("%s predicate has invalid fields", p.Operator)
		}
		if err := validateProbeTemplate(p.Value, scratch, sources, sourceRoots, inputCount); err != nil {
			return fmt.Errorf("%s predicate: %w", p.Operator, err)
		}
		return nil
	}
	if _, ok := steps[p.Step]; !ok {
		return fmt.Errorf("predicate references unavailable step %q", p.Step)
	}
	if p.Operator == "exit-zero" {
		if p.Stream != "" || p.Value != "" || p.Scratch != "" || p.Result != "" {
			return fmt.Errorf("exit-zero predicate has unrelated fields")
		}
		return nil
	}
	if p.Stream != "stdout" && p.Stream != "stderr" && p.Stream != "combined" {
		return fmt.Errorf("predicate has invalid stream %q", p.Stream)
	}
	if p.Scratch != "" || p.Result != "" {
		return fmt.Errorf("predicate %q has invalid value/scratch fields", p.Operator)
	}
	switch p.Operator {
	case "stream-empty", "stream-trimmed-empty":
		if p.Value != "" {
			return fmt.Errorf("predicate %q has invalid value fields", p.Operator)
		}
	case "stream-contains", "stream-matches":
		if p.Value == "" {
			return fmt.Errorf("predicate %q has invalid value fields", p.Operator)
		}
	}
	if p.Operator == "stream-matches" {
		if len(p.Value) > 4096 {
			return fmt.Errorf("stream-matches pattern exceeds 4096 bytes")
		}
		if _, err := regexp.Compile(p.Value); err != nil {
			return fmt.Errorf("stream-matches pattern is invalid: %w", err)
		}
	}
	return nil
}

// NormalizeProbeExecrootRelativePath converts one process-reported path to the
// canonical slash-separated form stored in ProbeResults. Paths already inside
// execroot take the direct path. A compiler may instead report the physical
// repository-cache path behind its Bazel execroot symlink; in that case this
// function rebases through the deepest configured-tool ancestor that resolves
// over the reported artifact, and verifies both names resolve identically.
func NormalizeProbeExecrootRelativePath(execroot, configuredTool, value string) (string, error) {
	root, err := filepath.Abs(execroot)
	if err != nil {
		return "", fmt.Errorf("resolve probe execroot: %w", err)
	}
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\x00\r\n") || strings.ContainsRune(value, '\\') {
		return "", fmt.Errorf("probe path %q is empty or contains an unsupported character", value)
	}
	reported := filepath.Clean(filepath.FromSlash(value))
	if !filepath.IsAbs(reported) {
		return normalizeProbeRelativePath(reported, value)
	}
	if relative, inside := probePathWithin(root, reported); inside {
		return normalizeProbeRelativePath(relative, value)
	}

	tool := filepath.Clean(configuredTool)
	if !filepath.IsAbs(tool) {
		tool = filepath.Join(root, tool)
	}
	if _, inside := probePathWithin(root, tool); !inside {
		return "", fmt.Errorf("configured probe tool %q is outside the execroot", configuredTool)
	}
	reportedResolved, err := filepath.EvalSymlinks(reported)
	if err != nil {
		return "", fmt.Errorf("resolve reported probe path %q: %w", value, err)
	}
	for ancestor := filepath.Dir(tool); ; ancestor = filepath.Dir(ancestor) {
		if _, inside := probePathWithin(root, ancestor); !inside {
			break
		}
		resolvedAncestor, resolveErr := filepath.EvalSymlinks(ancestor)
		if resolveErr == nil {
			if suffix, contains := probePathWithin(resolvedAncestor, reportedResolved); contains {
				candidate := filepath.Join(ancestor, suffix)
				if _, statErr := os.Stat(candidate); statErr != nil {
					return "", fmt.Errorf("inspect rebased probe path %q: %w", candidate, statErr)
				}
				candidateResolved, evalErr := filepath.EvalSymlinks(candidate)
				if evalErr != nil {
					return "", fmt.Errorf("resolve rebased probe path %q: %w", candidate, evalErr)
				}
				if filepath.Clean(candidateResolved) != filepath.Clean(reportedResolved) {
					return "", fmt.Errorf("rebased probe path %q does not resolve to reported path %q", candidate, value)
				}
				relative, inside := probePathWithin(root, candidate)
				if !inside {
					return "", fmt.Errorf("rebased probe path %q is outside the execroot", candidate)
				}
				return normalizeProbeRelativePath(relative, value)
			}
		}
		if ancestor == root {
			break
		}
	}
	return "", fmt.Errorf("probe path %q is outside the execroot and unrelated to configured tool %q", value, configuredTool)
}

func probePathWithin(root, candidate string) (string, bool) {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", false
	}
	return relative, true
}

func normalizeProbeRelativePath(local, original string) (string, error) {
	normalized := filepath.ToSlash(filepath.Clean(local))
	if normalized == "." || normalized == ".." || strings.HasPrefix(normalized, "../") || path.IsAbs(normalized) || path.Clean(normalized) != normalized {
		return "", fmt.Errorf("probe path %q is outside the execroot or noncanonical", original)
	}
	return normalized, nil
}

// ValidateProbeExecrootRelativePath rejects absolute, escaping, or
// noncanonical values that claim to have already been normalized by proberun.
func ValidateProbeExecrootRelativePath(value string) error {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") || strings.ContainsRune(value, '\\') || path.IsAbs(value) || path.Clean(value) != value || value == "." || value == ".." || strings.HasPrefix(value, "../") {
		return fmt.Errorf("probe path %q is not canonical and execroot-relative", value)
	}
	return nil
}

func ValidateProbePathComponent(value string) error {
	if value == "" || value == "." || value == ".." || strings.TrimSpace(value) != value ||
		strings.ContainsAny(value, "\x00\r\n/\\") || !linuxProbeSafePathComponent.MatchString(value) {
		return fmt.Errorf("probe text %q is not one safe path component", value)
	}
	return nil
}

// ValidateProbeSingleMakeWord verifies the exact cardinality contract used by
// symbolic Make word-boundary lowering. strings.Fields is also the parser's
// word model, so discovery and replay agree on what constitutes one word.
func ValidateProbeSingleMakeWord(value string) error {
	fields := strings.Fields(value)
	if strings.ContainsRune(value, 0) || len(fields) != 1 || fields[0] != value {
		return fmt.Errorf("probe text %q is not one nonempty Make word", value)
	}
	return nil
}

func (o ProbeOutcome) validate(steps map[string]int, scratch, sources, sourceRoots map[string]bool, inputCount int) error {
	switch o.Kind {
	case "boolean":
		if o.Predicate == nil || o.Step != "" || o.Stream != "" || len(o.Fragments) != 0 || o.Result != "" || o.Word != 0 || o.TrimSpace || o.FirstLine || o.LastLine || o.GNUMakeShell || o.RequireSuccess || o.SingleMakeWord || o.PathComponent {
			return errors.New("boolean probe outcome requires only predicate")
		}
		if err := o.Predicate.validate(steps, scratch, sources, sourceRoots, inputCount); err != nil {
			return fmt.Errorf("probe outcome: %w", err)
		}
	case "text":
		if o.Predicate != nil {
			return errors.New("text probe outcome cannot have predicate")
		}
		if len(o.Fragments) != 0 {
			if o.Step != "" || o.Stream != "" || o.Result != "" || o.Word != 0 || o.TrimSpace || o.FirstLine || o.LastLine || o.GNUMakeShell || o.RequireSuccess || o.SingleMakeWord || o.PathComponent {
				return errors.New("derived text probe outcome requires only fragments")
			}
			fragmentCount, fragmentBytes, transformCount := 0, 0, 0
			if err := validateProbeValueFragments(
				"derived text probe outcome", o.Fragments, 0,
				steps, scratch, sources, sourceRoots, inputCount,
				&fragmentCount, &fragmentBytes, &transformCount,
			); err != nil {
				return err
			}
			if err := validateProbeDependencyFragments(o.Fragments, inputCount); err != nil {
				return fmt.Errorf("derived text probe outcome: %w", err)
			}
			return nil
		}
		if o.Result != "" {
			if !validProbeResultOrdinal(o.Result, inputCount) || o.Word < 1 || o.Word > 1024 || o.Step != "" || o.Stream != "" || o.TrimSpace || o.FirstLine || o.LastLine || o.GNUMakeShell || o.RequireSuccess || o.SingleMakeWord || o.PathComponent {
				return errors.New("dependency text probe outcome requires only a valid result and word")
			}
			return nil
		}
		if o.Word != 0 {
			return errors.New("step text probe outcome cannot select a dependency word")
		}
		if o.FirstLine && o.LastLine {
			return errors.New("step text probe outcome cannot select both first and last line")
		}
		if o.GNUMakeShell && (o.TrimSpace || o.FirstLine || o.LastLine || o.PathComponent) {
			return errors.New("GNU Make shell text outcome cannot combine another text reduction")
		}
		if o.SingleMakeWord && o.PathComponent {
			return errors.New("single-Make-word and path-component text outcomes are redundant")
		}
		if _, ok := steps[o.Step]; !ok {
			return fmt.Errorf("text probe outcome references unavailable step %q", o.Step)
		}
		if o.Stream != "stdout" && o.Stream != "stderr" && o.Stream != "combined" {
			return fmt.Errorf("text probe outcome has invalid stream %q", o.Stream)
		}
	default:
		return fmt.Errorf("probe outcome has unsupported kind %q", o.Kind)
	}
	return nil
}

// validateProbeDependencyFragments narrows the generic fragment grammar for a
// derived text outcome. Such an outcome is deliberately a pure projection of
// completed dependency results: it cannot observe a process, configured tool,
// source tree, scratch path, or execroot state that replay cannot reproduce.
func validateProbeDependencyFragments(fragments []ProbeValueFragment, inputCount int) error {
	var validateFragments func([]ProbeValueFragment) error
	validateFragments = func(values []ProbeValueFragment) error {
		for _, fragment := range values {
			if fragment.Value != "" {
				if err := validateProbeDependencyTemplate(fragment.Value, inputCount); err != nil {
					return err
				}
			}
			if fragment.When != nil {
				if err := validateProbeDependencyPredicate(*fragment.When); err != nil {
					return err
				}
			}
			if err := validateFragments(fragment.Fragments); err != nil {
				return err
			}
			for _, transform := range fragment.Transforms {
				for _, argument := range transform.Arguments {
					if err := validateProbeDependencyTemplate(argument, inputCount); err != nil {
						return err
					}
				}
				for _, group := range transform.ArgumentFragments {
					if err := validateFragments(group.Fragments); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	return validateFragments(fragments)
}

func validateProbeDependencyTemplate(value string, inputCount int) error {
	for _, match := range probePlaceholder.FindAllStringSubmatch(value, -1) {
		if match[1] != "result" {
			return fmt.Errorf("dependency fragment references non-result placeholder %q", match[0])
		}
		parts := probeResultReference.FindStringSubmatch(match[2])
		if parts == nil || parts[2] != "text" || !validProbeResultOrdinal(parts[1], inputCount) {
			return fmt.Errorf("dependency fragment references unavailable text result %q", match[2])
		}
	}
	if strings.Contains(probePlaceholder.ReplaceAllString(value, ""), "${") {
		return errors.New("dependency fragment contains malformed or unsupported placeholder")
	}
	return nil
}

func validateProbeDependencyPredicate(predicate ProbePredicate) error {
	switch predicate.Operator {
	case "result-true", "result-false":
		return nil
	case "all", "any", "not":
		for _, operand := range predicate.Operands {
			if err := validateProbeDependencyPredicate(operand); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("dependency fragment uses non-boolean-result predicate %q", predicate.Operator)
	}
}

func validProbeResultOrdinal(value string, inputCount int) bool {
	if len(value) != 8 || strings.Trim(value, "0123456789") != "" {
		return false
	}
	var ordinal int
	for _, character := range value {
		ordinal = ordinal*10 + int(character-'0')
	}
	return ordinal < inputCount
}

func ReadProbeRequest(filename string) (*ProbeRequest, error) {
	var request ProbeRequest
	if err := readCanonicalProbeJSON(filename, &request, func() ([]byte, error) { return request.CanonicalJSON() }); err != nil {
		return nil, fmt.Errorf("read probe request: %w", err)
	}
	return &request, nil
}

func (r ProbeResult) CanonicalJSON() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func (r ProbeResult) Validate() error {
	if r.Schema != LinuxProbeResultSchema {
		return fmt.Errorf("probe result schema %q, want %q", r.Schema, LinuxProbeResultSchema)
	}
	if err := validatePlanDigest("probe result node ID", r.NodeID); err != nil {
		return err
	}
	if err := validatePlanDigest("probe result request ID", r.RequestID); err != nil {
		return err
	}
	if r.Scope != "target" && r.Scope != "host" {
		return fmt.Errorf("probe result has invalid scope %q", r.Scope)
	}
	if err := validateProbeIdentity(r.ToolsetIdentity); err != nil {
		return err
	}
	if r.Kind == "boolean" {
		if r.Boolean == nil || r.Text != "" {
			return errors.New("boolean probe result has invalid value fields")
		}
	} else if r.Kind == "text" {
		if r.Boolean != nil {
			return errors.New("text probe result has a boolean value")
		}
	} else {
		return fmt.Errorf("probe result has unsupported kind %q", r.Kind)
	}
	seen := map[string]bool{}
	for _, step := range r.Steps {
		if err := validatePlanName("probe result step", step.Name); err != nil {
			return err
		}
		if seen[step.Name] {
			return fmt.Errorf("probe result repeats step %q", step.Name)
		}
		seen[step.Name] = true
		if step.Status != "success" && step.Status != "failure" && step.Status != "skipped" {
			return fmt.Errorf("probe result step %q has invalid status %q", step.Name, step.Status)
		}
		if step.Status == "success" && step.ExitCode != 0 || step.Status == "failure" && step.ExitCode == 0 || step.Status == "skipped" && (step.ExitCode != -1 || step.Stdout != "" || step.Stderr != "") {
			return fmt.Errorf("probe result step %q status and process fields disagree", step.Name)
		}
	}
	return nil
}

func ReadProbeResult(filename string) (*ProbeResult, error) {
	var result ProbeResult
	if err := readCanonicalProbeJSON(filename, &result, func() ([]byte, error) { return result.CanonicalJSON() }); err != nil {
		return nil, fmt.Errorf("read probe result: %w", err)
	}
	return &result, nil
}

func readCanonicalProbeJSON(filename string, value any, canonical func() ([]byte, error)) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	want, err := canonical()
	if err != nil {
		return err
	}
	if !bytes.Equal(data, want) {
		return errors.New("not canonically encoded")
	}
	return nil
}

type ProbePlan struct {
	Toolsets map[string]string
	Requests map[string]ProbeRequest
	Nodes    []ProbePlanNode
	Terminal []string
}

type ProbePlanNode struct {
	ID        string
	Scope     string
	RequestID string
	Inputs    []string
}

func (n ProbePlanNode) ContentID() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00", LinuxProbePlanSchema, n.Scope, n.RequestID)
	for index, input := range n.Inputs {
		fmt.Fprintf(h, "%08d\x00%s\x00", index, input)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (p *ProbePlan) Write(outputDir string) error {
	entries, err := p.entries()
	if err != nil {
		return err
	}
	return writeActionPlanTree(outputDir, entries)
}

func (p *ProbePlan) entries() ([]actionPlanEntry, error) {
	if p == nil {
		return nil, errors.New("probe plan is nil")
	}
	entries := []actionPlanEntry{{path: path.Join("schema", LinuxProbePlanSchema)}}
	for scope := range p.Toolsets {
		if scope != "target" && scope != "host" {
			return nil, fmt.Errorf("probe plan has unknown toolset scope %q", scope)
		}
	}
	if p.Toolsets["target"] == "" {
		return nil, errors.New("probe plan has no target toolset")
	}
	for _, scope := range []string{"host", "target"} {
		if identity := p.Toolsets[scope]; identity != "" {
			if err := validateProbeIdentity(identity); err != nil {
				return nil, fmt.Errorf("%s probe toolset: %w", scope, err)
			}
			entries = append(entries, actionPlanEntry{path: path.Join("toolsets", scope, identity)})
		}
	}
	requests := map[string]ProbeRequest{}
	for id, request := range p.Requests {
		if err := validatePlanDigest("probe request ID", id); err != nil {
			return nil, err
		}
		data, err := request.CanonicalJSON()
		if err != nil {
			return nil, fmt.Errorf("probe request %s: %w", id, err)
		}
		actual, _ := request.ID()
		if actual != id {
			return nil, fmt.Errorf("probe request ID %s does not match canonical content %s", id, actual)
		}
		requests[id] = request
		entries = append(entries, actionPlanEntry{path: path.Join("requests", id+".json"), data: data})
	}
	nodes := map[string]ProbePlanNode{}
	for _, node := range p.Nodes {
		if node.Scope != "target" && node.Scope != "host" {
			return nil, fmt.Errorf("probe node has invalid scope %q", node.Scope)
		}
		if p.Toolsets[node.Scope] == "" {
			return nil, fmt.Errorf("probe node %s has no %s toolset", node.ID, node.Scope)
		}
		request, ok := requests[node.RequestID]
		if !ok {
			return nil, fmt.Errorf("probe node %s references unknown request %s", node.ID, node.RequestID)
		}
		if err := validatePlanDigest("probe node ID", node.ID); err != nil {
			return nil, err
		}
		if len(node.Inputs) != request.InputCount {
			return nil, fmt.Errorf("probe node %s has %d inputs, request requires %d", node.ID, len(node.Inputs), request.InputCount)
		}
		if node.ContentID() != node.ID {
			return nil, fmt.Errorf("probe node ID %s does not match canonical content %s", node.ID, node.ContentID())
		}
		if _, exists := nodes[node.ID]; exists {
			return nil, fmt.Errorf("probe plan repeats node %s", node.ID)
		}
		nodes[node.ID] = node
		root := path.Join("nodes", node.ID)
		entries = append(entries, actionPlanEntry{path: path.Join(root, "scope", node.Scope)}, actionPlanEntry{path: path.Join(root, "request", node.RequestID)})
		for _, role := range request.ToolRoles() {
			entries = append(entries, actionPlanEntry{path: path.Join(root, "tool", role)})
		}
		for _, source := range request.Sources {
			entries = append(entries, actionPlanEntry{path: probePlanSourceMarker(root, source)})
		}
		for _, sourceRoot := range request.SourceRoots {
			entries = append(entries, actionPlanEntry{path: path.Join(root, "source_root", sourceRoot)})
		}
		for index, input := range node.Inputs {
			if err := validatePlanDigest("probe dependency", input); err != nil {
				return nil, err
			}
			entries = append(entries, actionPlanEntry{path: path.Join(root, "in", planOrdinal(index), input)})
		}
	}
	for _, node := range p.Nodes {
		for _, input := range node.Inputs {
			producer, ok := nodes[input]
			if !ok {
				return nil, fmt.Errorf("probe node %s references unknown dependency %s", node.ID, input)
			}
			if node.Scope == "host" && producer.Scope != "host" {
				return nil, fmt.Errorf("host probe node %s depends on target node %s", node.ID, input)
			}
		}
	}
	if err := validateProbePlanAcyclic(nodes); err != nil {
		return nil, err
	}
	seenTerminal := map[string]bool{}
	for _, id := range p.Terminal {
		if _, ok := nodes[id]; !ok || seenTerminal[id] {
			return nil, fmt.Errorf("probe plan has invalid or repeated terminal %s", id)
		}
		seenTerminal[id] = true
		entries = append(entries, actionPlanEntry{path: path.Join("terminal", id)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	for i := 1; i < len(entries); i++ {
		if entries[i-1].path == entries[i].path {
			return nil, fmt.Errorf("probe plan repeats marker %q", entries[i].path)
		}
	}
	return entries, nil
}

// probePlanSourceMarker preserves a canonical source-relative path in the
// path-only plan without creating file/directory prefix collisions. Every
// source component is a directory prefixed with '+', while the final marker
// is the unprefixed literal "path". Thus sources "foo" and "foo/path" map to
// .../source/+foo/path and .../source/+foo/+path/path respectively.
func probePlanSourceMarker(nodeRoot, source string) string {
	components := []string{nodeRoot, "source"}
	for _, component := range strings.Split(source, "/") {
		components = append(components, "+"+component)
	}
	return path.Join(append(components, "path")...)
}

func validateProbePlanAcyclic(nodes map[string]ProbePlanNode) error {
	state := map[string]byte{}
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return fmt.Errorf("probe plan contains a cycle at node %s", id)
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		for _, input := range nodes[id].Inputs {
			if err := visit(input); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for id := range nodes {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

// ReadProbeResultTree strictly loads results/<node>.json. It rejects extra
// files so a final planner cannot accidentally consume undeclared state.
func ReadProbeResultTree(root string) (map[string]ProbeResult, error) {
	out := map[string]ProbeResult{}
	emptyMarker := false
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ".empty" {
			info, err := os.Stat(filename)
			if err != nil {
				return err
			}
			if info.Size() != 0 || emptyMarker {
				return fmt.Errorf("probe result tree has invalid empty marker")
			}
			emptyMarker = true
			return nil
		}
		parts := strings.Split(rel, "/")
		if len(parts) != 2 || parts[0] != "results" || !strings.HasSuffix(parts[1], ".json") {
			return fmt.Errorf("probe result tree contains unknown file %q", rel)
		}
		id := strings.TrimSuffix(parts[1], ".json")
		if err := validatePlanDigest("probe result filename", id); err != nil {
			return err
		}
		result, err := ReadProbeResult(filename)
		if err != nil {
			return err
		}
		if result.NodeID != id {
			return fmt.Errorf("probe result %s claims node ID %s", id, result.NodeID)
		}
		if _, exists := out[id]; exists {
			return fmt.Errorf("probe result tree repeats node %s", id)
		}
		out[id] = *result
		return nil
	})
	if err == nil && emptyMarker && len(out) != 0 {
		return nil, fmt.Errorf("probe result tree mixes empty marker and results")
	}
	return out, err
}
