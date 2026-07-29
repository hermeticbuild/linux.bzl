package ccprofile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

type profileWire struct {
	Schema           string                `json:"schema"`
	Architecture     string                `json:"architecture"`
	DriverContract   string                `json:"driver_contract"`
	AnalysisIdentity analysisIdentityWire  `json:"analysis_identity"`
	KconfigIdentity  kconfigIdentityWire   `json:"kconfig_identity"`
	StructuralProbes []structuralProbeWire `json:"structural_probes"`
}

type analysisIdentityWire struct {
	Compiler            string `json:"compiler"`
	TargetGNUSystemName string `json:"target_gnu_system_name"`
}

type kconfigIdentityWire struct {
	CCName        string            `json:"cc_name"`
	CCVersion     *int              `json:"cc_version"`
	CCVersionText string            `json:"cc_version_text"`
	ASName        string            `json:"as_name"`
	ASVersion     *int              `json:"as_version"`
	LDName        string            `json:"ld_name"`
	LDVersion     *int              `json:"ld_version"`
	CanLink       *bool             `json:"can_link"`
	BuiltinMacros map[string]string `json:"builtin_macros"`
}

type structuralProbeWire struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Language   string   `json:"language"`
	PrefixArgv []string `json:"prefix_argv"`
	Argv       []string `json:"argv"`
	Supported  *bool    `json:"supported"`
}

func Decode(data []byte) (Profile, error) {
	if err := rejectDuplicateKeys(data); err != nil {
		return Profile{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var wire profileWire
	if err := decoder.Decode(&wire); err != nil {
		return Profile{}, fmt.Errorf("decode CC profile: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Profile{}, err
	}
	if wire.KconfigIdentity.CCVersion == nil {
		return Profile{}, fmt.Errorf("kconfig_identity.cc_version is required")
	}
	if wire.KconfigIdentity.ASVersion == nil {
		return Profile{}, fmt.Errorf("kconfig_identity.as_version is required")
	}
	if wire.KconfigIdentity.LDVersion == nil {
		return Profile{}, fmt.Errorf("kconfig_identity.ld_version is required")
	}
	if wire.KconfigIdentity.CanLink == nil {
		return Profile{}, fmt.Errorf("kconfig_identity.can_link is required")
	}
	if wire.KconfigIdentity.BuiltinMacros == nil {
		return Profile{}, fmt.Errorf("kconfig_identity.builtin_macros is required")
	}
	if wire.StructuralProbes == nil {
		return Profile{}, fmt.Errorf("structural_probes is required")
	}
	profile := Profile{
		Schema:         wire.Schema,
		Architecture:   wire.Architecture,
		DriverContract: wire.DriverContract,
		AnalysisIdentity: AnalysisIdentity{
			Compiler:            wire.AnalysisIdentity.Compiler,
			TargetGNUSystemName: wire.AnalysisIdentity.TargetGNUSystemName,
		},
		KconfigIdentity: KconfigIdentity{
			CCName:        wire.KconfigIdentity.CCName,
			CCVersion:     *wire.KconfigIdentity.CCVersion,
			CCVersionText: wire.KconfigIdentity.CCVersionText,
			ASName:        wire.KconfigIdentity.ASName,
			ASVersion:     *wire.KconfigIdentity.ASVersion,
			LDName:        wire.KconfigIdentity.LDName,
			LDVersion:     *wire.KconfigIdentity.LDVersion,
			CanLink:       *wire.KconfigIdentity.CanLink,
			BuiltinMacros: wire.KconfigIdentity.BuiltinMacros,
		},
		StructuralProbes: make([]StructuralProbe, len(wire.StructuralProbes)),
	}
	for index, probe := range wire.StructuralProbes {
		if probe.Supported == nil {
			return Profile{}, fmt.Errorf("structural_probes[%d].supported is required", index)
		}
		if probe.PrefixArgv == nil {
			return Profile{}, fmt.Errorf("structural_probes[%d].prefix_argv is required", index)
		}
		if probe.Argv == nil {
			return Profile{}, fmt.Errorf("structural_probes[%d].argv is required", index)
		}
		profile.StructuralProbes[index] = StructuralProbe{
			ID:         probe.ID,
			Kind:       probe.Kind,
			Language:   probe.Language,
			PrefixArgv: probe.PrefixArgv,
			Argv:       probe.Argv,
			Supported:  *probe.Supported,
		}
	}
	if err := Validate(profile); err != nil {
		return Profile{}, fmt.Errorf("validate CC profile: %w", err)
	}
	return profile, nil
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := consumeJSONValue(decoder, "$"); err != nil {
		return fmt.Errorf("decode CC profile: %w", err)
	}
	return requireJSONEOF(decoder)
}

func consumeJSONValue(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("%s contains a non-string object key", path)
			}
			if seen[key] {
				return fmt.Errorf("%s contains duplicate key %q", path, key)
			}
			seen[key] = true
			if err := consumeJSONValue(decoder, path+"."+key); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("%s object has invalid terminator", path)
		}
	case '[':
		index := 0
		for decoder.More() {
			if err := consumeJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
			index++
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("%s array has invalid terminator", path)
		}
	default:
		return fmt.Errorf("%s has unexpected delimiter %q", path, delimiter)
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode CC profile: trailing JSON value")
		}
		return fmt.Errorf("decode CC profile: %w", err)
	}
	return nil
}
