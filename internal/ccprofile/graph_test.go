package ccprofile

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func graphString(value string) *string {
	return &value
}

func graphBool(value bool) *bool {
	return &value
}

func testKconfigCommand(
	t *testing.T,
	kind string,
	commandText string,
	result any,
) KconfigCommand {
	t.Helper()
	command := KconfigCommand{
		Kind:        kind,
		Command:     commandText,
		Environment: map[string]string{"LC_ALL": "C"},
		Inputs: map[string]string{
			"include/config/auto.conf": strings.Repeat("a", 64),
		},
	}
	switch value := result.(type) {
	case string:
		command.Stdout = graphString(value)
	case bool:
		command.Success = graphBool(value)
	default:
		t.Fatalf("unsupported test command result %T", result)
	}
	command.ID = KconfigCommandID(command)
	return command
}

func testKbuildGraphProbe(
	t *testing.T,
	kind string,
	contextArgv []string,
	candidateArgv []string,
	supported bool,
) KbuildGraphProbe {
	t.Helper()
	language := map[string]string{
		"as_option": "asm",
		"cc_option": "c",
		"ld_option": "link",
	}[kind]
	probe := KbuildGraphProbe{
		Kind:          kind,
		Language:      language,
		ContextArgv:   slices.Clone(contextArgv),
		CandidateArgv: slices.Clone(candidateArgv),
		Inputs: map[string]string{
			"include/linux/compiler_types.h": strings.Repeat("c", 64),
		},
		Supported: supported,
	}
	probe.ID = KbuildGraphProbeID(probe)
	return probe
}

func testGraphProfile(t *testing.T, commands ...KconfigCommand) GraphProfile {
	t.Helper()
	profile := GraphProfile{
		Schema:         GraphProfileSchema,
		Architecture:   "x86_64",
		DriverContract: DriverContract,
		AnalysisIdentity: AnalysisIdentity{
			Compiler:            "clang",
			TargetGNUSystemName: "x86_64-unknown-linux-gnu",
		},
		KconfigCommands: commands,
	}
	data, err := CanonicalGraphProfileJSON(profile)
	if err != nil {
		t.Fatalf("CanonicalGraphProfileJSON() failed: %v", err)
	}
	profile, err = DecodeGraphProfile(data)
	if err != nil {
		t.Fatalf("DecodeGraphProfile() failed: %v", err)
	}
	return profile
}

func testGraphProfileWithProbes(
	t *testing.T,
	commands []KconfigCommand,
	probes []KbuildGraphProbe,
) GraphProfile {
	t.Helper()
	profile := GraphProfile{
		Schema:         GraphProfileSchema,
		Architecture:   "x86_64",
		DriverContract: DriverContract,
		AnalysisIdentity: AnalysisIdentity{
			Compiler:            "clang",
			TargetGNUSystemName: "x86_64-unknown-linux-gnu",
		},
		KconfigCommands:   commands,
		KbuildGraphProbes: probes,
	}
	data, err := CanonicalGraphProfileJSON(profile)
	if err != nil {
		t.Fatalf("CanonicalGraphProfileJSON() failed: %v", err)
	}
	profile, err = DecodeGraphProfile(data)
	if err != nil {
		t.Fatalf("DecodeGraphProfile() failed: %v", err)
	}
	return profile
}

func TestKconfigCommandIDUsesOnlyExactIdentity(t *testing.T) {
	command := testKconfigCommand(t, KconfigCommandKindStdout, "printf '%s' value", "value")
	otherResult := cloneKconfigCommand(command)
	otherResult.Stdout = graphString("different")
	if got, want := KconfigCommandID(otherResult), command.ID; got != want {
		t.Fatalf("result changed command ID: got %s, want %s", got, want)
	}

	otherEnvironment := cloneKconfigCommand(command)
	otherEnvironment.Environment["LC_ALL"] = "POSIX"
	if got := KconfigCommandID(otherEnvironment); got == command.ID {
		t.Fatal("environment did not change command ID")
	}

	otherInput := cloneKconfigCommand(command)
	otherInput.Inputs["include/config/auto.conf"] = strings.Repeat("b", 64)
	if got := KconfigCommandID(otherInput); got == command.ID {
		t.Fatal("input digest did not change command ID")
	}
}

