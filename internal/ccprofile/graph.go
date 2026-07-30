package ccprofile

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	GraphProfileSchema     = "linux.bzl/toolchain-graph-profile-v1"
	GraphProjectionSchema  = "linux.bzl/consumed-graph-projection-v1"
	GraphProfileSourceRoot = "__LINUX_BZL_SRCTREE__"

	KconfigCommandKindStdout  = "stdout"
	KconfigCommandKindSuccess = "success"

	KbuildGraphProbeKindASOption = "as_option"
	KbuildGraphProbeKindCCOption = "cc_option"
	KbuildGraphProbeKindLDOption = "ld_option"
)

const (
	kconfigCommandIDDomain   = "linux.bzl/kconfig-command/v1\x00"
	kbuildGraphProbeIDDomain = "linux.bzl/kbuild-graph-probe/v1\x00"
)

const (
	GraphProfileEntryKconfigCommand   = "kconfig command"
	GraphProfileEntryKbuildGraphProbe = "Kbuild graph probe"
)

// MissingGraphProfileEntryError identifies an exact request absent from an
// otherwise valid graph profile. Callers may explicitly extend a profile only
// for this error; validation and semantic errors must remain hard failures.
type MissingGraphProfileEntryError struct {
	Entry string
	ID    string
}

func (err *MissingGraphProfileEntryError) Error() string {
	return fmt.Sprintf("%s %s is missing from graph profile", err.Entry, err.ID)
}

func IsMissingGraphProfileEntry(err error) bool {
	var missing *MissingGraphProfileEntryError
	return errors.As(err, &missing)
}

// GraphProfile is the checked-in, toolchain-and-architecture-specific
// superset of exact Kconfig command results that may affect a generated graph.
type GraphProfile struct {
	Schema            string             `json:"schema"`
	Architecture      string             `json:"architecture"`
	DriverContract    string             `json:"driver_contract"`
	AnalysisIdentity  AnalysisIdentity   `json:"analysis_identity"`
	KconfigCommands   []KconfigCommand   `json:"kconfig_commands"`
	KbuildGraphProbes []KbuildGraphProbe `json:"kbuild_graph_probes"`
}

// KconfigCommand records an exact command identity and its typed result.
// Exactly one of Stdout and Success must be present, as selected by Kind.
type KconfigCommand struct {
	ID          string            `json:"id"`
	Kind        string            `json:"kind"`
	Command     string            `json:"command"`
	Environment map[string]string `json:"environment"`
	Inputs      map[string]string `json:"inputs"`
	Stdout      *string           `json:"stdout,omitempty"`
	Success     *bool             `json:"success,omitempty"`
}

// KconfigCommandIdentity is the result-independent identity used to request a
// command from a graph-profile superset.
type KconfigCommandIdentity struct {
	ID          string            `json:"id"`
	Kind        string            `json:"kind"`
	Command     string            `json:"command"`
	Environment map[string]string `json:"environment"`
	Inputs      map[string]string `json:"inputs"`
}

// KbuildGraphProbe records one exact graph-shaping Kbuild flag-selection
// decision in the generated graph's consumed toolchain projection.
type KbuildGraphProbe struct {
	ID            string            `json:"id"`
	Kind          string            `json:"kind"`
	Language      string            `json:"language"`
	ContextArgv   []string          `json:"context_argv"`
	CandidateArgv []string          `json:"candidate_argv"`
	Inputs        map[string]string `json:"inputs"`
	Supported     bool              `json:"supported"`
}

// KbuildGraphProbeIdentity is the result-independent probe request.
type KbuildGraphProbeIdentity struct {
	ID            string            `json:"id"`
	Kind          string            `json:"kind"`
	Language      string            `json:"language"`
	ContextArgv   []string          `json:"context_argv"`
	CandidateArgv []string          `json:"candidate_argv"`
	Inputs        map[string]string `json:"inputs"`
}

// GraphProfileIdentity names the selected toolchain and architecture boundary.
type GraphProfileIdentity struct {
	Architecture     string           `json:"architecture"`
	DriverContract   string           `json:"driver_contract"`
	AnalysisIdentity AnalysisIdentity `json:"analysis_identity"`
}

// GraphProjection contains only the exact command and Kbuild probe results
// consumed by one generated graph. The checked-in GraphProfile may contain
// additional entries.
type GraphProjection struct {
	Architecture      string             `json:"architecture"`
	DriverContract    string             `json:"driver_contract"`
	AnalysisIdentity  AnalysisIdentity   `json:"analysis_identity"`
	KconfigCommands   []KconfigCommand   `json:"kconfig_commands"`
	KbuildGraphProbes []KbuildGraphProbe `json:"kbuild_graph_probes"`
}

// GraphProfileResolver resolves exact command results and records the subset
// consumed by one graph evaluation.
type GraphProfileResolver struct {
	profile        GraphProfile
	commandsByID   map[string]KconfigCommand
	probesByID     map[string]KbuildGraphProbe
	mu             sync.Mutex
	consumed       map[string]KconfigCommand
	consumedProbes map[string]KbuildGraphProbe
}

func (profile GraphProfile) Identity() GraphProfileIdentity {
	return GraphProfileIdentity{
		Architecture:     profile.Architecture,
		DriverContract:   profile.DriverContract,
		AnalysisIdentity: profile.AnalysisIdentity,
	}
}

func (projection GraphProjection) Identity() GraphProfileIdentity {
	return GraphProfileIdentity{
		Architecture:     projection.Architecture,
		DriverContract:   projection.DriverContract,
		AnalysisIdentity: projection.AnalysisIdentity,
	}
}

func (command KconfigCommand) Identity() KconfigCommandIdentity {
	return KconfigCommandIdentity{
		ID:          command.ID,
		Kind:        command.Kind,
		Command:     command.Command,
		Environment: cloneStringMap(command.Environment),
		Inputs:      cloneStringMap(command.Inputs),
	}
}

