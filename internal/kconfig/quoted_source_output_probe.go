package kconfig

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

// selectedSourceOutputProbeLookup keeps an earlier measured source-output plan
// with its independently validated results. A selected writer must match its
// exact terminal, request, scope and inputs before looking up any result.
type selectedSourceOutputProbeLookup struct {
	ProbeResultLookup
	plan *ProbePlan
}

// NewSelectedSourceOutputProbeLookup binds a previously validated source
// output Plan to its result oracle for ordinary Kbuild replay. Discovery does
// not inspect either Plan or result oracle.
func NewSelectedSourceOutputProbeLookup(plan *ProbePlan, results ProbeResultLookup) ProbeResultLookup {
	return &selectedSourceOutputProbeLookup{ProbeResultLookup: results, plan: plan}
}

func (lookup *selectedSourceOutputProbeLookup) verifySelectedWriter(
	target string, selected ProbeReference, request ProbeRequest, dependencies []ProbeReference,
) error {
	if lookup == nil || lookup.plan == nil || lookup.ProbeResultLookup == nil {
		return fmt.Errorf("selected source writer %s request %s has no measured source output plan/results", target, selected.NodeID)
	}
	if !slices.Contains(lookup.plan.Terminal, selected.NodeID) {
		return fmt.Errorf("selected source writer %s request %s is absent from measured source output terminals", target, selected.NodeID)
	}
	currentID, err := request.ID()
	if err != nil {
		return fmt.Errorf("selected source writer %s request %s: %w", target, selected.NodeID, err)
	}
	measuredRequest, exists := lookup.plan.Requests[selected.RequestID]
	if !exists {
		return fmt.Errorf("selected source writer %s request %s has no measured request %s", target, selected.NodeID, selected.RequestID)
	}
	measuredID, err := measuredRequest.ID()
	if err != nil {
		return fmt.Errorf("selected source writer %s measured request %s: %w", target, selected.RequestID, err)
	}
	if currentID != selected.RequestID || measuredID != selected.RequestID || selected.Kind != request.Outcome.Kind ||
		measuredRequest.Outcome.Kind != selected.Kind {
		return fmt.Errorf("selected source writer %s request %s differs from its measured request identity", target, selected.NodeID)
	}
	inputIDs := make([]string, len(dependencies))
	for index, dependency := range dependencies {
		inputIDs[index] = dependency.NodeID
	}
	found := false
	for _, node := range lookup.plan.Nodes {
		if node.ID != selected.NodeID {
			continue
		}
		if found || node.Scope != selected.Scope || node.RequestID != selected.RequestID ||
			!slices.Equal(node.Inputs, inputIDs) || node.ContentID() != selected.NodeID {
			return fmt.Errorf("selected source writer %s request %s differs from its measured node provenance", target, selected.NodeID)
		}
		found = true
	}
	if !found {
		return fmt.Errorf("selected source writer %s request %s has no measured source output node", target, selected.NodeID)
	}
	return nil
}

func (lookup *selectedSourceOutputProbeLookup) selectedWriterMeasured(reference ProbeReference) bool {
	return lookup != nil && lookup.plan != nil && slices.Contains(lookup.plan.Terminal, reference.NodeID)
}

// compactKbuildSelectedSourceFilechkRecipe accepts one source-owned shell
// program as the direct payload of a selected filechk, whether Make invokes
// it as a script or inside an echo command substitution. The source-selected
// recipe and immutable script are the measured program; execution binds the
// direct script's shebang to the declared shell runtime.
func compactKbuildSelectedSourceFilechkRecipe(recipe, target string) (selected, script string, direct bool) {
	if target == "" || canonicalKbuildRulePath(target) != target || validateProbeSourcePath(target) != nil {
		return "", "", false
	}
	recipe = strings.TrimSpace(recipe)
	const prefix = "{\n"
	suffix := "\n} > " + shellSingleQuoted(target)
	if !strings.HasPrefix(recipe, prefix) || !strings.HasSuffix(recipe, suffix) {
		return "", "", false
	}
	payload := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(recipe, prefix), suffix))
	if nested, ok := compactKbuildQuotedSourceScriptFilechk(payload); ok {
		return recipe, nested, false
	}
	words, err := lexCompactKbuildRecipe(payload)
	if err != nil || len(words) != 2 || words[0].operator || words[1].operator ||
		words[1].value != "${tree:kernel}" {
		return "", "", false
	}
	script, ok := strings.CutPrefix(words[0].value, "${tree:kernel}/")
	if !ok || script == "" || script != canonicalKbuildRulePath(script) ||
		validateProbeSourcePath(script) != nil || strings.ContainsAny(script, "$`\\*?[~;|&<>() \t\r\n") {
		return "", "", false
	}
	return "{\n${tree:kernel}/" + script + " ${tree:kernel}\n} > " + shellSingleQuoted(target), script, true
}