func TestKbuildGraphProbeIDUsesExactIdentityAndInputs(t *testing.T) {
	probe := testKbuildGraphProbe(
		t,
		"cc_option",
		[]string{"-Werror", "-include", GraphProfileSourceRoot + "/include/linux/compiler_types.h"},
		[]string{"-fno-pic"},
		true,
	)
	otherResult := cloneKbuildGraphProbe(probe)
	otherResult.Supported = false
	if got, want := KbuildGraphProbeID(otherResult), probe.ID; got != want {
		t.Fatalf("result changed probe ID: got %s, want %s", got, want)
	}
	otherContext := cloneKbuildGraphProbe(probe)
	otherContext.ContextArgv = append(otherContext.ContextArgv, "-DCHANGED")
	if got := KbuildGraphProbeID(otherContext); got == probe.ID {
		t.Fatal("context argv did not change probe ID")
	}
	otherInput := cloneKbuildGraphProbe(probe)
	otherInput.Inputs["include/linux/compiler_types.h"] = strings.Repeat("d", 64)
	if got := KbuildGraphProbeID(otherInput); got == probe.ID {
		t.Fatal("input digest did not change probe ID")
	}
}

func TestGraphProfileCanonicalJSONSortsAndNormalizes(t *testing.T) {
	stdout := testKconfigCommand(t, KconfigCommandKindStdout, "printf output", "")
	success := KconfigCommand{
		Kind:    KconfigCommandKindSuccess,
		Command: "test -f include/generated/autoconf.h",
		Success: graphBool(false),
	}
	success.ID = KconfigCommandID(success)
	profile := GraphProfile{
		Schema:         GraphProfileSchema,
		Architecture:   "x86_64",
		DriverContract: DriverContract,
		AnalysisIdentity: AnalysisIdentity{
			Compiler:            "clang",
			TargetGNUSystemName: "x86_64-unknown-linux-gnu",
		},
		KconfigCommands: []KconfigCommand{stdout, success},
	}
	data, err := CanonicalGraphProfileJSON(profile)
	if err != nil {
		t.Fatalf("CanonicalGraphProfileJSON() failed: %v", err)
	}
	decoded, err := DecodeGraphProfile(data)
	if err != nil {
		t.Fatalf("DecodeGraphProfile() failed: %v", err)
	}
	if decoded.KconfigCommands[0].ID >= decoded.KconfigCommands[1].ID {
		t.Fatalf("commands are not sorted: %#v", decoded.KconfigCommands)
	}
	if decoded.KconfigCommands[0].Environment == nil ||
		decoded.KconfigCommands[0].Inputs == nil {
		t.Fatal("omitted maps were not normalized")
	}
	if !strings.Contains(string(data), "\"environment\": {}") ||
		!strings.Contains(string(data), "\"inputs\": {}") {
		t.Fatalf("canonical JSON did not encode empty maps:\n%s", data)
	}
	if !strings.Contains(string(data), "\"kbuild_graph_probes\": []") {
		t.Fatalf("canonical JSON did not encode empty Kbuild probe section:\n%s", data)
	}
	if strings.Contains(string(data), "\"stdout\": \"\"") &&
		strings.Contains(string(data), "\"success\": false") {
		// Both zero-valued result fields must remain present on their respective
		// records. This condition deliberately documents that they survived.
		return
	}
	t.Fatalf("canonical JSON dropped a zero-valued result:\n%s", data)
}

