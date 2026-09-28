package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

const linuxFamilyCheckpointSchema = "linux-family-lowered-checkpoint-v1"
const maxLinuxFamilyCheckpointBytes = kconfig.MaxActionPlanSnapshotBytes + kconfig.MaxKbuildCompilerCheckpointBytes + 4096

// Both payloads are one declared initial action output. A consumer cannot mix
// a plan from one config with another producer's compiler-expression namespace.
type linuxFamilyCheckpoint struct {
	Schema, Variant string
	Target          sourceDerivedLinuxTarget
	Plan, Compiler  json.RawMessage
}

type checkpointLimitedWriter struct {
	output    io.Writer
	remaining int64
}

func (w *checkpointLimitedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, fmt.Errorf("compressed checkpoint exceeds byte budget")
	}
	n, err := w.output.Write(data)
	w.remaining -= int64(n)
	return n, err
}

func writeLinuxFamilyCheckpoint(filename string, checkpoint linuxFamilyCheckpoint) error {
	if len(checkpoint.Plan) == 0 || len(checkpoint.Plan) > kconfig.MaxActionPlanSnapshotBytes || len(checkpoint.Compiler) == 0 || len(checkpoint.Compiler) > kconfig.MaxKbuildCompilerCheckpointBytes {
		return fmt.Errorf("invalid family checkpoint payload size")
	}
	data, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	if len(data) > maxLinuxFamilyCheckpointBytes {
		return fmt.Errorf("family checkpoint exceeds byte budget")
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return err
	}
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	writer, err := gzip.NewWriterLevel(&checkpointLimitedWriter{file, kconfig.MaxActionPlanSnapshotCompressedBytes}, gzip.BestSpeed)
	if err != nil {
		return err
	}
	if _, err := writer.Write(data); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return file.Close()
}

func readLinuxFamilyCheckpoint(filename string) (*linuxFamilyCheckpoint, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() > kconfig.MaxActionPlanSnapshotCompressedBytes {
		return nil, fmt.Errorf("invalid compressed checkpoint size/type")
	}
	buffered := bufio.NewReader(io.LimitReader(file, kconfig.MaxActionPlanSnapshotCompressedBytes+1))
	reader, err := gzip.NewReader(buffered)
	if err != nil {
		return nil, err
	}
	reader.Multistream(false)
	data, err := io.ReadAll(io.LimitReader(reader, maxLinuxFamilyCheckpointBytes+1))
	closeErr := reader.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > maxLinuxFamilyCheckpointBytes {
		return nil, fmt.Errorf("family checkpoint exceeds byte budget")
	}
	if _, err := buffered.Peek(1); err != io.EOF {
		return nil, fmt.Errorf("family checkpoint has trailing compressed data")
	}
	var checkpoint linuxFamilyCheckpoint
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&checkpoint); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(checkpoint)
	if err != nil || !bytes.Equal(data, canonical) {
		return nil, fmt.Errorf("noncanonical family checkpoint")
	}
	if checkpoint.Schema != linuxFamilyCheckpointSchema || len(checkpoint.Plan) == 0 || len(checkpoint.Plan) > kconfig.MaxActionPlanSnapshotBytes || len(checkpoint.Compiler) == 0 || len(checkpoint.Compiler) > kconfig.MaxKbuildCompilerCheckpointBytes {
		return nil, fmt.Errorf("invalid family checkpoint schema/payload")
	}
	return &checkpoint, nil
}

func linuxCheckpointSourceArtifacts(opts linuxKbuildProbeOptions) (map[string]string, map[string]string, error) {
	root, err := workspaceDirectory(opts.rootPath)
	if err != nil {
		return nil, nil, err
	}
	candidates := maps.Clone(opts.sourceRoots)
	if candidates == nil {
		candidates = map[string]string{}
	}
	candidates["kernel"] = root
	virtual := map[string]string{}
	for name, candidate := range candidates {
		value, err := workspaceDirectory(candidate)
		if err != nil {
			// Self-mapped placeholders are inert Kbuild roots, not declared
			// filesystem trees (for example the host dependency namespace).
			if os.IsNotExist(err) && name == candidate && fs.ValidPath(name) {
				virtual[name] = candidate
				delete(candidates, name)
				continue
			}
			return nil, nil, err
		}
		value, err = filepath.Abs(value)
		if err != nil {
			return nil, nil, err
		}
		value, err = filepath.EvalSymlinks(value)
		if err != nil {
			return nil, nil, err
		}
		candidates[name] = value
	}
	// Nested aliases describe the same declared source artifact. Keep the
	// outermost binding and retain each profile's relative path in its record.
	names := slices.SortedFunc(maps.Keys(candidates), func(a, b string) int {
		if len(candidates[a]) != len(candidates[b]) {
			return len(candidates[a]) - len(candidates[b])
		}
		return strings.Compare(a, b)
	})
	artifacts := map[string]string{}
	for _, name := range names {
		covered := false
		for _, parent := range artifacts {
			rel, err := filepath.Rel(parent, candidates[name])
			if err == nil && fs.ValidPath(filepath.ToSlash(rel)) {
				covered = true
				break
			}
		}
		if !covered {
			artifacts[name] = candidates[name]
		}
	}
	return artifacts, virtual, nil
}