// SelectedSourceFilechkOutputText selects and measures a source-owned filechk
// in the target stage. `measured`
// is an optional, independently sealed earlier probe oracle. Discovery always
// registers requests in its current builder. The optional discoveryOnly mode
// registers selected requests without consulting either result oracle: an
// earlier Kconfig result may exist before the source output has been measured.
// Normal replay reads only this selected request's own result identity.
// selected contains the actual returned request references even when the
// discovery builder had registered an identical request earlier.
func (s *KbuildProbeScopes) SelectedSourceFilechkOutputText(
	target, recipe string,
	workingTreeContents map[string]string,
	prewriterObjectNames []string,
	prewriterObjectFiles, prewriterObjectOwners map[string]string,
	measured ProbeResultLookup,
	discoveryOnly ...bool,
) (text string, concrete, recognized bool, selected []ProbeReference, err error) {
	if len(discoveryOnly) > 1 {
		return "", false, false, nil, fmt.Errorf("selected source output accepts only one discovery mode")
	}
	registerOnly := len(discoveryOnly) == 1 && discoveryOnly[0]
	if s == nil {
		return "", false, false, nil, fmt.Errorf("Kbuild probe scopes are nil")
	}
	// A selected Make target is executed in its target stage. Target probes can
	// depend on both target and host compiler results; host probes cannot adopt
	// target-scoped exports. One exact target request is its producer identity.
	value, concrete, recognized, reference, err := s.selectedSourceFilechkOutputText(
		"target", target, recipe, workingTreeContents,
		prewriterObjectNames, prewriterObjectFiles, prewriterObjectOwners, measured, registerOnly,
	)
	if err != nil || !recognized {
		return "", false, recognized, nil, err
	}
	return value, concrete, true, []ProbeReference{reference}, nil
}