func (probe KbuildGraphProbe) Identity() KbuildGraphProbeIdentity {
	return KbuildGraphProbeIdentity{
		ID:            probe.ID,
		Kind:          probe.Kind,
		Language:      probe.Language,
		ContextArgv:   slices.Clone(probe.ContextArgv),
		CandidateArgv: slices.Clone(probe.CandidateArgv),
		Inputs:        cloneStringMap(probe.Inputs),
	}
}

// KconfigCommandID returns the result-independent ID for a command record.
func KconfigCommandID(command KconfigCommand) string {
	return KconfigCommandIdentityID(command.Identity())
}

// KconfigCommandIdentityID returns the canonical ID for a command request.
func KconfigCommandIdentityID(identity KconfigCommandIdentity) string {
	digest := sha256.New()
	digest.Write([]byte(kconfigCommandIDDomain))
	writeGraphHashString(digest, identity.Kind)
	writeGraphHashString(digest, identity.Command)
	writeGraphHashMap(digest, identity.Environment)
	writeGraphHashMap(digest, identity.Inputs)
	return hex.EncodeToString(digest.Sum(nil))
}

func NewKconfigCommandIdentity(
	kind string,
	command string,
	environment map[string]string,
	inputs map[string]string,
) (KconfigCommandIdentity, error) {
	identity := KconfigCommandIdentity{
		Kind:        kind,
		Command:     command,
		Environment: cloneStringMap(environment),
		Inputs:      cloneStringMap(inputs),
	}
	identity.ID = KconfigCommandIdentityID(identity)
	if err := validateKconfigCommandIdentity(identity, "kconfig command identity"); err != nil {
		return KconfigCommandIdentity{}, err
	}
	return identity, nil
}

func KbuildGraphProbeID(probe KbuildGraphProbe) string {
	return KbuildGraphProbeIdentityID(probe.Identity())
}

func KbuildGraphProbeIdentityID(identity KbuildGraphProbeIdentity) string {
	digest := sha256.New()
	digest.Write([]byte(kbuildGraphProbeIDDomain))
	writeGraphHashString(digest, identity.Kind)
	writeGraphHashString(digest, identity.Language)
	writeGraphHashStrings(digest, identity.ContextArgv)
	writeGraphHashStrings(digest, identity.CandidateArgv)
	writeGraphHashMap(digest, identity.Inputs)
	return hex.EncodeToString(digest.Sum(nil))
}

func NewKbuildGraphProbeIdentity(
	kind string,
	language string,
	contextArgv []string,
	candidateArgv []string,
	inputs map[string]string,
) (KbuildGraphProbeIdentity, error) {
	identity := KbuildGraphProbeIdentity{
		Kind:          kind,
		Language:      language,
		ContextArgv:   slices.Clone(contextArgv),
		CandidateArgv: slices.Clone(candidateArgv),
		Inputs:        cloneStringMap(inputs),
	}
	identity.ID = KbuildGraphProbeIdentityID(identity)
	if err := validateKbuildGraphProbeIdentity(identity, "Kbuild graph probe identity"); err != nil {
		return KbuildGraphProbeIdentity{}, err
	}
	return identity, nil
}

func writeGraphHashMap(digest hash.Hash, values map[string]string) {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	writeGraphHashUint64(digest, uint64(len(names)))
	for _, name := range names {
		writeGraphHashString(digest, name)
		writeGraphHashString(digest, values[name])
	}
}

func writeGraphHashStrings(digest hash.Hash, values []string) {
	writeGraphHashUint64(digest, uint64(len(values)))
	for _, value := range values {
		writeGraphHashString(digest, value)
	}
}

func writeGraphHashString(digest hash.Hash, value string) {
	writeGraphHashUint64(digest, uint64(len(value)))
	digest.Write([]byte(value))
}

func writeGraphHashUint64(digest hash.Hash, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	digest.Write(encoded[:])
}

func ValidateGraphProfile(profile GraphProfile) error {
	if profile.Schema != GraphProfileSchema {
		return fmt.Errorf("graph profile schema %q, want %q", profile.Schema, GraphProfileSchema)
	}
	if err := validateGraphProfileIdentity(profile.Identity()); err != nil {
		return err
	}
	previousID := ""
	for index, command := range profile.KconfigCommands {
		context := fmt.Sprintf("kconfig_commands[%d]", index)
		if err := validateKconfigCommand(command, context); err != nil {
			return err
		}
		if command.ID <= previousID {
			return fmt.Errorf("kconfig_commands must be sorted by unique ID")
		}
		previousID = command.ID
	}
	previousID = ""
	for index, probe := range profile.KbuildGraphProbes {
		context := fmt.Sprintf("kbuild_graph_probes[%d]", index)
		if err := validateKbuildGraphProbe(probe, context); err != nil {
			return err
		}
		if probe.ID <= previousID {
			return fmt.Errorf("kbuild_graph_probes must be sorted by unique ID")
		}
		previousID = probe.ID
	}
	return nil
}

func validateGraphProfileIdentity(identity GraphProfileIdentity) error {
	if err := validateArchitecture(identity.Architecture); err != nil {
		return err
	}
	if identity.DriverContract != DriverContract {
		return fmt.Errorf("driver_contract %q, want %q", identity.DriverContract, DriverContract)
	}
	return validateAnalysisIdentity(identity.AnalysisIdentity)
}

func validateKconfigCommand(command KconfigCommand, context string) error {
	if err := validateKconfigCommandIdentity(command.Identity(), context); err != nil {
		return err
	}
	switch command.Kind {
	case KconfigCommandKindStdout:
		if command.Stdout == nil {
			return fmt.Errorf("%s stdout result is required for kind %q", context, command.Kind)
		}
		if command.Success != nil {
			return fmt.Errorf("%s success result is forbidden for kind %q", context, command.Kind)
		}
		if strings.ContainsRune(*command.Stdout, '\x00') {
			return fmt.Errorf("%s stdout contains NUL", context)
		}
	case KconfigCommandKindSuccess:
		if command.Success == nil {
			return fmt.Errorf("%s success result is required for kind %q", context, command.Kind)
		}
		if command.Stdout != nil {
			return fmt.Errorf("%s stdout result is forbidden for kind %q", context, command.Kind)
		}
	}
	return nil
}

