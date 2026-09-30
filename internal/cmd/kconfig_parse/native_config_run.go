package main

import (
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
	"github.com/hermeticbuild/linux.bzl/internal/toolsetpath"
)

type nativeConfigRunOptions struct {
	executable, sourceRoot, output string
	anchors                        []string
	sourceRoots                    map[string]string
	identities, manifests          map[string]string
	toolsets                       map[string]configuredKbuildToolsetManifest
	contracts                      nativeConfigActionContractFlags
	evaluation                     *linuxKconfigProbeEvaluation
	seed, mode                     string
}

// Contract values arrive through Bazel Args so toolchain Files retain typed
// path mapping. The identity manifest contains canonical paths, not the
// execution-time action envelopes used after changing directory.
type nativeConfigActionContractFlags map[string]toolaction.Contract

// Native conf runs in a private object tree. Keep declared source roots at
// their logical relative names so source probes can read them without putting
// temporary execution paths into configuration values or dependency files.
func bindNativeConfigSourceRoots(work, execroot string, roots map[string]string) error {
	for _, alias := range slices.Sorted(maps.Keys(roots)) {
		if err := toolaction.ValidateCanonicalArtifactPath(alias); err != nil {
			return fmt.Errorf("native config source alias: %w", err)
		}
		destination, err := toolaction.ExpandExecutionRootValue(roots[alias], execroot)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(destination) {
			destination = filepath.Join(execroot, destination)
		}
		info, err := os.Stat(destination)
		if err != nil {
			return fmt.Errorf("native config source alias %q: %w", alias, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("native config source alias %q is not a directory", alias)
		}
		filename := filepath.Join(work, filepath.FromSlash(alias))
		parent := work
		components := strings.Split(alias, "/")
		for _, component := range components[:len(components)-1] {
			parent = filepath.Join(parent, component)
			if err := os.Mkdir(parent, 0o755); err != nil && !os.IsExist(err) {
				return err
			}
			info, err := os.Lstat(parent)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return fmt.Errorf("native config source alias %q traverses non-directory %q", alias, parent)
			}
		}
		if err := os.Symlink(destination, filename); err != nil {
			return err
		}
	}
	return nil
}

func (contracts nativeConfigActionContractFlags) register(flags *flag.FlagSet) {
	flags.Func("native_action_role", "Declared scoped tool action role", func(role string) error {
		if !toolaction.ValidBinding(role) {
			return fmt.Errorf("invalid native config action role %q", role)
		}
		if _, exists := contracts[role]; exists {
			return fmt.Errorf("duplicate native config action role %q", role)
		}
		contracts[role] = toolaction.Contract{Arguments: []string{}, Environment: map[string]string{}}
		return nil
	})
	for _, kind := range []string{"arg", "env"} {
		flags.Func("native_action_"+kind, "Scoped tool action "+kind+" in ROLE=VALUE form", func(value string) error {
			role, value, ok := strings.Cut(value, "=")
			contract, exists := contracts[role]
			if !ok || !exists {
				return fmt.Errorf("native config action %s requires a declared role: %q", kind, role)
			}
			if kind == "arg" {
				contract.Arguments = append(contract.Arguments, value)
			} else {
				name, value, ok := strings.Cut(value, "=")
				if !ok {
					return fmt.Errorf("invalid native config action environment %q", name)
				}
				contract.Environment[name] = value
			}
			contracts[role] = contract
			return nil
		})
	}
}