func (s *KbuildProbeScopes) selectedSourceFilechkOutputText(
	scope, target, recipe string,
	workingTreeContents map[string]string,
	prewriterObjectNames []string,
	prewriterObjectFiles, prewriterObjectOwners map[string]string,
	measured ProbeResultLookup,
	discoveryOnly bool,
) (text string, concrete, recognized bool, selected ProbeReference, err error) {
	owner := s.evaluators[scope]
	if owner == nil {
		return "", false, false, ProbeReference{}, fmt.Errorf("Kbuild probe workload has no %s scope", scope)
	}
	evaluator := owner
	var selectedExports []*selectedSourceOutputExportContext
	if scope == "target" && s.activeSelectedSourceExports != nil {
		// Normal scope-specific probes cannot run host actions. The selected
		// filechk belongs to a target Make recipe and observes its entire export
		// snapshot, so construct a temporary evaluator for this request only.
		evaluator, err = owner.WithScriptEnvironment(s.activeSelectedSourceExports)
		if err != nil {
			return "", false, false, ProbeReference{}, fmt.Errorf("bind selected source exports: %w", err)
		}
		context := &selectedSourceOutputExportContext{}
		if host := s.evaluators["host"]; host != nil {
			context.hostTools = host.tools
			context.hostToolsetIdentity = host.facts.ToolsetIdentity()
		}
		selectedExports = append(selectedExports, context)
	}
	proof, staged, recognized, err := evaluator.selectedSourceFilechkProof(
		target, recipe, workingTreeContents,
		prewriterObjectNames, prewriterObjectFiles, prewriterObjectOwners,
	)
	if err != nil || !recognized {
		return "", false, recognized, ProbeReference{}, err
	}
	request, dependencies, recognized, err := evaluator.evaluatedScriptOutputTextRequestWithSourceProof(
		target, recipe, nil, staged, proof, selectedExports...,
	)
	if err != nil || !recognized {
		return "", false, recognized, ProbeReference{}, err
	}
	request = evaluator.canonicalSourceRequest(request)
	selected, err = evaluator.discovery.Request(scope, request, dependencies...)
	if err != nil {
		return "", false, true, ProbeReference{}, err
	}
	if err := evaluator.symbolRegistry.publishDefinition(selected, request, dependencies); err != nil {
		return "", false, true, ProbeReference{}, err
	}
	if !evaluator.seen[selected.NodeID] {
		evaluator.seen[selected.NodeID] = true
		evaluator.references = append(evaluator.references, selected)
	}
	if evaluator != owner && !owner.seen[selected.NodeID] {
		// The temporary evaluator shares the registry but owns a cloned list of
		// terminals. Publish its selected request to the persistent workload so
		// EvaluateKbuildProbeWorkload includes this exact producer in the plan.
		owner.seen[selected.NodeID] = true
		owner.references = append(owner.references, selected)
	}
	if discoveryOnly {
		// A later discovery round can advance past only a previously selected
		// and independently sealed writer. A new request remains the next
		// causal boundary; ordinary replay still requires every writer to be
		// an exact terminal of the final union plan.
		lookup, measuredPrior := measured.(*selectedSourceOutputProbeLookup)
		if !measuredPrior || !lookup.selectedWriterMeasured(selected) {
			return "", false, true, selected, nil
		}
	}
	if _, bare := measured.(*ProbeResultOracle); bare {
		return "", false, true, selected, fmt.Errorf("selected source writer %s request %s has a measured result oracle without its source output plan", target, selected.NodeID)
	}
	if lookup, sealed := measured.(*selectedSourceOutputProbeLookup); sealed {
		if err := lookup.verifySelectedWriter(target, selected, request, dependencies); err != nil {
			return "", false, true, selected, err
		}
	}
	oracle := measured
	if oracle == nil {
		oracle = evaluator.oracle
	}
	if oracle == nil {
		return "", false, true, selected, nil
	}
	// readText verifies result request identity, outcome step success and exact
	// reduced text. The temporary reader shares no mutable evaluator state.
	reader := &LinuxProbeEvaluator{oracle: oracle}
	envelope, err := reader.readText(selected, request, dependencies...)
	if err != nil {
		return "", false, true, selected, err
	}
	if envelope == linuxProbeEvaluatedScriptUnsafeOutput {
		return "", true, false, selected, nil
	}
	if !strings.HasPrefix(envelope, linuxProbeEvaluatedScriptSafePrefix) {
		return "", false, true, selected, fmt.Errorf("selected source probe returned an invalid result envelope")
	}
	contents, err := base64.StdEncoding.DecodeString(strings.TrimSpace(strings.TrimPrefix(envelope, linuxProbeEvaluatedScriptSafePrefix)))
	if err != nil {
		return "", false, true, selected, fmt.Errorf("decode selected source result: %w", err)
	}
	return string(contents), true, true, selected, nil
}

