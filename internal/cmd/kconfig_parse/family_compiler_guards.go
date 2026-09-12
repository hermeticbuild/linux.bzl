package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

const familyCompilerGuardSchema = "linux-kbuild-compiler-guards-v4"
const maxFamilyCompilerGuardRounds = 3
const maxFamilyCompilerGuardBytes = 64 << 20
const maxFamilyCompilerGuardQueries = 8192

// Sharing descriptors must not make serialized memberships or replay work
// unbounded. These fixed ceilings admit four formerly admissible frontiers;
// they do not grow with the number of variants. The original compact discovery
// and serialized byte ceilings remain unchanged. None is a total heap bound.
const maxFamilyCompilerGuardMemberships = 4 * maxFamilyCompilerGuardQueries
const maxFamilyCompilerGuardExpandedBytes = 4 * (maxFamilyCompilerGuardBytes / 2)
const familyCompilerGuardQueryReferenceBytes = 160

// This transport contains only pure initial-compiler queries. Reached-file
// hints are not source-closure receipts, and are deliberately not promoted to
// one by serialization. Every replay still verifies the original lowering and
// observes the current execution cut, then scans the original source anew.
type familyCompilerGuardQuery struct {
	Kind                        string `json:",omitempty"`
	Scope, Role, Language       string
	Arguments, TranslationUnits []string
	Environment                 map[string]string
	Names                       []string
	Calls                       []kconfig.CompilerIntrinsicCall `json:",omitempty"`
}

func (q familyCompilerGuardQuery) contextKey() string {
	return q.Kind + ":" + q.compilerContext().id()
}

// Only complete, sorted vectors are admitted to the unique-query budget.
// This is a descriptor identity, not an authenticated compiler answer.
func (q familyCompilerGuardQuery) payloadKey() string {
	data, _ := json.Marshal(struct {
		Context, Kind string
		Names         []string
		Calls         []kconfig.CompilerIntrinsicCall
	}{q.compilerContext().id(), q.Kind, q.Names, q.Calls})
	return string(data)
}

type familyCompilerGuardManifest struct {
	Schema    string
	Round     int
	Previous  []string
	Toolsets  map[string]string
	Variants  map[string][]familyCompilerGuardQuery
	PlanID    string
	Truncated bool
}

type familyCompilerGuardFlags struct {
	manifestOut, planOut             string
	manifests, plans, hosts, targets namedPathFlag
	provided                         map[string]bool
}

func (f *familyCompilerGuardFlags) register(flags *flag.FlagSet) {
	f.provided = map[string]bool{}
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"family_compiler_guard_manifest_out", &f.manifestOut},
		{"family_compiler_guard_plan_out", &f.planOut},
	} {
		flags.Func(field.name, "Supplemental compiler-guard discovery output", func(value string) error {
			if f.provided[field.name] || strings.TrimSpace(value) == "" {
				return fmt.Errorf("-%s requires one nonempty value", field.name)
			}
			f.provided[field.name] = true
			*field.value = workspacePath(value)
			return nil
		})
	}
	for _, field := range []struct {
		name  string
		value *namedPathFlag
	}{
		{"family_compiler_guard_manifest", &f.manifests},
		{"family_compiler_guard_plan", &f.plans},
		{"family_compiler_guard_host_results", &f.hosts},
		{"family_compiler_guard_target_results", &f.targets},
	} {
		flags.Var(field.value, field.name, "Frozen supplemental round input in ROUND=PATH form")
	}
}

func (f *familyCompilerGuardFlags) requested() bool {
	return f != nil && (f.manifestOut != "" || f.planOut != "" ||
		len(f.manifests)+len(f.plans)+len(f.hosts)+len(f.targets) != 0)
}

type familyCompilerGuardRoundInput struct {
	manifest, plan, host, target string
}

func (f *familyCompilerGuardFlags) validate(execution *familyExecutionRequest) ([]familyCompilerGuardRoundInput, error) {
	if !f.requested() && (execution == nil || execution.mode != "guards") {
		return nil, nil
	}
	if execution == nil || execution.mode != "guards" && execution.mode != "replay" {
		return nil, fmt.Errorf("supplemental compiler guards require family execution guards or replay mode")
	}
	if execution.mode == "guards" {
		if f.manifestOut == "" || f.planOut == "" {
			return nil, fmt.Errorf("compiler guard discovery requires both manifest and plan outputs")
		}
	} else if f.manifestOut != "" || f.planOut != "" {
		return nil, fmt.Errorf("family replay does not accept compiler guard discovery outputs")
	}
	count := len(f.manifests)
	if count > maxFamilyCompilerGuardRounds || execution.mode == "guards" && count >= maxFamilyCompilerGuardRounds {
		return nil, fmt.Errorf("compiler guard rounds exceed the bounded frontier")
	}
	names := map[string]bool{}
	for index := range count {
		names[strconv.Itoa(index)] = true
	}
	inputs := make([]familyCompilerGuardRoundInput, count)
	for _, field := range []struct {
		name   string
		values namedPathFlag
		assign func(*familyCompilerGuardRoundInput, string)
	}{
		{"family_compiler_guard_manifest", f.manifests, func(r *familyCompilerGuardRoundInput, v string) { r.manifest = v }},
		{"family_compiler_guard_plan", f.plans, func(r *familyCompilerGuardRoundInput, v string) { r.plan = v }},
		{"family_compiler_guard_host_results", f.hosts, func(r *familyCompilerGuardRoundInput, v string) { r.host = v }},
		{"family_compiler_guard_target_results", f.targets, func(r *familyCompilerGuardRoundInput, v string) { r.target = v }},
	} {
		values, err := familyExecutionNamedPaths(field.name, field.values, names)
		if err != nil {
			return nil, err
		}
		for index := range inputs {
			field.assign(&inputs[index], values[strconv.Itoa(index)])
		}
	}
	// Extend the existing overlap check to all supplemental inputs/outputs;
	// no guard output may replace an immutable cut or an earlier round.
	paths := *execution
	paths.segments = maps.Clone(execution.segments)
	if paths.segments == nil {
		paths.segments = map[string]string{}
	}
	paths.stores = maps.Clone(execution.stores)
	if paths.stores == nil {
		paths.stores = map[string]string{}
	}
	if f.manifestOut != "" {
		paths.segments["guard manifest"] = f.manifestOut
	}
	if f.planOut != "" {
		paths.segments["guard plan"] = f.planOut
	}
	for index, input := range inputs {
		for name, path := range map[string]string{"manifest": input.manifest, "plan": input.plan, "host": input.host, "target": input.target} {
			paths.stores[fmt.Sprintf("guard round %d %s", index, name)] = path
		}
	}
	if err := paths.validatePaths(); err != nil {
		return nil, err
	}
	return inputs, nil
}