func validateKconfigCommandIdentity(identity KconfigCommandIdentity, context string) error {
	if identity.Kind != KconfigCommandKindStdout && identity.Kind != KconfigCommandKindSuccess {
		return fmt.Errorf("%s has unsupported kind %q", context, identity.Kind)
	}
	if identity.Command == "" {
		return fmt.Errorf("%s command must not be empty", context)
	}
	if strings.ContainsRune(identity.Command, '\x00') {
		return fmt.Errorf("%s command contains NUL", context)
	}
	if err := validateGraphStringMap(identity.Environment, context+".environment", true); err != nil {
		return err
	}
	if err := validateGraphInputs(identity.Inputs, context+".inputs"); err != nil {
		return err
	}
	expectedID := KconfigCommandIdentityID(identity)
	if identity.ID != expectedID {
		return fmt.Errorf("%s ID %q, want %q", context, identity.ID, expectedID)
	}
	return nil
}

func validateKbuildGraphProbe(probe KbuildGraphProbe, context string) error {
	return validateKbuildGraphProbeIdentity(probe.Identity(), context)
}

func validateKbuildGraphProbeIdentity(
	identity KbuildGraphProbeIdentity,
	context string,
) error {
	languages := map[string]string{
		KbuildGraphProbeKindASOption: "asm",
		KbuildGraphProbeKindCCOption: "c",
		KbuildGraphProbeKindLDOption: "link",
	}
	language, ok := languages[identity.Kind]
	if !ok {
		return fmt.Errorf("%s has unsupported kind %q", context, identity.Kind)
	}
	if identity.Language != language {
		return fmt.Errorf(
			"%s kind %q requires language %q, got %q",
			context,
			identity.Kind,
			language,
			identity.Language,
		)
	}
	for _, values := range []struct {
		name string
		argv []string
	}{
		{name: "context_argv", argv: identity.ContextArgv},
		{name: "candidate_argv", argv: identity.CandidateArgv},
	} {
		if values.name == "candidate_argv" && len(values.argv) == 0 {
			return fmt.Errorf("%s candidate_argv must not be empty", context)
		}
		for index, arg := range values.argv {
			if arg == "" || strings.ContainsAny(arg, "\r\n\x00") {
				return fmt.Errorf(
					"%s %s[%d] must be non-empty and contain no CR, LF, or NUL",
					context,
					values.name,
					index,
				)
			}
		}
	}
	if err := validateGraphInputs(identity.Inputs, context+".inputs"); err != nil {
		return err
	}
	expectedID := KbuildGraphProbeIdentityID(identity)
	if identity.ID != expectedID {
		return fmt.Errorf("%s ID %q, want %q", context, identity.ID, expectedID)
	}
	return nil
}

func validateGraphStringMap(values map[string]string, context string, environment bool) error {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := values[name]
		if name == "" || strings.ContainsRune(name, '\x00') {
			return fmt.Errorf("%s contains invalid key %q", context, name)
		}
		if environment && strings.ContainsRune(name, '=') {
			return fmt.Errorf("%s contains invalid environment name %q", context, name)
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("%s[%q] contains NUL", context, name)
		}
	}
	return nil
}

func validateGraphInputs(inputs map[string]string, context string) error {
	if err := validateGraphStringMap(inputs, context, false); err != nil {
		return err
	}
	paths := make([]string, 0, len(inputs))
	for path := range inputs {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := validateLowerSHA256(inputs[path]); err != nil {
			return fmt.Errorf("%s[%q] must be a lowercase SHA-256 digest", context, path)
		}
	}
	return nil
}

func validateLowerSHA256(value string) error {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return fmt.Errorf("invalid lowercase SHA-256 digest")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("invalid lowercase SHA-256 digest")
	}
	return nil
}

func CanonicalGraphProfileJSON(profile GraphProfile) ([]byte, error) {
	profile = normalizeGraphProfile(profile)
	if err := ValidateGraphProfile(profile); err != nil {
		return nil, err
	}
	return marshalCanonical(profile, "toolchain graph profile")
}

func normalizeGraphProfile(profile GraphProfile) GraphProfile {
	commands := make([]KconfigCommand, len(profile.KconfigCommands))
	for index, command := range profile.KconfigCommands {
		commands[index] = cloneKconfigCommand(command)
	}
	slices.SortFunc(commands, func(left, right KconfigCommand) int {
		return strings.Compare(left.ID, right.ID)
	})
	profile.KconfigCommands = commands
	probes := make([]KbuildGraphProbe, len(profile.KbuildGraphProbes))
	for index, probe := range profile.KbuildGraphProbes {
		probes[index] = cloneKbuildGraphProbe(probe)
	}
	slices.SortFunc(probes, func(left, right KbuildGraphProbe) int {
		return strings.Compare(left.ID, right.ID)
	})
	profile.KbuildGraphProbes = probes
	return profile
}

func cloneKconfigCommand(command KconfigCommand) KconfigCommand {
	command.Environment = cloneStringMap(command.Environment)
	command.Inputs = cloneStringMap(command.Inputs)
	if command.Stdout != nil {
		value := *command.Stdout
		command.Stdout = &value
	}
	if command.Success != nil {
		value := *command.Success
		command.Success = &value
	}
	return command
}

func cloneKbuildGraphProbe(probe KbuildGraphProbe) KbuildGraphProbe {
	probe.ContextArgv = slices.Clone(probe.ContextArgv)
	if probe.ContextArgv == nil {
		probe.ContextArgv = []string{}
	}
	probe.CandidateArgv = slices.Clone(probe.CandidateArgv)
	if probe.CandidateArgv == nil {
		probe.CandidateArgv = []string{}
	}
	probe.Inputs = cloneStringMap(probe.Inputs)
	return probe
}

