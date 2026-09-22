package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func TestSelectedKbuildFeatureDumpPathIgnoresMakeComments(t *testing.T) {
	root := t.TempDir()
	const makefile = "tools/lib/bpf/Makefile"
	filename := filepath.Join(root, filepath.FromSlash(makefile))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(`# FEATURES_DUMP is mentioned in documentation only
FEATURE_VECTOR := 1 # the FEATURES_DUMP alternate is not selected
all: ; @:
`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, request := range []kbuildInvocationRequest{
		{name: "comment-only"},
		{name: "unrelated override", variables: map[string]string{"FEATURES_DUMP": "unrelated"}},
	} {
		path, selected, err := selectedKbuildFeatureDumpPath(root, makefile, request)
		if err != nil || selected || path != "" {
			t.Fatalf("Make comments became a feature alternate for %q: path %q selected %t error %v", request.name, path, selected, err)
		}
	}
}

func TestSelectedResolveBtfidsLibbpfFeatureDumpUsesSealedIndividualResults(t *testing.T) {
	root := t.TempDir()
	const makefile = "tools/lib/bpf/Makefile"
	for name, content := range map[string]string{
		"Kconfig": "mainmenu \"feature fixture\"\n",
		makefile: `
check_feat := 1
FEATURE_TESTS := libelf zlib bpf
FEATURE_CHECK_CFLAGS-bpf = -I$(srctree)/tools/include
ifeq ($(check_feat),1)
ifeq ($(FEATURES_DUMP),)
include $(srctree)/tools/build/Makefile.feature
else
include $(FEATURES_DUMP)
endif
endif
FEATURE_VECTOR := $(feature-libelf):$(feature-zlib):$(feature-bpf)
`,
		"tools/build/Makefile.feature": `
feature_dir := $(srctree)/tools/build/feature
ifneq ($(OUTPUT),)
OUTPUT_FEATURES = $(OUTPUT)feature/
$(shell mkdir -p $(OUTPUT_FEATURES))
endif
feature_check = $(eval $(feature_check_code))
define feature_check_code
feature-$(1) := $(shell $(MAKE) OUTPUT=$(OUTPUT_FEATURES) CC="$(CC)" CXX="$(CXX)" CFLAGS="$(EXTRA_CFLAGS) $(FEATURE_CHECK_CFLAGS-$(1))" CXXFLAGS="$(EXTRA_CXXFLAGS) $(FEATURE_CHECK_CXXFLAGS-$(1))" LDFLAGS="$(LDFLAGS) $(FEATURE_CHECK_LDFLAGS-$(1))" -C $(feature_dir) $(OUTPUT_FEATURES)test-$1.bin >/dev/null 2>/dev/null && echo 1 || echo 0)
endef
`,
		"tools/build/feature/Makefile": `
__BUILD = $(CC) $(CFLAGS) -MD -Wall -Werror -o $@ $(patsubst %.bin,%.c,$(@F)) $(LDFLAGS)
BUILD = $(__BUILD) > $(@:.bin=.make.output) 2>&1
$(OUTPUT)test-libelf.bin: ; $(BUILD) -lelf
$(OUTPUT)test-zlib.bin: ; $(BUILD) -lz
$(OUTPUT)test-bpf.bin: ; $(BUILD) -lbpf
`,
		"tools/build/feature/test-libelf.c": "int main(void) { return 0; }\n",
		"tools/build/feature/test-zlib.c":   "int main(void) { return 0; }\n",
		"tools/build/feature/test-bpf.c":    "int main(void) { return 0; }\n",
	} {
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const identity = "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bootstrap, err := newLinuxCompilerBootstrapPlan(identity, identity)
	if err != nil {
		t.Fatal(err)
	}
	hostFacts, err := kconfig.ParseLinuxCompilerBootstrapResult(
		testLinuxCompilerBootstrapResult(t, bootstrap.host, identity, true), "host", identity,
	)
	if err != nil {
		t.Fatal(err)
	}
	targetFacts, err := kconfig.ParseLinuxCompilerBootstrapResult(
		testLinuxCompilerBootstrapResult(t, bootstrap.target, identity, true), "target", identity,
	)
	if err != nil {
		t.Fatal(err)
	}
	options := kconfig.KbuildProbeWorkloadOptions{
		Target: kconfig.KbuildProbeScopeOptions{
			Architecture: "x86", SourceArchitecture: "x86", SourceRoot: root,
			Facts: targetFacts, Tools: map[string]string{"cc": "/configured/target/cc"},
		},
		Host: &kconfig.KbuildProbeScopeOptions{
			Architecture: "x86", SourceArchitecture: "x86", SourceRoot: root,
			Facts: hostFacts, Tools: map[string]string{"cc": "/configured/host/cc", "cxx": "/configured/host/cxx"},
		},
	}
	request := kbuildInvocationRequest{name: "resolve_btfids->libbpf", makefile: makefile}
	dumpPath, hooked, err := selectedKbuildFeatureDumpPath(root, makefile, request)
	if err != nil || !hooked {
		t.Fatalf("selected libbpf feature hook = (%q,%t,%v)", dumpPath, hooked, err)
	}
	parseSelected := func(scopes *kconfig.KbuildProbeScopes, selectedMakefile, selectedDumpPath, output, contents string) (kconfig.CompactKbuildProfile, error) {
		variables := map[string]string{
			"srctree": kbuildEvalSourceTree, "objtree": kbuildEvalObjectTree,
			"OUTPUT": output, "MAKE": kbuildEvalRecursiveMake,
		}
		base := kconfig.KbuildOptions{
			RootDir: root, WorkingDir: filepath.Join(root, filepath.FromSlash(filepath.Dir(selectedMakefile))), Variables: variables,
			CommandLineVariables: map[string]string{
				"CC":            kconfig.KbuildActionRoleToken("host", "cc"),
				"CXX":           kconfig.KbuildActionRoleToken("host", "cxx"),
				"FEATURES_DUMP": kbuildEvalObjectTree + "/" + selectedDumpPath,
			},
			VirtualFileView: kbuildFrontierVirtualFileView{
				immutableContents: map[string]string{selectedDumpPath: contents},
			},
			SourceRoots: map[string]string{
				kbuildEvalSourceTree: root,
				kbuildEvalObjectTree: filepath.Join(root, "no-physical-output"),
			},
			CaptureTargetEvaluator: true, ConfigVariablesComplete: true, MakeVariablesComplete: true,
		}
		opts, optionsErr := scopes.Options("host", base)
		if optionsErr != nil {
			return kconfig.CompactKbuildProfile{}, optionsErr
		}
		parsed, parseErr := kconfig.ParseKbuildFileTree(filepath.Join(root, filepath.FromSlash(selectedMakefile)), opts)
		if parseErr != nil {
			return kconfig.CompactKbuildProfile{}, parseErr
		}
		return kconfig.NewCompactKbuildProfile("driver:"+selectedMakefile, selectedMakefile, root, parsed)
	}
	parse := func(scopes *kconfig.KbuildProbeScopes, contents string) (kconfig.CompactKbuildProfile, error) {
		return parseSelected(scopes, makefile, dumpPath, kbuildEvalObjectTree+"/private/", contents)
	}
	type result struct {
		ids []string
	}
	discovery, err := kconfig.EvaluateKbuildProbeWorkload(options, nil, func(scopes *kconfig.KbuildProbeScopes) (result, error) {
		profile, parseErr := parse(scopes, "")
		if parseErr != nil {
			return result{}, parseErr
		}
		// A source-output round with no prior feature result stops at the
		// selected include, while the feature round must register the first
		// source-authenticated compiler status even with an empty source plan.
		sourcePass := selectedKbuildOutputProbeMeasurement(linuxKbuildProbeOptions{
			sourceOutputDiscoveryOnly: true,
		}, nil).selectedFeaturePass(scopes)
		var sourceCut *pendingKbuildFeatureDump
		if cutErr := selectedFeatureDumpSourceCut(sourcePass, makefile); !errors.As(cutErr, &sourceCut) || len(sourceCut.requestIDs) != 0 {
			return result{}, fmt.Errorf("source round must stop before measuring first feature: %v", cutErr)
		}
		featurePass := selectedKbuildOutputProbeMeasurement(linuxKbuildProbeOptions{
			featureDumpDiscoveryOnly: true,
		}, nil).selectedFeaturePass(scopes)
		if cutErr := selectedFeatureDumpSourceCut(featurePass, makefile); cutErr != nil {
			return result{}, fmt.Errorf("feature round stopped before registering compiler checks: %w", cutErr)
		}
		_, measuredErr := measureSelectedKbuildFeatureDump(root, makefile, dumpPath, profile,
			featurePass)
		var pending *pendingKbuildFeatureDump
		if !errors.As(measuredErr, &pending) {
			return result{}, measuredErr
		}
		return result{ids: pending.requestIDs}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Value.ids) != 3 || len(discovery.Plan.Nodes) != 3 {
		t.Fatalf("libbpf selected three statuses = %#v, nodes %#v", discovery.Value, discovery.Plan.Nodes)
	}
	plan, err := kconfig.SelectProbePlanTerminals(discovery.Plan, discovery.Value.ids)
	if err != nil {
		t.Fatal(err)
	}
	resultRoot := t.TempDir()
	for _, node := range plan.Nodes {
		request := plan.Requests[node.RequestID]
		success := !slices.Contains(request.Sources, "tools/build/feature/test-zlib.c")
		step := request.Steps[0]
		status, exitCode := "success", 0
		if !success {
			status, exitCode = "failure", 1
		}
		writeTestProbeResult(t, resultRoot, kconfig.ProbeResult{
			Schema: kconfig.LinuxProbeResultSchema,
			NodeID: node.ID, RequestID: node.RequestID, Scope: node.Scope,
			ToolsetIdentity: identity, Kind: "boolean", Boolean: &success,
			Steps: []kconfig.ProbeStepResult{{Name: step.Name, Status: status, ExitCode: exitCode}},
		})
	}
	oracle, err := kconfig.NewProbeResultOracleFromTrees(map[string]string{"host": resultRoot}, plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := kconfig.NewKbuildGraphGuardResults(plan, oracle)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := kconfig.EvaluateKbuildProbeWorkload(options, nil, func(scopes *kconfig.KbuildProbeScopes) (string, error) {
		profile, parseErr := parse(scopes, "")
		if parseErr != nil {
			return "", parseErr
		}
		content, measureErr := measureSelectedKbuildFeatureDump(root, makefile, dumpPath, profile,
			&kbuildSelectedFeatureDump{scopes: scopes, results: sealed})
		if measureErr != nil {
			return "", measureErr
		}
		confirmed, parseErr := parse(scopes, content)
		if parseErr != nil {
			return "", parseErr
		}
		vector, evalErr := kconfig.EvaluateCompactKbuildTextSymbolic(
			confirmed, "", "", nil, nil, nil, "$(FEATURE_VECTOR)",
		)
		if evalErr != nil {
			return "", evalErr
		}
		return content + "|" + vector, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "feature-libelf=1\nfeature-zlib=0\nfeature-bpf=1\n|1:0:1"; replay.Value != want {
		t.Fatalf("measured libbpf include = %q, want %q", replay.Value, want)
	}
	if !slices.Equal(slices.Sorted(slices.Values(plan.Terminal)), slices.Sorted(slices.Values(discovery.Value.ids))) {
		t.Fatalf("selected source terminals = %q, want %q", plan.Terminal, discovery.Value.ids)
	}
	const secondMakefile = "tools/lib/second/Makefile"
	firstSource, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(makefile)))
	if err != nil {
		t.Fatal(err)
	}
	secondSource := filepath.Join(root, filepath.FromSlash(secondMakefile))
	if err := os.MkdirAll(filepath.Dir(secondSource), 0o755); err != nil {
		t.Fatal(err)
	}
	// A second selected invocation may use the same immutable hook with
	// different source-controlled per-feature flags and a distinct OUTPUT.
	secondContents := append(slices.Clone(firstSource), []byte("\nFEATURE_CHECK_CFLAGS-libelf = -DSECOND_LIBELF\nFEATURE_CHECK_CFLAGS-zlib = -DSECOND_ZLIB\nFEATURE_CHECK_CFLAGS-bpf = -DSECOND_BPF\n")...)
	if err := os.WriteFile(secondSource, secondContents, 0o644); err != nil {
		t.Fatal(err)
	}
	secondRequest := kbuildInvocationRequest{name: "resolve_btfids->second", makefile: secondMakefile}
	secondDumpPath, hooked, err := selectedKbuildFeatureDumpPath(root, secondMakefile, secondRequest)
	if err != nil || !hooked || secondDumpPath == dumpPath {
		t.Fatalf("second selected feature hook = (%q,%t,%v)", secondDumpPath, hooked, err)
	}
	secondDiscovery, err := kconfig.EvaluateKbuildProbeWorkload(options, nil, func(scopes *kconfig.KbuildProbeScopes) (result, error) {
		first, parseErr := parse(scopes, "")
		if parseErr != nil {
			return result{}, parseErr
		}
		if _, measureErr := measureSelectedKbuildFeatureDump(root, makefile, dumpPath, first,
			&kbuildSelectedFeatureDump{scopes: scopes, results: sealed, discoveryOnly: true}); measureErr != nil {
			return result{}, measureErr
		}
		second, parseErr := parseSelected(scopes, secondMakefile, secondDumpPath, kbuildEvalObjectTree+"/second/", "")
		if parseErr != nil {
			return result{}, parseErr
		}
		_, measureErr := measureSelectedKbuildFeatureDump(root, secondMakefile, secondDumpPath, second,
			&kbuildSelectedFeatureDump{scopes: scopes, results: sealed, discoveryOnly: true})
		var pending *pendingKbuildFeatureDump
		if !errors.As(measureErr, &pending) {
			return result{}, measureErr
		}
		return result{ids: pending.requestIDs}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(secondDiscovery.Value.ids) != 3 || slices.Equal(secondDiscovery.Value.ids, plan.Terminal) {
		t.Fatalf("second selected feature statuses = %#v", secondDiscovery.Value)
	}
	secondPlan, err := kconfig.SelectProbePlanTerminals(secondDiscovery.Plan, secondDiscovery.Value.ids)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nextKbuildSelectedProbeRound(plan, secondPlan, "feature-dump", true); err == nil || !strings.Contains(err.Error(), "rounds exhausted") {
		t.Fatalf("an unmeasured second feature must prevent convergence: %v", err)
	}
	combined, err := nextKbuildSelectedProbeRound(plan, secondPlan, "feature-dump", false)
	if err != nil || len(combined.Terminal) != 6 {
		t.Fatalf("both selected feature producers = %v, %v", combined, err)
	}
	_, err = kconfig.EvaluateKbuildProbeWorkload(options, nil, func(scopes *kconfig.KbuildProbeScopes) (string, error) {
		second, parseErr := parseSelected(scopes, secondMakefile, secondDumpPath, kbuildEvalObjectTree+"/second/", "")
		if parseErr != nil {
			return "", parseErr
		}
		return measureSelectedKbuildFeatureDump(root, secondMakefile, secondDumpPath, second,
			&kbuildSelectedFeatureDump{scopes: scopes, results: sealed})
	})
	if err == nil || !strings.Contains(err.Error(), "differ from the sealed pregraph plan") {
		t.Fatalf("unmeasured second feature must fail ordinary replay: %v", err)
	}
	for _, node := range secondPlan.Nodes {
		request := secondPlan.Requests[node.RequestID]
		step := request.Steps[0]
		success := true
		writeTestProbeResult(t, resultRoot, kconfig.ProbeResult{
			Schema: kconfig.LinuxProbeResultSchema,
			NodeID: node.ID, RequestID: node.RequestID, Scope: node.Scope,
			ToolsetIdentity: identity, Kind: "boolean", Boolean: &success,
			Steps: []kconfig.ProbeStepResult{{Name: step.Name, Status: "success", ExitCode: 0}},
		})
	}
	combinedOracle, err := kconfig.NewProbeResultOracleFromTrees(map[string]string{"host": resultRoot}, combined.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	combinedResults, err := kconfig.NewKbuildGraphGuardResults(combined, combinedOracle)
	if err != nil {
		t.Fatal(err)
	}
	secondReplay, err := kconfig.EvaluateKbuildProbeWorkload(options, nil, func(scopes *kconfig.KbuildProbeScopes) (string, error) {
		second, parseErr := parseSelected(scopes, secondMakefile, secondDumpPath, kbuildEvalObjectTree+"/second/", "")
		if parseErr != nil {
			return "", parseErr
		}
		return measureSelectedKbuildFeatureDump(root, secondMakefile, secondDumpPath, second,
			&kbuildSelectedFeatureDump{scopes: scopes, results: combinedResults, discoveryOnly: true})
	})
	if err != nil || secondReplay.Value != "feature-libelf=1\nfeature-zlib=1\nfeature-bpf=1\n" {
		t.Fatalf("second selected feature include = %q, %v", secondReplay.Value, err)
	}
	request.variables = map[string]string{"FEATURES_DUMP": kbuildEvalSourceTree + "/foreign"}
	if _, _, err := selectedKbuildFeatureDumpPath(root, makefile, request); err == nil || !strings.Contains(err.Error(), "already has a FEATURES_DUMP assignment") {
		t.Fatalf("foreign caller FEATURES_DUMP override must fail closed: %v", err)
	}
}
