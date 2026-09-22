package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
	"github.com/hermeticbuild/linux.bzl/internal/toolsetpath"
)

type selectedKbuildOutput struct {
	Tree string
	Path string
}

// The producer is selected from the validated plan here. The descriptor and
// its immutable output trees are declared inputs of the projection action.
func writeSelectedKbuildOutput(plan *kconfig.ActionPlan, target, filename string) error {
	var selected *selectedKbuildOutput
	for _, node := range plan.Nodes {
		for _, output := range node.Outputs {
			if output.Path != target || output.ObservedPath != "" || output.ArtifactPath != "" && output.ArtifactPath != target {
				continue
			}
			if selected != nil {
				return fmt.Errorf("selected Kbuild output %q has multiple published producers", target)
			}
			selected = &selectedKbuildOutput{Tree: output.Tree, Path: output.Path}
		}
	}
	if selected == nil {
		return fmt.Errorf("selected Kbuild output %q has no published producer", target)
	}
	data, err := json.Marshal(selected)
	if err != nil {
		return err
	}
	return os.WriteFile(workspacePath(filename), data, 0o644)
}

func projectSelectedKbuildOutput(filename string, trees map[string]string, output string) error {
	data, err := os.ReadFile(workspacePath(filename))
	if err != nil {
		return err
	}
	var selected selectedKbuildOutput
	if err := json.Unmarshal(data, &selected); err != nil {
		return err
	}
	if err := toolaction.ValidateCanonicalArtifactPath(selected.Path); err != nil {
		return err
	}
	root := trees[selected.Tree]
	if root == "" {
		return fmt.Errorf("selected Kbuild output %q references unbound tree %q", selected.Path, selected.Tree)
	}
	source := filepath.Join(workspacePath(root), selected.Path)
	current := workspacePath(root)
	components := strings.Split(selected.Path, "/")
	for index, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		// A sandbox may expose a declared TreeFile through a final symlink;
		// intermediate symlinks must not redirect traversal outside the tree.
		if info.Mode()&os.ModeSymlink != 0 && index != len(components)-1 {
			return fmt.Errorf("selected Kbuild output %q traverses symlink %q", selected.Path, current)
		}
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("selected Kbuild output %q is not a regular file", source)
	}
	data, err = os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(workspacePath(output), data, info.Mode().Perm())
}

// Native Kconfig is itself a host Kbuild target. The root configuration goal
// selects its bootstrap ancestors (including fixdep) and compiler environment.
// Keep the executable's dependency closure, before the command that invokes it.
func nativeKconfigToolMetadata(opts linuxKbuildProbeOptions, scopes *kconfig.KbuildProbeScopes, sourceRoot string) (*kconfig.CompactMetadata, *kconfig.ResolvedConfig, error) {
	resolved := &kconfig.ResolvedConfig{Effective: map[string]string{}, Written: map[string]bool{}}
	metadataOptions := linuxCompactMetadataOptions(opts.variables, opts.sourceNamespaces, sourceRoot, true, opts.targetContract, opts.hostContract)
	metadataOptions.PreconfiguredObjectTree = true
	metadata, err := opts.tree.CompactMetadataForResolvedConfigWithOptions(resolved, metadataOptions, func(*kconfig.ResolvedConfig) (kconfig.CompactConfigGraph, error) {
		variables := maps.Clone(opts.variables)
		for name, value := range linuxRootMakeInvocationVariables(sourceRoot) {
			if _, configured := variables[name]; !configured {
				variables[name] = value
			}
		}
		commandLine, err := kbuildCommandLineVariables(opts.targetContract, opts.hostContract, opts.variables)
		if err != nil {
			return kconfig.CompactConfigGraph{}, err
		}
		options, err := scopes.Options("target", kconfig.KbuildOptions{
			Variables:                         variables,
			CommandLineVariables:              commandLine,
			SyntheticToolCommandLineVariables: kbuildSyntheticToolRoleCommandLineVariables(opts.targetContract, opts.hostContract, opts.variables),
			AutoExportCommandLineVariables:    kbuildConfiguredCommandLineAutoExports(opts.variables, opts.targetContract, opts.hostContract),
			ActionRoles:                       metadataOptions.ActionRoles,
			SourceRoots:                       opts.sourceRoots,
			ConfigVariablesComplete:           true,
			MakeVariablesComplete:             true,
			Shell: func(command string) (string, error) {
				return hermeticLinuxKbuildShell(command, sourceRoot)
			},
		})
		if err != nil {
			return kconfig.CompactConfigGraph{}, err
		}
		bindEnvironment := func(exported map[string]string) (func() error, error) {
			byScope := map[string]map[string]string{}
			for _, scope := range []string{"target", "host"} {
				values, err := linuxProbeEnvironmentForScope(scope, exported)
				if err != nil {
					return nil, err
				}
				byScope[scope] = values
			}
			return scopes.BindExactScriptEnvironments(byScope, exported)
		}
		profiles, selections, _, err := evaluatedKbuildInvocationProfiles(sourceRoot, sourceRoot,
			[]string{"syncconfig"}, nil, variables, options, bindEnvironment,
			nil, nil, opts.kbuildInputCache, true, false, nil, nil, nil)
		if err != nil {
			return kconfig.CompactConfigGraph{}, err
		}
		return kconfig.CompactConfigGraph{KbuildProfiles: profiles, KbuildSelections: selections}, nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("plan native Kconfig host tool: %w", err)
	}
	if err := scopes.BindActionPlanToolsetPathCapabilities(metadata); err != nil {
		return nil, nil, err
	}
	if err := metadata.SelectKbuildOutput("scripts/kconfig/conf"); err != nil {
		return nil, nil, err
	}
	return metadata, resolved, nil
}

// nativeConfigProjection contains the selected source's outputs, including
// their exact Make and C spellings. Native conf owns both the values and
// serialization; the Go parser supplies only the declared symbol inventory.
type nativeConfigProjection struct {
	files map[string]string
}

// Declared TreeArtifact inputs may be exposed through a sandbox root symlink.
// Resolve that binding, as copyRecipeTree does, without relaxing the audit of
// the serializer's actual file leaves or following symlinked subdirectories.
func readDeclaredNativeConfigProjection(root string) (*nativeConfigProjection, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve native config input tree: %w", err)
	}
	return readNativeConfigProjection(resolved)
}