func GraphProfileDigest(profile GraphProfile) (string, error) {
	data, err := CanonicalGraphProfileJSON(profile)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(GraphProfileSchema+"\x00"), data...))
	return hex.EncodeToString(sum[:]), nil
}

func CompareGraphProfiles(expected, actual GraphProfile) error {
	if err := ValidateGraphProfile(expected); err != nil {
		return fmt.Errorf("expected graph profile: %w", err)
	}
	if err := ValidateGraphProfile(actual); err != nil {
		return fmt.Errorf("actual graph profile: %w", err)
	}
	if err := compareGraphProfileIdentity(expected.Identity(), actual.Identity()); err != nil {
		return err
	}
	if len(expected.KconfigCommands) != len(actual.KconfigCommands) {
		return fmt.Errorf(
			"kconfig_commands length mismatch: expected %d, got %d",
			len(expected.KconfigCommands),
			len(actual.KconfigCommands),
		)
	}
	for index := range expected.KconfigCommands {
		if err := compareKconfigCommands(expected.KconfigCommands[index], actual.KconfigCommands[index]); err != nil {
			return err
		}
	}
	if len(expected.KbuildGraphProbes) != len(actual.KbuildGraphProbes) {
		return fmt.Errorf(
			"kbuild_graph_probes length mismatch: expected %d, got %d",
			len(expected.KbuildGraphProbes),
			len(actual.KbuildGraphProbes),
		)
	}
	for index := range expected.KbuildGraphProbes {
		if err := compareKbuildGraphProbes(
			expected.KbuildGraphProbes[index],
			actual.KbuildGraphProbes[index],
		); err != nil {
			return err
		}
	}
	return nil
}

func MergeGraphProfiles(profiles ...GraphProfile) (GraphProfile, error) {
	if len(profiles) == 0 {
		return GraphProfile{}, fmt.Errorf("merge graph profiles requires at least one profile")
	}
	for index, profile := range profiles {
		if err := ValidateGraphProfile(profile); err != nil {
			return GraphProfile{}, fmt.Errorf("graph profile %d: %w", index, err)
		}
	}
	identity := profiles[0].Identity()
	commands := make(map[string]KconfigCommand)
	probes := make(map[string]KbuildGraphProbe)
	for profileIndex, profile := range profiles {
		if err := compareGraphProfileIdentity(identity, profile.Identity()); err != nil {
			return GraphProfile{}, fmt.Errorf("graph profile %d identity: %w", profileIndex, err)
		}
		for _, command := range profile.KconfigCommands {
			existing, ok := commands[command.ID]
			if ok {
				if err := compareKconfigCommands(existing, command); err != nil {
					return GraphProfile{}, fmt.Errorf(
						"graph profile %d conflicts on kconfig command %s: %w",
						profileIndex,
						command.ID,
						err,
					)
				}
				continue
			}
			commands[command.ID] = cloneKconfigCommand(command)
		}
		for _, probe := range profile.KbuildGraphProbes {
			existing, ok := probes[probe.ID]
			if ok {
				if err := compareKbuildGraphProbes(existing, probe); err != nil {
					return GraphProfile{}, fmt.Errorf(
						"graph profile %d conflicts on Kbuild graph probe %s: %w",
						profileIndex,
						probe.ID,
						err,
					)
				}
				continue
			}
			probes[probe.ID] = cloneKbuildGraphProbe(probe)
		}
	}
	mergedCommands := make([]KconfigCommand, 0, len(commands))
	for _, command := range commands {
		mergedCommands = append(mergedCommands, command)
	}
	mergedProbes := make([]KbuildGraphProbe, 0, len(probes))
	for _, probe := range probes {
		mergedProbes = append(mergedProbes, probe)
	}
	merged := normalizeGraphProfile(GraphProfile{
		Schema:            GraphProfileSchema,
		Architecture:      identity.Architecture,
		DriverContract:    identity.DriverContract,
		AnalysisIdentity:  identity.AnalysisIdentity,
		KconfigCommands:   mergedCommands,
		KbuildGraphProbes: mergedProbes,
	})
	if err := ValidateGraphProfile(merged); err != nil {
		return GraphProfile{}, err
	}
	return merged, nil
}

func ValidateGraphProfileIdentity(expected GraphProfile, actual GraphProfileIdentity) error {
	if err := ValidateGraphProfile(expected); err != nil {
		return err
	}
	if err := validateGraphProfileIdentity(actual); err != nil {
		return fmt.Errorf("selected graph profile identity: %w", err)
	}
	return compareGraphProfileIdentity(expected.Identity(), actual)
}

func ValidateGraphProfileCompilerIdentity(
	expected GraphProfile,
	actual CompilerIdentity,
) error {
	if err := ValidateCompilerIdentity(actual); err != nil {
		return err
	}
	if err := ValidateGraphProfileIdentity(expected, GraphProfileIdentity{
		Architecture:     actual.Architecture,
		DriverContract:   actual.DriverContract,
		AnalysisIdentity: actual.AnalysisIdentity,
	}); err != nil {
		return err
	}
	return validateGraphCompilerFacts(expected.KconfigCommands, actual)
}

func ValidateGraphProjectionCompilerIdentity(
	expected GraphProjection,
	actual CompilerIdentity,
) error {
	expected = normalizeGraphProjection(expected)
	if err := ValidateGraphProjection(expected); err != nil {
		return err
	}
	if err := ValidateCompilerIdentity(actual); err != nil {
		return err
	}
	if err := compareGraphProfileIdentity(expected.Identity(), GraphProfileIdentity{
		Architecture:     actual.Architecture,
		DriverContract:   actual.DriverContract,
		AnalysisIdentity: actual.AnalysisIdentity,
	}); err != nil {
		return err
	}
	return validateGraphCompilerFacts(expected.KconfigCommands, actual)
}

