package main

import (
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

// Execution metadata is transported separately from snapshot v3. Initial
// planning and replay retain the same original source/object/probe inputs.
type familyExecutionFlags struct {
	checkpointOut, checkpointIn                                                            string
	mode, cutOut, selectionOut, cutIn, pinnedOut, headersOut, artifactsOut, reuseReportOut string
	segments, initialSnapshots, stores                                                     namedPathFlag
	provided                                                                               map[string]bool
}

func (f *familyExecutionFlags) register(flags *flag.FlagSet) {
	f.provided = map[string]bool{}
	for _, field := range []struct {
		name, help string
		value      *string
	}{
		{"family_execution_checkpoint_out", "Initial lowered-plan/compiler checkpoint directory output", &f.checkpointOut},
		{"family_execution_checkpoint_in", "Replay/guard input: initial lowered-plan/compiler checkpoint directory", &f.checkpointIn},
		{"family_execution_mode", "Generated-header family execution phase: initial, replay or guards", &f.mode},
		{"family_execution_cut_out", "Initial complete-family execution-cut contract output", &f.cutOut},
		{"family_execution_selection_out", "Initial authenticated cut selection marker directory", &f.selectionOut},
		{"family_execution_cut_in", "Replay/guard input: initial complete-family execution-cut contract", &f.cutIn},
		{"family_execution_pinned_out", "Replay output: verified complete-cut copy-forward markers", &f.pinnedOut},
		{"family_execution_headers_out", "Replay output: authenticated immutable observed header contents", &f.headersOut},
		{"family_execution_artifacts_out", "Replay output: authenticated executed artifacts and modes", &f.artifactsOut},
		{"family_execution_reuse_report_out", "Replay output: final image-family reuse report", &f.reuseReportOut},
	} {
		flags.Func(field.name, field.help, func(value string) error {
			if f.provided[field.name] {
				return fmt.Errorf("-%s may be supplied only once", field.name)
			}
			f.provided[field.name] = true
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("-%s requires a nonempty value", field.name)
			}
			*field.value = value
			return nil
		})
	}
	for _, field := range []struct {
		name, help string
		value      *namedPathFlag
	}{
		{"family_execution_segment_out", "Execution plan output in SEGMENT=DIR form; requires all four segments", &f.segments},
		{"family_execution_initial_snapshot", "Replay/guard input snapshot in NAME=PATH form; requires every family variant", &f.initialSnapshots},
		{"family_execution_store", "Completed immutable cut output in TREE=DIR form; requires all ten stores", &f.stores},
	} {
		flags.Func(field.name, field.help, func(value string) error {
			f.provided[field.name] = true
			return field.value.Set(value)
		})
	}
}

func (f *familyExecutionFlags) requested() bool {
	return f != nil && (f.checkpointIn != "" || f.checkpointOut != "" || len(f.provided) != 0 || f.mode != "" || f.cutOut != "" || f.selectionOut != "" ||
		f.cutIn != "" || f.pinnedOut != "" || f.headersOut != "" || f.artifactsOut != "" || f.reuseReportOut != "" ||
		len(f.segments) != 0 || len(f.initialSnapshots) != 0 || len(f.stores) != 0)
}

type familyExecutionRequest struct {
	checkpointOut, checkpointIn                                                            string
	mode, cutOut, selectionOut, cutIn, pinnedOut, headersOut, artifactsOut, reuseReportOut string
	segments, initialSnapshots, stores                                                     map[string]string
	variants                                                                               []familyPlanVariantRequest
}

func familyExecutionNamedPaths(name string, values []namedPath, names map[string]bool) (map[string]string, error) {
	result := make(map[string]string, len(values))
	for _, value := range values {
		if !names[value.Name] {
			return nil, fmt.Errorf("-%s references unknown name %q", name, value.Name)
		}
		if _, exists := result[value.Name]; exists {
			return nil, fmt.Errorf("-%s repeats name %q", name, value.Name)
		}
		if strings.TrimSpace(value.Path) == "" {
			return nil, fmt.Errorf("-%s has an empty path for %q", name, value.Name)
		}
		result[value.Name] = workspacePath(value.Path)
	}
	for _, key := range slices.Sorted(maps.Keys(names)) {
		if result[key] == "" {
			return nil, fmt.Errorf("-%s is missing name %q", name, key)
		}
	}
	return result, nil
}