func linuxFamilyCheckpointBindings(opts linuxKbuildProbeOptions, scopes *kconfig.KbuildProbeScopes, resolved *kconfig.ResolvedConfig) (kconfig.ActionPlanCheckpointBindings, error) {
	if opts.familyVariantOptions == nil {
		return kconfig.ActionPlanCheckpointBindings{}, fmt.Errorf("checkpoint requires a family variant")
	}
	current := cloneResolvedConfig(resolved)
	if err := normalizeResolvedConfigValues(current, func(value string) (string, error) {
		return scopes.ImportToolsetPathCapabilities(value, func(value string) (string, error) { return value, nil })
	}); err != nil {
		return kconfig.ActionPlanCheckpointBindings{}, err
	}
	metadataOptions := linuxCompactMetadataOptions(opts.variables, opts.sourceNamespaces, opts.objectRoot, opts.selectedProductsOnly, opts.targetContract, opts.hostContract)
	if opts.objectRoot != "" {
		root, err := workspaceDirectory(opts.objectRoot)
		if err != nil {
			return kconfig.ActionPlanCheckpointBindings{}, err
		}
		if opts.objectNamespace == "" {
			return kconfig.ActionPlanCheckpointBindings{}, fmt.Errorf("preconfigured checkpoint object tree requires its current namespace")
		}
		files, err := newKbuildSourceInputIndex(root)
		if err != nil {
			return kconfig.ActionPlanCheckpointBindings{}, err
		}
		metadataOptions.ExactSourceNamespaces = map[string]string{}
		for _, filename := range files.files {
			metadataOptions.ExactSourceNamespaces[filename] = opts.objectNamespace
		}
	}
	artifacts, virtual, err := linuxCheckpointSourceArtifacts(opts)
	if err != nil {
		return kconfig.ActionPlanCheckpointBindings{}, err
	}
	bindings, err := kconfig.NewActionPlanCheckpointBindings(opts.familyVariantOptions.Variant, artifacts,
		map[string]string{"target": opts.targetFacts.ToolsetIdentity(), "host": opts.hostFacts.ToolsetIdentity()},
		resolvedConfigObjectTreeContents(opts.tree, resolved, opts.kernelVersion), current, metadataOptions)
	bindings.VirtualSourceRoots = virtual
	return bindings, err
}

func replayLinuxFamilyCheckpoint(opts linuxKbuildProbeOptions, scopes *kconfig.KbuildProbeScopes) (linuxKbuildProbeValue, error) {
	if opts.familyVariantOptions == nil || opts.familyVariantOptions.Cut == nil || opts.checkpointOutput != "" {
		return linuxKbuildProbeValue{}, fmt.Errorf("saved Kbuild replay requires complete family replay inputs")
	}
	checkpoint, err := readLinuxFamilyCheckpoint(opts.checkpointInput)
	if err != nil {
		return linuxKbuildProbeValue{}, err
	}
	if checkpoint.Variant != opts.familyVariantOptions.Variant || checkpoint.Target.Arch != opts.target.Arch || checkpoint.Target.Srcarch != opts.target.Srcarch || checkpoint.Target.Machine != opts.targetFacts.Machine() {
		return linuxKbuildProbeValue{}, fmt.Errorf("checkpoint differs from current source-derived target/compiler")
	}
	if err := scopes.RestoreCompilerCheckpoint(checkpoint.Compiler, opts.normalizeConfigValue); err != nil {
		return linuxKbuildProbeValue{}, err
	}
	resolveOptions, err := resolveConfigOptions(opts.configMode)
	if err != nil {
		return linuxKbuildProbeValue{}, err
	}
	resolved, err := opts.tree.ResolveConfigWithOptions(opts.configFlags, resolveOptions)
	if err != nil {
		return linuxKbuildProbeValue{}, err
	}
	if err := normalizeResolvedConfigValues(resolved, opts.normalizeConfigValue); err != nil {
		return linuxKbuildProbeValue{}, err
	}
	bindings, err := linuxFamilyCheckpointBindings(opts, scopes, resolved)
	if err != nil {
		return linuxKbuildProbeValue{}, err
	}
	plan, err := kconfig.RestoreActionPlanCheckpoint(checkpoint.Plan, bindings)
	if err != nil {
		return linuxKbuildProbeValue{}, err
	}
	metadata, err := scopes.BindActionPlanCheckpoint(plan)
	if err != nil {
		return linuxKbuildProbeValue{}, err
	}
	options := *opts.familyVariantOptions
	options.ResolvedConfigFiles = bindings.ConfigFiles
	if opts.familyCompilerGuards != nil {
		options.PrepareCompilerGuards = func() error { return opts.familyCompilerGuards.prepareVariant(options.Variant, scopes, metadata) }
	}
	var result *kconfig.ActionPlanFamilyVariantPlanningResult
	if opts.guardDiscoveryOnly {
		err = kconfig.DiscoverActionPlanCheckpointCompilerGuards(plan, options)
	} else {
		result, err = kconfig.ReplayActionPlanCheckpoint(plan, options)
	}
	if err != nil {
		return linuxKbuildProbeValue{}, err
	}
	if opts.familyCompilerGuards != nil {
		if err := opts.familyCompilerGuards.finishVariant(options.Variant, scopes); err != nil {
			return linuxKbuildProbeValue{}, err
		}
	}
	value := linuxKbuildProbeValue{target: checkpoint.Target, resolved: resolved}
	if result != nil {
		value.actionPlan, value.configDependencies, value.familyPlanningResult = result.Plan, result.Dependencies, result
	}
	return value, nil
}