func validateGraphCompilerFacts(
	commands []KconfigCommand,
	actual CompilerIdentity,
) error {
	expectedVersionText := ""
	expectedCCName := ""
	expectedCCVersion := 0
	for _, command := range commands {
		if versionText := command.Environment["CC_VERSION_TEXT"]; versionText != "" {
			if expectedVersionText != "" && expectedVersionText != versionText {
				return fmt.Errorf(
					"consumed graph commands disagree on CC_VERSION_TEXT: %q and %q",
					expectedVersionText,
					versionText,
				)
			}
			expectedVersionText = versionText
		}
		if command.Kind != KconfigCommandKindStdout ||
			command.Stdout == nil ||
			!strings.Contains(command.Command, "/scripts/cc-version.sh") {
			continue
		}
		fields := strings.Fields(*command.Stdout)
		if len(fields) != 2 {
			return fmt.Errorf(
				"consumed cc-version command %s returned malformed result %q",
				command.ID,
				*command.Stdout,
			)
		}
		version, err := strconv.Atoi(fields[1])
		if err != nil || version <= 0 {
			return fmt.Errorf(
				"consumed cc-version command %s returned malformed version %q",
				command.ID,
				fields[1],
			)
		}
		if expectedCCName != "" &&
			(expectedCCName != fields[0] || expectedCCVersion != version) {
			return fmt.Errorf(
				"consumed cc-version commands disagree: %s %d and %s %d",
				expectedCCName,
				expectedCCVersion,
				fields[0],
				version,
			)
		}
		expectedCCName = fields[0]
		expectedCCVersion = version
	}
	if expectedVersionText != "" && actual.CCVersionText != expectedVersionText {
		return fmt.Errorf(
			"cc_version_text mismatch: expected %q from consumed graph commands, got %q",
			expectedVersionText,
			actual.CCVersionText,
		)
	}
	if expectedCCName != "" && actual.CCName != expectedCCName {
		return fmt.Errorf(
			"cc_name mismatch: expected %q from consumed graph commands, got %q",
			expectedCCName,
			actual.CCName,
		)
	}
	if expectedCCVersion != 0 && actual.CCVersion != expectedCCVersion {
		return fmt.Errorf(
			"cc_version mismatch: expected %d from consumed graph commands, got %d",
			expectedCCVersion,
			actual.CCVersion,
		)
	}
	return nil
}

func compareGraphProfileIdentity(expected, actual GraphProfileIdentity) error {
	if expected.Architecture != actual.Architecture {
		return fmt.Errorf(
			"architecture mismatch: expected %q, got %q",
			expected.Architecture,
			actual.Architecture,
		)
	}
	if expected.DriverContract != actual.DriverContract {
		return fmt.Errorf(
			"driver_contract mismatch: expected %q, got %q",
			expected.DriverContract,
			actual.DriverContract,
		)
	}
	if !analysisIdentityEqual(expected.AnalysisIdentity, actual.AnalysisIdentity) {
		return fmt.Errorf(
			"analysis_identity mismatch: expected %#v, got %#v",
			expected.AnalysisIdentity,
			actual.AnalysisIdentity,
		)
	}
	return nil
}

// ResolveKconfigCommand resolves one exact identity from a graph-profile
// superset. Profile entries that were not requested are deliberately ignored.
func ResolveKconfigCommand(
	profile GraphProfile,
	request KconfigCommandIdentity,
) (KconfigCommand, error) {
	if err := ValidateGraphProfile(profile); err != nil {
		return KconfigCommand{}, err
	}
	if err := validateKconfigCommandIdentity(request, "requested kconfig command"); err != nil {
		return KconfigCommand{}, err
	}
	index, found := slices.BinarySearchFunc(
		profile.KconfigCommands,
		request.ID,
		func(command KconfigCommand, id string) int {
			return strings.Compare(command.ID, id)
		},
	)
	if !found {
		return KconfigCommand{}, &MissingGraphProfileEntryError{
			Entry: GraphProfileEntryKconfigCommand,
			ID:    request.ID,
		}
	}
	command := profile.KconfigCommands[index]
	if err := compareKconfigCommandIdentities(command.Identity(), request); err != nil {
		return KconfigCommand{}, fmt.Errorf(
			"kconfig command %s semantic mismatch: %w",
			request.ID,
			err,
		)
	}
	return cloneKconfigCommand(command), nil
}

// LookupKconfigCommand constructs and resolves one exact command identity.
func LookupKconfigCommand(
	profile GraphProfile,
	kind string,
	command string,
	environment map[string]string,
	inputs map[string]string,
) (KconfigCommand, error) {
	request, err := NewKconfigCommandIdentity(kind, command, environment, inputs)
	if err != nil {
		return KconfigCommand{}, err
	}
	return ResolveKconfigCommand(profile, request)
}

func ResolveKbuildGraphProbe(
	profile GraphProfile,
	request KbuildGraphProbeIdentity,
) (KbuildGraphProbe, error) {
	if err := ValidateGraphProfile(profile); err != nil {
		return KbuildGraphProbe{}, err
	}
	if err := validateKbuildGraphProbeIdentity(
		request,
		"requested Kbuild graph probe",
	); err != nil {
		return KbuildGraphProbe{}, err
	}
	index, found := slices.BinarySearchFunc(
		profile.KbuildGraphProbes,
		request.ID,
		func(probe KbuildGraphProbe, id string) int {
			return strings.Compare(probe.ID, id)
		},
	)
	if !found {
		return KbuildGraphProbe{}, &MissingGraphProfileEntryError{
			Entry: GraphProfileEntryKbuildGraphProbe,
			ID:    request.ID,
		}
	}
	probe := profile.KbuildGraphProbes[index]
	if err := compareKbuildGraphProbeIdentities(probe.Identity(), request); err != nil {
		return KbuildGraphProbe{}, fmt.Errorf(
			"Kbuild graph probe %s semantic mismatch: %w",
			request.ID,
			err,
		)
	}
	return cloneKbuildGraphProbe(probe), nil
}

