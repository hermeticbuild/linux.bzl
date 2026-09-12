// This test-only binary never executes a compiler. Discovery uses the real
// detached API, Bazel's existing probe map executes the selected compiler, and
// verification strictly replays those unmodified results.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

const (
	presentName  = "__LINUX_BZL_OPTIONAL_PRESENT"
	absentName   = "__LINUX_BZL_OPTIONAL_ABSENT"
	invalidName  = "__VA_ARGS__"
	resultSchema = "linux-optional-definedness-fixture-v1"
)

var fixtureArguments = []string{
	"-nostdinc", "-std=c11", "-pedantic-errors", "-Werror",
	"-D" + presentName + "=1", "-U" + absentName,
}

type queryCase struct {
	name      string
	names     []string
	mandatory bool
	rejected  bool
}

func queryCases() []queryCase {
	return []queryCase{
		{"rejected-singleton", []string{invalidName}, false, true},
		{"rejected-pair", []string{presentName, invalidName}, false, true},
		{"valid-vector", []string{presentName, absentName}, false, false},
		{"valid-singleton", []string{presentName}, false, false},
		{"mandatory-vector", []string{presentName, absentName}, true, false},
	}
}

type options struct{ mode, bootstrap, identity, manifest, arch, plan, results, out string }
type fixture struct {
	scopes           *kconfig.KbuildProbeScopes
	toolset, version string
	plans            map[string]*kconfig.ProbePlan
	union            *kconfig.ProbePlan
	macroWrites      map[string]kconfig.ProbeReference
}
type caseResult struct {
	NodeID   string          `json:"node_id"`
	State    string          `json:"state"`
	Values   map[string]bool `json:"values"`
	ExitCode int             `json:"exit_code"`
}
type receipt struct {
	Schema          string                      `json:"schema"`
	Toolset         string                      `json:"toolset"`
	CompilerVersion string                      `json:"compiler_version"`
	Cases           map[string]caseResult       `json:"cases"`
	Checks          []string                    `json:"checks"`
	MacroWrites     map[string]macroWriteResult `json:"macro_writes"`
}

