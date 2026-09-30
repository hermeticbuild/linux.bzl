package kconfig

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const selectedFeatureDumpParentFixture = `
check_feat := 1
FEATURE_TESTS := libelf bpf
FEATURE_CHECK_CFLAGS-bpf = -I$(srctree)/tools/include
ifeq ($(check_feat),1)
ifeq ($(FEATURES_DUMP),)
include $(srctree)/tools/build/Makefile.feature
else
include $(FEATURES_DUMP)
endif
endif
`

const selectedFeatureDumpSourceFixture = `
feature_dir := $(srctree)/tools/build/feature
ifneq ($(OUTPUT),)
OUTPUT_FEATURES = $(OUTPUT)feature/
$(shell mkdir -p $(OUTPUT_FEATURES))
endif
feature_check = $(eval $(feature_check_code))
define feature_check_code
feature-$(1) := $(shell $(MAKE) OUTPUT=$(OUTPUT_FEATURES) CC="$(CC)" CXX="$(CXX)" CFLAGS="$(EXTRA_CFLAGS) $(FEATURE_CHECK_CFLAGS-$(1))" CXXFLAGS="$(EXTRA_CXXFLAGS) $(FEATURE_CHECK_CXXFLAGS-$(1))" LDFLAGS="$(LDFLAGS) $(FEATURE_CHECK_LDFLAGS-$(1))" -C $(feature_dir) $(OUTPUT_FEATURES)test-$1.bin >/dev/null 2>/dev/null && echo 1 || echo 0)
endef
define feature_check_display_code
feature_display := 1
endef
`