func (e *LinuxProbeEvaluator) selectedSourceFilechkProof(
	target, recipe string,
	workingTreeContents map[string]string,
	prewriterObjectNames []string,
	prewriterObjectFiles, prewriterObjectOwners map[string]string,
) (proof *selectedSourceScriptProof, staged map[string]string, recognized bool, err error) {
	target = canonicalKbuildRulePath(target)
	_, script, direct := compactKbuildSelectedSourceFilechkRecipe(recipe, target)
	if script == "" {
		return nil, nil, false, nil
	}
	selectedScript, err := e.immutableLinuxSourcePath(script, true)
	if err != nil {
		return nil, nil, true, fmt.Errorf("selected filechk source script %q: %w", script, err)
	}
	script = selectedScript
	contents, err := os.ReadFile(filepath.Join(e.sourceRoot, filepath.FromSlash(script)))
	if err != nil {
		return nil, nil, true, fmt.Errorf("read selected filechk source script %q: %w", script, err)
	}
	if len(contents) > 1<<20 || strings.ContainsRune(string(contents), 0) {
		return nil, nil, false, nil
	}
	if direct {
		interpreter, selected := compactKbuildSourceShebangInterpreter(contents)
		if !selected || interpreter.program != "sh" || len(interpreter.arguments) != 0 {
			return nil, nil, false, nil
		}
	}
	// The immutable script and full source tree are declared action inputs.
	// Bound dynamic environment use, selected generated inputs, and mutable
	// filesystem effects as properties of the execution contract rather than
	// comparing the script to one historical kernel implementation.
	scan, err := scanCompactKbuildSourceScript(string(contents))
	if err != nil || scan.usage.ObservesAll || scan.dynamicDotSource || scan.unboundChildExecution || len(scan.programSources) != 0 {
		return nil, nil, false, nil
	}
	for _, source := range scan.sources {
		// The generated wrapper binds the selected script to the immutable
		// kernel tree. A nested helper named by the raw script has no such
		// runtime binding: it could resolve to a prior object-tree writer.
		// Only the exact config dot import below has authenticated bytes.
		if source != "include/config/auto.conf" || !slices.Contains(scan.dotSources, source) {
			return nil, nil, false, nil
		}
	}
	if !compactKbuildSourceLocalversionClosure(e.sourceRoot) ||
		!compactKbuildSourceSCMClosure(e.sourceRoot) {
		return nil, nil, false, nil
	}
	staged = make(map[string]string, len(workingTreeContents)+len(prewriterObjectNames))
	for candidate, content := range workingTreeContents {
		if candidate == target {
			// The measurement is at the prewriter frontier: never feed a
			// selected output into the action which produces those bytes.
			return nil, nil, false, nil
		}
		staged[candidate] = content
	}
	proof = &selectedSourceScriptProof{
		script: script, scriptDigest: fmt.Sprintf("%x", sha256.Sum256(contents)),
		direct: direct, owners: map[string]string{}, processRead: map[string]bool{},
	}
	for name := range scan.usage.Names {
		proof.processRead[name] = true
	}
	// Tool settings can alter the behavior of a selected child process even
	// when the shell itself never expands those names explicitly.
	for _, name := range []string{"IFS", "POSIXLY_CORRECT", "GREP_OPTIONS", "AWKPATH"} {
		proof.processRead[name] = true
	}
	seen := map[string]bool{}
	for _, candidate := range prewriterObjectNames {
		if candidate == "" || canonicalKbuildRulePath(candidate) != candidate || seen[candidate] {
			return nil, nil, false, nil
		}
		seen[candidate] = true
		if candidate == target {
			return nil, nil, false, nil
		}
		content, exact := prewriterObjectFiles[candidate]
		owner := prewriterObjectOwners[candidate]
		if !exact || owner == "" || len(owner) > 4096 || strings.ContainsRune(owner, 0) {
			// This output may never be read during Make evaluation. Preserve
			// its ordinary action when an earlier working artifact is opaque;
			// a later read of its bytes will fail at the consumer boundary.
			return nil, nil, false, nil
		}
		staged[candidate] = content
		proof.owners[candidate] = owner
	}
	for candidate := range prewriterObjectFiles {
		if !seen[candidate] {
			return nil, nil, false, nil
		}
	}
	if slices.Contains(scan.dotSources, "include/config/auto.conf") {
		content, exists := staged["include/config/auto.conf"]
		if !exists || toolaction.ValidateStaticConfigAssignments(content) != nil {
			return nil, nil, false, nil
		}
	}
	return proof, staged, true, nil
}

func compactKbuildSourceLocalversionPath(candidate string) bool {
	return candidate != "" && path.Dir(candidate) == "." && strings.HasPrefix(candidate, "localversion")
}

func compactKbuildSourceLocalversionClosure(sourceRoot string) bool {
	entries, err := os.ReadDir(sourceRoot)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !compactKbuildSourceLocalversionPath(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || entry.Type()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}

// A source-selected SCM query must not traverse a repository that happened to
// contain the declared source directory on the worker. The probe runner's
// private PATH is checked separately at execution time: a configured Git,
// Mercurial or SVN executable would require its own source-owned input proof.
func compactKbuildSourceSCMClosure(sourceRoot string) bool {
	root, err := filepath.EvalSymlinks(sourceRoot)
	if err != nil {
		return false
	}
	for directory := root; ; directory = filepath.Dir(directory) {
		for _, candidate := range []string{".scmversion", ".git", ".hg", ".svn"} {
			_, err := os.Lstat(filepath.Join(directory, candidate))
			if !os.IsNotExist(err) {
				return false
			}
		}
		if parent := filepath.Dir(directory); parent == directory {
			break
		}
	}
	return true
}