func LookupKbuildGraphProbe(
	profile GraphProfile,
	kind string,
	language string,
	contextArgv []string,
	candidateArgv []string,
	inputs map[string]string,
) (KbuildGraphProbe, error) {
	request, err := NewKbuildGraphProbeIdentity(
		kind,
		language,
		contextArgv,
		candidateArgv,
		inputs,
	)
	if err != nil {
		return KbuildGraphProbe{}, err
	}
	return ResolveKbuildGraphProbe(profile, request)
}

func NewGraphProfileResolver(profile GraphProfile) (*GraphProfileResolver, error) {
	if err := ValidateGraphProfile(profile); err != nil {
		return nil, err
	}
	profile = normalizeGraphProfile(profile)
	commandsByID := make(map[string]KconfigCommand, len(profile.KconfigCommands))
	for _, command := range profile.KconfigCommands {
		commandsByID[command.ID] = command
	}
	probesByID := make(map[string]KbuildGraphProbe, len(profile.KbuildGraphProbes))
	for _, probe := range profile.KbuildGraphProbes {
		probesByID[probe.ID] = probe
	}
	return &GraphProfileResolver{
		profile:        profile,
		commandsByID:   commandsByID,
		probesByID:     probesByID,
		consumed:       map[string]KconfigCommand{},
		consumedProbes: map[string]KbuildGraphProbe{},
	}, nil
}

func (resolver *GraphProfileResolver) Lookup(
	kind string,
	command string,
	environment map[string]string,
	inputs map[string]string,
) (KconfigCommand, error) {
	if resolver == nil {
		return KconfigCommand{}, fmt.Errorf("graph profile resolver is nil")
	}
	request, err := NewKconfigCommandIdentity(
		kind,
		command,
		environment,
		inputs,
	)
	if err != nil {
		return KconfigCommand{}, err
	}
	resolved, ok := resolver.commandsByID[request.ID]
	if !ok {
		return KconfigCommand{}, &MissingGraphProfileEntryError{
			Entry: GraphProfileEntryKconfigCommand,
			ID:    request.ID,
		}
	}
	if err := compareKconfigCommandIdentities(resolved.Identity(), request); err != nil {
		return KconfigCommand{}, fmt.Errorf(
			"kconfig command %s semantic mismatch: %w",
			request.ID,
			err,
		)
	}
	resolved = cloneKconfigCommand(resolved)
	resolver.mu.Lock()
	resolver.consumed[resolved.ID] = cloneKconfigCommand(resolved)
	resolver.mu.Unlock()
	return resolved, nil
}

func (resolver *GraphProfileResolver) LookupKbuildGraphProbe(
	kind string,
	language string,
	contextArgv []string,
	candidateArgv []string,
	inputs map[string]string,
) (bool, error) {
	if resolver == nil {
		return false, fmt.Errorf("graph profile resolver is nil")
	}
	request, err := NewKbuildGraphProbeIdentity(
		kind,
		language,
		contextArgv,
		candidateArgv,
		inputs,
	)
	if err != nil {
		return false, err
	}
	resolved, ok := resolver.probesByID[request.ID]
	if !ok {
		return false, &MissingGraphProfileEntryError{
			Entry: GraphProfileEntryKbuildGraphProbe,
			ID:    request.ID,
		}
	}
	if err := compareKbuildGraphProbeIdentities(resolved.Identity(), request); err != nil {
		return false, fmt.Errorf(
			"Kbuild graph probe %s semantic mismatch: %w",
			request.ID,
			err,
		)
	}
	resolved = cloneKbuildGraphProbe(resolved)
	resolver.mu.Lock()
	resolver.consumedProbes[resolved.ID] = cloneKbuildGraphProbe(resolved)
	resolver.mu.Unlock()
	return resolved.Supported, nil
}

func (resolver *GraphProfileResolver) LookupStdout(
	command string,
	environment map[string]string,
	inputs map[string]string,
) (string, error) {
	resolved, err := resolver.Lookup(
		KconfigCommandKindStdout,
		command,
		environment,
		inputs,
	)
	if err != nil {
		return "", err
	}
	return *resolved.Stdout, nil
}

func (resolver *GraphProfileResolver) LookupSuccess(
	command string,
	environment map[string]string,
	inputs map[string]string,
) (bool, error) {
	resolved, err := resolver.Lookup(
		KconfigCommandKindSuccess,
		command,
		environment,
		inputs,
	)
	if err != nil {
		return false, err
	}
	return *resolved.Success, nil
}

func (resolver *GraphProfileResolver) Projection() (GraphProjection, error) {
	if resolver == nil {
		return GraphProjection{}, fmt.Errorf("graph profile resolver is nil")
	}
	resolver.mu.Lock()
	commands := make([]KconfigCommand, 0, len(resolver.consumed))
	for _, command := range resolver.consumed {
		commands = append(commands, cloneKconfigCommand(command))
	}
	probes := make([]KbuildGraphProbe, 0, len(resolver.consumedProbes))
	for _, probe := range resolver.consumedProbes {
		probes = append(probes, cloneKbuildGraphProbe(probe))
	}
	resolver.mu.Unlock()
	projection := GraphProjection{
		Architecture:      resolver.profile.Architecture,
		DriverContract:    resolver.profile.DriverContract,
		AnalysisIdentity:  resolver.profile.AnalysisIdentity,
		KconfigCommands:   commands,
		KbuildGraphProbes: probes,
	}
	projection = normalizeGraphProjection(projection)
	if err := ValidateGraphProjection(projection); err != nil {
		return GraphProjection{}, err
	}
	return projection, nil
}