func selectedFeatureDumpFixture(t *testing.T, parent, featureSource string, overrides map[string]string) (string, CompactKbuildProfile) {
	t.Helper()
	root := t.TempDir()
	for file, value := range map[string]string{
		"tools/lib/bpf/Makefile":       parent,
		"tools/build/Makefile.feature": featureSource,
		"foreign.mk":                   "feature-libelf=1\nfeature-bpf=0\n",
		"external.mk":                  "feature-libelf=1\nfeature-bpf=0\n",
	} {
		filename := filepath.Join(root, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	vars := map[string]string{
		"srctree":       featureProbeSourceTree,
		"objtree":       featureProbeObjectTree,
		"OUTPUT":        featureProbeObjectTree + "/private/",
		"FEATURES_DUMP": featureProbeObjectTree + "/private/FEATURE-DUMP.libbpf",
		"MAKE":          CompactKbuildRecursiveMakeProvenanceToken,
		"CC":            KbuildActionRoleToken("host", "cc"),
		"CXX":           KbuildActionRoleToken("host", "cxx"),
	}
	for name, value := range overrides {
		vars[name] = value
	}
	parsed, err := ParseKbuildFileTree(filepath.Join(root, "tools/lib/bpf/Makefile"), KbuildOptions{
		RootDir: root, WorkingDir: filepath.Join(root, "tools/lib/bpf"),
		SourceRoots: map[string]string{
			featureProbeSourceTree: root,
			featureProbeObjectTree: filepath.Join(root, "unmaterialized-object-tree"),
		},
		Variables: vars, VirtualFileView: &testKbuildVirtualFileView{files: map[string]testKbuildVirtualFile{
			vars["FEATURES_DUMP"]: {content: "", exact: true},
		}},
		CaptureTargetEvaluator: true, SkipExportedVariables: true,
		MakeVariablesComplete: true, ConfigVariablesComplete: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("libbpf", "tools/lib/bpf/Makefile", root, parsed)
	if err != nil {
		t.Fatal(err)
	}
	return root, profile
}

func TestSelectedFeatureDumpUsesExactSourceCheckFlagsAndIndividualStatuses(t *testing.T) {
	root, profile := selectedFeatureDumpFixture(t, selectedFeatureDumpParentFixture, selectedFeatureDumpSourceFixture, nil)
	probes, err := DeriveSelectedFeatureDumpProbeCommands(root, profile.Path, profile, []string{"libelf", "bpf"})
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != 2 || !slices.Equal([]string{probes[0].Feature, probes[1].Feature}, []string{"libelf", "bpf"}) {
		t.Fatalf("selected individual feature probes = %#v", probes)
	}
	for _, probe := range probes {
		if probe.DumpPath != "private/FEATURE-DUMP.libbpf" {
			t.Fatalf("feature %s selected dump path = %q", probe.Feature, probe.DumpPath)
		}
		invocation, err := parseRecursiveMakeFeatureInvocation(probe.Command)
		if err != nil {
			t.Fatal(err)
		}
		if invocation.goal != "private/feature/test-"+probe.Feature+".bin" ||
			invocation.success != "1" || invocation.failure != "0" {
			t.Fatalf("feature %s must retain a measured Boolean child status: %#v", probe.Feature, invocation)
		}
		wantFlags := " "
		if probe.Feature == "bpf" {
			wantFlags = " -I" + featureProbeSourceTree + "/tools/include"
		}
		if got := invocation.variables["CFLAGS"]; got != wantFlags {
			t.Fatalf("feature %s source CFLAGS = %q; want %q", probe.Feature, got, wantFlags)
		}
	}
}

func TestSelectedFeatureDumpAbsentHookIsDistinctFromChangedHook(t *testing.T) {
	parent := "FEATURE_TESTS := libelf bpf\n"
	root, profile := selectedFeatureDumpFixture(t, parent, selectedFeatureDumpSourceFixture, nil)
	probes, err := DeriveSelectedFeatureDumpProbeCommands(root, profile.Path, profile, []string{"libelf", "bpf"})
	if err != nil || probes != nil {
		t.Fatalf("source with no FEATURES_DUMP hook returned probes %#v, error %v", probes, err)
	}
	parent = selectedFeatureDumpParentFixture + "include $(FEATURES_DUMP)\n"
	root, profile = selectedFeatureDumpFixture(t, parent, selectedFeatureDumpSourceFixture, nil)
	_, err = DeriveSelectedFeatureDumpProbeCommands(root, profile.Path, profile, []string{"libelf", "bpf"})
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("malformed FEATURES_DUMP branch error = %v", err)
	}
}

func TestSelectedFeatureDumpRequiresProfileSourceIdentity(t *testing.T) {
	root, profile := selectedFeatureDumpFixture(t, selectedFeatureDumpParentFixture, selectedFeatureDumpSourceFixture, nil)
	_, err := DeriveSelectedFeatureDumpProbeCommands(filepath.Dir(root), profile.Path, profile, []string{"libelf", "bpf"})
	if err == nil || !strings.Contains(err.Error(), "source root differs from the selected Makefile profile") {
		t.Fatalf("foreign source root error = %v", err)
	}
}

func TestMeasureSelectedFeatureDumpStatusRequiresSealedMatchingBooleanResults(t *testing.T) {
	options, command := featureChildFixture(t, featureChildMakefileFixture, "int main(void) { return 0; }\n")
	type measurement struct {
		value string
		ids   []string
	}
	discovery, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) (measurement, error) {
		value, ids, err := scopes.MeasureSelectedFeatureDumpStatus(context.Background(), command, nil)
		return measurement{value, ids}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(discovery.Value.value) || len(discovery.Value.ids) != 1 ||
		len(discovery.Plan.Nodes) != 1 || discovery.Value.ids[0] != discovery.Plan.Nodes[0].ID {
		t.Fatalf("feature status discovery = %#v, plan %#v", discovery.Value, discovery.Plan.Nodes)
	}
	plan, err := SelectProbePlanTerminals(discovery.Plan, discovery.Value.ids)
	if err != nil {
		t.Fatal(err)
	}
	node := plan.Nodes[0]
	for _, success := range []bool{true, false} {
		status, exitCode := "failure", 1
		if success {
			status, exitCode = "success", 0
		}
		oracle := &ProbeResultOracle{toolsets: plan.Toolsets, results: map[string]ProbeResult{
			node.ID: {
				Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
				Scope: node.Scope, ToolsetIdentity: plan.Toolsets[node.Scope],
				Kind: "boolean", Boolean: &success,
				Steps: []ProbeStepResult{{Name: "compiler-link", Status: status, ExitCode: exitCode}},
			},
		}}
		sealed, err := NewKbuildGraphGuardResults(plan, oracle)
		if err != nil {
			t.Fatal(err)
		}
		replay := func(command string) (measurement, error) {
			value, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) (measurement, error) {
				answer, ids, err := scopes.MeasureSelectedFeatureDumpStatus(context.Background(), command, sealed)
				return measurement{answer, ids}, err
			})
			if err != nil {
				return measurement{}, err
			}
			return value.Value, err
		}
		measured, err := replay(command)
		if err != nil {
			t.Fatal(err)
		}
		want := "0"
		if success {
			want = "1"
		}
		if measured.value != want || !slices.Equal(measured.ids, discovery.Value.ids) {
			t.Fatalf("compiler success=%t measured %#v, want %q and original terminal", success, measured, want)
		}
		changed := strings.Replace(command, `CFLAGS=" -I."`, `CFLAGS=" -I. -O3"`, 1)
		if changed == command {
			t.Fatal("test did not change the source compiler flags")
		}
		if _, err := replay(changed); err == nil || !strings.Contains(err.Error(), "differ from the sealed pregraph plan") {
			t.Fatalf("changed compiler probe must not bind a sealed result: %v", err)
		}
		delete(oracle.results, node.ID)
		if _, err := replay(command); err == nil || !strings.Contains(err.Error(), "missing result for non-pure Linux probe node") {
			t.Fatalf("missing sealed result must fail closed: %v", err)
		}
	}
}

func TestSelectedFeatureDumpRejectsChangedBranchMacroAndList(t *testing.T) {
	for _, test := range []struct {
		name, parent, source, want string
		features                   []string
		overrides                  map[string]string
	}{
		{name: "foreign include", parent: strings.Replace(selectedFeatureDumpParentFixture,
			"include $(FEATURES_DUMP)", "include $(srctree)/external.mk", 1), source: selectedFeatureDumpSourceFixture,
			features: []string{"libelf", "bpf"}, want: "alternate must include only"},
		{name: "changed macro status", parent: selectedFeatureDumpParentFixture,
			source:   strings.Replace(selectedFeatureDumpSourceFixture, "&& echo 1 || echo 0", "&& echo yes || echo no", 1),
			features: []string{"libelf", "bpf"}, want: "changed its private compiler query or Boolean status"},
		{name: "changed feature goal", parent: selectedFeatureDumpParentFixture,
			source:   strings.Replace(selectedFeatureDumpSourceFixture, "test-$1.bin", "test-other.bin", 1),
			features: []string{"libelf", "bpf"}, want: "no longer selects its source-defined per-feature query"},
		{name: "overridden feature output", parent: selectedFeatureDumpParentFixture,
			source:   selectedFeatureDumpSourceFixture + "OUTPUT_FEATURES := $(OUTPUT)foreign/\n",
			features: []string{"libelf", "bpf"}, want: "redefines its private output directory"},
		{name: "later feature code assignment", parent: selectedFeatureDumpParentFixture,
			source:   selectedFeatureDumpSourceFixture + "feature_check_code = feature-bpf := 1\n",
			features: []string{"libelf", "bpf"}, want: "redefines or undefines feature_check_code"},
		{name: "later feature code undefine", parent: selectedFeatureDumpParentFixture,
			source:   selectedFeatureDumpSourceFixture + "undefine feature_check_code\n",
			features: []string{"libelf", "bpf"}, want: "redefines or undefines feature_check_code"},
		{name: "later feature check undefine", parent: selectedFeatureDumpParentFixture,
			source:   selectedFeatureDumpSourceFixture + "override undefine feature_check\n",
			features: []string{"libelf", "bpf"}, want: "redefines or undefines feature_check"},
		{name: "later feature check definition", parent: selectedFeatureDumpParentFixture,
			source:   selectedFeatureDumpSourceFixture + "define feature_check\nfeature-bpf := 1\nendef\n",
			features: []string{"libelf", "bpf"}, want: "redefines or undefines feature_check"},
		{name: "later conditional feature check eval", parent: selectedFeatureDumpParentFixture,
			source:   selectedFeatureDumpSourceFixture + "ifeq ($(OUTPUT),)\n$(eval feature_check := feature-bpf=1)\nendif\n",
			features: []string{"libelf", "bpf"}, want: "redefines or undefines feature_check"},
		{name: "omitted selected feature", parent: selectedFeatureDumpParentFixture, source: selectedFeatureDumpSourceFixture,
			features: []string{"libelf"}, want: "differ from the source-derived feature list"},
		{name: "missing configured CC", parent: selectedFeatureDumpParentFixture, source: selectedFeatureDumpSourceFixture,
			features: []string{"libelf", "bpf"}, overrides: map[string]string{"CC": ""}, want: "configured host CC"},
		{name: "untrusted selected dump", parent: selectedFeatureDumpParentFixture, source: selectedFeatureDumpSourceFixture,
			features: []string{"libelf", "bpf"}, overrides: map[string]string{"FEATURES_DUMP": featureProbeSourceTree + "/foreign.mk"},
			want: "FEATURES_DUMP must select an object-tree file"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, profile := selectedFeatureDumpFixture(t, test.parent, test.source, test.overrides)
			_, err := DeriveSelectedFeatureDumpProbeCommands(root, profile.Path, profile, test.features)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("selected source probe error = %v, want %q", err, test.want)
			}
		})
	}
}