// Native configuration is one declared action per variant. Subsequent Kbuild
// discovery and replay actions only consume its immutable outputs.
func runNativeConfig(opts nativeConfigRunOptions) error {
	if err := toolaction.Validate(opts.contracts); err != nil {
		return err
	}
	execroot, err := os.Getwd()
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "linux-bzl-native-config-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	var identities, manifests []string
	for _, scope := range []string{"host", "target"} {
		identities = append(identities, scope+"="+opts.identities[scope])
		manifests = append(manifests, scope+"="+opts.manifests[scope])
	}
	projectionRoot := filepath.Join(work, ".toolset")
	if err := os.Mkdir(projectionRoot, 0o700); err != nil {
		return err
	}
	resolver, err := toolsetpath.LoadFlags(execroot, projectionRoot, identities, manifests, opts.anchors)
	if err != nil {
		return err
	}
	sourceRoot, err := filepath.Abs(opts.sourceRoot)
	if err != nil {
		return err
	}
	for alias, destination := range map[string]string{kbuildEvalSourceTree: sourceRoot, kbuildEvalObjectTree: "."} {
		if err := os.Symlink(destination, filepath.Join(work, alias)); err != nil {
			return err
		}
	}
	if err := bindNativeConfigSourceRoots(work, execroot, opts.sourceRoots); err != nil {
		return err
	}
	toolDirectory := filepath.Join(work, ".tools")
	if err := os.Mkdir(toolDirectory, 0o700); err != nil {
		return err
	}
	multicall, err := resolver.Resolve("target", opts.toolsets["target"].Tools["script-runtime"])
	if err != nil {
		return err
	}
	list := exec.Command(multicall, "--list")
	list.Env = []string{"LC_ALL=C"}
	applets, err := list.Output()
	if err != nil {
		return fmt.Errorf("list declared native Kconfig runtime applets: %w", err)
	}
	for _, name := range strings.Fields(string(applets)) {
		if name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, "\x00\r\n\t ") {
			return fmt.Errorf("invalid native Kconfig runtime applet %q", name)
		}
		if err := os.Symlink(multicall, filepath.Join(toolDirectory, name)); err != nil {
			return err
		}
	}
	for _, scope := range []string{"host", "target"} {
		manifest := opts.toolsets[scope]
		contracts := map[string]toolaction.Contract{}
		for role := range manifest.Actions {
			binding, _ := toolaction.ScopedBinding(scope, role)
			declared, exists := opts.contracts[binding]
			if !exists {
				return fmt.Errorf("native config action lacks declared %s contract", binding)
			}
			contract := toolaction.Contract{Arguments: slices.Clone(declared.Arguments), Environment: maps.Clone(declared.Environment)}
			for index, value := range contract.Arguments {
				contract.Arguments[index], err = toolaction.ExpandExecutionRootValue(value, execroot)
				if err != nil {
					return err
				}
			}
			for name, value := range contract.Environment {
				contract.Environment[name], err = toolaction.ExpandExecutionRootValue(value, execroot)
				if err != nil {
					return err
				}
			}
			contracts[role] = contract
		}
		for _, role := range slices.Sorted(maps.Keys(manifest.Tools)) {
			if _, companion := toolaction.BaseContractRole(role); companion {
				continue
			}
			executable, err := resolver.Resolve(scope, manifest.Tools[role])
			if err != nil {
				return err
			}
			binding, _ := toolaction.ScopedBinding(scope, role)
			var link *toolaction.Contract
			if companion, ok := toolaction.LinkContractRole(role); ok {
				if contract, found := contracts[companion]; found {
					link = &contract
				}
			}
			proxy, err := toolaction.InstallToolActionProxy(toolDirectory, multicall, binding, executable, contracts[role], link)
			if err != nil {
				return err
			}
			aliases := []string{kconfig.KbuildActionRoleToken(scope, role)}
			if scope == "target" {
				aliases = append(aliases, role, kconfig.KbuildActionRoleToken(kconfig.KbuildActionRoleAutoScope, role))
				if applet, ok := strings.CutPrefix(role, "script-applet-"); ok {
					aliases = append(aliases, applet)
				}
			}
			for _, alias := range aliases {
				filename := filepath.Join(toolDirectory, alias)
				if err := os.Remove(filename); err != nil && !os.IsNotExist(err) {
					return err
				}
				if err := os.Symlink(proxy, filename); err != nil {
					return err
				}
			}
		}
	}
	environment, err := opts.evaluation.environment()
	if err != nil {
		return err
	}
	for name, value := range environment {
		value, err = opts.evaluation.normalizeToolsetPathCapabilities(value)
		if err != nil {
			return fmt.Errorf("native Kconfig environment %s: %w", name, err)
		}
		environment[name], err = toolsetpath.RewriteShell(value, resolver)
		if err != nil {
			return fmt.Errorf("native Kconfig environment %s: %w", name, err)
		}
	}
	environment["srctree"] = kbuildEvalSourceTree
	environment["PATH"] = toolDirectory
	environment["LC_ALL"] = "C"
	environment["TZ"] = "UTC"
	environment["KCONFIG_CONFIG"] = ".config"
	delete(environment, "KCONFIG_NOSILENTUPDATE")
	environment["KCONFIG_ALLCONFIG"] = ".config"
	if err := os.WriteFile(filepath.Join(work, ".config"), []byte(opts.seed), 0o644); err != nil {
		return err
	}
	executable, err := filepath.Abs(opts.executable)
	if err != nil {
		return err
	}
	aliasRoot, err := resolver.OpenShellAliasRoot()
	if err != nil {
		return err
	}
	defer aliasRoot.Close()
	// Resolve new choices without prompting, then generate the source's full
	// configuration projection, including its dependency marker files.
	for _, mode := range []string{opts.mode, "--syncconfig"} {
		command := exec.Command(executable, mode, "Kconfig")
		command.Dir = work
		for _, name := range slices.Sorted(maps.Keys(environment)) {
			command.Env = append(command.Env, name+"="+environment[name])
		}
		command.ExtraFiles = []*os.File{aliasRoot}
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("selected source conf %s: %w\n%s", mode, err, output)
		}
	}
	projection, err := readNativeConfigProjection(work)
	if err != nil {
		return err
	}
	if err := projection.rejectWorkerPaths(append(resolver.PhysicalRoots(), work, execroot, sourceRoot)); err != nil {
		return err
	}
	for path, contents := range projection.files {
		filename := filepath.Join(opts.output, path)
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
			return err
		}
	}
	return nil
}