func (f *familyExecutionFlags) request(variants []familyPlanVariantRequest) (*familyExecutionRequest, error) {
	if !f.requested() {
		return nil, nil
	}
	if len(variants) == 0 {
		if f.mode == "guards" {
			return nil, fmt.Errorf("guard discovery requires -family_plan_variant")
		}
		return nil, fmt.Errorf("family execution requires -family_plan_variant and its complete outputs")
	}
	if f.mode != "initial" && f.mode != "replay" && f.mode != "guards" {
		return nil, fmt.Errorf("-family_execution_mode must be initial, replay or guards")
	}
	request := &familyExecutionRequest{
		checkpointOut: workspacePath(f.checkpointOut), checkpointIn: workspacePath(f.checkpointIn),
		mode: f.mode, cutOut: workspacePath(f.cutOut), selectionOut: workspacePath(f.selectionOut),
		cutIn: workspacePath(f.cutIn), pinnedOut: workspacePath(f.pinnedOut),
		headersOut: workspacePath(f.headersOut), artifactsOut: workspacePath(f.artifactsOut), reuseReportOut: workspacePath(f.reuseReportOut),
		variants: slices.Clone(variants),
	}
	var err error
	if f.mode == "guards" {
		if len(f.segments) != 0 || f.cutOut != "" || f.selectionOut != "" || f.pinnedOut != "" ||
			f.headersOut != "" || f.artifactsOut != "" || f.reuseReportOut != "" {
			return nil, fmt.Errorf("guard discovery does not accept family execution output flags")
		}
		for _, variant := range variants {
			for _, output := range familyExecutionVariantOutputs(variant) {
				if output != "" {
					return nil, fmt.Errorf("guard discovery does not accept family variant output fields for %s", variant.name)
				}
			}
		}
	} else {
		request.segments, err = familyExecutionNamedPaths("family_execution_segment_out", f.segments, kconfig.LinuxKernelFamilyPlanSegments)
		if err != nil {
			return nil, err
		}
	}
	if f.mode == "initial" {
		if strings.TrimSpace(f.cutOut) == "" || strings.TrimSpace(f.selectionOut) == "" {
			return nil, fmt.Errorf("initial family execution requires -family_execution_cut_out and -family_execution_selection_out")
		}
		if f.cutIn != "" || f.pinnedOut != "" || f.headersOut != "" || f.artifactsOut != "" || f.reuseReportOut != "" || len(f.initialSnapshots) != 0 || len(f.stores) != 0 {
			return nil, fmt.Errorf("initial family execution does not accept replay-only flags")
		}
	} else {
		if f.cutOut != "" || f.selectionOut != "" {
			return nil, fmt.Errorf("replay family execution does not accept initial-only flags")
		}
		if f.mode == "guards" && strings.TrimSpace(f.cutIn) == "" {
			return nil, fmt.Errorf("guard discovery requires -family_execution_cut_in")
		}
		if f.mode == "replay" && (strings.TrimSpace(f.cutIn) == "" || strings.TrimSpace(f.pinnedOut) == "" || strings.TrimSpace(f.headersOut) == "" || strings.TrimSpace(f.artifactsOut) == "" || strings.TrimSpace(f.reuseReportOut) == "") {
			return nil, fmt.Errorf("replay family execution requires -family_execution_cut_in, -family_execution_pinned_out, -family_execution_headers_out, -family_execution_artifacts_out and -family_execution_reuse_report_out")
		}
		names := make(map[string]bool, len(variants))
		for _, variant := range variants {
			names[variant.name] = true
		}
		request.initialSnapshots, err = familyExecutionNamedPaths("family_execution_initial_snapshot", f.initialSnapshots, names)
		if err != nil {
			return nil, err
		}
		request.stores, err = familyExecutionNamedPaths("family_execution_store", f.stores, kconfig.LinuxKernelPlanTrees)
		if err != nil {
			return nil, err
		}
	}
	if err := request.validatePaths(); err != nil {
		return nil, err
	}
	if f.mode == "initial" {
		if f.checkpointOut == "" {
			return nil, fmt.Errorf("initial family execution requires -family_execution_checkpoint_out")
		}
		if f.checkpointIn != "" {
			return nil, fmt.Errorf("initial family execution does not accept replay-only checkpoint input")
		}
	} else {
		if f.checkpointIn == "" {
			return nil, fmt.Errorf("family replay/guards require -family_execution_checkpoint_in")
		}
		if f.checkpointOut != "" {
			return nil, fmt.Errorf("family replay/guards do not accept initial-only checkpoint output")
		}
	}
	return request, nil
}