func (resolver *GraphProfileResolver) ProjectionDigest() (string, error) {
	projection, err := resolver.Projection()
	if err != nil {
		return "", err
	}
	return GraphProjectionDigest(projection)
}

// ProjectGraphProfile resolves only the requested identities. Requests may be
// presented in any order, but duplicate command IDs are rejected.
func ProjectGraphProfile(
	profile GraphProfile,
	commandRequests []KconfigCommandIdentity,
	probeRequests []KbuildGraphProbeIdentity,
) (GraphProjection, error) {
	if err := ValidateGraphProfile(profile); err != nil {
		return GraphProjection{}, err
	}
	commands := make([]KconfigCommand, 0, len(commandRequests))
	seen := make(map[string]bool, len(commandRequests))
	for index, request := range commandRequests {
		if err := validateKconfigCommandIdentity(
			request,
			fmt.Sprintf("requested kconfig_commands[%d]", index),
		); err != nil {
			return GraphProjection{}, err
		}
		if seen[request.ID] {
			return GraphProjection{}, fmt.Errorf(
				"requested kconfig_commands contains duplicate ID %s",
				request.ID,
			)
		}
		seen[request.ID] = true
		command, err := ResolveKconfigCommand(profile, request)
		if err != nil {
			return GraphProjection{}, err
		}
		commands = append(commands, command)
	}
	slices.SortFunc(commands, func(left, right KconfigCommand) int {
		return strings.Compare(left.ID, right.ID)
	})
	probes := make([]KbuildGraphProbe, 0, len(probeRequests))
	seenProbes := make(map[string]bool, len(probeRequests))
	for index, request := range probeRequests {
		if err := validateKbuildGraphProbeIdentity(
			request,
			fmt.Sprintf("requested kbuild_graph_probes[%d]", index),
		); err != nil {
			return GraphProjection{}, err
		}
		if seenProbes[request.ID] {
			return GraphProjection{}, fmt.Errorf(
				"requested kbuild_graph_probes contains duplicate ID %s",
				request.ID,
			)
		}
		seenProbes[request.ID] = true
		probe, err := ResolveKbuildGraphProbe(profile, request)
		if err != nil {
			return GraphProjection{}, err
		}
		probes = append(probes, probe)
	}
	slices.SortFunc(probes, func(left, right KbuildGraphProbe) int {
		return strings.Compare(left.ID, right.ID)
	})
	return GraphProjection{
		Architecture:      profile.Architecture,
		DriverContract:    profile.DriverContract,
		AnalysisIdentity:  profile.AnalysisIdentity,
		KconfigCommands:   commands,
		KbuildGraphProbes: probes,
	}, nil
}

func ValidateGraphProjection(projection GraphProjection) error {
	if err := validateGraphProfileIdentity(projection.Identity()); err != nil {
		return err
	}
	previousID := ""
	for index, command := range projection.KconfigCommands {
		context := fmt.Sprintf("kconfig_commands[%d]", index)
		if err := validateKconfigCommand(command, context); err != nil {
			return err
		}
		if command.ID <= previousID {
			return fmt.Errorf("kconfig_commands must be sorted by unique ID")
		}
		previousID = command.ID
	}
	previousID = ""
	for index, probe := range projection.KbuildGraphProbes {
		context := fmt.Sprintf("kbuild_graph_probes[%d]", index)
		if err := validateKbuildGraphProbe(probe, context); err != nil {
			return err
		}
		if probe.ID <= previousID {
			return fmt.Errorf("kbuild_graph_probes must be sorted by unique ID")
		}
		previousID = probe.ID
	}
	return nil
}

func CanonicalGraphProjectionJSON(projection GraphProjection) ([]byte, error) {
	projection = normalizeGraphProjection(projection)
	if err := ValidateGraphProjection(projection); err != nil {
		return nil, err
	}
	return marshalCanonical(struct {
		Schema            string             `json:"schema"`
		Architecture      string             `json:"architecture"`
		DriverContract    string             `json:"driver_contract"`
		AnalysisIdentity  AnalysisIdentity   `json:"analysis_identity"`
		KconfigCommands   []KconfigCommand   `json:"kconfig_commands"`
		KbuildGraphProbes []KbuildGraphProbe `json:"kbuild_graph_probes"`
	}{
		Schema:            GraphProjectionSchema,
		Architecture:      projection.Architecture,
		DriverContract:    projection.DriverContract,
		AnalysisIdentity:  projection.AnalysisIdentity,
		KconfigCommands:   projection.KconfigCommands,
		KbuildGraphProbes: projection.KbuildGraphProbes,
	}, "consumed graph projection")
}

func normalizeGraphProjection(projection GraphProjection) GraphProjection {
	commands := make([]KconfigCommand, len(projection.KconfigCommands))
	for index, command := range projection.KconfigCommands {
		commands[index] = cloneKconfigCommand(command)
	}
	slices.SortFunc(commands, func(left, right KconfigCommand) int {
		return strings.Compare(left.ID, right.ID)
	})
	projection.KconfigCommands = commands
	probes := make([]KbuildGraphProbe, len(projection.KbuildGraphProbes))
	for index, probe := range projection.KbuildGraphProbes {
		probes[index] = cloneKbuildGraphProbe(probe)
	}
	slices.SortFunc(probes, func(left, right KbuildGraphProbe) int {
		return strings.Compare(left.ID, right.ID)
	})
	projection.KbuildGraphProbes = probes
	return projection
}

func GraphProjectionDigest(projection GraphProjection) (string, error) {
	data, err := CanonicalGraphProjectionJSON(projection)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(GraphProjectionSchema+"\x00"), data...))
	return hex.EncodeToString(sum[:]), nil
}