func familyCompilerGuardPlanID(plan *kconfig.ProbePlan) (string, error) {
	canonical, err := kconfig.MergeProbePlans([]kconfig.ProbePlanVariant{{Name: "round", Plan: plan}})
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func readFamilyCompilerGuardManifest(filename string) (*familyCompilerGuardManifest, string, error) {
	info, err := os.Stat(filename)
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("compiler guard manifest must be a regular file")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("compiler guard manifest must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxFamilyCompilerGuardBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxFamilyCompilerGuardBytes {
		return nil, "", fmt.Errorf("compiler guard manifest exceeds its byte budget")
	}
	if err := preflightFamilyCompilerGuardJSON(data); err != nil {
		return nil, "", err
	}
	manifest, err := unmarshalFamilyCompilerGuardManifest(data)
	if err != nil {
		return nil, "", err
	}
	canonical, err := marshalFamilyCompilerGuardManifest(*manifest)
	if err != nil {
		return nil, "", err
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(data, canonical) {
		return nil, "", fmt.Errorf("compiler guard manifest is not canonical")
	}
	return manifest, fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// Bound structural allocation before decoding slices of query structs. A byte
// limit alone permits millions of tiny JSON objects to expand into gigabytes.
func preflightFamilyCompilerGuardJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	queries, contexts, names, calls, values := 0, 0, 0, 0, 0
	var value func(int, string) error
	value = func(depth int, kind string) error {
		values++
		if depth > 32 || values > 1<<22 {
			return fmt.Errorf("compiler guard JSON exceeds its structural budget")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delimiter {
		case '{':
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				child := ""
				// encoding/json matches struct field names case-insensitively.
				// Count aliases before typed decoding too, even though the final
				// canonical-byte comparison rejects their spelling afterward.
				field, _ := key.(string)
				if kind == "root" && strings.EqualFold(field, "Variants") {
					child = "variants"
				}
				if kind == "root" && strings.EqualFold(field, "Contexts") {
					child = "contexts"
				}
				if kind == "contexts" {
					contexts++
					if contexts > maxFamilyCompilerGuardQueries {
						return fmt.Errorf("compiler guard JSON exceeds its context budget")
					}
				}
				if kind == "variants" {
					child = "queries"
				}
				if kind == "query" && strings.EqualFold(field, "Names") {
					child = "names"
				}
				if kind == "query" && strings.EqualFold(field, "Calls") {
					child = "calls"
				}
				if err := value(depth+1, child); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				child := ""
				if kind == "queries" {
					queries++
					child = "query"
				}
				if kind == "names" {
					names++
				}
				if kind == "calls" {
					calls++
				}
				if queries > maxFamilyCompilerGuardMemberships || names > 1<<20 {
					return fmt.Errorf("compiler guard JSON exceeds its query/name budget")
				}
				if calls > maxFamilyCompilerIntrinsicCalls {
					return fmt.Errorf("compiler guard JSON exceeds its intrinsic call budget")
				}
				if err := value(depth+1, child); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("invalid compiler guard JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := value(0, "root"); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("compiler guard JSON has trailing content")
	}
	return nil
}

type familyCompilerGuardPriorRound struct {
	manifest *familyCompilerGuardManifest
	oracle   *kconfig.ProbeResultOracle
	replayed []kconfig.ProbePlanVariant
}

type familyCompilerGuardPipeline struct {
	flags    *familyCompilerGuardFlags
	names    []string
	toolsets map[string]string
	previous []string
	prior    []familyCompilerGuardPriorRound
	queries  map[string][]familyCompilerGuardQuery
	plans    []kconfig.ProbePlanVariant
	active   map[string]*familyCompilerGuardQuery
	known    map[string]map[string]bool
	// Exact-vector failed attempts, scoped to fresh current variant replay.
	// Never interpreted as per-name definedness or source-closure evidence.
	rejected                map[string]bool
	queryBytes              map[string]int
	activeVariant           string
	prepared, finished      map[string]bool
	bytes, count, nameCount int
	callCount               int
	expandedBytes           int
	contexts                map[string]familyCompilerGuardContext
	uniqueQueries           map[string]struct{}
	truncated               bool
	diagnostics             io.Writer
	limitReason             string
	limitCurrent, limitMax  int
	optional                *familyCompilerGuardOptionalStage
	tokenHints              *familyCompilerGuardOptionalStage
	literalHints            *familyCompilerGuardOptionalStage
	// Pending reservations are tier-local and never become compiler answers.
	// Entries are inserted only after the existing context/name/byte limits pass.
	pendingNames map[[32]byte]map[string]bool
	priority     *familyCompilerGuardPriorityIndex
}

// This per-variant index connects original known-name keys to their scheduling
// class. Unlike pending reservations, it survives a discarded optional stage so
// weaker hints still respect already-recorded demands. It contains no facts.
type familyCompilerGuardPriorityIndex struct {
	contexts map[string][32]byte
	// A prior failed vector disables this optional optimization for its class;
	// it never rejects or answers another vector in that class.
	rejected  map[familyCompilerGuardPendingClass]bool
	truncated bool
}

type familyCompilerGuardPendingClass struct {
	kind string
	key  [32]byte
}

func (index *familyCompilerGuardPriorityIndex) rejectPending(query familyCompilerGuardQuery) {
	if index == nil || index.truncated {
		return
	}
	class := familyCompilerGuardPendingClass{query.Kind, familyCompilerGuardSchedulingKey(query)}
	if index.rejected[class] {
		return
	}
	if len(index.contexts)+len(index.rejected) >= maxFamilyCompilerGuardMemberships {
		index.truncated = true
		index.contexts, index.rejected = nil, nil
		return
	}
	if index.rejected == nil {
		index.rejected = map[familyCompilerGuardPendingClass]bool{}
	}
	index.rejected[class] = true
}

func familyCompilerGuardSchedulingKey(query familyCompilerGuardQuery) [32]byte {
	return kconfig.CompilerGuardDefinednessSchedulingKey(query.Scope, query.Role, query.Language,
		query.Arguments, query.TranslationUnits, query.Environment)
}

func (index *familyCompilerGuardPriorityIndex) record(key string, query familyCompilerGuardQuery) {
	if index == nil || index.truncated || query.Kind != "" && query.Kind != familyCompilerOptionalDefinednessQueryKind && query.Kind != familyCompilerTokenHintQueryKind {
		return
	}
	if _, exists := index.contexts[key]; exists {
		return
	}
	if len(index.contexts)+len(index.rejected) >= maxFamilyCompilerGuardMemberships {
		index.truncated = true
		index.contexts, index.rejected = nil, nil
		return
	}
	if index.contexts == nil {
		index.contexts = map[string][32]byte{}
	}
	index.contexts[key] = familyCompilerGuardSchedulingKey(query)
}

// Optional expansion demands are staged independently while baseline discovery
// visits every variant. Only immutable query descriptors, exact terminal
// bindings and frozen dependency closures survive finishVariant; no scope,
// evaluator, source scanner or compiler facts are retained here.
type familyCompilerGuardOptionalStage struct {
	kind   string
	ledger familyCompilerGuardPipeline
	// One immutable DAG per tier; variants retain only their exact query roots.
	// Repeated compiler contexts must not occupy the staging budget twice.
	plan                 *kconfig.ProbePlan
	variants             []familyCompilerGuardOptionalVariant
	planBytes, planNodes int
	disabled             bool
	omitted              int
	reason               string
}

type familyCompilerGuardOptionalVariant struct {
	name      string
	queries   []familyCompilerGuardOptionalQuery
	terminals []string
}

type familyCompilerGuardOptionalQuery struct {
	query    familyCompilerGuardQuery
	terminal string
}

func newFamilyCompilerGuardPipeline(flags *familyCompilerGuardFlags, inputs []familyCompilerGuardRoundInput, variants []familyPlanVariantRequest, toolsets map[string]string) (*familyCompilerGuardPipeline, error) {
	if !flags.requested() {
		return nil, nil
	}
	p := &familyCompilerGuardPipeline{flags: flags, toolsets: maps.Clone(toolsets), queries: map[string][]familyCompilerGuardQuery{}, prepared: map[string]bool{}, finished: map[string]bool{}, diagnostics: os.Stderr}
	for _, variant := range variants {
		p.names = append(p.names, variant.name)
	}
	slices.Sort(p.names)
	for index, input := range inputs {
		manifest, id, err := readFamilyCompilerGuardManifest(input.manifest)
		if err != nil {
			return nil, fmt.Errorf("read compiler guard round %d: %w", index, err)
		}
		if manifest.Schema != familyCompilerGuardSchema || manifest.Round != index ||
			!slices.Equal(manifest.Previous, p.previous) || !maps.Equal(manifest.Toolsets, toolsets) ||
			!slices.Equal(slices.Sorted(maps.Keys(manifest.Variants)), p.names) {
			return nil, fmt.Errorf("compiler guard round %d changed its chain, toolsets or family membership", index)
		}
		queryCount, nameCount, callCount, expandedBytes := 0, 0, 0, 0
		uniqueQueries := map[string]struct{}{}
		for _, queries := range manifest.Variants {
			last := ""
			for _, query := range queries {
				key := query.contextKey()
				if key <= last || query.validatePayload() != nil {
					return nil, fmt.Errorf("compiler guard round %d contains noncanonical queries", index)
				}
				last = key
				queryCount++
				nameCount += len(query.Names)
				callCount += len(query.Calls)
				uniqueQueries[query.payloadKey()] = struct{}{}
				expandedBytes += familyCompilerGuardExpandedQueryBytes(query)
				if queryCount > maxFamilyCompilerGuardMemberships || len(uniqueQueries) > maxFamilyCompilerGuardQueries ||
					expandedBytes > maxFamilyCompilerGuardExpandedBytes || nameCount > 1<<20 || callCount > maxFamilyCompilerIntrinsicCalls {
					return nil, fmt.Errorf("compiler guard round %d contains an over-budget frontier", index)
				}
			}
		}
		if manifest.Truncated && queryCount != 0 {
			return nil, fmt.Errorf("compiler guard round %d contains an over-budget or partial frontier", index)
		}
		plan, err := kconfig.ReadProbePlan(input.plan)
		if err != nil {
			return nil, err
		}
		planID, err := familyCompilerGuardPlanID(plan)
		if err != nil {
			return nil, err
		}
		if planID != manifest.PlanID || !maps.Equal(plan.Toolsets, toolsets) {
			return nil, fmt.Errorf("compiler guard round %d changed its frozen plan", index)
		}
		oracle, err := kconfig.NewProbeResultOracleFromTrees(map[string]string{"host": input.host, "target": input.target}, toolsets)
		if err != nil {
			return nil, err
		}
		if err := oracle.ValidatePlan(plan); err != nil {
			return nil, err
		}
		p.prior = append(p.prior, familyCompilerGuardPriorRound{manifest: manifest, oracle: oracle})
		p.previous = append(p.previous, id)
	}
	return p, nil
}

// carryForwardEmptyRound skips optional per-variant discovery once the prior
// round contributed no new compiler facts. The caller has already loaded the
// ordinary probes and authenticated the initial snapshots and completed cut.
// Bazel's rule-owned chain gives every discovery round the same immutable
// source/config/tool/probe/cut inputs; the manifest by itself is NOT evidence
// that unrelated source bytes have no further queries.
//
// This writes only a new empty query transport. It grants no definedness or
// source-closure receipt, and cannot run in final replay. Final replay still
// lowers and scans every variant, binds prior queries to current symbolic
// dependency results, and verifies every frozen plan union before publication.
func (p *familyCompilerGuardPipeline) carryForwardEmptyRound(mode string) (bool, error) {
	if p == nil || mode != "guards" || p.flags == nil || p.flags.manifestOut == "" || p.flags.planOut == "" ||
		len(p.prior) == 0 || len(p.prior) >= maxFamilyCompilerGuardRounds {
		return false, nil
	}
	if len(p.prepared) != 0 || len(p.finished) != 0 || p.activeVariant != "" {
		return false, nil
	}
	last := p.prior[len(p.prior)-1].manifest
	if last.Truncated {
		return false, nil
	}
	for _, queries := range last.Variants {
		if len(queries) != 0 {
			return false, nil
		}
	}
	builder, err := kconfig.NewProbePlanBuilder(p.toolsets["target"], p.toolsets["host"])
	if err != nil {
		return false, err
	}
	empty, err := builder.Plan()
	if err != nil {
		return false, err
	}
	id, err := familyCompilerGuardPlanID(empty)
	if err != nil {
		return false, err
	}
	// The constructor checked this ID against the actual frozen plan and its
	// results. Empty query lists alone do not establish an empty probe DAG.
	if last.PlanID != id {
		return false, nil
	}
	queries := make(map[string][]familyCompilerGuardQuery, len(p.names))
	for _, name := range p.names {
		queries[name] = nil
	}
	if err := p.writeRound(empty, queries, false); err != nil {
		return true, err
	}
	p.reportDiagnostics("carried_empty")
	return true, nil
}

func (p *familyCompilerGuardPipeline) prepareVariant(name string, scopes *kconfig.KbuildProbeScopes, metadata *kconfig.CompactMetadata) error {
	if p.prepared[name] || p.activeVariant != "" || !slices.Contains(p.names, name) {
		return fmt.Errorf("compiler guards repeat or reference unknown variant %q", name)
	}
	p.prepared[name] = true
	p.activeVariant = name
	p.active = map[string]*familyCompilerGuardQuery{}
	p.known = map[string]map[string]bool{}
	p.rejected = map[string]bool{}
	p.queryBytes = map[string]int{}
	p.pendingNames = nil
	p.priority = &familyCompilerGuardPriorityIndex{}
	var snapshots []*kconfig.KbuildCompilerGuardAnswers
	for index := range p.prior {
		round := &p.prior[index]
		batch, err := kconfig.NewKbuildCompilerGuardBatch(scopes, round.oracle)
		if err != nil {
			return err
		}
		var rejected []string
		for _, query := range round.manifest.Variants[name] {
			state, err := query.evaluateCompletion(batch)
			if err != nil {
				return fmt.Errorf("replay compiler guard round %d variant %s: %w", index, name, err)
			}
			if state == kconfig.OptionalCompilerDefinednessPending {
				return fmt.Errorf("frozen compiler guard result is missing")
			}
			if state == kconfig.OptionalCompilerDefinednessUnqueryable {
				rejected = append(rejected, query.payloadKey())
				p.priority.rejectPending(query)
				continue
			}
			key := query.contextKey()
			p.priority.record(key, query)
			if p.known[key] == nil {
				p.known[key] = map[string]bool{}
			}
			for _, name := range query.Names {
				p.known[key][name] = true
			}
			for _, call := range query.Calls {
				p.known[key][familyCompilerIntrinsicCallKey(call)] = true
			}
		}
		plan, err := batch.Plan()
		if err != nil {
			return err
		}
		answers, err := batch.Answers()
		if err != nil {
			return err
		}
		// Only after exact requests, configured toolsets, imported current
		// ordinary dependency values and this batch plan validate may these
		// process-local keys suppress a new identical vector. Prior terminals
		// remain in round.replayed and its exact frozen union is still checked.
		for _, key := range rejected {
			p.rejected[key] = true
		}
		round.replayed = append(round.replayed, kconfig.ProbePlanVariant{Name: name, Plan: plan})
		snapshots = append(snapshots, answers)
	}
	answers, err := kconfig.MergeKbuildCompilerGuardAnswers(snapshots...)
	if err != nil {
		return err
	}
	metadata.SetCompilerGuardAnswers(answers)
	for _, stage := range []*familyCompilerGuardOptionalStage{p.optional, p.tokenHints, p.literalHints} {
		if stage != nil && !stage.disabled {
			stage.ledger.active = map[string]*familyCompilerGuardQuery{}
			stage.ledger.known = p.known
			stage.ledger.queryBytes = map[string]int{}
			stage.ledger.pendingNames = nil
			stage.ledger.priority = p.priority
		}
	}
	if p.flags.manifestOut != "" {
		metadata.SetCompilerGuardObserver(p.observe)
	}
	return nil
}

// Diagnostics are deliberately outside the canonical manifest and probe plan.
// Counts are attempted per-variant query memberships, not deduplicated compiler
// executions. No context keys, source names, arguments or environment are logged.
func (p *familyCompilerGuardPipeline) reportDiagnostics(event string) {
	if p.diagnostics == nil {
		return
	}
	reason := p.limitReason
	if reason == "" {
		reason = "none"
	}
	_, _ = fmt.Fprintf(p.diagnostics, "compiler_guard_round round=%d event=%s reason=%s current=%d limit=%d query_memberships=%d unique_queries=%d contexts=%d names=%d calls=%d estimated_bytes=%d expanded_bytes=%d finished_variants=%d variants=%d\n",
		len(p.prior), event, reason, p.limitCurrent, p.limitMax, p.count, len(p.uniqueQueries), len(p.contexts), p.nameCount, p.callCount, p.bytes, p.expandedBytes, len(p.finished), len(p.names))
}

func familyCompilerGuardExpandedQueryBytes(query familyCompilerGuardQuery) int {
	data, _ := json.Marshal(query.compilerContext())
	size := len(data) + familyCompilerGuardQueryReferenceBytes
	for _, name := range query.Names {
		size += len(name) + 4
	}
	for _, call := range query.Calls {
		size += len(call.Operator) + len(call.Operand) + 48
	}
	return size
}

// Retain one immutable descriptor across kinds and variants. This is called
// only when a new membership is needed, not for already answered observations.
func (p *familyCompilerGuardPipeline) internContext(query familyCompilerGuardQuery) (familyCompilerGuardQuery, error) {
	context := query.compilerContext()
	if err := context.validate(); err != nil {
		return familyCompilerGuardQuery{}, err
	}
	id := context.id()
	data, _ := json.Marshal(context)
	if p.contexts == nil {
		p.contexts = map[string]familyCompilerGuardContext{}
	}
	retained, found := p.contexts[id]
	if found && !equalFamilyCompilerGuardContexts(retained, context) {
		return familyCompilerGuardQuery{}, fmt.Errorf("compiler guard context ID has contradictory contents")
	}
	if !found {
		context.Arguments = slices.Clone(context.Arguments)
		context.TranslationUnits = slices.Clone(context.TranslationUnits)
		context.Environment = maps.Clone(context.Environment)
		p.contexts[id] = context
		retained = context
		p.bytes += len(data) + len(id) + 4
	}
	p.count++
	p.bytes += familyCompilerGuardQueryReferenceBytes
	p.expandedBytes += len(data) + familyCompilerGuardQueryReferenceBytes
	return retained.bind(query), nil
}

func (p *familyCompilerGuardPipeline) retainQuery(query familyCompilerGuardQuery) bool {
	key := query.payloadKey()
	if _, found := p.uniqueQueries[key]; found {
		return true
	}
	if p.exceedsLimit("unique_query_limit", len(p.uniqueQueries)+1, maxFamilyCompilerGuardQueries) {
		return false
	}
	if p.uniqueQueries == nil {
		p.uniqueQueries = map[string]struct{}{}
	}
	p.uniqueQueries[key] = struct{}{}
	return true
}

func (p *familyCompilerGuardPipeline) truncate(reason string, current, maximum int) {
	if p.truncated {
		return
	}
	p.truncated = true
	p.active = nil
	p.limitReason, p.limitCurrent, p.limitMax = reason, current, maximum
	p.reportDiagnostics("limit")
}

func (p *familyCompilerGuardPipeline) exceedsLimit(reason string, current, maximum int) bool {
	if current <= maximum {
		return false
	}
	p.truncate(reason, current, maximum)
	return true
}

func (p *familyCompilerGuardPipeline) observe(value kconfig.ConfigDependencyCompilerGuardObservation) error {
	if value.OptionalDefinedness && value.OptionalTokenHints || value.LiteralIncludeHints && !value.OptionalTokenHints {
		return fmt.Errorf("compiler observation mixes demanded and hinted names")
	}
	if p.truncated {
		return nil
	}
	if value.OptionalDefinedness {
		return p.observeOptionalDefinedness(value)
	}
	if value.OptionalTokenHints {
		if value.LiteralIncludeHints {
			return p.observeOptionalStage(value, &p.literalHints)
		}
		return p.observeOptionalStage(value, &p.tokenHints)
	}
	if value.Truncated {
		// The scanner does not expose which source bound failed or its counts.
		p.truncate("source_hint_limit", 0, 0)
		return nil
	}
	if len(value.Calls) != 0 {
		if len(value.Names) != 0 || value.OptionalDefinedness {
			return fmt.Errorf("compiler guard observation mixes definedness and intrinsic calls")
		}
		return p.observeIntrinsicCalls(value)
	}
	return p.observeDefinedness(value)
}

func (p *familyCompilerGuardPipeline) observeDefinedness(value kconfig.ConfigDependencyCompilerGuardObservation) error {
	query := familyCompilerGuardQuery{Scope: value.Scope, Role: value.Role, Language: value.Language, Arguments: value.Arguments, TranslationUnits: value.TranslationUnits, Environment: value.Environment}
	if value.OptionalDefinedness {
		if len(value.Names) == 0 {
			return fmt.Errorf("optional compiler definedness observation has no expansion demand")
		}
		query.Kind = familyCompilerOptionalDefinednessQueryKind
	}
	if value.OptionalTokenHints {
		query.Kind = familyCompilerTokenHintQueryKind
	}
	if value.LiteralIncludeHints {
		query.Kind = familyCompilerLiteralHintQueryKind
	}
	if err := query.compilerContext().validate(); err != nil {
		return err
	}
	key := query.contextKey()
	if p.priority == nil {
		p.priority = &familyCompilerGuardPriorityIndex{}
	}
	optional := value.OptionalDefinedness || value.OptionalTokenHints
	var pendingKey [32]byte
	if optional {
		pendingKey = familyCompilerGuardSchedulingKey(query)
	}
	// A rejected prior vector must not reserve names before its exact-vector
	// rejection is applied at finish. Fall back to original scheduling for its
	// class, allowing a separate singleton or raw context to be attempted.
	deduplicate := optional && !p.priority.truncated &&
		!p.priority.rejected[familyCompilerGuardPendingClass{query.Kind, pendingKey}]
	for _, name := range value.Names {
		if p.known[key][name] || deduplicate && p.pendingNames[pendingKey][name] {
			continue
		}
		if p.known[key] == nil {
			p.priority.record(key, query)
			p.known[key] = map[string]bool{}
		}
		p.known[key][name] = true
		entry := p.active[key]
		if entry == nil {
			var err error
			query, err = p.internContext(query)
			if err != nil {
				return err
			}
			entry = &query
			p.active[key] = entry
		}
		entry.Names = append(entry.Names, name)
		p.nameCount++
		p.bytes += len(name) + 4
		p.expandedBytes += len(name) + 4
		if p.queryBytes == nil {
			p.queryBytes = map[string]int{}
		}
		// Reserve more than the definedness protocol's directive framing per
		// name, so optional discovery cannot produce an oversized probe stdin.
		p.queryBytes[key] += len(name) + 64
		if p.exceedsLimit("query_membership_limit", p.count, maxFamilyCompilerGuardMemberships) ||
			p.exceedsLimit("context_limit", len(p.contexts), maxFamilyCompilerGuardQueries) ||
			p.exceedsLimit("name_value_limit", p.nameCount, 1<<20) ||
			p.exceedsLimit("estimated_bytes_limit", p.bytes, maxFamilyCompilerGuardBytes/2) ||
			p.exceedsLimit("expanded_bytes_limit", p.expandedBytes, maxFamilyCompilerGuardExpandedBytes) ||
			p.exceedsLimit("query_name_limit", len(entry.Names), 4096) ||
			p.exceedsLimit("query_stdin_limit", p.queryBytes[key], kconfig.MaxProbeInterpolatedBytes) {
			return nil
		}
		if deduplicate {
			if p.pendingNames == nil {
				p.pendingNames = map[[32]byte]map[string]bool{}
			}
			if p.pendingNames[pendingKey] == nil {
				p.pendingNames[pendingKey] = map[string]bool{}
			}
			// Every reservation owns an already-counted active name; classes
			// cannot outnumber its already-bounded original compiler contexts.
			p.pendingNames[pendingKey][name] = true
		}
	}
	return nil
}

func (p *familyCompilerGuardPipeline) finishVariant(name string, scopes *kconfig.KbuildProbeScopes) error {
	if !p.prepared[name] || p.finished[name] || p.activeVariant != name {
		return fmt.Errorf("compiler guard variant %q has no unique completed scan", name)
	}
	p.finished[name] = true
	p.activeVariant = ""
	if p.flags.manifestOut == "" {
		return nil
	}
	batch, err := kconfig.NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		return err
	}
	if !p.truncated {
		for _, key := range slices.Sorted(maps.Keys(p.active)) {
			query := *p.active[key]
			slices.Sort(query.Names)
			slices.SortFunc(query.Calls, compareFamilyCompilerIntrinsicCalls)
			if err := query.validatePayload(); err != nil {
				return err
			}
			if !p.retainQuery(query) {
				break
			}
			if _, err := query.evaluate(batch); err != nil {
				return err
			}
			p.queries[name] = append(p.queries[name], query)
		}
	}
	if _, found := p.queries[name]; !found {
		p.queries[name] = nil
	}
	plan, err := batch.Plan()
	if err != nil {
		return err
	}
	p.plans = append(p.plans, kconfig.ProbePlanVariant{Name: name, Plan: plan})
	p.filterTokenHintDemands()
	if err := p.finishOptionalVariant(name, scopes, p.optional); err != nil {
		return err
	}
	if err := p.finishOptionalVariant(name, scopes, p.tokenHints); err != nil {
		return err
	}
	if err := p.finishOptionalVariant(name, scopes, p.literalHints); err != nil {
		return err
	}
	p.active, p.known = nil, nil
	p.rejected = nil
	p.priority = nil
	return nil
}

func (p *familyCompilerGuardPipeline) publish() error {
	if p == nil {
		return nil
	}
	if len(p.finished) != len(p.names) {
		return fmt.Errorf("compiler guard round did not evaluate every family variant")
	}
	// Validate exact frozen union membership, not just successful lookups in an
	// oracle (which intentionally permits unused results for ordinary Kbuild).
	for index, round := range p.prior {
		plan, err := kconfig.MergeProbePlans(round.replayed)
		if err != nil {
			return err
		}
		id, err := familyCompilerGuardPlanID(plan)
		if err != nil {
			return err
		}
		if id != round.manifest.PlanID {
			return fmt.Errorf("compiler guard round %d replay changed its exact frozen membership", index)
		}
	}
	if p.flags.manifestOut == "" {
		return nil
	}
	if !p.truncated {
		if err := p.admitOptionalQueries(); err != nil {
			return err
		}
		if err := p.admitOptionalStage(p.tokenHints); err != nil {
			return err
		}
		p.tokenHints = nil
		if err := p.admitOptionalStage(p.literalHints); err != nil {
			return err
		}
		p.literalHints = nil
	}
	if p.truncated {
		// One overflow invalidates the entire new frontier, including already
		// scanned siblings. Previously completed rounds remain valid facts.
		builder, err := kconfig.NewProbePlanBuilder(p.toolsets["target"], p.toolsets["host"])
		if err != nil {
			return err
		}
		empty, err := builder.Plan()
		if err != nil {
			return err
		}
		p.plans = nil
		for _, name := range p.names {
			p.queries[name] = nil
			p.plans = append(p.plans, kconfig.ProbePlanVariant{Name: name, Plan: empty})
		}
	}
	plan, err := kconfig.MergeProbePlans(p.plans)
	if err != nil {
		return err
	}
	if err := p.writeRound(plan, p.queries, p.truncated); err != nil {
		return err
	}
	p.reportDiagnostics("published")
	return nil
}

// A separate staging ledger uses the existing descriptor limits; retained
// frozen plans additionally share a fixed 32 MiB encoded-size/32768-node cap.
// These are bounds on retained diagnostic work, not total process heap. Neither
// staging nor omission consumes or enlarges the emitted frontier's budgets.
func (p *familyCompilerGuardPipeline) observeOptionalDefinedness(value kconfig.ConfigDependencyCompilerGuardObservation) error {
	return p.observeOptionalStage(value, &p.optional)
}

// Do not let a weaker vector poison a mandatory or actually requested name's
// attempt. Finish both observations first, then subtract stronger names before
// freezing either plan. Staging accounting stays conservative after removal.
func (p *familyCompilerGuardPipeline) filterTokenHintDemands() {
	p.filterHintDemands(p.tokenHints, false)
	p.filterHintDemands(p.literalHints, true)
}

// An entered-file hint remains stronger than a speculative literal candidate,
// including when the two original contexts differ only in projected -D values.
// The scheduling index is not an answer and survives discarded weaker stages.
func (p *familyCompilerGuardPipeline) filterHintDemands(stage *familyCompilerGuardOptionalStage, includeEntered bool) {
	if stage == nil || stage.disabled {
		return
	}
	if p.priority != nil && p.priority.truncated {
		// A bounded scheduling index may lose hints, never stronger queries.
		stage.disable("priority_context_limit")
		return
	}
	// Index only the already-bounded weak candidates. Borrow their names for
	// this filtering pass, without accumulating another global namespace.
	wanted := map[[32]byte]map[string]bool{}
	for _, query := range stage.ledger.active {
		key := familyCompilerGuardSchedulingKey(*query)
		if wanted[key] == nil {
			wanted[key] = map[string]bool{}
		}
		for _, name := range query.Names {
			wanted[key][name] = true
		}
	}
	if p.priority != nil {
		for original, key := range p.priority.contexts {
			if !includeEntered && strings.HasPrefix(original, familyCompilerTokenHintQueryKind+":") {
				continue
			}
			if names := wanted[key]; names != nil {
				for name := range p.known[original] {
					delete(names, name)
				}
			}
		}
	}
	for key, query := range stage.ledger.active {
		strong := map[string]bool{}
		context := query.compilerContext().id()
		for name := range p.known[":"+context] {
			strong[name] = true
		}
		for name := range p.known[familyCompilerOptionalDefinednessQueryKind+":"+context] {
			strong[name] = true
		}
		if includeEntered {
			for name := range p.known[familyCompilerTokenHintQueryKind+":"+context] {
				strong[name] = true
			}
		}
		remaining := wanted[familyCompilerGuardSchedulingKey(*query)]
		query.Names = slices.DeleteFunc(query.Names, func(name string) bool { return strong[name] || !remaining[name] })
		if len(query.Names) == 0 {
			delete(stage.ledger.active, key)
		}
	}
}

func (p *familyCompilerGuardPipeline) observeOptionalStage(value kconfig.ConfigDependencyCompilerGuardObservation, destination **familyCompilerGuardOptionalStage) error {
	if len(value.Calls) != 0 || value.Truncated && len(value.Names) != 0 || !value.Truncated && len(value.Names) == 0 {
		return fmt.Errorf("optional compiler definedness observation has an invalid demand payload")
	}
	if *destination == nil {
		if p.priority == nil {
			p.priority = &familyCompilerGuardPriorityIndex{}
		}
		kind := familyCompilerOptionalDefinednessQueryKind
		if value.OptionalTokenHints {
			kind = familyCompilerTokenHintQueryKind
		}
		if value.LiteralIncludeHints {
			kind = familyCompilerLiteralHintQueryKind
		}
		*destination = &familyCompilerGuardOptionalStage{kind: kind, ledger: familyCompilerGuardPipeline{
			active: map[string]*familyCompilerGuardQuery{}, known: p.known, queryBytes: map[string]int{},
			priority: p.priority,
		}}
	}
	stage := *destination
	if stage.disabled {
		return nil
	}
	if value.Truncated {
		stage.disable("source_hint_limit")
		return nil
	}
	if err := stage.ledger.observeDefinedness(value); err != nil {
		return err
	}
	if stage.ledger.truncated {
		stage.disable("staging_" + stage.ledger.limitReason)
	}
	return nil
}

func (stage *familyCompilerGuardOptionalStage) omit(reason string) {
	stage.omitted++
	if stage.reason == "" {
		stage.reason = reason
	}
}

func (stage *familyCompilerGuardOptionalStage) disable(reason string) {
	stage.omit(reason)
	stage.disabled = true
	stage.ledger = familyCompilerGuardPipeline{}
	stage.plan = nil
	stage.variants = nil
	stage.planBytes, stage.planNodes = 0, 0
}

func (p *familyCompilerGuardPipeline) finishOptionalVariant(name string, scopes *kconfig.KbuildProbeScopes, stage *familyCompilerGuardOptionalStage) error {
	if stage == nil {
		return nil
	}
	defer func() {
		stage.ledger.active, stage.ledger.known, stage.ledger.queryBytes = nil, nil, nil
		stage.ledger.pendingNames, stage.ledger.priority = nil, nil
	}()
	if p.truncated || stage.disabled || len(stage.ledger.active) == 0 {
		return nil
	}
	batch, err := kconfig.NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		return err
	}
	variant := familyCompilerGuardOptionalVariant{name: name}
	for _, key := range slices.Sorted(maps.Keys(stage.ledger.active)) {
		query := *stage.ledger.active[key]
		slices.Sort(query.Names)
		if err := query.validatePayload(); err != nil {
			return err
		}
		// Rejection is authenticated against current ordinary dependencies in
		// prepareVariant, and binds this exact residual vector, not its names.
		if p.rejected[query.payloadKey()] {
			continue
		}
		if !stage.ledger.retainQuery(query) {
			stage.disable("staging_unique_query_limit")
			return nil
		}
		_, state, reference, err := batch.OptionalCompilerDefinednessAttempt(query.Scope, query.Role, query.Language, query.Arguments, query.TranslationUnits, query.Names, query.Environment)
		if err != nil {
			var limit *kconfig.CompilerGuardDependencyLimitError
			if errors.As(err, &limit) {
				// This batch has no oracle and supplies no facts. Discard every
				// staged optional candidate; never catch this in prior replay.
				stage.disable("staging_dependency_limit")
				return nil
			}
			return err
		}
		if state != kconfig.OptionalCompilerDefinednessPending || reference.NodeID == "" {
			return fmt.Errorf("optional discovery did not register its exact pending terminal")
		}
		variant.queries = append(variant.queries, familyCompilerGuardOptionalQuery{query: query, terminal: reference.NodeID})
	}
	if len(variant.queries) == 0 {
		return nil
	}
	plan, err := batch.Plan()
	if err != nil {
		return err
	}
	variant.terminals = slices.Clone(plan.Terminal)
	// Bound the incoming detached plan before merging it with retained work.
	// This also bounds temporary union construction independently of deduplication.
	if len(plan.Nodes) > maxFamilyCompilerGuardMemberships {
		stage.disable("staging_plan_limit")
		return nil
	}
	size, fits := familyCompilerGuardOptionalPlanBytes(plan, maxFamilyCompilerGuardBytes/2)
	if !fits {
		stage.disable("staging_plan_limit")
		return nil
	}
	previousSize, previousNodes := 0, 0
	if stage.plan != nil {
		previousSize, fits = familyCompilerGuardOptionalPlanBytes(stage.plan, maxFamilyCompilerGuardBytes/2)
		if !fits {
			return fmt.Errorf("retained optional compiler plan exceeds its storage bound")
		}
		previousNodes = len(stage.plan.Nodes)
		plan, err = kconfig.MergeProbePlans([]kconfig.ProbePlanVariant{
			{Name: "retained", Plan: stage.plan}, {Name: "incoming", Plan: plan},
		})
		if err != nil {
			return err
		}
		size, fits = familyCompilerGuardOptionalPlanBytes(plan, maxFamilyCompilerGuardBytes/2)
	}
	if !fits || size-previousSize > maxFamilyCompilerGuardBytes/2-stage.planBytes ||
		len(plan.Nodes)-previousNodes > maxFamilyCompilerGuardMemberships-stage.planNodes {
		stage.disable("staging_plan_limit")
		return nil
	}
	if size < previousSize || len(plan.Nodes) < previousNodes {
		return fmt.Errorf("optional compiler plan union lost retained storage")
	}
	stage.planBytes += size - previousSize
	stage.planNodes += len(plan.Nodes) - previousNodes
	stage.plan = plan
	stage.variants = append(stage.variants, variant)
	p.enforceOptionalStagingBudget()
	return nil
}

// All optional tiers share the original cap, across all variants. Reconsider
// them strongest-first whenever a later sibling adds work: literal candidates
// may be discarded, but can never evict an entered-file hint or actual demand.
func (p *familyCompilerGuardPipeline) enforceOptionalStagingBudget() {
	bytes, nodes := 0, 0
	for _, stage := range []*familyCompilerGuardOptionalStage{p.optional, p.tokenHints, p.literalHints} {
		if stage == nil || stage.disabled {
			continue
		}
		if stage.planBytes > maxFamilyCompilerGuardBytes/2-bytes || stage.planNodes > maxFamilyCompilerGuardMemberships-nodes {
			stage.disable("staging_plan_limit")
			continue
		}
		bytes += stage.planBytes
		nodes += stage.planNodes
	}
}

// Count each already-bounded request separately instead of allocating a second
// serialized copy of the full candidate plan. Include conservative JSON map,
// slice and key framing. No plan pointer is retained after a failed check.
func familyCompilerGuardOptionalPlanBytes(plan *kconfig.ProbePlan, remaining int) (int, bool) {
	size := 128
	add := func(value any, framing int) bool {
		encoded, err := json.Marshal(value)
		if err != nil || len(encoded)+framing > remaining-size {
			return false
		}
		size += len(encoded) + framing
		return true
	}
	if size > remaining || !add(plan.Toolsets, 16) {
		return 0, false
	}
	for id, request := range plan.Requests {
		if !add(request, len(id)+8) {
			return 0, false
		}
	}
	for _, node := range plan.Nodes {
		if !add(node, 2) {
			return 0, false
		}
	}
	for _, terminal := range plan.Terminal {
		if !add(terminal, 2) {
			return 0, false
		}
	}
	return size, true
}

// Preflight one COMPLETE optional membership against the final baseline
// ledger. Every check precedes mutation; a rejection changes only diagnostics.
// Sharing a descriptor saves its storage once, never membership or replay work.
func (p *familyCompilerGuardPipeline) admitOptionalQuery(query familyCompilerGuardQuery) (familyCompilerGuardQuery, bool, error) {
	return p.admitOptionalQueryForStage(query, p.optional)
}

func (p *familyCompilerGuardPipeline) admitOptionalQueryForStage(query familyCompilerGuardQuery, stage *familyCompilerGuardOptionalStage) (familyCompilerGuardQuery, bool, error) {
	if query.Kind != familyCompilerOptionalDefinednessQueryKind && query.Kind != familyCompilerTokenHintQueryKind && query.Kind != familyCompilerLiteralHintQueryKind {
		return familyCompilerGuardQuery{}, false, fmt.Errorf("optional admission received a baseline query")
	}
	if err := query.validatePayload(); err != nil {
		return familyCompilerGuardQuery{}, false, err
	}
	context := query.compilerContext()
	if err := context.validate(); err != nil {
		return familyCompilerGuardQuery{}, false, err
	}
	id := context.id()
	retained, found := p.contexts[id]
	if found && !equalFamilyCompilerGuardContexts(retained, context) {
		return familyCompilerGuardQuery{}, false, fmt.Errorf("optional compiler context ID has contradictory contents")
	}
	data, _ := json.Marshal(context)
	compact, expanded := familyCompilerGuardQueryReferenceBytes, len(data)+familyCompilerGuardQueryReferenceBytes
	contexts, unique := len(p.contexts), len(p.uniqueQueries)
	if !found {
		contexts++
		compact += len(data) + len(id) + 4
	}
	key := query.payloadKey()
	if _, found := p.uniqueQueries[key]; !found {
		unique++
	}
	stdin := 0
	for _, name := range query.Names {
		compact += len(name) + 4
		expanded += len(name) + 4
		stdin += len(name) + 64
	}
	for _, limit := range []struct {
		reason           string
		current, maximum int
	}{
		{"unique_query_limit", unique, maxFamilyCompilerGuardQueries},
		{"query_membership_limit", p.count + 1, maxFamilyCompilerGuardMemberships},
		{"context_limit", contexts, maxFamilyCompilerGuardQueries},
		{"name_value_limit", p.nameCount + len(query.Names), 1 << 20},
		{"intrinsic_value_limit", p.callCount, maxFamilyCompilerIntrinsicCalls},
		{"estimated_bytes_limit", p.bytes + compact, maxFamilyCompilerGuardBytes / 2},
		{"expanded_bytes_limit", p.expandedBytes + expanded, maxFamilyCompilerGuardExpandedBytes},
		{"query_name_limit", len(query.Names), 4096},
		{"query_stdin_limit", stdin, kconfig.MaxProbeInterpolatedBytes},
	} {
		if limit.current > limit.maximum {
			stage.omit(limit.reason)
			return familyCompilerGuardQuery{}, false, nil
		}
	}
	query, err := p.internContext(query)
	if err != nil {
		return familyCompilerGuardQuery{}, false, err
	}
	// internContext charged context and membership framing above. Only the
	// payload remains; retainQuery cannot truncate after identical preflight.
	for _, name := range query.Names {
		p.bytes += len(name) + 4
		p.expandedBytes += len(name) + 4
	}
	p.nameCount += len(query.Names)
	if p.uniqueQueries == nil {
		p.uniqueQueries = map[string]struct{}{}
	}
	p.uniqueQueries[key] = struct{}{}
	return query, true, nil
}

func (p *familyCompilerGuardPipeline) admitOptionalQueries() error {
	if err := p.admitOptionalStage(p.optional); err != nil {
		return err
	}
	p.optional = nil
	return nil
}

func (p *familyCompilerGuardPipeline) admitOptionalStage(stage *familyCompilerGuardOptionalStage) error {
	if stage == nil {
		return nil
	}
	// All variants' baseline queries and all previous-round replay checks have
	// finished before any admission, including a later sibling's last slot.
	slices.SortFunc(stage.variants, func(a, b familyCompilerGuardOptionalVariant) int { return strings.Compare(a.name, b.name) })
	for _, variant := range stage.variants {
		allowed := make(map[string]bool, len(variant.terminals))
		for _, terminal := range variant.terminals {
			allowed[terminal] = true
		}
		var terminals []string
		for _, candidate := range variant.queries {
			if !allowed[candidate.terminal] {
				return fmt.Errorf("optional compiler query selects a terminal outside its original variant")
			}
			query, admitted, err := p.admitOptionalQueryForStage(candidate.query, stage)
			if err != nil {
				return err
			}
			if admitted {
				p.queries[variant.name] = append(p.queries[variant.name], query)
				terminals = append(terminals, candidate.terminal)
			}
		}
		selected, err := kconfig.SelectProbePlanTerminals(stage.plan, terminals)
		if err != nil {
			return err
		}
		// Selecting the exact closure removes both omitted terminal requests
		// and dependency nodes not reachable from any admitted query.
		index := slices.IndexFunc(p.plans, func(plan kconfig.ProbePlanVariant) bool { return plan.Name == variant.name })
		if index < 0 {
			return fmt.Errorf("optional admission has no completed baseline variant")
		}
		merged, err := kconfig.MergeProbePlans([]kconfig.ProbePlanVariant{{Name: "baseline", Plan: p.plans[index].Plan}, {Name: "optional", Plan: selected}})
		if err != nil {
			return err
		}
		p.plans[index].Plan = merged
		slices.SortFunc(p.queries[variant.name], func(a, b familyCompilerGuardQuery) int { return strings.Compare(a.contextKey(), b.contextKey()) })
	}
	if stage.omitted != 0 && p.diagnostics != nil {
		_, _ = fmt.Fprintf(p.diagnostics, "compiler_guard_optional round=%d event=omitted reason=%s omission_events=%d staging_discarded=%t tier=%s\n", len(p.prior), stage.reason, stage.omitted, stage.disabled, stage.kind)
	}
	return nil
}

// Only serialization is shared with the optional empty-round path. The normal
// publish path above must first verify every variant and exact prior union;
// carrying empty work must never manufacture that replay evidence.
func (p *familyCompilerGuardPipeline) writeRound(plan *kconfig.ProbePlan, queries map[string][]familyCompilerGuardQuery, truncated bool) error {
	id, err := familyCompilerGuardPlanID(plan)
	if err != nil {
		return err
	}
	manifest := familyCompilerGuardManifest{Schema: familyCompilerGuardSchema, Round: len(p.prior), Previous: p.previous, Toolsets: p.toolsets, Variants: queries, PlanID: id, Truncated: truncated}
	data, err := marshalFamilyCompilerGuardManifest(manifest)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > maxFamilyCompilerGuardBytes {
		return fmt.Errorf("compiler guard manifest exceeds its byte budget")
	}
	if err := plan.Write(p.flags.planOut); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.flags.manifestOut), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(p.flags.manifestOut, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