func readNativeConfigProjection(root string) (*nativeConfigProjection, error) {
	files := map[string]string{}
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		selected := relative == ".config" || strings.HasPrefix(relative, "include/config/") || strings.HasPrefix(relative, "include/generated/")
		if !selected && relative != "." && relative != "include" && relative != "include/config" && relative != "include/generated" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("native Kconfig output %q is not a regular file", filename)
		}
		contents, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = string(contents)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read native Kconfig projection: %w", err)
	}
	if _, err := kconfig.NativeConfigProjectionPaths(files); err != nil {
		return nil, err
	}
	return &nativeConfigProjection{files: files}, nil
}

// Import precisely the values consumed by planning. Native .config
// also records visible disabled symbols, whereas auto.conf omits them. An
// empty string remains a present value: older sources write it as quoted text
// in auto.conf, so treating it as an absent symbol changes Make semantics.
func (p *nativeConfigProjection) resolved(tree *kconfig.Tree) (*kconfig.ResolvedConfig, error) {
	if p == nil {
		return nil, fmt.Errorf("Kbuild planning requires the selected source's native Kconfig projection")
	}
	native, err := kconfig.ParseConfig(strings.NewReader(p.files[".config"]))
	if err != nil {
		return nil, fmt.Errorf("parse native .config: %w", err)
	}
	resolved := &kconfig.ResolvedConfig{
		Raw: maps.Clone(native), Effective: native, Written: map[string]bool{},
	}
	for key, value := range native {
		resolved.Written[key] = value != "n"
	}
	// Prefix-based generator dependencies must also include currently absent
	// symbols, so another variant cannot widen their dependency contract.
	for name, symbol := range tree.Symbols {
		if symbol.Const || symbol.Transitional || len(symbol.Menus) == 0 {
			continue
		}
		key := "CONFIG_" + name
		if _, present := resolved.Effective[key]; !present {
			resolved.Effective[key] = "n"
		}
	}
	return resolved, nil
}

// Native files are copied unchanged between actions, so compiler queries must
// not embed build-worker paths. Runtime paths such as /sbin/init remain data.
func (p *nativeConfigProjection) rejectWorkerPaths(roots []string) error {
	private := map[string]bool{toolsetpath.ShellAliasRootPath: true}
	for _, root := range roots {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return fmt.Errorf("resolve native config build root %q: %w", root, err)
		}
		private[filepath.Clean(root)] = true
		private[resolved] = true
	}
	values, err := kconfig.ParseConfig(strings.NewReader(p.files[".config"]))
	if err != nil {
		return fmt.Errorf("parse native .config: %w", err)
	}
	privateRoots := slices.Sorted(maps.Keys(private))
	for _, key := range slices.Sorted(maps.Keys(values)) {
		value := values[key]
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = strings.NewReplacer(`\\`, `\`, `\"`, `"`).Replace(value[1 : len(value)-1])
		}
		for _, root := range privateRoots {
			if strings.Contains(value, root) {
				return fmt.Errorf("native %s contains build-worker path %q; build-worker paths in configuration are not supported", key, root)
			}
		}
	}
	return nil
}
