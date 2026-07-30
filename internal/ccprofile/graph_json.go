package ccprofile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
)

type graphProfileWire struct {
	Schema            string                 `json:"schema"`
	Architecture      string                 `json:"architecture"`
	DriverContract    string                 `json:"driver_contract"`
	AnalysisIdentity  AnalysisIdentity       `json:"analysis_identity"`
	KconfigCommands   []kconfigCommandWire   `json:"kconfig_commands"`
	KbuildGraphProbes []kbuildGraphProbeWire `json:"kbuild_graph_probes"`
}

type graphProjectionWire struct {
	Schema            string                 `json:"schema"`
	Architecture      string                 `json:"architecture"`
	DriverContract    string                 `json:"driver_contract"`
	AnalysisIdentity  AnalysisIdentity       `json:"analysis_identity"`
	KconfigCommands   []kconfigCommandWire   `json:"kconfig_commands"`
	KbuildGraphProbes []kbuildGraphProbeWire `json:"kbuild_graph_probes"`
}

type kconfigCommandWire struct {
	ID          string            `json:"id"`
	Kind        string            `json:"kind"`
	Command     string            `json:"command"`
	Environment map[string]string `json:"environment"`
	Inputs      map[string]string `json:"inputs"`
	Stdout      *string           `json:"stdout,omitempty"`
	Success     *bool             `json:"success,omitempty"`
}

type kbuildGraphProbeWire struct {
	ID            string            `json:"id"`
	Kind          string            `json:"kind"`
	Language      string            `json:"language"`
	ContextArgv   []string          `json:"context_argv"`
	CandidateArgv []string          `json:"candidate_argv"`
	Inputs        map[string]string `json:"inputs"`
	Supported     *bool             `json:"supported"`
}

func DecodeGraphProfile(data []byte) (GraphProfile, error) {
	if err := rejectDuplicateKeysNamed(data, "toolchain graph profile"); err != nil {
		return GraphProfile{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var wire graphProfileWire
	if err := decoder.Decode(&wire); err != nil {
		return GraphProfile{}, fmt.Errorf("decode toolchain graph profile: %w", err)
	}
	if err := requireJSONEOFNamed(decoder, "toolchain graph profile"); err != nil {
		return GraphProfile{}, err
	}
	if wire.KconfigCommands == nil {
		return GraphProfile{}, fmt.Errorf("toolchain graph profile kconfig_commands is required")
	}
	if wire.KbuildGraphProbes == nil {
		return GraphProfile{}, fmt.Errorf("toolchain graph profile kbuild_graph_probes is required")
	}
	probes, err := probesFromWire(wire.KbuildGraphProbes)
	if err != nil {
		return GraphProfile{}, err
	}
	profile := GraphProfile{
		Schema:            wire.Schema,
		Architecture:      wire.Architecture,
		DriverContract:    wire.DriverContract,
		AnalysisIdentity:  wire.AnalysisIdentity,
		KconfigCommands:   commandsFromWire(wire.KconfigCommands),
		KbuildGraphProbes: probes,
	}
	if err := ValidateGraphProfile(profile); err != nil {
		return GraphProfile{}, fmt.Errorf("validate toolchain graph profile: %w", err)
	}
	return profile, nil
}

func DecodeGraphProjection(data []byte) (GraphProjection, error) {
	if err := rejectDuplicateKeysNamed(data, "consumed graph projection"); err != nil {
		return GraphProjection{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var wire graphProjectionWire
	if err := decoder.Decode(&wire); err != nil {
		return GraphProjection{}, fmt.Errorf("decode consumed graph projection: %w", err)
	}
	if err := requireJSONEOFNamed(decoder, "consumed graph projection"); err != nil {
		return GraphProjection{}, err
	}
	if wire.Schema != GraphProjectionSchema {
		return GraphProjection{}, fmt.Errorf(
			"consumed graph projection schema %q, want %q",
			wire.Schema,
			GraphProjectionSchema,
		)
	}
	if wire.KconfigCommands == nil {
		return GraphProjection{}, fmt.Errorf("consumed graph projection kconfig_commands is required")
	}
	if wire.KbuildGraphProbes == nil {
		return GraphProjection{}, fmt.Errorf("consumed graph projection kbuild_graph_probes is required")
	}
	probes, err := probesFromWire(wire.KbuildGraphProbes)
	if err != nil {
		return GraphProjection{}, err
	}
	projection := GraphProjection{
		Architecture:      wire.Architecture,
		DriverContract:    wire.DriverContract,
		AnalysisIdentity:  wire.AnalysisIdentity,
		KconfigCommands:   commandsFromWire(wire.KconfigCommands),
		KbuildGraphProbes: probes,
	}
	if err := ValidateGraphProjection(projection); err != nil {
		return GraphProjection{}, fmt.Errorf("validate consumed graph projection: %w", err)
	}
	return projection, nil
}

func commandsFromWire(wire []kconfigCommandWire) []KconfigCommand {
	commands := make([]KconfigCommand, len(wire))
	for index, command := range wire {
		environment := cloneStringMap(command.Environment)
		inputs := cloneStringMap(command.Inputs)
		commands[index] = KconfigCommand{
			ID:          command.ID,
			Kind:        command.Kind,
			Command:     command.Command,
			Environment: environment,
			Inputs:      inputs,
			Stdout:      command.Stdout,
			Success:     command.Success,
		}
	}
	return commands
}

func probesFromWire(wire []kbuildGraphProbeWire) ([]KbuildGraphProbe, error) {
	probes := make([]KbuildGraphProbe, len(wire))
	for index, probe := range wire {
		if probe.ContextArgv == nil {
			return nil, fmt.Errorf(
				"kbuild_graph_probes[%d].context_argv is required",
				index,
			)
		}
		if probe.CandidateArgv == nil {
			return nil, fmt.Errorf(
				"kbuild_graph_probes[%d].candidate_argv is required",
				index,
			)
		}
		if probe.Supported == nil {
			return nil, fmt.Errorf(
				"kbuild_graph_probes[%d].supported is required",
				index,
			)
		}
		probes[index] = KbuildGraphProbe{
			ID:            probe.ID,
			Kind:          probe.Kind,
			Language:      probe.Language,
			ContextArgv:   slices.Clone(probe.ContextArgv),
			CandidateArgv: slices.Clone(probe.CandidateArgv),
			Inputs:        cloneStringMap(probe.Inputs),
			Supported:     *probe.Supported,
		}
	}
	return probes, nil
}