func ProjectGraphProfileDigest(
	profile GraphProfile,
	commandRequests []KconfigCommandIdentity,
	probeRequests []KbuildGraphProbeIdentity,
) (GraphProjection, string, error) {
	projection, err := ProjectGraphProfile(
		profile,
		commandRequests,
		probeRequests,
	)
	if err != nil {
		return GraphProjection{}, "", err
	}
	digest, err := GraphProjectionDigest(projection)
	if err != nil {
		return GraphProjection{}, "", err
	}
	return projection, digest, nil
}

// ValidateGraphConsumption checks the selected identity and every consumed
// command or probe result against a graph-profile superset. Unconsumed profile
// entries are intentionally irrelevant.
func ValidateGraphConsumption(expected GraphProfile, actual GraphProjection) error {
	actual = normalizeGraphProjection(actual)
	if err := ValidateGraphProjection(actual); err != nil {
		return fmt.Errorf("actual consumed graph: %w", err)
	}
	if err := ValidateGraphProfileIdentity(expected, actual.Identity()); err != nil {
		return err
	}
	requests := make([]KconfigCommandIdentity, len(actual.KconfigCommands))
	for index, command := range actual.KconfigCommands {
		requests[index] = command.Identity()
	}
	probeRequests := make(
		[]KbuildGraphProbeIdentity,
		len(actual.KbuildGraphProbes),
	)
	for index, probe := range actual.KbuildGraphProbes {
		probeRequests[index] = probe.Identity()
	}
	projected, err := ProjectGraphProfile(
		expected,
		requests,
		probeRequests,
	)
	if err != nil {
		return err
	}
	for index := range projected.KconfigCommands {
		if err := compareKconfigCommands(
			projected.KconfigCommands[index],
			actual.KconfigCommands[index],
		); err != nil {
			return err
		}
	}
	for index := range projected.KbuildGraphProbes {
		if err := compareKbuildGraphProbes(
			projected.KbuildGraphProbes[index],
			actual.KbuildGraphProbes[index],
		); err != nil {
			return err
		}
	}
	return nil
}

func ValidateGraphConsumptionDigest(
	expected GraphProfile,
	actual GraphProjection,
) (string, error) {
	actual = normalizeGraphProjection(actual)
	if err := ValidateGraphConsumption(expected, actual); err != nil {
		return "", err
	}
	return GraphProjectionDigest(actual)
}

func compareKconfigCommands(expected, actual KconfigCommand) error {
	if err := compareKconfigCommandIdentities(expected.Identity(), actual.Identity()); err != nil {
		return fmt.Errorf("kconfig command identity mismatch: %w", err)
	}
	switch expected.Kind {
	case KconfigCommandKindStdout:
		if actual.Stdout == nil {
			return fmt.Errorf("kconfig command %s stdout result is missing", expected.ID)
		}
		if *expected.Stdout != *actual.Stdout {
			return fmt.Errorf(
				"kconfig command %s stdout mismatch: expected %q, got %q",
				expected.ID,
				*expected.Stdout,
				*actual.Stdout,
			)
		}
	case KconfigCommandKindSuccess:
		if actual.Success == nil {
			return fmt.Errorf("kconfig command %s success result is missing", expected.ID)
		}
		if *expected.Success != *actual.Success {
			return fmt.Errorf(
				"kconfig command %s success mismatch: expected %t, got %t",
				expected.ID,
				*expected.Success,
				*actual.Success,
			)
		}
	}
	return nil
}

func compareKconfigCommandIdentities(expected, actual KconfigCommandIdentity) error {
	if expected.ID != actual.ID {
		return fmt.Errorf("ID: expected %q, got %q", expected.ID, actual.ID)
	}
	if expected.Kind != actual.Kind {
		return fmt.Errorf("kind: expected %q, got %q", expected.Kind, actual.Kind)
	}
	if expected.Command != actual.Command {
		return fmt.Errorf("command: expected %q, got %q", expected.Command, actual.Command)
	}
	if !maps.Equal(expected.Environment, actual.Environment) {
		return fmt.Errorf(
			"environment: expected %#v, got %#v",
			expected.Environment,
			actual.Environment,
		)
	}
	if !maps.Equal(expected.Inputs, actual.Inputs) {
		return fmt.Errorf("inputs: expected %#v, got %#v", expected.Inputs, actual.Inputs)
	}
	return nil
}

func compareKbuildGraphProbes(expected, actual KbuildGraphProbe) error {
	if err := compareKbuildGraphProbeIdentities(
		expected.Identity(),
		actual.Identity(),
	); err != nil {
		return fmt.Errorf("Kbuild graph probe identity mismatch: %w", err)
	}
	if expected.Supported != actual.Supported {
		return fmt.Errorf(
			"Kbuild graph probe %s supported mismatch: expected %t, got %t",
			expected.ID,
			expected.Supported,
			actual.Supported,
		)
	}
	return nil
}

func compareKbuildGraphProbeIdentities(
	expected KbuildGraphProbeIdentity,
	actual KbuildGraphProbeIdentity,
) error {
	if expected.ID != actual.ID {
		return fmt.Errorf("ID: expected %q, got %q", expected.ID, actual.ID)
	}
	if expected.Kind != actual.Kind {
		return fmt.Errorf("kind: expected %q, got %q", expected.Kind, actual.Kind)
	}
	if expected.Language != actual.Language {
		return fmt.Errorf(
			"language: expected %q, got %q",
			expected.Language,
			actual.Language,
		)
	}
	if !slices.Equal(expected.ContextArgv, actual.ContextArgv) {
		return fmt.Errorf(
			"context_argv: expected %#v, got %#v",
			expected.ContextArgv,
			actual.ContextArgv,
		)
	}
	if !slices.Equal(expected.CandidateArgv, actual.CandidateArgv) {
		return fmt.Errorf(
			"candidate_argv: expected %#v, got %#v",
			expected.CandidateArgv,
			actual.CandidateArgv,
		)
	}
	if !maps.Equal(expected.Inputs, actual.Inputs) {
		return fmt.Errorf("inputs: expected %#v, got %#v", expected.Inputs, actual.Inputs)
	}
	return nil
}