func familyExecutionVariantOutputs(variant familyPlanVariantRequest) map[string]string {
	return map[string]string{
		"arch": variant.arch, "snapshot": variant.snapshot,
		"config": variant.resolved.config, "auto.conf": variant.resolved.autoConf,
		"auto.conf.cmd": variant.resolved.autoConfCmd, "autoconf.h": variant.resolved.autoconf,
		"rustc_cfg": variant.resolved.rustcCfg, "kernel.release": variant.resolved.kernelRelease,
	}
}

// Prevent an output from replacing another phase's immutable input (or another
// output tree), including lexical parent/child aliases after workspace mapping.
func (r *familyExecutionRequest) validatePaths() error {
	outputs := map[string]string{}
	for segment, output := range r.segments {
		outputs["segment "+segment] = output
	}
	for name, output := range map[string]string{
		"checkpoint": r.checkpointOut,
		"cut":        r.cutOut, "selection": r.selectionOut, "pinned": r.pinnedOut,
		"headers": r.headersOut, "artifacts": r.artifactsOut, "reuse report": r.reuseReportOut,
	} {
		if output != "" {
			outputs[name] = output
		}
	}
	if r.mode != "guards" {
		for _, variant := range r.variants {
			for name, output := range familyExecutionVariantOutputs(variant) {
				outputs[variant.name+" "+name] = output
			}
		}
	}
	inputs := map[string]string{}
	if r.checkpointIn != "" {
		inputs["initial checkpoint"] = r.checkpointIn
	}
	if r.cutIn != "" {
		inputs["initial cut"] = r.cutIn
	}
	for variant, filename := range r.initialSnapshots {
		inputs["initial snapshot "+variant] = filename
	}
	for tree, directory := range r.stores {
		inputs["cut store "+tree] = directory
	}
	canonical := func(name, filename string) (string, error) {
		if strings.TrimSpace(filename) == "" {
			return "", fmt.Errorf("family execution %s path is empty", name)
		}
		path, err := filepath.Abs(filename)
		if err != nil {
			return "", fmt.Errorf("family execution %s path: %w", name, err)
		}
		if path == string(filepath.Separator) {
			return "", fmt.Errorf("family execution %s cannot use a filesystem root", name)
		}
		return path, nil
	}
	for name, path := range outputs {
		value, err := canonical(name, path)
		if err != nil {
			return err
		}
		outputs[name] = value
	}
	for name, path := range inputs {
		value, err := canonical(name, path)
		if err != nil {
			return err
		}
		inputs[name] = value
	}
	overlaps := func(a, b string) bool {
		return a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator))
	}
	names := slices.Sorted(maps.Keys(outputs))
	for index, name := range names {
		for _, previous := range names[:index] {
			if overlaps(outputs[name], outputs[previous]) {
				return fmt.Errorf("family execution outputs %s and %s overlap", name, previous)
			}
		}
		for _, input := range slices.Sorted(maps.Keys(inputs)) {
			if overlaps(outputs[name], inputs[input]) {
				return fmt.Errorf("family execution output %s overlaps %s", name, input)
			}
		}
	}
	return nil
}

type familyExecutionPipeline struct {
	request   *familyExecutionRequest
	initial   map[string]*kconfig.ActionPlanSnapshot
	cut       *kconfig.ActionPlanFamilyExecutionCut
	observed  *kconfig.ActionPlanFamilyObservedHeaders
	artifacts *kconfig.ActionPlanFamilyExecutedArtifacts
	variants  []kconfig.ActionPlanFamilyVariant
	demands   map[string]kconfig.ConfigDependencyGeneratedHeaderDemandCollection
	results   []*kconfig.ActionPlanFamilyVariantPlanningResult
	recorded  map[string]bool
}

