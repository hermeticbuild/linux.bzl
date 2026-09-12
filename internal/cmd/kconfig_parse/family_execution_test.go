package main

import (
	"bytes"
	"flag"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func completeFamilyExecutionFlags(t *testing.T, mode string, variants []familyPlanVariantRequest) familyExecutionFlags {
	t.Helper()
	root := t.TempDir()
	flags := familyExecutionFlags{mode: mode}
	if mode != "guards" {
		for _, segment := range []string{"target", "host", "bootstrap", "prehost"} {
			flags.segments = append(flags.segments, namedPath{Name: segment, Path: filepath.Join(root, "segments", segment)})
		}
	}
	if mode == "initial" {
		flags.checkpointOut = filepath.Join(root, "checkpoints")
		flags.cutOut = filepath.Join(root, "cut.json")
		flags.selectionOut = filepath.Join(root, "selection")
		return flags
	}
	flags.cutIn = filepath.Join(root, "initial", "cut.json")
	flags.checkpointIn = filepath.Join(root, "initial", "checkpoints")
	if mode != "guards" {
		flags.pinnedOut = filepath.Join(root, "pinned")
		flags.headersOut = filepath.Join(root, "headers")
		flags.artifactsOut = filepath.Join(root, "artifacts")
		flags.reuseReportOut = filepath.Join(root, "reuse.json")
	}
	for _, variant := range variants {
		flags.initialSnapshots = append(flags.initialSnapshots, namedPath{Name: variant.name, Path: filepath.Join(root, "initial", variant.name+".snapshot.json.gz")})
	}
	for _, tree := range slices.Sorted(maps.Keys(kconfig.LinuxKernelPlanTrees)) {
		flags.stores = append(flags.stores, namedPath{Name: tree, Path: filepath.Join(root, "stores", tree)})
	}
	return flags
}

func familyExecutionVariantsForTest(t *testing.T, names ...string) []familyPlanVariantRequest {
	t.Helper()
	flags := completeFamilyPlanFlags(t.TempDir(), names...)
	variants, err := flags.requests()
	if err != nil {
		t.Fatal(err)
	}
	return variants
}

func TestFamilyExecutionFlagsRequireCompleteUnambiguousPhase(t *testing.T) {
	variants := familyExecutionVariantsForTest(t, "debug", "base")
	for _, mode := range []string{"initial", "replay"} {
		t.Run(mode, func(t *testing.T) {
			flags := completeFamilyExecutionFlags(t, mode, variants)
			request, err := flags.request(variants)
			if err != nil {
				t.Fatal(err)
			}
			if request.mode != mode || len(request.segments) != 4 || len(request.variants) != 2 {
				t.Fatalf("request = %#v", request)
			}
			if mode == "replay" && (len(request.initialSnapshots) != 2 || len(request.stores) != len(kconfig.LinuxKernelPlanTrees)) {
				t.Fatalf("incomplete replay inputs: %#v", request)
			}
		})
	}
	for _, test := range []struct {
		name, mode, want string
		change           func(*familyExecutionFlags)
	}{
		{"missing mode", "initial", "must be initial, replay or guards", func(f *familyExecutionFlags) { f.mode = "" }},
		{"unknown mode", "initial", "must be initial, replay or guards", func(f *familyExecutionFlags) { f.mode = "prepared" }},
		{"missing segment", "initial", "missing name", func(f *familyExecutionFlags) { f.segments = f.segments[1:] }},
		{"duplicate segment", "initial", "repeats name", func(f *familyExecutionFlags) { f.segments = append(f.segments, f.segments[0]) }},
		{"unknown segment", "initial", "unknown name", func(f *familyExecutionFlags) { f.segments[0].Name = "later" }},
		{"empty segment", "initial", "empty path", func(f *familyExecutionFlags) { f.segments[0].Path = " " }},
		{"missing cut output", "initial", "requires", func(f *familyExecutionFlags) { f.cutOut = "" }},
		{"missing selection", "initial", "requires", func(f *familyExecutionFlags) { f.selectionOut = "" }},
		{"missing checkpoint output", "initial", "requires -family_execution_checkpoint_out", func(f *familyExecutionFlags) { f.checkpointOut = "" }},
		{"initial checkpoint input", "initial", "replay-only", func(f *familyExecutionFlags) { f.checkpointIn = "old-checkpoint" }},
		{"missing checkpoint input", "replay", "require -family_execution_checkpoint_in", func(f *familyExecutionFlags) { f.checkpointIn = "" }},
		{"replay checkpoint output", "replay", "initial-only", func(f *familyExecutionFlags) { f.checkpointOut = "new-checkpoint" }},
		{"nested checkpoint output", "initial", "overlap", func(f *familyExecutionFlags) { f.checkpointOut = filepath.Join(f.selectionOut, "checkpoints") }},
		{"checkpoint overwrite", "replay", "overlaps initial checkpoint", func(f *familyExecutionFlags) { f.headersOut = filepath.Join(f.checkpointIn, "headers") }},
		{"initial with cut input", "initial", "replay-only", func(f *familyExecutionFlags) { f.cutIn = "old-cut" }},
		{"initial with pins", "initial", "replay-only", func(f *familyExecutionFlags) { f.pinnedOut = "pins" }},
		{"initial with headers", "initial", "replay-only", func(f *familyExecutionFlags) { f.headersOut = "headers" }},
		{"initial with report", "initial", "replay-only", func(f *familyExecutionFlags) { f.reuseReportOut = "report" }},
		{"initial with stores", "initial", "replay-only", func(f *familyExecutionFlags) { f.stores = namedPathFlag{{Name: "objects", Path: "objects"}} }},
		{"initial with snapshot", "initial", "replay-only", func(f *familyExecutionFlags) { f.initialSnapshots = namedPathFlag{{Name: "base", Path: "snapshot"}} }},
		{"replay with cut output", "replay", "initial-only", func(f *familyExecutionFlags) { f.cutOut = "cut" }},
		{"replay with selection", "replay", "initial-only", func(f *familyExecutionFlags) { f.selectionOut = "selection" }},
		{"missing cut input", "replay", "requires", func(f *familyExecutionFlags) { f.cutIn = "" }},
		{"missing pins", "replay", "requires", func(f *familyExecutionFlags) { f.pinnedOut = "" }},
		{"missing headers", "replay", "requires", func(f *familyExecutionFlags) { f.headersOut = "" }},
		{"missing artifacts", "replay", "requires", func(f *familyExecutionFlags) { f.artifactsOut = "" }},
		{"initial with artifacts", "initial", "replay-only", func(f *familyExecutionFlags) { f.artifactsOut = "artifacts" }},
		{"artifact store overwrite", "replay", "overlaps cut store", func(f *familyExecutionFlags) { f.artifactsOut = f.stores[0].Path }},
		{"missing report", "replay", "requires", func(f *familyExecutionFlags) { f.reuseReportOut = "" }},
		{"missing snapshot", "replay", "missing name", func(f *familyExecutionFlags) { f.initialSnapshots = f.initialSnapshots[1:] }},
		{"duplicate snapshot", "replay", "repeats name", func(f *familyExecutionFlags) { f.initialSnapshots = append(f.initialSnapshots, f.initialSnapshots[0]) }},
		{"unknown snapshot", "replay", "unknown name", func(f *familyExecutionFlags) { f.initialSnapshots[0].Name = "unknown" }},
		{"missing store", "replay", "missing name", func(f *familyExecutionFlags) { f.stores = f.stores[1:] }},
		{"duplicate store", "replay", "repeats name", func(f *familyExecutionFlags) { f.stores = append(f.stores, f.stores[0]) }},
		{"unknown store", "replay", "unknown name", func(f *familyExecutionFlags) { f.stores[0].Name = "work" }},
		{"empty store", "replay", "empty path", func(f *familyExecutionFlags) { f.stores[0].Path = "\t" }},
		{"duplicate output", "initial", "overlap", func(f *familyExecutionFlags) { f.selectionOut = f.segments[0].Path }},
		{"nested output", "initial", "overlap", func(f *familyExecutionFlags) { f.cutOut = filepath.Join(f.selectionOut, "cut.json") }},
		{"snapshot overwrite", "replay", "overlaps initial snapshot", func(f *familyExecutionFlags) { f.initialSnapshots[0].Path = variants[0].snapshot }},
		{"store overwrite", "replay", "overlaps cut store", func(f *familyExecutionFlags) { f.headersOut = f.stores[0].Path }},
		{"cut overwrite", "replay", "overlaps initial cut", func(f *familyExecutionFlags) { f.reuseReportOut = f.cutIn }},
	} {
		t.Run(test.name, func(t *testing.T) {
			flags := completeFamilyExecutionFlags(t, test.mode, variants)
			test.change(&flags)
			if _, err := flags.request(variants); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("request() error = %v, want %q", err, test.want)
			}
		})
	}
	flags := completeFamilyExecutionFlags(t, "initial", variants)
	if _, err := flags.request(nil); err == nil || !strings.Contains(err.Error(), "family_plan_variant") {
		t.Fatalf("missing variants error = %v", err)
	}
}