func TestDecodeGraphProfileAcceptsOmittedCommandMaps(t *testing.T) {
	command := KconfigCommand{
		Kind:    KconfigCommandKindSuccess,
		Command: "true",
		Success: graphBool(true),
	}
	command.ID = KconfigCommandID(command)
	data := []byte(`{
  "schema": "linux.bzl/toolchain-graph-profile-v1",
  "architecture": "aarch64",
  "driver_contract": "gnu-cc-response-v1",
  "analysis_identity": {
    "compiler": "clang",
    "target_gnu_system_name": "aarch64-unknown-linux-gnu"
  },
  "kbuild_graph_probes": [],
  "kconfig_commands": [{
    "id": "` + command.ID + `",
    "kind": "success",
    "command": "true",
    "success": true
  }]
}
`)
	profile, err := DecodeGraphProfile(data)
	if err != nil {
		t.Fatalf("DecodeGraphProfile() failed: %v", err)
	}
	canonical, err := CanonicalGraphProfileJSON(profile)
	if err != nil {
		t.Fatalf("CanonicalGraphProfileJSON() failed: %v", err)
	}
	if !strings.Contains(string(canonical), "\"environment\": {}") ||
		!strings.Contains(string(canonical), "\"inputs\": {}") {
		t.Fatalf("canonical JSON did not normalize omitted maps:\n%s", canonical)
	}
}