func newFamilyExecutionPipeline(request *familyExecutionRequest) (*familyExecutionPipeline, error) {
	if request == nil {
		return nil, nil
	}
	result := &familyExecutionPipeline{
		request: request, initial: map[string]*kconfig.ActionPlanSnapshot{},
		demands: map[string]kconfig.ConfigDependencyGeneratedHeaderDemandCollection{}, recorded: map[string]bool{},
	}
	if request.mode == "initial" {
		return result, nil
	}
	var variants []kconfig.ActionPlanFamilyVariant
	for _, variant := range request.variants {
		snapshot, err := kconfig.ReadActionPlanSnapshot(request.initialSnapshots[variant.name])
		if err != nil {
			return nil, fmt.Errorf("read initial family snapshot %s: %w", variant.name, err)
		}
		result.initial[variant.name] = &snapshot
		variants = append(variants, kconfig.ActionPlanFamilyVariant{Name: variant.name, Snapshot: snapshot})
	}
	family, err := kconfig.BuildConservativeActionPlanFamily(variants)
	if err != nil {
		return nil, fmt.Errorf("reconstruct initial conservative family: %w", err)
	}
	result.cut, err = kconfig.ReadActionPlanFamilyExecutionCut(family, request.cutIn)
	if err != nil {
		return nil, fmt.Errorf("read initial family execution cut: %w", err)
	}
	// Read the complete immutable cut once. Ordinary transitive outputs can
	// supply header observations too, even when literal inference meant they
	// were not unresolved-header roots. These are executed bytes, not recipes.
	result.artifacts, err = result.cut.ObserveArtifacts(request.stores)
	if err != nil {
		return nil, fmt.Errorf("observe completed family execution artifacts: %w", err)
	}
	result.observed, err = result.artifacts.ObserveHeaders()
	if err != nil {
		return nil, fmt.Errorf("observe completed family execution headers: %w", err)
	}
	return result, nil
}

func (p *familyExecutionPipeline) variantOptions(name string, cache *kconfig.ActionPlanFamilyPlanningCache) *kconfig.ActionPlanFamilyVariantPlanningOptions {
	if p == nil {
		return nil
	}
	return &kconfig.ActionPlanFamilyVariantPlanningOptions{
		Variant: name, Cache: cache, InitialSnapshot: p.initial[name], Cut: p.cut, ObservedHeaders: p.observed,
		// Actual current resolved files are filled by evaluateLinuxKbuildProbes,
		// never copied from the initial snapshot to satisfy replay verification.
	}
}

func (p *familyExecutionPipeline) record(request familyPlanVariantRequest, value linuxKbuildProbeValue) error {
	if p == nil {
		return nil
	}
	if p.recorded[request.name] || value.familyPlanningResult == nil || value.familyPlanningResult.Plan != value.actionPlan {
		return fmt.Errorf("family execution variant %s has missing or repeated planning evidence", request.name)
	}
	if !slices.ContainsFunc(p.request.variants, func(variant familyPlanVariantRequest) bool { return variant.name == request.name }) {
		return fmt.Errorf("family execution references unknown variant %q", request.name)
	}
	if p.request.mode == "initial" {
		// Re-read exactly the normal snapshot artifact that replay will consume;
		// demands remain separate bounded metadata, not snapshot transport fields.
		snapshot, err := kconfig.ReadActionPlanSnapshot(request.snapshot)
		if err != nil {
			return fmt.Errorf("read written family snapshot %s: %w", request.name, err)
		}
		p.variants = append(p.variants, kconfig.ActionPlanFamilyVariant{Name: request.name, Snapshot: snapshot})
		p.demands[request.name] = value.familyPlanningResult.GeneratedHeaderDemands
	} else {
		p.results = append(p.results, value.familyPlanningResult)
	}
	p.recorded[request.name] = true
	return nil
}

func (p *familyExecutionPipeline) publish() error {
	if p == nil {
		return nil
	}
	if p.request.mode == "guards" {
		return fmt.Errorf("guard discovery cannot publish a family execution plan")
	}
	if len(p.recorded) != len(p.request.variants) {
		return fmt.Errorf("family execution requires planning evidence for every variant")
	}
	if p.request.mode == "replay" {
		return kconfig.BuildAndWriteObservedActionPlanFamily(
			p.results, p.request.segments, p.request.reuseReportOut, p.request.pinnedOut, p.request.headersOut, p.request.artifactsOut, p.artifacts,
		)
	}
	initial, err := kconfig.NewActionPlanFamilyInitialExecution(p.variants, p.demands)
	if err != nil {
		return err
	}
	data, err := initial.Cut.CanonicalJSON()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.request.cutOut), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(p.request.cutOut, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create family execution cut: %w", err)
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := initial.Cut.WriteSelectionMarkers(p.request.selectionOut); err != nil {
		return err
	}
	return initial.Cut.WriteSegments(p.request.segments)
}