func TestFamilyExecutionGuardsRequireCompleteInputsAndNoOutputs(t *testing.T) {
	variants := []familyPlanVariantRequest{{name: "base"}, {name: "debug", overlay: "debug.config"}}
	flags := completeFamilyExecutionFlags(t, "guards", variants)
	request, err := flags.request(variants)
	if err != nil {
		t.Fatal(err)
	}
	if request.mode != "guards" || len(request.segments) != 0 || len(request.variants) != 2 ||
		request.variants[1].overlay != "debug.config" || len(request.initialSnapshots) != 2 ||
		len(request.stores) != len(kconfig.LinuxKernelPlanTrees) {
		t.Fatalf("guard request lost input-only contract: %#v", request)
	}
	for _, test := range []struct {
		name, want string
		change     func(*familyExecutionFlags)
	}{
		{"missing cut", "requires -family_execution_cut_in", func(f *familyExecutionFlags) { f.cutIn = "" }},
		{"missing checkpoint", "require -family_execution_checkpoint_in", func(f *familyExecutionFlags) { f.checkpointIn = "" }},
		{"checkpoint output", "initial-only", func(f *familyExecutionFlags) { f.checkpointOut = "output-checkpoint" }},
		{"empty cut", "requires -family_execution_cut_in", func(f *familyExecutionFlags) { f.cutIn = " " }},
		{"missing snapshot", "missing name", func(f *familyExecutionFlags) { f.initialSnapshots = f.initialSnapshots[1:] }},
		{"duplicate snapshot", "repeats name", func(f *familyExecutionFlags) { f.initialSnapshots = append(f.initialSnapshots, f.initialSnapshots[0]) }},
		{"unknown snapshot", "unknown name", func(f *familyExecutionFlags) { f.initialSnapshots[0].Name = "unknown" }},
		{"empty snapshot", "empty path", func(f *familyExecutionFlags) { f.initialSnapshots[0].Path = "\t" }},
		{"missing store", "missing name", func(f *familyExecutionFlags) { f.stores = f.stores[1:] }},
		{"duplicate store", "repeats name", func(f *familyExecutionFlags) { f.stores = append(f.stores, f.stores[0]) }},
		{"unknown store", "unknown name", func(f *familyExecutionFlags) { f.stores[0].Name = "work" }},
		{"empty store", "empty path", func(f *familyExecutionFlags) { f.stores[0].Path = " " }},
		{"root cut", "filesystem root", func(f *familyExecutionFlags) { f.cutIn = string(filepath.Separator) }},
		{"root snapshot", "filesystem root", func(f *familyExecutionFlags) { f.initialSnapshots[0].Path = string(filepath.Separator) }},
		{"root store", "filesystem root", func(f *familyExecutionFlags) { f.stores[0].Path = string(filepath.Separator) }},
		{"segment output", "output flags", func(f *familyExecutionFlags) { f.segments = namedPathFlag{{Name: "host", Path: "host-plan"}} }},
		{"cut output", "output flags", func(f *familyExecutionFlags) { f.cutOut = "cut.json" }},
		{"selection output", "output flags", func(f *familyExecutionFlags) { f.selectionOut = "selection" }},
		{"pin output", "output flags", func(f *familyExecutionFlags) { f.pinnedOut = "pinned" }},
		{"header output", "output flags", func(f *familyExecutionFlags) { f.headersOut = "headers" }},
		{"artifact output", "output flags", func(f *familyExecutionFlags) { f.artifactsOut = "artifacts" }},
		{"report output", "output flags", func(f *familyExecutionFlags) { f.reuseReportOut = "reuse.json" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			flags := completeFamilyExecutionFlags(t, "guards", variants)
			test.change(&flags)
			if _, err := flags.request(variants); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("guard request error = %v, want %q", err, test.want)
			}
		})
	}
	if _, err := flags.request(nil); err == nil || !strings.Contains(err.Error(), "family_plan_variant") {
		t.Fatalf("guard request without variants = %v", err)
	}
	for _, mode := range []string{"initial", "replay"} {
		flags := completeFamilyExecutionFlags(t, mode, variants)
		if _, err := flags.request(variants); err == nil || !strings.Contains(err.Error(), "path is empty") {
			t.Fatalf("%s accepted guard-only variant input requests: %v", mode, err)
		}
	}
}