func main() {
	var opts options
	for name, value := range map[string]*string{
		"mode": &opts.mode, "bootstrap": &opts.bootstrap, "identity": &opts.identity,
		"manifest": &opts.manifest, "arch": &opts.arch, "plan": &opts.plan,
		"results": &opts.results, "out": &opts.out,
	} {
		flag.StringVar(value, name, "", "test fixture input/output")
	}
	flag.Parse()
	if err := run(opts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(opts options) error {
	if flag.NArg() != 0 || opts.out == "" || (opts.mode != "discover" && opts.mode != "verify") {
		return fmt.Errorf("fixture requires discover or verify mode and output")
	}
	f, err := loadFixture(opts)
	if err != nil {
		return err
	}
	if opts.mode == "discover" {
		return f.union.Write(opts.out)
	}
	actual, err := kconfig.ReadProbePlan(opts.plan)
	if err != nil {
		return err
	}
	if !samePlan(actual, f.union) {
		return fmt.Errorf("executed fixture plan differs from exact replay discovery")
	}
	oracle, err := kconfig.NewProbeResultOracleFromTrees(
		map[string]string{"target": opts.results}, map[string]string{"target": f.toolset})
	if err != nil {
		return err
	}
	if err := oracle.ValidatePlan(actual); err != nil {
		return err
	}
	results, err := kconfig.ReadProbeResultTree(opts.results)
	if err != nil {
		return err
	}
	if len(results) != len(queryCases())+len(macroWriteCases()) {
		return fmt.Errorf("unexpected fixture result count")
	}
	got, err := f.verify(oracle)
	if err != nil {
		return err
	}
	data, err := json.Marshal(got)
	if err != nil {
		return err
	}
	return os.WriteFile(opts.out, append(data, '\n'), 0o600)
}

func loadFixture(opts options) (*fixture, error) {
	markers, err := os.ReadDir(opts.identity)
	if err != nil {
		return nil, err
	}
	if len(markers) != 1 || !markers[0].Type().IsRegular() {
		return nil, fmt.Errorf("fixture needs exactly one regular toolset identity marker")
	}
	identity := markers[0].Name()
	manifest, err := toolaction.ReadKbuildToolsetManifest(opts.manifest)
	if err != nil {
		return nil, err
	}
	manifestID, err := manifest.Identity()
	if err != nil {
		return nil, err
	}
	if manifest.Scope != "target" || manifestID != identity || len(manifest.Actions["cc"]) == 0 {
		return nil, fmt.Errorf("fixture manifest is not the configured target compiler contract")
	}
	boundaries := 0
	for _, arg := range manifest.Actions["cc"] {
		if arg == "__LINUX_BZL_KBUILD_ARGS_V1__" {
			boundaries++
		}
	}
	if boundaries != 1 {
		return nil, fmt.Errorf("fixture compiler contract needs one argument boundary")
	}
	builder, err := kconfig.NewProbePlanBuilder(identity, "")
	if err != nil {
		return nil, err
	}
	bootstrapRef, err := kconfig.AddLinuxCompilerBootstrap(builder, "target")
	if err != nil {
		return nil, err
	}
	bootstrapPlan, err := builder.Plan(bootstrapRef)
	if err != nil {
		return nil, err
	}
	bootstrap, err := kconfig.NewProbeResultOracleFromTrees(
		map[string]string{"target": opts.bootstrap}, map[string]string{"target": identity})
	if err != nil {
		return nil, err
	}
	if err := bootstrap.ValidatePlan(bootstrapPlan); err != nil {
		return nil, err
	}
	result, err := bootstrap.Result(bootstrapRef)
	if err != nil {
		return nil, err
	}
	facts, err := kconfig.ParseLinuxCompilerBootstrapResult(result, "target", identity)
	if err != nil {
		return nil, err
	}
	arch, err := os.ReadFile(opts.arch)
	if err != nil {
		return nil, err
	}
	evaluation, err := kconfig.EvaluateKbuildProbeWorkload(kconfig.KbuildProbeWorkloadOptions{
		Target: kconfig.KbuildProbeScopeOptions{Architecture: strings.TrimSpace(string(arch)), Facts: facts, Tools: maps.Clone(manifest.Tools)},
	}, nil, func(scopes *kconfig.KbuildProbeScopes) (*fixture, error) {
		f := &fixture{scopes: scopes, toolset: identity, version: facts.VersionText(), plans: map[string]*kconfig.ProbePlan{}}
		var variants []kconfig.ProbePlanVariant
		for _, test := range queryCases() {
			plan, err := f.discover(test)
			if err != nil {
				return nil, err
			}
			f.plans[test.name] = plan
			variants = append(variants, kconfig.ProbePlanVariant{Name: test.name, Plan: plan})
		}
		writes, err := f.discoverMacroWrites()
		if err != nil {
			return nil, err
		}
		variants = append(variants, kconfig.ProbePlanVariant{Name: "macro-writes", Plan: writes})
		f.union, err = kconfig.MergeProbePlans(variants)
		if err != nil {
			return nil, err
		}
		if len(f.union.Nodes) != len(queryCases())+len(macroWriteCases()) {
			return nil, fmt.Errorf("fixture cases aliased")
		}
		return f, nil
	})
	if err != nil {
		return nil, err
	}
	if len(evaluation.Plan.Nodes) != 0 {
		return nil, fmt.Errorf("detached fixture changed ordinary discovery")
	}
	return evaluation.Value, nil
}

func (f *fixture) discover(test queryCase) (*kconfig.ProbePlan, error) {
	batch, err := kconfig.NewKbuildCompilerGuardBatch(f.scopes, nil)
	if err != nil {
		return nil, err
	}
	if test.mandatory {
		values, ready, err := batch.CompilerDefinedness("target", "cc", "c", fixtureArguments, nil, test.names, nil)
		if err != nil {
			return nil, err
		}
		if ready || values != nil {
			return nil, fmt.Errorf("mandatory discovery fabricated facts")
		}
	} else {
		values, state, err := batch.OptionalCompilerDefinedness("target", "cc", "c", fixtureArguments, nil, test.names, nil)
		if err != nil {
			return nil, err
		}
		if state != kconfig.OptionalCompilerDefinednessPending || values != nil {
			return nil, fmt.Errorf("optional discovery fabricated facts")
		}
	}
	plan, err := batch.Plan()
	if err != nil {
		return nil, err
	}
	if len(plan.Nodes) != 1 || len(plan.Terminal) != 1 {
		return nil, fmt.Errorf("fixture query is not one exact terminal")
	}
	return plan, nil
}

func (f *fixture) verify(oracle *kconfig.ProbeResultOracle) (*receipt, error) {
	empty, err := kconfig.NewKbuildCompilerGuardBatch(f.scopes, oracle)
	if err != nil {
		return nil, err
	}
	emptyAnswers, err := empty.Answers()
	if err != nil {
		return nil, err
	}
	report := &receipt{Schema: resultSchema, Toolset: f.toolset, CompilerVersion: f.version, Cases: map[string]caseResult{}}
	for _, test := range queryCases() {
		batch, err := kconfig.NewKbuildCompilerGuardBatch(f.scopes, oracle)
		if err != nil {
			return nil, err
		}
		var values map[string]bool
		stateName := "answered"
		if test.mandatory {
			var ready bool
			values, ready, err = batch.CompilerDefinedness("target", "cc", "c", fixtureArguments, nil, test.names, nil)
			if err == nil && !ready {
				err = fmt.Errorf("mandatory real result stayed pending")
			}
			stateName = "mandatory-answered"
		} else {
			var state kconfig.OptionalCompilerDefinednessState
			values, state, err = batch.OptionalCompilerDefinedness("target", "cc", "c", fixtureArguments, nil, test.names, nil)
			want := kconfig.OptionalCompilerDefinednessAnswered
			if test.rejected {
				want, stateName = kconfig.OptionalCompilerDefinednessUnqueryable, "unqueryable"
			}
			if err == nil && state != want {
				err = fmt.Errorf("%s returned state %d, want %d", test.name, state, want)
			}
		}
		if err != nil {
			return nil, err
		}
		want := map[string]bool{}
		if !test.rejected {
			for _, name := range test.names {
				want[name] = name == presentName
			}
		}
		if !maps.Equal(values, want) || test.rejected && values != nil {
			return nil, fmt.Errorf("%s returned wrong or failed-probe facts", test.name)
		}
		answers, err := batch.Answers()
		if err != nil {
			return nil, err
		}
		if reflect.DeepEqual(answers, emptyAnswers) != test.rejected {
			return nil, fmt.Errorf("%s published wrong immutable facts", test.name)
		}
		plan, err := batch.Plan()
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(plan, f.plans[test.name]) {
			return nil, fmt.Errorf("%s changed request on replay", test.name)
		}
		node := plan.Nodes[0]
		request := plan.Requests[node.RequestID]
		result, err := oracle.Result(kconfig.ProbeReference{NodeID: node.ID, RequestID: node.RequestID, Scope: node.Scope, Kind: request.Outcome.Kind})
		if err != nil {
			return nil, err
		}
		if len(result.Steps) != 1 {
			return nil, fmt.Errorf("%s has wrong process shape", test.name)
		}
		step := result.Steps[0]
		if test.rejected {
			if step.Status != "failure" || step.ExitCode < 1 || step.ExitCode > 255 {
				return nil, fmt.Errorf("%s has no ordinary rejected compiler process", test.name)
			}
		} else if step.Status != "success" || step.ExitCode != 0 {
			return nil, fmt.Errorf("%s compiler did not succeed", test.name)
		}
		report.Cases[test.name] = caseResult{node.ID, stateName, values, step.ExitCode}
	}
	// Identical vectors canonicalize to one request. A singleton is a new
	// attempt, not a fact supplied by the earlier rejected pair.
	pair := queryCases()[1]
	repeat := pair
	repeat.names = []string{invalidName, presentName, invalidName}
	plan, err := f.discover(repeat)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(plan, f.plans[pair.name]) {
		return nil, fmt.Errorf("exact pair retry changed canonical identity")
	}
	if err := f.checkRejectedOnlyOracle(oracle, pair); err != nil {
		return nil, err
	}
	// Optional failure is never a replacement for the mandatory request. The
	// actual failing mandatory process is deliberately not a normal dependency.
	mandatory, err := kconfig.NewKbuildCompilerGuardBatch(f.scopes, oracle)
	if err != nil {
		return nil, err
	}
	values, ready, err := mandatory.CompilerDefinedness("target", "cc", "c", fixtureArguments, nil, pair.names, nil)
	if err == nil || ready || values != nil {
		return nil, fmt.Errorf("mandatory query accepted optional failure authority")
	}
	if _, err := mandatory.Answers(); err == nil {
		return nil, fmt.Errorf("failed mandatory replay published answers")
	}
	optionalRequest := f.plans["valid-vector"].Requests[f.plans["valid-vector"].Nodes[0].RequestID]
	mandatoryRequest := f.plans["mandatory-vector"].Requests[f.plans["mandatory-vector"].Nodes[0].RequestID]
	if mandatoryRequest.Outcome.Kind != "text" || !mandatoryRequest.Outcome.RequireSuccess ||
		!reflect.DeepEqual(optionalRequest.Steps, mandatoryRequest.Steps) {
		return nil, fmt.Errorf("mandatory compiler process semantics changed")
	}
	report.Checks = []string{"empty-failure-snapshot", "exact-vector-identity", "rejected-only-singleton-miss", "fresh-singleton-answer", "mandatory-success", "mandatory-optional-separation", "ordinary-discovery-unchanged"}
	report.MacroWrites, err = f.verifyMacroWrites(oracle)
	if err != nil {
		return nil, err
	}
	return report, nil
}

func (f *fixture) checkRejectedOnlyOracle(oracle *kconfig.ProbeResultOracle, pair queryCase) error {
	node := f.plans[pair.name].Nodes[0]
	result, err := oracle.Result(kconfig.ProbeReference{NodeID: node.ID, RequestID: node.RequestID, Scope: node.Scope, Kind: "boolean"})
	if err != nil {
		return err
	}
	data, err := result.CanonicalJSON()
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "optional-definedness-real-rejection-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.Mkdir(filepath.Join(dir, "results"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "results", node.ID+".json"), data, 0o600); err != nil {
		return err
	}
	prior, err := kconfig.NewProbeResultOracleFromTrees(map[string]string{"target": dir}, map[string]string{"target": f.toolset})
	if err != nil {
		return err
	}
	replay, err := kconfig.NewKbuildCompilerGuardBatch(f.scopes, prior)
	if err != nil {
		return err
	}
	values, state, err := replay.OptionalCompilerDefinedness("target", "cc", "c", fixtureArguments, nil, pair.names, nil)
	if err != nil {
		return err
	}
	if state != kconfig.OptionalCompilerDefinednessUnqueryable || values != nil {
		return fmt.Errorf("real rejected pair did not replay exactly")
	}
	if _, err := replay.Answers(); err != nil {
		return err
	}
	missing, err := kconfig.NewKbuildCompilerGuardBatch(f.scopes, prior)
	if err != nil {
		return err
	}
	values, _, err = missing.OptionalCompilerDefinedness("target", "cc", "c", fixtureArguments, nil, []string{presentName}, nil)
	if err == nil || values != nil {
		return fmt.Errorf("rejected pair supplied an unmeasured singleton")
	}
	if _, err := missing.Answers(); err == nil {
		return fmt.Errorf("missing singleton result published partial facts")
	}
	singleton, err := f.discover(queryCases()[3])
	if err != nil {
		return err
	}
	if singleton.Nodes[0].ID == node.ID || !slices.Equal(singleton.Terminal, f.plans["valid-singleton"].Terminal) {
		return fmt.Errorf("fresh singleton lost exact new attempt identity")
	}
	return nil
}

// The wire tree canonicalizes node/terminal ordering and omits empty JSON
// fields. Compare the full semantic declaration and exact request bytes, not
// incidental nil-versus-empty slices or registration order.
func samePlan(a, b *kconfig.ProbePlan) bool {
	if !maps.Equal(a.Toolsets, b.Toolsets) || len(a.Nodes) != len(b.Nodes) || len(a.Requests) != len(b.Requests) {
		return false
	}
	at, bt := slices.Clone(a.Terminal), slices.Clone(b.Terminal)
	slices.Sort(at)
	slices.Sort(bt)
	if !slices.Equal(at, bt) {
		return false
	}
	nodes := map[string]kconfig.ProbePlanNode{}
	for _, node := range a.Nodes {
		nodes[node.ID] = node
	}
	for _, node := range b.Nodes {
		other, ok := nodes[node.ID]
		if !ok || node.Scope != other.Scope || node.RequestID != other.RequestID || !slices.Equal(node.Inputs, other.Inputs) {
			return false
		}
	}
	for id, request := range a.Requests {
		other, ok := b.Requests[id]
		if !ok {
			return false
		}
		left, err := request.CanonicalJSON()
		if err != nil {
			return false
		}
		right, err := other.CanonicalJSON()
		if err != nil || !bytes.Equal(left, right) {
			return false
		}
	}
	return true
}