func TestDecodeGraphProfileRejectsNoncanonicalOrAmbiguousData(t *testing.T) {
	validCommand := testKconfigCommand(t, KconfigCommandKindSuccess, "true", true)
	valid := testGraphProfile(t, validCommand)
	data, err := CanonicalGraphProfileJSON(valid)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "missing Kbuild graph probes",
			data: func() []byte {
				var object map[string]any
				if err := json.Unmarshal(data, &object); err != nil {
					t.Fatal(err)
				}
				delete(object, "kbuild_graph_probes")
				out, err := json.Marshal(object)
				if err != nil {
					t.Fatal(err)
				}
				return out
			}(),
			want: "kbuild_graph_probes is required",
		},
		{
			name: "unknown field",
			data: []byte(strings.Replace(
				string(data),
				"\"schema\":",
				"\"unknown\": true,\n  \"schema\":",
				1,
			)),
			want: "unknown field",
		},
		{
			name: "duplicate field",
			data: []byte(strings.Replace(
				string(data),
				"\"architecture\": \"x86_64\",",
				"\"architecture\": \"x86_64\",\n  \"architecture\": \"x86_64\",",
				1,
			)),
			want: "duplicate key",
		},
		{
			name: "both results",
			data: []byte(strings.Replace(
				string(data),
				"\"success\": true",
				"\"stdout\": \"yes\",\n      \"success\": true",
				1,
			)),
			want: "stdout result is forbidden",
		},
		{
			name: "bad input digest",
			data: func() []byte {
				var object map[string]any
				if err := json.Unmarshal(data, &object); err != nil {
					t.Fatal(err)
				}
				commands := object["kconfig_commands"].([]any)
				command := commands[0].(map[string]any)
				command["inputs"] = map[string]any{"Kconfig": strings.Repeat("A", 64)}
				command["id"] = KconfigCommandIdentityID(KconfigCommandIdentity{
					Kind:    KconfigCommandKindSuccess,
					Command: "true",
					Inputs:  map[string]string{"Kconfig": strings.Repeat("A", 64)},
				})
				out, err := json.Marshal(object)
				if err != nil {
					t.Fatal(err)
				}
				return out
			}(),
			want: "lowercase SHA-256",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeGraphProfile(test.data)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodeGraphProfile() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestProjectGraphProfileAllowsSupersetAndPinsResults(t *testing.T) {
	used := testKconfigCommand(t, KconfigCommandKindStdout, "printf used", "used\n")
	extra := testKconfigCommand(t, KconfigCommandKindSuccess, "test -e extra", false)
	profile := testGraphProfile(t, extra, used)

	projection, digest, err := ProjectGraphProfileDigest(
		profile,
		[]KconfigCommandIdentity{used.Identity()},
		nil,
	)
	if err != nil {
		t.Fatalf("ProjectGraphProfileDigest() failed: %v", err)
	}
	if got, want := len(projection.KconfigCommands), 1; got != want {
		t.Fatalf("projection command count = %d, want %d", got, want)
	}
	if projection.KconfigCommands[0].ID != used.ID {
		t.Fatalf("projection selected %s, want %s", projection.KconfigCommands[0].ID, used.ID)
	}

	changedExtra := cloneKconfigCommand(extra)
	changedExtra.Success = graphBool(true)
	changedProfile := testGraphProfile(t, changedExtra, used)
	_, unchangedDigest, err := ProjectGraphProfileDigest(
		changedProfile,
		[]KconfigCommandIdentity{used.Identity()},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if unchangedDigest != digest {
		t.Fatalf("unconsumed result changed projection digest: %s != %s", unchangedDigest, digest)
	}

	changedUsed := cloneKconfigCommand(used)
	changedUsed.Stdout = graphString("changed\n")
	changedProfile = testGraphProfile(t, extra, changedUsed)
	_, changedDigest, err := ProjectGraphProfileDigest(
		changedProfile,
		[]KconfigCommandIdentity{used.Identity()},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if changedDigest == digest {
		t.Fatal("consumed result did not change projection digest")
	}

	missing := used.Identity()
	missing.Command = "printf missing"
	missing.ID = KconfigCommandIdentityID(missing)
	if _, err := ProjectGraphProfile(profile, []KconfigCommandIdentity{missing}, nil); err == nil ||
		!strings.Contains(err.Error(), "missing from graph profile") ||
		!IsMissingGraphProfileEntry(err) {
		t.Fatalf("missing projection error = %v", err)
	} else {
		var missingEntry *MissingGraphProfileEntryError
		if !errors.As(err, &missingEntry) ||
			missingEntry.Entry != GraphProfileEntryKconfigCommand ||
			missingEntry.ID != missing.ID {
			t.Fatalf("missing projection error type = %#v", missingEntry)
		}
	}
}

func TestKbuildGraphProbeProjectionAllowsSupersetAndPinsResult(t *testing.T) {
	used := testKbuildGraphProbe(
		t,
		"cc_option",
		[]string{"-Werror"},
		[]string{"-fno-pic"},
		true,
	)
	extra := testKbuildGraphProbe(
		t,
		"as_option",
		[]string{"-D__ASSEMBLY__"},
		[]string{"-Wa,--fatal-warnings"},
		false,
	)
	profile := testGraphProfileWithProbes(t, nil, []KbuildGraphProbe{extra, used})
	projection, digest, err := ProjectGraphProfileDigest(
		profile,
		nil,
		[]KbuildGraphProbeIdentity{used.Identity()},
	)
	if err != nil {
		t.Fatalf("ProjectGraphProfileDigest() failed: %v", err)
	}
	if got, want := len(projection.KbuildGraphProbes), 1; got != want {
		t.Fatalf("projection probe count = %d, want %d", got, want)
	}
	if projection.KbuildGraphProbes[0].ID != used.ID {
		t.Fatalf(
			"projection selected %s, want %s",
			projection.KbuildGraphProbes[0].ID,
			used.ID,
		)
	}

	changedExtra := cloneKbuildGraphProbe(extra)
	changedExtra.Supported = true
	changedProfile := testGraphProfileWithProbes(
		t,
		nil,
		[]KbuildGraphProbe{changedExtra, used},
	)
	_, unchangedDigest, err := ProjectGraphProfileDigest(
		changedProfile,
		nil,
		[]KbuildGraphProbeIdentity{used.Identity()},
	)
	if err != nil {
		t.Fatal(err)
	}
	if unchangedDigest != digest {
		t.Fatalf(
			"unconsumed probe changed projection digest: %s != %s",
			unchangedDigest,
			digest,
		)
	}

	changedUsed := cloneKbuildGraphProbe(used)
	changedUsed.Supported = false
	changedProfile = testGraphProfileWithProbes(
		t,
		nil,
		[]KbuildGraphProbe{extra, changedUsed},
	)
	_, changedDigest, err := ProjectGraphProfileDigest(
		changedProfile,
		nil,
		[]KbuildGraphProbeIdentity{used.Identity()},
	)
	if err != nil {
		t.Fatal(err)
	}
	if changedDigest == digest {
		t.Fatal("consumed probe result did not change projection digest")
	}

	actual := projection
	actual.KbuildGraphProbes[0].Supported = false
	err = ValidateGraphConsumption(profile, actual)
	if err == nil ||
		!strings.Contains(err.Error(), used.ID) ||
		!strings.Contains(err.Error(), "supported mismatch") {
		t.Fatalf("probe consumption error = %v", err)
	}
}

func TestMergeGraphProfilesUnionsAndRejectsConflicts(t *testing.T) {
	leftCommand := testKconfigCommand(t, KconfigCommandKindStdout, "printf left", "left")
	shared := testKconfigCommand(t, KconfigCommandKindSuccess, "test shared", true)
	rightCommand := testKconfigCommand(t, KconfigCommandKindStdout, "printf right", "right")
	left := testGraphProfile(t, shared, leftCommand)
	right := testGraphProfile(t, rightCommand, shared)

	merged, err := MergeGraphProfiles(left, right)
	if err != nil {
		t.Fatalf("MergeGraphProfiles() failed: %v", err)
	}
	if got, want := len(merged.KconfigCommands), 3; got != want {
		t.Fatalf("merged command count = %d, want %d", got, want)
	}

	conflictingShared := cloneKconfigCommand(shared)
	conflictingShared.Success = graphBool(false)
	conflicting := testGraphProfile(t, conflictingShared)
	_, err = MergeGraphProfiles(left, conflicting)
	if err == nil ||
		!strings.Contains(err.Error(), shared.ID) ||
		!strings.Contains(err.Error(), "success mismatch") {
		t.Fatalf("conflicting merge error = %v", err)
	}

	wrongIdentity := right
	wrongIdentity.Architecture = "aarch64"
	_, err = MergeGraphProfiles(left, wrongIdentity)
	if err == nil || !strings.Contains(err.Error(), "architecture mismatch") {
		t.Fatalf("identity merge error = %v", err)
	}

	probe := testKbuildGraphProbe(
		t,
		"ld_option",
		nil,
		[]string{"--build-id"},
		true,
	)
	probeProfile := testGraphProfileWithProbes(t, nil, []KbuildGraphProbe{probe})
	conflictingProbe := cloneKbuildGraphProbe(probe)
	conflictingProbe.Supported = false
	conflictingProbeProfile := testGraphProfileWithProbes(
		t,
		nil,
		[]KbuildGraphProbe{conflictingProbe},
	)
	_, err = MergeGraphProfiles(probeProfile, conflictingProbeProfile)
	if err == nil ||
		!strings.Contains(err.Error(), probe.ID) ||
		!strings.Contains(err.Error(), "supported mismatch") {
		t.Fatalf("conflicting probe merge error = %v", err)
	}
}

func TestValidateGraphProjectionCompilerIdentityUsesConsumedCompilerFacts(t *testing.T) {
	ccVersion := testKconfigCommand(
		t,
		KconfigCommandKindStdout,
		GraphProfileSourceRoot+"/scripts/cc-version.sh clang",
		"Clang 220108",
	)
	ccVersion.Environment["CC_VERSION_TEXT"] = "clang version 22.1.8"
	ccVersion.ID = KconfigCommandID(ccVersion)
	profile := testGraphProfile(t, ccVersion)
	projection, err := ProjectGraphProfile(
		profile,
		[]KconfigCommandIdentity{ccVersion.Identity()},
		nil,
	)
	if err != nil {
		t.Fatalf("ProjectGraphProfile() failed: %v", err)
	}
	identity := CompilerIdentity{
		Schema:           CompilerIdentitySchema,
		Architecture:     "x86_64",
		DriverContract:   DriverContract,
		AnalysisIdentity: profile.AnalysisIdentity,
		CCName:           "Clang",
		CCVersion:        220108,
		CCVersionText:    "clang version 22.1.8",
		BuiltinMacros:    map[string]string{},
	}
	if err := ValidateGraphProjectionCompilerIdentity(projection, identity); err != nil {
		t.Fatalf("ValidateGraphProjectionCompilerIdentity() failed: %v", err)
	}

	wrongVersion := identity
	wrongVersion.CCVersion = 180100
	wrongVersion.CCVersionText = "clang version 18.1.0"
	err = ValidateGraphProjectionCompilerIdentity(projection, wrongVersion)
	if err == nil ||
		!strings.Contains(err.Error(), "cc_version_text mismatch") {
		t.Fatalf("version mismatch error = %v", err)
	}
}

func TestValidateGraphConsumptionReportsExactResultDiff(t *testing.T) {
	expectedCommand := testKconfigCommand(
		t,
		KconfigCommandKindStdout,
		"printf expected",
		"expected\n",
	)
	profile := testGraphProfile(t, expectedCommand)
	actualCommand := cloneKconfigCommand(expectedCommand)
	actualCommand.Stdout = graphString("actual\n")
	actual := GraphProjection{
		Architecture:     profile.Architecture,
		DriverContract:   profile.DriverContract,
		AnalysisIdentity: profile.AnalysisIdentity,
		KconfigCommands:  []KconfigCommand{actualCommand},
	}
	err := ValidateGraphConsumption(profile, actual)
	if err == nil ||
		!strings.Contains(err.Error(), expectedCommand.ID) ||
		!strings.Contains(err.Error(), `expected "expected\n", got "actual\n"`) {
		t.Fatalf("ValidateGraphConsumption() error = %v", err)
	}

	actual.Architecture = "aarch64"
	err = ValidateGraphConsumption(profile, actual)
	if err == nil || !strings.Contains(err.Error(), "architecture mismatch") {
		t.Fatalf("identity mismatch error = %v", err)
	}
}

func TestGraphProfileResolverRecordsOnlyConsumedCommands(t *testing.T) {
	stdout := testKconfigCommand(t, KconfigCommandKindStdout, "printf value", "value")
	success := testKconfigCommand(t, KconfigCommandKindSuccess, "test -e value", true)
	extra := testKconfigCommand(t, KconfigCommandKindSuccess, "test -e extra", false)
	profile := testGraphProfile(t, extra, stdout, success)
	resolver, err := NewGraphProfileResolver(profile)
	if err != nil {
		t.Fatal(err)
	}
	gotStdout, err := resolver.LookupStdout(
		stdout.Command,
		stdout.Environment,
		stdout.Inputs,
	)
	if err != nil || gotStdout != *stdout.Stdout {
		t.Fatalf("LookupStdout() = %q, %v", gotStdout, err)
	}
	gotSuccess, err := resolver.LookupSuccess(
		success.Command,
		success.Environment,
		success.Inputs,
	)
	if err != nil || gotSuccess != *success.Success {
		t.Fatalf("LookupSuccess() = %t, %v", gotSuccess, err)
	}
	if _, err := resolver.LookupStdout(
		stdout.Command,
		stdout.Environment,
		stdout.Inputs,
	); err != nil {
		t.Fatal(err)
	}
	projection, err := resolver.Projection()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(projection.KconfigCommands), 2; got != want {
		t.Fatalf("consumed command count = %d, want %d", got, want)
	}
	if _, err := resolver.ProjectionDigest(); err != nil {
		t.Fatalf("ProjectionDigest() failed: %v", err)
	}
}

func TestGraphProfileResolverRecordsConsumedKbuildProbe(t *testing.T) {
	used := testKbuildGraphProbe(
		t,
		"cc_option",
		[]string{"-Werror"},
		[]string{"-fno-pic"},
		true,
	)
	extra := testKbuildGraphProbe(
		t,
		"cc_option",
		nil,
		[]string{"-fstack-protector"},
		false,
	)
	profile := testGraphProfileWithProbes(t, nil, []KbuildGraphProbe{extra, used})
	resolver, err := NewGraphProfileResolver(profile)
	if err != nil {
		t.Fatal(err)
	}
	supported, err := resolver.LookupKbuildGraphProbe(
		used.Kind,
		used.Language,
		used.ContextArgv,
		used.CandidateArgv,
		used.Inputs,
	)
	if err != nil {
		t.Fatalf("LookupKbuildGraphProbe() failed: %v", err)
	}
	if !supported {
		t.Fatal("LookupKbuildGraphProbe() returned unsupported")
	}
	projection, err := resolver.Projection()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(projection.KbuildGraphProbes), 1; got != want {
		t.Fatalf("consumed probe count = %d, want %d", got, want)
	}
	if projection.KbuildGraphProbes[0].ID != used.ID {
		t.Fatalf(
			"consumed probe ID = %s, want %s",
			projection.KbuildGraphProbes[0].ID,
			used.ID,
		)
	}
}