func TestFamilyExecutionGuardsRejectEveryVariantOutputField(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*familyPlanVariantRequest)
	}{
		{"arch", func(v *familyPlanVariantRequest) { v.arch = "arch" }},
		{"snapshot", func(v *familyPlanVariantRequest) { v.snapshot = "snapshot" }},
		{"config", func(v *familyPlanVariantRequest) { v.resolved.config = "config" }},
		{"auto.conf", func(v *familyPlanVariantRequest) { v.resolved.autoConf = "auto.conf" }},
		{"auto.conf.cmd", func(v *familyPlanVariantRequest) { v.resolved.autoConfCmd = "auto.conf.cmd" }},
		{"autoconf.h", func(v *familyPlanVariantRequest) { v.resolved.autoconf = "autoconf.h" }},
		{"rustc_cfg", func(v *familyPlanVariantRequest) { v.resolved.rustcCfg = "rustc_cfg" }},
		{"kernel.release", func(v *familyPlanVariantRequest) { v.resolved.kernelRelease = "kernel.release" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			variants := []familyPlanVariantRequest{{name: "base"}}
			test.change(&variants[0])
			flags := completeFamilyExecutionFlags(t, "guards", variants)
			if _, err := flags.request(variants); err == nil || !strings.Contains(err.Error(), "variant output fields") {
				t.Fatalf("guard mode admitted %s output: %v", test.name, err)
			}
		})
	}
}

func TestFamilyExecutionFlagRegistrationRejectsBlankAndRepeatedScalars(t *testing.T) {
	for _, arguments := range [][]string{
		{"-family_execution_mode="},
		{"-family_execution_checkpoint_in="},
		{"-family_execution_checkpoint_out="},
		{"-family_execution_checkpoint_in=a", "-family_execution_checkpoint_in=b"},
		{"-family_execution_checkpoint_out=a", "-family_execution_checkpoint_out=b"},
		{"-family_execution_mode=initial", "-family_execution_mode=replay"},
		{"-family_execution_cut_out=one", "-family_execution_cut_out=two"},
		{"-family_execution_store=objects="},
	} {
		var values familyExecutionFlags
		flags := flag.NewFlagSet("execution", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		values.register(flags)
		if err := flags.Parse(arguments); err == nil {
			t.Fatalf("Parse(%q) accepted malformed flags", arguments)
		}
	}
}

func TestFamilyExecutionDisabledLeavesOrdinaryPlanningUntouched(t *testing.T) {
	var flags familyExecutionFlags
	request, err := flags.request(nil)
	if err != nil || request != nil {
		t.Fatalf("ordinary request = %v, %v", request, err)
	}
	pipeline, err := newFamilyExecutionPipeline(nil)
	if err != nil || pipeline != nil || pipeline.variantOptions("base", nil) != nil {
		t.Fatalf("ordinary pipeline = %v, %v", pipeline, err)
	}
	if err := pipeline.record(familyPlanVariantRequest{}, linuxKbuildProbeValue{}); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.publish(); err != nil {
		t.Fatal(err)
	}
	code, stderr := runKconfigParseForTest(t, "-family_execution_mode=initial", "-probe_plan_union_out=ignored")
	if code != 2 || !strings.Contains(stderr, "family_plan_variant") {
		t.Fatalf("unrelated mode silently ignored execution flags: %d, %q", code, stderr)
	}
}

func TestFamilyExecutionInitialPublishesConservativeFamilyAndReplayAuthenticatesIt(t *testing.T) {
	variants := familyExecutionVariantsForTest(t, "base")
	variants[0].snapshot = writeTestActionPlanFamilySnapshot(t)
	flags := completeFamilyExecutionFlags(t, "initial", variants)
	request, err := flags.request(variants)
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := newFamilyExecutionPipeline(request)
	if err != nil {
		t.Fatal(err)
	}
	options := pipeline.variantOptions("base", kconfig.NewActionPlanFamilyPlanningCache())
	if options.Variant != "base" || options.Cache == nil || options.InitialSnapshot != nil || options.Cut != nil || options.ObservedHeaders != nil || options.ResolvedConfigFiles != nil {
		t.Fatalf("initial options = %#v", options)
	}
	if err := pipeline.publish(); err == nil {
		t.Fatal("published incomplete family")
	}
	if _, err := os.Stat(request.cutOut); !os.IsNotExist(err) {
		t.Fatalf("incomplete family published cut: %v", err)
	}
	snapshot, err := kconfig.ReadActionPlanSnapshot(variants[0].snapshot)
	if err != nil {
		t.Fatal(err)
	}
	plan := &kconfig.ActionPlan{
		Toolsets: snapshot.Toolsets, Sources: snapshot.Sources, Recipes: snapshot.Recipes,
		Nodes: snapshot.Nodes, InputSets: snapshot.InputSets, Products: snapshot.Products,
	}
	value := linuxKbuildProbeValue{
		actionPlan: plan,
		familyPlanningResult: &kconfig.ActionPlanFamilyVariantPlanningResult{
			Plan: plan, GeneratedHeaderDemands: kconfig.ConfigDependencyGeneratedHeaderDemandCollection{Enabled: true},
		},
	}
	if err := pipeline.record(variants[0], value); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.record(variants[0], value); err == nil {
		t.Fatal("accepted duplicate variant evidence")
	}
	if err := pipeline.publish(); err != nil {
		t.Fatal(err)
	}
	for segment, directory := range request.segments {
		if _, err := os.Stat(filepath.Join(directory, "schema", kconfig.LinuxKernelFamilyPlanSchema)); err != nil {
			t.Errorf("%s segment: %v", segment, err)
		}
	}
	if _, err := os.Stat(filepath.Join(request.selectionOut, "mode", "cut")); err != nil {
		t.Fatal(err)
	}
	cutData, err := os.ReadFile(request.cutOut)
	if err != nil {
		t.Fatal(err)
	}
	if err := pipeline.publish(); err == nil {
		t.Fatal("overwrote an existing execution-cut output")
	}
	if after, err := os.ReadFile(request.cutOut); err != nil || !bytes.Equal(cutData, after) {
		t.Fatalf("existing cut changed: %v", err)
	}
	replayVariants := familyExecutionVariantsForTest(t, "base")
	replayFlags := completeFamilyExecutionFlags(t, "replay", replayVariants)
	replayFlags.cutIn = request.cutOut
	replayFlags.initialSnapshots = namedPathFlag{{Name: "base", Path: variants[0].snapshot}}
	replayRequest, err := replayFlags.request(replayVariants)
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range replayRequest.stores {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	replay, err := newFamilyExecutionPipeline(replayRequest)
	if err != nil {
		t.Fatal(err)
	}
	replayOptions := replay.variantOptions("base", nil)
	if replayOptions.InitialSnapshot == nil || replayOptions.Cut == nil || replayOptions.ObservedHeaders == nil || replayOptions.ResolvedConfigFiles != nil {
		t.Fatalf("replay options = %#v", replayOptions)
	}
	// The CLI deliberately leaves these nil until the current evaluation has
	// resolved real config files. Initial snapshot contents cannot self-verify.
	if len(replayOptions.Cut.NodeIDs()) != 0 {
		t.Fatal("empty frontier unexpectedly selected a generator")
	}
	guardVariants := []familyPlanVariantRequest{{name: "base"}}
	guardFlags := completeFamilyExecutionFlags(t, "guards", guardVariants)
	guardFlags.cutIn = request.cutOut
	guardFlags.initialSnapshots = slices.Clone(replayFlags.initialSnapshots)
	guardFlags.stores = slices.Clone(replayFlags.stores)
	guardRequest, err := guardFlags.request(guardVariants)
	if err != nil {
		t.Fatal(err)
	}
	guards, err := newFamilyExecutionPipeline(guardRequest)
	if err != nil {
		t.Fatal(err)
	}
	guardOptions := guards.variantOptions("base", nil)
	if guardOptions.InitialSnapshot == nil || guardOptions.Cut == nil || guardOptions.ObservedHeaders == nil ||
		guardOptions.ResolvedConfigFiles != nil || guards.artifacts == nil || guardOptions.Cut.ID() != replayOptions.Cut.ID() {
		t.Fatalf("guard discovery bypassed original replay/observation inputs: %#v", guardOptions)
	}
	if err := guards.publish(); err == nil || !strings.Contains(err.Error(), "cannot publish") {
		t.Fatalf("guard discovery attempted ordinary family publication: %v", err)
	}
	if err := os.WriteFile(request.cutOut, append(slices.Clone(cutData), '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newFamilyExecutionPipeline(replayRequest); err == nil {
		t.Fatal("replay accepted a changed cut contract")
	}
	if _, err := newFamilyExecutionPipeline(guardRequest); err == nil {
		t.Fatal("guard discovery accepted a changed cut contract")
	}
	for _, directory := range []string{replayRequest.pinnedOut, replayRequest.headersOut, replayRequest.artifactsOut} {
		if _, err := os.Stat(directory); !os.IsNotExist(err) {
			t.Fatalf("failed replay published %s: %v", directory, err)
		}
	}
}
