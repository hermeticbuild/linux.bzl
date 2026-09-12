package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	pathpkg "path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

type stringMapFlag map[string]string

func (f stringMapFlag) String() string {
	return fmt.Sprint(map[string]string(f))
}

func (f stringMapFlag) Set(value string) error {
	key, val, ok := strings.Cut(value, "=")
	if !ok || key == "" {
		return fmt.Errorf("expected KEY=VALUE")
	}
	f[key] = val
	return nil
}

// addKbuildOnlyVariables opens the phase boundary after reusable Kconfig
// replay. It returns the kernel-context variables used to derive the final
// source-owned architecture identity, then overlays consumer-only assignments
// onto variables for the actual Kbuild graph. Kbuild-only assignments
// intentionally override a shared assignment with the same name, matching GNU
// Make's final command-line value.
func addKbuildOnlyVariables(variables, kbuildVariables map[string]string) map[string]string {
	identityVariables := maps.Clone(variables)
	for name, value := range kbuildVariables {
		variables[name] = value
	}
	return identityVariables
}

func validateConfiguredKbuildInputs(
	variables, kbuildVariables map[string]string,
	targets, preparationTargets []string,
) error {
	for _, input := range []struct {
		name   string
		values map[string]string
	}{
		{name: "-var", values: variables},
		{name: "-kbuild_var", values: kbuildVariables},
	} {
		if err := kconfig.ValidateKbuildOrdinaryVariables(input.name, input.values); err != nil {
			return err
		}
	}
	for _, input := range []struct {
		name   string
		values []string
	}{
		{name: "-kbuild_target", values: targets},
		{name: "-kbuild_prepare_target", values: preparationTargets},
	} {
		for index, value := range input.values {
			if err := kconfig.ValidateKbuildOrdinaryValue(
				fmt.Sprintf("%s value %d", input.name, index+1), value,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

type linuxCompilerBootstrapPlan struct {
	plan   *kconfig.ProbePlan
	target kconfig.ProbeReference
	host   kconfig.ProbeReference
}

type linuxCompilerBootstrapResults struct {
	target *kconfig.LinuxCompilerFacts
	host   *kconfig.LinuxCompilerFacts
}

type linuxKconfigProbeEvaluation struct {
	tree                             *kconfig.Tree
	plan                             *kconfig.ProbePlan
	target                           sourceDerivedLinuxTarget
	normalizeToolsetPathCapabilities func(string) (string, error)
}

type linuxKbuildProbeValue struct {
	target               sourceDerivedLinuxTarget
	resolved             *kconfig.ResolvedConfig
	actionPlan           *kconfig.ActionPlan
	configDependencies   map[string]kconfig.ConfigDependencySet
	familyPlanningResult *kconfig.ActionPlanFamilyVariantPlanningResult
}

type linuxKbuildProbeOptions struct {
	tree                      *kconfig.Tree
	rootPath                  string
	kbuildPath                string
	configInput               string
	configOverlays            []string
	configFlags               map[string]string
	configMode                string
	variables                 map[string]string
	identityVariables         map[string]string
	sourceRoots               map[string]string
	sourceNamespaces          map[string]string
	objectRoot                string
	objectNamespace           string
	entryTargets              []string
	preparationTargets        []string
	selectedProductsOnly      bool
	analyzeConfigDependencies bool
	guardDiscoveryOnly        bool
	familyPlanningCache       *kconfig.ActionPlanFamilyPlanningCache
	familyVariantOptions      *kconfig.ActionPlanFamilyVariantPlanningOptions
	familyCompilerGuards      *familyCompilerGuardPipeline
	kbuildInputCache          *kbuildInvocationInputCache
	kernelVersion             string
	target                    sourceDerivedLinuxTarget
	targetFacts               *kconfig.LinuxCompilerFacts
	hostFacts                 *kconfig.LinuxCompilerFacts
	targetContract            *hostKbuildContract
	hostContract              *hostKbuildContract
	rustSourceRoot            string
	normalizeConfigValue      func(string) (string, error)
}

func newLinuxCompilerBootstrapPlan(targetIdentity, hostIdentity string) (*linuxCompilerBootstrapPlan, error) {
	builder, err := kconfig.NewProbePlanBuilder(targetIdentity, hostIdentity)
	if err != nil {
		return nil, err
	}
	target, err := kconfig.AddLinuxCompilerBootstrap(builder, "target")
	if err != nil {
		return nil, err
	}
	host, err := kconfig.AddLinuxCompilerBootstrap(builder, "host")
	if err != nil {
		return nil, err
	}
	plan, err := builder.Plan(target, host)
	if err != nil {
		return nil, err
	}
	return &linuxCompilerBootstrapPlan{plan: plan, target: target, host: host}, nil
}

func writeLinuxCompilerProbePlan(outputDir, targetIdentityRoot, hostIdentityRoot string) error {
	targetIdentity, err := readToolsetIdentity(targetIdentityRoot)
	if err != nil {
		return fmt.Errorf("target toolset identity: %w", err)
	}
	hostIdentity, err := readToolsetIdentity(hostIdentityRoot)
	if err != nil {
		return fmt.Errorf("host toolset identity: %w", err)
	}
	bootstrap, err := newLinuxCompilerBootstrapPlan(targetIdentity, hostIdentity)
	if err != nil {
		return err
	}
	return bootstrap.plan.Write(outputDir)
}

func loadLinuxCompilerBootstrapResults(targetRoot, hostRoot, targetIdentity, hostIdentity string) (*linuxCompilerBootstrapResults, error) {
	bootstrap, err := newLinuxCompilerBootstrapPlan(targetIdentity, hostIdentity)
	if err != nil {
		return nil, err
	}
	oracle, err := kconfig.NewProbeResultOracleFromTrees(
		map[string]string{"target": targetRoot, "host": hostRoot},
		map[string]string{"target": targetIdentity, "host": hostIdentity},
	)
	if err != nil {
		return nil, err
	}
	if err := oracle.ValidatePlan(bootstrap.plan); err != nil {
		return nil, err
	}
	targetResult, err := oracle.Result(bootstrap.target)
	if err != nil {
		return nil, err
	}
	targetFacts, err := kconfig.ParseLinuxCompilerBootstrapResult(targetResult, "target", targetIdentity)
	if err != nil {
		return nil, err
	}
	hostResult, err := oracle.Result(bootstrap.host)
	if err != nil {
		return nil, err
	}
	hostFacts, err := kconfig.ParseLinuxCompilerBootstrapResult(hostResult, "host", hostIdentity)
	if err != nil {
		return nil, err
	}
	return &linuxCompilerBootstrapResults{target: targetFacts, host: hostFacts}, nil
}

func evaluateLinuxKconfigProbes(
	ctx context.Context,
	root string,
	sourceRoot string,
	sourceRoots map[string]string,
	variables map[string]string,
	targetIdentity string,
	hostIdentity string,
	targetFacts *kconfig.LinuxCompilerFacts,
	targetContract *hostKbuildContract,
	hostContract *hostKbuildContract,
	rustSourceRoot string,
	oracle *kconfig.ProbeResultOracle,
	kbuildInputCaches ...*kbuildInvocationInputCache,
) (*linuxKconfigProbeEvaluation, error) {
	// Kconfig evaluation enriches these maps with source-derived compiler state,
	// scoped tool tokens, and its internal srctree. Those values belong only to
	// this evaluation: mutating the caller would make a later Kbuild phase treat
	// them as user-supplied GNU Make command-line assignments.
	variables = maps.Clone(variables)
	if variables == nil {
		variables = map[string]string{}
	}
	environment := map[string]string{}
	if configured := variables["srctree"]; configured != "" && filepath.Clean(configured) != filepath.Clean(sourceRoot) {
		return nil, fmt.Errorf("symbolic Linux Kconfig requires srctree=%q, got %q", sourceRoot, configured)
	}
	variables["srctree"] = sourceRoot
	builder, err := kconfig.NewProbePlanBuilder(targetIdentity, hostIdentity)
	if err != nil {
		return nil, err
	}
	if targetFacts == nil || targetContract == nil || hostContract == nil {
		return nil, fmt.Errorf("symbolic Linux Kconfig requires selected target and host toolsets")
	}
	var kbuildInputCache *kbuildInvocationInputCache
	if len(kbuildInputCaches) != 0 {
		kbuildInputCache = kbuildInputCaches[0]
	}
	if kbuildInputCache == nil {
		kbuildInputCache = &kbuildInvocationInputCache{}
	}
	sourceCache := kbuildInvocationSourceProgramCache(
		kbuildInputCache, sourceRoot, sourceRoot, sourceRoots,
	)
	tools := make(map[string]string, len(targetContract.Actions))
	for role, action := range targetContract.Actions {
		if action.Path != "" {
			tools[role] = action.Path
		}
	}
	var lookup kconfig.ProbeResultLookup
	if oracle != nil {
		lookup = oracle
	}
	bootstrapArchitecture, err := kconfig.LinuxCompilerMachineArchitecture(targetFacts.Machine())
	if err != nil {
		return nil, err
	}
	bootstrapEvaluator, err := kconfig.NewLinuxProbeEvaluator(kconfig.LinuxProbeEvaluatorOptions{
		Scope:              "target",
		Architecture:       bootstrapArchitecture,
		SourceRoot:         sourceRoot,
		SourceArchitecture: bootstrapArchitecture,
		ScriptEnvironment:  linuxProbeScriptEnvironment(bootstrapArchitecture, bootstrapArchitecture, "target", variables, targetContract.MakeVariables, tools, rustSourceRoot),
		Facts:              targetFacts,
		Tools:              tools,
		Discovery:          builder,
		Oracle:             lookup,
		RustSourceRoot:     rustSourceRoot,
	})
	if err != nil {
		return nil, err
	}
	target, err := sourceDerivedLinuxKconfigIdentity(
		ctx, sourceRoot, variables,
		targetContract, hostContract, bootstrapEvaluator, sourceCache,
	)
	if err != nil {
		return nil, err
	}
	for name, value := range map[string]string{"ARCH": target.Arch, "SRCARCH": target.Srcarch} {
		if configured, ok := variables[name]; ok && configured != value {
			return nil, fmt.Errorf("source-derived Linux target requires %s=%q, got %q", name, value, configured)
		}
		variables[name] = value
	}
	policyScriptEnvironment := linuxProbeScriptEnvironment(target.Arch, target.Srcarch, "target", variables, targetContract.MakeVariables, tools, rustSourceRoot)
	policyEvaluator, err := kconfig.NewLinuxProbeEvaluator(kconfig.LinuxProbeEvaluatorOptions{
		Scope:              "target",
		Architecture:       target.Arch,
		SourceRoot:         sourceRoot,
		SourceArchitecture: target.Srcarch,
		ScriptEnvironment:  policyScriptEnvironment,
		Facts:              targetFacts,
		Tools:              tools,
		Discovery:          builder,
		Oracle:             lookup,
		RustSourceRoot:     rustSourceRoot,
	})
	if err != nil {
		return nil, err
	}
	sourceEnvironment, err := sourceDerivedLinuxKconfigEnvironment(
		ctx, sourceRoot, target.Arch, variables, environment,
		targetContract, hostContract, policyEvaluator, sourceCache,
	)
	if err != nil {
		return nil, err
	}
	for name, value := range sourceEnvironment {
		environment[name] = value
	}
	finalScriptEnvironment := linuxProbeScriptEnvironment(target.Arch, target.Srcarch, "target", variables, targetContract.MakeVariables, tools, rustSourceRoot)
	scopedFinalEnvironment, err := linuxProbeEnvironmentForScope("target", environment)
	if err != nil {
		return nil, err
	}
	for name, value := range scopedFinalEnvironment {
		finalScriptEnvironment[name] = value
	}
	finalEvaluator, err := policyEvaluator.WithScriptEnvironment(finalScriptEnvironment)
	if err != nil {
		return nil, fmt.Errorf("create final source-exported Kconfig probe evaluator: %w", err)
	}
	tree, err := kconfig.ParseFile(ctx, root, kconfig.Options{
		RootDir:         sourceRoot,
		SourceRoots:     sourceRoots,
		Variables:       variables,
		Env:             environment,
		Shell:           finalEvaluator.Shell,
		ResolveSymbolic: finalEvaluator.ResolveSymbolic,
	})
	if err != nil {
		return nil, err
	}
	terminals := append(bootstrapEvaluator.References(), policyEvaluator.References()...)
	terminals = append(terminals, finalEvaluator.References()...)
	plan, err := builder.Plan(terminals...)
	if err != nil {
		return nil, err
	}
	if oracle != nil {
		if err := oracle.ValidatePlan(plan); err != nil {
			return nil, err
		}
	}
	return &linuxKconfigProbeEvaluation{
		tree:                             tree,
		plan:                             plan,
		target:                           target,
		normalizeToolsetPathCapabilities: finalEvaluator.NormalizeOrAuthorizeToolsetPathCapabilities,
	}, nil
}

func linuxProbeScriptEnvironment(
	arch, srcarch, scope string,
	variables, makeVariables, tools map[string]string,
	rustSourceRoot string,
) map[string]string {
	environment := map[string]string{
		"ARCH": arch, "SRCARCH": srcarch,
	}
	for variable, role := range makeVariables {
		if tools[role] != "" {
			environment[variable] = kbuildActionRoleMakeCommand(scope, role)
		}
	}
	if rustSourceRoot != "" {
		environment["KRUSTFLAGS"] = variables["KRUSTFLAGS"]
		environment["RUST_LIB_SRC"] = rustSourceRoot
	}
	return environment
}

// linuxProbeEnvironmentForScope retains source-exported values which are
// role-free or whose action-role provenance belongs to the selected scope.
// This keeps arbitrary source policy dynamic without exposing target action
// tokens to host probes (or vice versa).
func linuxProbeEnvironmentForScope(scope string, environment map[string]string) (map[string]string, error) {
	result := make(map[string]string, len(environment))
	for name, value := range environment {
		refs, err := kconfig.KbuildActionRoleRefs(value)
		if err != nil {
			return nil, fmt.Errorf("source-exported environment %s: %w", name, err)
		}
		selected := true
		for _, ref := range refs {
			if ref.Scope != kconfig.KbuildActionRoleAutoScope && ref.Scope != scope {
				selected = false
				break
			}
		}
		if selected {
			result[name] = value
		}
	}
	return result, nil
}

// sourceDerivedLinuxKconfigEnvironment evaluates the exact environment that
// the selected kernel's root Makefile exports to Kconfig. The source tree owns
// compiler identification, compiler-specific setup, and the names and values
// of every field visible to Kconfig; this adapter carries no compiler flag
// inventory. Source-defined probes remain in the same DAG consumed by Kconfig.
func sourceDerivedLinuxKconfigEnvironment(
	ctx context.Context,
	sourceRoot, arch string,
	variables map[string]string,
	environment map[string]string,
	target, host *hostKbuildContract,
	evaluator *kconfig.LinuxProbeEvaluator,
	sourceCaches ...*kconfig.KbuildSourceCache,
) (map[string]string, error) {
	var sourceCache *kconfig.KbuildSourceCache
	if len(sourceCaches) != 0 {
		sourceCache = sourceCaches[0]
	}
	root, err := filepath.Abs(sourceRoot)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Linux compiler-policy source root %q: %w", sourceRoot, err)
	}
	root = filepath.Clean(root)
	values := linuxRootKconfigInvocationVariables(root)
	configured := make(map[string]string, len(variables)+2)
	for name, value := range variables {
		// MAKECMDGOALS is invocation-local state synthesized by the planner,
		// never a caller-configured Make command-line assignment.
		if name == "MAKECMDGOALS" {
			continue
		}
		values[name] = value
		configured[name] = value
	}
	values["ARCH"] = arch
	values["SUBARCH"] = arch
	configured["ARCH"] = arch
	configured["SUBARCH"] = arch
	values = kbuildInvocationSentinelVariables(root, values, "")
	commandLine, err := kbuildCommandLineVariables(target, host, configured)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"abs_srctree", "objtree", "srctree"} {
		commandLine[name] = values[name]
	}
	shell := func(command string) (string, error) {
		value, probeErr := evaluator.KbuildShell(ctx, command)
		if probeErr == nil {
			return value, nil
		}
		if !kconfig.IsLinuxProbeUnsupportedCommand(probeErr) {
			return "", probeErr
		}
		return hermeticLinuxKbuildShell(command, root, target, host)
	}
	parsed, err := kconfig.ParseKbuildFileTree(filepath.Join(root, "Makefile"), kconfig.KbuildOptions{
		RootDir:                        root,
		Variables:                      values,
		EnvironmentVariables:           maps.Clone(environment),
		CommandLineVariables:           commandLine,
		AutoExportCommandLineVariables: kbuildConfiguredCommandLineAutoExports(configured, target, host),
		SourceRoots:                    map[string]string{kbuildEvalSourceTree: root, kbuildEvalObjectTree: root},
		ConfigVariablesComplete:        true,
		MakeVariablesComplete:          true,
		Shell:                          shell,
		ResolveSymbolic:                evaluator.ResolveSymbolic,
		SelectSymbolic:                 evaluator.SelectSymbolic,
		TransformSymbolic:              evaluator.TransformSymbolic,
		SourceCache:                    sourceCache,
	})
	if err != nil {
		return nil, fmt.Errorf("evaluate source-derived Linux Kconfig environment: %w", err)
	}
	exportedEnvironment := parsed.ExportedEnvironment()
	for name, value := range exportedEnvironment {
		if strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("source-derived Linux Kconfig environment %s contains NUL", name)
		}
	}
	return exportedEnvironment, nil
}

func kbuildProbeTools(
	contract *hostKbuildContract,
	facts *kconfig.LinuxCompilerFacts,
) (map[string]string, error) {
	if contract == nil || facts == nil {
		return nil, fmt.Errorf("symbolic Kbuild probes require selected actions and compiler facts")
	}
	tools := make(map[string]string, len(contract.Actions))
	for role, action := range contract.Actions {
		if action.Path != "" {
			tools[role] = action.Path
		}
	}
	return tools, nil
}

func evaluateLinuxKbuildProbes(
	opts linuxKbuildProbeOptions,
	oracle *kconfig.ProbeResultOracle,
) (*kconfig.KbuildProbeEvaluation[linuxKbuildProbeValue], error) {
	if opts.guardDiscoveryOnly && (oracle == nil || opts.familyVariantOptions == nil || opts.familyCompilerGuards == nil) {
		return nil, fmt.Errorf("compiler guard discovery requires an ordinary replay oracle, family replay options and supplemental coordinator")
	}
	if opts.kbuildInputCache == nil {
		opts.kbuildInputCache = &kbuildInvocationInputCache{}
	}
	sourceRoot, err := workspaceDirectory(opts.rootPath)
	if err != nil {
		return nil, fmt.Errorf("resolve Kbuild probe source root: %w", err)
	}
	targetTools, err := kbuildProbeTools(opts.targetContract, opts.targetFacts)
	if err != nil {
		return nil, fmt.Errorf("configure target Kbuild probe scope: %w", err)
	}
	hostTools, err := kbuildProbeTools(opts.hostContract, opts.hostFacts)
	if err != nil {
		return nil, fmt.Errorf("configure host Kbuild probe scope: %w", err)
	}
	targetScriptEnvironment := linuxProbeScriptEnvironment(opts.target.Arch, opts.target.Srcarch, "target", opts.variables, opts.targetContract.MakeVariables, targetTools, opts.rustSourceRoot)
	hostScriptEnvironment := linuxProbeScriptEnvironment(opts.target.Arch, opts.target.Srcarch, "host", opts.variables, opts.hostContract.MakeVariables, hostTools, "")
	hostScope := &kconfig.KbuildProbeScopeOptions{
		Architecture:       opts.target.Arch,
		SourceArchitecture: opts.target.Srcarch,
		SourceRoot:         sourceRoot,
		ScriptEnvironment:  hostScriptEnvironment,
		Facts:              opts.hostFacts,
		Tools:              hostTools,
	}
	return kconfig.EvaluateKbuildProbeWorkload(
		kconfig.KbuildProbeWorkloadOptions{
			Target: kconfig.KbuildProbeScopeOptions{
				Architecture:       opts.target.Arch,
				SourceArchitecture: opts.target.Srcarch,
				SourceRoot:         sourceRoot,
				ScriptEnvironment:  targetScriptEnvironment,
				Facts:              opts.targetFacts,
				Tools:              targetTools,
				RustSourceRoot:     opts.rustSourceRoot,
			},
			Host: hostScope,
		},
		oracle,
		func(scopes *kconfig.KbuildProbeScopes) (linuxKbuildProbeValue, error) {
			identitySourceCache := kbuildInvocationSourceProgramCache(
				opts.kbuildInputCache, sourceRoot, sourceRoot, opts.sourceRoots,
			)
			target, err := sourceDerivedLinuxMakeIdentity(
				opts.rootPath,
				opts.target.Arch,
				opts.identityVariables,
				opts.sourceRoots,
				opts.targetContract,
				opts.hostContract,
				scopes,
				identitySourceCache,
			)
			if err != nil {
				return linuxKbuildProbeValue{}, err
			}
			for name, pair := range map[string][2]string{
				"ARCH":    {target.Arch, opts.target.Arch},
				"SRCARCH": {target.Srcarch, opts.target.Srcarch},
			} {
				if pair[0] != pair[1] {
					return linuxKbuildProbeValue{}, fmt.Errorf("source-derived Kbuild %s=%q differs from Kconfig %q", name, pair[0], pair[1])
				}
			}
			target.Machine = opts.targetFacts.Machine()
			variables := make(map[string]string, len(opts.variables)+1)
			for name, value := range opts.variables {
				variables[name] = value
			}
			variables["UTS_MACHINE"] = target.UTSMachine
			metadata, resolved, err := compactMetadata(
				opts.tree,
				opts.rootPath,
				opts.kbuildPath,
				opts.configInput,
				opts.configOverlays,
				opts.configFlags,
				opts.configMode,
				variables,
				opts.sourceRoots,
				opts.sourceNamespaces,
				opts.objectRoot,
				opts.objectNamespace,
				opts.entryTargets,
				opts.preparationTargets,
				opts.selectedProductsOnly,
				opts.kbuildInputCache,
				opts.kernelVersion,
				opts.targetContract,
				opts.hostContract,
				scopes,
				opts.normalizeConfigValue,
			)
			if err != nil {
				return linuxKbuildProbeValue{}, err
			}
			// Action lowering is part of the probe workload itself. Some compiler
			// and source-script expressions are reached only after the selected
			// Kbuild graph has been lowered; building the plan after the workload
			// would register those probes too late for discovery/replay parity.
			// Discovery needs that complete native lowering traversal, but not the
			// product facades, SDK projections, final content addressing, or node
			// sorting of the action graph it discards.
			var actionPlan *kconfig.ActionPlan
			if oracle == nil {
				err = metadata.DiscoverActionPlanProbes(
					opts.targetFacts.ToolsetIdentity(),
					opts.hostFacts.ToolsetIdentity(),
				)
			} else if opts.familyVariantOptions != nil {
				options := *opts.familyVariantOptions
				if options.InitialSnapshot != nil {
					options.ResolvedConfigFiles = resolvedConfigObjectTreeContents(opts.tree, resolved, opts.kernelVersion)
				}
				if opts.familyCompilerGuards != nil {
					options.PrepareCompilerGuards = func() error {
						return opts.familyCompilerGuards.prepareVariant(options.Variant, scopes, metadata)
					}
				}
				var result *kconfig.ActionPlanFamilyVariantPlanningResult
				var planErr error
				if opts.guardDiscoveryOnly {
					planErr = metadata.DiscoverVerifiedCompilerGuards(
						opts.targetFacts.ToolsetIdentity(), opts.hostFacts.ToolsetIdentity(), options,
					)
				} else {
					result, planErr = metadata.ActionPlanFamilyVariant(
						opts.targetFacts.ToolsetIdentity(), opts.hostFacts.ToolsetIdentity(), options,
					)
				}
				if planErr != nil {
					return linuxKbuildProbeValue{}, planErr
				}
				if opts.familyCompilerGuards != nil {
					if err := opts.familyCompilerGuards.finishVariant(options.Variant, scopes); err != nil {
						return linuxKbuildProbeValue{}, err
					}
				}
				if opts.guardDiscoveryOnly {
					return linuxKbuildProbeValue{target: target, resolved: resolved}, nil
				}
				return linuxKbuildProbeValue{
					target: target, resolved: resolved, actionPlan: result.Plan,
					configDependencies: result.Dependencies, familyPlanningResult: result,
				}, nil
			} else if opts.analyzeConfigDependencies {
				var dependencies map[string]kconfig.ConfigDependencySet
				actionPlan, dependencies, err = metadata.ActionPlanWithFamilyPlanningCache(
					opts.targetFacts.ToolsetIdentity(), opts.hostFacts.ToolsetIdentity(), opts.familyPlanningCache,
				)
				if err == nil {
					return linuxKbuildProbeValue{target: target, resolved: resolved, actionPlan: actionPlan, configDependencies: dependencies}, nil
				}
			} else {
				actionPlan, err = metadata.ActionPlan(
					opts.targetFacts.ToolsetIdentity(), opts.hostFacts.ToolsetIdentity(),
				)
			}
			if err != nil {
				return linuxKbuildProbeValue{}, err
			}
			return linuxKbuildProbeValue{target: target, resolved: resolved, actionPlan: actionPlan}, nil
		},
	)
}

type stringSliceFlag []string

func (f *stringSliceFlag) String() string {
	return strings.Join(*f, ",")
}

type configuredKbuildAction struct {
	Path        string
	PrefixArgs  []string
	SuffixArgs  []string
	Environment map[string]string
}

const configuredKbuildToolsetSchema = toolaction.KbuildToolsetManifestSchema
const configuredKbuildArgsSentinel = "__LINUX_BZL_KBUILD_ARGS_V1__"

type configuredKbuildToolsetManifest = toolaction.KbuildToolsetManifest

func readConfiguredKbuildToolsetManifest(filename, scope, identity string) (configuredKbuildToolsetManifest, error) {
	manifest, err := toolaction.ReadKbuildToolsetManifest(workspacePath(filename))
	if err != nil {
		return configuredKbuildToolsetManifest{}, err
	}
	if manifest.Scope != scope {
		return configuredKbuildToolsetManifest{}, fmt.Errorf("toolset manifest identifies schema/scope %q/%q, want %q/%q", manifest.Schema, manifest.Scope, configuredKbuildToolsetSchema, scope)
	}
	for role, argv := range manifest.Actions {
		count := 0
		for _, argument := range argv {
			if argument == configuredKbuildArgsSentinel {
				count++
			}
		}
		if count > 1 {
			return configuredKbuildToolsetManifest{}, fmt.Errorf("toolset action role %q repeats the Kbuild argument sentinel", role)
		}
	}
	got, err := manifest.Identity()
	if err != nil {
		return configuredKbuildToolsetManifest{}, err
	}
	if got != identity {
		return configuredKbuildToolsetManifest{}, fmt.Errorf("toolset manifest identity is %q, want independently measured %q", got, identity)
	}
	return manifest, nil
}

func configuredKbuildActionsFromManifest(manifest configuredKbuildToolsetManifest) map[string]configuredKbuildAction {
	actions := make(map[string]configuredKbuildAction, len(manifest.Actions))
	for role, argv := range manifest.Actions {
		sentinel := slices.Index(argv, configuredKbuildArgsSentinel)
		prefix, suffix := []string{}, []string{}
		if sentinel >= 0 {
			prefix = slices.Clone(argv[:sentinel])
			suffix = slices.Clone(argv[sentinel+1:])
		}
		actions[role] = configuredKbuildAction{
			Path: manifest.Tools[role], PrefixArgs: prefix, SuffixArgs: suffix,
			Environment: maps.Clone(manifest.Environments[role]),
		}
	}
	return actions
}

type hostKbuildContract struct {
	Actions         map[string]configuredKbuildAction
	CompilerMachine string
	MakeVariables   map[string]string
}

func configuredKbuildContract(scope string, actions map[string]configuredKbuildAction, compilerFacts *kconfig.LinuxCompilerFacts) (*hostKbuildContract, error) {
	provided := 0
	for role, action := range actions {
		for name := range action.Environment {
			if name == "" || strings.ContainsAny(name, "=\x00") {
				return nil, fmt.Errorf("%s %s probe environment has invalid name %q", scope, role, name)
			}
		}
		if action.Path != "" {
			provided++
		}
		if action.Path == "" && (len(action.PrefixArgs) != 0 || len(action.SuffixArgs) != 0) {
			return nil, fmt.Errorf("%s probe arguments require their selected action tool", scope)
		}
	}
	if provided == 0 {
		for role, action := range actions {
			if len(action.Environment) != 0 {
				return nil, fmt.Errorf("%s_probe_%s_env requires its selected action tool", scope, role)
			}
		}
		return nil, nil
	}
	if provided != len(actions) {
		return nil, fmt.Errorf("%s Kbuild action contract is incomplete", scope)
	}
	for role, action := range actions {
		action.Path = workspacePath(action.Path)
		actions[role] = action
	}
	if compilerFacts == nil {
		return nil, fmt.Errorf("selected %s Kbuild actions require replayed compiler facts", scope)
	}
	if compilerFacts.Scope() != scope {
		return nil, fmt.Errorf("selected %s compiler facts have scope %q", scope, compilerFacts.Scope())
	}
	return &hostKbuildContract{
		Actions: actions, CompilerMachine: compilerFacts.Machine(),
	}, nil
}

// configuredRustSourceRoot validates only the configured source-tree mapping.
// The selected kernel's own Kconfig probes decide whether the independently
// configured Rust-related action roles are sufficient for Rust support.
func configuredRustSourceRoot(
	_, _ *hostKbuildContract,
	variables map[string]string,
	sourceRoots map[string]string,
) (string, error) {
	root := strings.TrimSpace(variables["RUST_LIB_SRC"])
	if root == "" {
		return "", nil
	}
	if filepath.IsAbs(root) || strings.Contains(root, "\\") || pathpkg.Clean(root) != root || root == "." || strings.HasPrefix(root, "../") {
		return "", fmt.Errorf("RUST_LIB_SRC=%q is not a canonical relative source root", root)
	}
	mapped, ok := sourceRoots[root]
	if !ok {
		return "", fmt.Errorf("selected Rust source root %q has no -source_root_map", root)
	}
	if filepath.Clean(mapped) != filepath.Clean(workspacePath(root)) {
		return "", fmt.Errorf("selected Rust source root %q maps to %q", root, mapped)
	}
	return root, nil
}

func (f *stringSliceFlag) Set(value string) error {
	if value == "" {
		return fmt.Errorf("empty value")
	}
	*f = append(*f, value)
	return nil
}

type namedPath struct {
	Name string
	Path string
}

type namedPathFlag []namedPath

func (f *namedPathFlag) String() string {
	parts := make([]string, len(*f))
	for i, value := range *f {
		parts[i] = value.Name + "=" + value.Path
	}
	return strings.Join(parts, ",")
}

func (f *namedPathFlag) Set(value string) error {
	name, path, ok := strings.Cut(value, "=")
	if !ok || name == "" || path == "" {
		return fmt.Errorf("expected NAME=PATH")
	}
	*f = append(*f, namedPath{Name: name, Path: path})
	return nil
}

func namedPathMap(values []namedPath) map[string]string {
	out := map[string]string{}
	for _, value := range values {
		out[value.Name] = workspacePath(value.Path)
	}
	return out
}

type familyPlanFlags struct {
	variants         stringSliceFlag
	overlays         namedPathFlag
	resolvedArch     namedPathFlag
	resolvedConfig   namedPathFlag
	resolvedAutoConf namedPathFlag
	resolvedCmd      namedPathFlag
	resolvedAutoconf namedPathFlag
	resolvedRustcCfg namedPathFlag
	resolvedRelease  namedPathFlag
	snapshots        namedPathFlag
}

func (f *familyPlanFlags) requested() bool {
	return f != nil && (len(f.variants) != 0 || len(f.overlays) != 0 ||
		len(f.resolvedArch) != 0 || len(f.resolvedConfig) != 0 ||
		len(f.resolvedAutoConf) != 0 || len(f.resolvedCmd) != 0 ||
		len(f.resolvedAutoconf) != 0 || len(f.resolvedRustcCfg) != 0 ||
		len(f.resolvedRelease) != 0 || len(f.snapshots) != 0)
}

type familyPlanVariantRequest struct {
	name     string
	overlay  string
	arch     string
	resolved resolvedConfigOutputs
	snapshot string
}

func familyPlanNamedPaths(
	flagName string,
	values []namedPath,
	variants map[string]bool,
	required bool,
) (map[string]string, error) {
	result := make(map[string]string, len(values))
	for _, value := range values {
		if !variants[value.Name] {
			return nil, fmt.Errorf("-%s references unknown family variant %q", flagName, value.Name)
		}
		if _, exists := result[value.Name]; exists {
			return nil, fmt.Errorf("-%s repeats family variant %q", flagName, value.Name)
		}
		result[value.Name] = workspacePath(value.Path)
	}
	if required {
		for variant := range variants {
			if result[variant] == "" {
				return nil, fmt.Errorf("-%s is missing family variant %q", flagName, variant)
			}
		}
	}
	return result, nil
}

func (f *familyPlanFlags) requests() ([]familyPlanVariantRequest, error) {
	return f.requestsWithOutputs(true)
}

func (f *familyPlanFlags) requestsWithOutputs(outputs bool) ([]familyPlanVariantRequest, error) {
	if f == nil || len(f.variants) == 0 {
		return nil, fmt.Errorf("family planning requires -family_plan_variant")
	}
	variantSet := make(map[string]bool, len(f.variants))
	for _, name := range f.variants {
		if !validSourceDerivedLinuxIdentity(name) {
			return nil, fmt.Errorf("invalid family variant name %q", name)
		}
		if variantSet[name] {
			return nil, fmt.Errorf("duplicate family variant %q", name)
		}
		variantSet[name] = true
	}
	type namedValues struct {
		name     string
		values   []namedPath
		required bool
	}
	fields := []namedValues{
		{name: "family_plan_overlay", values: f.overlays},
		{name: "family_plan_resolved_arch_out", values: f.resolvedArch, required: true},
		{name: "family_plan_resolved_config_out", values: f.resolvedConfig, required: true},
		{name: "family_plan_resolved_auto_conf_out", values: f.resolvedAutoConf, required: true},
		{name: "family_plan_resolved_auto_conf_cmd_out", values: f.resolvedCmd, required: true},
		{name: "family_plan_resolved_autoconf_out", values: f.resolvedAutoconf, required: true},
		{name: "family_plan_resolved_rustc_cfg_out", values: f.resolvedRustcCfg, required: true},
		{name: "family_plan_resolved_kernel_release_out", values: f.resolvedRelease, required: true},
		{name: "family_plan_snapshot_out", values: f.snapshots, required: true},
	}
	byFlag := make(map[string]map[string]string, len(fields))
	for _, field := range fields {
		if !outputs && field.required && len(field.values) != 0 {
			return nil, fmt.Errorf("compiler guard discovery does not accept -%s", field.name)
		}
		values, err := familyPlanNamedPaths(field.name, field.values, variantSet, outputs && field.required)
		if err != nil {
			return nil, err
		}
		byFlag[field.name] = values
	}
	names := slices.Sorted(maps.Keys(variantSet))
	requests := make([]familyPlanVariantRequest, 0, len(names))
	for _, name := range names {
		requests = append(requests, familyPlanVariantRequest{
			name:    name,
			overlay: byFlag["family_plan_overlay"][name],
			arch:    byFlag["family_plan_resolved_arch_out"][name],
			resolved: resolvedConfigOutputs{
				config:        byFlag["family_plan_resolved_config_out"][name],
				autoConf:      byFlag["family_plan_resolved_auto_conf_out"][name],
				autoConfCmd:   byFlag["family_plan_resolved_auto_conf_cmd_out"][name],
				autoconf:      byFlag["family_plan_resolved_autoconf_out"][name],
				rustcCfg:      byFlag["family_plan_resolved_rustc_cfg_out"][name],
				kernelRelease: byFlag["family_plan_resolved_kernel_release_out"][name],
			},
			snapshot: byFlag["family_plan_snapshot_out"][name],
		})
	}
	return requests, nil
}

func mergeProbePlanInputs(values []namedPath) (*kconfig.ProbePlan, error) {
	values = slices.Clone(values)
	sort.Slice(values, func(i, j int) bool { return values[i].Name < values[j].Name })
	for index, value := range values {
		if index != 0 && values[index-1].Name == value.Name {
			return nil, fmt.Errorf("duplicate probe-plan union input %q", value.Name)
		}
	}
	variants := make([]kconfig.ProbePlanVariant, 0, len(values))
	for _, value := range values {
		plan, err := kconfig.ReadProbePlan(workspacePath(value.Path))
		if err != nil {
			return nil, fmt.Errorf("invalid probe-plan union input %q: %w", value.Name, err)
		}
		variants = append(variants, kconfig.ProbePlanVariant{Name: value.Name, Plan: plan})
	}
	return kconfig.MergeProbePlans(variants)
}

func actionPlanStageOutputMap(values []namedPath) (map[string]string, error) {
	outputs := map[string]string{}
	for _, value := range values {
		if !kconfig.LinuxKernelPlanStages[value.Name] {
			return nil, fmt.Errorf("unknown action-plan stage %q", value.Name)
		}
		if _, exists := outputs[value.Name]; exists {
			return nil, fmt.Errorf("duplicate action-plan stage %q", value.Name)
		}
		outputs[value.Name] = workspacePath(value.Path)
	}
	for _, stage := range []string{"prehost", "bootstrap", "host", "prep", "target"} {
		if outputs[stage] == "" {
			return nil, fmt.Errorf("missing action-plan stage %q", stage)
		}
	}
	return outputs, nil
}

func actionPlanFamilySegmentOutputMap(values []namedPath) (map[string]string, error) {
	outputs := map[string]string{}
	for _, value := range values {
		if !kconfig.LinuxKernelFamilyPlanSegments[value.Name] {
			return nil, fmt.Errorf("unknown action-plan family segment %q", value.Name)
		}
		if _, exists := outputs[value.Name]; exists {
			return nil, fmt.Errorf("duplicate action-plan family segment %q", value.Name)
		}
		outputs[value.Name] = workspacePath(value.Path)
	}
	for _, segment := range []string{"prehost", "bootstrap", "host", "target"} {
		if outputs[segment] == "" {
			return nil, fmt.Errorf("missing action-plan family segment %q", segment)
		}
	}
	return outputs, nil
}

const (
	hermeticKbuildBuildVersion      = "1"
	hermeticKbuildBuildTimestamp    = "1970-01-01T00:00:00Z"
	hermeticKbuildBuildUser         = "bazel"
	hermeticKbuildBuildHost         = "bazel"
	hermeticKbuildHostKernelRelease = "linux.bzl-unavailable"
)

type kbuildInvocationRequest struct {
	name, makefile, directory string
	processLocation           kconfig.CompactKbuildInvocationLocation
	entryTargets              []string
	environment               map[string]string
	variables                 map[string]string
	commandLineAutoExport     map[string]bool
	// visibleState is the process-local immutable object-tree snapshot. Request
	// identity hashes its canonical observable digest; cumulative slice/map
	// projections are never constructed.
	visibleState              kbuildFrontierState
	invocationPredecessors    []string
	suppressParentCommandLine bool
}

type sourceDerivedLinuxTarget struct {
	Machine    string
	Arch       string
	Srcarch    string
	UTSMachine string
}

func sourceDerivedLinuxKconfigIdentity(
	ctx context.Context,
	sourceRoot string,
	variables map[string]string,
	target, host *hostKbuildContract,
	evaluator *kconfig.LinuxProbeEvaluator,
	sourceCaches ...*kconfig.KbuildSourceCache,
) (sourceDerivedLinuxTarget, error) {
	var sourceCache *kconfig.KbuildSourceCache
	if len(sourceCaches) != 0 {
		sourceCache = sourceCaches[0]
	}
	if target == nil || host == nil || evaluator == nil {
		return sourceDerivedLinuxTarget{}, fmt.Errorf("source-derived Linux architecture requires selected target/host tools and probe evaluator")
	}
	root, err := workspaceDirectory(sourceRoot)
	if err != nil {
		return sourceDerivedLinuxTarget{}, fmt.Errorf("resolve Linux architecture source root %q: %w", sourceRoot, err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return sourceDerivedLinuxTarget{}, fmt.Errorf("canonicalize Linux architecture source root %q: %w", sourceRoot, err)
	}
	root = filepath.Clean(root)
	values := linuxRootMakeInvocationVariables(root)
	configured := make(map[string]string, len(variables))
	for name, value := range variables {
		switch name {
		case "ARCH", "SRCARCH", "SUBARCH", "UTS_MACHINE", "srctree":
			// Architecture is an output of this source evaluation. Supplying a
			// matching value is allowed, but it must not select its own result.
			continue
		}
		values[name] = value
		configured[name] = value
	}
	values = kbuildInvocationSentinelVariables(root, values, "")
	commandLine, err := kbuildCommandLineVariables(target, host, configured)
	if err != nil {
		return sourceDerivedLinuxTarget{}, err
	}
	for _, name := range []string{"abs_srctree", "objtree", "srctree"} {
		commandLine[name] = values[name]
	}
	shell := func(command string) (string, error) {
		value, probeErr := evaluator.KbuildShell(ctx, command)
		if probeErr == nil {
			return value, nil
		}
		if !kconfig.IsLinuxProbeUnsupportedCommand(probeErr) {
			return "", probeErr
		}
		return hermeticLinuxKbuildShell(command, root, target, host)
	}
	parsed, err := kconfig.ParseKbuildFileTree(filepath.Join(root, "Makefile"), kconfig.KbuildOptions{
		RootDir:                        root,
		Variables:                      values,
		CommandLineVariables:           commandLine,
		AutoExportCommandLineVariables: map[string]bool{},
		SourceRoots:                    map[string]string{kbuildEvalSourceTree: root, kbuildEvalObjectTree: root},
		ConfigVariablesComplete:        true,
		MakeVariablesComplete:          true,
		Shell:                          shell,
		ResolveSymbolic:                evaluator.ResolveSymbolic,
		SelectSymbolic:                 evaluator.SelectSymbolic,
		TransformSymbolic:              evaluator.TransformSymbolic,
		CaptureVariables:               []string{"ARCH", "SRCARCH", "UTS_MACHINE"},
		SkipExportedVariables:          true,
		SourceCache:                    sourceCache,
	})
	if err != nil {
		return sourceDerivedLinuxTarget{}, fmt.Errorf("evaluate source-derived Linux architecture: %w", err)
	}
	identity := sourceDerivedLinuxTarget{
		Machine:    target.CompilerMachine,
		Arch:       strings.TrimSpace(parsed.Variables["ARCH"]),
		Srcarch:    strings.TrimSpace(parsed.Variables["SRCARCH"]),
		UTSMachine: strings.TrimSpace(parsed.Variables["UTS_MACHINE"]),
	}
	for name, value := range map[string]string{"ARCH": identity.Arch, "SRCARCH": identity.Srcarch, "UTS_MACHINE": identity.UTSMachine} {
		if !validSourceDerivedLinuxIdentity(value) {
			return sourceDerivedLinuxTarget{}, fmt.Errorf("source Makefiles returned invalid %s=%q", name, value)
		}
	}
	return identity, nil
}

func validSourceDerivedLinuxIdentity(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("_.+-", character) {
			continue
		}
		return false
	}
	return true
}

func sourceDerivedLinuxMakeIdentity(
	sourceRoot, arch string,
	vars map[string]string,
	sourceRoots map[string]string,
	target, host *hostKbuildContract,
	probeScopes *kconfig.KbuildProbeScopes,
	sourceCaches ...*kconfig.KbuildSourceCache,
) (sourceDerivedLinuxTarget, error) {
	var sourceCache *kconfig.KbuildSourceCache
	if len(sourceCaches) != 0 {
		sourceCache = sourceCaches[0]
	}
	root, err := workspaceDirectory(sourceRoot)
	if err != nil {
		return sourceDerivedLinuxTarget{}, fmt.Errorf("resolve Kbuild source root %q: %w", sourceRoot, err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return sourceDerivedLinuxTarget{}, fmt.Errorf("canonicalize Kbuild source root %q: %w", sourceRoot, err)
	}
	root = filepath.Clean(root)
	values := linuxRootMakeInvocationVariables(root)
	configured := make(map[string]string, len(vars)+2)
	for name, value := range vars {
		values[name] = value
		configured[name] = value
	}
	values["ARCH"] = arch
	values["SUBARCH"] = arch
	configured["ARCH"] = arch
	configured["SUBARCH"] = arch
	values = kbuildInvocationSentinelVariables(root, values, "")
	commandLineVariables, err := kbuildCommandLineVariables(target, host, configured)
	if err != nil {
		return sourceDerivedLinuxTarget{}, err
	}
	for _, name := range []string{"abs_srctree", "objtree", "srctree"} {
		commandLineVariables[name] = values[name]
	}
	mappedSourceRoots := maps.Clone(sourceRoots)
	if mappedSourceRoots == nil {
		mappedSourceRoots = map[string]string{}
	}
	// Identity inputs can refer to additional declared source trees, so retain
	// their mappings while pinning the two canonical roots to this kernel. More
	// specific mappings win for filesystem functions such as $(realpath ...).
	// Consumer-only variables such as M are intentionally absent here: Linux's
	// outer external-module driver does not include the architecture Makefile
	// which defines the final UTS_MACHINE. They remain visible to the actual
	// compactMetadata Kbuild evaluation below this identity phase.
	mappedSourceRoots[kbuildEvalSourceTree] = root
	mappedSourceRoots[kbuildEvalObjectTree] = root
	options, err := probeScopes.Options("target", kconfig.KbuildOptions{
		RootDir:                        root,
		Variables:                      values,
		CommandLineVariables:           commandLineVariables,
		AutoExportCommandLineVariables: map[string]bool{},
		SourceRoots:                    mappedSourceRoots,
		ConfigVariablesComplete:        true,
		MakeVariablesComplete:          true,
		CaptureVariables:               []string{"ARCH", "SRCARCH", "UTS_MACHINE"},
		SkipExportedVariables:          true,
		Shell: func(command string) (string, error) {
			return hermeticLinuxKbuildShell(command, root, target, host)
		},
		SourceCache: sourceCache,
	})
	if err != nil {
		return sourceDerivedLinuxTarget{}, err
	}
	parsed, err := kconfig.ParseKbuildFileTree(filepath.Join(root, "Makefile"), options)
	if err != nil {
		return sourceDerivedLinuxTarget{}, err
	}
	identity := sourceDerivedLinuxTarget{
		Arch: strings.TrimSpace(parsed.Variables["ARCH"]), Srcarch: strings.TrimSpace(parsed.Variables["SRCARCH"]), UTSMachine: strings.TrimSpace(parsed.Variables["UTS_MACHINE"]),
	}
	for name, value := range map[string]string{"ARCH": identity.Arch, "SRCARCH": identity.Srcarch, "UTS_MACHINE": identity.UTSMachine} {
		if value == "" || strings.ContainsAny(value, "\x00\r\n/\\") {
			return sourceDerivedLinuxTarget{}, fmt.Errorf("top Makefile returned invalid %s=%q", name, value)
		}
	}
	return identity, nil
}

func hermeticLinuxKbuildShell(command, sourceRoot string, target, host *hostKbuildContract) (string, error) {
	if value, handled, err := evaluateHermeticKbuildDirectoryQuery(command, sourceRoot); handled {
		return value, err
	}
	commandPath := command
	for _, sentinel := range []string{kbuildEvalSourceTree, kbuildEvalObjectTree} {
		if commandPath == sentinel {
			commandPath = sourceRoot
			break
		}
		if suffix, ok := strings.CutPrefix(commandPath, sentinel+"/"); ok {
			commandPath = filepath.Join(sourceRoot, filepath.FromSlash(suffix))
			break
		}
	}
	if !filepath.IsAbs(commandPath) {
		commandPath = filepath.Join(sourceRoot, filepath.FromSlash(commandPath))
	}
	resolvedCommandPath, _ := filepath.EvalSymlinks(commandPath)
	resolvedBuildVersionPath, _ := filepath.EvalSymlinks(filepath.Join(sourceRoot, "scripts", "build-version"))
	if commandPath == filepath.Join(sourceRoot, "scripts", "build-version") ||
		(resolvedCommandPath != "" && resolvedCommandPath == resolvedBuildVersionPath) {
		// scripts/build-version reads and mutates .version. Planning uses the
		// same normalized build-version contract as the versionheaders action.
		return hermeticKbuildBuildVersion, nil
	}
	if command == "LC_ALL=C date" {
		// Wall-clock time cannot affect a hermetic plan. This is the normalized
		// timestamp emitted by the versionheaders action as well.
		return hermeticKbuildBuildTimestamp, nil
	}
	if command == "uname -r" {
		// scripts/kconfig/Makefile only uses this value to search ambient host
		// configuration paths when ARCH happens to match SUBARCH. Those files
		// are not declared Bazel inputs, so use a stable non-existent release
		// instead of making the plan depend on the local or RBE worker kernel.
		return hermeticKbuildHostKernelRelease, nil
	}
	if strings.HasPrefix(command, "set -e;") && strings.Contains(command, "if () >/dev/null 2>&1;") && strings.Contains(command, `then echo "";`) && strings.Contains(command, `else echo "";`) {
		// An empty compiler try-run can arise while evaluating a definition
		// before any concrete cc-option call supplies argument 1. Both branches
		// are exactly empty, so the result is deterministic without execution.
		return "", nil
	}
	if expression, ok := strings.CutPrefix(command, "expr "); ok {
		return evaluateHermeticIntegerExpression(expression)
	}
	unsupportedErr := fmt.Errorf("unsupported hermetic Kbuild shell command %q", command)
	if strings.Contains(command, kbuildEvalSourceTree) || strings.Contains(command, kbuildEvalObjectTree) {
		return "", kbuildInvocationPhysicalPathError{err: unsupportedErr}
	}
	return "", unsupportedErr
}

func evaluateHermeticKbuildDirectoryQuery(command, sourceRoot string) (string, bool, error) {
	commands, connectors, err := kbuildShellSimpleCommands(command)
	if err != nil {
		return "", false, nil
	}
	for _, simple := range commands {
		if len(simple.redirections) != 0 {
			return "", false, nil
		}
	}
	// Root Kbuild uses $(shell mkdir -p $(output)) before recursively invoking
	// the external module Makefile. The mapped execution graph materializes
	// every output directory itself, so planning records this mutation as an
	// already-satisfied directory declaration instead of touching the planner
	// machine or synthesizing a shell recipe.
	if len(commands) == 1 && len(connectors) == 0 && len(commands[0].argv) == 3 &&
		commands[0].argv[0] == "mkdir" && commands[0].argv[1] == "-p" {
		directory := filepath.ToSlash(filepath.Clean(commands[0].argv[2]))
		if directory == kbuildEvalObjectTree || strings.HasPrefix(directory, kbuildEvalObjectTree+"/") {
			return "", true, nil
		}
	}
	root, err := filepath.Abs(sourceRoot)
	if err != nil {
		return "", true, err
	}
	root = filepath.Clean(root)
	objectPath := func(value string) (string, error) {
		value = filepath.Clean(value)
		for _, sentinel := range []string{kbuildEvalSourceTree, kbuildEvalObjectTree} {
			if value == sentinel {
				value = root
				break
			}
			prefix := sentinel + string(filepath.Separator)
			if strings.HasPrefix(value, prefix) {
				value = filepath.Join(root, strings.TrimPrefix(value, prefix))
				break
			}
		}
		relative, err := filepath.Rel(root, value)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			escapeErr := fmt.Errorf("Kbuild directory query path %q escapes declared invocation root %q", value, root)
			if strings.Contains(value, kbuildEvalSourceTree) || strings.Contains(value, kbuildEvalObjectTree) {
				return "", kbuildInvocationPhysicalPathError{err: escapeErr}
			}
			return "", escapeErr
		}
		if relative == "." {
			return kbuildEvalObjectTree, nil
		}
		return kbuildEvalObjectTree + "/" + filepath.ToSlash(relative), nil
	}
	// tools/scripts/Makefile.include validates O before deriving ABSOLUTE_O.
	// The object tree is declared even though it does not exist while the
	// planner itself runs, so the exact existence test succeeds.
	if len(commands) == 3 && slices.Equal(connectors, []string{";", "||"}) &&
		slices.Equal(commands[0].argv, []string{"cd"}) && len(commands[1].argv) == 3 &&
		commands[1].argv[0] == "test" && commands[1].argv[1] == "-d" &&
		len(commands[2].argv) == 2 && commands[2].argv[0] == "echo" &&
		commands[1].argv[2] == commands[2].argv[1] {
		if _, err := objectPath(commands[1].argv[2]); err != nil {
			return "", true, err
		}
		return "", true, nil
	}
	if len(commands) == 3 && slices.Equal(connectors, []string{";", "||"}) &&
		len(commands[0].argv) == 2 && commands[0].argv[0] == "cd" &&
		len(commands[1].argv) == 3 && commands[1].argv[0] == "test" && commands[1].argv[1] == "-d" &&
		len(commands[2].argv) == 2 && commands[2].argv[0] == "echo" &&
		commands[1].argv[2] == commands[2].argv[1] {
		if _, err := objectPath(commands[0].argv[1]); err != nil {
			return "", true, err
		}
		if _, err := objectPath(commands[1].argv[2]); err != nil {
			return "", true, err
		}
		return "", true, nil
	}
	// Resolve the exact pwd forms used to canonicalize O/OUTPUT without
	// consulting the planner machine's filesystem.
	if len(commands) == 2 && slices.Equal(connectors, []string{"&&"}) &&
		len(commands[0].argv) == 2 && commands[0].argv[0] == "cd" && slices.Equal(commands[1].argv, []string{"pwd"}) {
		value, err := objectPath(commands[0].argv[1])
		return value, true, err
	}
	if len(commands) == 3 && slices.Equal(connectors, []string{";", ";"}) &&
		len(commands[0].argv) == 2 && commands[0].argv[0] == "cd" &&
		len(commands[1].argv) == 2 && commands[1].argv[0] == "cd" && slices.Equal(commands[2].argv, []string{"pwd"}) {
		if _, err := objectPath(commands[0].argv[1]); err != nil {
			return "", true, err
		}
		value, err := objectPath(commands[1].argv[1])
		return value, true, err
	}
	return "", false, nil
}

type kbuildInvocationPhysicalPathError struct {
	err error
}

func (e kbuildInvocationPhysicalPathError) Error() string { return e.err.Error() }
func (e kbuildInvocationPhysicalPathError) Unwrap() error { return e.err }

func kbuildCommandLineVariables(targetContract, host *hostKbuildContract, configured map[string]string) (map[string]string, error) {
	if targetContract == nil || host == nil {
		return nil, fmt.Errorf("Kbuild command-line variables require target/host toolsets")
	}
	// Every explicitly configured -var is a Make command-line variable.  That
	// distinction matters for upstream makefiles which provide a host-default
	// assignment: configured hermetic dependencies must override the default
	// without evaluating its ambient discovery command first.
	// Source-owned metadata scripts otherwise discover the executing user and
	// hostname, making identical actions produce different bytes on RBE workers.
	// These reproducibility defaults are ordinary, exported Make assignments,
	// not compiler capabilities or flags; explicit nonempty values remain inputs.
	out := map[string]string{
		"KBUILD_BUILD_USER": hermeticKbuildBuildUser,
		"KBUILD_BUILD_HOST": hermeticKbuildBuildHost,
	}
	for name, value := range configured {
		if (name == "KBUILD_BUILD_USER" || name == "KBUILD_BUILD_HOST") && value == "" {
			return nil, fmt.Errorf("%s must be nonempty to avoid ambient build metadata", name)
		}
		out[name] = value
	}
	for variable, role := range targetContract.MakeVariables {
		hostRole, shared := host.MakeVariables[variable]
		if shared {
			if hostRole != role {
				return nil, fmt.Errorf(
					"target and host toolset manifests bind Make variable %s to different roles (%s and %s)",
					variable, role, hostRole,
				)
			}
			out[variable] = kbuildActionRoleMakeCommand(kconfig.KbuildActionRoleAutoScope, role)
			continue
		}
		out[variable] = kbuildActionRoleMakeCommand("target", role)
	}
	for variable, role := range host.MakeVariables {
		if _, shared := targetContract.MakeVariables[variable]; shared {
			continue
		}
		out[variable] = kbuildActionRoleMakeCommand("host", role)
	}
	return out, nil
}

// kbuildActionRoleMakeCommand replaces a Kbuild tool executable with its
// toolchain-selected role. Source Makefiles retain ownership of command modes
// and arguments; for example, Linux defines CPP as `$(CC) -E`.
func kbuildActionRoleMakeCommand(scope, role string) string {
	return kconfig.KbuildActionRoleToken(scope, role)
}

func kbuildConfiguredCommandLineAutoExports(
	configured map[string]string,
	targetContract, hostContract *hostKbuildContract,
) map[string]bool {
	exported := map[string]bool{"KBUILD_BUILD_USER": true, "KBUILD_BUILD_HOST": true}
	for name := range configured {
		if targetContract != nil {
			if _, internal := targetContract.MakeVariables[name]; internal {
				continue
			}
		}
		if hostContract != nil {
			if _, internal := hostContract.MakeVariables[name]; internal {
				continue
			}
		}
		exported[name] = true
	}
	return exported
}

func evaluateHermeticIntegerExpression(expression string) (string, error) {
	fields := strings.Fields(strings.ReplaceAll(expression, `\*`, `*`))
	if len(fields) == 0 || len(fields)%2 == 0 {
		return "", fmt.Errorf("unsupported integer expression %q", expression)
	}
	values := make([]int64, 0, (len(fields)+1)/2)
	operators := make([]string, 0, len(fields)/2)
	for index, field := range fields {
		if index%2 == 0 {
			value, err := strconv.ParseInt(field, 10, 64)
			if err != nil {
				return "", fmt.Errorf("unsupported integer expression %q", expression)
			}
			values = append(values, value)
			continue
		}
		if field != "+" && field != "-" && field != "*" {
			return "", fmt.Errorf("unsupported integer expression %q", expression)
		}
		operators = append(operators, field)
	}
	for index := 0; index < len(operators); {
		if operators[index] != "*" {
			index++
			continue
		}
		values[index] *= values[index+1]
		values = append(values[:index+1], values[index+2:]...)
		operators = append(operators[:index], operators[index+1:]...)
	}
	result := values[0]
	for index, operator := range operators {
		if operator == "+" {
			result += values[index+1]
		} else {
			result -= values[index+1]
		}
	}
	return strconv.FormatInt(result, 10), nil
}

func main() {
	os.Exit(run())
}

func run() (exitCode int) {
	var (
		root                        = flag.String("root", "", "Root Kconfig file to parse")
		srctree                     = flag.String("srctree", "", "Source tree used to resolve source statements")
		kbuildPath                  = flag.String("kbuild", "", "Kbuild/Makefile path for action-plan generation")
		actionPlanStageOuts         namedPathFlag
		actionPlanSnapshotOut       = flag.String("action_plan_snapshot_out", "", "Deterministic gzip transport of a canonical lossless action-plan snapshot for image-family reduction")
		familyPlan                  familyPlanFlags
		familyExecution             familyExecutionFlags
		familyCompilerGuards        familyCompilerGuardFlags
		actionPlanFamilyVariants    namedPathFlag
		actionPlanFamilySegmentOuts namedPathFlag
		actionPlanFamilyOut         = flag.String("action_plan_family_out", "", "Deprecated unified image-family action-plan TreeArtifact")
		actionPlanReuseReportOut    = flag.String("action_plan_reuse_report_out", "", "Canonical image-family reuse report")
		probePlanUnionInputs        namedPathFlag
		probePlanUnionOut           = flag.String("probe_plan_union_out", "", "Unified probe-plan TreeArtifact")
		targetToolsetIdentity       = flag.String("target_toolset_identity", "", "Directory containing the independently measured target toolset identity marker")
		hostToolsetIdentity         = flag.String("host_toolset_identity", "", "Directory containing the independently measured host toolset identity marker")
		targetToolsetManifest       = flag.String("target_toolset_manifest", "", "Identity-bound target Kbuild toolset manifest")
		hostToolsetManifest         = flag.String("host_toolset_manifest", "", "Identity-bound host Kbuild toolset manifest")
		probePlanOut                = flag.String("probe_plan_out", "", "Directory to write the execution-time compiler/Kconfig probe plan")
		targetProbeResults          = flag.String("target_probe_results", "", "Target-scoped probe result TreeArtifact consumed by final planning")
		hostProbeResults            = flag.String("host_probe_results", "", "Host-scoped probe result TreeArtifact consumed by final planning")
		kconfigProbePlanOut         = flag.String("kconfig_probe_plan_out", "", "Directory to write the source-derived Kconfig capability probe plan")
		targetKconfigProbeResults   = flag.String("target_kconfig_probe_results", "", "Target-scoped Kconfig capability result TreeArtifact consumed by replay")
		hostKconfigProbeResults     = flag.String("host_kconfig_probe_results", "", "Host-scoped Kconfig capability result TreeArtifact consumed by replay")
		kbuildProbePlanOut          = flag.String("kbuild_probe_plan_out", "", "Directory to write the source-derived Kbuild capability probe plan")
		targetKbuildProbeResults    = flag.String("target_kbuild_probe_results", "", "Target-scoped Kbuild capability result TreeArtifact consumed by final replay")
		hostKbuildProbeResults      = flag.String("host_kbuild_probe_results", "", "Host-scoped Kbuild capability result TreeArtifact consumed by final replay")
		objectRoot                  = flag.String("object_root", "", "Preconfigured Kbuild object tree used as the object namespace")
		objectNamespace             = flag.String("object_namespace", "", "Action-plan tree namespace for files below -object_root")
		selectedProductsOnly        = flag.Bool("selected_products_only", false, "Plan only source-selected Kbuild products and omit kernel facade products")
		resolveConfig               = flag.String("resolve_config", "", ".config input path to resolve through Kconfig defaults and dependencies")
		resolveConfigOverlays       stringSliceFlag
		configMode                  = flag.String("config_mode", "default", "Config resolver mode. Supported: default, allnoconfig")
		resolvedArchOut             = flag.String("resolved_arch_out", "", "Path to write the exact source-derived Linux ARCH")
		resolvedConfigOut           = flag.String("resolved_config_out", "", "Path to write the resolved .config")
		resolvedAutoConfOut         = flag.String("resolved_auto_conf_out", "", "Path to write the resolved include/config/auto.conf")
		resolvedCmdOut              = flag.String("resolved_auto_conf_cmd_out", "", "Path to write the resolved include/config/auto.conf.cmd")
		resolvedAutoconfOut         = flag.String("resolved_autoconf_out", "", "Path to write the resolved include/generated/autoconf.h")
		resolvedRustcCfgOut         = flag.String("resolved_rustc_cfg_out", "", "Path to write the resolved include/generated/rustc_cfg")
		resolvedReleaseOut          = flag.String("resolved_kernel_release_out", "", "Path to write the resolved include/config/kernel.release")
		kernelVersion               = flag.String("kernel_version", "", "Base kernel release used for resolved config and Kbuild action planning")
		heapProfile                 = flag.String("heap_profile", "", "Optional sampled heap profile; requires the separate heap diagnostic binary and bounded CPU profiling")
		cpuProfile                  = flag.String("cpu_profile", "", "Optional diagnostic CPU profile output; requires -profile_duration")
		profileDuration             = flag.Duration("profile_duration", 0, "Diagnostic wall-time limit: flush CPU profile and exit 124 when reached; requires -cpu_profile")
		vars                        = stringMapFlag{}
		kbuildVars                  = stringMapFlag{}
		sourceRootMaps              = namedPathFlag{}
		sourceNamespaces            = stringMapFlag{}
		kbuildTargets               stringSliceFlag
		kbuildPreparationTargets    stringSliceFlag
	)
	flag.Var(vars, "var", "Shared Kconfig/Kbuild variable in KEY=VALUE form. May be repeated")
	flag.Var(&actionPlanStageOuts, "action_plan_stage_out", "Stage-specific map_directory action plan in STAGE=PATH form. Must be repeated for every stage")
	flag.Var(&familyPlan.variants, "family_plan_variant", "Named image-family variant to resolve and plan. May be repeated")
	flag.Var(&familyPlan.overlays, "family_plan_overlay", "Optional variant config overlay in NAME=PATH form. May be repeated")
	flag.Var(&familyPlan.resolvedArch, "family_plan_resolved_arch_out", "Variant source-derived ARCH output in NAME=PATH form")
	flag.Var(&familyPlan.resolvedConfig, "family_plan_resolved_config_out", "Variant resolved .config output in NAME=PATH form")
	flag.Var(&familyPlan.resolvedAutoConf, "family_plan_resolved_auto_conf_out", "Variant resolved auto.conf output in NAME=PATH form")
	flag.Var(&familyPlan.resolvedCmd, "family_plan_resolved_auto_conf_cmd_out", "Variant resolved auto.conf.cmd output in NAME=PATH form")
	flag.Var(&familyPlan.resolvedAutoconf, "family_plan_resolved_autoconf_out", "Variant resolved autoconf.h output in NAME=PATH form")
	flag.Var(&familyPlan.resolvedRustcCfg, "family_plan_resolved_rustc_cfg_out", "Variant resolved rustc_cfg output in NAME=PATH form")
	flag.Var(&familyPlan.resolvedRelease, "family_plan_resolved_kernel_release_out", "Variant resolved kernel.release output in NAME=PATH form")
	flag.Var(&familyPlan.snapshots, "family_plan_snapshot_out", "Variant deterministic action-plan snapshot in NAME=PATH form")
	familyExecution.register(flag.CommandLine)
	familyCompilerGuards.register(flag.CommandLine)
	flag.Var(&actionPlanFamilyVariants, "action_plan_family_variant", "Image-family variant snapshot in NAME=PATH form. May be repeated")
	flag.Var(&actionPlanFamilySegmentOuts, "action_plan_family_segment_out", "Segment-specific image-family action plan in SEGMENT=PATH form. Must be repeated for prehost, bootstrap, host, and target")
	flag.Var(&probePlanUnionInputs, "probe_plan_union_input", "Named probe plan in NAME=PATH form. May be repeated")
	flag.Var(kbuildVars, "kbuild_var", "Kbuild-only variable in KEY=VALUE form. May be repeated")
	flag.Var(&resolveConfigOverlays, "resolve_config_overlay", "Overlay .config path merged after -resolve_config. May be repeated")
	flag.Var(&sourceRootMaps, "source_root_map", "Virtual source prefix to filesystem root in PREFIX=PATH form. May be repeated")
	flag.Var(sourceNamespaces, "source_namespace", "Canonical source prefix to action-plan tree namespace in PREFIX=NAMESPACE form. May be repeated")
	flag.Var(&kbuildTargets, "kbuild_target", "Top-level Kbuild goal. May be repeated")
	flag.Var(&kbuildPreparationTargets, "kbuild_prepare_target", "Top-level Kbuild goal whose source-derived closure is exported through the module SDK. May be repeated")
	flag.Parse()
	if (familyExecution.requested() || familyCompilerGuards.requested()) && !familyPlan.requested() {
		fmt.Fprintln(os.Stderr, "family execution requires -family_plan_variant and its complete outputs")
		return 2
	}
	if err := validateDiagnosticProfileOptions(*cpuProfile, *heapProfile, *profileDuration); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	profile, profileErr := startDiagnosticProfile(workspacePath(*cpuProfile), workspacePath(*heapProfile), *profileDuration, os.Stderr, os.Exit)
	if profileErr != nil {
		fmt.Fprintln(os.Stderr, profileErr)
		return 1
	}
	defer func() { exitCode = profile.finishExitCode(exitCode) }()
	profile.phase("start")
	if len(probePlanUnionInputs) != 0 || *probePlanUnionOut != "" {
		if familyPlan.requested() {
			fmt.Fprintln(os.Stderr, "probe-plan union and family planning are mutually exclusive")
			return 2
		}
		if len(probePlanUnionInputs) == 0 || *probePlanUnionOut == "" {
			fmt.Fprintln(os.Stderr, "probe-plan union requires -probe_plan_union_input and -probe_plan_union_out")
			return 2
		}
		merged, err := mergeProbePlanInputs(probePlanUnionInputs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid probe-plan union: %v\n", err)
			return 2
		}
		if err := merged.Write(workspacePath(*probePlanUnionOut)); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write probe-plan union: %v\n", err)
			return 1
		}
		return 0
	}
	if len(actionPlanFamilyVariants) != 0 || len(actionPlanFamilySegmentOuts) != 0 || *actionPlanFamilyOut != "" || *actionPlanReuseReportOut != "" {
		if familyPlan.requested() {
			fmt.Fprintln(os.Stderr, "image-family reduction and family planning are mutually exclusive")
			return 2
		}
		if *actionPlanFamilyOut != "" {
			fmt.Fprintln(os.Stderr, "-action_plan_family_out is no longer supported; use one -action_plan_family_segment_out for each execution segment")
			return 2
		}
		if len(actionPlanFamilyVariants) == 0 || len(actionPlanFamilySegmentOuts) == 0 || *actionPlanReuseReportOut == "" {
			fmt.Fprintln(os.Stderr, "image-family reduction requires -action_plan_family_variant, all four -action_plan_family_segment_out values, and -action_plan_reuse_report_out")
			return 2
		}
		segmentOutputs, err := actionPlanFamilySegmentOutputMap(actionPlanFamilySegmentOuts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid action-plan family segment outputs: %v\n", err)
			return 2
		}
		variants := make([]kconfig.ValidatedActionPlanFamilyVariant, 0, len(actionPlanFamilyVariants))
		for _, named := range actionPlanFamilyVariants {
			variant, err := kconfig.ReadActionPlanFamilyVariant(named.Name, workspacePath(named.Path))
			if err != nil {
				fmt.Fprintf(os.Stderr, "invalid action-plan family variant %s: %v\n", named.Name, err)
				return 2
			}
			variants = append(variants, variant)
		}
		if err := kconfig.BuildAndWriteActionPlanFamily(
			variants,
			segmentOutputs,
			workspacePath(*actionPlanReuseReportOut),
		); err != nil {
			fmt.Fprintf(os.Stderr, "failed to reduce and write image-family action plans: %v\n", err)
			return 1
		}
		return 0
	}
	if err := validateConfiguredKbuildInputs(
		vars, kbuildVars, []string(kbuildTargets), []string(kbuildPreparationTargets),
	); err != nil {
		fmt.Fprintf(os.Stderr, "invalid configured Kbuild input: %v\n", err)
		return 2
	}
	var familyPlanRequests []familyPlanVariantRequest
	if familyPlan.requested() {
		var err error
		familyPlanRequests, err = familyPlan.requestsWithOutputs(familyExecution.mode != "guards")
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid family planning request: %v\n", err)
			return 2
		}
	}
	familyExecutionRequest, err := familyExecution.request(familyPlanRequests)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid family execution request: %v\n", err)
		return 2
	}
	guardRoundInputs, err := familyCompilerGuards.validate(familyExecutionRequest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid supplemental compiler guard request: %v\n", err)
		return 2
	}

	selectedPlannerOutput := ""
	if len(familyPlanRequests) != 0 {
		selectedPlannerOutput = "-family_plan_variant"
	}
	for _, output := range []struct {
		name  string
		value string
	}{
		{"-probe_plan_out", *probePlanOut},
		{"-kconfig_probe_plan_out", *kconfigProbePlanOut},
		{"-kbuild_probe_plan_out", *kbuildProbePlanOut},
	} {
		if output.value == "" {
			continue
		}
		if selectedPlannerOutput != "" {
			fmt.Fprintf(os.Stderr, "%s and %s are mutually exclusive planner outputs\n", selectedPlannerOutput, output.name)
			return 2
		}
		selectedPlannerOutput = output.name
	}
	var actionPlanStageOutputs map[string]string
	if len(actionPlanStageOuts) != 0 {
		if selectedPlannerOutput != "" {
			fmt.Fprintf(os.Stderr, "%s and -action_plan_stage_out are mutually exclusive planner outputs\n", selectedPlannerOutput)
			return 2
		}
		selectedPlannerOutput = "-action_plan_stage_out"
		var err error
		actionPlanStageOutputs, err = actionPlanStageOutputMap(actionPlanStageOuts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid action-plan stage outputs: %v\n", err)
			return 2
		}
	}
	if *actionPlanSnapshotOut != "" && selectedPlannerOutput != "" {
		fmt.Fprintf(os.Stderr, "%s and -action_plan_snapshot_out are mutually exclusive planner outputs\n", selectedPlannerOutput)
		return 2
	}
	if len(familyPlanRequests) != 0 {
		legacyOutputs := []struct {
			name  string
			value string
		}{
			{name: "-resolved_arch_out", value: *resolvedArchOut},
			{name: "-resolved_config_out", value: *resolvedConfigOut},
			{name: "-resolved_auto_conf_out", value: *resolvedAutoConfOut},
			{name: "-resolved_auto_conf_cmd_out", value: *resolvedCmdOut},
			{name: "-resolved_autoconf_out", value: *resolvedAutoconfOut},
			{name: "-resolved_rustc_cfg_out", value: *resolvedRustcCfgOut},
			{name: "-resolved_kernel_release_out", value: *resolvedReleaseOut},
		}
		for _, output := range legacyOutputs {
			if output.value != "" {
				fmt.Fprintf(os.Stderr, "-family_plan_variant and %s are mutually exclusive outputs\n", output.name)
				return 2
			}
		}
		if len(resolveConfigOverlays) != 0 {
			fmt.Fprintln(os.Stderr, "-resolve_config_overlay cannot be combined with -family_plan_variant; use -family_plan_overlay")
			return 2
		}
	}

	resolvedConfigRequested := len(familyPlanRequests) != 0 || *resolvedConfigOut != "" || *resolvedAutoConfOut != "" || *resolvedCmdOut != "" || *resolvedAutoconfOut != "" || *resolvedRustcCfgOut != "" || *resolvedReleaseOut != ""
	kbuildPlanningRequested := len(familyPlanRequests) != 0 || *kbuildProbePlanOut != "" || *targetKbuildProbeResults != "" || len(actionPlanStageOutputs) != 0 || *actionPlanSnapshotOut != ""
	if len(kbuildVars) != 0 && !kbuildPlanningRequested {
		fmt.Fprintln(os.Stderr, "-kbuild_var requires Kbuild probe or action planning")
		return 2
	}

	// Probe discovery is a separate, pre-architecture planner invocation. It
	// intentionally reads only the two identity marker trees: configured tool
	// paths may be execution-only artifacts and must not be inspected here.
	if *probePlanOut != "" {
		if *targetProbeResults != "" || *hostProbeResults != "" || *kconfigProbePlanOut != "" || *targetKconfigProbeResults != "" || *hostKconfigProbeResults != "" ||
			*kbuildProbePlanOut != "" || *targetKbuildProbeResults != "" || *hostKbuildProbeResults != "" {
			fmt.Fprintln(os.Stderr, "-probe_plan_out cannot be combined with probe result trees or staged Kconfig/Kbuild probe planning")
			return 2
		}
		for flagName, value := range map[string]string{
			"-target_toolset_identity": *targetToolsetIdentity,
			"-host_toolset_identity":   *hostToolsetIdentity,
		} {
			if value == "" {
				fmt.Fprintf(os.Stderr, "%s is required with -probe_plan_out\n", flagName)
				return 2
			}
		}
		if err := writeLinuxCompilerProbePlan(
			workspacePath(*probePlanOut),
			workspacePath(*targetToolsetIdentity),
			workspacePath(*hostToolsetIdentity),
		); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write probe plan: %v\n", err)
			return 1
		}
		return 0
	}
	if *kconfigProbePlanOut == "" && *targetKconfigProbeResults == "" && *hostKconfigProbeResults == "" {
		fmt.Fprintln(os.Stderr, "Kconfig evaluation requires staged probe discovery or replay via -kconfig_probe_plan_out or target/host Kconfig probe results")
		return 2
	}
	if err := validateKernelVersion(*kernelVersion, resolvedConfigRequested || kbuildPlanningRequested); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if (*targetProbeResults == "") != (*hostProbeResults == "") {
		fmt.Fprintln(os.Stderr, "-target_probe_results and -host_probe_results must be supplied together")
		return 2
	}
	if (*targetKconfigProbeResults == "") != (*hostKconfigProbeResults == "") {
		fmt.Fprintln(os.Stderr, "-target_kconfig_probe_results and -host_kconfig_probe_results must be supplied together")
		return 2
	}
	if *kconfigProbePlanOut != "" && (*targetKconfigProbeResults != "" || *hostKconfigProbeResults != "") {
		fmt.Fprintln(os.Stderr, "-kconfig_probe_plan_out cannot be combined with Kconfig probe result trees")
		return 2
	}
	if *targetProbeResults == "" {
		fmt.Fprintln(os.Stderr, "Kconfig probe discovery and replay require target and host compiler probe results")
		return 2
	}
	if *kbuildProbePlanOut != "" && (*targetKbuildProbeResults != "" || *hostKbuildProbeResults != "") {
		fmt.Fprintln(os.Stderr, "-kbuild_probe_plan_out cannot be combined with Kbuild probe result trees")
		return 2
	}
	if (*targetKbuildProbeResults == "") != (*hostKbuildProbeResults == "") {
		fmt.Fprintln(os.Stderr, "-target_kbuild_probe_results and -host_kbuild_probe_results must be supplied together")
		return 2
	}
	if (*kbuildProbePlanOut != "" || *targetKbuildProbeResults != "") && *targetKconfigProbeResults == "" {
		fmt.Fprintln(os.Stderr, "Kbuild probe discovery and replay require replayed Kconfig probe results")
		return 2
	}

	// Bazel must receive artifacts, rather than File.dirname strings, in an
	// Args object for output-path mapping to work. Rules therefore pass the root
	// Kconfig File for directory-valued source-tree flags. Preserve the CLI's
	// directory form while normalizing a regular root marker to its parent.
	if *srctree != "" {
		directory, err := workspaceDirectory(*srctree)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid -srctree: %v\n", err)
			return 2
		}
		*srctree = directory
	}

	targetIdentity, err := readToolsetIdentity(workspacePath(*targetToolsetIdentity))
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid target toolset identity: %v\n", err)
		return 2
	}
	hostIdentity, err := readToolsetIdentity(workspacePath(*hostToolsetIdentity))
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid host toolset identity: %v\n", err)
		return 2
	}
	compilerBootstrap, err := loadLinuxCompilerBootstrapResults(
		workspacePath(*targetProbeResults),
		workspacePath(*hostProbeResults),
		targetIdentity,
		hostIdentity,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid compiler probe results: %v\n", err)
		return 2
	}
	targetCompilerFacts := compilerBootstrap.target
	hostCompilerFacts := compilerBootstrap.host

	for flagName, value := range map[string]string{
		"-target_toolset_manifest": *targetToolsetManifest,
		"-host_toolset_manifest":   *hostToolsetManifest,
	} {
		if value == "" {
			fmt.Fprintf(os.Stderr, "%s is required with compiler probe results\n", flagName)
			return 2
		}
	}
	targetManifest, manifestErr := readConfiguredKbuildToolsetManifest(*targetToolsetManifest, "target", targetIdentity)
	if manifestErr != nil {
		fmt.Fprintf(os.Stderr, "invalid target toolset manifest: %v\n", manifestErr)
		return 2
	}
	hostManifest, manifestErr := readConfiguredKbuildToolsetManifest(*hostToolsetManifest, "host", hostIdentity)
	if manifestErr != nil {
		fmt.Fprintf(os.Stderr, "invalid host toolset manifest: %v\n", manifestErr)
		return 2
	}
	targetContract, err := configuredKbuildContract("target", configuredKbuildActionsFromManifest(targetManifest), targetCompilerFacts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to configure selected target Kbuild actions: %v\n", err)
		return 2
	}
	hostContract, err := configuredKbuildContract("host", configuredKbuildActionsFromManifest(hostManifest), hostCompilerFacts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to configure selected host Kbuild actions: %v\n", err)
		return 2
	}
	targetContract.MakeVariables = maps.Clone(targetManifest.MakeVariables)
	hostContract.MakeVariables = maps.Clone(hostManifest.MakeVariables)
	if *srctree == "" {
		fmt.Fprintln(os.Stderr, "selected Linux tools require -srctree for source-derived architecture selection")
		return 2
	}

	if *root == "" {
		flag.PrintDefaults()
		return 2
	}

	var kbuildActionPlan *kconfig.ActionPlan
	var kbuildConfigDependencies map[string]kconfig.ConfigDependencySet
	var kbuildResolvedConfig *kconfig.ResolvedConfig
	sourceRoots := namedPathMap(sourceRootMaps)
	rustSourceRoot, rustErr := configuredRustSourceRoot(targetContract, hostContract, vars, sourceRoots)
	if rustErr != nil {
		fmt.Fprintf(os.Stderr, "invalid selected Rust toolset: %v\n", rustErr)
		return 2
	}
	resolvedRoot := *root
	resolvedSrctree := workspacePath(*srctree)
	kbuildInputCache := &kbuildInvocationInputCache{}
	var kconfigOracle *kconfig.ProbeResultOracle
	if *targetKconfigProbeResults != "" {
		kconfigOracle, err = kconfig.NewProbeResultOracleFromTrees(
			map[string]string{
				"target": workspacePath(*targetKconfigProbeResults),
				"host":   workspacePath(*hostKconfigProbeResults),
			},
			map[string]string{"target": targetIdentity, "host": hostIdentity},
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid Kconfig probe results: %v\n", err)
			return 2
		}
	}
	kconfigEvaluation, evaluateErr := evaluateLinuxKconfigProbes(
		context.Background(), resolvedRoot, resolvedSrctree, sourceRoots, vars,
		targetIdentity, hostIdentity,
		targetCompilerFacts, targetContract, hostContract,
		rustSourceRoot, kconfigOracle, kbuildInputCache,
	)
	if evaluateErr != nil {
		fmt.Fprintf(os.Stderr, "failed to evaluate Kconfig probes: %v\n", evaluateErr)
		return 1
	}
	if *kconfigProbePlanOut != "" {
		if err := kconfigEvaluation.plan.Write(workspacePath(*kconfigProbePlanOut)); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write Kconfig probe plan: %v\n", err)
			return 1
		}
		return 0
	}
	tree := kconfigEvaluation.tree
	selectedTarget := kconfigEvaluation.target
	// Kconfig probe results belong to the configured kernel and are reused by
	// external modules. Keep consumer-only variables out of that replay: M, for
	// example, switches the root Makefile into KBUILD_EXTMOD mode and changes the
	// probe graph. They become visible only to the subsequent Kbuild evaluation.
	vars["ARCH"] = selectedTarget.Arch
	vars["SRCARCH"] = selectedTarget.Srcarch
	identityVariables := addKbuildOnlyVariables(vars, kbuildVars)
	if len(familyPlanRequests) != 0 {
		if *targetKbuildProbeResults == "" {
			fmt.Fprintln(os.Stderr, "family planning requires staged target and host Kbuild probe results")
			return 2
		}
		baseConfig, err := readConfigFlags(*resolveConfig, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to read family base config: %v\n", err)
			return 1
		}
		oracle, err := kconfig.NewProbeResultOracleFromTrees(
			map[string]string{
				"target": workspacePath(*targetKbuildProbeResults),
				"host":   workspacePath(*hostKbuildProbeResults),
			},
			map[string]string{"target": targetIdentity, "host": hostIdentity},
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid Kbuild probe results: %v\n", err)
			return 2
		}
		familyPlanningCache := kconfig.NewActionPlanFamilyPlanningCache()
		if profile != nil {
			familyPlanningCache.SetConfigDependencyDiagnosticObserver(profile.observeConfigDependencies)
		}
		familyExecutionPipeline, err := newFamilyExecutionPipeline(familyExecutionRequest)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to load family execution inputs: %v\n", err)
			return 1
		}
		guardPipeline, err := newFamilyCompilerGuardPipeline(&familyCompilerGuards, guardRoundInputs, familyPlanRequests,
			map[string]string{"target": targetIdentity, "host": hostIdentity})
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to load supplemental compiler guards: %v\n", err)
			return 1
		}
		guardDiscoveryOnly := familyExecutionRequest != nil && familyExecutionRequest.mode == "guards"
		if guardDiscoveryOnly {
			carried, err := guardPipeline.carryForwardEmptyRound(familyExecutionRequest.mode)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to carry forward empty compiler guard round: %v\n", err)
				return 1
			}
			if carried {
				profile.phase("empty compiler guard carry-forward")
				return 0
			}
		}
		for _, request := range familyPlanRequests {
			profile.phase("variant " + request.name + " evaluation")
			configFlags, err := familyVariantConfigFlags(baseConfig, request.overlay)
			if err != nil {
				fmt.Fprintf(os.Stderr, "family variant %s overlay: %v\n", request.name, err)
				return 1
			}
			evaluation, evaluateErr := evaluateLinuxKbuildProbes(linuxKbuildProbeOptions{
				tree: tree, rootPath: *root, kbuildPath: *kbuildPath,
				configInput: *resolveConfig, configFlags: configFlags,
				configMode: *configMode, variables: maps.Clone(vars), identityVariables: maps.Clone(identityVariables), sourceRoots: sourceRoots,
				sourceNamespaces: sourceNamespaces,
				objectRoot:       *objectRoot, objectNamespace: *objectNamespace,
				entryTargets: kbuildTargets, preparationTargets: kbuildPreparationTargets,
				selectedProductsOnly:      *selectedProductsOnly,
				analyzeConfigDependencies: true,
				guardDiscoveryOnly:        guardDiscoveryOnly,
				familyPlanningCache:       familyPlanningCache,
				familyVariantOptions:      familyExecutionPipeline.variantOptions(request.name, familyPlanningCache),
				familyCompilerGuards:      guardPipeline,
				kbuildInputCache:          kbuildInputCache,
				kernelVersion:             *kernelVersion, target: selectedTarget,
				targetFacts: targetCompilerFacts, hostFacts: hostCompilerFacts,
				targetContract: targetContract, hostContract: hostContract,
				rustSourceRoot:       rustSourceRoot,
				normalizeConfigValue: kconfigEvaluation.normalizeToolsetPathCapabilities,
			}, oracle)
			if evaluateErr != nil {
				fmt.Fprintf(os.Stderr, "failed to plan family variant %s: %v\n", request.name, evaluateErr)
				return 1
			}
			value := evaluation.Value
			if value.resolved == nil || !guardDiscoveryOnly && value.actionPlan == nil {
				fmt.Fprintf(os.Stderr, "family variant %s did not produce a resolved action plan\n", request.name)
				return 1
			}
			if guardDiscoveryOnly {
				// This phase discovers pure compiler queries. It must not write
				// final snapshots or publish partial source-closure evidence.
				if value.actionPlan != nil || value.familyPlanningResult != nil || value.configDependencies != nil {
					fmt.Fprintf(os.Stderr, "compiler guard discovery variant %s returned final planning evidence\n", request.name)
					return 1
				}
				continue
			}
			profile.phase("variant " + request.name + " resolved outputs")
			if err := writeResolvedArchitecture(request.arch, value.target.Arch); err != nil {
				fmt.Fprintf(os.Stderr, "failed to write family variant %s source-derived Linux ARCH: %v\n", request.name, err)
				return 1
			}
			if err := writeResolvedConfigOutputs(tree, value.resolved, request.resolved, *kernelVersion); err != nil {
				fmt.Fprintf(os.Stderr, "failed to write family variant %s resolved config: %v\n", request.name, err)
				return 1
			}
			profile.phase("variant " + request.name + " snapshot")
			if err := kconfig.WriteActionPlanSnapshot(
				request.snapshot,
				value.actionPlan,
				value.configDependencies,
				resolvedConfigObjectTreeContents(tree, value.resolved, *kernelVersion),
			); err != nil {
				fmt.Fprintf(os.Stderr, "failed to write family variant %s action-plan snapshot: %v\n", request.name, err)
				return 1
			}
			if err := familyExecutionPipeline.record(request, value); err != nil {
				fmt.Fprintf(os.Stderr, "failed to retain family execution variant %s: %v\n", request.name, err)
				return 1
			}
			profile.phase("variant " + request.name + " complete")
		}
		if err := guardPipeline.publish(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to publish supplemental compiler guards: %v\n", err)
			return 1
		}
		if familyExecutionRequest != nil && familyExecutionRequest.mode == "guards" {
			return 0
		}
		if err := familyExecutionPipeline.publish(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to publish family execution plan: %v\n", err)
			return 1
		}
		return 0
	}

	if *kbuildProbePlanOut != "" || *targetKbuildProbeResults != "" {
		if *resolveConfig == "" {
			fmt.Fprintln(os.Stderr, "-resolve_config PATH is required for staged Kbuild probes")
			return 2
		}
		var oracle *kconfig.ProbeResultOracle
		if *targetKbuildProbeResults != "" {
			oracle, err = kconfig.NewProbeResultOracleFromTrees(
				map[string]string{
					"target": workspacePath(*targetKbuildProbeResults),
					"host":   workspacePath(*hostKbuildProbeResults),
				},
				map[string]string{"target": targetIdentity, "host": hostIdentity},
			)
			if err != nil {
				fmt.Fprintf(os.Stderr, "invalid Kbuild probe results: %v\n", err)
				return 2
			}
		}
		evaluation, evaluateErr := evaluateLinuxKbuildProbes(linuxKbuildProbeOptions{
			tree: tree, rootPath: *root, kbuildPath: *kbuildPath,
			configInput: *resolveConfig, configOverlays: resolveConfigOverlays,
			configMode: *configMode, variables: vars, identityVariables: identityVariables, sourceRoots: sourceRoots,
			sourceNamespaces: sourceNamespaces,
			objectRoot:       *objectRoot, objectNamespace: *objectNamespace,
			entryTargets: kbuildTargets, preparationTargets: kbuildPreparationTargets,
			selectedProductsOnly:      *selectedProductsOnly,
			analyzeConfigDependencies: *actionPlanSnapshotOut != "",
			kbuildInputCache:          kbuildInputCache,
			kernelVersion:             *kernelVersion, target: selectedTarget,
			targetFacts: targetCompilerFacts, hostFacts: hostCompilerFacts,
			targetContract: targetContract, hostContract: hostContract,
			rustSourceRoot:       rustSourceRoot,
			normalizeConfigValue: kconfigEvaluation.normalizeToolsetPathCapabilities,
		}, oracle)
		if evaluateErr != nil {
			fmt.Fprintf(os.Stderr, "failed to evaluate Kbuild probes: %v\n", evaluateErr)
			return 1
		}
		if *kbuildProbePlanOut != "" {
			if err := evaluation.Plan.Write(workspacePath(*kbuildProbePlanOut)); err != nil {
				fmt.Fprintf(os.Stderr, "failed to write Kbuild probe plan: %v\n", err)
				return 1
			}
			return 0
		}
		vars["UTS_MACHINE"] = evaluation.Value.target.UTSMachine
		kbuildResolvedConfig = evaluation.Value.resolved
		kbuildActionPlan = evaluation.Value.actionPlan
		if *actionPlanSnapshotOut != "" {
			kbuildConfigDependencies = evaluation.Value.configDependencies
		}
	}

	if *resolvedArchOut != "" {
		if err := writeResolvedArchitecture(*resolvedArchOut, selectedTarget.Arch); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write source-derived Linux ARCH: %v\n", err)
			return 1
		}
	}

	if resolvedConfigRequested {
		outputs := resolvedConfigOutputs{
			config:        *resolvedConfigOut,
			autoConf:      *resolvedAutoConfOut,
			autoConfCmd:   *resolvedCmdOut,
			autoconf:      *resolvedAutoconfOut,
			rustcCfg:      *resolvedRustcCfgOut,
			kernelRelease: *resolvedReleaseOut,
		}
		var err error
		if kbuildResolvedConfig != nil {
			err = writeResolvedConfigOutputs(tree, kbuildResolvedConfig, outputs, *kernelVersion)
		} else {
			err = writeResolvedConfig(tree, *resolveConfig, resolveConfigOverlays, *configMode, outputs, *kernelVersion, kconfigEvaluation.normalizeToolsetPathCapabilities)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to write resolved config: %v\n", err)
			return 1
		}
	}

	if len(actionPlanStageOutputs) != 0 {
		if kbuildActionPlan == nil {
			fmt.Fprintln(os.Stderr, "action-plan generation requires staged target and host Kbuild probe results")
			return 2
		}
		if err := kbuildActionPlan.WriteStages(actionPlanStageOutputs); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write action plan: %v\n", err)
			return 1
		}
	}
	if *actionPlanSnapshotOut != "" {
		if kbuildActionPlan == nil {
			fmt.Fprintln(os.Stderr, "action-plan snapshot generation requires staged target and host Kbuild probe results")
			return 2
		}
		configFiles, err := readResolvedConfigSnapshotFiles(resolvedConfigOutputs{
			config: *resolvedConfigOut, autoConf: *resolvedAutoConfOut, autoConfCmd: *resolvedCmdOut,
			autoconf: *resolvedAutoconfOut, rustcCfg: *resolvedRustcCfgOut, kernelRelease: *resolvedReleaseOut,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to read resolved config projections for action-plan snapshot: %v\n", err)
			return 1
		}
		if err := kconfig.WriteActionPlanSnapshot(
			workspacePath(*actionPlanSnapshotOut), kbuildActionPlan, kbuildConfigDependencies, configFiles,
		); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write action-plan snapshot: %v\n", err)
			return 1
		}
	}
	return 0
}

type resolvedConfigOutputs struct {
	config        string
	autoConf      string
	autoConfCmd   string
	autoconf      string
	rustcCfg      string
	kernelRelease string
}

func readResolvedConfigSnapshotFiles(outputs resolvedConfigOutputs) (map[string]string, error) {
	paths := map[string]string{
		".config":                       outputs.config,
		"include/config/auto.conf":      outputs.autoConf,
		"include/config/auto.conf.cmd":  outputs.autoConfCmd,
		"include/generated/autoconf.h":  outputs.autoconf,
		"include/generated/rustc_cfg":   outputs.rustcCfg,
		"include/config/kernel.release": outputs.kernelRelease,
	}
	files := make(map[string]string, len(paths))
	for logical, filename := range paths {
		if filename == "" {
			return nil, fmt.Errorf("resolved config projection %s has no output path", logical)
		}
		data, err := os.ReadFile(workspacePath(filename))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", logical, err)
		}
		files[logical] = string(data)
	}
	return files, nil
}

func writeResolvedArchitecture(path, arch string) error {
	return os.WriteFile(workspacePath(path), []byte(arch+"\n"), 0o644)
}

func writeResolvedConfig(
	tree *kconfig.Tree,
	input string,
	overlays []string,
	configMode string,
	outputs resolvedConfigOutputs,
	kernelVersion string,
	normalizeConfigValue func(string) (string, error),
) error {
	if input == "" {
		return fmt.Errorf("-resolve_config is required when resolved config outputs are requested")
	}
	missing := map[string]string{
		"-resolved_config_out":         outputs.config,
		"-resolved_auto_conf_out":      outputs.autoConf,
		"-resolved_auto_conf_cmd_out":  outputs.autoConfCmd,
		"-resolved_autoconf_out":       outputs.autoconf,
		"-resolved_rustc_cfg_out":      outputs.rustcCfg,
		"-resolved_kernel_release_out": outputs.kernelRelease,
	}
	var missingFlags []string
	for flagName, path := range missing {
		if path == "" {
			missingFlags = append(missingFlags, flagName)
		}
	}
	sort.Strings(missingFlags)
	if len(missingFlags) != 0 {
		return fmt.Errorf("missing required output flags: %s", strings.Join(missingFlags, ", "))
	}

	raw, err := readConfigFlags(input, overlays)
	if err != nil {
		return err
	}
	resolveOpts, err := resolveConfigOptions(configMode)
	if err != nil {
		return err
	}
	resolved, err := tree.ResolveConfigWithOptions(raw, resolveOpts)
	if err != nil {
		return err
	}
	if err := normalizeResolvedConfigValues(resolved, normalizeConfigValue); err != nil {
		return err
	}
	return writeResolvedConfigOutputs(tree, resolved, outputs, kernelVersion)
}

// readConfigFlags parses one base configuration and applies overlays in order.
// The returned map is caller-owned, which lets a family planner parse the base
// once and cheaply clone it before applying each variant overlay.
func readConfigFlags(input string, overlays []string) (map[string]string, error) {
	if input == "" {
		return nil, fmt.Errorf("-resolve_config PATH is required")
	}
	file, err := os.Open(workspacePath(input))
	if err != nil {
		return nil, err
	}
	raw, parseErr := kconfig.ParseConfig(file)
	closeErr := file.Close()
	if parseErr != nil {
		return nil, fmt.Errorf("%s: %w", input, parseErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("%s: %w", input, closeErr)
	}
	for _, overlay := range overlays {
		overlayFlags, err := parseConfigPath(workspacePath(overlay))
		if err != nil {
			return nil, fmt.Errorf("%s overlay %s: %w", input, overlay, err)
		}
		for key, value := range overlayFlags {
			raw[key] = value
		}
	}
	return raw, nil
}

func familyVariantConfigFlags(base map[string]string, overlay string) (map[string]string, error) {
	flags := maps.Clone(base)
	if flags == nil {
		flags = map[string]string{}
	}
	if overlay == "" {
		return flags, nil
	}
	overlayFlags, err := parseConfigPath(workspacePath(overlay))
	if err != nil {
		return nil, err
	}
	for key, value := range overlayFlags {
		flags[key] = value
	}
	return flags, nil
}

// normalizeResolvedConfigValues verifies workload-local compiler-path
// capabilities and replaces them with the deterministic runtime provenance
// representation before Kconfig values cross into generated files, Kbuild,
// or stable compact metadata. The resolver result is private to this call, so
// replacing its maps cannot mutate the parsed source tree or caller input.
// Generated config artifacts retain that deterministic provenance token, not
// a worker-local physical pathname. The typed Kconfig-to-Kbuild import below
// reauthorizes it for planning and recipe lowering; embedding or executing a
// path-valued CONFIG string as a physical runtime path is intentionally not
// supported until those generated contents have their own runtime projection.
func normalizeResolvedConfigValues(
	resolved *kconfig.ResolvedConfig,
	normalize func(string) (string, error),
) error {
	if resolved == nil || normalize == nil {
		return nil
	}
	normalizeMap := func(kind string, values map[string]string) (map[string]string, error) {
		if values == nil {
			return nil, nil
		}
		out := maps.Clone(values)
		keys := make([]string, 0, len(values))
		for key := range values {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value := values[key]
			quoteNormalized := false
			if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
				body := value[1 : len(value)-1]
				if strings.ContainsAny(body, "\x07\x08") {
					// Reserved provenance bytes are never ordinary Kconfig string
					// data. Strip only the outer quotes so the capability parser
					// can validate them without treating the closing quote as a
					// path suffix.
					value = body
					quoteNormalized = true
				} else if unquoted, decodeErr := strconv.Unquote(value); decodeErr == nil && strings.Contains(unquoted, toolaction.ExecutionRootProvenanceMarker) {
					// configScalarValue uses strconv.Quote, which renders the two
					// reserved delimiters as \a and \b. Decode that generated
					// representation only when it actually contains provenance.
					// All ordinary quoted values—including Linux-accepted escape
					// spellings which are not Go literals—remain byte-for-byte
					// untouched below.
					value = unquoted
					quoteNormalized = true
				}
			}
			value, err := normalize(value)
			if err != nil {
				return nil, fmt.Errorf("normalize %s Kconfig value %s: %w", kind, key, err)
			}
			if quoteNormalized {
				value = strconv.Quote(value)
			}
			out[key] = value
		}
		return out, nil
	}
	var err error
	resolved.Raw, err = normalizeMap("raw", resolved.Raw)
	if err != nil {
		return err
	}
	resolved.Effective, err = normalizeMap("effective", resolved.Effective)
	return err
}

func cloneResolvedConfig(resolved *kconfig.ResolvedConfig) *kconfig.ResolvedConfig {
	if resolved == nil {
		return nil
	}
	return &kconfig.ResolvedConfig{
		Raw:       maps.Clone(resolved.Raw),
		Effective: maps.Clone(resolved.Effective),
		Written:   maps.Clone(resolved.Written),
	}
}

func validateKernelVersion(value string, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("-kernel_version is required for resolved config or Kbuild action planning")
	}
	return nil
}

func resolveConfigOptions(mode string) (kconfig.ResolveConfigOptions, error) {
	switch mode {
	case "", "default":
		return kconfig.ResolveConfigOptions{}, nil
	case "allnoconfig":
		return kconfig.ResolveConfigOptions{AllNoConfig: true}, nil
	default:
		return kconfig.ResolveConfigOptions{}, fmt.Errorf("unsupported -config_mode %q", mode)
	}
}

func rustcCfgLines(tree *kconfig.Tree, resolved *kconfig.ResolvedConfig) []string {
	keys := make([]string, 0, len(resolved.Effective))
	for key := range resolved.Effective {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var lines []string
	for _, key := range keys {
		if !resolved.ShouldWrite(key) {
			continue
		}
		symbol := tree.Symbols[strings.TrimPrefix(key, "CONFIG_")]
		if symbol == nil || symbol.Type == kconfig.SymbolUnknown {
			continue
		}
		value := resolved.Effective[key]
		switch symbol.Type {
		case kconfig.SymbolBool, kconfig.SymbolTristate:
			if value == "n" {
				continue
			}
			lines = append(lines, "--cfg="+key)
		}
		if symbol.Type == kconfig.SymbolHex &&
			!strings.HasPrefix(value, "0x") &&
			!strings.HasPrefix(value, "0X") {
			value = "0x" + value
		}
		if symbol.Type == kconfig.SymbolString {
			value = unquoteKconfigString(value)
		}
		lines = append(lines, "--cfg="+key+"="+strconv.Quote(value))
	}
	return lines
}

func unquoteKconfigString(value string) string {
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return value
	}

	value = value[1 : len(value)-1]
	var out strings.Builder
	out.Grow(len(value))
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+1 < len(value) &&
			(value[i+1] == '\\' || value[i+1] == '"') {
			i++
		}
		out.WriteByte(value[i])
	}
	return out.String()
}

func kbuildConfigValue(tree *kconfig.Tree, key, value string) string {
	symbol := tree.Symbols[strings.TrimPrefix(key, "CONFIG_")]
	if symbol != nil && symbol.Type == kconfig.SymbolString {
		return unquoteKconfigString(value)
	}
	return value
}

func resolvedConfigObjectTreeContents(
	tree *kconfig.Tree,
	resolved *kconfig.ResolvedConfig,
	kernelVersion string,
) map[string]string {
	keys := make([]string, 0, len(resolved.Effective))
	for key := range resolved.Effective {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	configLines := make([]string, 0, len(keys))
	autoConfLines := make([]string, 0, len(keys))
	headerLines := []string{
		"/* Generated by Bazel kconfig_parse. */",
		"#ifndef __GENERATED_AUTOCONF_H__",
		"#define __GENERATED_AUTOCONF_H__",
	}
	for _, key := range keys {
		value := resolved.Effective[key]
		if value == "n" {
			configLines = append(configLines, "# "+key+" is not set")
			continue
		}
		if !resolved.ShouldWrite(key) {
			continue
		}
		symbol := tree.Symbols[strings.TrimPrefix(key, "CONFIG_")]
		if value != "" {
			configLines = append(configLines, key+"="+value)
			if header := configValueToHeaderLine(key, value); header != "" {
				headerLines = append(headerLines, header)
			}
		}
		if value != "" || (symbol != nil && symbol.Type == kconfig.SymbolString) {
			autoConfLines = append(autoConfLines, key+"="+kbuildConfigValue(tree, key, value))
		}
	}
	headerLines = append(headerLines, "#endif")

	localVersion := strings.Trim(resolved.Effective["CONFIG_LOCALVERSION"], `"`)
	return map[string]string{
		".config":                       strings.Join(configLines, "\n") + "\n",
		"include/config/auto.conf":      strings.Join(autoConfLines, "\n") + "\n",
		"include/config/auto.conf.cmd":  "cmd_include/config/auto.conf := bazel kconfig_parse -resolve_config\n",
		"include/config/kernel.release": kernelVersion + localVersion + "\n",
		"include/generated/autoconf.h":  strings.Join(headerLines, "\n") + "\n",
		"include/generated/rustc_cfg":   strings.Join(rustcCfgLines(tree, resolved), "\n") + "\n",
	}
}

func writeResolvedConfigOutputs(tree *kconfig.Tree, resolved *kconfig.ResolvedConfig, outputs resolvedConfigOutputs, kernelVersion string) error {
	contents := resolvedConfigObjectTreeContents(tree, resolved, kernelVersion)
	files := map[string]string{
		outputs.config:        contents[".config"],
		outputs.autoConf:      contents["include/config/auto.conf"],
		outputs.autoConfCmd:   contents["include/config/auto.conf.cmd"],
		outputs.autoconf:      contents["include/generated/autoconf.h"],
		outputs.rustcCfg:      contents["include/generated/rustc_cfg"],
		outputs.kernelRelease: contents["include/config/kernel.release"],
	}
	for path, content := range files {
		if err := os.WriteFile(workspacePath(path), []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func configValueToHeaderLine(key, value string) string {
	switch value {
	case "y":
		return "#define " + key + " 1"
	case "m":
		return "#define " + key + "_MODULE 1"
	case "", "n":
		return ""
	default:
		return "#define " + key + " " + value
	}
}

func compactMetadata(
	tree *kconfig.Tree,
	rootPath string,
	kbuildPath string,
	configInput string,
	configOverlays []string,
	configFlags map[string]string,
	configMode string,
	vars map[string]string,
	sourceRoots map[string]string,
	sourceNamespaces map[string]string,
	objectRoot string,
	objectNamespace string,
	entryTargets []string,
	preparationTargets []string,
	selectedProductsOnly bool,
	kbuildInputCache *kbuildInvocationInputCache,
	kernelVersion string,
	targetContract *hostKbuildContract,
	hostContract *hostKbuildContract,
	probeScopes *kconfig.KbuildProbeScopes,
	normalizeConfigValue func(string) (string, error),
) (*kconfig.CompactMetadata, *kconfig.ResolvedConfig, error) {
	if probeScopes == nil {
		return nil, nil, fmt.Errorf("action-plan generation requires symbolic Kbuild probes")
	}
	if hostContract == nil {
		return nil, nil, fmt.Errorf("action-plan generation requires selected host Kbuild actions")
	}
	if kbuildPath == "" {
		return nil, nil, fmt.Errorf("-kbuild is required")
	}
	if configInput == "" {
		return nil, nil, fmt.Errorf("-resolve_config PATH is required")
	}
	resolveOpts, err := resolveConfigOptions(configMode)
	if err != nil {
		return nil, nil, err
	}
	preparationTargets, err = canonicalKbuildPreparationTargets(preparationTargets)
	if err != nil {
		return nil, nil, err
	}
	flags := maps.Clone(configFlags)
	if flags == nil {
		flags, err = readConfigFlags(configInput, configOverlays)
		if err != nil {
			return nil, nil, err
		}
	}
	resolved, err := tree.ResolveConfigWithOptions(flags, resolveOpts)
	if err != nil {
		return nil, nil, err
	}
	// Keep the generated config projection in the upstream Kconfig provenance
	// representation. Kbuild gets a private clone whose path capabilities are
	// rebound to this workload's symbolic authority. This matches the legacy
	// two-resolution behavior without resolving the Kconfig graph twice.
	if err := normalizeResolvedConfigValues(resolved, normalizeConfigValue); err != nil {
		return nil, nil, err
	}
	metadataResolved := cloneResolvedConfig(resolved)
	if err := normalizeResolvedConfigValues(metadataResolved, func(value string) (string, error) {
		return probeScopes.ImportToolsetPathCapabilities(value, func(value string) (string, error) {
			return value, nil
		})
	}); err != nil {
		return nil, nil, err
	}
	sourceRoot := ""
	if rootPath != "" {
		var err error
		sourceRoot, err = filepath.Abs(filepath.Dir(workspacePath(rootPath)))
		if err != nil {
			return nil, nil, fmt.Errorf("resolve source root: %w", err)
		}
	}
	actionRoles := func(scope string, contract *hostKbuildContract) []kconfig.KbuildActionRoleRef {
		roles := make([]kconfig.KbuildActionRoleRef, 0, len(contract.Actions))
		for role := range contract.Actions {
			roles = append(roles, kconfig.KbuildActionRoleRef{Scope: scope, Role: role})
		}
		sort.Slice(roles, func(i, j int) bool { return roles[i].Role < roles[j].Role })
		return roles
	}
	actionContracts := func(scope string, contract *hostKbuildContract) map[kconfig.KbuildActionRoleRef]kconfig.CompactKbuildActionContract {
		contracts := make(map[kconfig.KbuildActionRoleRef]kconfig.CompactKbuildActionContract, len(contract.Actions))
		for role, action := range contract.Actions {
			contracts[kconfig.KbuildActionRoleRef{Scope: scope, Role: role}] = kconfig.CompactKbuildActionContract{
				PrefixArguments: slices.Clone(action.PrefixArgs),
				SuffixArguments: slices.Clone(action.SuffixArgs),
				Environment:     maps.Clone(action.Environment),
			}
		}
		return contracts
	}
	configuredContracts := actionContracts("target", targetContract)
	for ref, contract := range actionContracts("host", hostContract) {
		configuredContracts[ref] = contract
	}
	// Preserve whether the caller supplied an object tree independently of its
	// normalized path. An explicitly supplied tree may alias the source root but
	// still exposes caller-owned bytes which generated-content probes do not
	// declare.
	preconfiguredObjectTree := objectRoot != ""
	opts := kconfig.CompactMetadataOptions{
		SourceNamespaces: maps.Clone(sourceNamespaces),
		ActionRoles: append(
			actionRoles("target", targetContract),
			actionRoles("host", hostContract)...,
		),
		ActionContracts:         configuredContracts,
		PreconfiguredObjectTree: preconfiguredObjectTree,
		SelectedProductsOnly:    selectedProductsOnly,
	}
	if rustSourceRoot := strings.TrimSpace(vars["RUST_LIB_SRC"]); rustSourceRoot != "" {
		opts.SourceNamespaces[rustSourceRoot] = "rust"
	}
	rootDir := sourceRoot
	if rootDir == "" {
		rootDir = filepath.Dir(workspacePath(kbuildPath))
	}
	if objectRoot == "" {
		objectRoot = rootDir
	} else {
		objectRoot, err = workspaceDirectory(objectRoot)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve preconfigured Kbuild object root: %w", err)
		}
		if objectNamespace == "" {
			return nil, nil, fmt.Errorf("preconfigured Kbuild object root requires -object_namespace")
		}
		objectFiles, indexErr := newKbuildSourceInputIndex(objectRoot)
		if indexErr != nil {
			return nil, nil, fmt.Errorf("index preconfigured Kbuild object root: %w", indexErr)
		}
		for _, filename := range objectFiles.files {
			if opts.ExactSourceNamespaces == nil {
				opts.ExactSourceNamespaces = map[string]string{}
			}
			opts.ExactSourceNamespaces[filename] = objectNamespace
		}
	}
	if len(entryTargets) == 0 {
		entryTargets = []string{"all"}
	}
	entryTargets = uniquePathsInOrder(append(append([]string(nil), entryTargets...), preparationTargets...))
	commandLineVariables, err := kbuildCommandLineVariables(targetContract, hostContract, vars)
	if err != nil {
		return nil, nil, err
	}
	metadata, err := tree.CompactMetadataForResolvedConfigWithOptions(metadataResolved, opts, func(resolved *kconfig.ResolvedConfig) (kconfig.CompactConfigGraph, error) {
		kbuildVars, err := kbuildVariablesForConfig(vars, tree, resolved)
		if err != nil {
			return kconfig.CompactConfigGraph{}, err
		}
		for name, value := range linuxRootMakeInvocationVariables(rootDir) {
			if _, configured := kbuildVars[name]; !configured {
				kbuildVars[name] = value
			}
		}
		hermeticSourceRoot, err := filepath.EvalSymlinks(rootDir)
		if err != nil {
			return kconfig.CompactConfigGraph{}, fmt.Errorf("resolve hermetic Kbuild source root: %w", err)
		}
		kbuildOpts, err := probeScopes.Options("target", kconfig.KbuildOptions{
			Variables:                      kbuildVars,
			CommandLineVariables:           commandLineVariables,
			AutoExportCommandLineVariables: kbuildConfiguredCommandLineAutoExports(vars, targetContract, hostContract),
			SourceRoots:                    sourceRoots,
			ConfigVariablesComplete:        true,
			MakeVariablesComplete:          true,
			Shell: func(command string) (string, error) {
				return hermeticLinuxKbuildShell(command, hermeticSourceRoot, targetContract, hostContract)
			},
		})
		if err != nil {
			return kconfig.CompactConfigGraph{}, err
		}
		kbuildOpts.RootDir = rootDir
		bindProbeEnvironment := func(exported map[string]string) (func() error, error) {
			byScope := map[string]map[string]string{}
			for _, scope := range []string{"target", "host"} {
				scoped, err := linuxProbeEnvironmentForScope(scope, exported)
				if err != nil {
					return nil, err
				}
				byScope[scope] = scoped
			}
			return probeScopes.BindExactScriptEnvironments(byScope)
		}
		resolvedConfigContents := resolvedConfigObjectTreeContents(tree, resolved, kernelVersion)
		profiles, selections, imageTarget, parseErr := evaluatedKbuildProfilesWithGeneratedContent(
			rootDir, objectRoot, entryTargets, preparationTargets, kbuildVars, kbuildOpts, bindProbeEnvironment,
			linuxKbuildGeneratedContentResolver(
				probeScopes, resolvedConfigContents, rootDir, objectRoot, preconfiguredObjectTree,
			),
			resolvedConfigContents,
			kbuildInputCache,
			preconfiguredObjectTree,
		)
		if parseErr != nil {
			return kconfig.CompactConfigGraph{}, parseErr
		}
		deferredSelections, parseErr := kconfig.KbuildDeferredContentSelections(profiles)
		if parseErr != nil {
			return kconfig.CompactConfigGraph{}, parseErr
		}
		return kconfig.CompactConfigGraph{
			KbuildProfiles:                  profiles,
			KbuildSelections:                selections,
			KbuildDeferredContentSelections: deferredSelections,
			ImageTarget:                     imageTarget,
		}, nil
	})
	if err != nil {
		return nil, nil, err
	}
	if err := probeScopes.BindActionPlanToolsetPathCapabilities(metadata); err != nil {
		return nil, nil, fmt.Errorf("bind Kbuild toolset-path capabilities: %w", err)
	}
	return metadata, resolved, nil
}

// evaluatedKbuildProfilesWithOptions snapshots terminal Make invocations from roots
// selected by the evaluated top-level Kbuild environment. The image directory
// comes from KBUILD_IMAGE, then every Kbuild/Makefile below that directory is
// evaluated with the same resolved config and measured tool probes. This is a
// data-derived walk: architecture-specific boot/setup/compressed filenames are
// never enumerated here.
func evaluatedKbuildProfilesWithOptions(
	rootDir string,
	objectRoot string,
	entryTargets []string,
	variables map[string]string,
	baseOptions kconfig.KbuildOptions,
	bindProbeEnvironment func(map[string]string) (func() error, error),
) ([]kconfig.CompactKbuildProfile, []kconfig.CompactKbuildSelection, string, error) {
	return evaluatedKbuildProfilesWithGeneratedContent(
		rootDir, objectRoot, entryTargets, nil, variables, baseOptions, bindProbeEnvironment, nil, nil, nil, false,
	)
}

func evaluatedKbuildProfilesWithGeneratedContent(
	rootDir string,
	objectRoot string,
	entryTargets []string,
	preparationTargets []string,
	variables map[string]string,
	baseOptions kconfig.KbuildOptions,
	bindProbeEnvironment func(map[string]string) (func() error, error),
	generatedContent kbuildGeneratedContentResolver,
	immutableContents map[string]string,
	kbuildInputCache *kbuildInvocationInputCache,
	preconfiguredObjectTree bool,
) ([]kconfig.CompactKbuildProfile, []kconfig.CompactKbuildSelection, string, error) {
	if rootDir == "" {
		return nil, nil, "", nil
	}
	rootDir = filepath.Clean(rootDir)
	profiles, selections, imageTarget, err := evaluatedKbuildInvocationProfiles(
		rootDir, objectRoot, entryTargets, preparationTargets, variables, baseOptions, bindProbeEnvironment, generatedContent,
		immutableContents, kbuildInputCache, preconfiguredObjectTree,
	)
	if err != nil {
		return nil, nil, "", err
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return profiles, selections, imageTarget, nil
}

const (
	kbuildEvalSourceTree    = "__LINUX_BZL_SOURCE_TREE__"
	kbuildEvalObjectTree    = "__LINUX_BZL_OBJECT_TREE__"
	kbuildEvalRecursiveMake = kconfig.CompactKbuildRecursiveMakeProvenanceToken
)

// evaluatedKbuildInvocationProfiles parses the actual Make drivers used for
// selected Kbuild actions. Unlike the source-position profiles above, these
// retain a lazy evaluator and bind paths to stable sentinels so no execroot
// spelling can enter an action recipe.
func evaluatedKbuildInvocationProfiles(
	rootDir string,
	objectRoot string,
	entryTargets []string,
	preparationTargets []string,
	variables map[string]string,
	baseOptions kconfig.KbuildOptions,
	bindProbeEnvironment func(map[string]string) (func() error, error),
	generatedContent kbuildGeneratedContentResolver,
	immutableContents map[string]string,
	kbuildInputCache *kbuildInvocationInputCache,
	preconfiguredObjectTree bool,
) ([]kconfig.CompactKbuildProfile, []kconfig.CompactKbuildSelection, string, error) {
	if err := validateConfiguredKbuildInputs(variables, nil, entryTargets, preparationTargets); err != nil {
		return nil, nil, "", fmt.Errorf("configure Kbuild root invocation: %w", err)
	}
	for _, input := range []struct {
		name   string
		values map[string]string
	}{
		{name: "Kbuild root environment", values: baseOptions.EnvironmentVariables},
		{name: "Kbuild root command line", values: baseOptions.CommandLineVariables},
	} {
		if err := kconfig.ValidateKbuildOrdinaryVariables(input.name, input.values); err != nil {
			return nil, nil, "", fmt.Errorf("configure Kbuild root invocation: %w", err)
		}
	}
	sourceIndex, satisfied, err := kbuildInvocationInputs(
		kbuildInputCache, rootDir, objectRoot, baseOptions.SourceRoots,
	)
	if err != nil {
		return nil, nil, "", fmt.Errorf("index Kbuild source inputs: %w", err)
	}
	if baseOptions.SourceCache == nil {
		baseOptions.SourceCache = kbuildInvocationSourceProgramCache(
			kbuildInputCache, rootDir, objectRoot, baseOptions.SourceRoots,
		)
	}
	sourceOverlayDirectories := kbuildFrontierSourceOverlayDirectories(baseOptions.SourceRoots)
	immutableContents = maps.Clone(immutableContents)
	dispatchCommandLine := cloneKbuildVariables(baseOptions.CommandLineVariables)
	dispatchAutoExport := map[string]bool{}
	if baseOptions.AutoExportCommandLineVariables == nil {
		for name := range dispatchCommandLine {
			if name != "MAKECMDGOALS" {
				dispatchAutoExport[name] = true
			}
		}
	} else {
		for name, enabled := range baseOptions.AutoExportCommandLineVariables {
			if enabled {
				dispatchAutoExport[name] = true
			}
		}
	}
	for _, name := range []string{"abs_output", "abs_srctree", "objtree", "srcroot", "srctree"} {
		// These are planner-owned precedence pins, not user MAKEOVERRIDES
		// payload. ensureProfile installs their canonical sentinel values below;
		// retaining an execroot spelling here would overwrite that stable form.
		delete(dispatchCommandLine, name)
		delete(dispatchAutoExport, name)
	}
	dispatchCommandLine["MAKECMDGOALS"] = strings.Join(entryTargets, " ")
	dispatchRequest := kbuildInvocationRequest{
		name: "driver:Makefile", makefile: "Makefile", entryTargets: append([]string(nil), entryTargets...),
		processLocation: kconfig.CompactKbuildInvocationLocation{Tree: kconfig.CompactKbuildInvocationObjectTree},
		environment:     cloneKbuildVariables(baseOptions.EnvironmentVariables),
		variables:       dispatchCommandLine, commandLineAutoExport: dispatchAutoExport,
	}
	invocationVariables := make(map[string]string, len(variables))
	for name, value := range variables {
		invocationVariables[name] = value
	}
	sourceArchitecture := filepath.ToSlash(strings.Trim(invocationVariables["SRCARCH"], "/"))
	sentinelNormalization := newKbuildInvocationSentinelNormalization(rootDir, sourceArchitecture)
	// Root invocation variables do not vary between recursive Make requests.
	// Normalize their potentially physical source-tree spellings once and retain
	// the resulting immutable environment in kconfig. Each recursive profile then
	// supplies only its sparse directory, environment, and argv overrides.
	invocationVariableBase, err := kconfig.NewKbuildVariableBaseWithRecursiveMakeDefault(
		sentinelNormalization.normalizedVariableBase(invocationVariables),
	)
	if err != nil {
		return nil, nil, "", fmt.Errorf("configure Kbuild invocation variables: %w", err)
	}
	profiles := []kconfig.CompactKbuildProfile{}
	// GNU Make carries command-line assignments into recursive invocations via
	// MAKEOVERRIDES, even when a nested $(MAKE) argv does not repeat them. Keep
	// the effective assignment set beside each evaluated profile so source-time
	// action-role tokens follow the same recursive invocation semantics.
	profileCommandLineVariables := []map[string]string{}
	profileCommandLineAutoExports := []map[string]bool{}
	profileCompletedVisibleArtifactDeltas := [][]kconfig.CompactKbuildVisibleArtifact{}
	profileInitialFrontiers := []kbuildFrontierState{}
	profileCompletedVisibleContentDeltas := []map[string]string{}
	profileInitialProbeEnvironments := []map[string]string{}
	profileByRequest := map[[sha256.Size]byte][]int{}
	profileRequests := []kbuildInvocationRequest{}
	controlByProfile := map[int]kconfig.KbuildControlEvaluation{}
	resolvedTargets := newKbuildResolvedTargetCache()
	activateProbeEnvironment := func(environment map[string]string) error {
		if bindProbeEnvironment == nil {
			return nil
		}
		activate, err := bindProbeEnvironment(environment)
		if err != nil {
			return err
		}
		if activate == nil {
			return fmt.Errorf("Kbuild probe environment binding returned no activation")
		}
		return activate()
	}
	ensureProfile := func(request kbuildInvocationRequest) (int, error) {
		if request.processLocation.Tree == "" {
			request.processLocation.Tree = kconfig.CompactKbuildInvocationObjectTree
		}
		initialFrontier := request.visibleState
		requestDigest := canonicalKbuildInvocationRequestDigest(request)
		for _, index := range profileByRequest[requestDigest] {
			if !canonicalKbuildInvocationRequestsEqual(profileRequests[index], request) {
				continue
			}
			if !maps.Equal(profileInitialProbeEnvironments[index], request.environment) {
				return -1, fmt.Errorf("reused Kbuild invocation %q has a different probe environment", request.name)
			}
			return index, nil
		}
		makefile := filepath.Join(rootDir, filepath.FromSlash(request.makefile))
		if info, err := os.Stat(makefile); err != nil || info.IsDir() {
			return -1, nil
		}
		profileVariables := kbuildInvocationSentinelVariableOverrides(request.directory)
		processLocation := request.processLocation
		workingDirectory := mappedKbuildInvocationLocation(processLocation, rootDir, objectRoot, baseOptions.SourceRoots)
		workingMarker := kbuildEvalObjectTree
		if processLocation.Tree == kconfig.CompactKbuildInvocationSourceTree {
			workingMarker = kbuildEvalSourceTree
		}
		profileVariables["CURDIR"] = workingMarker
		if processLocation.Directory != "" {
			profileVariables["CURDIR"] += "/" + strings.Trim(processLocation.Directory, "/")
		}
		profileVariables["PWD"] = profileVariables["CURDIR"]
		profileEnvironment := make(map[string]string, len(request.environment))
		for name, value := range request.environment {
			normalized := sentinelNormalization.value(value)
			profileVariables[name] = normalized
			profileEnvironment[name] = normalized
		}
		for name, value := range request.variables {
			profileVariables[name] = sentinelNormalization.value(value)
		}
		options := baseOptions
		options.RootDir = rootDir
		options.WorkingDir = workingDirectory
		options.VariableBase = invocationVariableBase
		options.Variables = profileVariables
		options.EnvironmentVariables = profileEnvironment
		options.VirtualFileView = kbuildFrontierVirtualFileView{
			state: initialFrontier, directory: processLocation.Directory,
			sourceOverlayDirectories: sourceOverlayDirectories,
			immutableContents:        immutableContents,
		}
		options.CommandLineVariables = make(map[string]string, len(request.variables))
		for name := range request.variables {
			options.CommandLineVariables[name] = profileVariables[name]
		}
		options.AutoExportCommandLineVariables = maps.Clone(request.commandLineAutoExport)
		if options.AutoExportCommandLineVariables == nil {
			options.AutoExportCommandLineVariables = map[string]bool{}
		}
		// Only the actual parent/argv assignments participate in recursive
		// MAKEOVERRIDES. The canonical aliases installed below are evaluator
		// precedence pins; every invocation receives its own stable pins, while
		// the source Makefiles retain ownership of their export membership.
		effectiveCommandLineVariables := cloneKbuildVariables(options.CommandLineVariables)
		effectiveCommandLineAutoExports := maps.Clone(options.AutoExportCommandLineVariables)
		// The real root Makefile derives these paths from CURDIR and from
		// $(realpath $(lastword $(MAKEFILE_LIST))). During analysis both are an
		// ephemeral local/RBE execroot, so ordinary variable precedence would
		// bake that path into probe identities and simply-expanded helpers such
		// as `build := -f $(srctree)/...`. Give the invariant root aliases
		// command-line
		// precedence, just like recursive Make's explicit obj= assignment, while
		// keeping child directories and goals source-driven. abs_output and
		// srcroot are deliberately excluded: Linux derives both from M= before
		// its root self-submake, and those source-owned relocations move an
		// external module into its writable object overlay while retaining its
		// immutable source root.
		options.CommandLineVariables["abs_srctree"] = kbuildEvalSourceTree
		options.CommandLineVariables["objtree"] = kbuildEvalObjectTree
		options.CommandLineVariables["srctree"] = kbuildEvalSourceTree
		virtualSourceDirectory := kbuildEvalSourceTree
		if request.directory != "" {
			virtualSourceDirectory += "/" + strings.Trim(request.directory, "/")
		}
		if kbuildInvocationSourceOverlayPath(virtualSourceDirectory, baseOptions.SourceRoots) {
			// Linux 6.12 derives src=$(obj) in scripts/Makefile.build. That is
			// normally correct because M= is both the source and output directory,
			// but this planner deliberately overlays immutable external sources on
			// a distinct writable object directory. Keep src on the declared
			// source mapping while obj and CURDIR retain their object-tree
			// locations. This is an evaluator-only precedence pin: omitting it
			// from effectiveCommandLineVariables keeps it out of MAKEOVERRIDES.
			options.CommandLineVariables["src"] = virtualSourceDirectory
		}
		options.SourceRoots = make(map[string]string, len(baseOptions.SourceRoots)+2)
		for prefix, path := range baseOptions.SourceRoots {
			options.SourceRoots[prefix] = path
		}
		options.SourceRoots[kbuildEvalSourceTree] = rootDir
		options.SourceRoots[kbuildEvalObjectTree] = objectRoot
		options.CaptureVariables = nil
		options.CaptureTargetEvaluator = true
		// The target evaluator retains the parser's export declarations and can
		// expand them in an exact target context when an action actually needs
		// the recursive-Make environment.  Eagerly materializing the same values
		// into KbuildFile duplicates a large map for every selected invocation.
		options.SkipExportedVariables = true
		options.Shell = kbuildInvocationSentinelShell(rootDir, objectRoot, invocationVariables["SRCARCH"], sourceIndex, options.SourceRoots, baseOptions.Shell)
		parsed, err := kconfig.ParseKbuildFileTree(makefile, options)
		if err != nil {
			return -1, fmt.Errorf("evaluate Kbuild invocation %s: %w", request.name, err)
		}
		profile, err := kconfig.NewCompactKbuildProfile(
			kbuildInvocationProfileNameFromDigest(request.name, requestDigest), makefile, rootDir, parsed,
		)
		if err != nil {
			return -1, err
		}
		kconfig.SetCompactKbuildProfileDirectory(&profile, request.directory)
		if err := kconfig.SetCompactKbuildProfileInvocationLocation(&profile, processLocation); err != nil {
			return -1, fmt.Errorf("record Kbuild invocation %s working directory: %w", request.name, err)
		}
		kconfig.SetCompactKbuildProfileInitialVisibleArtifactView(
			&profile, kbuildFrontierArtifactView{state: initialFrontier},
		)
		profile.InvocationPredecessors = append([]string(nil), request.invocationPredecessors...)
		profile.EntryTargets = make([]string, 0, len(request.entryTargets))
		for _, requested := range request.entryTargets {
			target := ""
			if requested == kbuildEvalSourceTree || requested == kbuildEvalObjectTree ||
				strings.HasPrefix(requested, kbuildEvalSourceTree+"/") || strings.HasPrefix(requested, kbuildEvalObjectTree+"/") {
				target = kconfig.CanonicalKbuildProfileTarget(profile, requested)
			} else {
				// kbuildInvocationEntryTargets already resolved ordinary argv
				// goals against obj= or make -C. Do not apply the parser cwd a
				// second time when those graph identities enter the profile.
				target = kconfig.CanonicalKbuildGraphTarget(requested)
			}
			if target != "" {
				profile.EntryTargets = append(profile.EntryTargets, target)
			}
		}
		// MAKECMDGOALS order is observable for grouped targets: the first peer
		// reached supplies $@ for the one shared recipe. Canonicalize above, then
		// remove duplicates without sorting away command-line order.
		profile.EntryTargets = uniquePathsInOrder(profile.EntryTargets)
		if len(profile.EntryTargets) == 0 {
			goal, goalErr := kbuildInvocationDefaultGoal(profile)
			if goalErr != nil {
				return -1, fmt.Errorf("resolve Kbuild invocation %s default goal: %w", request.name, goalErr)
			}
			if goal != "" {
				profile.EntryTargets = []string{goal}
			}
		}
		index := len(profiles)
		profiles = append(profiles, profile)
		profileCommandLineVariables = append(profileCommandLineVariables, effectiveCommandLineVariables)
		profileCommandLineAutoExports = append(profileCommandLineAutoExports, effectiveCommandLineAutoExports)
		profileInitialFrontiers = append(profileInitialFrontiers, initialFrontier)
		profileInitialProbeEnvironments = append(profileInitialProbeEnvironments, maps.Clone(request.environment))
		profileRequests = append(profileRequests, request)
		// A completed child is exposed only after processProfile returns. Store
		// its changed-path frontier then; a provisional full snapshot here would
		// be both stale and quadratic across source-ordered sibling invocations.
		profileCompletedVisibleArtifactDeltas = append(profileCompletedVisibleArtifactDeltas, nil)
		profileCompletedVisibleContentDeltas = append(profileCompletedVisibleContentDeltas, nil)
		profileByRequest[requestDigest] = append(profileByRequest[requestDigest], index)
		return index, nil
	}
	evaluateControl := func(profile kconfig.CompactKbuildProfile) (kconfig.KbuildControlEvaluation, error) {
		ruleIndex := newKbuildProfileTargetIndex(profile, nil)
		currentness := newKbuildProfileTargetSatisfaction(profile, ruleIndex, satisfied)
		return kconfig.EvaluateSelectedKbuildControlEffectsWithOptions(
			profile,
			kconfig.KbuildControlEvaluationOptions{
				SelectedRuleIndexes: func(target, makeTarget string) []int {
					return kbuildProfileRuleIndexesForMakeTargetIndexed(
						profile, ruleIndex, target, makeTarget, satisfied,
					)
				},
				TargetIsSatisfied: func(target string) bool {
					return currentness.targetIsSatisfied(target)
				},
				BindProbeEnvironment: bindProbeEnvironment,
			},
		)
	}
	recordEvaluatedProfile := func(index int, control kconfig.KbuildControlEvaluation) error {
		resolvedTargets.invalidateProfile(profiles[index].Name)
		profiles[index] = control.Profile
		controlByProfile[index] = control
		return nil
	}
	dispatchIndex, err := ensureProfile(dispatchRequest)
	if err != nil {
		return nil, nil, "", err
	}
	imageTarget := ""
	if dispatchIndex >= 0 {
		evaluation, evaluationErr := evaluateControl(profiles[dispatchIndex])
		if evaluationErr != nil {
			return nil, nil, "", fmt.Errorf("evaluate selected root Kbuild control effects: %w", evaluationErr)
		}
		exported, exportErr := kconfig.ExportedKbuildControlVariables(evaluation)
		if exportErr != nil {
			return nil, nil, "", exportErr
		}
		if err := recordEvaluatedProfile(dispatchIndex, evaluation); err != nil {
			return nil, nil, "", err
		}
		// The evaluated root invocation is the sole owner of recursive Make's
		// exported environment.  In particular, KBUILD_IMAGE may depend on the
		// selected architecture and other root Make state, so derive it from this
		// exact snapshot instead of the standalone Kbuild directory parser.
		imageTarget = canonicalKbuildProfilePath(exported["KBUILD_IMAGE"], rootDir)
	}
	// Recursive Make is synchronous. Process a selected child to completion
	// before parsing the next source-ordered child, then replay the parent's
	// exact frontier. This turns generated text into an ordinary data dependency:
	// a later invocation can read bytes produced by earlier recipes without a
	// filename-, architecture-, or toolchain-specific planner rule.
	processedProfiles := map[int]bool{}
	processingProfiles := map[int]bool{}
	restoreProfileEnvironment := func(index int) error {
		// The root control evaluation installs its source-selected final export
		// snapshot before recursive discovery starts. Its incoming request has no
		// parent environment and therefore must not overwrite that snapshot.
		if bindProbeEnvironment == nil || index == dispatchIndex {
			return nil
		}
		if err := activateProbeEnvironment(profileInitialProbeEnvironments[index]); err != nil {
			return fmt.Errorf("restore Kbuild probe environment for invocation %q: %w", profiles[index].Name, err)
		}
		return nil
	}
	type frontierReplayKey struct {
		profile  int
		frontier *kbuildRecursiveMakeFrontier
	}
	frontierReplayCache := map[frontierReplayKey]frontierReplayResult{}
	frontierAncestryCache := map[string]bool{}
	applyFrontierEvent := func(
		index int,
		frontier *kbuildRecursiveMakeFrontier,
		event kbuildRecursiveMakeFrontierEvent,
		result frontierReplayResult,
		childProfiles map[string]int,
	) (frontierReplayResult, error) {
		if event.invocation != "" {
			predecessorIndex, ok := childProfiles[event.invocation]
			if !ok {
				return frontierReplayResult{}, fmt.Errorf(
					"selected Kbuild frontier for invocation %q references undiscovered child %q",
					profiles[index].Name, event.invocation,
				)
			}
			if !processedProfiles[predecessorIndex] {
				return frontierReplayResult{}, fmt.Errorf(
					"selected Kbuild frontier for invocation %q references incomplete child %q",
					profiles[index].Name, profiles[predecessorIndex].Name,
				)
			}
			for _, artifact := range profileCompletedVisibleArtifactDeltas[predecessorIndex] {
				value := kbuildFrontierValue{artifact: artifact, origin: frontier}
				if data, exact := profileCompletedVisibleContentDeltas[predecessorIndex][artifact.Path]; exact {
					value.content = data
					value.exact = true
				}
				if previous, exists := kbuildFrontierGet(result.state, artifact.Path); exists && sameFrontierValue(previous, value) {
					// A child completion includes its inherited frontier. Preserve the
					// original version when the child did not rewrite this path.
					continue
				}
				result.state = kbuildFrontierSet(result.state, artifact.Path, value)
				result.touched = kbuildPathSetAdd(result.touched, artifact.Path)
			}
			return result, nil
		}

		artifact := event.artifact
		artifact.Path = kconfig.CanonicalKbuildGraphTarget(artifact.Path)
		artifact.Target = kconfig.CanonicalKbuildGraphTarget(artifact.Target)
		if artifact.Path == "" || artifact.Path == "." {
			return result, nil
		}
		value := kbuildFrontierValue{artifact: artifact, origin: frontier}
		if event.command != "" {
			commandTarget := event.commandTarget
			if commandTarget == "" {
				commandTarget = artifact.Target
			}
			resolved, err := kconfig.ResolveCompactKbuildTargetSymbolicText(
				profiles[index], commandTarget, event.command,
			)
			if err != nil {
				return frontierReplayResult{}, fmt.Errorf(
					"resolve generated text recipe for %s target %q: %w",
					profiles[index].Name, artifact.Target, err,
				)
			}
			data, exact, projectionErr := kbuildInvocationGeneratedTextProjectionFromFrontier(
				profiles[index], resolved, artifact.Path, result.state,
			)
			if projectionErr != nil {
				return frontierReplayResult{}, fmt.Errorf(
					"project generated text for %s target %q: %w",
					profiles[index].Name, artifact.Target, projectionErr,
				)
			}
			if exact {
				value.content = data
				value.exact = true
			}
		}
		result.state = kbuildFrontierSet(result.state, artifact.Path, value)
		result.touched = kbuildPathSetAdd(result.touched, artifact.Path)
		return result, nil
	}

	var replayFrontier func(
		int,
		*kbuildRecursiveMakeFrontier,
		map[string]int,
	) (frontierReplayResult, error)
	replayFrontier = func(
		index int,
		frontier *kbuildRecursiveMakeFrontier,
		childProfiles map[string]int,
	) (frontierReplayResult, error) {
		cacheKey := frontierReplayKey{profile: index, frontier: frontier}
		if cached, ok := frontierReplayCache[cacheKey]; ok {
			return cached, nil
		}
		if frontier == nil {
			result := frontierReplayResult{
				state: profileInitialFrontiers[index], touched: emptyKbuildPathSet(),
			}
			frontierReplayCache[cacheKey] = result
			return result, nil
		}

		if frontier.hasEvent {
			// A Linux invocation can have thousands of source-ordered writes before
			// its next recursive Make. Replaying the parent recursively cloned and
			// retained the complete, growing object-tree map once per write. Walk
			// the immutable single-parent chain back to the nearest materialized
			// snapshot or join, then apply its deltas forward through the persistent
			// ordered map.
			// Only callers and joins need snapshots; intermediate event nodes remain
			// compact causal history.
			chain := []*kbuildRecursiveMakeFrontier{}
			base := frontier
			var result frontierReplayResult
			resultReady := false
			for base != nil && base.hasEvent {
				if cached, ok := frontierReplayCache[frontierReplayKey{profile: index, frontier: base}]; ok {
					result = cached
					resultReady = true
					break
				}
				chain = append(chain, base)
				base = base.parents[0]
			}
			if !resultReady {
				var err error
				result, err = replayFrontier(index, base, childProfiles)
				if err != nil {
					return frontierReplayResult{}, err
				}
			}
			for chainIndex := len(chain) - 1; chainIndex >= 0; chainIndex-- {
				eventFrontier := chain[chainIndex]
				var err error
				result, err = applyFrontierEvent(
					index, eventFrontier, eventFrontier.event, result, childProfiles,
				)
				if err != nil {
					return frontierReplayResult{}, err
				}
			}
			frontierReplayCache[cacheKey] = result
			return result, nil
		}

		parentResults := make([]frontierReplayResult, 0, len(frontier.parents))
		for _, parent := range frontier.parents {
			result, err := replayFrontier(index, parent, childProfiles)
			if err != nil {
				return frontierReplayResult{}, err
			}
			parentResults = append(parentResults, result)
		}
		merged, err := mergeKbuildFrontierReplayResults(
			profileInitialFrontiers[index], parentResults, frontierAncestryCache,
		)
		if err != nil {
			return frontierReplayResult{}, err
		}
		frontierReplayCache[cacheKey] = merged
		return merged, nil
	}

	var processProfile func(int) error
	processProfile = func(index int) error {
		if processedProfiles[index] {
			return nil
		}
		if processingProfiles[index] {
			return fmt.Errorf("selected recursive Make invocation cycle reaches %q", profiles[index].Name)
		}
		processingProfiles[index] = true
		completed := false
		defer func() {
			delete(processingProfiles, index)
			if completed {
				processedProfiles[index] = true
			}
		}()
		if err := restoreProfileEnvironment(index); err != nil {
			return err
		}
		control, ok := controlByProfile[index]
		if !ok {
			var controlErr error
			control, controlErr = evaluateControl(profiles[index])
			if controlErr != nil {
				return fmt.Errorf("evaluate selected Kbuild control effects for invocation %q: %w", profiles[index].Name, controlErr)
			}
			if controlErr = recordEvaluatedProfile(index, control); controlErr != nil {
				return controlErr
			}
		}

		var completionFrontier *kbuildRecursiveMakeFrontier
		children, err := selectedKbuildRecursiveMakePlanWithResolvedTargets(
			profiles[index], satisfied, &control, &completionFrontier, resolvedTargets,
		)
		if err != nil {
			return err
		}
		// Recursive planning copied the exact pre-recipe snapshots needed by
		// each child into the returned entries. The evaluated profile already
		// retains its final and target-specific evaluators, so keeping the full
		// source-order snapshot table while every descendant is processed only
		// overlaps two complete semantic graphs at peak memory.
		delete(controlByProfile, index)
		control = kconfig.KbuildControlEvaluation{}
		childProfiles := map[string]int{}
		for _, child := range children {
			if err := restoreProfileEnvironment(index); err != nil {
				return err
			}
			visibleFrontier, replayErr := replayFrontier(index, child.frontier, childProfiles)
			if replayErr != nil {
				return fmt.Errorf("resolve frontier before recursive Make invocation %q: %w", child.request.name, replayErr)
			}
			effectiveRequest := inheritKbuildInvocationCommandLineVariables(
				child.request,
				profileCommandLineVariables[index],
				profileCommandLineAutoExports[index],
			)
			effectiveRequest = kbuildInvocationSourceOverlayRequest(effectiveRequest, baseOptions.SourceRoots)
			effectiveRequest.visibleState = visibleFrontier.state
			for _, predecessor := range child.predecessors {
				predecessorIndex, ok := childProfiles[predecessor]
				if !ok {
					return fmt.Errorf(
						"selected recursive Make invocation %q references undiscovered predecessor %q",
						child.request.name, predecessor,
					)
				}
				effectiveRequest.invocationPredecessors = appendUniqueKbuildProfileName(
					effectiveRequest.invocationPredecessors,
					profiles[predecessorIndex].Name,
				)
			}
			if bindProbeEnvironment != nil {
				if refreshErr := activateProbeEnvironment(effectiveRequest.environment); refreshErr != nil {
					return fmt.Errorf("refresh Kbuild probe environments for invocation %q: %w", child.request.name, refreshErr)
				}
			}
			beforeProfiles := len(profiles)
			childIndex, ensureErr := ensureProfile(effectiveRequest)
			if ensureErr != nil {
				return ensureErr
			}
			if childIndex < 0 {
				return fmt.Errorf(
					"selected recursive Make invocation %q has no declared source driver %q (directory %q, goals %q)",
					child.request.name, child.request.makefile, child.request.directory, child.request.entryTargets,
				)
			}
			if child.control != nil {
				profiles[childIndex], ensureErr = kconfig.AttachKbuildDeferredContentQueries(profiles[childIndex], *child.control)
				if ensureErr != nil {
					return fmt.Errorf("attach source-ordered Kbuild control provenance to invocation %q: %w", child.request.name, ensureErr)
				}
				// A non-empty attachment changes the child's private deferred-query
				// registry. A reused profile may already have resolved targets from
				// an earlier parent, so drop only that profile's handles before
				// processing it again or reusing it during final selection. An empty
				// attachment merely clones an equivalent registry and preserves reuse.
				if len(child.control.Queries) != 0 {
					resolvedTargets.invalidateProfile(profiles[childIndex].Name)
				}
			}
			if len(profiles) != beforeProfiles {
				childControl, controlErr := evaluateControl(profiles[childIndex])
				if controlErr != nil {
					return fmt.Errorf(
						"evaluate selected Kbuild control effects for invocation %q: %w",
						profiles[childIndex].Name, controlErr,
					)
				}
				if controlErr = recordEvaluatedProfile(childIndex, childControl); controlErr != nil {
					return controlErr
				}
			}
			if err := processProfile(childIndex); err != nil {
				return err
			}
			childProfiles[child.key] = childIndex
			for _, consumer := range child.consumers {
				// Invocation dependencies are consumed by graph ordering and later
				// lowering; resolved target context/effects do not consult them, so
				// appending this edge does not invalidate the parent's handle.
				profiles[index].TargetInvocationDependencies = appendKbuildInvocationDependency(
					profiles[index].TargetInvocationDependencies,
					kconfig.CompactKbuildInvocationDependency{
						Target: consumer, Profile: profiles[childIndex].Name,
						Goals:           append([]string(nil), profiles[childIndex].EntryTargets...),
						ReplayArguments: append([]string(nil), child.replayArguments...),
					},
				)
			}
		}
		if err := restoreProfileEnvironment(index); err != nil {
			return err
		}
		completedFrontier, replayErr := replayFrontier(index, completionFrontier, childProfiles)
		if replayErr != nil {
			return fmt.Errorf("resolve completed frontier for invocation %q: %w", profiles[index].Name, replayErr)
		}
		deltaArtifacts := make([]kconfig.CompactKbuildVisibleArtifact, 0, kbuildPathSetLen(completedFrontier.touched))
		var deltaContents map[string]string
		var deltaErr error
		kbuildPathSetRange(completedFrontier.touched, func(path string) bool {
			initial, initiallyPresent := kbuildFrontierGet(profileInitialFrontiers[index], path)
			final, finallyPresent := kbuildFrontierGet(completedFrontier.state, path)
			if initiallyPresent && !finallyPresent {
				deltaErr = fmt.Errorf(
					"visible path %q disappeared from a completed recursive Make frontier", path,
				)
				return false
			}
			if !finallyPresent || (initiallyPresent && sameFrontierValue(initial, final)) {
				return true
			}
			deltaArtifacts = append(deltaArtifacts, final.artifact)
			if final.exact {
				if deltaContents == nil {
					deltaContents = map[string]string{}
				}
				deltaContents[path] = final.content
			}
			return true
		})
		if deltaErr != nil {
			return fmt.Errorf("resolve completed frontier delta for invocation %q: %w", profiles[index].Name, deltaErr)
		}
		profileCompletedVisibleArtifactDeltas[index] = deltaArtifacts
		profileCompletedVisibleContentDeltas[index] = deltaContents
		completed = true
		return nil
	}
	if dispatchIndex >= 0 {
		if err := processProfile(dispatchIndex); err != nil {
			return nil, nil, "", err
		}
	}
	effectiveGeneratedContent := generatedContent
	if generatedContent != nil {
		effectiveGeneratedContent = func(
			profile kconfig.CompactKbuildProfile,
			target, recipe string,
			sourcePrerequisites, generatedPrerequisites []string,
		) (string, bool, bool, error) {
			if err := kconfig.ActivateCompactKbuildProfileTargetProbeEnvironment(profile, target); err != nil {
				return "", false, false, fmt.Errorf(
					"activate generated-content probe environment for %q target %q: %w",
					profile.Name, target, err,
				)
			}
			return generatedContent(profile, target, recipe, sourcePrerequisites, generatedPrerequisites)
		}
	}
	selections, err := selectedKbuildSelectionsWithResolvedTargets(
		profiles, satisfied, nil, rootDir, preparationTargets, effectiveGeneratedContent, resolvedTargets,
		preconfiguredObjectTree,
	)
	if err != nil {
		return nil, nil, "", err
	}
	return profiles, selections, imageTarget, nil
}

func kbuildGeneratedContentHasClosedWorkingFrontier(
	profile kconfig.CompactKbuildProfile,
	target, recipe, rootDir, objectRoot string,
	preconfiguredObjectTree bool,
) bool {
	// A preconfigured object tree may supersede any planner-owned config
	// projection or expose additional existing inputs. It needs a separately
	// declared probe input tree before exact evaluation is possible.
	if preconfiguredObjectTree || filepath.Clean(rootDir) != filepath.Clean(objectRoot) {
		return false
	}
	location, located := kconfig.CompactKbuildProfileInvocationLocation(profile)
	if !located || location.Tree != kconfig.CompactKbuildInvocationObjectTree ||
		kconfig.CanonicalKbuildGraphTarget(location.Directory) != "" {
		return false
	}
	if _, overlay, err := kconfig.CompactKbuildProfileSourceOverlayRoot(profile); err != nil || overlay {
		return false
	}
	canonicalTarget := kconfig.CanonicalKbuildGraphTarget(target)
	for _, dependency := range profile.TargetInvocationDependencies {
		if kconfig.CanonicalKbuildGraphTarget(dependency.Target) == canonicalTarget {
			return false
		}
	}
	observation := kconfig.ObserveCompactKbuildObjectTree(recipe)
	if observation.ObservesObjectTree {
		return false
	}
	programs, err := kconfig.CompactKbuildObjectTreeCommandPrograms(profile, recipe)
	return err == nil && len(programs) == 0
}

func appendKbuildInvocationDependency(
	dependencies []kconfig.CompactKbuildInvocationDependency,
	candidate kconfig.CompactKbuildInvocationDependency,
) []kconfig.CompactKbuildInvocationDependency {
	for _, existing := range dependencies {
		if existing.Target == candidate.Target && existing.Profile == candidate.Profile &&
			slices.Equal(existing.Goals, candidate.Goals) && slices.Equal(existing.ReplayArguments, candidate.ReplayArguments) {
			return dependencies
		}
	}
	return append(dependencies, candidate)
}

func appendUniqueKbuildProfileName(names []string, name string) []string {
	if name == "" || slices.Contains(names, name) {
		return names
	}
	return append(names, name)
}

func kbuildSatisfiedTargets(sourceIndex kbuildSourceInputIndex) map[string]bool {
	targets := map[string]bool{}
	for _, target := range kconfig.ResolvedConfigProjectionOutputs() {
		targets[canonicalKbuildProfilePath(target, "")] = true
	}
	for _, source := range sourceIndex.files {
		if source = canonicalKbuildProfilePath(source, ""); source != "" {
			targets[source] = true
		}
	}
	return targets
}

type kbuildSourceInputIndex struct {
	files       []string
	directories []string
}

type kbuildInvocationInputSnapshot struct {
	index     kbuildSourceInputIndex
	satisfied map[string]bool
}

// kbuildInvocationInputCache is scoped to one sequential family-planner
// process. Its entries describe only the immutable filesystem roots declared
// to that Bazel action; config-dependent profiles and evaluator state never
// enter this cache.
//
// Stored slices and maps are private to the cache. Lookup results never alias
// that stored state, so a later planner refactor cannot accidentally make one
// variant's target walk mutate a sibling's immutable input snapshot.
type kbuildInvocationInputCache struct {
	entries        map[string]kbuildInvocationInputSnapshot
	sourcePrograms map[string]*kconfig.KbuildSourceCache
}

func kbuildInvocationSourceProgramCache(
	cache *kbuildInvocationInputCache,
	rootDir string,
	objectRoot string,
	sourceRoots map[string]string,
) *kconfig.KbuildSourceCache {
	key := kbuildInvocationInputCacheKey(rootDir, objectRoot, sourceRoots)
	if cache != nil {
		if sourceCache := cache.sourcePrograms[key]; sourceCache != nil {
			return sourceCache
		}
	}
	immutableRoots := make([]string, 0, len(sourceRoots)+1)
	immutableRoots = append(immutableRoots, rootDir)
	for _, root := range sourceRoots {
		immutableRoots = append(immutableRoots, root)
	}
	excludedRoots := []string(nil)
	if cleanKbuildInvocationInputRoot(objectRoot) != cleanKbuildInvocationInputRoot(rootDir) {
		excludedRoots = []string{objectRoot}
	}
	sourceCache := kconfig.NewKbuildSourceCache(immutableRoots, excludedRoots)
	if cache != nil {
		if cache.sourcePrograms == nil {
			cache.sourcePrograms = map[string]*kconfig.KbuildSourceCache{}
		}
		cache.sourcePrograms[key] = sourceCache
	}
	return sourceCache
}

func appendKbuildInvocationInputCacheKeyString(key *strings.Builder, value string) {
	key.WriteString(strconv.Itoa(len(value)))
	key.WriteByte(':')
	key.WriteString(value)
}

func cleanKbuildInvocationInputRoot(root string) string {
	return filepath.Clean(root)
}

func kbuildInvocationInputCacheKey(
	rootDir string,
	objectRoot string,
	sourceRoots map[string]string,
) string {
	var key strings.Builder
	appendKbuildInvocationInputCacheKeyString(&key, "kbuild-inputs-v1")
	appendKbuildInvocationInputCacheKeyString(&key, cleanKbuildInvocationInputRoot(rootDir))
	appendKbuildInvocationInputCacheKeyString(&key, cleanKbuildInvocationInputRoot(objectRoot))
	markers := slices.Sorted(maps.Keys(sourceRoots))
	appendKbuildInvocationInputCacheKeyString(&key, strconv.Itoa(len(markers)))
	for _, marker := range markers {
		appendKbuildInvocationInputCacheKeyString(&key, marker)
		appendKbuildInvocationInputCacheKeyString(
			&key,
			cleanKbuildInvocationInputRoot(sourceRoots[marker]),
		)
	}
	return key.String()
}

func cloneKbuildSourceInputIndex(index kbuildSourceInputIndex) kbuildSourceInputIndex {
	return kbuildSourceInputIndex{
		files:       slices.Clone(index.files),
		directories: slices.Clone(index.directories),
	}
}

func cloneKbuildInvocationInputSnapshot(
	snapshot kbuildInvocationInputSnapshot,
) (kbuildSourceInputIndex, map[string]bool) {
	return cloneKbuildSourceInputIndex(snapshot.index), maps.Clone(snapshot.satisfied)
}

func kbuildInvocationInputs(
	cache *kbuildInvocationInputCache,
	rootDir string,
	objectRoot string,
	sourceRoots map[string]string,
) (kbuildSourceInputIndex, map[string]bool, error) {
	cacheKey := ""
	if cache != nil {
		cacheKey = kbuildInvocationInputCacheKey(rootDir, objectRoot, sourceRoots)
		if snapshot, ok := cache.entries[cacheKey]; ok {
			index, satisfied := cloneKbuildInvocationInputSnapshot(snapshot)
			return index, satisfied, nil
		}
	}
	index, err := newKbuildInvocationInputIndex(rootDir, objectRoot, sourceRoots)
	if err != nil {
		return kbuildSourceInputIndex{}, nil, err
	}
	snapshot := kbuildInvocationInputSnapshot{
		index:     index,
		satisfied: kbuildSatisfiedTargets(index),
	}
	if cache != nil {
		if cache.entries == nil {
			cache.entries = map[string]kbuildInvocationInputSnapshot{}
		}
		cache.entries[cacheKey] = kbuildInvocationInputSnapshot{
			index:     cloneKbuildSourceInputIndex(snapshot.index),
			satisfied: maps.Clone(snapshot.satisfied),
		}
	}
	return snapshot.index, snapshot.satisfied, nil
}

func newKbuildSourceInputIndex(root string) (kbuildSourceInputIndex, error) {
	index := kbuildSourceInputIndex{}
	err := filepath.WalkDir(root, func(filename string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == "." {
			return nil
		}
		if entry.IsDir() {
			index.directories = append(index.directories, relative)
		} else {
			// Bazel commonly presents declared source files as symlinks. They are
			// still source-file index members even though find(1) would observe
			// the execroot implementation detail as a link without -L.
			index.files = append(index.files, relative)
		}
		return nil
	})
	if err != nil {
		return kbuildSourceInputIndex{}, err
	}
	sort.Strings(index.files)
	sort.Strings(index.directories)
	return index, nil
}

func appendKbuildSourceInputIndex(index *kbuildSourceInputIndex, root, prefix string) error {
	if root == "" {
		return nil
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve Kbuild source input root %q: %w", root, err)
	}
	root = resolvedRoot
	return filepath.WalkDir(root, func(filename string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		relative = filepath.ToSlash(relative)
		if prefix != "" {
			relative = pathpkg.Join(prefix, relative)
		}
		if entry.IsDir() {
			index.directories = append(index.directories, relative)
		} else {
			index.files = append(index.files, relative)
		}
		return nil
	})
}

func newKbuildInvocationInputIndex(rootDir, objectRoot string, sourceRoots map[string]string) (kbuildSourceInputIndex, error) {
	index := kbuildSourceInputIndex{}
	seenRoots := map[string]bool{}
	add := func(root, prefix string) error {
		root = filepath.Clean(root)
		key := root + "\x00" + prefix
		if root == "" || root == "." || seenRoots[key] {
			return nil
		}
		seenRoots[key] = true
		return appendKbuildSourceInputIndex(&index, root, prefix)
	}
	if err := add(rootDir, ""); err != nil {
		return kbuildSourceInputIndex{}, err
	}
	if err := add(objectRoot, ""); err != nil {
		return kbuildSourceInputIndex{}, err
	}
	for virtual, physical := range sourceRoots {
		prefix := ""
		switch {
		case virtual == kbuildEvalSourceTree:
		case strings.HasPrefix(virtual, kbuildEvalSourceTree+"/"):
			prefix = strings.TrimPrefix(virtual, kbuildEvalSourceTree+"/")
		default:
			continue
		}
		if err := add(physical, prefix); err != nil {
			return kbuildSourceInputIndex{}, err
		}
	}
	index.files = sortedUniquePaths(index.files)
	index.directories = sortedUniquePaths(index.directories)
	return index, nil
}

func mappedKbuildInvocationDirectory(directory, rootDir string, sourceRoots map[string]string) string {
	virtual := kbuildEvalSourceTree + "/" + strings.Trim(directory, "/")
	bestPrefix := ""
	bestRoot := ""
	for prefix, physical := range sourceRoots {
		prefix = filepath.ToSlash(strings.TrimSuffix(prefix, "/"))
		if virtual != prefix && !strings.HasPrefix(virtual, prefix+"/") {
			continue
		}
		if len(prefix) > len(bestPrefix) {
			bestPrefix, bestRoot = prefix, physical
		}
	}
	if bestPrefix != "" {
		relative := strings.TrimPrefix(virtual, bestPrefix)
		relative = strings.TrimPrefix(relative, "/")
		return filepath.Join(bestRoot, filepath.FromSlash(relative))
	}
	return filepath.Join(rootDir, filepath.FromSlash(directory))
}

func mappedKbuildInvocationLocation(
	location kconfig.CompactKbuildInvocationLocation,
	rootDir, objectRoot string,
	sourceRoots map[string]string,
) string {
	if location.Tree == kconfig.CompactKbuildInvocationSourceTree {
		return mappedKbuildInvocationDirectory(location.Directory, rootDir, sourceRoots)
	}
	return filepath.Join(objectRoot, filepath.FromSlash(location.Directory))
}

func kbuildInvocationSentinelShell(rootDir, objectRoot, srcarch string, sourceIndex kbuildSourceInputIndex, sourceRoots map[string]string, shell func(string) (string, error)) func(string) (string, error) {
	root := filepath.ToSlash(filepath.Clean(rootDir))
	physicalRoot := root
	if resolved, err := filepath.EvalSymlinks(rootDir); err == nil {
		physicalRoot = filepath.ToSlash(filepath.Clean(resolved))
	}
	sourceArchitecture := filepath.ToSlash(strings.Trim(srcarch, "/"))
	return func(command string) (string, error) {
		// GNU Make's realpath function can reintroduce Bazel's physical
		// repository-cache spelling while expanding an otherwise stable variable.
		// Normalize the complete source-derived command at the probe boundary so
		// every route into a content-addressed request has identical path identity.
		command = kbuildInvocationSentinelValueForArch(physicalRoot, sourceArchitecture, command)
		command = kbuildInvocationSentinelValueForArch(root, sourceArchitecture, command)
		for virtual, physical := range sourceRoots {
			resolved, resolveErr := filepath.EvalSymlinks(physical)
			if resolveErr == nil {
				command = strings.ReplaceAll(command, filepath.ToSlash(filepath.Clean(resolved)), filepath.ToSlash(virtual))
			}
			command = strings.ReplaceAll(command, filepath.ToSlash(filepath.Clean(physical)), filepath.ToSlash(virtual))
		}
		// Resolve tools/scripts/Makefile.include's O/OUTPUT directory queries
		// before the generic source-shell probe grammar sees them. The object
		// sentinel is a Make-visible rooted namespace, not a relative directory
		// below a probe action's private scratch cwd.
		if value, handled, err := evaluateHermeticKbuildDirectoryQuery(command, rootDir); handled {
			return value, err
		}
		if value, handled, err := evaluateKbuildSourceOverlayDirectoryQuery(command, sourceRoots); handled {
			return value, err
		}
		if value, handled, err := evaluateKbuildIndexedFind(command, sourceIndex); handled {
			return value, err
		}
		if shell == nil {
			return "", fmt.Errorf("Kbuild shell command %q has no hermetic evaluator", command)
		}
		// Let symbolic compiler probing see the stable source/object sentinels.
		// Physicalizing first made otherwise identical local, sandboxed, and RBE
		// probes hash different -fmacro-prefix-map arguments. Only filesystem
		// fallback queries need the declared source root's physical spelling.
		value, err := shell(command)
		var physicalPathErr kbuildInvocationPhysicalPathError
		if err != nil && errors.As(err, &physicalPathErr) {
			physicalObjectRoot := objectRoot
			if resolved, resolveErr := filepath.EvalSymlinks(objectRoot); resolveErr == nil {
				physicalObjectRoot = resolved
			}
			physicalCommand := strings.ReplaceAll(command, kbuildEvalObjectTree, filepath.ToSlash(filepath.Clean(physicalObjectRoot)))
			physicalCommand = strings.ReplaceAll(physicalCommand, kbuildEvalSourceTree, physicalRoot)
			if physicalValue, physicalErr := shell(physicalCommand); physicalErr == nil {
				value, err = physicalValue, nil
			}
		}
		if err != nil {
			return "", err
		}
		value = kbuildInvocationSentinelValueForArch(physicalRoot, sourceArchitecture, value)
		value = kbuildInvocationSentinelValueForArch(root, sourceArchitecture, value)
		for virtual, physical := range sourceRoots {
			resolved, resolveErr := filepath.EvalSymlinks(physical)
			if resolveErr == nil {
				value = strings.ReplaceAll(value, filepath.ToSlash(filepath.Clean(resolved)), filepath.ToSlash(virtual))
			}
			value = strings.ReplaceAll(value, filepath.ToSlash(filepath.Clean(physical)), filepath.ToSlash(virtual))
		}
		return value, nil
	}
}

func evaluateKbuildSourceOverlayDirectoryQuery(command string, sourceRoots map[string]string) (string, bool, error) {
	simple, ok, err := kbuildSingleShellSimpleCommand(command)
	if err != nil || !ok || len(simple.redirections) != 0 || len(simple.argv) != 3 || simple.argv[0] != "mkdir" || simple.argv[1] != "-p" {
		return "", false, err
	}
	fields := simple.argv
	directory := filepath.ToSlash(filepath.Clean(fields[2]))
	if !kbuildInvocationSourceOverlayPath(directory, sourceRoots) {
		return "", false, nil
	}
	// The source TreeArtifact is immutable. map_directory creates the matching
	// writable object-overlay directory before any selected recipe executes, so
	// Linux's root `mkdir -p $(output)` is already satisfied declaratively.
	return "", true, nil
}

type kbuildFindRoot struct {
	prefix string
	source bool
}

// evaluateKbuildIndexedFind evaluates the non-mutating find subset used by
// Kbuild discovery. Explicit source-tree roots query the declared source input
// index. Object-tree sentinels and relative paths produce no matches because
// outputs have not been materialized while the action plan is computed.
func evaluateKbuildIndexedFind(command string, sourceIndex kbuildSourceInputIndex) (string, bool, error) {
	simple, ok, err := kbuildSingleShellSimpleCommand(command)
	if err != nil || !ok || len(simple.argv) < 2 || simple.argv[0] != "find" {
		return "", false, err
	}
	words := simple.argv
	// Ignore the conventional best-effort stderr redirect, but reject other
	// shell composition rather than accidentally interpreting only a prefix.
	for _, redirection := range simple.redirections {
		if redirection.ioNumber != "2" || redirection.operator != ">" || redirection.operand != "/dev/null" {
			return "", false, nil
		}
	}
	roots := []kbuildFindRoot{}
	index := 1
	for index < len(words) && !strings.HasPrefix(words[index], "-") && words[index] != "!" && words[index] != "(" {
		value := filepath.ToSlash(strings.TrimSuffix(words[index], "/"))
		root := kbuildFindRoot{}
		switch {
		case value == kbuildEvalSourceTree:
			root.source = true
		case strings.HasPrefix(value, kbuildEvalSourceTree+"/"):
			root.source = true
			root.prefix = strings.TrimPrefix(value, kbuildEvalSourceTree+"/")
		case value == kbuildEvalObjectTree || strings.HasPrefix(value, kbuildEvalObjectTree+"/"):
			// Generated object inputs do not exist at planning time.
		case !filepath.IsAbs(value):
			// Recursive Kbuild drivers run in the object tree, so relative roots
			// are object-tree-relative even if the checkout has the same spelling.
		default:
			return "", false, nil
		}
		root.prefix = strings.Trim(root.prefix, "/")
		roots = append(roots, root)
		index++
	}
	if len(roots) == 0 {
		return "", false, nil
	}
	namePattern := "*"
	typeFilter := byte(0)
	minDepth, maxDepth := 0, -1
	for index < len(words) {
		switch words[index] {
		case "-name":
			if index+1 >= len(words) {
				return "", true, fmt.Errorf("find -name has no pattern")
			}
			namePattern = words[index+1]
			index += 2
		case "-type":
			if index+1 >= len(words) || (words[index+1] != "f" && words[index+1] != "d") {
				return "", true, fmt.Errorf("find supports only -type f or -type d")
			}
			typeFilter = words[index+1][0]
			index += 2
		case "-mindepth", "-maxdepth":
			if index+1 >= len(words) {
				return "", true, fmt.Errorf("find %s has no depth", words[index])
			}
			depth, parseErr := strconv.Atoi(words[index+1])
			if parseErr != nil || depth < 0 {
				return "", true, fmt.Errorf("find %s has invalid depth %q", words[index], words[index+1])
			}
			if words[index] == "-mindepth" {
				minDepth = depth
			} else {
				maxDepth = depth
			}
			index += 2
		case "-print":
			index++
		default:
			return "", true, fmt.Errorf("unsupported hermetic find expression %q", strings.Join(words[index:], " "))
		}
	}
	matches := []string{}
	for _, root := range roots {
		if !root.source {
			continue
		}
		entries := sourceIndex.files
		if typeFilter == 'd' {
			entries = sourceIndex.directories
		}
		for _, entry := range entries {
			relative := entry
			if root.prefix != "" {
				if entry == root.prefix {
					relative = ""
				} else if suffix, ok := strings.CutPrefix(entry, root.prefix+"/"); ok {
					relative = suffix
				} else {
					continue
				}
			}
			depth := 0
			if relative != "" {
				depth = strings.Count(relative, "/") + 1
			}
			if depth < minDepth || (maxDepth >= 0 && depth > maxDepth) {
				continue
			}
			matched, matchErr := pathpkg.Match(namePattern, pathpkg.Base(entry))
			if matchErr != nil {
				return "", true, fmt.Errorf("invalid find -name pattern %q: %w", namePattern, matchErr)
			}
			if matched {
				matches = append(matches, kbuildEvalSourceTree+"/"+entry)
			}
		}
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return "", true, nil
	}
	return strings.Join(matches, "\n") + "\n", true, nil
}

type kbuildInvocationSentinelNormalization struct {
	root               string
	physicalRoot       string
	sourceArchitecture string
}

func newKbuildInvocationSentinelNormalization(rootDir, srcarch string) kbuildInvocationSentinelNormalization {
	root := filepath.ToSlash(filepath.Clean(rootDir))
	physicalRoot := root
	if resolved, err := filepath.EvalSymlinks(rootDir); err == nil {
		physicalRoot = filepath.ToSlash(filepath.Clean(resolved))
	}
	return kbuildInvocationSentinelNormalization{
		root:               root,
		physicalRoot:       physicalRoot,
		sourceArchitecture: srcarch,
	}
}

func (normalization kbuildInvocationSentinelNormalization) value(value string) string {
	// Bazel presents external repositories through a symlink forest. Root Make
	// can therefore export either the lexical execroot spelling or the physical
	// repository-cache spelling, depending on whether a value was derived
	// through $(realpath ...). Both describe the same declared source tree and
	// must produce the same content-addressed compiler probe.
	value = kbuildInvocationSentinelValueForArch(normalization.physicalRoot, normalization.sourceArchitecture, value)
	return kbuildInvocationSentinelValueForArch(normalization.root, normalization.sourceArchitecture, value)
}

func (normalization kbuildInvocationSentinelNormalization) normalizedVariableBase(base map[string]string) map[string]string {
	values := make(map[string]string, len(base)+9)
	for name, value := range base {
		values[name] = normalization.value(value)
	}
	values["srctree"] = kbuildEvalSourceTree
	values["abs_srctree"] = kbuildEvalSourceTree
	values["srcroot"] = kbuildEvalSourceTree
	values["objtree"] = kbuildEvalObjectTree
	values["abs_output"] = kbuildEvalObjectTree
	values["CURDIR"] = kbuildEvalObjectTree
	values["PWD"] = kbuildEvalObjectTree
	values["obj"] = "."
	values["src"] = kbuildEvalSourceTree
	return values
}

func kbuildInvocationSentinelVariableOverrides(directory string) map[string]string {
	values := make(map[string]string, 2)
	directory = filepath.ToSlash(strings.Trim(directory, "/"))
	values["obj"] = "."
	values["src"] = kbuildEvalSourceTree
	if directory != "" {
		values["obj"] = directory
		values["src"] += "/" + directory
	}
	return values
}

func kbuildInvocationSentinelVariables(rootDir string, base map[string]string, directory string) map[string]string {
	srcarch := filepath.ToSlash(strings.Trim(base["SRCARCH"], "/"))
	normalization := newKbuildInvocationSentinelNormalization(rootDir, srcarch)
	values := normalization.normalizedVariableBase(base)
	for name, value := range kbuildInvocationSentinelVariableOverrides(directory) {
		values[name] = value
	}
	return values
}

func kbuildInvocationSentinelValueForArch(root, srcarch, value string) string {
	if root == "" || root == "." {
		return value
	}
	if srcarch != "" {
		value = strings.ReplaceAll(value, root+"/arch/"+srcarch+"/include/generated", kbuildEvalObjectTree+"/arch/"+srcarch+"/include/generated")
	}
	value = strings.ReplaceAll(value, root+"/include/generated", kbuildEvalObjectTree+"/include/generated")
	return strings.ReplaceAll(value, root, kbuildEvalSourceTree)
}

// graphReachabilityMemo memoizes transitive predecessor queries for one
// immutable graph round. The maps passed to newGraphReachabilityMemo must not be
// mutated while the memo is in use; callers must create a new memo after adding
// or removing an edge.
type graphReachabilityMemo[node comparable] struct {
	dependencies           map[node]map[node]bool
	additionalDependencies map[node]map[node]bool
	predecessors           map[node]map[node]bool
	reachability           map[graphReachabilityKey[node]]bool
	onWalk                 func()
	onHit                  func()
}

type graphReachabilityKey[node comparable] struct {
	from node
	to   node
}

func newGraphReachabilityMemo[node comparable](
	dependencies map[node]map[node]bool,
	additionalDependencies map[node]map[node]bool,
	onWalk func(),
	onHit func(),
) *graphReachabilityMemo[node] {
	return &graphReachabilityMemo[node]{
		dependencies:           dependencies,
		additionalDependencies: additionalDependencies,
		predecessors:           map[node]map[node]bool{},
		reachability:           map[graphReachabilityKey[node]]bool{},
		onWalk:                 onWalk,
		onHit:                  onHit,
	}
}

// allPredecessors returns the transitive dependencies of node. The returned
// set is owned by the memo and must be treated as read-only.
func (m *graphReachabilityMemo[node]) allPredecessors(value node) map[node]bool {
	if predecessors, cached := m.predecessors[value]; cached {
		if m.onHit != nil {
			m.onHit()
		}
		return predecessors
	}
	if m.onWalk != nil {
		m.onWalk()
	}
	predecessors := map[node]bool{}
	pending := make([]node, 0, len(m.dependencies[value])+len(m.additionalDependencies[value]))
	for dependency := range m.dependencies[value] {
		pending = append(pending, dependency)
	}
	for dependency := range m.additionalDependencies[value] {
		pending = append(pending, dependency)
	}
	for len(pending) != 0 {
		candidate := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if predecessors[candidate] {
			continue
		}
		predecessors[candidate] = true
		for dependency := range m.dependencies[candidate] {
			pending = append(pending, dependency)
		}
		for dependency := range m.additionalDependencies[candidate] {
			pending = append(pending, dependency)
		}
	}
	m.predecessors[value] = predecessors
	return predecessors
}

func (m *graphReachabilityMemo[node]) reaches(from, to node) bool {
	key := graphReachabilityKey[node]{from: from, to: to}
	if reachable, cached := m.reachability[key]; cached {
		if m.onHit != nil {
			m.onHit()
		}
		return reachable
	}
	if predecessors, cached := m.predecessors[from]; cached {
		if m.onHit != nil {
			m.onHit()
		}
		reachable := predecessors[to]
		m.reachability[key] = reachable
		return reachable
	}
	if m.onWalk != nil {
		m.onWalk()
	}
	reachable := false
	seen := map[node]bool{}
	pending := make([]node, 0, len(m.dependencies[from])+len(m.additionalDependencies[from]))
	for dependency := range m.dependencies[from] {
		pending = append(pending, dependency)
	}
	for dependency := range m.additionalDependencies[from] {
		pending = append(pending, dependency)
	}
	for len(pending) != 0 {
		candidate := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if candidate == to {
			reachable = true
			break
		}
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		for dependency := range m.dependencies[candidate] {
			pending = append(pending, dependency)
		}
		for dependency := range m.additionalDependencies[candidate] {
			pending = append(pending, dependency)
		}
	}
	m.reachability[key] = reachable
	return reachable
}

// selectedKbuildSelections turns the selected top-level Make goal closure into
// the only concrete actions consumed by lowering. The root prepare closure
// determines lifecycle independently from compiler scope. Source-traced action
// roles seed host/target scope; host scope propagates through neutral
// prerequisites until an explicitly target-scoped action forms a cross-scope
// boundary. Neutral actions default to the target scope. Generated collection
// names and output filenames carry no scope policy.
func selectedKbuildSelections(
	profiles []kconfig.CompactKbuildProfile,
	satisfied map[string]bool,
) ([]kconfig.CompactKbuildSelection, error) {
	return selectedKbuildSelectionsWithStatsAndSourceRoot(profiles, satisfied, nil, "")
}

type kbuildSelectionWalkStats struct {
	Scheduled                   int
	EvaluatedTargets            int
	RuleCandidateChecks         int
	RecipeEvaluations           int
	MaxPending                  int
	VisibleOwnerCandidateChecks int
	InvocationDescentWalks      int
	ReachabilityWalks           int
	ReachabilityCacheHits       int
	OpaqueRootPredecessorChecks int
}

// kbuildSelectionPrerequisite keeps the two identities of a parsed Make
// prerequisite separate. target is the canonical graph/output identity;
// makeTarget is the lexical filename GNU Make uses for implicit-rule lookup.
// In particular, makeTarget may deliberately contain parent traversal while
// target never does.
type kbuildSelectionPrerequisite struct {
	target     string
	makeTarget string
}

// kbuildGeneratedContentResolver registers and replays one immutable-source
// generator whose exact bytes affect compiler include closure. recognized is
// true only when the complete selected recipe is owned by the resolver;
// concrete is false during probe discovery and true during result replay.
type kbuildGeneratedContentResolver func(
	profile kconfig.CompactKbuildProfile,
	target, recipe string,
	sourcePrerequisites, generatedPrerequisites []string,
) (contents string, concrete, recognized bool, err error)

func selectedKbuildSelectionsWithStats(
	profiles []kconfig.CompactKbuildProfile,
	satisfied map[string]bool,
	stats *kbuildSelectionWalkStats,
) ([]kconfig.CompactKbuildSelection, error) {
	return selectedKbuildSelectionsWithStatsAndSourceRoot(profiles, satisfied, stats, "")
}

func selectedKbuildSelectionsFromSourceRoot(
	profiles []kconfig.CompactKbuildProfile,
	satisfied map[string]bool,
	sourceRoot string,
) ([]kconfig.CompactKbuildSelection, error) {
	return selectedKbuildSelectionsWithStatsAndSourceRoot(profiles, satisfied, nil, sourceRoot)
}

func selectedKbuildSelectionsWithStatsAndSourceRoot(
	profiles []kconfig.CompactKbuildProfile,
	satisfied map[string]bool,
	stats *kbuildSelectionWalkStats,
	sourceRoot string,
) ([]kconfig.CompactKbuildSelection, error) {
	return selectedKbuildSelectionsWithStatsSourceRootAndGeneratedContent(
		profiles, satisfied, stats, sourceRoot, nil,
	)
}

func selectedKbuildSelectionsWithStatsSourceRootAndGeneratedContent(
	profiles []kconfig.CompactKbuildProfile,
	satisfied map[string]bool,
	stats *kbuildSelectionWalkStats,
	sourceRoot string,
	generatedContent kbuildGeneratedContentResolver,
) ([]kconfig.CompactKbuildSelection, error) {
	return selectedKbuildSelectionsWithStatsSourceRootPreparationTargetsAndGeneratedContent(
		profiles, satisfied, stats, sourceRoot, nil, generatedContent,
	)
}

func selectedKbuildSelectionsWithStatsSourceRootPreparationTargetsAndGeneratedContent(
	profiles []kconfig.CompactKbuildProfile,
	satisfied map[string]bool,
	stats *kbuildSelectionWalkStats,
	sourceRoot string,
	preparationTargets []string,
	generatedContent kbuildGeneratedContentResolver,
) ([]kconfig.CompactKbuildSelection, error) {
	return selectedKbuildSelectionsWithResolvedTargets(
		profiles, satisfied, stats, sourceRoot, preparationTargets, generatedContent, nil, false,
	)
}

func selectedKbuildSelectionsWithResolvedTargets(
	profiles []kconfig.CompactKbuildProfile,
	satisfied map[string]bool,
	stats *kbuildSelectionWalkStats,
	sourceRoot string,
	preparationTargets []string,
	generatedContent kbuildGeneratedContentResolver,
	resolvedTargets *kbuildResolvedTargetCache,
	preconfiguredObjectTree bool,
) ([]kconfig.CompactKbuildSelection, error) {
	var err error
	preparationTargets, err = canonicalKbuildPreparationTargets(preparationTargets)
	if err != nil {
		return nil, err
	}
	if len(preparationTargets) != 0 && len(profiles) == 0 {
		return nil, fmt.Errorf("Kbuild preparation targets require a root invocation profile")
	}
	type workItem struct {
		profile    int
		target     string
		makeTarget string
		lifecycle  string
	}
	type actionIdentity struct {
		profile int
		target  string
	}
	type groupedActionIdentity struct {
		profile   int
		ruleIndex int
		stem      string
	}
	type deferredQueryEvaluation struct {
		query                kconfig.KbuildDeferredContentQuery
		origin               actionIdentity
		lifecycle            string
		consumers            map[actionIdentity]bool
		dependencies         map[actionIdentity]bool
		initialArtifacts     []kconfig.CompactKbuildVisibleArtifact
		generatedArtifacts   []kconfig.CompactKbuildVisibleArtifact
		initialArtifactSet   map[kconfig.CompactKbuildVisibleArtifact]bool
		generatedArtifactSet map[kconfig.CompactKbuildVisibleArtifact]bool
	}
	type targetEvaluation struct {
		prerequisites        []string
		prerequisiteWork     []kbuildSelectionPrerequisite
		sourcePrerequisites  []string
		normalPrerequisites  []string
		orderOnly            []string
		normalMakeTargets    []string
		orderOnlyMakeTargets []string
		lookupTarget         string
		automaticTarget      string
		stem                 string
		resolvedTarget       *kconfig.CompactKbuildResolvedTarget
		commandTexts         []string
		children             []workItem
		groupedPeers         []string
		materialized         bool
		unruled              bool
		// objectTreeSnapshot is source-derived from the exact selected recipe.
		// It distinguishes an action which can consume the recursive Make
		// invocation's initial object-tree frontier from one which merely runs
		// after that frontier as a weak execution-order consequence.
		objectTreeSnapshot         bool
		objectTreeAllVisible       bool
		objectTreeReferences       []string
		directObjectTreeAllVisible bool
		directObjectTreeReferences []string
		objectTreePrograms         []string
		producesNonIncludeOutput   bool
		includeSearches            []kbuildIncludeSearchPlan
		actionRoles                []kconfig.KbuildActionRoleRef
		primaryActionRoles         []kconfig.KbuildActionRoleRef
		deferredQueries            []kconfig.KbuildDeferredContentQuery
		sourceProjection           string
		projectionAmbiguous        bool
		literalProjection          string
		literalProjectionSet       bool
		literalProjectionAmbiguous bool
		generatedContent           string
		generatedContentSet        bool
		generatedContentPending    bool
		generatedContentRecognized bool
		evaluatedCommandTexts      int
	}
	if stats != nil {
		*stats = kbuildSelectionWalkStats{}
	}
	queue := []workItem{}
	head := 0
	profileByName := map[string]int{}
	indexes := make([]*kbuildProfileTargetIndex, len(profiles))
	currentness := make([]*kbuildProfileTargetSatisfaction, len(profiles))
	evaluations := make([]map[string]targetEvaluation, len(profiles))
	groupedTriggers := map[groupedActionIdentity]actionIdentity{}
	groupedMembers := map[groupedActionIdentity]map[actionIdentity]bool{}
	groupedAction := map[actionIdentity]groupedActionIdentity{}
	quotedIncludes := newKbuildQuotedIncludeIndex(sourceRoot)
	for index, profile := range profiles {
		profileByName[profile.Name] = index
		indexes[index] = newKbuildProfileTargetIndexWithResolvedTargets(profile, stats, resolvedTargets)
		currentness[index] = newKbuildProfileTargetSatisfaction(profile, indexes[index], satisfied)
		evaluations[index] = map[string]targetEvaluation{}
	}
	scheduled := map[workItem]bool{}
	normalizeWorkItem := func(item workItem, includeSatisfied bool) (workItem, bool) {
		if item.profile < 0 || item.profile >= len(profiles) {
			return workItem{}, false
		}
		item.target = kconfig.CanonicalKbuildGraphTarget(item.target)
		item.makeTarget = kbuildProfileLookupTarget(profiles[item.profile], item.target, item.makeTarget)
		if item.target == "" || item.target == "FORCE" ||
			currentness[item.profile].targetIsSatisfied(item.target) && !includeSatisfied {
			return workItem{}, false
		}
		if item.lifecycle == "" {
			item.lifecycle = "target"
		}
		return item, true
	}
	enqueueWorkItem := func(item workItem, includeSatisfied bool) (workItem, bool) {
		item, ok := normalizeWorkItem(item, includeSatisfied)
		if !ok {
			return workItem{}, false
		}
		if scheduled[item] {
			return item, true
		}
		scheduled[item] = true
		queue = append(queue, item)
		if stats != nil {
			stats.Scheduled++
			if pending := len(queue) - head; pending > stats.MaxPending {
				stats.MaxPending = pending
			}
		}
		return item, true
	}
	enqueue := func(item workItem) (workItem, bool) {
		return enqueueWorkItem(item, false)
	}
	enqueueGroupedPeer := func(item workItem) (workItem, bool) {
		// A peer may already exist in the source tree. Once another output has
		// triggered the grouped rule, GNU Make still evaluates every peer's merged
		// prerequisite declarations before running the one shared recipe.
		return enqueueWorkItem(item, true)
	}
	preparationRoots := make(map[string]bool, len(preparationTargets))
	resolvedPreparationRoots := make(map[string]bool, len(preparationTargets))
	for _, target := range preparationTargets {
		preparationRoots[target] = true
	}
	// Invocation discovery always appends the explicitly requested root before
	// following any recursive Make recipe. Seed from that structural root
	// position, not from a diagnostic profile-name convention.
	if len(profiles) != 0 {
		rootEntryTargets := make(map[string]bool, len(profiles[0].EntryTargets))
		for _, target := range profiles[0].EntryTargets {
			rootEntryTargets[kconfig.CanonicalKbuildGraphTarget(target)] = true
		}
		for _, target := range preparationTargets {
			if !rootEntryTargets[target] {
				return nil, fmt.Errorf(
					"Kbuild preparation target %q is absent from root profile %q entry targets",
					target, profiles[0].Name,
				)
			}
		}
		for _, target := range profiles[0].EntryTargets {
			canonicalTarget := kconfig.CanonicalKbuildGraphTarget(target)
			lifecycle := "target"
			if preparationRoots[canonicalTarget] {
				lifecycle = "prep"
				if currentness[0].targetIsSatisfied(canonicalTarget) {
					resolvedPreparationRoots[canonicalTarget] = true
				}
			}
			enqueue(workItem{profile: 0, target: target, lifecycle: lifecycle})
		}
	}

	dependencies := map[workItem][]workItem{}
	materialized := map[workItem]bool{}
	for head < len(queue) {
		item := queue[head]
		queue[head] = workItem{}
		head++
		if err := validateKbuildTraversalTarget(profiles[item.profile].Name, "selected goal", item.target); err != nil {
			return nil, err
		}
		profile := profiles[item.profile]
		evaluation, cached := evaluations[item.profile][item.target]
		if !cached {
			if stats != nil {
				stats.EvaluatedTargets++
			}
			ruleIndexes := kbuildProfileRuleIndexesForMakeTargetIndexed(
				profile, indexes[item.profile], item.target, item.makeTarget, satisfied,
			)
			effectiveRecipeIndexes, effectiveRecipeErr := kconfig.EffectiveCompactKbuildRecipeRuleIndexes(profile, ruleIndexes)
			if effectiveRecipeErr != nil {
				return nil, fmt.Errorf("select effective recipes for target %s: %w", item.target, effectiveRecipeErr)
			}
			effectiveRecipeSet := make(map[int]bool, len(effectiveRecipeIndexes))
			for _, ruleIndex := range effectiveRecipeIndexes {
				effectiveRecipeSet[ruleIndex] = true
			}
			rules := make([]kconfig.KbuildRule, 0, len(ruleIndexes))
			for _, ruleIndex := range ruleIndexes {
				rules = append(rules, profile.Rules[ruleIndex])
			}
			var normal, orderOnly []string
			var normalWork, orderOnlyWork []kbuildSelectionPrerequisite
			evaluation.lookupTarget = item.makeTarget
			evaluation.automaticTarget = item.makeTarget
			selectedStem := ""
			syntheticGroupedPeer := false
			groupedTrigger := actionIdentity{}
			if len(rules) != 0 {
				var contextErr error
				normalWork, orderOnlyWork, selectedStem, evaluation.resolvedTarget, contextErr =
					evaluateSelectedKbuildRuleContextForMakeTarget(
						profile, indexes[item.profile], item.target, item.makeTarget,
						ruleIndexes, effectiveRecipeIndexes,
					)
				if contextErr != nil {
					return nil, fmt.Errorf("evaluate selected target %s prerequisite context: %w", item.target, contextErr)
				}
				normal = kbuildSelectionPrerequisiteTargets(normalWork)
				orderOnly = kbuildSelectionPrerequisiteTargets(orderOnlyWork)
				evaluation.normalPrerequisites = append([]string(nil), normal...)
				evaluation.orderOnly = append([]string(nil), orderOnly...)
				evaluation.normalMakeTargets = kbuildSelectionPrerequisiteMakeTargets(normalWork)
				evaluation.orderOnlyMakeTargets = kbuildSelectionPrerequisiteMakeTargets(orderOnlyWork)
				evaluation.stem = selectedStem
				for _, prerequisite := range append(normalWork, orderOnlyWork...) {
					candidate := kconfig.CanonicalKbuildGraphTarget(prerequisite.target)
					if candidate == "" || candidate == "FORCE" {
						continue
					}
					if currentness[item.profile].targetIsSatisfied(candidate) {
						evaluation.sourcePrerequisites = append(evaluation.sourcePrerequisites, candidate)
					} else {
						evaluation.prerequisites = append(evaluation.prerequisites, candidate)
						evaluation.prerequisiteWork = append(evaluation.prerequisiteWork, kbuildSelectionPrerequisite{
							target: candidate, makeTarget: prerequisite.makeTarget,
						})
					}
				}
				groupedRuleIndex, peers, grouped, groupedErr := selectedKbuildGroupedRuleOutputs(
					profile, ruleIndexes, item.target, selectedStem,
				)
				if groupedErr != nil {
					return nil, fmt.Errorf("evaluate selected target %s grouped outputs: %w", item.target, groupedErr)
				}
				identity := actionIdentity{profile: item.profile, target: item.target}
				if grouped {
					group := groupedActionIdentity{profile: item.profile, ruleIndex: groupedRuleIndex, stem: selectedStem}
					authorityRule, authorityStem, authorityTarget, authorityPeers, authorityExists :=
						kconfig.CompactKbuildGroupedActionForTarget(profile, item.target)
					if !authorityExists {
						return nil, fmt.Errorf("selected grouped Kbuild target %s has no source-order trigger authority", item.target)
					}
					if authorityRule != groupedRuleIndex || authorityStem != selectedStem || !slices.Equal(authorityPeers, peers) {
						return nil, fmt.Errorf("selected grouped Kbuild target %s disagrees with its source-order trigger authority", item.target)
					}
					if previous, exists := groupedAction[identity]; exists && previous != group {
						return nil, fmt.Errorf("selected target %s resolves to conflicting grouped Kbuild rules", item.target)
					}
					authoritativeTrigger := actionIdentity{profile: item.profile, target: authorityTarget}
					if previous, exists := groupedTriggers[group]; exists && previous != authoritativeTrigger {
						return nil, fmt.Errorf("selected grouped Kbuild target %s has conflicting trigger authorities", item.target)
					}
					groupedTriggers[group] = authoritativeTrigger
					groupedTrigger = groupedTriggers[group]
					syntheticGroupedPeer = groupedTrigger != identity
					if groupedMembers[group] == nil {
						groupedMembers[group] = map[actionIdentity]bool{}
					}
					for _, peer := range peers {
						peerIdentity := actionIdentity{profile: item.profile, target: peer}
						if previous, exists := groupedAction[peerIdentity]; exists && previous != group {
							return nil, fmt.Errorf("grouped Kbuild output %s belongs to conflicting selected rule instances", peer)
						}
						groupedAction[peerIdentity] = group
						groupedMembers[group][peerIdentity] = true
						if !syntheticGroupedPeer && peer != item.target {
							evaluation.groupedPeers = append(evaluation.groupedPeers, peer)
						}
					}
					if syntheticGroupedPeer {
						enqueueGroupedPeer(workItem{profile: groupedTrigger.profile, target: groupedTrigger.target, lifecycle: item.lifecycle})
					}
				} else if _, expectedGrouped := groupedAction[identity]; expectedGrouped {
					return nil, fmt.Errorf("grouped Kbuild output %s resolves to a different effective recipe", item.target)
				}
			}
			// Generated assignments select and classify the closure, but they do
			// not themselves own an action.  Only a source-evaluated recipe can
			// materialize a file; recursive children may own a generated pathname
			// declared by their parent invocation.
			recipeRules := make([]kconfig.KbuildRule, 0, len(effectiveRecipeIndexes))
			for ruleOffset, rule := range rules {
				if effectiveRecipeSet[ruleIndexes[ruleOffset]] {
					recipeRules = append(recipeRules, rule)
				}
			}
			if syntheticGroupedPeer {
				// Peer-specific prerequisite-only declarations are real GNU Make
				// scheduling inputs, but the grouped recipe itself runs in the first
				// reached target's automatic-variable context. Replaying it for a
				// synthetic peer can discover spurious probes, recursive invocations,
				// or action roles from a `$@` branch which never executes.
				recipeRules = nil
			}
			for _, rule := range recipeRules {
				// GNU Make chooses one effective rule context for the target. Explicit
				// declarations merged with a selected pattern rule share that rule's
				// automatic $* value; recomputing a stem from each declaration loses
				// it for expressions such as $(syscall_abis_$*).
				stem := selectedStem
				automaticTarget := kbuildProfileRuleAutomaticTarget(profile, rule, item.target, stem)
				evaluation.automaticTarget = automaticTarget
				injections, injectionErr := kconfig.CompactKbuildTargetEvaluationInjectionsForMakeTarget(
					profile, item.target, item.makeTarget, automaticTarget, stem,
					evaluation.normalMakeTargets, evaluation.orderOnlyMakeTargets,
				)
				if injectionErr != nil {
					return nil, fmt.Errorf("evaluate selected target %s target-context paths: %w", item.target, injectionErr)
				}
				injections["Q"] = ""
				for _, recipe := range rule.Recipe {
					expanded, recipeRoles, err := kconfig.EvaluateCompactKbuildTextActionRolesForMakeTarget(
						profile, item.target, item.makeTarget, automaticTarget, stem,
						evaluation.normalMakeTargets, evaluation.orderOnlyMakeTargets, injections, recipe,
					)
					if err != nil {
						return nil, fmt.Errorf("evaluate selected target %s action-role provenance: %w", item.target, err)
					}
					// Command-template selection intentionally projects an
					// if_changed[_rule] wrapper onto its cmd_<name> payload for
					// compiler semantics.  Generated command heads in the wrapper
					// remain real executable inputs, however: cmd_and_fixdep is the
					// canonical example.  Observe those heads from the exact expanded
					// recipe before replacing it with the inner command template.
					if programs, programErr := kconfig.CompactKbuildObjectTreeCommandPrograms(profile, expanded); programErr == nil {
						evaluation.objectTreePrograms = append(evaluation.objectTreePrograms, programs...)
					}
					lineRule := rule
					lineRule.Recipe = []string{recipe}
					lineRule.Prerequisites = evaluation.normalMakeTargets
					lineRule.OrderOnly = evaluation.orderOnlyMakeTargets
					templates, templateErr := kconfig.EvaluateCompactKbuildCommandTemplatesSymbolicForMakeTarget(
						profile, lineRule, item.target, item.makeTarget, automaticTarget, stem, injections,
					)
					if templateErr != nil {
						return nil, fmt.Errorf("evaluate selected command template for %s target %q: %w", profile.Name, item.target, templateErr)
					}
					directTemplateOwnsWholeRecipe := len(templates) == 1 && templates[0].Name == "" &&
						strings.TrimSpace(templates[0].Source) == strings.TrimSpace(recipe)
					templateOwnsWholeRecipe := len(templates) == 0 || directTemplateOwnsWholeRecipe ||
						kconfig.CompactKbuildRecipeIsExactCommandTemplateCall(recipe)
					if !templateOwnsWholeRecipe {
						// Command-template discovery deliberately finds calls embedded in
						// larger recipe lines. Replacing such a line with cmd_<name> is
						// correct for action discovery, but any shell tail can rewrite the
						// target and therefore invalidates byte provenance.
						evaluation.projectionAmbiguous = true
						evaluation.literalProjectionAmbiguous = true
					}
					usageTexts := []string{expanded}
					if len(templates) != 0 {
						// Final lowering executes cmd_<name>, not Kbuild's outer
						// if_changed[_dep] incremental-Make bookkeeping wrapper.
						usageTexts = usageTexts[:0]
						for _, template := range templates {
							usageTexts = append(usageTexts, template.Text)
						}
					}
					// Final lowering gives a sole direct filechk call its selected
					// payload's stdout, not Make's incremental tmp/cmp/mv wrapper.
					// Use that same candidate for content AND frontier observations;
					// the resolver still proves command, environment and input closure.
					if generatedContent != nil && templateOwnsWholeRecipe && len(recipeRules) == 1 &&
						len(rule.Recipe) == 1 && evaluation.resolvedTarget != nil {
						projected, selected, projectionErr := evaluation.resolvedTarget.SelectedDirectFilechkOutputRecipe()
						if projectionErr != nil {
							return nil, fmt.Errorf("evaluate selected filechk payload for %s: %w", item.target, projectionErr)
						}
						if selected {
							usageTexts = []string{projected}
						}
					}
					for _, usageText := range usageTexts {
						resolvedUsageText, resolveErr := kconfig.ResolveCompactKbuildTargetSymbolicText(profile, item.target, usageText)
						if resolveErr != nil {
							return nil, fmt.Errorf("resolve selected command template for %s target %q: %w", profile.Name, item.target, resolveErr)
						}
						evaluation.commandTexts = append(evaluation.commandTexts, resolvedUsageText)
						evaluation.evaluatedCommandTexts++
						if generatedContent != nil && !preconfiguredObjectTree {
							contents, concrete, recognized, contentErr := generatedContent(
								profile, item.target, resolvedUsageText,
								evaluation.sourcePrerequisites, evaluation.prerequisites,
							)
							if contentErr != nil {
								return nil, fmt.Errorf("probe selected target %s generated content: %w", item.target, contentErr)
							}
							if recognized {
								evaluation.generatedContentRecognized = true
								if concrete {
									evaluation.generatedContent = contents
									evaluation.generatedContentSet = true
								} else {
									evaluation.generatedContentPending = true
								}
							}
						}
						observation := kconfig.ObserveCompactKbuildObjectTree(resolvedUsageText)
						if observation.ObservesObjectTree {
							evaluation.objectTreeSnapshot = true
							evaluation.objectTreeAllVisible = evaluation.objectTreeAllVisible || observation.ObservesAll
							evaluation.objectTreeReferences = append(evaluation.objectTreeReferences, observation.References...)
							evaluation.directObjectTreeAllVisible = evaluation.directObjectTreeAllVisible || observation.ObservesAll
							evaluation.directObjectTreeReferences = append(evaluation.directObjectTreeReferences, observation.References...)
						}
						programs, programErr := kconfig.CompactKbuildObjectTreeCommandPrograms(profile, resolvedUsageText)
						if programErr == nil {
							// This is an optional narrowing observation. If shell command
							// heads are dynamic, keep opaque compiler include roots
							// conservative instead of guessing which selected output is a
							// program.
							evaluation.objectTreePrograms = append(evaluation.objectTreePrograms, programs...)
						}
						nonIncludeOutput, nonIncludeOutputErr := kconfig.CompactKbuildRecipeProducesNonIncludeOutput(
							profile, resolvedUsageText, item.target,
						)
						if nonIncludeOutputErr == nil {
							evaluation.producesNonIncludeOutput = evaluation.producesNonIncludeOutput || nonIncludeOutput
						}
						if slices.ContainsFunc(recipeRoles, func(role kconfig.KbuildActionRoleRef) bool { return kbuildCompilerIncludeRole(role.Role) }) {
							searches, searchErr := kbuildSelectedRecipeIncludeSearchReferences(
								profile, resolvedUsageText,
								evaluation.sourcePrerequisites, evaluation.prerequisites,
							)
							if searchErr != nil {
								return nil, fmt.Errorf("inspect selected target %s include search references: %w", item.target, searchErr)
							}
							for _, search := range searches {
								if len(search.sources) != 0 || len(search.generatedSources) != 0 || len(search.forced) != 0 {
									evaluation.includeSearches = append(evaluation.includeSearches, search)
								}
							}
						}
						projection, exactProjection, writesTarget, opaqueProjection := kbuildSelectedRecipeSourceProjection(
							resolvedUsageText, item.target, evaluation.sourcePrerequisites,
						)
						if opaqueProjection {
							evaluation.projectionAmbiguous = true
						}
						if writesTarget {
							if !exactProjection || evaluation.sourceProjection != "" && evaluation.sourceProjection != projection {
								evaluation.projectionAmbiguous = true
							} else if evaluation.sourceProjection == "" {
								evaluation.sourceProjection = projection
							}
						}
						literal, exactLiteral, writesLiteral, opaqueLiteral := kbuildSelectedRecipeLiteralProjection(
							resolvedUsageText, item.target,
						)
						if opaqueLiteral {
							evaluation.literalProjectionAmbiguous = true
						}
						if writesLiteral {
							if !exactLiteral || evaluation.literalProjectionSet && evaluation.literalProjection != literal {
								evaluation.literalProjectionAmbiguous = true
							} else if !evaluation.literalProjectionSet {
								evaluation.literalProjection = literal
								evaluation.literalProjectionSet = true
							}
						}
						observation, observationErr := kconfig.EvaluateCompactKbuildSourceScriptObjectTreeObservationForMakeTarget(
							profile, item.target, item.makeTarget, automaticTarget, stem,
							evaluation.normalMakeTargets, evaluation.orderOnlyMakeTargets,
							injections, resolvedUsageText,
						)
						if observationErr != nil {
							return nil, fmt.Errorf("inspect selected target %s source-script object-tree usage: %w", item.target, observationErr)
						}
						if observation.ObservesObjectTree {
							evaluation.objectTreeSnapshot = true
							evaluation.objectTreeAllVisible = evaluation.objectTreeAllVisible || observation.ObservesAll
							evaluation.objectTreeReferences = append(evaluation.objectTreeReferences, observation.References...)
							evaluation.directObjectTreeAllVisible = evaluation.directObjectTreeAllVisible || observation.ObservesAll
							evaluation.directObjectTreeReferences = append(evaluation.directObjectTreeReferences, observation.References...)
						}
					}
					processLocation, locationOK := kconfig.CompactKbuildProfileInvocationLocation(profile)
					if !locationOK {
						return nil, fmt.Errorf("selected target %s profile %q has no typed Kbuild invocation location", item.target, profile.Name)
					}
					invocations, invocationErr := kbuildRecursiveMakeInvocationsAt(expanded, processLocation)
					if invocationErr == nil && len(invocations) != 0 {
						if stats != nil {
							stats.RecipeEvaluations++
						}
						effects, effectsErr := kbuildSelectedRecipeExecutionEffects(
							profile, rule, item.target, item.makeTarget, automaticTarget, stem,
							evaluation.normalMakeTargets, evaluation.orderOnlyMakeTargets, recipe,
						)
						if effectsErr != nil {
							return nil, fmt.Errorf(
								"interpret selected recursive recipe for %s target %q: %w",
								profile.Name, item.target, effectsErr,
							)
						}
						for _, effect := range effects {
							evaluation.materialized = evaluation.materialized || effect.materializesTarget
						}
						for _, invocation := range invocations {
							child := kbuildInvocationProfileName(invocation.request)
							childIndex, exists := profileByName[child]
							if !exists {
								continue
							}
							// The child profile owns argv-goal canonicalization. Its
							// entry targets have already resolved source/object-root
							// markers against the evaluated invocation and must not be
							// interpreted relative to that profile's cwd again.
							goals := profiles[childIndex].EntryTargets
							for _, goal := range goals {
								evaluation.children = append(evaluation.children, workItem{profile: childIndex, target: goal})
							}
						}
						continue
					}
					if kbuildRecipeHasAction(recipe) && !kbuildRecipeOnlyCreatesDirectories(expanded) {
						evaluation.materialized = true
					}
				}
			}
			if syntheticGroupedPeer {
				peerPrerequisites := evaluation.prerequisites
				peerPrerequisiteWork := evaluation.prerequisiteWork
				peerSourcePrerequisites := evaluation.sourcePrerequisites
				peerGroupedPeers := evaluation.groupedPeers
				if physical, available := evaluations[groupedTrigger.profile][groupedTrigger.target]; available {
					evaluation = physical
				}
				evaluation.prerequisites = peerPrerequisites
				evaluation.prerequisiteWork = peerPrerequisiteWork
				evaluation.sourcePrerequisites = peerSourcePrerequisites
				evaluation.groupedPeers = peerGroupedPeers
			} else {
				for _, dependency := range profile.TargetInvocationDependencies {
					if dependency.Target != item.target {
						continue
					}
					childIndex, exists := profileByName[dependency.Profile]
					if !exists {
						continue
					}
					goals := dependency.Goals
					if len(goals) == 0 {
						goals = profiles[childIndex].EntryTargets
					}
					for _, goal := range goals {
						evaluation.children = append(evaluation.children, workItem{profile: childIndex, target: goal})
					}
				}
			}
			// A selected leaf with neither a source rule nor a recursive owner is
			// retained as an invocation-boundary side-output demand. Its producer
			// is proven after selection, but the strong artifact boundary must be
			// visible while solving the bounded physical stages.
			evaluation.unruled = len(rules) == 0 && len(evaluation.children) == 0
			if evaluation.materialized && !syntheticGroupedPeer {
				resolvedTarget := evaluation.resolvedTarget
				if resolvedTarget == nil {
					var resolveErr error
					resolvedTarget, resolveErr = indexes[item.profile].resolveTarget(
						profile, item.target, item.makeTarget,
					)
					if resolveErr != nil {
						return nil, fmt.Errorf("resolve selected target %s exact action provenance: %w", item.target, resolveErr)
					}
				}
				actionEffects, selectedAction, roleErr := resolvedTarget.SelectedTargetEffects()
				if roleErr != nil {
					return nil, fmt.Errorf("evaluate selected target %s exact action provenance: %w", item.target, roleErr)
				}
				evaluation.materialized = selectedAction
				evaluation.actionRoles = actionEffects.ActionRoles
				evaluation.primaryActionRoles = actionEffects.PrimaryActionRoles
				evaluation.deferredQueries = actionEffects.DeferredContentQueries
			}
			evaluations[item.profile][item.target] = evaluation
		}
		if item.profile == 0 && item.lifecycle == "prep" && preparationRoots[item.target] {
			if evaluation.unruled && !indexes[0].targetIsPhony(profile, item.target) {
				return nil, fmt.Errorf(
					"Kbuild preparation target %q has no source rule, recursive invocation, phony declaration, or satisfied input",
					item.target,
				)
			}
			resolvedPreparationRoots[item.target] = true
		}
		for _, prerequisite := range evaluation.prerequisiteWork {
			if dependency, ok := enqueue(workItem{
				profile: item.profile, target: prerequisite.target,
				makeTarget: prerequisite.makeTarget, lifecycle: item.lifecycle,
			}); ok {
				dependencies[item] = append(dependencies[item], dependency)
			}
		}
		for _, child := range evaluation.children {
			child.lifecycle = item.lifecycle
			if dependency, ok := enqueue(child); ok {
				dependencies[item] = append(dependencies[item], dependency)
			}
		}
		for _, peer := range evaluation.groupedPeers {
			enqueueGroupedPeer(workItem{profile: item.profile, target: peer, lifecycle: item.lifecycle})
		}
		materializedAction := evaluation.materialized
		if group, grouped := groupedAction[actionIdentity{profile: item.profile, target: item.target}]; grouped {
			trigger := groupedTriggers[group]
			materializedAction = evaluations[trigger.profile][trigger.target].materialized
		}
		if !materializedAction {
			continue
		}
		materialized[item] = true
	}
	for _, target := range preparationTargets {
		if !resolvedPreparationRoots[target] {
			return nil, fmt.Errorf("Kbuild preparation target %q was not resolved from the root invocation", target)
		}
	}
	// A FIFO closure walk may encounter a non-trigger peer before the DFS-selected
	// trigger. Once the trigger has been evaluated, project its one physical
	// materialization back onto every scheduled logical peer and lifecycle view.
	for item := range scheduled {
		identity := actionIdentity{profile: item.profile, target: item.target}
		group, grouped := groupedAction[identity]
		if !grouped {
			continue
		}
		trigger := groupedTriggers[group]
		if evaluations[trigger.profile][trigger.target].materialized {
			materialized[item] = true
		}
	}

	// Collapse duplicate lifecycle views to the exact materialized Make action.
	// Preparation wins when one action is reachable through both closures, but
	// action scope remains a separate fixed-point property below.
	selectedLifecycle := map[actionIdentity]string{}
	evaluationForAction := map[actionIdentity]targetEvaluation{}
	for item := range materialized {
		identity := actionIdentity{profile: item.profile, target: item.target}
		if selectedLifecycle[identity] == "" || item.lifecycle == "prep" {
			selectedLifecycle[identity] = item.lifecycle
		}
		evaluationForAction[identity] = evaluations[item.profile][item.target]
	}
	physicalAction := func(identity actionIdentity) actionIdentity {
		if group, grouped := groupedAction[identity]; grouped {
			return groupedTriggers[group]
		}
		return identity
	}
	for group, members := range groupedMembers {
		trigger := groupedTriggers[group]
		lifecycle := ""
		for member := range members {
			candidate := selectedLifecycle[member]
			if candidate != "" && (lifecycle == "" || candidate == "prep") {
				lifecycle = candidate
			}
		}
		if lifecycle == "" {
			continue
		}
		physicalEvaluation := evaluations[trigger.profile][trigger.target]
		for member := range members {
			if _, selected := selectedLifecycle[member]; selected {
				selectedLifecycle[member] = lifecycle
				evaluationForAction[member] = physicalEvaluation
			}
		}
	}
	sourceLifecycle := map[actionIdentity]string{}
	for item := range scheduled {
		identity := actionIdentity{profile: item.profile, target: item.target}
		if sourceLifecycle[identity] == "" || item.lifecycle == "prep" {
			sourceLifecycle[identity] = item.lifecycle
		}
	}
	for _, members := range groupedMembers {
		lifecycle := ""
		for member := range members {
			candidate := sourceLifecycle[member]
			if candidate != "" && (lifecycle == "" || candidate == "prep") {
				lifecycle = candidate
			}
		}
		for member := range members {
			if _, selected := sourceLifecycle[member]; selected {
				sourceLifecycle[member] = lifecycle
			}
		}
	}
	queryEvaluations := map[string]*deferredQueryEvaluation{}
	queryTokensByAction := map[actionIdentity][]string{}
	for consumer, evaluation := range evaluationForAction {
		for _, query := range evaluation.deferredQueries {
			originProfile := query.Origin.Profile
			if originProfile == "" {
				originProfile = query.Profile.Name
			}
			originTarget := query.Origin.Target
			if originTarget == "" {
				originTarget = query.Target
			}
			profileIndex, ok := profileByName[originProfile]
			if !ok {
				return nil, fmt.Errorf("deferred Kbuild content query %q references missing origin profile %q", query.Token, originProfile)
			}
			origin := actionIdentity{profile: profileIndex, target: kconfig.CanonicalKbuildGraphTarget(originTarget)}
			lifecycle := sourceLifecycle[origin]
			if lifecycle == "" {
				return nil, fmt.Errorf(
					"deferred Kbuild content query %q origin %s:%s is outside the selected source closure",
					query.Token, originProfile, originTarget,
				)
			}
			effect := queryEvaluations[query.Token]
			if effect == nil {
				effect = &deferredQueryEvaluation{
					query: query, origin: origin, lifecycle: lifecycle,
					consumers: map[actionIdentity]bool{}, dependencies: map[actionIdentity]bool{},
				}
				queryEvaluations[query.Token] = effect
			} else if effect.query.Command != query.Command || effect.origin != origin || effect.query.Transform != query.Transform {
				return nil, fmt.Errorf("deferred Kbuild content query %q has conflicting selected source origins", query.Token)
			}
			effect.consumers[consumer] = true
			queryTokensByAction[consumer] = append(queryTokensByAction[consumer], query.Token)
		}
	}

	// Reduce the complete selected-target walk to edges between materialized
	// actions. Non-materialized phony and recursive dispatch nodes remain
	// transparent source-defined control flow.
	nativeActionDependencies := map[actionIdentity]map[actionIdentity]bool{}
	unruledActionDependencies := map[actionIdentity]bool{}
	for item := range materialized {
		consumer := actionIdentity{profile: item.profile, target: item.target}
		seen := map[workItem]bool{}
		pending := append([]workItem(nil), dependencies[item]...)
		for len(pending) != 0 {
			candidate := pending[0]
			pending = pending[1:]
			if seen[candidate] {
				continue
			}
			seen[candidate] = true
			candidateIdentity := actionIdentity{profile: candidate.profile, target: candidate.target}
			if evaluations[candidate.profile][candidate.target].unruled {
				unruledActionDependencies[consumer] = true
				continue
			}
			if materialized[candidate] && candidateIdentity != consumer {
				if candidateIdentity.target == consumer.target &&
					indexes[candidate.profile].initialVisibleArtifactOwnedBy(
						consumer.target, profiles[consumer.profile].Name, consumer.target,
					) {
					// This recursive invocation started after the current action
					// materialized the same pathname. Its terminal is a successor
					// overwrite, not a prerequisite of the earlier writer.
					continue
				}
				if nativeActionDependencies[consumer] == nil {
					nativeActionDependencies[consumer] = map[actionIdentity]bool{}
				}
				nativeActionDependencies[consumer][candidateIdentity] = true
				continue
			}
			pending = append(pending, dependencies[candidate]...)
		}
	}
	actionsByProfile := make([][]actionIdentity, len(profiles))
	ownerActionsByTarget := map[string][]actionIdentity{}
	for identity := range selectedLifecycle {
		actionsByProfile[identity.profile] = append(actionsByProfile[identity.profile], identity)
		ownerActionsByTarget[identity.target] = append(ownerActionsByTarget[identity.target], identity)
	}
	// Recursive Make invocation order is an execution edge even when no native
	// prerequisite path joins the two child graphs. Keep these weak edges
	// available while resolving compiler includes as well as while propagating
	// physical stages: an action may consume a generated include only when its
	// selected producer is not known to execute in a later invocation.
	invocationActionDependencies := map[actionIdentity]map[actionIdentity]bool{}
	for profileIndex := range profiles {
		consumers := actionsByProfile[profileIndex]
		if len(consumers) == 0 || len(profiles[profileIndex].InvocationPredecessors) == 0 {
			continue
		}
		for _, predecessorName := range profiles[profileIndex].InvocationPredecessors {
			predecessorIndex, exists := profileByName[predecessorName]
			if !exists {
				return nil, fmt.Errorf("Kbuild invocation %q references missing execution predecessor %q", profiles[profileIndex].Name, predecessorName)
			}
			for _, consumer := range consumers {
				if invocationActionDependencies[consumer] == nil {
					invocationActionDependencies[consumer] = map[actionIdentity]bool{}
				}
				for _, predecessor := range actionsByProfile[predecessorIndex] {
					if predecessor != consumer {
						invocationActionDependencies[consumer][predecessor] = true
					}
				}
			}
		}
	}
	unionGroupedActionDependencies := func(byAction map[actionIdentity]map[actionIdentity]bool) {
		for group, members := range groupedMembers {
			trigger := groupedTriggers[group]
			union := map[actionIdentity]bool{}
			for member := range members {
				if _, selected := selectedLifecycle[member]; !selected {
					continue
				}
				for dependency := range byAction[member] {
					if physicalAction(dependency) != trigger {
						union[dependency] = true
					}
				}
			}
			for member := range members {
				if _, selected := selectedLifecycle[member]; selected {
					// One shared map is intentional: generated include and object-tree
					// discovery below may add another exact edge through any logical
					// output, but every such edge belongs to the same physical action.
					byAction[member] = union
				}
			}
		}
	}
	unionGroupedActionDependencies(nativeActionDependencies)
	unionGroupedActionDependencies(invocationActionDependencies)
	for _, members := range groupedMembers {
		groupUnruled := false
		for member := range members {
			groupUnruled = groupUnruled || unruledActionDependencies[member]
		}
		if groupUnruled {
			for member := range members {
				if _, selected := selectedLifecycle[member]; selected {
					unruledActionDependencies[member] = true
				}
			}
		}
	}

	// The same content-addressed generated target can be reached through two
	// independently evaluated recursive Make invocations. Path equality alone
	// is not ownership: compare the complete selected physical action contract,
	// including its recursively content-addressed native inputs, before treating
	// those logical producers as interchangeable. Invocation-predecessor edges
	// are deliberately absent: they constrain source order and scope propagation,
	// but are not ActionRecipe inputs. Lifecycle and configured action roles are
	// part of the local contract, so differently scoped executions do not
	// collapse merely because their shell text happens to match. Ordered
	// same-path versions are resolved separately before this comparison. All profiles
	// in this selection walk share the function's one declared sourceRoot/source
	// namespace; ProfilePath and every logical source prerequisite below are
	// identities inside that namespace, never planner-machine filesystem paths.
	type selectedProducerQueryIdentity struct {
		Command      string
		Target       string
		Transform    string
		Generation   uint64
		CommandShell string
		Environment  map[string]string
		OriginTarget string
		ActionRoles  []kconfig.KbuildActionRoleRef
		ObjectTree   kconfig.CompactKbuildObjectTreeObservation
	}
	type selectedProducerPlanIdentity struct {
		Target                    string
		GroupedOutputs            []string
		Lifecycle                 string
		ProfilePath               string
		ProfileDirectory          string
		InvocationLocation        kconfig.CompactKbuildInvocationLocation
		Stem                      string
		NormalPrerequisites       []string
		OrderOnlyPrerequisites    []string
		SourcePrerequisites       []string
		GeneratedPrerequisites    []string
		NativeDependencies        []string
		CommandTexts              []string
		ExportedEnvironment       map[string]string
		ActionRoles               []kconfig.KbuildActionRoleRef
		DeferredQueries           []selectedProducerQueryIdentity
		InitialVisibleArtifacts   []kconfig.CompactKbuildVisibleArtifact
		ObjectTreeSnapshot        bool
		ObjectTreeAllVisible      bool
		ObjectTreeReferences      []string
		DirectObjectTreeAll       bool
		DirectObjectTreeRefs      []string
		ObjectTreePrograms        []string
		ProducesNonIncludeOutput  bool
		IncludeSearches           []string
		SourceProjection          string
		ProjectionAmbiguous       bool
		LiteralProjection         string
		LiteralProjectionSet      bool
		LiteralProjectionAmbig    bool
		GeneratedContent          string
		GeneratedContentSet       bool
		GeneratedContentPending   bool
		GeneratedContentKnown     bool
		EvaluatedCommandTextCount int
	}
	includeSearchIdentity := func(search kbuildIncludeSearchPlan) string {
		parts := []string{}
		for _, source := range search.sources {
			parts = append(parts, "source\x00"+source)
		}
		for _, source := range search.generatedSources {
			parts = append(parts, "generated\x00"+source)
		}
		for _, directory := range search.directories {
			parts = append(parts, fmt.Sprintf(
				"directory\x00%s\x00%t\x00%t\x00%s\x00%t",
				directory.path, directory.source, directory.searchAfterDirect,
				directory.searchName, directory.quoteOnly,
			))
		}
		for _, forced := range search.forced {
			parts = append(parts, fmt.Sprintf(
				"forced\x00%s\x00%t\x00%t\x00%s",
				forced.path, forced.source, forced.searchAfterDirect, forced.searchName,
			))
		}
		return strings.Join(parts, "\x01")
	}
	producerPlanIdentities := map[actionIdentity]string{}
	producerPlanPayloads := map[actionIdentity][]byte{}
	producerPlanIdentityVisiting := map[actionIdentity]bool{}
	var producerPlanIdentity func(actionIdentity) (string, bool, error)
	producerPlanIdentity = func(identity actionIdentity) (string, bool, error) {
		identity = physicalAction(identity)
		if digest, ok := producerPlanIdentities[identity]; ok {
			return digest, true, nil
		}
		if producerPlanIdentityVisiting[identity] {
			// A cyclic selected action graph is rejected later; it cannot prove
			// that two ambiguous producers are interchangeable here.
			return "", false, nil
		}
		producerPlanIdentityVisiting[identity] = true
		defer delete(producerPlanIdentityVisiting, identity)

		profile := profiles[identity.profile]
		evaluation := evaluationForAction[identity]
		if len(evaluation.deferredQueries) != 0 || slices.ContainsFunc(evaluation.includeSearches, func(search kbuildIncludeSearchPlan) bool {
			return len(search.generatedSources) != 0 ||
				slices.ContainsFunc(search.directories, func(directory kbuildIncludeSearchDirectory) bool {
					return !directory.source
				}) ||
				slices.ContainsFunc(search.forced, func(forced kbuildIncludeTreePath) bool {
					return !forced.source
				})
		}) {
			// Deferred-query and opaque include-root dependencies are completed
			// after this ownership check reaches its fixed point. Their current
			// edge set is therefore not a complete plan identity yet. Conversely,
			// an identity admitted below has no later weak opaque edge of its own,
			// so caching its recursively hashed native dependency set is safe.
			return "", false, nil
		}
		location, locationSet := kconfig.CompactKbuildProfileInvocationLocation(profile)
		if !locationSet {
			return "", false, nil
		}
		environmentInjections, err := kconfig.CompactKbuildTargetEvaluationInjectionsForMakeTarget(
			profile, identity.target, evaluation.lookupTarget, evaluation.automaticTarget, evaluation.stem,
			evaluation.normalMakeTargets, evaluation.orderOnlyMakeTargets,
		)
		if err != nil {
			return "", false, fmt.Errorf("derive selected producer environment paths: %w", err)
		}
		environmentInjections["Q"] = ""
		exportedEnvironment, err := kconfig.EvaluateCompactKbuildTargetEnvironmentForMakeTarget(
			profile, identity.target, evaluation.lookupTarget, evaluation.automaticTarget, evaluation.stem,
			evaluation.normalMakeTargets, evaluation.orderOnlyMakeTargets, environmentInjections,
		)
		if err != nil {
			return "", false, fmt.Errorf("evaluate selected producer exported environment: %w", err)
		}
		dependencyIdentities := []string{}
		for dependency := range nativeActionDependencies[identity] {
			digest, available, err := producerPlanIdentity(dependency)
			if err != nil {
				return "", false, err
			}
			if !available {
				return "", false, nil
			}
			dependencyIdentities = append(dependencyIdentities, digest)
		}
		sort.Strings(dependencyIdentities)

		groupedOutputs := []string{identity.target}
		if group, grouped := groupedAction[identity]; grouped {
			groupedOutputs = groupedOutputs[:0]
			for member := range groupedMembers[group] {
				groupedOutputs = append(groupedOutputs, member.target)
			}
			sort.Strings(groupedOutputs)
		}
		queryIdentities := make([]selectedProducerQueryIdentity, 0, len(evaluation.deferredQueries))
		for _, query := range evaluation.deferredQueries {
			queryIdentities = append(queryIdentities, selectedProducerQueryIdentity{
				Command: query.Command, Target: query.Target, Transform: query.Transform,
				Generation: query.Generation, CommandShell: query.CommandShell,
				Environment: query.Environment, OriginTarget: query.Origin.Target,
				ActionRoles: query.ActionRoles, ObjectTree: query.ObjectTree,
			})
		}
		includeSearches := make([]string, 0, len(evaluation.includeSearches))
		for _, search := range evaluation.includeSearches {
			includeSearches = append(includeSearches, includeSearchIdentity(search))
		}
		initialArtifacts := []kconfig.CompactKbuildVisibleArtifact(nil)
		if evaluation.objectTreeSnapshot {
			references := sortedUniquePaths(evaluation.objectTreeReferences)
			initialArtifacts = indexes[identity.profile].matchingInitialVisibleArtifacts(
				references, evaluation.objectTreeAllVisible,
			)
			initialArtifacts = canonicalKbuildVisibleArtifacts(initialArtifacts)
		}
		payload := selectedProducerPlanIdentity{
			Target: identity.target, GroupedOutputs: groupedOutputs,
			Lifecycle:        selectedLifecycle[identity],
			ProfilePath:      profile.Path,
			ProfileDirectory: profile.Directory, InvocationLocation: location,
			Stem:                   evaluation.stem,
			NormalPrerequisites:    append([]string(nil), evaluation.normalPrerequisites...),
			OrderOnlyPrerequisites: append([]string(nil), evaluation.orderOnly...),
			SourcePrerequisites:    append([]string(nil), evaluation.sourcePrerequisites...),
			GeneratedPrerequisites: append([]string(nil), evaluation.prerequisites...),
			NativeDependencies:     dependencyIdentities,
			CommandTexts:           append([]string(nil), evaluation.commandTexts...),
			ExportedEnvironment:    exportedEnvironment,
			ActionRoles:            append([]kconfig.KbuildActionRoleRef(nil), evaluation.actionRoles...),
			DeferredQueries:        queryIdentities, InitialVisibleArtifacts: initialArtifacts,
			ObjectTreeSnapshot:        evaluation.objectTreeSnapshot,
			ObjectTreeAllVisible:      evaluation.objectTreeAllVisible,
			ObjectTreeReferences:      sortedUniquePaths(evaluation.objectTreeReferences),
			DirectObjectTreeAll:       evaluation.directObjectTreeAllVisible,
			DirectObjectTreeRefs:      sortedUniquePaths(evaluation.directObjectTreeReferences),
			ObjectTreePrograms:        sortedUniquePaths(evaluation.objectTreePrograms),
			ProducesNonIncludeOutput:  evaluation.producesNonIncludeOutput,
			IncludeSearches:           includeSearches,
			SourceProjection:          evaluation.sourceProjection,
			ProjectionAmbiguous:       evaluation.projectionAmbiguous,
			LiteralProjection:         evaluation.literalProjection,
			LiteralProjectionSet:      evaluation.literalProjectionSet,
			LiteralProjectionAmbig:    evaluation.literalProjectionAmbiguous,
			GeneratedContent:          evaluation.generatedContent,
			GeneratedContentSet:       evaluation.generatedContentSet,
			GeneratedContentPending:   evaluation.generatedContentPending,
			GeneratedContentKnown:     evaluation.generatedContentRecognized,
			EvaluatedCommandTextCount: evaluation.evaluatedCommandTexts,
		}
		// The selected action contract still carries authenticated toolset-path
		// capabilities so its eventual recipe can verify them.  Its structural
		// identity must not carry the workload-local MAC, including capabilities
		// nested in command text, exported variables, or generated content.
		data, err := marshalCanonicalKbuildToolsetPathCapabilityIdentity(payload)
		if err != nil {
			return "", false, fmt.Errorf("encode selected producer plan identity: %w", err)
		}
		digest := sha256.Sum256(data)
		identityDigest := hex.EncodeToString(digest[:])
		producerPlanIdentities[identity] = identityDigest
		producerPlanPayloads[identity] = data
		return identityDigest, true, nil
	}
	dedupeEquivalentSelectedProducers := func(candidates []actionIdentity) ([]actionIdentity, string, error) {
		if len(candidates) < 2 {
			return candidates, "", nil
		}
		firstIdentity, available, err := producerPlanIdentity(candidates[0])
		if err != nil || !available {
			return candidates, "", err
		}
		for _, candidate := range candidates[1:] {
			identity, candidateAvailable, identityErr := producerPlanIdentity(candidate)
			if identityErr != nil {
				return nil, "", identityErr
			}
			if !candidateAvailable || identity != firstIdentity {
				difference := "physical action contract unavailable"
				if candidateAvailable {
					difference = firstCanonicalJSONDifference(
						producerPlanPayloads[physicalAction(candidates[0])],
						producerPlanPayloads[physicalAction(candidate)],
					)
				}
				return candidates, fmt.Sprintf(
					"%s:%s versus %s:%s: %s",
					profiles[candidates[0].profile].Name, candidates[0].target,
					profiles[candidate.profile].Name, candidate.target, difference,
				), nil
			}
		}
		// selectedFeedCandidates orders by the stable profile name and target.
		// Keep that canonical representative, never map iteration or discovery
		// order, so equivalent incomparable invocations produce a stable selection.
		return candidates[:1], "", nil
	}
	type invocationDescentKey struct {
		parentProfile     int
		target            string
		descendantProfile int
	}
	invocationDescentCache := map[invocationDescentKey]bool{}
	invocationDescendsTo := func(parentProfile int, target string, descendantProfile int) bool {
		key := invocationDescentKey{
			parentProfile: parentProfile, target: target, descendantProfile: descendantProfile,
		}
		if descends, cached := invocationDescentCache[key]; cached {
			return descends
		}
		if parentProfile == descendantProfile {
			invocationDescentCache[key] = false
			return false
		}
		if stats != nil {
			stats.InvocationDescentWalks++
		}
		pending := []int{parentProfile}
		seen := map[int]bool{}
		descends := false
		for len(pending) != 0 {
			profileIndex := pending[0]
			pending = pending[1:]
			if seen[profileIndex] {
				continue
			}
			seen[profileIndex] = true
			for _, dependency := range profiles[profileIndex].TargetInvocationDependencies {
				if kconfig.CanonicalKbuildGraphTarget(dependency.Target) != target {
					continue
				}
				child, exists := profileByName[dependency.Profile]
				if !exists {
					continue
				}
				if child == descendantProfile {
					descends = true
					pending = nil
					break
				}
				pending = append(pending, child)
			}
		}
		invocationDescentCache[key] = descends
		return descends
	}
	visibleArtifactOwnerCache := map[kconfig.CompactKbuildVisibleArtifact]actionIdentity{}
	resolveVisibleArtifactOwner := func(consumer actionIdentity, artifact kconfig.CompactKbuildVisibleArtifact) (actionIdentity, error) {
		if owner, cached := visibleArtifactOwnerCache[artifact]; cached {
			return owner, nil
		}
		originProfile, exists := profileByName[artifact.Profile]
		if !exists {
			return actionIdentity{}, fmt.Errorf(
				"Kbuild invocation %q visible artifact %q references missing provenance profile %q",
				profiles[consumer.profile].Name, artifact.Path, artifact.Profile,
			)
		}
		producerTarget := kconfig.CanonicalKbuildGraphTarget(artifact.Target)
		if producerTarget == "" || producerTarget != artifact.Target {
			return actionIdentity{}, fmt.Errorf(
				"Kbuild invocation %q visible artifact %q has invalid provenance target %q in profile %q",
				profiles[consumer.profile].Name, artifact.Path, artifact.Target, artifact.Profile,
			)
		}
		if canonical := kconfig.CanonicalKbuildGraphTarget(artifact.Path); canonical == "" || canonical != artifact.Path {
			return actionIdentity{}, fmt.Errorf(
				"Kbuild invocation %q has invalid visible artifact path %q (canonical %q)",
				profiles[consumer.profile].Name, artifact.Path, canonical,
			)
		}
		exact := actionIdentity{profile: originProfile, target: producerTarget}
		if _, selected := selectedLifecycle[exact]; selected {
			hasDescendant := false
			materializesBeforeDescendant := false
			for _, candidate := range ownerActionsByTarget[producerTarget] {
				if candidate == exact || !invocationDescendsTo(originProfile, producerTarget, candidate.profile) {
					continue
				}
				hasDescendant = true
				if indexes[candidate.profile].initialVisibleArtifactOwnedBy(
					artifact.Path, artifact.Profile, artifact.Target,
				) {
					materializesBeforeDescendant = true
					break
				}
			}
			if !hasDescendant || materializesBeforeDescendant {
				// A visible-artifact record names one exact source-ordered
				// version. Forward through a same-target recursive descendant only
				// when no descendant started from this materialized version.
				visibleArtifactOwnerCache[artifact] = exact
				return exact, nil
			}
		}
		candidates := []actionIdentity{}
		ownerCandidates := ownerActionsByTarget[producerTarget]
		if stats != nil {
			stats.VisibleOwnerCandidateChecks += len(ownerCandidates)
		}
		for _, candidate := range ownerCandidates {
			if candidate.profile == originProfile || invocationDescendsTo(originProfile, producerTarget, candidate.profile) {
				candidates = append(candidates, candidate)
			}
		}
		// A parent rule containing recursive $(MAKE) forwards ownership to the
		// deepest selected descendant. This is the same source-proven rule used
		// by compactKbuildSelectionGraph; an unrelated same-path action is never
		// considered a candidate.
		terminal := []actionIdentity{}
		for _, candidate := range candidates {
			forwarded := false
			for _, other := range candidates {
				if candidate != other && invocationDescendsTo(candidate.profile, producerTarget, other.profile) {
					forwarded = true
					break
				}
			}
			if !forwarded {
				terminal = append(terminal, candidate)
			}
		}
		if len(terminal) != 1 {
			labels := make([]string, 0, len(terminal))
			for _, candidate := range terminal {
				labels = append(labels, profiles[candidate.profile].Name+":"+candidate.target)
			}
			sort.Strings(labels)
			return actionIdentity{}, fmt.Errorf(
				"Kbuild invocation %q action %q visible artifact %q from %s:%s resolves to %d terminal selected owners %q",
				profiles[consumer.profile].Name, consumer.target, artifact.Path, artifact.Profile, artifact.Target,
				len(terminal), labels,
			)
		}
		visibleArtifactOwnerCache[artifact] = terminal[0]
		return terminal[0], nil
	}
	initialObjectTreeArtifactsByAction := map[actionIdentity][]kconfig.CompactKbuildVisibleArtifact{}
	generatedObjectTreeArtifactsByAction := map[actionIdentity][]kconfig.CompactKbuildVisibleArtifact{}
	initialObjectTreeArtifactSetByAction := map[actionIdentity]map[kconfig.CompactKbuildVisibleArtifact]bool{}
	generatedObjectTreeArtifactSetByAction := map[actionIdentity]map[kconfig.CompactKbuildVisibleArtifact]bool{}
	generatedObjectTreeReplacementPathsByAction := map[actionIdentity]map[string]bool{}
	// An opaque compiler include root can observe any already-available header,
	// but it is not a Make dependency on every selected header that happens to
	// precede the command. Keep those candidate inputs for same/earlier-stage
	// replay without letting an unrelated host header pull a target prerequisite
	// across a physical scope boundary. Exact includes and ordinary
	// prerequisites remain strong: H -> T -> H uses prehost/bootstrap/host, and
	// one further alternation still fails closed.
	type objectTreeArtifactEdge struct {
		consumer actionIdentity
		producer actionIdentity
	}
	weakObjectTreeEdges := map[objectTreeArtifactEdge]bool{}
	weakObjectTreeArtifacts := map[actionIdentity]map[kconfig.CompactKbuildVisibleArtifact]actionIdentity{}
	physicalObjectTreeEdge := func(consumer, producer actionIdentity) objectTreeArtifactEdge {
		return objectTreeArtifactEdge{
			consumer: physicalAction(consumer),
			producer: physicalAction(producer),
		}
	}
	newSelectionReachabilityRound := func(
		dependencies map[actionIdentity]map[actionIdentity]bool,
	) *graphReachabilityMemo[actionIdentity] {
		var onWalk func()
		var onHit func()
		if stats != nil {
			onWalk = func() { stats.ReachabilityWalks++ }
			onHit = func() { stats.ReachabilityCacheHits++ }
		}
		return newGraphReachabilityMemo(
			dependencies,
			invocationActionDependencies,
			onWalk,
			onHit,
		)
	}
	// This memo is valid only until bindObjectTreeArtifact adds a dependency.
	// Prospective command-owner graphs below receive their own per-round memo.
	reachabilityRound := newSelectionReachabilityRound(nativeActionDependencies)
	isWeakObjectTreeEdge := func(consumer, producer actionIdentity) bool {
		return weakObjectTreeEdges[physicalObjectTreeEdge(consumer, producer)]
	}
	bindObjectTreeArtifact := func(consumer, producer actionIdentity, artifact kconfig.CompactKbuildVisibleArtifact, weak bool) error {
		if producer == consumer {
			return fmt.Errorf(
				"Kbuild invocation %q action %q consumes itself through generated artifact %q",
				profiles[consumer.profile].Name, consumer.target, artifact.Path,
			)
		}
		existingDependency := nativeActionDependencies[consumer][producer]
		existingStrongEdge := existingDependency && !isWeakObjectTreeEdge(consumer, producer)
		if nativeActionDependencies[consumer] == nil {
			nativeActionDependencies[consumer] = map[actionIdentity]bool{}
		}
		nativeActionDependencies[consumer][producer] = true
		if !existingDependency {
			// Never retain a transitive-closure result across a graph mutation.
			reachabilityRound = newSelectionReachabilityRound(nativeActionDependencies)
		}
		edge := physicalObjectTreeEdge(consumer, producer)
		physicalConsumer := edge.consumer
		if weak && !existingStrongEdge {
			weakObjectTreeEdges[edge] = true
			if weakObjectTreeArtifacts[physicalConsumer] == nil {
				weakObjectTreeArtifacts[physicalConsumer] = map[kconfig.CompactKbuildVisibleArtifact]actionIdentity{}
			}
			weakObjectTreeArtifacts[physicalConsumer][artifact] = edge.producer
		} else {
			delete(weakObjectTreeEdges, edge)
			delete(weakObjectTreeArtifacts[physicalConsumer], artifact)
		}
		return nil
	}
	appendObjectTreeArtifact := func(
		byAction map[actionIdentity][]kconfig.CompactKbuildVisibleArtifact,
		seenByAction map[actionIdentity]map[kconfig.CompactKbuildVisibleArtifact]bool,
		consumer actionIdentity,
		artifact kconfig.CompactKbuildVisibleArtifact,
	) {
		seen := seenByAction[consumer]
		if seen == nil {
			seen = map[kconfig.CompactKbuildVisibleArtifact]bool{}
			seenByAction[consumer] = seen
		}
		if seen[artifact] {
			return
		}
		seen[artifact] = true
		byAction[consumer] = append(byAction[consumer], artifact)
	}

	// A literal source reference may name an output generated inside an earlier
	// recursive invocation but omitted from that invocation's terminal frontier.
	// This includes compiler includes (x86 capflags.c is one such shape) and
	// object-tree operands hidden inside deferred Make-shell queries. Every bind
	// still requires an exact, unique selected producer. The opaque-include pass
	// below may enumerate already-selected target paths under compiler-derived
	// roots, but it never infers an output which Kbuild did not select.
	selectedActionsByTarget := map[string][]actionIdentity{}
	for identity := range selectedLifecycle {
		// Phony/control and directory goals execute real recipes but do not
		// publish a file named by the target. They remain selected actions for
		// ordering and scope propagation, while exact/opaque generated-file
		// discovery considers only source-declared file targets.
		if indexes[identity.profile].targetIsPhony(profiles[identity.profile], identity.target) ||
			identity.target == "." || strings.HasSuffix(identity.target, "/") {
			continue
		}
		selectedActionsByTarget[identity.target] = append(selectedActionsByTarget[identity.target], identity)
	}
	producerCanFeedUsing := func(
		reachability *graphReachabilityMemo[actionIdentity],
		consumer, producer actionIdentity,
	) bool {
		if producer == consumer {
			return false
		}
		// The preparation closure is a source-defined prefix of the target
		// closure. An action selected only after that prefix cannot provide a
		// generated file to a preparation recipe. Exact preparation
		// prerequisites are reached through both roots and collapse to the prep
		// lifecycle above, regardless of their eventual compiler scope.
		if selectedLifecycle[consumer] == "prep" && selectedLifecycle[producer] != "prep" {
			return false
		}
		return !reachability.reaches(producer, consumer)
	}
	producerCanFeed := func(consumer, producer actionIdentity) bool {
		return producerCanFeedUsing(reachabilityRound, consumer, producer)
	}
	sortSelectedActionCandidates := func(candidates []actionIdentity) {
		sort.Slice(candidates, func(i, j int) bool {
			leftName := profiles[candidates[i].profile].Name
			rightName := profiles[candidates[j].profile].Name
			if leftName != rightName {
				return leftName < rightName
			}
			if candidates[i].target != candidates[j].target {
				return candidates[i].target < candidates[j].target
			}
			return candidates[i].profile < candidates[j].profile
		})
	}
	selectedFeedCandidates := func(consumer actionIdentity, reference string) []actionIdentity {
		candidates := append([]actionIdentity(nil), selectedActionsByTarget[reference]...)
		candidates = slices.DeleteFunc(candidates, func(candidate actionIdentity) bool {
			return !producerCanFeed(consumer, candidate)
		})
		sortSelectedActionCandidates(candidates)
		return candidates
	}
	latestSelectedProducerVersionsUsing := func(
		candidates []actionIdentity,
		predecessors func(actionIdentity) map[actionIdentity]bool,
	) []actionIdentity {
		if len(candidates) < 2 {
			return candidates
		}
		latest := make([]actionIdentity, 0, len(candidates))
		precedes := func(earlier, later actionIdentity) bool {
			// The root prepare closure is a source-defined prefix of the target
			// closure. If both closures select a writer for the same pathname,
			// the target-lifecycle action is the observable later version.
			if selectedLifecycle[earlier] == "prep" && selectedLifecycle[later] != "prep" {
				return true
			}
			if predecessors(later)[earlier] {
				return true
			}
			// A recursive invocation's initial frontier is the exact completed
			// object-tree version it starts from. When it contains another
			// candidate for this same pathname, the action selected in the later
			// profile overwrites that earlier version even if Make did not expose
			// the process ordering as an invocation dependency.
			return indexes[later.profile].initialVisibleArtifactOwnedBy(
				later.target, profiles[earlier.profile].Name, earlier.target,
			)
		}
		for _, candidate := range candidates {
			superseded := false
			for _, other := range candidates {
				if candidate != other && precedes(candidate, other) {
					superseded = true
					break
				}
			}
			if !superseded {
				latest = append(latest, candidate)
			}
		}
		return latest
	}
	latestSelectedProducerVersions := func(candidates []actionIdentity) []actionIdentity {
		predecessors := func(consumer actionIdentity) map[actionIdentity]bool {
			return reachabilityRound.allPredecessors(consumer)
		}
		return latestSelectedProducerVersionsUsing(candidates, predecessors)
	}
	// Command-head provenance identifies executable producer identities, while
	// configured compiler mode proves primary object/link outputs cannot be
	// preprocessor includes. Prefer the selected command's native prerequisite
	// owner; a unique acyclic selected owner is sufficient when Make omitted the
	// program from ordinary prerequisites but the evaluated command names it
	// exactly.
	selectedNonIncludeActions := map[actionIdentity]bool{}
	for consumer, evaluation := range evaluationForAction {
		if evaluation.producesNonIncludeOutput {
			selectedNonIncludeActions[consumer] = true
		}
		for _, program := range sortedUniquePaths(evaluation.objectTreePrograms) {
			candidates := selectedFeedCandidates(consumer, program)
			direct := slices.DeleteFunc(append([]actionIdentity(nil), candidates...), func(candidate actionIdentity) bool {
				return !nativeActionDependencies[consumer][candidate]
			})
			if len(direct) == 1 {
				selectedNonIncludeActions[direct[0]] = true
			} else if len(direct) == 0 && len(candidates) == 1 {
				selectedNonIncludeActions[candidates[0]] = true
			}
		}
	}
	bindGeneratedArtifactCandidates := func(
		consumer actionIdentity,
		reference, origin string,
		candidates []actionIdentity,
		weak bool,
	) (bool, error) {
		if len(candidates) == 0 {
			return false, nil
		}
		// A later recursive Make invocation can overwrite the same pathname.
		// Once both versions precede this consumer, only the unique maximal
		// source-ordered producer is observable. Incomparable candidates retain
		// the fail-closed contract comparison below.
		candidates = latestSelectedProducerVersions(candidates)
		mismatch := ""
		if weak {
			var dedupeErr error
			candidates, mismatch, dedupeErr = dedupeEquivalentSelectedProducers(candidates)
			if dedupeErr != nil {
				return false, fmt.Errorf("compare selected producers for %s %q: %w", origin, reference, dedupeErr)
			}
		}
		if len(candidates) != 1 {
			labels := make([]string, 0, len(candidates))
			for _, candidate := range candidates {
				labels = append(labels, profiles[candidate.profile].Name+":"+candidate.target)
			}
			sort.Strings(labels)
			detail := ""
			if mismatch != "" {
				detail = "; contract mismatch: " + mismatch
			}
			return false, fmt.Errorf(
				"Kbuild invocation %q action %q %s %q resolves to %d selected producers %q%s",
				profiles[consumer.profile].Name, consumer.target, origin, reference, len(candidates), labels, detail,
			)
		}
		producer := candidates[0]
		artifact := kconfig.CompactKbuildVisibleArtifact{
			Path: reference, Profile: profiles[producer.profile].Name, Target: producer.target,
		}
		if err := bindObjectTreeArtifact(consumer, producer, artifact, weak); err != nil {
			return false, err
		}
		appendObjectTreeArtifact(
			generatedObjectTreeArtifactsByAction,
			generatedObjectTreeArtifactSetByAction,
			consumer,
			artifact,
		)
		return true, nil
	}
	bindSelectedGeneratedArtifact := func(consumer actionIdentity, reference, origin string) (bool, error) {
		return bindGeneratedArtifactCandidates(
			consumer, reference, origin, selectedFeedCandidates(consumer, reference), false,
		)
	}
	// A statically resolved object-tree command head is an exact executable
	// input, independently of compiler include/search-root observations. Decide
	// every program owner against a prospective graph first, then apply the
	// converged edges as one batch. Mutating nativeActionDependencies while
	// ranging evaluationForAction made ownership depend on Go map order; a
	// downstream consumer can also learn that a replacement precedes it only
	// after an upstream consumer's inferred program edge is present.
	type objectTreeProgramObservation struct {
		consumer actionIdentity
		program  string
	}
	type objectTreeProgramDecision struct {
		producer        actionIdentity
		artifact        kconfig.CompactKbuildVisibleArtifact
		bound           bool
		initial         bool
		replacesInitial bool
	}
	programObservations := []objectTreeProgramObservation{}
	for consumer, evaluation := range evaluationForAction {
		for _, program := range sortedUniquePaths(evaluation.objectTreePrograms) {
			programObservations = append(programObservations, objectTreeProgramObservation{
				consumer: consumer,
				program:  program,
			})
		}
	}
	sort.Slice(programObservations, func(i, j int) bool {
		left := programObservations[i]
		right := programObservations[j]
		leftName := profiles[left.consumer.profile].Name
		rightName := profiles[right.consumer.profile].Name
		if leftName != rightName {
			return leftName < rightName
		}
		if left.consumer.target != right.consumer.target {
			return left.consumer.target < right.consumer.target
		}
		return left.program < right.program
	})
	cloneNativeDependencies := func(source map[actionIdentity]map[actionIdentity]bool) map[actionIdentity]map[actionIdentity]bool {
		result := make(map[actionIdentity]map[actionIdentity]bool, len(source))
		for consumer, dependencies := range source {
			result[consumer] = maps.Clone(dependencies)
		}
		return result
	}
	ambiguousProgramOwner := func(observation objectTreeProgramObservation, candidates []actionIdentity) error {
		labels := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			labels = append(labels, profiles[candidate.profile].Name+":"+candidate.target)
		}
		sort.Strings(labels)
		return fmt.Errorf(
			"Kbuild invocation %q action %q object-tree command program %q resolves to %d selected producers %q",
			profiles[observation.consumer.profile].Name, observation.consumer.target,
			observation.program, len(candidates), labels,
		)
	}
	resolveObjectTreeProgramOwners := func() error {
		baseNativeDependencies := cloneNativeDependencies(nativeActionDependencies)
		programDecisions := map[objectTreeProgramObservation]objectTreeProgramDecision{}
		converged := false
		for round := 0; round <= len(programObservations)+1; round++ {
			prospectiveNativeDependencies := cloneNativeDependencies(baseNativeDependencies)
			for observation, decision := range programDecisions {
				if !decision.bound {
					continue
				}
				if prospectiveNativeDependencies[observation.consumer] == nil {
					prospectiveNativeDependencies[observation.consumer] = map[actionIdentity]bool{}
				}
				prospectiveNativeDependencies[observation.consumer][decision.producer] = true
			}
			prospectiveReachability := newSelectionReachabilityRound(prospectiveNativeDependencies)
			prospectivePredecessors := func(consumer actionIdentity) map[actionIdentity]bool {
				return prospectiveReachability.allPredecessors(consumer)
			}
			prospectiveProducerCanFeed := func(consumer, producer actionIdentity) bool {
				return producerCanFeedUsing(prospectiveReachability, consumer, producer)
			}
			nextDecisions := make(map[objectTreeProgramObservation]objectTreeProgramDecision, len(programObservations))
			ambiguousDecisions := make(map[objectTreeProgramObservation][]actionIdentity)
			for _, observation := range programObservations {
				consumer := observation.consumer
				program := observation.program
				visibleIndex := indexes[consumer.profile]
				if initialArtifact, visible := visibleIndex.initialVisibleArtifact(program); visible {
					initialOwner, err := resolveVisibleArtifactOwner(consumer, initialArtifact)
					if err != nil {
						return err
					}
					decision := objectTreeProgramDecision{
						producer: initialOwner, artifact: initialArtifact, bound: true, initial: true,
					}
					hasAlternative := slices.ContainsFunc(selectedActionsByTarget[program], func(candidate actionIdentity) bool {
						return candidate != initialOwner
					})
					if !hasAlternative {
						nextDecisions[observation] = decision
						continue
					}
					predecessors := prospectivePredecessors(consumer)
					candidates := []actionIdentity{initialOwner}
					for _, candidate := range selectedActionsByTarget[program] {
						// A concrete invocation-start version remains authoritative
						// over unordered same-path selections. Only a writer which the
						// prospective selected graph proves runs first can overwrite it.
						if candidate == initialOwner || !predecessors[candidate] ||
							selectedLifecycle[consumer] == "prep" && selectedLifecycle[candidate] != "prep" {
							continue
						}
						candidates = append(candidates, candidate)
					}
					sortSelectedActionCandidates(candidates)
					latest := latestSelectedProducerVersionsUsing(candidates, prospectivePredecessors)
					if len(latest) != 1 {
						nextDecisions[observation] = objectTreeProgramDecision{}
						ambiguousDecisions[observation] = latest
						continue
					}
					if latest[0] != initialOwner {
						decision = objectTreeProgramDecision{
							producer: latest[0], bound: true, replacesInitial: true,
							artifact: kconfig.CompactKbuildVisibleArtifact{
								Path: program, Profile: profiles[latest[0].profile].Name, Target: latest[0].target,
							},
						}
					}
					nextDecisions[observation] = decision
					continue
				}
				candidates := append([]actionIdentity(nil), selectedActionsByTarget[program]...)
				candidates = slices.DeleteFunc(candidates, func(candidate actionIdentity) bool {
					return !prospectiveProducerCanFeed(consumer, candidate)
				})
				sortSelectedActionCandidates(candidates)
				candidates = latestSelectedProducerVersionsUsing(candidates, prospectivePredecessors)
				if len(candidates) == 0 {
					nextDecisions[observation] = objectTreeProgramDecision{}
					continue
				}
				if len(candidates) != 1 {
					nextDecisions[observation] = objectTreeProgramDecision{}
					ambiguousDecisions[observation] = candidates
					continue
				}
				producer := candidates[0]
				nextDecisions[observation] = objectTreeProgramDecision{
					producer: producer, bound: true,
					artifact: kconfig.CompactKbuildVisibleArtifact{
						Path: program, Profile: profiles[producer.profile].Name, Target: producer.target,
					},
				}
			}
			if maps.Equal(programDecisions, nextDecisions) {
				// An observation can be ambiguous in an early round and become exact
				// after another observation contributes an inferred predecessor edge.
				// Fail closed only when the comparable decision state is stable and no
				// further edge can refine the ambiguity.
				for _, observation := range programObservations {
					if candidates := ambiguousDecisions[observation]; len(candidates) != 0 {
						return ambiguousProgramOwner(observation, candidates)
					}
				}
				programDecisions = nextDecisions
				converged = true
				break
			}
			programDecisions = nextDecisions
		}
		if !converged {
			return fmt.Errorf("object-tree program ownership did not converge after %d decisions", len(programObservations))
		}
		for _, observation := range programObservations {
			decision := programDecisions[observation]
			if !decision.bound {
				continue
			}
			selectedNonIncludeActions[decision.producer] = true
			if err := bindObjectTreeArtifact(observation.consumer, decision.producer, decision.artifact, false); err != nil {
				return err
			}
			byAction := generatedObjectTreeArtifactsByAction
			seenByAction := generatedObjectTreeArtifactSetByAction
			if decision.initial {
				byAction = initialObjectTreeArtifactsByAction
				seenByAction = initialObjectTreeArtifactSetByAction
			}
			appendObjectTreeArtifact(byAction, seenByAction, observation.consumer, decision.artifact)
			if decision.replacesInitial {
				if generatedObjectTreeReplacementPathsByAction[observation.consumer] == nil {
					generatedObjectTreeReplacementPathsByAction[observation.consumer] = map[string]bool{}
				}
				generatedObjectTreeReplacementPathsByAction[observation.consumer][observation.program] = true
			}
			evaluation := evaluationForAction[observation.consumer]
			evaluation.objectTreeSnapshot = true
			evaluationForAction[observation.consumer] = evaluation
		}
		return nil
	}
	// Kconfig replay supplies these projections independently of selected
	// Kbuild actions and of a recursive invocation's initial generated-file
	// frontier. They are nevertheless real object-tree files for compiler
	// include resolution. Preserve that distinct provenance here: a matching
	// include activates the working object-tree view, whose lowering rebases the
	// projection to its immutable config source without inventing a producer.
	resolvedConfigBaseline := map[string]bool{}
	for _, pathname := range kconfig.ResolvedConfigProjectionOutputs() {
		resolvedConfigBaseline[pathname] = true
	}
	// Candidate bytes may participate in include discovery before the final
	// ActionPlan exists. Apply the same closed-frontier admission here instead
	// of trusting every successful probe result. This predicate is intentionally
	// stricter than producer reachability: any selected writer for a resolved
	// config pathname keeps the candidate conservative, so later include-edge
	// discovery cannot invalidate an earlier decision.
	generatedContentAdmitted := make(map[actionIdentity]bool, len(evaluationForAction))
	for producer, evaluation := range evaluationForAction {
		if preconfiguredObjectTree || !evaluation.generatedContentRecognized || evaluation.evaluatedCommandTexts != 1 ||
			len(evaluation.prerequisites) != 0 || len(evaluation.deferredQueries) != 0 ||
			evaluation.objectTreeSnapshot || evaluation.objectTreeAllVisible ||
			len(evaluation.objectTreeReferences) != 0 || len(evaluation.objectTreePrograms) != 0 ||
			len(evaluation.includeSearches) != 0 ||
			len(initialObjectTreeArtifactsByAction[producer]) != 0 ||
			len(generatedObjectTreeArtifactsByAction[producer]) != 0 {
			continue
		}
		profile := profiles[producer.profile]
		location, located := kconfig.CompactKbuildProfileInvocationLocation(profile)
		if !located || location.Tree != kconfig.CompactKbuildInvocationObjectTree || location.Directory != "" {
			continue
		}
		_, overlay, overlayErr := kconfig.CompactKbuildProfileSourceOverlayRoot(profile)
		if overlayErr != nil {
			return nil, fmt.Errorf(
				"inspect selected target %s generated-content source overlay: %w",
				producer.target, overlayErr,
			)
		}
		if overlay {
			continue
		}
		invocationDependency := slices.ContainsFunc(
			profile.TargetInvocationDependencies,
			func(dependency kconfig.CompactKbuildInvocationDependency) bool {
				return kconfig.CanonicalKbuildGraphTarget(dependency.Target) == producer.target
			},
		)
		if invocationDependency {
			continue
		}
		configWriter := false
		for pathname := range resolvedConfigBaseline {
			if len(selectedActionsByTarget[pathname]) != 0 {
				configWriter = true
				break
			}
		}
		if configWriter {
			continue
		}
		generatedContentAdmitted[producer] = true
	}
	includeConsumers := make([]actionIdentity, 0, len(evaluationForAction))
	includeResolutions := make(map[actionIdentity]kbuildIncludeResolution, len(evaluationForAction))
	for consumer := range evaluationForAction {
		includeConsumers = append(includeConsumers, consumer)
	}
	sort.Slice(includeConsumers, func(i, j int) bool {
		if includeConsumers[i].profile != includeConsumers[j].profile {
			return includeConsumers[i].profile < includeConsumers[j].profile
		}
		return includeConsumers[i].target < includeConsumers[j].target
	})
	for _, consumer := range includeConsumers {
		evaluation := evaluationForAction[consumer]
		visibleIndex := indexes[consumer.profile]
		for _, reference := range sortedUniquePaths(evaluation.objectTreeReferences) {
			if visibleIndex.hasInitialVisibleArtifact(reference) || resolvedConfigBaseline[reference] {
				evaluation.objectTreeSnapshot = true
				continue
			}
			bound, err := bindSelectedGeneratedArtifact(consumer, reference, "object-tree reference")
			if err != nil {
				return nil, err
			}
			if bound {
				evaluation.objectTreeSnapshot = true
			}
		}
		objectExists := func(reference string) bool {
			if visibleIndex.hasInitialVisibleArtifact(reference) || resolvedConfigBaseline[reference] {
				return true
			}
			return slices.ContainsFunc(selectedActionsByTarget[reference], func(producer actionIdentity) bool {
				return producerCanFeed(consumer, producer)
			})
		}
		objectSource := func(reference string) (kbuildGeneratedIncludeProjection, bool, error) {
			if resolvedConfigBaseline[reference] {
				// The resolved-config writer is owned by this planner and emits no
				// preprocessor include directives. Model that exact content contract
				// instead of treating its files as opaque Kbuild producers and
				// widening every compiler search directory around them.
				return kbuildGeneratedIncludeProjection{
					cacheKey: "resolved-config:" + reference,
					literal:  true,
				}, true, nil
			}
			producer := actionIdentity{}
			if artifact, ok := visibleIndex.initialVisibleArtifact(reference); ok {
				var err error
				producer, err = resolveVisibleArtifactOwner(consumer, artifact)
				if err != nil {
					return kbuildGeneratedIncludeProjection{}, false, err
				}
			} else {
				candidates := append([]actionIdentity(nil), selectedActionsByTarget[reference]...)
				candidates = slices.DeleteFunc(candidates, func(candidate actionIdentity) bool {
					return !producerCanFeed(consumer, candidate)
				})
				if len(candidates) != 1 {
					return kbuildGeneratedIncludeProjection{}, false, nil
				}
				producer = candidates[0]
			}
			producerEvaluation, ok := evaluationForAction[producer]
			if !ok {
				return kbuildGeneratedIncludeProjection{}, false, nil
			}
			if !producerEvaluation.literalProjectionAmbiguous && producerEvaluation.literalProjectionSet {
				digest := kbuildGeneratedIncludeContentIdentityDigest(producerEvaluation.literalProjection)
				return kbuildGeneratedIncludeProjection{
					cacheKey: fmt.Sprintf("%d:%s:%x", producer.profile, producer.target, digest),
					contents: producerEvaluation.literalProjection,
					literal:  true,
				}, true, nil
			}
			if generatedContentAdmitted[producer] {
				if producerEvaluation.generatedContentSet {
					digest := kbuildGeneratedIncludeContentIdentityDigest(producerEvaluation.generatedContent)
					return kbuildGeneratedIncludeProjection{
						cacheKey: fmt.Sprintf("generated-content:%d:%s:%x", producer.profile, producer.target, digest),
						contents: producerEvaluation.generatedContent,
						literal:  true,
					}, true, nil
				}
				if producerEvaluation.generatedContentPending {
					// Probe discovery has registered this exact immutable-source
					// generator, but its bytes intentionally do not exist yet. The
					// discovery action plan is discarded; use an empty include body
					// only to finish discovering the complete probe DAG. Replay must
					// replace it with the measured content above.
					return kbuildGeneratedIncludeProjection{
						cacheKey: fmt.Sprintf("pending-generated-content:%d:%s", producer.profile, producer.target),
						literal:  true,
					}, true, nil
				}
			}
			if producerEvaluation.projectionAmbiguous || producerEvaluation.sourceProjection == "" {
				return kbuildGeneratedIncludeProjection{}, false, nil
			}
			filename, ok := kconfig.ResolveCompactKbuildProfileSourcePath(
				profiles[producer.profile], producerEvaluation.sourceProjection,
			)
			if !ok {
				return kbuildGeneratedIncludeProjection{}, false, nil
			}
			return kbuildGeneratedIncludeProjection{filename: filename}, true, nil
		}
		includeResolution, err := quotedIncludes.references(
			profiles[consumer.profile], evaluation.includeSearches, satisfied, objectExists, objectSource,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"inspect selected target %s compiler include closure: %w", consumer.target, err,
			)
		}
		includeResolutions[consumer] = includeResolution
		if includeResolution.objectAllVisible || len(includeResolution.objectDirectories) != 0 {
			// A compiler input generated by an opaque action cannot be read during
			// planning. Preserve GNU Make's initial frontier and every selected
			// producer which can execute before this consumer, but only beneath the
			// exact object-tree include roots selected by this compiler command.
			// Source include roots, later producers, and unrelated outputs remain
			// excluded.
			evaluation.objectTreeSnapshot = true
			evaluation.objectTreeAllVisible = evaluation.objectTreeAllVisible || includeResolution.objectAllVisible
			evaluation.objectTreeReferences = append(
				evaluation.objectTreeReferences, includeResolution.objectDirectories...,
			)
		}
		for _, reference := range includeResolution.references {
			evaluation.directObjectTreeReferences = append(evaluation.directObjectTreeReferences, reference)
			if visibleIndex.hasInitialVisibleArtifact(reference) || resolvedConfigBaseline[reference] {
				evaluation.objectTreeSnapshot = true
				evaluation.objectTreeReferences = append(evaluation.objectTreeReferences, reference)
				continue
			}
			bound, err := bindSelectedGeneratedArtifact(consumer, reference, "compiler include")
			if err != nil {
				return nil, err
			}
			if !bound {
				// Literal includes in inactive preprocessor branches are retained
				// conservatively. With no selected producer they create no action
				// edge and remain the compiler's responsibility if activated.
				continue
			}
			evaluation.objectTreeSnapshot = true
		}
		evaluationForAction[consumer] = evaluation
	}
	// Exact object-tree and compiler-include edges above are part of program
	// provenance. Resolve command-head owners only after those strong edges are
	// present, so a selected same-path overwrite can supersede an invocation-
	// start executable deterministically.
	if err := resolveObjectTreeProgramOwners(); err != nil {
		return nil, err
	}

	// Exact include edges above are independent of opaque-root availability and
	// must be established for every consumer before predecessor closure is
	// inspected. Every opaque-root candidate below is already in that closure;
	// binding its direct weak edge therefore cannot expose another predecessor.
	// One pass records the physical input without redundantly recomputing the
	// unchanged transitive closure.
	for _, consumer := range includeConsumers {
		includeResolution := includeResolutions[consumer]
		if !includeResolution.objectAllVisible && len(includeResolution.objectDirectories) == 0 {
			continue
		}
		visibleIndex := indexes[consumer.profile]
		predecessors := reachabilityRound.allPredecessors(consumer)
		predecessorCandidatesByTarget := map[string][]actionIdentity{}
		for candidate := range predecessors {
			if stats != nil {
				stats.OpaqueRootPredecessorChecks++
			}
			if candidate == consumer ||
				selectedLifecycle[consumer] == "prep" && selectedLifecycle[candidate] != "prep" {
				continue
			}
			if indexes[candidate.profile].targetIsPhony(profiles[candidate.profile], candidate.target) ||
				candidate.target == "." || strings.HasSuffix(candidate.target, "/") {
				continue
			}
			predecessorCandidatesByTarget[candidate.target] = append(
				predecessorCandidatesByTarget[candidate.target], candidate,
			)
		}
		predecessorTargets := make([]string, 0, len(predecessorCandidatesByTarget))
		for target := range predecessorCandidatesByTarget {
			predecessorTargets = append(predecessorTargets, target)
		}
		sort.Strings(predecessorTargets)
		for _, reference := range predecessorTargets {
			if visibleIndex.hasInitialVisibleArtifact(reference) || resolvedConfigBaseline[reference] {
				continue
			}
			if !includeResolution.objectAllVisible &&
				!kbuildVisibleArtifactMatchesObjectTreeReferences(reference, includeResolution.objectDirectories) {
				continue
			}
			candidates := predecessorCandidatesByTarget[reference]
			sortSelectedActionCandidates(candidates)
			if len(candidates) != 0 && slices.ContainsFunc(candidates, func(candidate actionIdentity) bool {
				return selectedNonIncludeActions[candidate]
			}) && !slices.ContainsFunc(candidates, func(candidate actionIdentity) bool {
				return !selectedNonIncludeActions[candidate]
			}) {
				// Every exact producer version visible to this consumer is either an
				// executable or has source-proven non-include output provenance. A
				// same-path data writer keeps the reference conservative and lets
				// exact ownership resolution fail closed.
				continue
			}
			if _, err := bindGeneratedArtifactCandidates(
				consumer, reference, "opaque compiler include root", candidates, true,
			); err != nil {
				return nil, err
			}
		}
	}

	appendQueryArtifact := func(
		effect *deferredQueryEvaluation,
		producer actionIdentity,
		artifact kconfig.CompactKbuildVisibleArtifact,
		initial bool,
	) error {
		if producer == effect.origin && selectedLifecycle[effect.origin] != "" {
			return fmt.Errorf(
				"deferred Kbuild content query %q consumes its origin action through generated artifact %q",
				effect.query.Token, artifact.Path,
			)
		}
		effect.dependencies[producer] = true
		artifacts := &effect.generatedArtifacts
		artifactSet := &effect.generatedArtifactSet
		if initial {
			artifacts = &effect.initialArtifacts
			artifactSet = &effect.initialArtifactSet
		}
		if *artifactSet == nil {
			*artifactSet = map[kconfig.CompactKbuildVisibleArtifact]bool{}
		}
		if (*artifactSet)[artifact] {
			return nil
		}
		(*artifactSet)[artifact] = true
		*artifacts = append(*artifacts, artifact)
		return nil
	}
	queryAnchors := func(effect *deferredQueryEvaluation) []actionIdentity {
		anchors := []actionIdentity{}
		if selectedLifecycle[effect.origin] != "" {
			anchors = append(anchors, effect.origin)
		}
		for consumer := range effect.consumers {
			if !slices.Contains(anchors, consumer) {
				anchors = append(anchors, consumer)
			}
		}
		return anchors
	}
	resolveQueryGeneratedArtifact := func(
		effect *deferredQueryEvaluation,
		reference string,
	) (actionIdentity, bool, error) {
		candidates := append([]actionIdentity(nil), selectedActionsByTarget[reference]...)
		anchors := queryAnchors(effect)
		candidates = slices.DeleteFunc(candidates, func(candidate actionIdentity) bool {
			for _, anchor := range anchors {
				if !producerCanFeed(anchor, candidate) {
					return true
				}
			}
			return false
		})
		if len(candidates) == 0 {
			return actionIdentity{}, false, nil
		}
		if len(candidates) != 1 {
			labels := make([]string, 0, len(candidates))
			for _, candidate := range candidates {
				labels = append(labels, profiles[candidate.profile].Name+":"+candidate.target)
			}
			sort.Strings(labels)
			return actionIdentity{}, false, fmt.Errorf(
				"deferred Kbuild content query %q object-tree reference %q resolves to %d selected producers %q",
				effect.query.Token, reference, len(candidates), labels,
			)
		}
		return candidates[0], true, nil
	}
	for _, effect := range queryEvaluations {
		observation := effect.query.ObjectTree
		if !observation.ObservesObjectTree {
			continue
		}
		anchor := effect.origin
		if selectedLifecycle[anchor] == "" {
			for consumer := range effect.consumers {
				anchor = consumer
				break
			}
		}
		for _, artifact := range indexes[effect.origin.profile].matchingInitialVisibleArtifacts(
			observation.References, observation.ObservesAll,
		) {
			producer, err := resolveVisibleArtifactOwner(anchor, artifact)
			if err != nil {
				return nil, err
			}
			if err := appendQueryArtifact(effect, producer, artifact, true); err != nil {
				return nil, err
			}
		}
		if observation.ObservesAll {
			originItem := workItem{profile: effect.origin.profile, target: effect.origin.target, lifecycle: effect.lifecycle}
			seen := map[workItem]bool{}
			pending := append([]workItem(nil), dependencies[originItem]...)
			for len(pending) != 0 {
				candidate := pending[0]
				pending = pending[1:]
				if seen[candidate] {
					continue
				}
				seen[candidate] = true
				identity := actionIdentity{profile: candidate.profile, target: candidate.target}
				if materialized[candidate] && identity != effect.origin {
					artifact := kconfig.CompactKbuildVisibleArtifact{
						Path: identity.target, Profile: profiles[identity.profile].Name, Target: identity.target,
					}
					if err := appendQueryArtifact(effect, identity, artifact, false); err != nil {
						return nil, err
					}
					continue
				}
				pending = append(pending, dependencies[candidate]...)
			}
		}
		for reference := range selectedActionsByTarget {
			if observation.ObservesAll || !kbuildVisibleArtifactMatchesObjectTreeReferences(reference, observation.References) {
				continue
			}
			producer, found, err := resolveQueryGeneratedArtifact(effect, reference)
			if err != nil {
				return nil, err
			}
			if !found {
				continue
			}
			artifact := kconfig.CompactKbuildVisibleArtifact{
				Path: reference, Profile: profiles[producer.profile].Name, Target: producer.target,
			}
			if err := appendQueryArtifact(effect, producer, artifact, false); err != nil {
				return nil, err
			}
		}
	}

	// A recursive Make invocation starts with a concrete object-tree frontier
	// produced by earlier selected actions. Parser evaluation uses that same
	// frontier for $(wildcard); actions whose evaluated recipe can observe the
	// object tree must also consume the exact recorded owners during physical
	// stage solving. Owner provenance was captured when the frontier was formed.
	for consumer, evaluation := range evaluationForAction {
		if !evaluation.objectTreeSnapshot {
			continue
		}
		references := sortedUniquePaths(evaluation.objectTreeReferences)
		directReferences := sortedUniquePaths(evaluation.directObjectTreeReferences)
		invokedPrograms := map[string]bool{}
		for _, program := range sortedUniquePaths(evaluation.objectTreePrograms) {
			invokedPrograms[program] = true
		}
		for _, artifact := range indexes[consumer.profile].matchingInitialVisibleArtifacts(
			references, evaluation.objectTreeAllVisible,
		) {
			if generatedObjectTreeReplacementPathsByAction[consumer][artifact.Path] {
				// Exact command-head arbitration proved that a selected writer
				// replaces this invocation-start version before the consumer runs.
				// A later opaque/all-visible snapshot must not reintroduce the stale
				// same-path executable alongside its replacement.
				continue
			}
			producer, err := resolveVisibleArtifactOwner(consumer, artifact)
			if err != nil {
				return nil, err
			}
			directlyObserved := evaluation.directObjectTreeAllVisible ||
				kbuildVisibleArtifactMatchesObjectTreeReferences(artifact.Path, directReferences) ||
				invokedPrograms[artifact.Path]
			if selectedNonIncludeActions[producer] && !directlyObserved {
				// The initial visible frontier is the complete source-ordered state
				// inherited from an earlier recursive invocation. An opaque compiler
				// include directory may overlap that frontier, but it cannot consume
				// a generated executable or source-proven non-include output merely
				// because that output lives below the search root. Preserve exact
				// source-derived observations (including command heads and explicit
				// data operands); ordinary native prerequisites already retain their
				// own strong edge independently of the snapshot.
				continue
			}
			if err := bindObjectTreeArtifact(consumer, producer, artifact, !directlyObserved); err != nil {
				return nil, err
			}
			appendObjectTreeArtifact(
				initialObjectTreeArtifactsByAction,
				initialObjectTreeArtifactSetByAction,
				consumer,
				artifact,
			)
		}
	}
	nativeActionConsumers := map[actionIdentity]map[actionIdentity]bool{}
	for consumer, actionDependencies := range nativeActionDependencies {
		for dependency := range actionDependencies {
			if isWeakObjectTreeEdge(consumer, dependency) {
				continue
			}
			if nativeActionConsumers[dependency] == nil {
				nativeActionConsumers[dependency] = map[actionIdentity]bool{}
			}
			nativeActionConsumers[dependency][consumer] = true
		}
	}
	terminalActionsByProfile := make([][]actionIdentity, len(profiles))
	for profileIndex, candidates := range actionsByProfile {
		for _, candidate := range candidates {
			consumed := false
			for consumer := range nativeActionConsumers[candidate] {
				if consumer.profile == profileIndex {
					consumed = true
					break
				}
			}
			if !consumed {
				terminalActionsByProfile[profileIndex] = append(terminalActionsByProfile[profileIndex], candidate)
			}
		}
	}
	// An unruled prerequisite in an invocation with a source-ordered
	// predecessor is a potential side output of that completed predecessor.
	// Add its terminal actions as strong stage-solving edges now. Exact path and
	// output-slot ownership remain fail-closed in compactKbuildSideOutputDemands;
	// this early edge only prevents an artifact consumer from being reordered
	// ahead of the invocation which must have produced it.
	for consumer := range unruledActionDependencies {
		for _, predecessorName := range profiles[consumer.profile].InvocationPredecessors {
			predecessorIndex, exists := profileByName[predecessorName]
			if !exists {
				return nil, fmt.Errorf("Kbuild invocation %q references missing side-output predecessor %q", profiles[consumer.profile].Name, predecessorName)
			}
			for _, producer := range terminalActionsByProfile[predecessorIndex] {
				if producer == consumer {
					continue
				}
				existingDependency := nativeActionDependencies[consumer][producer]
				if nativeActionDependencies[consumer] == nil {
					nativeActionDependencies[consumer] = map[actionIdentity]bool{}
				}
				nativeActionDependencies[consumer][producer] = true
				if !existingDependency {
					reachabilityRound = newSelectionReachabilityRound(nativeActionDependencies)
				}
				if nativeActionConsumers[producer] == nil {
					nativeActionConsumers[producer] = map[actionIdentity]bool{}
				}
				nativeActionConsumers[producer][consumer] = true
			}
		}
	}

	// Deferred Make-shell queries are selected actions in their own right. Their
	// source/native dependencies participate in scope propagation, while the
	// later action which consumes the textual result contributes only a content
	// ordering edge. This prevents a host consumer from lending its compiler
	// contract to a target query captured in an earlier Make expansion.
	type effectIdentity struct {
		profile int
		target  string
		token   string
	}
	actionEffect := func(identity actionIdentity) effectIdentity {
		identity = physicalAction(identity)
		return effectIdentity{profile: identity.profile, target: identity.target}
	}
	queryEffect := func(effect *deferredQueryEvaluation) effectIdentity {
		return effectIdentity{profile: effect.origin.profile, target: effect.origin.target, token: effect.query.Token}
	}
	effectName := func(identity effectIdentity) string {
		if identity.token != "" {
			return fmt.Sprintf("deferred query %s at %s:%s", identity.token, profiles[identity.profile].Name, identity.target)
		}
		return profiles[identity.profile].Name + ":" + identity.target
	}
	nativeEffectDependencyOrigin := func(consumer, dependency effectIdentity) string {
		if consumer.token != "" || dependency.token != "" {
			return "native dependency"
		}
		action := actionIdentity{profile: consumer.profile, target: consumer.target}
		for _, artifact := range initialObjectTreeArtifactsByAction[action] {
			if artifact.Target == dependency.target {
				return "initial object-tree artifact " + artifact.Path
			}
		}
		for _, artifact := range generatedObjectTreeArtifactsByAction[action] {
			if artifact.Target == dependency.target {
				return "generated object-tree artifact " + artifact.Path
			}
		}
		return "source prerequisite"
	}
	effectLifecycle := map[effectIdentity]string{}
	effectRoles := map[effectIdentity][]kconfig.KbuildActionRoleRef{}
	effectPrimaryRoles := map[effectIdentity][]kconfig.KbuildActionRoleRef{}
	nativeEffectDependencies := map[effectIdentity]map[effectIdentity]bool{}
	invocationEffectDependencies := map[effectIdentity]map[effectIdentity]bool{}
	contentEffectDependencies := map[effectIdentity]map[effectIdentity]bool{}
	for identity, lifecycle := range selectedLifecycle {
		effect := actionEffect(identity)
		if previous := effectLifecycle[effect]; previous == "" || lifecycle == "prep" {
			effectLifecycle[effect] = lifecycle
		}
		effectRoles[effect] = evaluationForAction[identity].actionRoles
		effectPrimaryRoles[effect] = evaluationForAction[identity].primaryActionRoles
		for dependency := range nativeActionDependencies[identity] {
			if isWeakObjectTreeEdge(identity, dependency) {
				continue
			}
			dependencyEffect := actionEffect(dependency)
			if dependencyEffect == effect {
				continue
			}
			if nativeEffectDependencies[effect] == nil {
				nativeEffectDependencies[effect] = map[effectIdentity]bool{}
			}
			nativeEffectDependencies[effect][dependencyEffect] = true
		}
		for dependency := range invocationActionDependencies[identity] {
			dependencyEffect := actionEffect(dependency)
			if dependencyEffect == effect {
				continue
			}
			if invocationEffectDependencies[effect] == nil {
				invocationEffectDependencies[effect] = map[effectIdentity]bool{}
			}
			invocationEffectDependencies[effect][dependencyEffect] = true
		}
	}
	for _, query := range queryEvaluations {
		effect := queryEffect(query)
		effectLifecycle[effect] = query.lifecycle
		effectRoles[effect] = query.query.ActionRoles
		for dependency := range query.dependencies {
			if nativeEffectDependencies[effect] == nil {
				nativeEffectDependencies[effect] = map[effectIdentity]bool{}
			}
			nativeEffectDependencies[effect][actionEffect(dependency)] = true
		}
		for consumer := range query.consumers {
			consumerEffect := actionEffect(consumer)
			if contentEffectDependencies[consumerEffect] == nil {
				contentEffectDependencies[consumerEffect] = map[effectIdentity]bool{}
			}
			contentEffectDependencies[consumerEffect][effect] = true
		}
	}
	nativeEffectConsumers := map[effectIdentity]map[effectIdentity]bool{}
	for consumer, effectDependencies := range nativeEffectDependencies {
		for dependency := range effectDependencies {
			if nativeEffectConsumers[dependency] == nil {
				nativeEffectConsumers[dependency] = map[effectIdentity]bool{}
			}
			nativeEffectConsumers[dependency][consumer] = true
		}
	}

	hostScoped := map[effectIdentity]bool{}
	pendingHost := []effectIdentity{}
	prehostScoped := map[effectIdentity]bool{}
	prehostReason := map[effectIdentity]string{}
	prehostParent := map[effectIdentity]effectIdentity{}
	pendingPrehost := []effectIdentity{}
	bootstrapScoped := map[effectIdentity]bool{}
	bootstrapReason := map[effectIdentity]string{}
	bootstrapParent := map[effectIdentity]effectIdentity{}
	pendingBootstrap := []effectIdentity{}
	markBootstrapScope := func(identity, parent effectIdentity, reason string) {
		if bootstrapScoped[identity] || prehostScoped[identity] {
			return
		}
		bootstrapScoped[identity] = true
		bootstrapReason[identity] = reason
		bootstrapParent[identity] = parent
		pendingBootstrap = append(pendingBootstrap, identity)
	}
	markPrehostScope := func(identity, parent effectIdentity, reason string) {
		if prehostScoped[identity] {
			return
		}
		delete(hostScoped, identity)
		delete(bootstrapScoped, identity)
		prehostScoped[identity] = true
		prehostReason[identity] = reason
		prehostParent[identity] = parent
		pendingPrehost = append(pendingPrehost, identity)
	}
	hostRoles := map[effectIdentity][]string{}
	targetRoles := map[effectIdentity][]string{}
	hostPrimaryRoles := map[effectIdentity][]string{}
	targetPrimaryRoles := map[effectIdentity][]string{}
	hostScopeSeed := map[effectIdentity]bool{}
	targetScopeBoundary := map[effectIdentity]bool{}
	for identity, refs := range effectRoles {
		for _, ref := range refs {
			switch ref.Scope {
			case "host":
				hostRoles[identity] = append(hostRoles[identity], ref.Role)
			case "target":
				targetRoles[identity] = append(targetRoles[identity], ref.Role)
			case kconfig.KbuildActionRoleAutoScope:
				// Scope-neutral roles follow this effect's source closure, never a
				// later content consumer.
			default:
				return nil, fmt.Errorf("selected Kbuild effect %s has invalid role scope %q", effectName(identity), ref.Scope)
			}
		}
		for _, ref := range effectPrimaryRoles[identity] {
			switch ref.Scope {
			case "host":
				hostPrimaryRoles[identity] = append(hostPrimaryRoles[identity], ref.Role)
			case "target":
				targetPrimaryRoles[identity] = append(targetPrimaryRoles[identity], ref.Role)
			case kconfig.KbuildActionRoleAutoScope:
				// A neutral command head follows the source-derived closure just
				// like a neutral auxiliary role.
			default:
				return nil, fmt.Errorf("selected Kbuild effect %s has invalid primary role scope %q", effectName(identity), ref.Scope)
			}
		}
		if len(hostPrimaryRoles[identity]) != 0 && len(targetPrimaryRoles[identity]) != 0 {
			sort.Strings(hostPrimaryRoles[identity])
			sort.Strings(targetPrimaryRoles[identity])
			return nil, fmt.Errorf(
				"selected Kbuild effect %s mixes explicit host primary roles %s with target primary roles %s",
				effectName(identity),
				strings.Join(slices.Compact(hostPrimaryRoles[identity]), ", "),
				strings.Join(slices.Compact(targetPrimaryRoles[identity]), ", "),
			)
		}
		// A neutral query is a source-time effect and defaults to its target
		// invocation. Content users may move it to bootstrap, but never change
		// which toolset answers the query.
		if identity.token != "" && len(hostRoles[identity]) == 0 && len(targetRoles[identity]) == 0 {
			targetRoles[identity] = append(targetRoles[identity], "source-origin")
		}
		// An explicit command head owns scope. For recipes whose primary is a
		// generated executable or source script, preserve the former source-
		// closure rule: host-only configured use selects host, while target or
		// mixed use defaults to target. Opposite-scope argv/environment tools do
		// not move a configured primary action between stages.
		hostScopeSeed[identity] = len(hostPrimaryRoles[identity]) != 0 ||
			len(hostPrimaryRoles[identity]) == 0 && len(targetPrimaryRoles[identity]) == 0 &&
				len(hostRoles[identity]) != 0 && len(targetRoles[identity]) == 0
		targetScopeBoundary[identity] = len(targetPrimaryRoles[identity]) != 0 ||
			!hostScopeSeed[identity] && len(targetRoles[identity]) != 0
		if hostScopeSeed[identity] {
			hostScoped[identity] = true
			pendingHost = append(pendingHost, identity)
		}
	}
	// A weak invocation predecessor can move ahead of a later host action only
	// when its native prerequisite closure is itself pre-host compatible. Record
	// native host dependence independently of the selected scope fixed point:
	// explicit target consumers remain target-scoped, and source ordering alone
	// must not pull their H -> T boundary across a later host action.
	nativeHostDependent := map[effectIdentity]bool{}
	pendingNativeHostDependent := []effectIdentity{}
	for identity := range hostScopeSeed {
		if !hostScopeSeed[identity] {
			continue
		}
		nativeHostDependent[identity] = true
		pendingNativeHostDependent = append(pendingNativeHostDependent, identity)
	}
	for len(pendingNativeHostDependent) != 0 {
		identity := pendingNativeHostDependent[0]
		pendingNativeHostDependent = pendingNativeHostDependent[1:]
		for consumer := range nativeEffectConsumers[identity] {
			if nativeHostDependent[consumer] {
				continue
			}
			nativeHostDependent[consumer] = true
			pendingNativeHostDependent = append(pendingNativeHostDependent, consumer)
		}
	}
	for len(pendingHost) != 0 {
		identity := pendingHost[0]
		pendingHost = pendingHost[1:]
		propagate := func(dependency effectIdentity, edge string) {
			if targetScopeBoundary[dependency] {
				markBootstrapScope(dependency, identity, fmt.Sprintf(
					"%s dependency of host-scoped %s", edge, effectName(identity),
				))
				return
			}
			if !hostScoped[dependency] {
				hostScoped[dependency] = true
				pendingHost = append(pendingHost, dependency)
			}
		}
		for dependency := range nativeEffectDependencies[identity] {
			propagate(dependency, nativeEffectDependencyOrigin(identity, dependency))
		}
		for dependency := range invocationEffectDependencies[identity] {
			if nativeHostDependent[dependency] {
				// This is source-owned execution order, not an artifact edge.
				// Keep the predecessor's native H -> T boundary and project the
				// independent later host action before its post-host target tail.
				continue
			}
			propagate(dependency, "invocation")
		}
	}
	// Content edges affect physical availability only. A target-scoped query
	// consumed by a host/bootstrap action is promoted, with its native producer
	// closure, without contaminating either effect's compiler scope.
	for consumer, dependencies := range contentEffectDependencies {
		if !hostScoped[consumer] && !bootstrapScoped[consumer] {
			continue
		}
		for dependency := range dependencies {
			if !hostScoped[dependency] {
				markBootstrapScope(dependency, consumer, fmt.Sprintf(
					"content dependency of %s", effectName(consumer),
				))
			}
		}
	}
	// Target-scoped prerequisites needed by host actions execute in the target
	// toolset's bootstrap phase. Their complete prerequisite closure may override
	// a propagated host classification for a shared neutral node: one bootstrap
	// execution can feed both sides of that diamond. An explicit host dependency
	// in this closure is promoted to the initial prehost phase; only a target
	// dependency reached again from that prehost closure exceeds the bounded
	// physical alternation graph.
	for len(pendingBootstrap) != 0 {
		identity := pendingBootstrap[0]
		pendingBootstrap = pendingBootstrap[1:]
		if prehostScoped[identity] {
			continue
		}
		markBootstrap := func(dependency effectIdentity) {
			if !bootstrapScoped[dependency] {
				delete(hostScoped, dependency)
				markBootstrapScope(dependency, identity, fmt.Sprintf(
					"%s of bootstrap-scoped %s", nativeEffectDependencyOrigin(identity, dependency), effectName(identity),
				))
			}
		}
		for dependency := range nativeEffectDependencies[identity] {
			if hostScopeSeed[dependency] {
				markPrehostScope(dependency, identity, fmt.Sprintf(
					"%s of bootstrap-scoped %s", nativeEffectDependencyOrigin(identity, dependency), effectName(identity),
				))
				continue
			}
			markBootstrap(dependency)
		}
		for dependency := range invocationEffectDependencies[identity] {
			if hostScopeSeed[dependency] || hostScoped[dependency] || nativeHostDependent[dependency] {
				// InvocationPredecessors are execution provenance, not a
				// consumed artifact. The stage-aware selection graph retains
				// every representable predecessor and omits this backward weak
				// edge for a bootstrap consumer. Effective host scope includes
				// neutral actions connected to an explicit host recipe through
				// native artifact edges.
				continue
			}
			markBootstrap(dependency)
		}
	}
	// One initial host phase makes Linux's native H -> T -> H build topology
	// representable without weakening artifact dependencies. Its complete hard
	// prerequisite closure must remain host-compatible. Reaching an explicit
	// target action here would require an earlier target phase and therefore one
	// more alternation than the bounded physical graph provides. Invocation
	// predecessors stay weak: a later-stage predecessor is omitted by the
	// selection graph instead of being promoted across this boundary.
	for len(pendingPrehost) != 0 {
		identity := pendingPrehost[0]
		pendingPrehost = pendingPrehost[1:]
		markPrehostDependency := func(dependency effectIdentity, edge string) error {
			if targetScopeBoundary[dependency] {
				causes := []string{}
				for current := identity; prehostScoped[current]; {
					causes = append(causes, prehostReason[current])
					parent := prehostParent[current]
					if parent == current || !prehostScoped[parent] {
						break
					}
					current = parent
				}
				return fmt.Errorf(
					"initial host Kbuild closure %s (%s) reaches explicit target-role dependency %s through %s and requires another toolchain-scope alternation",
					effectName(identity), strings.Join(causes, "; then "), effectName(dependency), edge,
				)
			}
			markPrehostScope(dependency, identity, fmt.Sprintf(
				"%s of prehost-scoped %s", edge, effectName(identity),
			))
			return nil
		}
		for dependency := range nativeEffectDependencies[identity] {
			if err := markPrehostDependency(dependency, nativeEffectDependencyOrigin(identity, dependency)); err != nil {
				return nil, err
			}
		}
		for dependency := range contentEffectDependencies[identity] {
			if err := markPrehostDependency(dependency, "content dependency"); err != nil {
				return nil, err
			}
		}
	}
	effectStage := func(identity effectIdentity) string {
		if prehostScoped[identity] {
			return "prehost"
		}
		if bootstrapScoped[identity] {
			return "bootstrap"
		}
		if hostScoped[identity] {
			return "host"
		}
		return effectLifecycle[identity]
	}
	stageOrder := map[string]int{"prehost": 0, "bootstrap": 1, "host": 2, "prep": 3, "target": 4}
	for consumer, dependencies := range nativeEffectDependencies {
		for dependency := range dependencies {
			if stageOrder[effectStage(dependency)] > stageOrder[effectStage(consumer)] {
				return nil, fmt.Errorf(
					"selected Kbuild effect %s in %s stage has later native dependency %s in %s stage",
					effectName(consumer), effectStage(consumer), effectName(dependency), effectStage(dependency),
				)
			}
		}
	}
	for consumer, dependencies := range contentEffectDependencies {
		for dependency := range dependencies {
			if stageOrder[effectStage(dependency)] > stageOrder[effectStage(consumer)] {
				return nil, fmt.Errorf(
					"selected Kbuild action %s in %s stage consumes deferred query %s in later %s stage",
					effectName(consumer), effectStage(consumer), effectName(dependency), effectStage(dependency),
				)
			}
		}
	}
	stageVisibleObjectTreeArtifacts := func(
		consumer actionIdentity,
		artifacts []kconfig.CompactKbuildVisibleArtifact,
	) []kconfig.CompactKbuildVisibleArtifact {
		consumer = physicalAction(consumer)
		weakArtifacts := weakObjectTreeArtifacts[consumer]
		if len(weakArtifacts) == 0 {
			return artifacts
		}
		consumerStage := effectStage(actionEffect(consumer))
		return slices.DeleteFunc(append([]kconfig.CompactKbuildVisibleArtifact(nil), artifacts...), func(artifact kconfig.CompactKbuildVisibleArtifact) bool {
			producer, weak := weakArtifacts[artifact]
			return weak && stageOrder[effectStage(actionEffect(producer))] > stageOrder[consumerStage]
		})
	}

	selections := make([]kconfig.CompactKbuildSelection, 0, len(selectedLifecycle))
	for identity := range selectedLifecycle {
		effect := actionEffect(identity)
		physical := physicalAction(identity)
		lifecycle := effectLifecycle[effect]
		scope := "target"
		stage := lifecycle
		if prehostScoped[effect] {
			scope = "host"
			stage = "prehost"
		} else if bootstrapScoped[effect] {
			stage = "bootstrap"
		} else if hostScoped[effect] {
			scope = "host"
			stage = "host"
		}
		groupedTrigger := ""
		if _, grouped := groupedAction[identity]; grouped {
			groupedTrigger = physical.target
		}
		makeTarget := kbuildProfileLookupTarget(
			profiles[identity.profile], identity.target,
			evaluations[identity.profile][identity.target].lookupTarget,
		)
		physicalEvaluation := evaluationForAction[physical]
		exactGeneratedContent := ""
		exactGeneratedContentSet := false
		if groupedTrigger == "" && generatedContentAdmitted[physical] &&
			physicalEvaluation.generatedContentSet && physicalEvaluation.evaluatedCommandTexts == 1 {
			exactGeneratedContent = physicalEvaluation.generatedContent
			exactGeneratedContentSet = true
		}
		selections = append(selections, kconfig.CompactKbuildSelection{
			Profile:               profiles[identity.profile].Name,
			Target:                identity.target,
			MakeTarget:            makeTarget,
			GroupedTrigger:        groupedTrigger,
			UsesInitialObjectTree: evaluationForAction[physical].objectTreeSnapshot,
			InitialObjectTreeArtifacts: kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts(
				stageVisibleObjectTreeArtifacts(physical, initialObjectTreeArtifactsByAction[physical]),
			),
			GeneratedObjectTreeArtifacts: kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts(
				stageVisibleObjectTreeArtifacts(physical, generatedObjectTreeArtifactsByAction[physical]),
			),
			Lifecycle: lifecycle, Scope: scope, Stage: stage,
			DeferredContentQueries: kconfig.EncodeCompactKbuildDeferredContentQueries(queryTokensByAction[physical]),
			ExactGeneratedContent:  exactGeneratedContent, ExactGeneratedContentSet: exactGeneratedContentSet,
		})
	}
	querySelections := map[string]kconfig.KbuildDeferredContentSelection{}
	for token, query := range queryEvaluations {
		effect := queryEffect(query)
		scope := "target"
		if prehostScoped[effect] || hostScoped[effect] {
			scope = "host"
		}
		querySelections[token] = kconfig.KbuildDeferredContentSelection{
			Token: token, Profile: profiles[query.origin.profile].Name, Target: query.origin.target,
			Lifecycle: query.lifecycle, Scope: scope, Stage: effectStage(effect),
			UsesInitialObjectTree:        query.query.ObjectTree.ObservesObjectTree,
			InitialObjectTreeArtifacts:   kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts(query.initialArtifacts),
			GeneratedObjectTreeArtifacts: kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts(query.generatedArtifacts),
		}
	}
	if err := kconfig.ApplyKbuildDeferredContentSelections(profiles, querySelections); err != nil {
		return nil, err
	}
	sort.Slice(selections, func(i, j int) bool {
		if selections[i].Stage != selections[j].Stage {
			return selections[i].Stage < selections[j].Stage
		}
		if selections[i].Profile != selections[j].Profile {
			return selections[i].Profile < selections[j].Profile
		}
		return selections[i].Target < selections[j].Target
	})
	return selections, nil
}

func kbuildProfileTarget(profile kconfig.CompactKbuildProfile, value string) string {
	return kconfig.CanonicalKbuildProfileTarget(profile, value)
}

// kbuildProfileLookupTarget keeps GNU Make's lexical filename for declaration
// matching only when it names the same canonical graph target. Linux uses this
// deliberately for cross-directory composite objects, for example
// arch/x86/kvm/../../../virt/kvm/kvm_main.o. Selection and action identities
// remain canonical so parent traversal never escapes into Bazel output paths.
func kbuildProfileLookupTarget(
	profile kconfig.CompactKbuildProfile,
	target, makeTarget string,
) string {
	lookupTarget, _ := kconfig.ResolveCompactKbuildMakeTarget(profile, target, makeTarget)
	return lookupTarget
}

type kbuildProfileRuleCandidate struct {
	stem         string
	index        int
	lookupTarget string
}

type kbuildProfileRuleMatches struct {
	explicit []kbuildProfileRuleCandidate
	implicit []kbuildProfileRuleCandidate
}

type kbuildProfileRuleLookup struct {
	target       string
	lookupTarget string
}

type kbuildResolvedTargetKey struct {
	profile    string
	target     string
	makeTarget string
}

type kbuildResolvedTargetResult struct {
	resolved *kconfig.CompactKbuildResolvedTarget
	err      error
}

// kbuildResolvedTargetCache is scoped to one discovery/selection walk. It
// retains the expensive GNU Make target resolution across recursive-invocation
// discovery and final action selection. Its lifetime ends with invocation-
// profile selection: handles are neither serialized nor shared with a later
// metadata/action-plan replay, so symbolic discovery state cannot cross that
// execution boundary.
type kbuildResolvedTargetCache struct {
	values      map[kbuildResolvedTargetKey]kbuildResolvedTargetResult
	profileKeys map[string][]kbuildResolvedTargetKey
}

func newKbuildResolvedTargetCache() *kbuildResolvedTargetCache {
	return &kbuildResolvedTargetCache{
		values:      map[kbuildResolvedTargetKey]kbuildResolvedTargetResult{},
		profileKeys: map[string][]kbuildResolvedTargetKey{},
	}
}

func (c *kbuildResolvedTargetCache) resolve(
	profile kconfig.CompactKbuildProfile,
	target, makeTarget string,
) (*kconfig.CompactKbuildResolvedTarget, error) {
	target = kconfig.CanonicalKbuildGraphTarget(target)
	makeTarget = kbuildProfileLookupTarget(profile, target, makeTarget)
	if c == nil {
		return kconfig.ResolveCompactKbuildTargetForMakeTarget(profile, target, makeTarget)
	}
	key := kbuildResolvedTargetKey{profile: profile.Name, target: target, makeTarget: makeTarget}
	if cached, ok := c.values[key]; ok {
		return cached.resolved, cached.err
	}
	resolved, err := kconfig.ResolveCompactKbuildTargetForMakeTarget(profile, target, makeTarget)
	c.values[key] = kbuildResolvedTargetResult{resolved: resolved, err: err}
	c.profileKeys[profile.Name] = append(c.profileKeys[profile.Name], key)
	return resolved, err
}

func (c *kbuildResolvedTargetCache) invalidateProfile(profile string) {
	if c == nil {
		return
	}
	for _, key := range c.profileKeys[profile] {
		delete(c.values, key)
	}
	delete(c.profileKeys, profile)
}

// kbuildProfileTargetIndex separates the immutable shape of a parsed Make
// invocation from demand-specific viability checks. A Linux invocation often
// has tens of thousands of explicit rules but only a small selected closure;
// scanning every rule for every selected target made that closure quadratic.
//
// Explicit candidate lists retain source rule order. Implicit candidates use
// GNU Make's shortest-stem ordering, with declaration order breaking ties. The
// latter matters when an invocation-boundary side output makes every implicit
// prerequisite appear absent during discovery: the retained diagnostic
// fallback must still be the rule GNU Make and production lowering select.
type kbuildProfileTargetIndex struct {
	ruleTargets           [][]string
	ruleTargetPattern     []string
	explicitRules         map[string][]int
	implicitRules         map[string][]int
	phony                 map[string]bool
	generated             map[string]string
	initialVisibleProfile kconfig.CompactKbuildProfile
	matches               map[kbuildProfileRuleLookup]kbuildProfileRuleMatches
	resolvedTargets       *kbuildResolvedTargetCache
	stats                 *kbuildSelectionWalkStats
}

func newKbuildProfileTargetIndex(profile kconfig.CompactKbuildProfile, stats *kbuildSelectionWalkStats) *kbuildProfileTargetIndex {
	return newKbuildProfileTargetIndexWithResolvedTargets(profile, stats, nil)
}

func newKbuildProfileTargetIndexWithResolvedTargets(
	profile kconfig.CompactKbuildProfile,
	stats *kbuildSelectionWalkStats,
	resolvedTargets *kbuildResolvedTargetCache,
) *kbuildProfileTargetIndex {
	index := &kbuildProfileTargetIndex{
		ruleTargets:           make([][]string, len(profile.Rules)),
		ruleTargetPattern:     make([]string, len(profile.Rules)),
		explicitRules:         map[string][]int{},
		implicitRules:         map[string][]int{},
		phony:                 map[string]bool{},
		generated:             map[string]string{},
		initialVisibleProfile: profile,
		matches:               map[kbuildProfileRuleLookup]kbuildProfileRuleMatches{},
		resolvedTargets:       resolvedTargets,
		stats:                 stats,
	}
	for ruleIndex, rule := range profile.Rules {
		index.ruleTargets[ruleIndex] = make([]string, 0, len(rule.Targets))
		implicitPrefixes := map[string]bool{}
		for _, rawPattern := range rule.Targets {
			pattern := kbuildProfileTarget(profile, rawPattern)
			index.ruleTargets[ruleIndex] = append(index.ruleTargets[ruleIndex], pattern)
			if strings.Contains(pattern, "%") {
				prefix, _, _ := strings.Cut(pattern, "%")
				if !implicitPrefixes[prefix] {
					index.implicitRules[prefix] = append(index.implicitRules[prefix], ruleIndex)
					implicitPrefixes[prefix] = true
				}
				continue
			}
			rules := index.explicitRules[pattern]
			if len(rules) == 0 || rules[len(rules)-1] != ruleIndex {
				index.explicitRules[pattern] = append(rules, ruleIndex)
			}
		}
		if rule.TargetPattern != "" {
			index.ruleTargetPattern[ruleIndex] = kbuildProfileTarget(profile, rule.TargetPattern)
		}
		for _, target := range index.ruleTargets[ruleIndex] {
			if target != ".PHONY" {
				continue
			}
			for _, prerequisite := range rule.Prerequisites {
				index.phony[kbuildProfileTarget(profile, prerequisite)] = true
			}
			break
		}
	}
	for _, target := range profile.Generated {
		canonical := kconfig.CanonicalKbuildProfileGeneratedTarget(profile, target.Target)
		if _, exists := index.generated[canonical]; !exists {
			// The previous linear lookup returned the first declaration.
			index.generated[canonical] = target.Kind
		}
	}
	return index
}

func (i *kbuildProfileTargetIndex) resolveTarget(
	profile kconfig.CompactKbuildProfile,
	target, makeTarget string,
) (*kconfig.CompactKbuildResolvedTarget, error) {
	if i == nil {
		return kconfig.ResolveCompactKbuildTargetForMakeTarget(profile, target, makeTarget)
	}
	return i.resolvedTargets.resolve(profile, target, makeTarget)
}

// initialVisibleArtifact returns the same last source-ordered exact owner as
// the maps historically rebuilt by selection planning.
func (index *kbuildProfileTargetIndex) initialVisibleArtifact(path string) (kconfig.CompactKbuildVisibleArtifact, bool) {
	if index == nil {
		return kconfig.CompactKbuildVisibleArtifact{}, false
	}
	return kconfig.CompactKbuildProfileInitialVisibleArtifact(index.initialVisibleProfile, path)
}

func (index *kbuildProfileTargetIndex) hasInitialVisibleArtifact(path string) bool {
	_, ok := index.initialVisibleArtifact(path)
	return ok
}

func (index *kbuildProfileTargetIndex) initialVisibleArtifactOwnedBy(path, profile, target string) bool {
	if index == nil {
		return false
	}
	artifact, ok := kconfig.CompactKbuildProfileInitialVisibleArtifact(index.initialVisibleProfile, path)
	return ok && artifact.Profile == profile && artifact.Target == target
}

// matchingInitialVisibleArtifacts implements
// kbuildVisibleArtifactMatchesObjectTreeReferences with indexed exact and
// lexicographic prefix ranges over the immutable persistent view.
func (index *kbuildProfileTargetIndex) matchingInitialVisibleArtifacts(references []string, all bool) []kconfig.CompactKbuildVisibleArtifact {
	if index == nil {
		return nil
	}
	artifactsByPath := map[string]kconfig.CompactKbuildVisibleArtifact{}
	if all {
		kconfig.RangeCompactKbuildProfileInitialVisibleArtifacts(
			index.initialVisibleProfile, "", func(artifact kconfig.CompactKbuildVisibleArtifact) bool {
				artifactsByPath[artifact.Path] = artifact
				return true
			},
		)
	} else {
		for _, reference := range references {
			if artifact, ok := kconfig.CompactKbuildProfileInitialVisibleArtifact(
				index.initialVisibleProfile, reference,
			); ok {
				artifactsByPath[artifact.Path] = artifact
			}
			prefix := strings.TrimSuffix(reference, "/") + "/"
			kconfig.RangeCompactKbuildProfileInitialVisibleArtifacts(
				index.initialVisibleProfile, prefix, func(artifact kconfig.CompactKbuildVisibleArtifact) bool {
					artifactsByPath[artifact.Path] = artifact
					return true
				},
			)
		}
	}
	paths := make([]string, 0, len(artifactsByPath))
	for path := range artifactsByPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	artifacts := make([]kconfig.CompactKbuildVisibleArtifact, 0, len(paths))
	for _, path := range paths {
		artifacts = append(artifacts, artifactsByPath[path])
	}
	return artifacts
}

// matchingImplicitRuleIndexes returns only pattern declarations whose literal
// prefix can match target. Linux Makefiles contain many directory-specific
// pattern families; scanning every family for every selected prerequisite
// makes source-derived closure discovery quadratic in the complete parsed
// graph. Buckets retain declaration order, and the final sort preserves GNU
// Make's source-order rule semantics when a mixed-target declaration appears
// in more than one compatible prefix bucket.
func (index *kbuildProfileTargetIndex) matchingImplicitRuleIndexes(target string) []int {
	if index == nil || len(index.implicitRules) == 0 {
		return nil
	}
	seen := map[int]bool{}
	candidates := []int{}
	for end := 0; end <= len(target); end++ {
		for _, ruleIndex := range index.implicitRules[target[:end]] {
			if seen[ruleIndex] {
				continue
			}
			seen[ruleIndex] = true
			candidates = append(candidates, ruleIndex)
		}
	}
	sort.Ints(candidates)
	return candidates
}

func (index *kbuildProfileTargetIndex) ruleMatches(
	profile kconfig.CompactKbuildProfile,
	target, makeTarget string,
) kbuildProfileRuleMatches {
	target = kconfig.CanonicalKbuildGraphTarget(target)
	lookupTarget := kbuildProfileLookupTarget(profile, target, makeTarget)
	lookup := kbuildProfileRuleLookup{target: target, lookupTarget: lookupTarget}
	if matches, ok := index.matches[lookup]; ok {
		return matches
	}
	explicitIndexes := index.explicitRules[target]
	implicitIndexSet := map[int]bool{}
	for _, candidateTarget := range []string{lookupTarget, target} {
		for _, ruleIndex := range index.matchingImplicitRuleIndexes(candidateTarget) {
			implicitIndexSet[ruleIndex] = true
		}
	}
	implicitIndexes := make([]int, 0, len(implicitIndexSet))
	for ruleIndex := range implicitIndexSet {
		implicitIndexes = append(implicitIndexes, ruleIndex)
	}
	sort.Ints(implicitIndexes)
	matches := kbuildProfileRuleMatches{}
	// Both index lists are in source order. Merge them instead of processing
	// explicit rules first because a mixed rule is classified by its first
	// matching target pattern, just as it was in the linear scan.
	for explicitAt, implicitAt := 0, 0; explicitAt < len(explicitIndexes) || implicitAt < len(implicitIndexes); {
		ruleIndex := 0
		switch {
		case implicitAt >= len(implicitIndexes):
			ruleIndex = explicitIndexes[explicitAt]
			explicitAt++
		case explicitAt >= len(explicitIndexes):
			ruleIndex = implicitIndexes[implicitAt]
			implicitAt++
		case explicitIndexes[explicitAt] < implicitIndexes[implicitAt]:
			ruleIndex = explicitIndexes[explicitAt]
			explicitAt++
		case implicitIndexes[implicitAt] < explicitIndexes[explicitAt]:
			ruleIndex = implicitIndexes[implicitAt]
			implicitAt++
		default:
			ruleIndex = explicitIndexes[explicitAt]
			explicitAt++
			implicitAt++
		}
		if index.stats != nil {
			index.stats.RuleCandidateChecks++
		}
		for _, pattern := range index.ruleTargets[ruleIndex] {
			candidateTarget := target
			stem, ok := matchMakeTargetPattern(pattern, candidateTarget)
			if strings.Contains(pattern, "%") && lookupTarget != target {
				if lexicalStem, lexical := matchMakeTargetPattern(pattern, lookupTarget); lexical {
					stem, ok = lexicalStem, true
					candidateTarget = lookupTarget
				}
			}
			if !ok {
				continue
			}
			if targetPattern := index.ruleTargetPattern[ruleIndex]; targetPattern != "" {
				if lookupTarget != target {
					if lexicalStem, lexical := matchMakeTargetPattern(targetPattern, lookupTarget); lexical {
						stem, ok = lexicalStem, true
						candidateTarget = lookupTarget
					} else {
						stem, ok = matchMakeTargetPattern(targetPattern, candidateTarget)
					}
				} else {
					stem, ok = matchMakeTargetPattern(targetPattern, candidateTarget)
				}
				if !ok {
					continue
				}
			}
			candidate := kbuildProfileRuleCandidate{stem: stem, index: ruleIndex, lookupTarget: candidateTarget}
			if strings.Contains(pattern, "%") {
				matches.implicit = append(matches.implicit, candidate)
			} else {
				matches.explicit = append(matches.explicit, candidate)
			}
			break
		}
	}
	sort.SliceStable(matches.implicit, func(i, j int) bool {
		if len(matches.implicit[i].stem) != len(matches.implicit[j].stem) {
			return len(matches.implicit[i].stem) < len(matches.implicit[j].stem)
		}
		return matches.implicit[i].index < matches.implicit[j].index
	})
	index.matches[lookup] = matches
	return matches
}

func kbuildSelectionPrerequisiteTargets(prerequisites []kbuildSelectionPrerequisite) []string {
	targets := make([]string, len(prerequisites))
	for index, prerequisite := range prerequisites {
		targets[index] = prerequisite.target
	}
	return targets
}

func kbuildSelectionPrerequisiteMakeTargets(prerequisites []kbuildSelectionPrerequisite) []string {
	targets := make([]string, len(prerequisites))
	for index, prerequisite := range prerequisites {
		targets[index] = prerequisite.makeTarget
	}
	return targets
}

func canonicalKbuildSelectionPrerequisites(targets []string) []kbuildSelectionPrerequisite {
	prerequisites := make([]kbuildSelectionPrerequisite, len(targets))
	for index, target := range targets {
		prerequisites[index] = kbuildSelectionPrerequisite{target: target, makeTarget: target}
	}
	return prerequisites
}

// evaluateSelectedKbuildRuleContextForMakeTarget keeps evaluator-backed target
// resolution authoritative while recovering each declaration's exact Make
// spelling. A lexical target which canonicalizes across directories already
// has a uniquely reconstructed indexed context; deferring its full resolution
// until an executable action needs provenance avoids doing that work for
// dependency-only declarations.
func evaluateSelectedKbuildRuleContextForMakeTarget(
	profile kconfig.CompactKbuildProfile,
	index *kbuildProfileTargetIndex,
	target, makeTarget string,
	ruleIndexes, effectiveRecipeIndexes []int,
) ([]kbuildSelectionPrerequisite, []kbuildSelectionPrerequisite, string, *kconfig.CompactKbuildResolvedTarget, error) {
	target = kconfig.CanonicalKbuildGraphTarget(target)
	makeTarget = kbuildProfileLookupTarget(profile, target, makeTarget)
	reconstructedNormal, reconstructedOrderOnly, reconstructedStem, reconstructed, err :=
		reconstructSelectedKbuildRuleContextForMakeTarget(
			profile, index, target, makeTarget, ruleIndexes, effectiveRecipeIndexes,
		)
	if err != nil {
		return nil, nil, "", nil, err
	}
	if makeTarget != target {
		if !reconstructed {
			return nil, nil, "", nil, fmt.Errorf(
				"lexical Make target %q (canonical %q) has no unique selected recipe context",
				makeTarget, target,
			)
		}
		return reconstructedNormal, reconstructedOrderOnly, reconstructedStem, nil, nil
	}

	resolved, err := index.resolveTarget(profile, target, makeTarget)
	if err != nil {
		return nil, nil, "", nil, err
	}
	context, err := resolved.Context()
	if err != nil {
		return nil, nil, "", nil, err
	}
	normal := make([]string, len(context.Normal))
	for index, prerequisite := range context.Normal {
		normal[index] = prerequisite.Target
	}
	orderOnly := make([]string, len(context.OrderOnly))
	for index, prerequisite := range context.OrderOnly {
		orderOnly[index] = prerequisite.Target
	}
	stem := context.Stem
	if reconstructed && stem == reconstructedStem &&
		kbuildSelectionPrerequisiteTargetsEqual(normal, reconstructedNormal) &&
		kbuildSelectionPrerequisiteTargetsEqual(orderOnly, reconstructedOrderOnly) {
		return reconstructedNormal, reconstructedOrderOnly, stem, resolved, nil
	}
	return canonicalKbuildSelectionPrerequisites(normal), canonicalKbuildSelectionPrerequisites(orderOnly), stem, resolved, nil
}

func kbuildSelectionPrerequisiteTargetsEqual(
	targets []string,
	prerequisites []kbuildSelectionPrerequisite,
) bool {
	if len(targets) != len(prerequisites) {
		return false
	}
	for index, target := range targets {
		if kconfig.CanonicalKbuildGraphTarget(target) != prerequisites[index].target {
			return false
		}
	}
	return true
}

// reconstructSelectedKbuildRuleContextForMakeTarget mirrors GNU Make's
// selected-recipe-first prerequisite merge from the immutable indexed rules.
// It is deliberately narrow: independent double-colon recipes fall back to the
// evaluator path because they do not have one shared automatic-variable stem.
func reconstructSelectedKbuildRuleContextForMakeTarget(
	profile kconfig.CompactKbuildProfile,
	index *kbuildProfileTargetIndex,
	target, makeTarget string,
	ruleIndexes, effectiveRecipeIndexes []int,
) ([]kbuildSelectionPrerequisite, []kbuildSelectionPrerequisite, string, bool, error) {
	if len(effectiveRecipeIndexes) != 1 {
		return nil, nil, "", false, nil
	}
	effectiveRuleIndex := effectiveRecipeIndexes[0]
	selectedRules := map[int]bool{}
	for _, ruleIndex := range ruleIndexes {
		selectedRules[ruleIndex] = true
	}
	matches := index.ruleMatches(profile, target, makeTarget)
	selected := kbuildProfileRuleCandidate{}
	selectedFound := false
	for _, candidates := range [][]kbuildProfileRuleCandidate{matches.explicit, matches.implicit} {
		for _, candidate := range candidates {
			if candidate.index == effectiveRuleIndex && selectedRules[candidate.index] {
				selected = candidate
				selectedFound = true
				break
			}
		}
		if selectedFound {
			break
		}
	}
	if !selectedFound {
		return nil, nil, "", false, nil
	}

	contextRules := []kbuildProfileRuleCandidate{selected}
	selectedRule := profile.Rules[selected.index]
	if selectedRule.Separator != "::" {
		for _, candidate := range matches.explicit {
			if candidate.index == selected.index || !selectedRules[candidate.index] {
				continue
			}
			if profile.Rules[candidate.index].Separator == "::" {
				return nil, nil, "", false, fmt.Errorf(
					"target %q mixes independent double-colon and merged rule contexts", target,
				)
			}
			contextRules = append(contextRules, candidate)
		}
	}

	expand := func(orderOnly bool) []kbuildSelectionPrerequisite {
		prerequisites := []kbuildSelectionPrerequisite{}
		for _, candidate := range contextRules {
			values := profile.Rules[candidate.index].Prerequisites
			if orderOnly {
				values = profile.Rules[candidate.index].OrderOnly
			}
			for _, value := range values {
				makeWord := value
				if strings.Count(makeWord, "%") == 1 {
					makeWord = strings.Replace(makeWord, "%", candidate.stem, 1)
				}
				graphPath := kbuildProfileTarget(profile, makeWord)
				if graphPath == "" {
					continue
				}
				prerequisites = append(prerequisites, kbuildSelectionPrerequisite{
					target: graphPath, makeTarget: filepath.ToSlash(strings.TrimSpace(makeWord)),
				})
			}
		}
		return prerequisites
	}
	normal := expand(false)
	orderOnly := expand(true)
	if len(orderOnly) != 0 {
		normalTargets := make(map[string]bool, len(normal))
		for _, prerequisite := range normal {
			normalTargets[prerequisite.target] = true
		}
		filtered := orderOnly[:0]
		for _, prerequisite := range orderOnly {
			if !normalTargets[prerequisite.target] {
				filtered = append(filtered, prerequisite)
			}
		}
		orderOnly = filtered
	}
	return normal, orderOnly, selected.stem, true, nil
}

func (index *kbuildProfileTargetIndex) targetIsPhony(profile kconfig.CompactKbuildProfile, target string) bool {
	return index.phony[kconfig.CanonicalKbuildGraphTarget(target)]
}

// kbuildProfileTargetSatisfaction distinguishes immutable source/config inputs
// from source-path collisions which GNU Make must still update. Only normal
// prerequisites affect currentness: an order-only FORCE may run, but does not
// make its parent recipe run. The memo also makes source prerequisite cycles
// match GNU Make's dropped-cycle behavior without unbounded recursion.
type kbuildProfileTargetSatisfaction struct {
	profile   kconfig.CompactKbuildProfile
	index     *kbuildProfileTargetIndex
	satisfied map[string]bool
	state     map[string]uint8
	current   map[string]bool
}

func newKbuildProfileTargetSatisfaction(
	profile kconfig.CompactKbuildProfile,
	index *kbuildProfileTargetIndex,
	satisfied map[string]bool,
) *kbuildProfileTargetSatisfaction {
	return &kbuildProfileTargetSatisfaction{
		profile: profile, index: index, satisfied: satisfied,
		state: map[string]uint8{}, current: map[string]bool{},
	}
}

func (resolver *kbuildProfileTargetSatisfaction) targetIsSatisfied(target string) bool {
	target = kconfig.CanonicalKbuildGraphTarget(target)
	if target == "" || !resolver.satisfied[target] || resolver.index.targetIsPhony(resolver.profile, target) {
		return false
	}
	switch resolver.state[target] {
	case 1:
		// GNU Make drops a circular prerequisite edge. It cannot, by itself,
		// turn an immutable source target into an action trigger.
		return true
	case 2:
		return resolver.current[target]
	}
	resolver.state[target] = 1
	current := true
	for _, ruleIndex := range kbuildProfileRuleIndexesForTargetIndexed(
		resolver.profile, resolver.index, target, resolver.satisfied,
	) {
		if ruleIndex < 0 || ruleIndex >= len(resolver.profile.Rules) {
			continue
		}
		rule := resolver.profile.Rules[ruleIndex]
		if rule.Separator == "::" && len(rule.Prerequisites) == 0 && len(rule.Recipe) != 0 {
			current = false
			break
		}
		stem := kbuildProfileRuleStem(resolver.profile, rule, target)
		for _, rawPrerequisite := range rule.Prerequisites {
			prerequisite := strings.Replace(rawPrerequisite, "%", stem, 1)
			prerequisite = kbuildProfileTarget(resolver.profile, prerequisite)
			if prerequisite == "FORCE" || resolver.index.targetIsPhony(resolver.profile, prerequisite) ||
				!resolver.targetIsSatisfied(prerequisite) {
				current = false
				break
			}
		}
		if !current {
			break
		}
	}
	resolver.state[target] = 2
	resolver.current[target] = current
	return current
}

func (index *kbuildProfileTargetIndex) generatedKind(target string) string {
	return index.generated[target]
}

func kbuildProfileRulesForTargetIndexed(
	profile kconfig.CompactKbuildProfile,
	index *kbuildProfileTargetIndex,
	target string,
	satisfied map[string]bool,
) []kconfig.KbuildRule {
	indexes := kbuildProfileRuleIndexesForTargetIndexed(profile, index, target, satisfied)
	rules := make([]kconfig.KbuildRule, 0, len(indexes))
	for _, ruleIndex := range indexes {
		rules = append(rules, profile.Rules[ruleIndex])
	}
	return rules
}

func kbuildProfileRuleIndexesForTargetIndexed(
	profile kconfig.CompactKbuildProfile,
	index *kbuildProfileTargetIndex,
	target string,
	satisfied map[string]bool,
) []int {
	return kbuildProfileRuleIndexesForMakeTargetIndexed(profile, index, target, target, satisfied)
}

func kbuildProfileRuleIndexesForMakeTargetIndexed(
	profile kconfig.CompactKbuildProfile,
	index *kbuildProfileTargetIndex,
	target, makeTarget string,
	satisfied map[string]bool,
) []int {
	target = kconfig.CanonicalKbuildGraphTarget(target)
	makeTarget = kbuildProfileLookupTarget(profile, target, makeTarget)
	matches := index.ruleMatches(profile, target, makeTarget)
	explicit := make([]int, 0, len(matches.explicit))
	for _, candidate := range matches.explicit {
		explicit = append(explicit, candidate.index)
	}
	if index.targetIsPhony(profile, target) || kbuildRuleIndexesHaveRecipe(profile, explicit) {
		return explicit
	}
	for _, candidate := range matches.implicit {
		if kbuildProfileImplicitRuleViableIndexed(
			profile, index, target, candidate, satisfied,
			map[string]bool{kbuildProfileRuleLookupStackKey(target, makeTarget): true}, map[int]bool{},
		) {
			return append(explicit, candidate.index)
		}
	}
	// Kbuild's generated-target collections are an explicit promise that the
	// target participates in this invocation. Its implicit prerequisite may be
	// a file emitted as a side effect by an ordered earlier invocation and thus
	// cannot exist during selection discovery. Retain GNU Make's first implicit
	// candidate in GNU Make's shortest-stem order so graph lowering can prove
	// and bind that side-output edge. This deliberately matches the canonical
	// production resolver in CompactMetadata.compactKbuildRuleForProfile.
	if index.generatedKind(target) != "" && len(profile.InvocationPredecessors) != 0 && len(matches.implicit) != 0 {
		return append(explicit, matches.implicit[0].index)
	}
	return explicit
}

func kbuildRuleIndexesHaveRecipe(profile kconfig.CompactKbuildProfile, indexes []int) bool {
	for _, index := range indexes {
		if index >= 0 && index < len(profile.Rules) && len(profile.Rules[index].Recipe) != 0 {
			return true
		}
	}
	return false
}

func kbuildProfileImplicitRuleViableIndexed(
	profile kconfig.CompactKbuildProfile,
	index *kbuildProfileTargetIndex,
	target string,
	candidate kbuildProfileRuleCandidate,
	satisfied map[string]bool,
	stack map[string]bool,
	activeRules map[int]bool,
) bool {
	if activeRules[candidate.index] {
		return false
	}
	activeRules[candidate.index] = true
	defer delete(activeRules, candidate.index)
	rule := profile.Rules[candidate.index]
	for _, prerequisites := range [][]string{rule.Prerequisites, rule.OrderOnly} {
		for _, rawPrerequisite := range prerequisites {
			makePrerequisite := strings.Replace(rawPrerequisite, "%", candidate.stem, 1)
			prerequisite := kbuildProfileTarget(profile, makePrerequisite)
			if prerequisite == "" || prerequisite == "FORCE" || prerequisite == target || satisfied[prerequisite] {
				continue
			}
			if !kbuildProfileTargetCanBeMadeIndexedForMakeTarget(
				profile, index, prerequisite, makePrerequisite, satisfied, stack, activeRules,
			) {
				return false
			}
		}
	}
	return true
}

func kbuildProfileTargetCanBeMadeIndexed(
	profile kconfig.CompactKbuildProfile,
	index *kbuildProfileTargetIndex,
	target string,
	satisfied map[string]bool,
	stack map[string]bool,
	activeRules map[int]bool,
) bool {
	return kbuildProfileTargetCanBeMadeIndexedForMakeTarget(
		profile, index, target, target, satisfied, stack, activeRules,
	)
}

func kbuildProfileTargetCanBeMadeIndexedForMakeTarget(
	profile kconfig.CompactKbuildProfile,
	index *kbuildProfileTargetIndex,
	target, makeTarget string,
	satisfied map[string]bool,
	stack map[string]bool,
	activeRules map[int]bool,
) bool {
	target = kconfig.CanonicalKbuildGraphTarget(target)
	makeTarget = kbuildProfileLookupTarget(profile, target, makeTarget)
	if target == "" || target == "FORCE" || satisfied[target] || index.hasInitialVisibleArtifact(target) {
		return true
	}
	stackKey := kbuildProfileRuleLookupStackKey(target, makeTarget)
	if stack[stackKey] {
		return false
	}
	stack[stackKey] = true
	defer delete(stack, stackKey)

	matches := index.ruleMatches(profile, target, makeTarget)
	if len(matches.explicit) != 0 {
		// GNU Make's implicit-rule search treats an explicitly mentioned
		// target as a file that ought to exist. The active implicit-rule
		// exclusion does not flow through that boundary; the explicit target's
		// own prerequisite closure is selected and validated when it is walked.
		return true
	}
	for _, candidate := range matches.implicit {
		if kbuildProfileImplicitRuleViableIndexed(profile, index, target, candidate, satisfied, stack, activeRules) {
			return true
		}
	}
	return false
}

func kbuildProfileRuleLookupStackKey(target, makeTarget string) string {
	return target + "\x00" + makeTarget
}

func kbuildProfileRuleStem(profile kconfig.CompactKbuildProfile, rule kconfig.KbuildRule, target string) string {
	if rule.TargetPattern != "" {
		stem, _ := matchMakeTargetPattern(kbuildProfileTarget(profile, rule.TargetPattern), target)
		return stem
	}
	for _, pattern := range rule.Targets {
		if stem, ok := matchMakeTargetPattern(kbuildProfileTarget(profile, pattern), target); ok {
			return stem
		}
	}
	return ""
}

// selectedKbuildGroupedRuleOutputs returns every concrete logical output of
// the effective recipe selected for target. GNU &: rules are always grouped;
// GNU Make also treats a multi-target implicit pattern rule as one historical
// grouped invocation. Prerequisite-only declarations may surround the selected
// recipe, so the last recipe-bearing rule index is the effective explicit
// override or the one viable implicit candidate appended by the target index.
func selectedKbuildGroupedRuleOutputs(
	profile kconfig.CompactKbuildProfile,
	ruleIndexes []int,
	target, stem string,
) (int, []string, bool, error) {
	return kconfig.ResolveCompactKbuildGroupedRule(profile, ruleIndexes, target, stem)
}

func kbuildProfileRuleAutomaticTarget(
	profile kconfig.CompactKbuildProfile,
	rule kconfig.KbuildRule,
	target, stem string,
) string {
	for _, declared := range rule.Targets {
		candidate := strings.Replace(declared, "%", stem, 1)
		if kbuildProfileTarget(profile, candidate) == target {
			return candidate
		}
	}
	return target
}

func kbuildRecipeHasAction(recipe string) bool {
	trimmed := strings.TrimSpace(strings.TrimLeft(recipe, "+@-"))
	return trimmed != "" && trimmed != ":" && trimmed != "true"
}

// kbuildRecipeOnlyCreatesDirectories recognizes a recipe whose complete
// filesystem effect is parent-directory setup for a later action. Bazel
// creates declared output parents itself, so this line does not materialize
// the Make target. Recursive Make discovery still follows the sibling recipe
// that owns the actual output graph.
func kbuildRecipeOnlyCreatesDirectories(recipe string) bool {
	trimmed := strings.TrimSpace(strings.TrimLeft(recipe, "+@-"))
	for _, prefix := range []string{"$(Q)", "${Q}"} {
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
	}
	command, ok, err := kbuildSingleShellSimpleCommand(trimmed)
	if err != nil || !ok || len(command.redirections) != 0 {
		return false
	}
	return kbuildRecipeWordsOnlyCreateDirectories(command.argv)
}

func kbuildRecipeWordsOnlyCreateDirectories(words []string) bool {
	if len(words) < 3 || strings.TrimLeft(words[0], "+@-") != "mkdir" {
		return false
	}
	parents := false
	operands := 0
	for _, word := range words[1:] {
		if strings.HasPrefix(word, "-") {
			if word == "--parents" || strings.Contains(strings.TrimPrefix(word, "-"), "p") {
				parents = true
			}
			continue
		}
		operands++
	}
	return parents && operands != 0
}

// kbuildInvocationDefaultGoal resolves the selected default goal from the
// parsed invocation itself. GNU Make exposes an explicit .DEFAULT_GOAL as a
// variable; without one, its default is the first ordinary target declared by
// the evaluated makefile graph.
func kbuildInvocationDefaultGoal(profile kconfig.CompactKbuildProfile) (string, error) {
	values, err := kconfig.EvaluateCompactKbuildTarget(profile, "", "", nil, nil, nil, ".DEFAULT_GOAL")
	if err != nil {
		return "", err
	}
	if goal := kbuildProfileTarget(profile, values[".DEFAULT_GOAL"]); goal != "" {
		return goal, nil
	}
	for _, rule := range profile.Rules {
		for _, rawTarget := range rule.Targets {
			target := strings.TrimSpace(rawTarget)
			if target == "" || target == "FORCE" || strings.ContainsAny(target, "$%") {
				continue
			}
			// GNU Make excludes an ordinary dot-prefixed filename from default
			// goal selection, but a target containing a slash is eligible. Linux
			// 6.18 relies on this distinction: scripts/Makefile.build declares
			// `$(obj)/` first, which is `./` for the root build directory.
			if strings.HasPrefix(target, ".") && !strings.Contains(target, "/") {
				continue
			}
			if target = kbuildProfileTarget(profile, target); target != "" {
				return target, nil
			}
		}
	}
	return "", nil
}

type kbuildRecursiveMakePlanEntry struct {
	key             string
	request         kbuildInvocationRequest
	predecessors    []string
	consumers       []string
	frontier        *kbuildRecursiveMakeFrontier
	replayArguments []string
	control         *kconfig.KbuildControlEvaluation
}

// kbuildRecursiveMakeFrontierEvent is one source-ordered mutation of the
// object-tree view visible to a later recursive Make invocation. An artifact
// event is a local write by this profile; an invocation event applies the
// completed frontier of an earlier child invocation. Exactly one field is set.
type kbuildRecursiveMakeFrontierEvent struct {
	artifact   kconfig.CompactKbuildVisibleArtifact
	invocation string
	// command is the exact source-selected recipe segment which materializes
	// artifact. It remains symbolic until replay, then a deliberately small
	// shell projection may prove its bytes without executing the recipe.
	command       string
	commandTarget string
}

// kbuildRecursiveMakeFrontier is one immutable node in the causal object-tree
// history of a Make invocation. An event node sequences one local write or
// recursive child after its sole parent. A join node has multiple incomparable
// prerequisite parents: all of them complete before the consuming recipe, but
// none is made visible to another merely because the traversal visited it
// first. Nodes are interned by their structural digest within one profile plan.
type kbuildRecursiveMakeFrontier struct {
	id       string
	parents  []*kbuildRecursiveMakeFrontier
	event    kbuildRecursiveMakeFrontierEvent
	hasEvent bool
}

type kbuildRecursiveMakeFrontierBuilder struct {
	nodes map[string]*kbuildRecursiveMakeFrontier
}

func newKbuildRecursiveMakeFrontierBuilder() *kbuildRecursiveMakeFrontierBuilder {
	return &kbuildRecursiveMakeFrontierBuilder{
		nodes: map[string]*kbuildRecursiveMakeFrontier{},
	}
}

func kbuildRecursiveMakeFrontierID(frontier *kbuildRecursiveMakeFrontier) string {
	if frontier == nil {
		return "base"
	}
	return frontier.id
}

func kbuildRecursiveMakeFrontierEventIdentity(event kbuildRecursiveMakeFrontierEvent) string {
	if event.invocation != "" {
		return "invocation\x1f" + canonicalKbuildToolsetPathCapabilityIdentity(event.invocation)
	}
	return strings.Join([]string{
		"artifact",
		canonicalKbuildToolsetPathCapabilityIdentity(event.artifact.Path),
		canonicalKbuildToolsetPathCapabilityIdentity(event.artifact.Profile),
		canonicalKbuildToolsetPathCapabilityIdentity(event.artifact.Target),
		canonicalKbuildToolsetPathCapabilityIdentity(event.commandTarget),
		canonicalKbuildToolsetPathCapabilityIdentity(event.command),
	}, "\x1f")
}

func (builder *kbuildRecursiveMakeFrontierBuilder) sequence(
	parent *kbuildRecursiveMakeFrontier,
	event kbuildRecursiveMakeFrontierEvent,
) *kbuildRecursiveMakeFrontier {
	hash := sha256.New()
	_, _ = hash.Write([]byte("sequence\x00" + kbuildRecursiveMakeFrontierID(parent) + "\x00" + kbuildRecursiveMakeFrontierEventIdentity(event)))
	id := hex.EncodeToString(hash.Sum(nil))
	if existing := builder.nodes[id]; existing != nil {
		return existing
	}
	node := &kbuildRecursiveMakeFrontier{
		id: id, parents: []*kbuildRecursiveMakeFrontier{parent}, event: event, hasEvent: true,
	}
	builder.nodes[id] = node
	return node
}

func kbuildRecursiveMakeFrontierDescendsFrom(
	frontier, ancestor *kbuildRecursiveMakeFrontier,
	memo map[string]bool,
) bool {
	if ancestor == nil {
		return true
	}
	if frontier == nil {
		return false
	}
	if frontier == ancestor || frontier.id == ancestor.id {
		return true
	}
	key := frontier.id + "\x00" + ancestor.id
	if result, ok := memo[key]; ok {
		return result
	}
	memo[key] = false
	for _, parent := range frontier.parents {
		if kbuildRecursiveMakeFrontierDescendsFrom(parent, ancestor, memo) {
			memo[key] = true
			return true
		}
	}
	return false
}

func (builder *kbuildRecursiveMakeFrontierBuilder) join(
	frontiers ...*kbuildRecursiveMakeFrontier,
) *kbuildRecursiveMakeFrontier {
	unique := map[string]*kbuildRecursiveMakeFrontier{}
	expandedJoins := map[string]bool{}
	var add func(*kbuildRecursiveMakeFrontier)
	add = func(frontier *kbuildRecursiveMakeFrontier) {
		if frontier == nil {
			return
		}
		if !frontier.hasEvent {
			if expandedJoins[frontier.id] {
				return
			}
			expandedJoins[frontier.id] = true
			for _, parent := range frontier.parents {
				add(parent)
			}
			return
		}
		unique[frontier.id] = frontier
	}
	for _, frontier := range frontiers {
		add(frontier)
	}
	// Traverse the causal DAG once from every candidate's parents. Every
	// candidate reached this way is a strict ancestor of another candidate and
	// therefore cannot be maximal. A global visited set is sufficient because a
	// frontier node's ancestry is immutable.
	shadowed := map[string]bool{}
	visited := map[string]bool{}
	for _, candidate := range unique {
		pending := append([]*kbuildRecursiveMakeFrontier(nil), candidate.parents...)
		for len(pending) > 0 {
			last := len(pending) - 1
			ancestor := pending[last]
			pending = pending[:last]
			if ancestor == nil || visited[ancestor.id] {
				continue
			}
			visited[ancestor.id] = true
			if unique[ancestor.id] != nil {
				shadowed[ancestor.id] = true
			}
			pending = append(pending, ancestor.parents...)
		}
	}
	maximal := make([]*kbuildRecursiveMakeFrontier, 0, len(unique))
	for _, candidate := range unique {
		if !shadowed[candidate.id] {
			maximal = append(maximal, candidate)
		}
	}
	if len(maximal) == 0 {
		return nil
	}
	if len(maximal) == 1 {
		return maximal[0]
	}
	sort.Slice(maximal, func(i, j int) bool { return maximal[i].id < maximal[j].id })
	hash := sha256.New()
	_, _ = hash.Write([]byte("join"))
	for _, parent := range maximal {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(parent.id))
	}
	id := hex.EncodeToString(hash.Sum(nil))
	if existing := builder.nodes[id]; existing != nil {
		return existing
	}
	node := &kbuildRecursiveMakeFrontier{id: id, parents: maximal}
	builder.nodes[id] = node
	return node
}

func kbuildRecursiveMakePlanKey(
	request kbuildInvocationRequest,
	frontier *kbuildRecursiveMakeFrontier,
) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(kbuildInvocationRequestKey(request)))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(kbuildRecursiveMakeFrontierID(frontier)))
	return hex.EncodeToString(hash.Sum(nil))
}

// selectedKbuildRecursiveMakePlan walks only the prerequisite closure of this
// invocation's selected entry goals in Make execution order. A child invoked
// by a recipe starts after every selected prerequisite recipe, and successive
// recursive recipe lines form a chain. The predecessor request keys preserve
// that source-owned ordering after invocation profiles are sorted for stable
// serialization.
func selectedKbuildRecursiveMakePlan(profile kconfig.CompactKbuildProfile, satisfied map[string]bool) ([]kbuildRecursiveMakePlanEntry, error) {
	return selectedKbuildRecursiveMakePlanWithControl(profile, satisfied, nil)
}

func selectedKbuildRecursiveMakePlanWithControl(
	profile kconfig.CompactKbuildProfile,
	satisfied map[string]bool,
	control *kconfig.KbuildControlEvaluation,
) ([]kbuildRecursiveMakePlanEntry, error) {
	return selectedKbuildRecursiveMakePlanWithControlAndCompletion(profile, satisfied, control, nil)
}

// selectedKbuildRecursiveMakePlanWithControlAndCompletion additionally returns
// the causal object-tree frontier at invocation completion. Independent Make
// prerequisites branch from the same initial state and join only before their
// consuming recipe; successive effects within a recipe remain sequenced.
// Production discovery replays that immutable frontier after recursively
// completing each child so later Make processes can observe both generated-file
// existence and exact source-derived bytes. The older wrapper remains the
// focused-test seam.
func selectedKbuildRecursiveMakePlanWithControlAndCompletion(
	profile kconfig.CompactKbuildProfile,
	satisfied map[string]bool,
	control *kconfig.KbuildControlEvaluation,
	completion **kbuildRecursiveMakeFrontier,
) ([]kbuildRecursiveMakePlanEntry, error) {
	return selectedKbuildRecursiveMakePlanWithResolvedTargets(profile, satisfied, control, completion, nil)
}

func selectedKbuildRecursiveMakePlanWithResolvedTargets(
	profile kconfig.CompactKbuildProfile,
	satisfied map[string]bool,
	control *kconfig.KbuildControlEvaluation,
	completion **kbuildRecursiveMakeFrontier,
	resolvedTargets *kbuildResolvedTargetCache,
) ([]kbuildRecursiveMakePlanEntry, error) {
	plan := []kbuildRecursiveMakePlanEntry{}
	planByRequest := map[string]int{}
	replayByRequest := map[string][]string{}
	frontiers := newKbuildRecursiveMakeFrontierBuilder()
	targetState := map[string]uint8{}
	type traversalResult struct {
		terminals []string
		frontier  *kbuildRecursiveMakeFrontier
	}
	targetResults := map[string]traversalResult{}
	ruleIndex := newKbuildProfileTargetIndexWithResolvedTargets(profile, nil, resolvedTargets)
	currentness := newKbuildProfileTargetSatisfaction(profile, ruleIndex, satisfied)

	appendUnique := func(values []string, candidates ...string) []string {
		for _, candidate := range candidates {
			if candidate != "" && !slices.Contains(values, candidate) {
				values = append(values, candidate)
			}
		}
		return values
	}
	var requestDependsOn func(request, predecessor string) bool
	requestDependsOn = func(request, predecessor string) bool {
		index, ok := planByRequest[request]
		if !ok {
			return false
		}
		for _, direct := range plan[index].predecessors {
			if direct == predecessor || requestDependsOn(direct, predecessor) {
				return true
			}
		}
		return false
	}
	reduceTerminals := func(values []string) []string {
		out := []string{}
		for _, candidate := range values {
			shadowed := false
			for _, later := range values {
				if later != candidate && requestDependsOn(later, candidate) {
					shadowed = true
					break
				}
			}
			if !shadowed {
				out = appendUnique(out, candidate)
			}
		}
		return out
	}
	recordInvocation := func(
		invocation kbuildRecursiveMakeInvocation,
		consumer string,
		terminals []string,
		frontier *kbuildRecursiveMakeFrontier,
		recipeControl *kconfig.KbuildControlEvaluation,
	) ([]string, *kbuildRecursiveMakeFrontier, error) {
		request := invocation.request
		// Visibility belongs to the causal frontier. DFS replay populates the
		// effective request only after every referenced child has completed.
		request.visibleState = kbuildFrontierState{}
		requestKey := kbuildInvocationRequestKey(request)
		if replay, exists := replayByRequest[requestKey]; exists {
			if !slices.Equal(replay, invocation.replayArguments) {
				return nil, nil, fmt.Errorf(
					"selected recursive Make invocation %q has inconsistent replay argv %q and %q",
					request.name, replay, invocation.replayArguments,
				)
			}
		} else {
			replayByRequest[requestKey] = append([]string(nil), invocation.replayArguments...)
		}
		key := kbuildRecursiveMakePlanKey(request, frontier)
		filteredPredecessors := []string{}
		for _, predecessor := range terminals {
			if predecessor != key && !requestDependsOn(predecessor, key) {
				filteredPredecessors = appendUnique(filteredPredecessors, predecessor)
			}
		}
		if index, exists := planByRequest[key]; exists {
			if !slices.Equal(plan[index].replayArguments, invocation.replayArguments) {
				return nil, nil, fmt.Errorf(
					"selected recursive Make invocation %q has inconsistent replay argv %q and %q",
					request.name, plan[index].replayArguments, invocation.replayArguments,
				)
			}
			plan[index].predecessors = appendUnique(plan[index].predecessors, filteredPredecessors...)
			plan[index].consumers = appendUnique(plan[index].consumers, consumer)
		} else {
			planByRequest[key] = len(plan)
			plan = append(plan, kbuildRecursiveMakePlanEntry{
				key: key, request: request, predecessors: filteredPredecessors, consumers: []string{consumer},
				frontier:        frontier,
				replayArguments: append([]string(nil), invocation.replayArguments...),
				control:         recipeControl,
			})
		}
		frontier = frontiers.sequence(frontier, kbuildRecursiveMakeFrontierEvent{invocation: key})
		return []string{key}, frontier, nil
	}

	var visit func(kbuildSelectionPrerequisite, string, bool) (traversalResult, error)
	visit = func(input kbuildSelectionPrerequisite, origin string, includeSatisfied bool) (traversalResult, error) {
		target := kconfig.CanonicalKbuildGraphTarget(input.target)
		makeTarget := kbuildProfileLookupTarget(profile, target, input.makeTarget)
		if err := validateKbuildTraversalTarget(profile.Name, origin, target); err != nil {
			return traversalResult{}, err
		}
		if target == "" || target == "FORCE" ||
			currentness.targetIsSatisfied(target) && !includeSatisfied {
			return traversalResult{}, nil
		}
		if _, _, trigger, _, grouped := kconfig.CompactKbuildGroupedActionForTarget(profile, target); grouped && target != trigger {
			result, err := visit(kbuildSelectionPrerequisite{
				target: trigger, makeTarget: trigger,
			}, "grouped trigger for "+target, true)
			if err != nil {
				return traversalResult{}, err
			}
			targetState[target] = 2
			targetResults[target] = result
			return result, nil
		}
		switch targetState[target] {
		case 1:
			// GNU Make drops a circular prerequisite edge. Do the same for
			// ordering discovery; ordinary rule lowering reports real cycles.
			return traversalResult{}, nil
		case 2:
			result := targetResults[target]
			result.terminals = append([]string(nil), result.terminals...)
			return result, nil
		}
		targetState[target] = 1
		ruleIndexes := kbuildProfileRuleIndexesForMakeTargetIndexed(
			profile, ruleIndex, target, makeTarget, satisfied,
		)
		effectiveRecipeIndexes, effectiveRecipeErr := kconfig.EffectiveCompactKbuildRecipeRuleIndexes(profile, ruleIndexes)
		if effectiveRecipeErr != nil {
			return traversalResult{}, fmt.Errorf("select effective recipes for target %s: %w", target, effectiveRecipeErr)
		}
		normal := []kbuildSelectionPrerequisite{}
		orderOnly := []kbuildSelectionPrerequisite{}
		stem := ""
		if len(ruleIndexes) != 0 {
			var contextErr error
			normal, orderOnly, stem, _, contextErr = evaluateSelectedKbuildRuleContextForMakeTarget(
				profile, ruleIndex, target, makeTarget, ruleIndexes, effectiveRecipeIndexes,
			)
			if contextErr != nil {
				return traversalResult{}, fmt.Errorf("evaluate selected target %s prerequisite context: %w", target, contextErr)
			}
		}
		groupRuleIndex, groupOutputs, grouped, groupErr := kconfig.ResolveCompactKbuildGroupedRule(
			profile, ruleIndexes, target, stem,
		)
		if groupErr != nil {
			return traversalResult{}, fmt.Errorf("resolve selected target %s grouped outputs: %w", target, groupErr)
		}
		if grouped {
			trigger := target
			if _, _, recordedTrigger, recordedOutputs, exists := kconfig.CompactKbuildGroupedActionForTarget(profile, target); exists {
				trigger = recordedTrigger
				groupOutputs = recordedOutputs
			}
			if err := kconfig.BindCompactKbuildGroupedAction(&profile, groupRuleIndex, stem, trigger, groupOutputs); err != nil {
				return traversalResult{}, err
			}
			if target != trigger {
				targetState[target] = 0
				result, err := visit(kbuildSelectionPrerequisite{
					target: trigger, makeTarget: trigger,
				}, "grouped trigger for "+target, true)
				if err != nil {
					return traversalResult{}, err
				}
				targetState[target] = 2
				targetResults[target] = result
				return result, nil
			}
		}
		predecessors := []string{}
		prerequisiteFrontiers := []*kbuildRecursiveMakeFrontier{}
		for _, prerequisite := range append(append([]kbuildSelectionPrerequisite(nil), normal...), orderOnly...) {
			result, err := visit(prerequisite, "prerequisite of "+target, false)
			if err != nil {
				return traversalResult{}, err
			}
			predecessors = appendUnique(predecessors, result.terminals...)
			prerequisiteFrontiers = append(prerequisiteFrontiers, result.frontier)
		}
		for _, peer := range groupOutputs {
			if peer == target {
				continue
			}
			peerMakeTarget := kbuildProfileRuleAutomaticTarget(
				profile, profile.Rules[groupRuleIndex], peer, stem,
			)
			peerRuleIndexes := kbuildProfileRuleIndexesForMakeTargetIndexed(
				profile, ruleIndex, peer, peerMakeTarget, satisfied,
			)
			peerEffectiveRecipeIndexes, peerEffectiveRecipeErr := kconfig.EffectiveCompactKbuildRecipeRuleIndexes(
				profile, peerRuleIndexes,
			)
			if peerEffectiveRecipeErr != nil {
				return traversalResult{}, peerEffectiveRecipeErr
			}
			peerNormal, peerOrderOnly, peerStem, _, contextErr := evaluateSelectedKbuildRuleContextForMakeTarget(
				profile, ruleIndex, peer, peerMakeTarget, peerRuleIndexes, peerEffectiveRecipeIndexes,
			)
			if contextErr != nil {
				return traversalResult{}, fmt.Errorf("evaluate grouped peer %s prerequisite context: %w", peer, contextErr)
			}
			peerRuleIndex, peerOutputs, peerGrouped, peerErr := kconfig.ResolveCompactKbuildGroupedRule(
				profile, peerRuleIndexes, peer, peerStem,
			)
			if peerErr != nil {
				return traversalResult{}, peerErr
			}
			if !peerGrouped || peerRuleIndex != groupRuleIndex || peerStem != stem {
				return traversalResult{}, fmt.Errorf("grouped Kbuild peer %q resolves to a different effective recipe than trigger %q", peer, target)
			}
			if err := kconfig.BindCompactKbuildGroupedAction(&profile, peerRuleIndex, peerStem, target, peerOutputs); err != nil {
				return traversalResult{}, err
			}
			for _, prerequisite := range append(append([]kbuildSelectionPrerequisite(nil), peerNormal...), peerOrderOnly...) {
				result, err := visit(prerequisite, "prerequisite of grouped peer "+peer, false)
				if err != nil {
					return traversalResult{}, err
				}
				predecessors = appendUnique(predecessors, result.terminals...)
				prerequisiteFrontiers = append(prerequisiteFrontiers, result.frontier)
			}
		}

		terminals := reduceTerminals(predecessors)
		frontier := frontiers.join(prerequisiteFrontiers...)
		for _, selectedRuleIndex := range effectiveRecipeIndexes {
			rule := profile.Rules[selectedRuleIndex]
			automaticTarget := kbuildProfileRuleAutomaticTarget(profile, rule, target, stem)
			for recipeIndex, recipe := range rule.Recipe {
				recipeProfile := profile
				var recipeControl *kconfig.KbuildControlEvaluation
				if control != nil {
					snapshot, ok := kconfig.KbuildControlEvaluationBeforeRecipeIndex(
						*control, target, selectedRuleIndex, recipeIndex,
					)
					if !ok {
						return traversalResult{}, fmt.Errorf(
							"selected recursive Make planning has no control-state snapshot for %s target %q at %s recipe %d",
							profile.Name, target, rule.Position, recipeIndex,
						)
					}
					recipeProfile = snapshot.Profile
					recipeControl = &snapshot
				}
				effects, err := kbuildSelectedRecipeExecutionEffects(
					recipeProfile, rule, target, makeTarget, automaticTarget, stem,
					kbuildSelectionPrerequisiteMakeTargets(normal),
					kbuildSelectionPrerequisiteMakeTargets(orderOnly), recipe,
				)
				if err != nil {
					return traversalResult{}, fmt.Errorf("interpret selected recipe for %s target %q: %w", profile.Name, target, err)
				}
				for _, effect := range effects {
					if effect.recursive {
						terminals, frontier, err = recordInvocation(
							effect.invocation, target, terminals, frontier, recipeControl,
						)
						if err != nil {
							return traversalResult{}, err
						}
						continue
					}
					if effect.materializesTarget && !ruleIndex.targetIsPhony(profile, target) && target != "." && !strings.HasSuffix(target, "/") {
						outputs := groupOutputs
						if len(outputs) == 0 {
							outputs = []string{target}
						}
						for _, output := range outputs {
							commandTarget := ""
							if len(groupOutputs) != 0 {
								commandTarget = target
							}
							artifact := kconfig.CompactKbuildVisibleArtifact{
								Path: output, Profile: profile.Name, Target: output,
							}
							frontier = frontiers.sequence(frontier, kbuildRecursiveMakeFrontierEvent{
								artifact: artifact, command: effect.command, commandTarget: commandTarget,
							})
						}
					}
				}
			}
		}
		if target != "" && target != "." {
			for _, terminal := range terminals {
				if index, exists := planByRequest[terminal]; exists {
					plan[index].consumers = appendUnique(plan[index].consumers, target)
				}
			}
		}
		targetState[target] = 2
		result := traversalResult{
			terminals: append([]string(nil), terminals...),
			frontier:  frontier,
		}
		targetResults[target] = result
		for _, peer := range groupOutputs {
			targetState[peer] = 2
			targetResults[peer] = result
		}
		return result, nil
	}

	completionFrontiers := []*kbuildRecursiveMakeFrontier{}
	for _, target := range profile.EntryTargets {
		result, err := visit(kbuildSelectionPrerequisite{
			target: target, makeTarget: target,
		}, "selected goal", false)
		if err != nil {
			return nil, err
		}
		completionFrontiers = append(completionFrontiers, result.frontier)
	}
	if completion != nil {
		*completion = frontiers.join(completionFrontiers...)
	}
	return plan, nil
}

// selectedKbuildRecursiveMakeRequests retains the flat discovery seam used by
// focused tests. Production profile discovery consumes the ordered plan above
// so it can serialize exact predecessor provenance.
func selectedKbuildRecursiveMakeRequests(profile kconfig.CompactKbuildProfile, satisfied map[string]bool) ([]kbuildInvocationRequest, error) {
	plan, err := selectedKbuildRecursiveMakePlan(profile, satisfied)
	if err != nil {
		return nil, err
	}
	requests := make([]kbuildInvocationRequest, 0, len(plan))
	for _, entry := range plan {
		requests = append(requests, entry.request)
	}
	return requests, nil
}

type kbuildRecursiveMakeInvocation struct {
	request         kbuildInvocationRequest
	replayArguments []string
}

type kbuildRecipeExecutionEffect struct {
	invocation         kbuildRecursiveMakeInvocation
	recursive          bool
	materializesTarget bool
	command            string
}

// kbuildSelectedRecipeExecutionEffects returns the source-ordered effects of
// exactly the command text selected by one recipe line. A selected cmd_<name>
// template is authoritative: the surrounding if_changed implementation is a
// Make/shell control wrapper, not a second action and not a second recursive
// invocation. Direct recipe lines use their fully evaluated source text.
func kbuildSelectedRecipeExecutionEffects(
	profile kconfig.CompactKbuildProfile,
	rule kconfig.KbuildRule,
	target, lookupTarget, automaticTarget, stem string,
	normal, orderOnly []string,
	recipe string,
) ([]kbuildRecipeExecutionEffect, error) {
	// GNU Make binds automatic variables and target-specific values against the
	// selected rule context, which merges ordinary explicit declarations into
	// the implicit (or explicit) declaration that owns the recipe. Keep the
	// source recipe itself, but replace its declaration-local prerequisites with
	// that exact evaluated context before expanding command templates or $<,$^.
	rule.Prerequisites = append([]string(nil), normal...)
	rule.OrderOnly = append([]string(nil), orderOnly...)
	injections, err := kconfig.CompactKbuildTargetEvaluationInjectionsForMakeTarget(
		profile, target, lookupTarget, automaticTarget, stem,
		rule.Prerequisites, rule.OrderOnly,
	)
	if err != nil {
		return nil, fmt.Errorf("evaluate selected recipe target-context paths: %w", err)
	}
	// These two aliases participate in Linux's recursive root control flow.
	// Action lowering later projects their final values onto immutable/writable
	// trees, but replacing them before recursive-Make discovery would collapse
	// `make -C $(abs_output)` back onto the kernel object root and would erase
	// the external source root derived from M=.
	delete(injections, "abs_output")
	delete(injections, "srcroot")
	injections["Q"] = ""
	lineRule := rule
	lineRule.Recipe = []string{recipe}
	templates, err := kconfig.EvaluateCompactKbuildCommandTemplatesSymbolicForMakeTarget(
		profile, lineRule, target, lookupTarget, automaticTarget, stem, injections,
	)
	if err != nil {
		return nil, fmt.Errorf("evaluate selected command template: %w", err)
	}
	if len(templates) != 0 {
		effects := []kbuildRecipeExecutionEffect{}
		for _, template := range templates {
			selected, err := kbuildCommandTemplateExecutionEffects(
				profile, rule, target, lookupTarget, automaticTarget, stem, injections, template,
			)
			if err != nil {
				name := template.Name
				if name == "" {
					name = "direct recipe"
				}
				return nil, fmt.Errorf("selected command %s: %w", name, err)
			}
			effects = append(effects, selected...)
		}
		return effects, nil
	}

	expanded, err := kconfig.EvaluateCompactKbuildTextSymbolicForMakeTarget(
		profile, target, lookupTarget, automaticTarget, stem, rule.Prerequisites, rule.OrderOnly,
		injections, recipe,
	)
	if err != nil {
		// Some selected control-effect lines intentionally retain a deferred
		// $(shell ...) result until action lowering. Preserve the old fail-closed
		// behavior for a structurally recursive line whose argv cannot be proven;
		// an ordinary local line still has the same whole-line materialization
		// classification it had before ordered effect discovery.
		if strings.Contains(recipe, "$(MAKE)") || strings.Contains(recipe, "${MAKE}") {
			return nil, nil
		}
		if !kbuildRecipeOnlyCreatesDirectories(recipe) && kconfig.CompactKbuildRecipeWritesTarget(recipe, automaticTarget) {
			// Expansion failed, so retain only existence provenance. Consumers
			// must not interpret the unevaluated source recipe as exact bytes.
			return []kbuildRecipeExecutionEffect{{materializesTarget: true}}, nil
		}
		return nil, nil
	}
	return kbuildEvaluatedRecipeExecutionEffects(
		profile, rule, target, lookupTarget, automaticTarget, stem, injections, expanded, false,
	)
}

// kbuildCommandTemplateExecutionEffects follows an evaluated cmd_<name> in
// execution order. Recursive Make can be selected directly by the template or
// indirectly by an immutable source-tree shell program; neither the command
// name nor the script pathname carries planner semantics.
func kbuildCommandTemplateExecutionEffects(
	profile kconfig.CompactKbuildProfile,
	rule kconfig.KbuildRule,
	target, lookupTarget, automaticTarget, stem string,
	injections map[string]string,
	template kconfig.CompactKbuildCommandTemplate,
) ([]kbuildRecipeExecutionEffect, error) {
	return kbuildEvaluatedRecipeExecutionEffects(
		profile, rule, target, lookupTarget, automaticTarget, stem, injections, template.Text, true,
	)
}

func kbuildEvaluatedRecipeExecutionEffects(
	profile kconfig.CompactKbuildProfile,
	rule kconfig.KbuildRule,
	target, lookupTarget, automaticTarget, stem string,
	injections map[string]string,
	command string,
	discoverSourceScripts bool,
) ([]kbuildRecipeExecutionEffect, error) {
	recipeTarget := automaticTarget
	structuralCommand, err := kconfig.ResolveCompactKbuildTargetSymbolicStructure(profile, target, command)
	if err != nil {
		return nil, fmt.Errorf("select recipe symbolic structure: %w", err)
	}
	resolvedCommand, err := kconfig.ResolveCompactKbuildTargetSymbolicText(profile, target, command)
	if err != nil {
		return nil, fmt.Errorf("resolve selected recipe symbolic text: %w", err)
	}
	command = resolvedCommand
	structuralSegments, structuralConnectors, err := kbuildEvaluatedShellCommandShape(structuralCommand)
	if err != nil {
		return nil, err
	}
	replaySegments, replayConnectors, err := kbuildEvaluatedShellCommandShape(command)
	if err != nil {
		return nil, err
	}
	if len(structuralSegments) != len(replaySegments) {
		return nil, fmt.Errorf(
			"selected recipe structural and replay forms have different command segment counts (%d and %d)",
			len(structuralSegments), len(replaySegments),
		)
	}
	if !slices.Equal(structuralConnectors, replayConnectors) {
		return nil, fmt.Errorf(
			"selected recipe structural and replay forms have different shell connector sequences (%q and %q)",
			structuralConnectors, replayConnectors,
		)
	}
	processLocation, ok := kconfig.CompactKbuildProfileInvocationLocation(profile)
	if !ok {
		return nil, fmt.Errorf("selected target %q profile %q has no typed Kbuild invocation location", target, profile.Name)
	}
	wholeRecipeWritesTarget := !kbuildRecipeOnlyCreatesDirectories(command) &&
		kconfig.CompactKbuildRecipeWritesTarget(command, recipeTarget)
	effects := []kbuildRecipeExecutionEffect{}
	materializedTarget := false
	var exportedEnvironment map[string]string
	loadExportedEnvironment := func() (map[string]string, error) {
		if exportedEnvironment != nil {
			return exportedEnvironment, nil
		}
		values, err := kconfig.EvaluateCompactKbuildTargetEnvironmentSymbolicForMakeTarget(
			profile, target, lookupTarget, automaticTarget, stem, rule.Prerequisites, rule.OrderOnly,
			injections,
		)
		if err != nil {
			return nil, fmt.Errorf("evaluate recursive Make exported environment: %w", err)
		}
		if values == nil {
			values = map[string]string{}
		}
		exportedEnvironment = cloneKbuildVariables(values)
		return exportedEnvironment, nil
	}
	var resolvedExportedEnvironment map[string]string
	loadResolvedExportedEnvironment := func() (map[string]string, error) {
		if resolvedExportedEnvironment != nil {
			return resolvedExportedEnvironment, nil
		}
		values, err := loadExportedEnvironment()
		if err != nil {
			return nil, err
		}
		resolvedExportedEnvironment = make(map[string]string, len(values))
		for name, value := range values {
			resolved, resolveErr := kconfig.ResolveCompactKbuildTargetSymbolicValue(profile, target, value)
			if resolveErr != nil {
				return nil, fmt.Errorf("resolve recursive Make exported environment %s: %w", name, resolveErr)
			}
			resolvedExportedEnvironment[name] = resolved
		}
		return resolvedExportedEnvironment, nil
	}
	attachEnvironment := func(invocations []kbuildRecursiveMakeInvocation) error {
		if len(invocations) == 0 {
			return nil
		}
		environment, err := loadExportedEnvironment()
		if err != nil {
			return err
		}
		origin, err := kconfig.EvaluateCompactKbuildTextSymbolicForMakeTarget(
			profile, target, lookupTarget, automaticTarget, stem, rule.Prerequisites, rule.OrderOnly,
			injections, "$(origin MAKEOVERRIDES)",
		)
		if err != nil {
			return fmt.Errorf("evaluate recursive Make MAKEOVERRIDES origin: %w", err)
		}
		makeOverrides, err := kconfig.EvaluateCompactKbuildTextSymbolicForMakeTarget(
			profile, target, lookupTarget, automaticTarget, stem, rule.Prerequisites, rule.OrderOnly,
			injections, "$(MAKEOVERRIDES)",
		)
		if err != nil {
			return fmt.Errorf("evaluate recursive Make MAKEOVERRIDES: %w", err)
		}
		makeOverrides, err = kconfig.ResolveCompactKbuildTargetSymbolicText(profile, target, makeOverrides)
		if err != nil {
			return fmt.Errorf("resolve recursive Make MAKEOVERRIDES: %w", err)
		}
		suppressParentCommandLine := origin != "undefined" && strings.TrimSpace(makeOverrides) == ""
		for index := range invocations {
			inline := invocations[index].request.environment
			invocations[index].request.environment = cloneKbuildVariables(environment)
			for name, value := range inline {
				invocations[index].request.environment[name] = value
			}
			invocations[index].request.suppressParentCommandLine = suppressParentCommandLine
		}
		return nil
	}
	for segmentIndex, segment := range replaySegments {
		structuralInvocations, err := kbuildRecursiveMakeInvocationsAt(
			structuralSegments[segmentIndex], processLocation,
		)
		if err != nil {
			return nil, fmt.Errorf("discover structural recursive Make invocation in shell segment %d: %w", segmentIndex, err)
		}
		replayInvocations, err := kbuildRecursiveMakeInvocationsAt(segment, processLocation)
		if err != nil {
			return nil, fmt.Errorf("discover replay recursive Make invocation in shell segment %d: %w", segmentIndex, err)
		}
		direct, err := kbuildBindRecursiveMakeReplayArguments(structuralInvocations, replayInvocations)
		if err != nil {
			return nil, fmt.Errorf("shell segment %d: %w", segmentIndex, err)
		}
		if err := attachEnvironment(direct); err != nil {
			return nil, err
		}
		for _, invocation := range direct {
			effects = append(effects, kbuildRecipeExecutionEffect{invocation: invocation, recursive: true})
		}
		recursive := len(direct) != 0

		if !discoverSourceScripts {
			if !recursive && !kbuildRecipeOnlyCreatesDirectories(segment) && kconfig.CompactKbuildRecipeWritesTarget(segment, recipeTarget) {
				effects = append(effects, kbuildRecipeExecutionEffect{
					materializesTarget: true,
					command:            segment,
				})
				materializedTarget = true
			}
			continue
		}

		scripts, err := kconfig.ReadCompactKbuildCommandSourceScriptsSymbolicForMakeTarget(
			profile, target, lookupTarget, automaticTarget, stem,
			rule.Prerequisites, rule.OrderOnly,
			injections, segment,
		)
		if err != nil {
			return nil, err
		}
		var scriptEnvironment map[string]string
		var replayScriptEnvironment map[string]string
		if len(scripts) != 0 {
			scriptEnvironment, err = loadExportedEnvironment()
			if err != nil {
				return nil, fmt.Errorf("evaluate exported source-script environment: %w", err)
			}
			replayScriptEnvironment, err = loadResolvedExportedEnvironment()
			if err != nil {
				return nil, fmt.Errorf("resolve exported source-script environment: %w", err)
			}
			scriptEnvironment = cloneKbuildVariables(scriptEnvironment)
			replayScriptEnvironment = cloneKbuildVariables(replayScriptEnvironment)
			effectiveMake, makeErr := kconfig.EvaluateCompactKbuildTextSymbolicForMakeTarget(
				profile, target, lookupTarget, automaticTarget, stem,
				rule.Prerequisites, rule.OrderOnly, injections, "$(MAKE)",
			)
			if makeErr != nil {
				return nil, fmt.Errorf("evaluate source-script MAKE: %w", makeErr)
			}
			resolvedMake, makeErr := kconfig.ResolveCompactKbuildTargetSymbolicText(
				profile, target, effectiveMake,
			)
			if makeErr != nil {
				return nil, fmt.Errorf("resolve source-script MAKE: %w", makeErr)
			}
			// A script may reference MAKE even when the Make frontend did not add
			// its default to the exported environment. Project the profile's actual
			// effective value: private provenance is carried only when it survived
			// normal Make precedence into this target context.
			scriptEnvironment["MAKE"] = effectiveMake
			scriptEnvironment["srctree"] = kbuildEvalSourceTree
			scriptEnvironment["objtree"] = kbuildEvalObjectTree
			replayScriptEnvironment["MAKE"] = resolvedMake
			replayScriptEnvironment["srctree"] = kbuildEvalSourceTree
			replayScriptEnvironment["objtree"] = kbuildEvalObjectTree
		}
		for _, script := range scripts {
			nested, err := kbuildSourceScriptRecursiveMakeInvocationsAt(
				script.Content, scriptEnvironment, processLocation,
			)
			if err != nil {
				return nil, fmt.Errorf("source script %q: %w", script.Path, err)
			}
			replayNested, err := kbuildSourceScriptRecursiveMakeInvocationsAt(
				script.Content, replayScriptEnvironment, processLocation,
			)
			if err != nil {
				return nil, fmt.Errorf("source script %q replay: %w", script.Path, err)
			}
			nested, err = kbuildBindRecursiveMakeReplayArguments(nested, replayNested)
			if err != nil {
				return nil, fmt.Errorf("source script %q: %w", script.Path, err)
			}
			if err := attachEnvironment(nested); err != nil {
				return nil, err
			}
			for _, invocation := range nested {
				effects = append(effects, kbuildRecipeExecutionEffect{invocation: invocation, recursive: true})
			}
			recursive = recursive || len(nested) != 0
		}
		declaredSourceScriptWriter := slices.ContainsFunc(scripts, func(script kconfig.CompactKbuildSourceScript) bool {
			declared := slices.Contains(rule.Prerequisites, script.Path) || slices.Contains(rule.OrderOnly, script.Path)
			if !declared {
				return false
			}
			if kconfig.CompactKbuildRecipeWritesTarget(script.Content, recipeTarget) {
				return true
			}
			// Linux's terminal link driver owns vmlinux internally rather than
			// receiving $@ or a shell redirection from its Make recipe. Keep that
			// explicit source ABI narrow: merely declaring any other validation
			// script is not evidence that it writes the rule target.
			return kconfig.CanonicalKbuildGraphTarget(recipeTarget) == "vmlinux" &&
				pathpkg.Clean(script.Path) == "scripts/link-vmlinux.sh"
		})
		if !kbuildRecipeOnlyCreatesDirectories(segment) &&
			(declaredSourceScriptWriter || !recursive && kconfig.CompactKbuildRecipeWritesTarget(segment, recipeTarget)) {
			effects = append(effects, kbuildRecipeExecutionEffect{
				materializesTarget: true,
				command:            segment,
			})
			materializedTarget = true
		}
	}
	if wholeRecipeWritesTarget && !materializedTarget {
		// A brace-group redirect belongs to the complete group. Semicolon
		// splitting is still required above to order recursive Make effects, but
		// no individual inner segment owns the redirect. Record its materialized
		// target after the group completes and retain the whole recipe for the
		// separate exact-byte projection.
		effects = append(effects, kbuildRecipeExecutionEffect{
			materializesTarget: true,
			command:            command,
		})
	}
	return effects, nil
}

// kbuildEvaluatedShellCommandSegments splits only execution connectors. It
// keeps redirections with their command and shell-quotes the already lexical
// argv again so downstream discovery cannot change spaces or empty operands.
func kbuildEvaluatedShellCommandSegments(command string) ([]string, error) {
	segments, _, err := kbuildEvaluatedShellCommandShape(command)
	return segments, err
}

func kbuildEvaluatedShellCommandShape(command string) ([]string, []string, error) {
	fields, err := kbuildShellLexemes(command)
	if err != nil {
		return nil, nil, err
	}
	lexemeSegments, connectors := kbuildShellLexemeCommandSegments(fields)
	segments := make([]string, 0, len(lexemeSegments))
	for _, segment := range lexemeSegments {
		if _, err := kbuildParseShellSimpleCommand(segment); err != nil {
			return nil, nil, err
		}
		segments = append(segments, kbuildShellJoinLexemes(segment))
	}
	return segments, connectors, nil
}

func kbuildShellJoinLexemes(fields []kbuildShellLexeme) string {
	quoted := make([]string, 0, len(fields))
	for _, field := range fields {
		switch field.kind {
		case kbuildShellConnectorLexeme:
			quoted = append(quoted, field.value)
		case kbuildShellRedirectionLexeme:
			quoted = append(quoted, field.ioNumber+field.value)
		default:
			quote := func(value string) string {
				return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
			}
			if field.assignmentName != "" {
				_, value, _ := strings.Cut(field.value, "=")
				quoted = append(quoted, field.assignmentName+"="+quote(value))
			} else if field.plain && kbuildShellReservedCommandPrefix(field.value) {
				quoted = append(quoted, field.value)
			} else {
				quoted = append(quoted, quote(field.value))
			}
		}
	}
	return strings.Join(quoted, " ")
}

// kbuildRecursiveMakeInvocations recovers every complete Make argv from an
// evaluated shell fragment. ReplayArguments excludes the Make executable and
// stops at the same control operator as kbuildRecursiveMakeRequest.
func kbuildRecursiveMakeInvocations(command, parentDirectory string) ([]kbuildRecursiveMakeInvocation, error) {
	return kbuildRecursiveMakeInvocationsAt(command, kconfig.CompactKbuildInvocationLocation{
		Tree: kconfig.CompactKbuildInvocationObjectTree, Directory: parentDirectory,
	})
}

func kbuildRecursiveMakeInvocationsAt(
	command string,
	parentLocation kconfig.CompactKbuildInvocationLocation,
) ([]kbuildRecursiveMakeInvocation, error) {
	commands, _, err := kbuildShellSimpleCommands(command)
	if err != nil {
		if strings.Contains(command, kbuildEvalRecursiveMake) {
			return nil, err
		}
		return nil, nil
	}
	invocations := []kbuildRecursiveMakeInvocation{}
	for _, simple := range commands {
		if len(simple.argv) == 0 || strings.TrimLeft(simple.argv[0], "+@-") != kbuildEvalRecursiveMake {
			continue
		}
		replay := append([]string(nil), simple.argv[1:]...)
		for replayIndex, argument := range replay {
			argument = strings.NewReplacer(
				"${tree:kernel}", kbuildEvalSourceTree,
				"${tree:prep}", kbuildEvalObjectTree,
			).Replace(argument)
			if kconfig.ActionRecipeTemplateRetainsDynamicShellSyntax(argument) {
				return nil, fmt.Errorf("recursive Make replay argv retains dynamic shell operand %q", argument)
			}
			replay[replayIndex] = argument
		}
		request, err := kbuildRecursiveMakeRequestArgvAt(replay, simple.assignments, parentLocation)
		if err != nil {
			return nil, err
		}
		// The child profile must retain the private MAKE capability, but runtime
		// replay argv is serialized into an ActionRecipe and must match the
		// lowered script, where every proven private occurrence names the replay
		// proxy. Printable lookalikes are ordinary bytes and remain unchanged.
		for replayIndex, argument := range replay {
			replay[replayIndex] = strings.ReplaceAll(
				argument,
				kbuildEvalRecursiveMake,
				kconfig.CompactKbuildRecursiveMakeReplayName,
			)
		}
		invocations = append(invocations, kbuildRecursiveMakeInvocation{
			request: request, replayArguments: replay,
		})
	}
	return invocations, nil
}

func kbuildBindRecursiveMakeReplayArguments(
	invocations, replayInvocations []kbuildRecursiveMakeInvocation,
) ([]kbuildRecursiveMakeInvocation, error) {
	if len(invocations) != len(replayInvocations) {
		return nil, fmt.Errorf(
			"recursive Make symbolic and replay forms contain different invocation counts (%d and %d)",
			len(invocations), len(replayInvocations),
		)
	}
	for index := range invocations {
		request := invocations[index].request
		replayRequest := replayInvocations[index].request
		if request.name != replayRequest.name ||
			request.makefile != replayRequest.makefile ||
			request.directory != replayRequest.directory ||
			request.processLocation != replayRequest.processLocation ||
			!slices.Equal(request.entryTargets, replayRequest.entryTargets) ||
			!kbuildSameVariableNames(request.environment, replayRequest.environment) ||
			!kbuildSameVariableNames(request.variables, replayRequest.variables) ||
			!maps.Equal(request.commandLineAutoExport, replayRequest.commandLineAutoExport) {
			return nil, fmt.Errorf(
				"recursive Make symbolic request %q does not match replay request %q",
				request.name, replayRequest.name,
			)
		}
		invocations[index].replayArguments = append(
			[]string(nil), replayInvocations[index].replayArguments...,
		)
	}
	return invocations, nil
}

func kbuildSameVariableNames(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for name := range left {
		if _, exists := right[name]; !exists {
			return false
		}
	}
	return true
}

func kbuildSourceScriptRecursiveMakeInvocationsAt(
	content string,
	environment map[string]string,
	parentLocation kconfig.CompactKbuildInvocationLocation,
) ([]kbuildRecursiveMakeInvocation, error) {
	lines, err := kbuildShellLogicalLines(content)
	if err != nil {
		return nil, err
	}
	invocations := []kbuildRecursiveMakeInvocation{}
	for lineNumber, line := range lines {
		if !strings.Contains(line, "$") || !strings.Contains(line, "MAKE") {
			continue
		}
		expanded, selected, err := expandKbuildSourceScriptEnvironment(line, environment)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber+1, err)
		}
		if !selected {
			continue
		}
		nested, err := kbuildRecursiveMakeInvocationsAt(expanded, parentLocation)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber+1, err)
		}
		invocations = append(invocations, nested...)
	}
	return invocations, nil
}

func kbuildShellLogicalLines(content string) ([]string, error) {
	physical := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	logical := []string{}
	var current strings.Builder
	for _, line := range physical {
		continued := false
		backslashes := 0
		for index := len(line) - 1; index >= 0 && line[index] == '\\'; index-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			continued = true
			line = line[:len(line)-1]
		}
		current.WriteString(line)
		if continued {
			current.WriteByte(' ')
			continue
		}
		logical = append(logical, current.String())
		current.Reset()
	}
	if current.Len() != 0 {
		return nil, fmt.Errorf("unterminated shell line continuation")
	}
	return logical, nil
}

// expandKbuildSourceScriptEnvironment applies only ordinary shell variable
// references from GNU Make's exact exported target environment. It never
// evaluates command substitution or script-local state; a surviving dynamic
// operand in recursive Make argv is rejected by replay discovery.
func expandKbuildSourceScriptEnvironment(line string, environment map[string]string) (string, bool, error) {
	var expanded strings.Builder
	quote := byte(0)
	escaped := false
	selectedMake := false
	for index := 0; index < len(line); {
		character := line[index]
		if escaped {
			expanded.WriteByte(character)
			escaped = false
			index++
			continue
		}
		if character == '\\' && quote != '\'' {
			expanded.WriteByte(character)
			escaped = true
			index++
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
				expanded.WriteByte(character)
				index++
				continue
			}
			if quote == '\'' || character != '$' {
				expanded.WriteByte(character)
				index++
				continue
			}
		} else {
			if character == '\'' || character == '"' {
				quote = character
				expanded.WriteByte(character)
				index++
				continue
			}
			if character == '#' && (index == 0 || strings.ContainsRune(" \t;&|()", rune(line[index-1]))) {
				expanded.WriteString(line[index:])
				break
			}
			if character != '$' {
				expanded.WriteByte(character)
				index++
				continue
			}
		}

		name, end, form, ok := kbuildSourceScriptVariableReference(line, index)
		if !ok {
			expanded.WriteByte(character)
			index++
			continue
		}
		value, known := environment[name]
		if name == "MAKE" {
			selectedMake = selectedMake || known && strings.Contains(value, kbuildEvalRecursiveMake)
		}
		if !known || form == '(' && name != "MAKE" {
			expanded.WriteString(line[index:end])
		} else {
			expanded.WriteString(value)
		}
		index = end
	}
	if escaped {
		return "", false, fmt.Errorf("unterminated shell escape")
	}
	if quote != 0 {
		return "", false, fmt.Errorf("unterminated shell quote")
	}
	return expanded.String(), selectedMake, nil
}

func kbuildSourceScriptVariableReference(line string, start int) (string, int, byte, bool) {
	if start+1 >= len(line) || line[start] != '$' {
		return "", start, 0, false
	}
	form := byte(0)
	begin := start + 1
	end := begin
	switch line[begin] {
	case '{', '(':
		form = line[begin]
		closing := byte('}')
		if form == '(' {
			closing = ')'
		}
		begin++
		end = begin
		for end < len(line) && line[end] != closing {
			end++
		}
		if end == len(line) {
			return "", start, 0, false
		}
		name := line[begin:end]
		if !kbuildShellVariableName(name) {
			return "", start, 0, false
		}
		return name, end + 1, form, true
	default:
		end = begin
		for end < len(line) && (line[end] == '_' || line[end] >= 'a' && line[end] <= 'z' ||
			line[end] >= 'A' && line[end] <= 'Z' || line[end] >= '0' && line[end] <= '9') {
			end++
		}
		if end == begin {
			return "", start, 0, false
		}
		name := line[begin:end]
		if !kbuildShellVariableName(name) {
			return "", start, 0, false
		}
		return name, end, form, true
	}
}

func kbuildShellVariableName(name string) bool {
	if name == "" || name[0] >= '0' && name[0] <= '9' {
		return false
	}
	for _, character := range name {
		if character == '_' || character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

const maxKbuildTraversalTargetLength = 4096

func canonicalKbuildPreparationTargets(targets []string) ([]string, error) {
	canonical := make([]string, 0, len(targets))
	seen := make(map[string]bool, len(targets))
	for _, raw := range targets {
		if len(raw) > maxKbuildTraversalTargetLength {
			return nil, fmt.Errorf("Kbuild preparation target is implausibly long (%d bytes)", len(raw))
		}
		if strings.ContainsAny(raw, "\x00\r\n\\") {
			return nil, fmt.Errorf("invalid Kbuild preparation target %q: control characters and backslashes are unsupported", raw)
		}
		if strings.ContainsAny(raw, "%$") {
			return nil, fmt.Errorf("invalid Kbuild preparation target %q: patterns and Make expressions are unsupported", raw)
		}
		trimmed := strings.TrimSpace(raw)
		if pathpkg.IsAbs(trimmed) {
			return nil, fmt.Errorf("invalid Kbuild preparation target %q: absolute paths are unsupported", raw)
		}
		target := kconfig.CanonicalKbuildGraphTarget(trimmed)
		if target == "" || target == "FORCE" {
			return nil, fmt.Errorf("invalid Kbuild preparation target %q", raw)
		}
		if target == ".." || strings.HasPrefix(target, "../") {
			return nil, fmt.Errorf("invalid Kbuild preparation target %q: target escapes the Kbuild graph root", raw)
		}
		if len(target) > maxKbuildTraversalTargetLength {
			return nil, fmt.Errorf("Kbuild preparation target is implausibly long (%d bytes)", len(target))
		}
		if seen[target] {
			continue
		}
		seen[target] = true
		canonical = append(canonical, target)
	}
	return canonical, nil
}

func validateKbuildTraversalTarget(profile, origin, target string) error {
	if len(target) <= maxKbuildTraversalTargetLength {
		return nil
	}
	const excerptLength = 160
	prefix := target
	suffix := ""
	if len(prefix) > excerptLength {
		prefix = prefix[:excerptLength]
		suffix = target[len(target)-excerptLength:]
	}
	return fmt.Errorf(
		"Kbuild profile %q produced an implausibly long %s (%d bytes; prefix %q; suffix %q)",
		profile, origin, len(target), prefix, suffix,
	)
}

// kbuildSelectedRecipeObjectTreeReferences extracts canonical paths rooted in
// the stable object-tree markers from evaluated argv/environment text. The
// parser does not interpret compiler flags: a marker followed by a path is a
// source-owned filesystem reference regardless of which program consumes it.
func kbuildSelectedRecipeObjectTreeReferences(value string) []string {
	return kconfig.ObserveCompactKbuildObjectTree(value).References
}

type kbuildIncludeTreePath struct {
	path   string
	source bool
	// searchAfterDirect applies to a relative -include/-imacros operand. The
	// compiler tries its object-tree working directory first, then its ordinary
	// quoted-include search chain.
	searchAfterDirect bool
	searchName        string
}

type kbuildIncludeSearchDirectory struct {
	kbuildIncludeTreePath
	quoteOnly bool
}

type kbuildIncludeSearchPlan struct {
	sources          []string
	generatedSources []string
	directories      []kbuildIncludeSearchDirectory
	forced           []kbuildIncludeTreePath
}

// kbuildSelectedRecipeSourceProjection classifies the complete evaluated
// command text for one selected Make recipe. It returns an immutable source
// projection only when every command which visibly writes target is the same
// byte-preserving copy. Shell control and diagnostics which do not write the
// target are ignored; ambiguous or later target rewrites fail closed. The
// final result reports an opaque effect separately so callers can preserve it
// across every recipe line and selected command template for the target.
func kbuildSelectedRecipeSourceProjection(value, target string, sources []string) (string, bool, bool, bool) {
	segments, connectors, err := kbuildEvaluatedShellCommandShape(value)
	if err != nil {
		return "", false, false, true
	}
	unsupportedControl := false
	for _, connector := range connectors {
		if connector != ";" && connector != "\n" {
			unsupportedControl = true
		}
	}
	projection := ""
	writesTarget := false
	compatible := true
	for _, segment := range segments {
		if source, projected := kconfig.CompactKbuildRecipeSourceProjection(segment, target, sources); projected {
			writesTarget = true
			if projection != "" && projection != source {
				return "", false, true, true
			}
			projection = source
			continue
		}
		if kconfig.CompactKbuildRecipeWritesTarget(segment, target) {
			return "", false, true, true
		}
		if !kbuildSelectedRecipeProjectionAuxiliary(segment, target) {
			compatible = false
		}
	}
	if projection != "" && (!compatible || unsupportedControl) {
		return "", false, true, true
	}
	return projection, projection != "", writesTarget, !compatible || unsupportedControl
}

// kbuildSelectedRecipeLiteralProjection classifies an exact literal target
// body across the complete evaluated recipe text. It shares the copy
// projection's fail-closed treatment of shell control and surrounding opaque
// commands, but keeps independent ambiguity state because a literal writer is
// necessarily not an immutable-source copy.
func kbuildSelectedRecipeLiteralProjection(value, target string) (string, bool, bool, bool) {
	segments, connectors, err := kbuildEvaluatedShellCommandShape(value)
	if err != nil {
		return "", false, false, true
	}
	unsupportedControl := false
	for _, connector := range connectors {
		if connector != ";" && connector != "\n" {
			unsupportedControl = true
		}
	}
	literal := ""
	literalSet := false
	writesTarget := false
	compatible := true
	for _, segment := range segments {
		if content, projected := kconfig.CompactKbuildRecipeLiteralOutput(segment, target); projected {
			writesTarget = true
			if literalSet && literal != content {
				return "", false, true, true
			}
			literal = content
			literalSet = true
			continue
		}
		if kconfig.CompactKbuildRecipeWritesTarget(segment, target) {
			return "", false, true, true
		}
		if !kbuildSelectedRecipeProjectionAuxiliary(segment, target) {
			compatible = false
		}
	}
	if literalSet && (!compatible || unsupportedControl) {
		return "", false, true, true
	}
	return literal, literalSet, writesTarget, !compatible || unsupportedControl
}

// kbuildSelectedRecipeProjectionAuxiliary recognizes the deliberately small
// set of commands which may surround Linux's header-install copies or literal
// wrappers without changing their bytes. Unknown commands fail closed once a
// projection is present; in particular, a later strip/filter cannot inherit
// byte provenance merely because its output syntax is opaque.
func kbuildSelectedRecipeProjectionAuxiliary(segment, target string) bool {
	command, ok, err := kbuildSingleShellSimpleCommand(segment)
	if err != nil || !ok || len(command.argv) == 0 || len(command.redirections) != 0 {
		return false
	}
	words := command.argv
	conditional := false
	for len(words) != 0 {
		switch words[0] {
		case "if", "elif", "!":
			conditional = true
			words = words[1:]
		case "then", "else", "do":
			words = words[1:]
		case "fi", "done":
			words = words[1:]
			if len(words) == 0 {
				return true
			}
		default:
			goto command
		}
	}
	return false

command:
	program, runtime := kbuildProjectionRuntimeApplet(words[0])
	if !runtime {
		return false
	}
	if conditional {
		return program == "test" || program == "[" && len(words) >= 2 && words[len(words)-1] == "]"
	}
	switch program {
	case "printf", ":":
		return true
	case "[":
		return len(words) >= 2 && words[len(words)-1] == "]"
	case "test":
		return true
	case "mkdir":
		return kbuildProjectionDirectorySetup(words[1:], target, false)
	case "install":
		return kbuildProjectionDirectorySetup(words[1:], target, true)
	default:
		return false
	}
}

func kbuildProjectionRuntimeApplet(program string) (string, bool) {
	program = strings.TrimLeft(program, "+@-")
	if pathpkg.Base(program) == program {
		return program, program != ""
	}
	clean := pathpkg.Clean(program)
	if directory := pathpkg.Dir(clean); directory != "/bin" && directory != "/usr/bin" {
		return "", false
	}
	return pathpkg.Base(clean), true
}

func kbuildProjectionDirectorySetup(arguments []string, target string, install bool) bool {
	operands := []string{}
	options := true
	directoryMode := !install
	parents := install
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			options = false
			continue
		}
		if !options || !strings.HasPrefix(argument, "-") || argument == "-" {
			operands = append(operands, argument)
			continue
		}
		if !install {
			switch argument {
			case "-p", "--parents":
				parents = true
			case "-v", "--verbose":
			case "-m", "--mode":
				if index+1 >= len(arguments) {
					return false
				}
				index++
			default:
				if !strings.HasPrefix(argument, "-m") || len(argument) == 2 {
					return false
				}
			}
			continue
		}
		switch {
		case argument == "-d" || argument == "--directory":
			directoryMode = true
		case argument == "-m" || argument == "--mode" || argument == "-o" || argument == "--owner" || argument == "-g" || argument == "--group":
			if index+1 >= len(arguments) {
				return false
			}
			index++
		case strings.HasPrefix(argument, "-m") && len(argument) > 2,
			strings.HasPrefix(argument, "--mode="),
			strings.HasPrefix(argument, "--owner="),
			strings.HasPrefix(argument, "--group="),
			argument == "-p", argument == "-v", argument == "--preserve-timestamps", argument == "--verbose":
		default:
			return false
		}
	}
	if !directoryMode || !parents || len(operands) == 0 {
		return false
	}
	cleanTarget := pathpkg.Clean(target)
	for _, operand := range operands {
		clean := pathpkg.Clean(filepath.ToSlash(operand))
		if clean == cleanTarget || strings.HasSuffix(clean, "/"+cleanTarget) {
			return false
		}
	}
	return true
}

type kbuildGeneratedSourceScriptInvocation struct {
	scope     string
	script    string
	arguments []kconfig.KbuildSourceScriptArgument
}

func kbuildGeneratedSourceScriptInvocationForRecipe(
	profile kconfig.CompactKbuildProfile,
	target, recipe string,
	sourcePrerequisites, generatedPrerequisites []string,
) (kbuildGeneratedSourceScriptInvocation, bool, error) {
	segments, err := kbuildEvaluatedShellCommandSegments(recipe)
	if err != nil || len(segments) != 1 {
		return kbuildGeneratedSourceScriptInvocation{}, false, nil
	}
	command, ok, err := kbuildSingleShellSimpleCommand(segments[0])
	if err != nil || !ok || len(command.argv) < 2 {
		return kbuildGeneratedSourceScriptInvocation{}, false, nil
	}
	fields := command.argv
	interpreterRoles, err := kconfig.KbuildActionRoleRefs(fields[0])
	if err != nil {
		return kbuildGeneratedSourceScriptInvocation{}, false, err
	}
	scope := "target"
	if len(interpreterRoles) == 0 {
		// CONFIG_SHELL defaults to the literal command name "sh" in upstream
		// Kbuild. Execute that request through the declared script-runtime
		// toolchain; never resolve it from the ambient executor PATH.
		if fields[0] != "sh" {
			return kbuildGeneratedSourceScriptInvocation{}, false, nil
		}
	} else {
		if len(interpreterRoles) != 1 || interpreterRoles[0].Role != "script-runtime" {
			return kbuildGeneratedSourceScriptInvocation{}, false, nil
		}
		scope = interpreterRoles[0].Scope
		if scope == kconfig.KbuildActionRoleAutoScope {
			scope = "target"
		}
	}
	if scope != "target" && scope != "host" {
		return kbuildGeneratedSourceScriptInvocation{}, false, nil
	}
	scriptArgument, fileMode := kconfig.CompactKbuildShellFileScriptIndex(fields[1:])
	if !fileMode || scriptArgument != 0 {
		// SourceScriptOutputText models only the immutable script and its argv;
		// it has no field for interpreter options. Keep the historical direct
		// `sh SCRIPT ...` shape, but decline every option-bearing invocation
		// instead of mistaking an option operand for the script.
		return kbuildGeneratedSourceScriptInvocation{}, false, nil
	}
	scriptField := scriptArgument + 1
	resolveTreePath := func(value string) (kconfig.CompactKbuildInvocationLocation, bool, error) {
		location, _, ok, pathErr := kconfig.ResolveCompactKbuildCompilerIncludePath(profile, value)
		return location, ok, pathErr
	}
	sourceSet := map[string]bool{}
	for _, source := range sourcePrerequisites {
		sourceSet[source] = true
	}
	generatedSet := map[string]bool{}
	for _, generated := range generatedPrerequisites {
		generatedSet[generated] = true
	}
	scriptLocation, ok, err := resolveTreePath(fields[scriptField])
	if err != nil {
		return kbuildGeneratedSourceScriptInvocation{}, false, err
	}
	// Kbuild commonly names generator scripts only in the recipe (for example
	// x86 mkcapflags.sh), not in the target's prerequisite list. The resolved
	// source-tree path is still immutable probe authority: SourceScriptOutputText
	// verifies that it is one regular shell script beneath the declared Linux
	// source root and adds it to the content-addressed request's source inputs.
	if !ok || scriptLocation.Tree != kconfig.CompactKbuildInvocationSourceTree {
		return kbuildGeneratedSourceScriptInvocation{}, false, nil
	}
	argumentFields := fields[scriptField+1:]
	stdoutOutput := false
	if len(command.redirections) != 0 {
		if len(command.redirections) != 1 || command.redirections[0].operator != ">" ||
			(command.redirections[0].ioNumber != "" && command.redirections[0].ioNumber != "1") {
			return kbuildGeneratedSourceScriptInvocation{}, false, nil
		}
		outputField := command.redirections[0].operand
		outputLocation, outputOK, outputErr := resolveTreePath(outputField)
		if outputErr != nil {
			return kbuildGeneratedSourceScriptInvocation{}, false, outputErr
		}
		canonical := kconfig.CanonicalKbuildGraphTarget(outputField)
		if canonical != target && (!outputOK || outputLocation.Tree != kconfig.CompactKbuildInvocationObjectTree || outputLocation.Directory != target) {
			return kbuildGeneratedSourceScriptInvocation{}, false, nil
		}
		stdoutOutput = true
	}
	arguments := make([]kconfig.KbuildSourceScriptArgument, 0, len(argumentFields)+1)
	outputCount := 0
	if stdoutOutput {
		arguments = append(arguments, kconfig.KbuildSourceScriptArgument{Kind: kconfig.KbuildSourceScriptStdoutArgument})
		outputCount++
	}
	for _, field := range argumentFields {
		// Automatic variables in evaluated Kbuild recipes retain canonical graph
		// paths. Match those exact source-declared identities before resolving
		// explicit tree markers or invocation-relative path operands.
		canonical := kconfig.CanonicalKbuildGraphTarget(field)
		if canonical == field {
			switch {
			case canonical == target:
				arguments = append(arguments, kconfig.KbuildSourceScriptArgument{Kind: kconfig.KbuildSourceScriptOutputArgument})
				outputCount++
				continue
			case sourceSet[canonical]:
				arguments = append(arguments, kconfig.KbuildSourceScriptArgument{
					Kind: kconfig.KbuildSourceScriptSourceArgument, Value: canonical,
				})
				continue
			case generatedSet[canonical]:
				return kbuildGeneratedSourceScriptInvocation{}, false, nil
			}
		}
		location, pathOK, pathErr := resolveTreePath(field)
		if pathErr != nil {
			return kbuildGeneratedSourceScriptInvocation{}, false, pathErr
		}
		if pathOK {
			switch {
			case location.Tree == kconfig.CompactKbuildInvocationObjectTree && location.Directory == target:
				arguments = append(arguments, kconfig.KbuildSourceScriptArgument{Kind: kconfig.KbuildSourceScriptOutputArgument})
				outputCount++
				continue
			case location.Tree == kconfig.CompactKbuildInvocationSourceTree && sourceSet[location.Directory]:
				arguments = append(arguments, kconfig.KbuildSourceScriptArgument{
					Kind: kconfig.KbuildSourceScriptSourceArgument, Value: location.Directory,
				})
				continue
			case generatedSet[location.Directory]:
				// A planner probe cannot consume a Kbuild output which does not
				// exist until the final mapped action graph executes.
				return kbuildGeneratedSourceScriptInvocation{}, false, nil
			}
		}
		roles, roleErr := kconfig.KbuildActionRoleRefs(field)
		if roleErr != nil {
			return kbuildGeneratedSourceScriptInvocation{}, false, roleErr
		}
		if len(roles) == 1 {
			role := roles[0]
			if role.Scope != kconfig.KbuildActionRoleAutoScope && role.Scope != scope {
				return kbuildGeneratedSourceScriptInvocation{}, false, nil
			}
			arguments = append(arguments, kconfig.KbuildSourceScriptArgument{
				Kind: kconfig.KbuildSourceScriptToolArgument, Value: role.Role,
			})
			continue
		}
		if len(roles) != 0 || field == "" || len(field) > 4096 || strings.ContainsAny(field, "/\\\x00\r\n$") {
			return kbuildGeneratedSourceScriptInvocation{}, false, nil
		}
		arguments = append(arguments, kconfig.KbuildSourceScriptArgument{
			Kind: kconfig.KbuildSourceScriptLiteralArgument, Value: field,
		})
	}
	if outputCount != 1 {
		return kbuildGeneratedSourceScriptInvocation{}, false, nil
	}
	return kbuildGeneratedSourceScriptInvocation{
		scope: scope, script: scriptLocation.Directory, arguments: arguments,
	}, true, nil
}

func linuxKbuildGeneratedContentResolver(
	scopes *kconfig.KbuildProbeScopes,
	workingTreeContents map[string]string,
	rootDir, objectRoot string,
	preconfiguredObjectTree bool,
) kbuildGeneratedContentResolver {
	if scopes == nil {
		return nil
	}
	resolvedConfigContents := make(map[string]string, len(kconfig.ResolvedConfigProjectionOutputs()))
	for _, pathname := range kconfig.ResolvedConfigProjectionOutputs() {
		contents, ok := workingTreeContents[pathname]
		if !ok {
			// An incomplete ambient contract can never authorize an exact-output
			// replacement. Keep a non-nil resolver so checked-in source-script
			// projections remain available below.
			resolvedConfigContents = nil
			break
		}
		resolvedConfigContents[pathname] = contents
	}
	return func(
		profile kconfig.CompactKbuildProfile,
		target, recipe string,
		sourcePrerequisites, generatedPrerequisites []string,
	) (string, bool, bool, error) {
		if resolvedConfigContents == nil {
			return "", false, false, nil
		}
		// A direct source-script probe executes from the immutable source root,
		// while final Kbuild lowering executes from a private writable object
		// tree. Its result is still useful to dedicated source-query callers, but
		// it is not byte authority for replacing a selected generator. Route this
		// path exclusively through the evaluated-recipe probe, which stages and
		// verifies the same source/config working frontier. Unsupported script
		// shapes remain ordinary conservative Kbuild actions.
		// The evaluated-output probe receives the exact immutable config
		// baseline below, while source prerequisites are declared separately.
		// Refuse every shape whose eventual compound action can acquire another
		// object-tree input: those producer bytes do not exist during probe
		// discovery and staging a profile-wide superset would change observable
		// directory topology.
		if !kbuildGeneratedContentHasClosedWorkingFrontier(
			profile, target, recipe, rootDir, objectRoot, preconfiguredObjectTree,
		) {
			return "", false, false, nil
		}

		// The evaluated-recipe probe cannot consume an object-tree result which
		// does not exist until the final mapped graph runs. Source prerequisites
		// and FORCE are already immutable or control-only; every other selected
		// prerequisite keeps the ordinary conservative generator.
		sourceSet := make(map[string]bool, len(sourcePrerequisites))
		for _, source := range sourcePrerequisites {
			sourceSet[kconfig.CanonicalKbuildGraphTarget(source)] = true
		}
		for _, prerequisite := range generatedPrerequisites {
			canonical := kconfig.CanonicalKbuildGraphTarget(prerequisite)
			if canonical == "" || canonical == "FORCE" || canonical == target || sourceSet[canonical] {
				continue
			}
			return "", false, false, nil
		}
		contents, concrete, recognized, err := scopes.EvaluatedScriptOutputTextAcrossScopes(
			target, recipe, sourcePrerequisites, resolvedConfigContents,
		)
		return contents, concrete, recognized, err
	}
}

// kbuildSelectedRecipeIncludeSearchReferences extracts the include contract
// from one exact source-selected compiler command. Search categories are
// returned in compiler order (-iquote, -I, -isystem, -idirafter), preserving
// argv order within each category. Plans remain command-local so flags from
// two compiler invocations can never be combined into a synthetic search path.
func kbuildSelectedRecipeIncludeSearchReferences(
	profile kconfig.CompactKbuildProfile,
	value string,
	sourcePrerequisites, generatedPrerequisites []string,
) ([]kbuildIncludeSearchPlan, error) {
	segments, err := kbuildEvaluatedShellCommandSegments(value)
	if err != nil {
		return nil, nil
	}
	plans := []kbuildIncludeSearchPlan{}
	for _, segment := range segments {
		roles, roleErr := kconfig.KbuildActionRoleRefs(segment)
		if roleErr != nil {
			return nil, roleErr
		}
		role := ""
		ambiguous := false
		for _, candidate := range roles {
			if !kbuildCompilerIncludeRole(candidate.Role) {
				continue
			}
			if role != "" && role != candidate.Role {
				ambiguous = true
				break
			}
			role = candidate.Role
		}
		if role == "" || ambiguous {
			continue
		}
		plan, planErr := kbuildCompilerIncludeSearchPlan(
			profile, role, segment, sourcePrerequisites, generatedPrerequisites,
		)
		if planErr != nil {
			return nil, planErr
		}
		if len(plan.sources) != 0 || len(plan.generatedSources) != 0 || len(plan.forced) != 0 {
			plans = append(plans, plan)
		}
	}
	return plans, nil
}

func kbuildCompilerIncludeRole(role string) bool {
	return role == "cc" || role == "cxx" || role == "bindgen"
}

func kbuildCompilerIncludeSearchPlan(
	profile kconfig.CompactKbuildProfile,
	role string,
	value string,
	sourcePrerequisites, generatedPrerequisites []string,
) (kbuildIncludeSearchPlan, error) {
	command, ok, err := kbuildSingleShellSimpleCommand(value)
	if err != nil || !ok {
		// Some non-compiler recipes retain shell syntax which is intentionally
		// opaque here. Their ordinary object-tree/frontier observation remains
		// handled by kbuildSelectedRecipeObjectTreeReferences.
		return kbuildIncludeSearchPlan{}, nil
	}
	fields := command.argv
	treePath := func(value string) (kbuildIncludeTreePath, bool, bool, error) {
		location, relative, ok, err := kconfig.ResolveCompactKbuildCompilerIncludePath(profile, value)
		if err != nil || !ok {
			return kbuildIncludeTreePath{}, relative, ok, err
		}
		return kbuildIncludeTreePath{
			path: location.Directory, source: location.Tree == kconfig.CompactKbuildInvocationSourceTree,
		}, relative, true, nil
	}
	type searchFlag struct {
		name      string
		class     int
		forced    bool
		quoteOnly bool
	}
	flags := []searchFlag{
		{name: "-idirafter", class: 3},
		{name: "-isystem", class: 2},
		{name: "-iquote", class: 0, quoteOnly: true},
		{name: "-imacros", forced: true},
		{name: "-include", forced: true},
		{name: "-I", class: 1},
	}
	directoryClasses := [4][]kbuildIncludeSearchDirectory{}
	forced := []kbuildIncludeTreePath{}
	add := func(flag searchFlag, value string) error {
		pathname, relative, ok, err := treePath(value)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if flag.forced {
			trimmed := strings.TrimSpace(value)
			pathname.searchAfterDirect = relative
			if pathname.searchAfterDirect {
				pathname.searchName = pathpkg.Clean(trimmed)
			}
			forced = append(forced, pathname)
			return nil
		}
		directoryClasses[flag.class] = append(directoryClasses[flag.class], kbuildIncludeSearchDirectory{
			kbuildIncludeTreePath: pathname,
			quoteOnly:             flag.quoteOnly,
		})
		return nil
	}
	start, end, ok := kconfig.KbuildCPreprocessorArgumentRange(role, fields)
	if !ok {
		return kbuildIncludeSearchPlan{}, nil
	}
	compilerFields := fields[start:end]
	for _, field := range compilerFields {
		if field == "-I-" {
			// This obsolete GCC option changes the meaning of preceding -I
			// operands. Linux does not emit it; leaving the plan empty is safer
			// than inventing a different search order.
			return kbuildIncludeSearchPlan{}, nil
		}
	}
	flagByName := map[string]searchFlag{}
	for _, flag := range flags {
		flagByName[flag.name] = flag
	}
	for _, operand := range kconfig.KbuildCompilerIncludeOperands(compilerFields) {
		if err := add(flagByName[operand.Flag], operand.Operand); err != nil {
			return kbuildIncludeSearchPlan{}, err
		}
	}
	directories := []kbuildIncludeSearchDirectory{}
	for _, class := range directoryClasses {
		directories = append(directories, class...)
	}
	sourceSet := map[string]bool{}
	for _, source := range sourcePrerequisites {
		sourceSet[source] = true
	}
	generatedSet := map[string]bool{}
	for _, source := range generatedPrerequisites {
		generatedSet[source] = true
	}
	generatedSourceMode := role == "bindgen" || slices.Contains(compilerFields, "-c") || slices.Contains(compilerFields, "-S") || slices.Contains(compilerFields, "-E")
	selectedSources := []string{}
	selectedGeneratedSources := []string{}
	seenSources := map[string]bool{}
	seenGeneratedSources := map[string]bool{}
	for _, field := range fields {
		candidate := ""
		if pathname, _, ok, _ := treePath(field); ok {
			candidate = pathname.path
		} else {
			candidate = kconfig.CanonicalKbuildGraphTarget(field)
		}
		if sourceSet[candidate] && !seenSources[candidate] {
			seenSources[candidate] = true
			selectedSources = append(selectedSources, candidate)
		}
		if generatedSourceMode && generatedSet[candidate] && !seenGeneratedSources[candidate] {
			seenGeneratedSources[candidate] = true
			selectedGeneratedSources = append(selectedGeneratedSources, candidate)
		}
	}
	if len(selectedSources) == 0 && len(sourcePrerequisites) == 1 {
		// Some source-selected compiler wrappers do not expose their sole input
		// as a standalone argv word. Retaining that one exact prerequisite is
		// unambiguous and still keeps separate commands from cross-contaminating
		// each other's include flags.
		selectedSources = append(selectedSources, sourcePrerequisites[0])
	}
	return kbuildIncludeSearchPlan{
		sources: selectedSources, generatedSources: selectedGeneratedSources,
		directories: directories, forced: forced,
	}, nil
}

type kbuildQuotedIncludeFile struct {
	regular  bool
	missing  bool
	includes []kbuildLiteralInclude
	err      error
}

// kbuildQuotedIncludeIndex memoizes immutable source reads for one selection
// walk. Cache identity is the resolved physical file; traversal still carries
// the logical pathname separately because quoted-relative lookup depends on
// the path by which an overlay exposed that file.
type kbuildQuotedIncludeIndex struct {
	sourceRoot   string
	files        map[string]kbuildQuotedIncludeFile
	literalFiles map[string]kbuildQuotedIncludeFile
	loads        int
}

func newKbuildQuotedIncludeIndex(sourceRoot string) *kbuildQuotedIncludeIndex {
	return &kbuildQuotedIncludeIndex{
		sourceRoot:   sourceRoot,
		files:        map[string]kbuildQuotedIncludeFile{},
		literalFiles: map[string]kbuildQuotedIncludeFile{},
	}
}

func (index *kbuildQuotedIncludeIndex) cacheLiteral(key, contents string) {
	if _, ok := index.literalFiles[key]; ok {
		return
	}
	index.literalFiles[key] = kbuildQuotedIncludeFile{
		regular:  true,
		includes: kbuildLiteralIncludes([]byte(contents)),
	}
}

func (index *kbuildQuotedIncludeIndex) sourceFilename(profile kconfig.CompactKbuildProfile, logical string) string {
	filename := ""
	if resolved, ok := kconfig.ResolveCompactKbuildProfileSourcePath(profile, logical); ok {
		filename = resolved
	} else if index.sourceRoot != "" {
		filename = filepath.Join(index.sourceRoot, filepath.FromSlash(logical))
	}
	return filename
}

func (index *kbuildQuotedIncludeIndex) lookup(profile kconfig.CompactKbuildProfile, logical string) kbuildQuotedIncludeFile {
	return index.lookupFilename(logical, index.sourceFilename(profile, logical))
}

func (index *kbuildQuotedIncludeIndex) lookupFilename(logical, filename string) kbuildQuotedIncludeFile {
	cacheKey := filepath.Clean(filename)
	if filename == "" {
		cacheKey = "\x00" + logical
	}
	if cached, ok := index.files[cacheKey]; ok {
		return cached
	}
	index.loads++
	if filename == "" {
		result := kbuildQuotedIncludeFile{missing: true}
		index.files[cacheKey] = result
		return result
	}
	info, err := os.Stat(filename)
	if err != nil {
		result := kbuildQuotedIncludeFile{missing: os.IsNotExist(err)}
		if !result.missing {
			result.err = fmt.Errorf("stat quoted-include source %q: %w", logical, err)
		}
		index.files[cacheKey] = result
		return result
	}
	if !info.Mode().IsRegular() {
		result := kbuildQuotedIncludeFile{}
		index.files[cacheKey] = result
		return result
	}
	contents, err := os.ReadFile(filename)
	if err != nil {
		result := kbuildQuotedIncludeFile{
			regular: true,
			err:     fmt.Errorf("read quoted-include source %q: %w", logical, err),
		}
		index.files[cacheKey] = result
		return result
	}
	result := kbuildQuotedIncludeFile{
		regular:  true,
		includes: kbuildLiteralIncludes(contents),
	}
	index.files[cacheKey] = result
	return result
}

// kbuildSelectedSourceRelativeObjectTreeReferences closes the one compiler
// dependency class which cannot appear in Make's prerequisite list: a quoted
// include resolved relative to an immutable source file may name an output
// produced earlier in the same object tree. The source text is authoritative;
// compiler family, architecture, target names, and generated-file catalogues
// are deliberately absent from this resolver.
//
// Checked-in quoted includes are followed recursively. A source-missing include
// becomes an exact canonical demand. The later selection fixed point retains it
// only when the source-evaluated graph contains one unambiguous selected
// producer, then supplies the edge and chooses its physical toolchain stage.
func kbuildSelectedSourceRelativeObjectTreeReferences(
	sourceRoot string,
	profile kconfig.CompactKbuildProfile,
	prerequisites []string,
	satisfied map[string]bool,
) ([]string, error) {
	index := newKbuildQuotedIncludeIndex(sourceRoot)
	plans := make([]kbuildIncludeSearchPlan, 0, len(prerequisites))
	for _, prerequisite := range prerequisites {
		directory := pathpkg.Dir(prerequisite)
		if directory == "." {
			directory = ""
		}
		plans = append(plans, kbuildIncludeSearchPlan{
			sources: []string{prerequisite},
			directories: []kbuildIncludeSearchDirectory{{
				kbuildIncludeTreePath: kbuildIncludeTreePath{path: directory},
			}},
		})
	}
	resolution, err := index.references(
		profile,
		plans,
		satisfied,
		func(path string) bool {
			_, ok := kconfig.CompactKbuildProfileInitialVisibleArtifact(profile, path)
			return ok
		},
		nil,
	)
	return resolution.references, err
}

type kbuildGeneratedIncludeProjection struct {
	cacheKey string
	filename string
	contents string
	literal  bool
}

// kbuildGeneratedIncludeContentIdentityDigest keys the parsed include cache by
// stable source-derived bytes.  The cached contents themselves retain their
// authenticated capabilities; only the key drops the workload-local MAC so an
// otherwise identical replay graph has the same identity under a fresh codec.
func kbuildGeneratedIncludeContentIdentityDigest(contents string) [sha256.Size]byte {
	return sha256.Sum256([]byte(canonicalKbuildToolsetPathCapabilityIdentity(contents)))
}

type kbuildGeneratedIncludeSource func(string) (kbuildGeneratedIncludeProjection, bool, error)

type kbuildIncludeResolution struct {
	references        []string
	objectDirectories []string
	objectAllVisible  bool
}

func (index *kbuildQuotedIncludeIndex) references(
	profile kconfig.CompactKbuildProfile,
	plans []kbuildIncludeSearchPlan,
	satisfied map[string]bool,
	objectExists func(string) bool,
	objectSource kbuildGeneratedIncludeSource,
) (kbuildIncludeResolution, error) {
	type pendingIncludeSource struct {
		plan       int
		logical    string
		filename   string
		literalKey string
		object     bool
	}
	pending := []pendingIncludeSource{}
	queued := map[pendingIncludeSource]bool{}
	lookupSource := func(source pendingIncludeSource) kbuildQuotedIncludeFile {
		if source.literalKey != "" {
			return index.literalFiles[source.literalKey]
		}
		return index.lookupFilename(source.logical, source.filename)
	}
	queueSource := func(plan int, candidate, filename, literalKey string, object bool) error {
		candidate = kconfig.CanonicalKbuildGraphTarget(candidate)
		key := pendingIncludeSource{
			plan: plan, logical: candidate, filename: filename, literalKey: literalKey, object: object,
		}
		if candidate == "" || candidate == "." || queued[key] {
			return nil
		}
		file := lookupSource(key)
		if file.err != nil {
			return file.err
		}
		if !file.regular {
			return nil
		}
		queued[key] = true
		pending = append(pending, key)
		return nil
	}
	generated := map[string]bool{}
	opaqueObjectDirectories := map[string]bool{}
	opaqueObjectAllVisible := false
	observeOpaqueGeneratedSource := func(plan kbuildIncludeSearchPlan, source string) {
		parent := pathpkg.Dir(source)
		if parent == "." {
			opaqueObjectAllVisible = true
		} else {
			opaqueObjectDirectories[parent] = true
		}
		for _, directory := range plan.directories {
			if directory.source {
				continue
			}
			if directory.path == "" {
				opaqueObjectAllVisible = true
				continue
			}
			opaqueObjectDirectories[directory.path] = true
		}
	}
	objectCandidateExists := func(candidate string) bool {
		return objectExists == nil || objectExists(candidate)
	}
	resolveAt := func(plan int, location kbuildIncludeTreePath, name string) (bool, error) {
		candidate := pathpkg.Clean(pathpkg.Join(location.path, name))
		if candidate == "." || candidate == ".." || strings.HasPrefix(candidate, "../") {
			return false, nil
		}
		canonical := kconfig.CanonicalKbuildGraphTarget(candidate)
		if canonical == "" || canonical == "." || canonical != candidate {
			return false, nil
		}
		if location.source {
			filename := index.sourceFilename(profile, canonical)
			file := index.lookupFilename(canonical, filename)
			if file.err != nil {
				return false, file.err
			}
			if !file.regular {
				return false, nil
			}
			if err := queueSource(plan, canonical, filename, "", false); err != nil {
				return false, err
			}
			return true, nil
		}
		if !objectCandidateExists(canonical) {
			return false, nil
		}
		generated[canonical] = true
		if objectSource != nil {
			projection, projected, err := objectSource(canonical)
			if err != nil {
				return false, err
			}
			if projected {
				if projection.literal {
					index.cacheLiteral(projection.cacheKey, projection.contents)
				}
				if err := queueSource(plan, canonical, projection.filename, projection.cacheKey, true); err != nil {
					return false, err
				}
			} else {
				// This generated include was discovered through immutable source
				// text rather than as a compiler argv operand. Its bytes are still
				// opaque until the selected producer executes, so preserve the same
				// bounded object-tree include frontier used for an opaque generated
				// translation unit. The evaluated compiler search path, not a header
				// or architecture catalogue, defines that frontier.
				observeOpaqueGeneratedSource(plans[plan], canonical)
			}
		}
		return true, nil
	}
	resolveSearch := func(
		plan int,
		name string,
		directories []kbuildIncludeSearchDirectory,
		quoted bool,
	) (bool, error) {
		for _, directory := range directories {
			if !quoted && directory.quoteOnly {
				continue
			}
			found, err := resolveAt(plan, directory.kbuildIncludeTreePath, name)
			if err != nil || found {
				return found, err
			}
		}
		return false, nil
	}
	for planIndex, plan := range plans {
		for _, source := range plan.sources {
			candidate := kconfig.CanonicalKbuildGraphTarget(source)
			if candidate == "" || candidate != source || !satisfied[candidate] {
				continue
			}
			if err := queueSource(planIndex, candidate, index.sourceFilename(profile, candidate), "", false); err != nil {
				return kbuildIncludeResolution{}, err
			}
		}
		for _, source := range plan.generatedSources {
			candidate := kconfig.CanonicalKbuildGraphTarget(source)
			if candidate == "" || candidate != source || !objectCandidateExists(candidate) || objectSource == nil {
				observeOpaqueGeneratedSource(plan, candidate)
				continue
			}
			projection, projected, err := objectSource(candidate)
			if err != nil {
				return kbuildIncludeResolution{}, err
			}
			if !projected {
				observeOpaqueGeneratedSource(plan, candidate)
				continue
			}
			if projection.literal {
				index.cacheLiteral(projection.cacheKey, projection.contents)
			}
			if err := queueSource(planIndex, candidate, projection.filename, projection.cacheKey, true); err != nil {
				return kbuildIncludeResolution{}, err
			}
		}
		for _, forced := range plan.forced {
			found, err := resolveAt(planIndex, forced, "")
			if err != nil {
				return kbuildIncludeResolution{}, fmt.Errorf("resolve forced compiler include %q: %w", forced.path, err)
			}
			if !found && forced.searchAfterDirect {
				if _, err := resolveSearch(planIndex, forced.searchName, plan.directories, true); err != nil {
					return kbuildIncludeResolution{}, fmt.Errorf("search for forced compiler include %q: %w", forced.path, err)
				}
			}
		}
	}
	for len(pending) != 0 {
		item := pending[0]
		pending = pending[1:]
		including := item.logical
		plan := plans[item.plan]
		file := lookupSource(item)
		if file.err != nil {
			return kbuildIncludeResolution{}, file.err
		}
		if !file.regular {
			continue
		}
		for _, include := range file.includes {
			if include.path == "" || pathpkg.IsAbs(include.path) || strings.ContainsRune(include.path, '\x00') || strings.Contains(include.path, `\`) {
				continue
			}
			found := false
			if include.quoted {
				var err error
				found, err = resolveAt(item.plan, kbuildIncludeTreePath{path: pathpkg.Dir(including), source: !item.object}, include.path)
				if err != nil {
					return kbuildIncludeResolution{}, fmt.Errorf("resolve quoted include %q from %q: %w", include.path, including, err)
				}
			}
			if found {
				continue
			}
			if _, err := resolveSearch(item.plan, include.path, plan.directories, include.quoted); err != nil {
				return kbuildIncludeResolution{}, fmt.Errorf("resolve include %q from %q: %w", include.path, including, err)
			}
		}
	}
	references := make([]string, 0, len(generated))
	for reference := range generated {
		references = append(references, reference)
	}
	sort.Strings(references)
	directories := make([]string, 0, len(opaqueObjectDirectories))
	for directory := range opaqueObjectDirectories {
		directories = append(directories, directory)
	}
	sort.Strings(directories)
	return kbuildIncludeResolution{
		references: references, objectDirectories: directories, objectAllVisible: opaqueObjectAllVisible,
	}, nil
}

type kbuildLiteralInclude struct {
	path   string
	quoted bool
}

// kbuildLiteralIncludes parses literal #include "..." and #include <...>
// directives. Macro operands remain compiler-owned.
// Conditional branches are intentionally retained as a conservative union;
// exact ownership is still required from the selected source-evaluated graph.
// Backslash-newline splicing is applied before directive recognition, matching
// the preprocessing phase needed for literal include operands.
func kbuildLiteralIncludes(contents []byte) []kbuildLiteralInclude {
	text := strings.ReplaceAll(string(contents), "\\\r\n", "")
	text = strings.ReplaceAll(text, "\\\n", "")
	includes := []kbuildLiteralInclude{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
		if !strings.HasPrefix(line, "include") {
			continue
		}
		line = strings.TrimPrefix(line, "include")
		if line != "" && !strings.ContainsRune(" \t\r\f\v\"<", rune(line[0])) {
			continue
		}
		line = strings.TrimSpace(line)
		if len(line) < 2 || line[0] != '"' && line[0] != '<' {
			continue
		}
		quoted := line[0] == '"'
		terminator := byte('>')
		if quoted {
			terminator = '"'
		}
		if end := strings.IndexByte(line[1:], terminator); end >= 0 {
			includes = append(includes, kbuildLiteralInclude{path: line[1 : 1+end], quoted: quoted})
		}
	}
	return includes
}

func kbuildVisibleArtifactMatchesObjectTreeReferences(path string, references []string) bool {
	for _, reference := range references {
		if path == reference || strings.HasPrefix(path, strings.TrimSuffix(reference, "/")+"/") {
			return true
		}
	}
	return false
}

func cloneKbuildVariables(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for name, value := range values {
		cloned[name] = value
	}
	return cloned
}

// inheritKbuildInvocationCommandLineVariables models GNU Make's recursive
// MAKEOVERRIDES behavior. Assignments from the parent command line remain
// command-line variables in a sub-make, while assignments written on the
// child's argv take precedence. MAKECMDGOALS is invocation-local state derived
// from that child's argv and must never be inherited from its parent.
func inheritKbuildInvocationCommandLineVariables(
	request kbuildInvocationRequest,
	parent map[string]string,
	parentAutoExport map[string]bool,
) kbuildInvocationRequest {
	variables := make(map[string]string, len(parent)+len(request.variables))
	autoExport := maps.Clone(request.commandLineAutoExport)
	if autoExport == nil {
		autoExport = map[string]bool{}
	}
	if !request.suppressParentCommandLine {
		for name, value := range parent {
			if name != "MAKECMDGOALS" {
				variables[name] = value
				if parentAutoExport[name] {
					autoExport[name] = true
				}
			}
		}
	}
	for name, value := range request.variables {
		variables[name] = value
	}
	request.variables = variables
	request.commandLineAutoExport = autoExport
	return request
}

// kbuildInvocationSourceOverlayRequest preserves GNU Make's `-C M` control
// flow while splitting an external module directory into immutable sources and
// writable objects. A nested declared source root supplies the former; the
// same canonical directory below the object tree supplies the latter. The
// broad kernel source root is installed separately by ensureProfile and is not
// an overlay.
func kbuildInvocationSourceOverlayRequest(
	request kbuildInvocationRequest,
	sourceRoots map[string]string,
) kbuildInvocationRequest {
	if request.processLocation.Tree != kconfig.CompactKbuildInvocationSourceTree {
		return request
	}
	virtual := kbuildEvalSourceTree
	if request.processLocation.Directory != "" {
		virtual += "/" + strings.Trim(request.processLocation.Directory, "/")
	}
	if kbuildInvocationSourceOverlayPath(virtual, sourceRoots) {
		request.processLocation.Tree = kconfig.CompactKbuildInvocationObjectTree
	}
	return request
}

func kbuildInvocationSourceOverlayPath(virtual string, sourceRoots map[string]string) bool {
	virtual = filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(virtual))))
	prefix := kbuildEvalSourceTree + "/"
	if !strings.HasPrefix(virtual, prefix) {
		return false
	}
	directory := kconfig.CanonicalKbuildGraphTarget(strings.TrimPrefix(virtual, prefix))
	if directory == "" || directory == "." {
		return false
	}
	for _, overlay := range kbuildFrontierSourceOverlayDirectories(sourceRoots) {
		if directory == overlay || strings.HasPrefix(directory, overlay+"/") {
			return true
		}
	}
	return false
}

// canonicalKbuildVisibleArtifacts resolves the current frontier in source
// order. A later producer of the same canonical path replaces its earlier
// owner, then the surviving records are sorted for stable request identities
// and serialization.
func canonicalKbuildVisibleArtifacts(artifacts []kconfig.CompactKbuildVisibleArtifact) []kconfig.CompactKbuildVisibleArtifact {
	byPath := make(map[string]kconfig.CompactKbuildVisibleArtifact, len(artifacts))
	for _, artifact := range artifacts {
		artifact.Path = kconfig.CanonicalKbuildGraphTarget(artifact.Path)
		artifact.Target = kconfig.CanonicalKbuildGraphTarget(artifact.Target)
		if artifact.Path == "" || artifact.Path == "." {
			continue
		}
		byPath[artifact.Path] = artifact
	}
	out := make([]kconfig.CompactKbuildVisibleArtifact, 0, len(byPath))
	for _, artifact := range byPath {
		out = append(out, artifact)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Path < out[j].Path
	})
	return out
}

func kbuildInvocationRequestKey(request kbuildInvocationRequest) string {
	return canonicalKbuildInvocationRequestKey(request)
}

func canonicalKbuildInvocationRequestKey(request kbuildInvocationRequest) string {
	parts := []string{}
	forEachCanonicalKbuildInvocationRequestKeyPart(request, func(part string) {
		parts = append(parts, part)
	})
	return strings.Join(parts, "\x00")
}

func canonicalKbuildInvocationRequestDigest(request kbuildInvocationRequest) [sha256.Size]byte {
	hash := sha256.New()
	first := true
	forEachCanonicalKbuildInvocationRequestKeyPart(request, func(part string) {
		if !first {
			_, _ = hash.Write([]byte{0})
		}
		_, _ = hash.Write([]byte(part))
		first = false
	})
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}

func forEachCanonicalKbuildInvocationRequestKeyPart(
	request kbuildInvocationRequest,
	visit func(string),
) {
	for _, part := range []string{
		request.name,
		request.makefile,
		request.directory,
		string(request.processLocation.Tree),
		request.processLocation.Directory,
		strings.Join(request.entryTargets, "\x1f"),
	} {
		visit(part)
	}
	if request.suppressParentCommandLine {
		visit("suppress-parent-command-line")
	}
	for _, predecessor := range request.invocationPredecessors {
		visit("invocation-predecessor=" + predecessor)
	}
	visibleDigest := kbuildFrontierDigest(request.visibleState)
	visit("visible-frontier=" + hex.EncodeToString(visibleDigest[:]))
	environmentNames := make([]string, 0, len(request.environment))
	for name := range request.environment {
		environmentNames = append(environmentNames, name)
	}
	sort.Strings(environmentNames)
	for _, name := range environmentNames {
		visit("environment:" + name + "=" + canonicalKbuildToolsetPathCapabilityIdentity(request.environment[name]))
	}
	names := make([]string, 0, len(request.variables))
	for name := range request.variables {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		visit("command-line:" + name + "=" + canonicalKbuildToolsetPathCapabilityIdentity(request.variables[name]))
		if request.commandLineAutoExport[name] {
			visit("command-line-export:" + name)
		}
	}
}

// canonicalKbuildToolsetPathCapabilityIdentity removes only the ephemeral MAC
// from well-formed planning capabilities before an intermediate stable hash is
// computed. Keyed verification remains mandatory when the value enters an
// action recipe; malformed values are left unchanged so they cannot be made
// valid by an identity-only projection.
func canonicalKbuildToolsetPathCapabilityIdentity(value string) string {
	canonical, err := toolaction.CanonicalizeExecutionRootProvenanceCapabilityIdentity(value)
	if err != nil {
		return value
	}
	return canonical
}

// marshalCanonicalKbuildToolsetPathCapabilityIdentity recursively projects
// every JSON string value onto its stable capability identity.  Marshaling
// through an independent value keeps the authenticated planning data owned by
// the caller byte-for-byte intact for the later keyed recipe boundary.  UseNumber
// also preserves integer identities which exceed the exact float64 range.
func marshalCanonicalKbuildToolsetPathCapabilityIdentity(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var identity any
	if err := decoder.Decode(&identity); err != nil {
		return nil, err
	}
	var canonicalize func(any) any
	canonicalize = func(value any) any {
		switch value := value.(type) {
		case string:
			return canonicalKbuildToolsetPathCapabilityIdentity(value)
		case []any:
			for index := range value {
				value[index] = canonicalize(value[index])
			}
		case map[string]any:
			for name, field := range value {
				value[name] = canonicalize(field)
			}
		}
		return value
	}
	return json.Marshal(canonicalize(identity))
}

func canonicalKbuildInvocationRequestsEqual(left, right kbuildInvocationRequest) bool {
	if left.name != right.name ||
		left.makefile != right.makefile ||
		left.directory != right.directory ||
		left.processLocation != right.processLocation ||
		left.suppressParentCommandLine != right.suppressParentCommandLine ||
		!slices.Equal(left.entryTargets, right.entryTargets) ||
		!slices.Equal(left.invocationPredecessors, right.invocationPredecessors) ||
		!maps.Equal(left.environment, right.environment) ||
		!maps.Equal(left.variables, right.variables) {
		return false
	}
	if kbuildFrontierDigest(left.visibleState) != kbuildFrontierDigest(right.visibleState) {
		return false
	}
	if kbuildFrontierRawDigest(left.visibleState) != kbuildFrontierRawDigest(right.visibleState) {
		return false
	}
	for name := range left.variables {
		if left.commandLineAutoExport[name] != right.commandLineAutoExport[name] {
			return false
		}
	}
	return true
}

// kbuildInvocationGeneratedTextProjectionFromFrontier resolves only the cat
// operands actually read by the compact generated-text language. Flattening
// every exact file at every source-ordered write makes a growing recursive-Make
// frontier quadratic even when the recipe is a plain echo or printf.
func kbuildInvocationGeneratedTextProjectionFromFrontier(
	profile kconfig.CompactKbuildProfile,
	recipe, target string,
	state kbuildFrontierState,
) (string, bool, error) {
	location, ok := kconfig.CompactKbuildProfileInvocationLocation(profile)
	if !ok {
		return "", false, fmt.Errorf("profile %q has no typed Kbuild invocation location", profile.Name)
	}
	resolveExact := func(path string) (string, bool) {
		path = filepath.ToSlash(path)
		rooted := false
		prepPrefix := "${tree:prep}/"
		objectPrefix := kbuildEvalObjectTree + "/"
		if strings.HasPrefix(path, prepPrefix) {
			path = strings.TrimPrefix(path, prepPrefix)
			rooted = true
		} else if strings.HasPrefix(path, objectPrefix) {
			path = strings.TrimPrefix(path, objectPrefix)
			rooted = true
		}
		if !rooted && location.Directory != "" {
			path = filepath.ToSlash(filepath.Join(location.Directory, path))
		}
		path = kconfig.CanonicalKbuildGraphTarget(path)
		value, found := kbuildFrontierGet(state, path)
		if !found || !value.exact {
			return "", false
		}
		return value.content, true
	}
	for _, alias := range kbuildInvocationGeneratedTextTargetAliases(target, location.Directory) {
		if data, exact := kconfig.CompactKbuildGeneratedTextProjectionWithResolver(
			recipe, alias, resolveExact,
		); exact {
			return data, true, nil
		}
	}
	return "", false, nil
}

func kbuildInvocationGeneratedTextTargetAliases(file, directory string) []string {
	aliases := kbuildInvocationVirtualPathAliases(file, directory)
	file = kconfig.CanonicalKbuildGraphTarget(file)
	if file != "" && file != "." {
		aliases = append(aliases, "${tree:prep}/"+file)
	}
	return aliases
}

func kbuildInvocationVirtualPathAliases(file, directory string) []string {
	first, second := kbuildInvocationVirtualPathAliasPair(file, directory)
	if first == "" {
		return nil
	}
	if second == "" {
		return []string{first}
	}
	if second < first {
		first, second = second, first
	}
	return []string{first, second}
}

func kbuildInvocationVirtualPathAliasPair(file, directory string) (string, string) {
	file = filepath.ToSlash(strings.TrimPrefix(file, "./"))
	if file == "" || file == "." || strings.HasPrefix(file, "../") || filepath.IsAbs(file) {
		return "", ""
	}
	first := kbuildEvalObjectTree + "/" + file
	second := ""
	directory = filepath.ToSlash(strings.Trim(directory, "/"))
	if directory == "" {
		second = file
	} else {
		if relative, err := filepath.Rel(filepath.FromSlash(directory), filepath.FromSlash(file)); err == nil {
			second = filepath.ToSlash(relative)
		}
	}
	if first == second {
		second = ""
	}
	return first, second
}

func kbuildInvocationProfileName(request kbuildInvocationRequest) string {
	return kbuildInvocationProfileNameFromKey(request.name, kbuildInvocationRequestKey(request))
}

func kbuildInvocationProfileNameFromKey(name, requestKey string) string {
	digest := sha256.Sum256([]byte(requestKey))
	return kbuildInvocationProfileNameFromDigest(name, digest)
}

func kbuildInvocationProfileNameFromDigest(name string, digest [sha256.Size]byte) string {
	return name + "#" + hex.EncodeToString(digest[:6])
}

func kbuildRecursiveMakeRequest(command, parentProcessDirectory string) (kbuildInvocationRequest, bool, error) {
	return kbuildRecursiveMakeRequestAt(command, kconfig.CompactKbuildInvocationLocation{
		Tree: kconfig.CompactKbuildInvocationObjectTree, Directory: parentProcessDirectory,
	})
}

func kbuildRecursiveMakeRequestAt(
	command string,
	parentLocation kconfig.CompactKbuildInvocationLocation,
) (kbuildInvocationRequest, bool, error) {
	commands, _, err := kbuildShellSimpleCommands(command)
	if err != nil {
		if strings.Contains(command, kbuildEvalRecursiveMake) {
			return kbuildInvocationRequest{}, false, err
		}
		return kbuildInvocationRequest{}, false, nil
	}
	for _, simple := range commands {
		if len(simple.argv) == 0 || strings.TrimLeft(simple.argv[0], "+@-") != kbuildEvalRecursiveMake {
			continue
		}
		request, err := kbuildRecursiveMakeRequestArgvAt(simple.argv[1:], simple.assignments, parentLocation)
		return request, err == nil, err
	}
	return kbuildInvocationRequest{}, false, nil
}

func kbuildRecursiveMakeRequestArgvAt(
	fields []string,
	inlineEnvironment map[string]string,
	parentLocation kconfig.CompactKbuildInvocationLocation,
) (kbuildInvocationRequest, error) {
	makefile := ""
	makefileRooted := false
	processLocation := parentLocation
	var err error
	if processLocation.Tree == "" {
		processLocation.Tree = kconfig.CompactKbuildInvocationObjectTree
	}
	processLocation.Directory = filepath.ToSlash(strings.Trim(processLocation.Directory, "/"))
	assignments := map[string]string{}
	rawGoals := []string{}
	for index := 0; index < len(fields); index++ {
		field := fields[index]
		switch field {
		case "-f", "-C":
			if index+1 >= len(fields) {
				return kbuildInvocationRequest{}, fmt.Errorf("recursive make %s has no argument", field)
			}
			value := fields[index+1]
			index++
			if field == "-f" {
				makefile, makefileRooted = kbuildInvocationMakefilePath(value, processLocation.Directory)
			} else {
				processLocation, err = kbuildInvocationDirectoryLocation(value, processLocation)
				if err != nil {
					return kbuildInvocationRequest{}, err
				}
			}
			continue
		}
		if value, ok := strings.CutPrefix(field, "-f"); ok && value != "" {
			makefile, makefileRooted = kbuildInvocationMakefilePath(value, processLocation.Directory)
			continue
		}
		if value, ok := strings.CutPrefix(field, "-C"); ok && value != "" {
			processLocation, err = kbuildInvocationDirectoryLocation(value, processLocation)
			if err != nil {
				return kbuildInvocationRequest{}, err
			}
			continue
		}
		if value, ok := strings.CutPrefix(field, "--file="); ok {
			makefile, makefileRooted = kbuildInvocationMakefilePath(value, processLocation.Directory)
			continue
		}
		if value, ok := strings.CutPrefix(field, "--directory="); ok {
			processLocation, err = kbuildInvocationDirectoryLocation(value, processLocation)
			if err != nil {
				return kbuildInvocationRequest{}, err
			}
			continue
		}
		if name, value, ok := strings.Cut(field, "="); ok && kbuildMakeAssignmentName(name) {
			assignments[name] = value
			continue
		}
		if strings.HasPrefix(field, "-") {
			continue
		}
		rawGoals = append(rawGoals, field)
	}
	if !makefileRooted && makefile != "" {
		makefile = canonicalKbuildProfilePath(filepath.Join(processLocation.Directory, filepath.FromSlash(makefile)), "")
	}
	assignments["MAKECMDGOALS"] = strings.Join(rawGoals, " ")
	commandLineAutoExport := map[string]bool{}
	for name := range assignments {
		if name != "MAKECMDGOALS" {
			commandLineAutoExport[name] = true
		}
	}
	if makefile == "" {
		makefile = canonicalKbuildProfilePath(filepath.Join(processLocation.Directory, "Makefile"), "")
	}
	if makefile == "" {
		return kbuildInvocationRequest{}, fmt.Errorf("recursive make has no source-tree driver")
	}
	directory := processLocation.Directory
	_, hasObjectDirectory := assignments["obj"]
	if hasObjectDirectory {
		objectDirectory := kbuildInvocationObjectPath(assignments["obj"])
		directory = objectDirectory
		if objectDirectory == "" && strings.TrimSpace(assignments["obj"]) == "." && processLocation.Directory != "" {
			// In an external-module root self-submake, Linux runs
			// scripts/Makefile.build with obj=. from inside M. Make must still see
			// the literal dot so src=$(srcroot)/$(obj) remains source-relative,
			// while graph targets and writable outputs belong to the typed process
			// directory below the object overlay.
			directory = processLocation.Directory
		}
		// Replay argv retains the object-tree marker, but the child Make sees
		// obj as its logical directory relative to objtree. Keeping the marker
		// in CommandLineVariables would make scripts/Makefile.build derive
		// src=$(srcroot)/__LINUX_BZL_OBJECT_TREE__/..., and consequently include
		// a nonexistent object-tree Makefile from beneath the source tree.
		assignments["obj"] = objectDirectory
		if assignments["obj"] == "" {
			assignments["obj"] = "."
		}
		// Command-line goals are literal Make target identities, not paths
		// relative to Kbuild's logical obj= directory.  In particular,
		// `$(MAKE) $(build)=arch/x86/entry/syscalls all` selects the literal
		// phony target `all`; obj= only makes that target's generated paths live
		// below arch/x86/entry/syscalls.  Rooting the goal at obj= silently turns
		// it into an unrelated arch/x86/entry/syscalls/all target.
		goalDirectory := ""
		if objectDirectory == "" && strings.TrimSpace(assignments["obj"]) == "." {
			goalDirectory = processLocation.Directory
		}
		entryTargets := kbuildInvocationEntryTargets("", rawGoals)
		if goalDirectory != "" {
			for index, target := range entryTargets {
				if target == kbuildEvalSourceTree || target == kbuildEvalObjectTree ||
					strings.HasPrefix(target, kbuildEvalSourceTree+"/") || strings.HasPrefix(target, kbuildEvalObjectTree+"/") {
					continue
				}
				entryTargets[index] = kconfig.CanonicalKbuildGraphTarget(pathpkg.Join(goalDirectory, target))
			}
		}
		return kbuildInvocationRequest{
			name: "build:" + directory, makefile: makefile, directory: directory, processLocation: processLocation,
			entryTargets: entryTargets, environment: inlineEnvironment, variables: assignments,
			commandLineAutoExport: commandLineAutoExport,
		}, nil
	}
	name := "driver:" + makefile
	makefileDirectory := filepath.ToSlash(filepath.Dir(makefile))
	if makefileDirectory == "." {
		makefileDirectory = ""
	}
	if processLocation.Directory != makefileDirectory {
		workingName := processLocation.Directory
		if workingName == "" {
			workingName = "."
		}
		name += "@" + workingName
	}
	return kbuildInvocationRequest{
		name: name, makefile: makefile, directory: processLocation.Directory, processLocation: processLocation,
		entryTargets:          kbuildInvocationEntryTargets(processLocation.Directory, rawGoals),
		environment:           inlineEnvironment,
		variables:             assignments,
		commandLineAutoExport: commandLineAutoExport,
	}, nil
}

func kbuildInvocationEntryTargets(directory string, goals []string) []string {
	targets := make([]string, 0, len(goals))
	profile := kconfig.CompactKbuildProfile{Directory: directory}
	for _, goal := range goals {
		goal = strings.TrimSpace(goal)
		// Preserve explicit source/object-tree provenance until the child
		// invocation has been evaluated. Canonicalizing this marker through a
		// synthetic Directory-only profile loses the distinction between an
		// OUTPUT-rooted goal and one relative to `make -C`.
		if goal == kbuildEvalSourceTree || goal == kbuildEvalObjectTree ||
			strings.HasPrefix(goal, kbuildEvalSourceTree+"/") || strings.HasPrefix(goal, kbuildEvalObjectTree+"/") {
			targets = append(targets, goal)
			continue
		}
		if target := kbuildProfileTarget(profile, goal); target != "" {
			targets = append(targets, target)
		}
	}
	return uniquePathsInOrder(targets)
}

func kbuildInvocationMakefilePath(value, workingDirectory string) (string, bool) {
	value = strings.TrimSpace(value)
	for _, sentinel := range []string{kbuildEvalSourceTree, kbuildEvalObjectTree} {
		if value == sentinel {
			return "", true
		}
		if suffix, ok := strings.CutPrefix(value, sentinel+"/"); ok {
			return canonicalKbuildProfilePath(suffix, ""), true
		}
	}
	return canonicalKbuildProfilePath(value, ""), false
}

func kbuildInvocationDirectoryLocation(
	value string,
	parent kconfig.CompactKbuildInvocationLocation,
) (kconfig.CompactKbuildInvocationLocation, error) {
	value = filepath.ToSlash(strings.TrimSpace(value))
	for _, root := range []struct {
		marker string
		tree   kconfig.CompactKbuildInvocationTree
	}{
		{marker: kbuildEvalSourceTree, tree: kconfig.CompactKbuildInvocationSourceTree},
		{marker: kbuildEvalObjectTree, tree: kconfig.CompactKbuildInvocationObjectTree},
	} {
		if value == root.marker {
			return kconfig.CompactKbuildInvocationLocation{Tree: root.tree}, nil
		}
		if suffix, ok := strings.CutPrefix(value, root.marker+"/"); ok {
			directory, err := canonicalKbuildInvocationDirectory(suffix)
			if err != nil {
				return kconfig.CompactKbuildInvocationLocation{}, err
			}
			return kconfig.CompactKbuildInvocationLocation{Tree: root.tree, Directory: directory}, nil
		}
	}
	if parent.Tree == "" {
		parent.Tree = kconfig.CompactKbuildInvocationObjectTree
	}
	directory, err := canonicalKbuildInvocationDirectory(pathpkg.Join(parent.Directory, value))
	if err != nil {
		return kconfig.CompactKbuildInvocationLocation{}, fmt.Errorf(
			"recursive make -C path %q from %s tree directory %q: %w",
			value, parent.Tree, parent.Directory, err,
		)
	}
	return kconfig.CompactKbuildInvocationLocation{Tree: parent.Tree, Directory: directory}, nil
}

func canonicalKbuildInvocationDirectory(value string) (string, error) {
	value = filepath.ToSlash(strings.TrimSpace(value))
	canonical := pathpkg.Clean(value)
	if canonical == "." {
		return "", nil
	}
	if value == "" || pathpkg.IsAbs(canonical) || canonical == ".." || strings.HasPrefix(canonical, "../") ||
		strings.Contains(canonical, `\`) || strings.ContainsRune(canonical, 0) {
		return "", fmt.Errorf("path %q escapes its declared tree", value)
	}
	return canonical, nil
}

func kbuildInvocationObjectPath(value string) string {
	value = strings.TrimSpace(value)
	for _, sentinel := range []string{kbuildEvalSourceTree, kbuildEvalObjectTree} {
		if value == sentinel {
			return ""
		}
		value = strings.TrimPrefix(value, sentinel+"/")
	}
	return canonicalKbuildProfilePath(value, "")
}

func kbuildMakeAssignmentName(value string) bool {
	if value == "" || value[0] >= '0' && value[0] <= '9' {
		return false
	}
	for _, character := range value {
		if character == '_' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '.' {
			continue
		}
		return false
	}
	return true
}

type kbuildShellLexemeKind uint8

const (
	kbuildShellWordLexeme kbuildShellLexemeKind = iota
	kbuildShellConnectorLexeme
	kbuildShellRedirectionLexeme
)

type kbuildShellLexeme struct {
	value          string
	kind           kbuildShellLexemeKind
	ioNumber       string
	plain          bool
	assignmentName string
}

type kbuildShellRedirection struct {
	ioNumber string
	operator string
	operand  string
}

type kbuildShellSimpleCommand struct {
	assignments  map[string]string
	argv         []string
	redirections []kbuildShellRedirection
}

func kbuildShellLexemeCommandSegments(fields []kbuildShellLexeme) ([][]kbuildShellLexeme, []string) {
	segments := [][]kbuildShellLexeme{}
	connectors := []string{}
	current := []kbuildShellLexeme{}
	finish := func() {
		if len(current) == 0 {
			return
		}
		segments = append(segments, current)
		current = nil
	}
	for _, field := range fields {
		if field.kind != kbuildShellConnectorLexeme {
			current = append(current, field)
			continue
		}
		finish()
		connectors = append(connectors, field.value)
	}
	finish()
	return segments, connectors
}

func kbuildParseShellSimpleCommand(fields []kbuildShellLexeme) (kbuildShellSimpleCommand, error) {
	command := kbuildShellSimpleCommand{assignments: map[string]string{}}
	prefix := true
	for index := 0; index < len(fields); index++ {
		field := fields[index]
		switch field.kind {
		case kbuildShellConnectorLexeme:
			return kbuildShellSimpleCommand{}, fmt.Errorf("unexpected shell connector %q inside simple command", field.value)
		case kbuildShellRedirectionLexeme:
			if index+1 >= len(fields) || fields[index+1].kind != kbuildShellWordLexeme {
				return kbuildShellSimpleCommand{}, fmt.Errorf("shell redirection %q has no word operand", field.ioNumber+field.value)
			}
			command.redirections = append(command.redirections, kbuildShellRedirection{
				ioNumber: field.ioNumber,
				operator: field.value,
				operand:  fields[index+1].value,
			})
			index++
		case kbuildShellWordLexeme:
			if prefix {
				if field.plain && kbuildShellReservedCommandPrefix(field.value) {
					continue
				}
				if field.assignmentName != "" {
					_, value, _ := strings.Cut(field.value, "=")
					command.assignments[field.assignmentName] = value
					continue
				}
				prefix = false
			}
			command.argv = append(command.argv, field.value)
		default:
			return kbuildShellSimpleCommand{}, fmt.Errorf("unknown shell lexeme kind %d", field.kind)
		}
	}
	return command, nil
}

func kbuildShellReservedCommandPrefix(value string) bool {
	switch value {
	case "!", "{", "if", "then", "elif", "else", "while", "until", "do":
		return true
	default:
		return false
	}
}

func kbuildShellSimpleCommands(command string) ([]kbuildShellSimpleCommand, []string, error) {
	fields, err := kbuildShellLexemes(command)
	if err != nil {
		return nil, nil, err
	}
	segments, connectors := kbuildShellLexemeCommandSegments(fields)
	commands := make([]kbuildShellSimpleCommand, 0, len(segments))
	for _, segment := range segments {
		simple, err := kbuildParseShellSimpleCommand(segment)
		if err != nil {
			return nil, nil, err
		}
		commands = append(commands, simple)
	}
	return commands, connectors, nil
}

func kbuildSingleShellSimpleCommand(command string) (kbuildShellSimpleCommand, bool, error) {
	commands, connectors, err := kbuildShellSimpleCommands(command)
	if err != nil {
		return kbuildShellSimpleCommand{}, false, err
	}
	if len(commands) != 1 || len(connectors) != 0 {
		return kbuildShellSimpleCommand{}, false, nil
	}
	return commands[0], true, nil
}

// kbuildShellLexemes performs the small, deterministic subset of shell lexical
// processing needed to recover recursive Make argv. Words, execution
// connectors, and redirections retain distinct kinds so quoted operator text
// cannot acquire shell syntax after quote removal and re-rendering.
func kbuildShellLexemes(command string) ([]kbuildShellLexeme, error) {
	lexemes := []kbuildShellLexeme{}
	var word strings.Builder
	quote := byte(0)
	started := false
	wordQuoted := false
	assignmentPossible := true
	assignmentName := ""
	flush := func() {
		if !started {
			return
		}
		lexemes = append(lexemes, kbuildShellLexeme{
			value: word.String(), plain: !wordQuoted, assignmentName: assignmentName,
		})
		word.Reset()
		started = false
		wordQuoted = false
		assignmentPossible = true
		assignmentName = ""
	}
	for index := 0; index < len(command); index++ {
		character := command[index]
		if quote != 0 {
			if character == quote {
				quote = 0
				continue
			}
			if quote == '"' && character == '\\' {
				if index+1 >= len(command) {
					return nil, fmt.Errorf("unterminated escape in %q", command)
				}
				next := command[index+1]
				switch next {
				case '\n':
					index++
				case '$', '`', '"', '\\':
					word.WriteByte(next)
					started = true
					index++
				default:
					word.WriteByte('\\')
					word.WriteByte(next)
					started = true
					index++
				}
				continue
			}
			word.WriteByte(character)
			started = true
			continue
		}
		if character == '\\' {
			if index+1 >= len(command) {
				return nil, fmt.Errorf("unterminated escape in %q", command)
			}
			next := command[index+1]
			if next != '\n' {
				word.WriteByte(next)
				started = true
				wordQuoted = true
				if assignmentName == "" {
					assignmentPossible = false
				}
			}
			index++
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			started = true
			wordQuoted = true
			if assignmentName == "" {
				assignmentPossible = false
			}
			continue
		}
		if character == '\n' {
			flush()
			lexemes = append(lexemes, kbuildShellLexeme{value: "\n", kind: kbuildShellConnectorLexeme})
			continue
		}
		if character == ' ' || character == '\t' || character == '\r' {
			flush()
			continue
		}
		if character == '#' && !started {
			remainder := strings.IndexByte(command[index:], '\n')
			if remainder < 0 {
				break
			}
			lexemes = append(lexemes, kbuildShellLexeme{value: "\n", kind: kbuildShellConnectorLexeme})
			index += remainder
			continue
		}
		if character == ';' || character == '|' {
			flush()
			operator := string(character)
			if character == '|' && index+1 < len(command) && command[index+1] == '|' {
				operator += string(character)
				index++
			}
			lexemes = append(lexemes, kbuildShellLexeme{value: operator, kind: kbuildShellConnectorLexeme})
			continue
		}
		if character == '&' {
			flush()
			if index+1 < len(command) && command[index+1] == '>' {
				operator := "&>"
				index++
				if index+1 < len(command) && command[index+1] == '>' {
					operator = "&>>"
					index++
				}
				lexemes = append(lexemes, kbuildShellLexeme{value: operator, kind: kbuildShellRedirectionLexeme})
				continue
			}
			operator := "&"
			if index+1 < len(command) && command[index+1] == '&' {
				operator = "&&"
				index++
			}
			lexemes = append(lexemes, kbuildShellLexeme{value: operator, kind: kbuildShellConnectorLexeme})
			continue
		}
		if character == '<' || character == '>' {
			ioNumber := ""
			previousNeedsOperand := len(lexemes) != 0 && lexemes[len(lexemes)-1].kind == kbuildShellRedirectionLexeme
			if started && !previousNeedsOperand && !wordQuoted && kbuildShellDigits(word.String()) {
				ioNumber = word.String()
				word.Reset()
				started = false
				wordQuoted = false
				assignmentPossible = true
				assignmentName = ""
			} else {
				flush()
			}
			operator := string(character)
			if index+1 < len(command) {
				next := command[index+1]
				switch {
				case character == '<' && next == '<':
					return nil, fmt.Errorf("heredoc redirection is not supported in %q", command)
				case character == '<' && (next == '&' || next == '>'):
					operator += string(next)
					index++
				case character == '>' && (next == '>' || next == '&' || next == '|'):
					operator += string(next)
					index++
				}
			}
			lexemes = append(lexemes, kbuildShellLexeme{
				value: operator, kind: kbuildShellRedirectionLexeme, ioNumber: ioNumber,
			})
			continue
		}
		if character == '=' && assignmentName == "" && assignmentPossible && kbuildShellVariableName(word.String()) {
			assignmentName = word.String()
		}
		word.WriteByte(character)
		started = true
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote in %q", command)
	}
	flush()
	return lexemes, nil
}

func kbuildShellDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func matchMakeTargetPattern(pattern, target string) (string, bool) {
	prefix, suffix, patternRule := strings.Cut(pattern, "%")
	if !patternRule {
		return "", pattern == target
	}
	if strings.Contains(suffix, "%") || len(target) < len(prefix)+len(suffix) || !strings.HasPrefix(target, prefix) || !strings.HasSuffix(target, suffix) {
		return "", false
	}
	return target[len(prefix) : len(target)-len(suffix)], true
}

func canonicalKbuildProfilePath(value, rootDir string) string {
	value = strings.TrimSpace(value)
	if fields := strings.Fields(value); len(fields) != 1 {
		return ""
	}
	for _, prefix := range []string{"$(srctree)/", "${srctree}/", "$(objtree)/", "${objtree}/"} {
		value = strings.TrimPrefix(value, prefix)
	}
	if filepath.IsAbs(value) {
		rel, err := filepath.Rel(rootDir, value)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return ""
		}
		value = rel
	}
	value = filepath.ToSlash(filepath.Clean(value))
	if value == "." || strings.HasPrefix(value, "../") {
		return ""
	}
	return value
}

func sortedUniquePaths(paths []string) []string {
	out := make([]string, len(paths))
	for index, path := range paths {
		out[index] = filepath.ToSlash(filepath.Clean(path))
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// firstCanonicalJSONDifference reports the first stable field path which made
// two content-addressed action contracts differ. Ambiguous producer failures
// are otherwise nearly impossible to diagnose from profile hashes alone.
func firstCanonicalJSONDifference(left, right []byte) string {
	if bytes.Equal(left, right) {
		return "no serialized field difference"
	}
	return firstCanonicalJSONDifferenceAt("contract", bytes.TrimSpace(left), bytes.TrimSpace(right))
}

func firstCanonicalJSONDifferenceAt(path string, left, right []byte) string {
	if bytes.Equal(left, right) {
		return ""
	}
	if len(left) != 0 && len(right) != 0 && left[0] == '{' && right[0] == '{' {
		var leftFields, rightFields map[string]json.RawMessage
		if json.Unmarshal(left, &leftFields) == nil && json.Unmarshal(right, &rightFields) == nil {
			keys := make([]string, 0, len(leftFields)+len(rightFields))
			seen := map[string]bool{}
			for key := range leftFields {
				seen[key] = true
				keys = append(keys, key)
			}
			for key := range rightFields {
				if !seen[key] {
					keys = append(keys, key)
				}
			}
			sort.Strings(keys)
			for _, key := range keys {
				leftValue, leftOK := leftFields[key]
				rightValue, rightOK := rightFields[key]
				fieldPath := path + "." + key
				if !leftOK || !rightOK {
					return fmt.Sprintf("%s presence differs (%t versus %t)", fieldPath, leftOK, rightOK)
				}
				if difference := firstCanonicalJSONDifferenceAt(fieldPath, leftValue, rightValue); difference != "" {
					return difference
				}
			}
		}
	}
	if len(left) != 0 && len(right) != 0 && left[0] == '[' && right[0] == '[' {
		var leftValues, rightValues []json.RawMessage
		if json.Unmarshal(left, &leftValues) == nil && json.Unmarshal(right, &rightValues) == nil {
			if len(leftValues) != len(rightValues) {
				return fmt.Sprintf("%s length differs (%d versus %d)", path, len(leftValues), len(rightValues))
			}
			for index := range leftValues {
				if difference := firstCanonicalJSONDifferenceAt(
					fmt.Sprintf("%s[%d]", path, index), leftValues[index], rightValues[index],
				); difference != "" {
					return difference
				}
			}
		}
	}
	return fmt.Sprintf("%s differs (%s versus %s)", path, canonicalJSONSummary(left), canonicalJSONSummary(right))
}

func canonicalJSONSummary(value []byte) string {
	if len(value) <= 160 {
		return string(value)
	}
	digest := sha256.Sum256(value)
	return fmt.Sprintf("sha256:%s/%d-bytes", hex.EncodeToString(digest[:]), len(value))
}

func uniquePathsInOrder(paths []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = filepath.Clean(path)
		if seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}

func linuxRootMakeInvocationVariables(rootDir string) map[string]string {
	rootDir = filepath.ToSlash(rootDir)
	return map[string]string{
		"CURDIR":        rootDir,
		"MAKEFLAGS":     "--no-print-directory",
		"MAKECMDGOALS":  "all",
		"KBUILD_EXTMOD": "",
		"KBUILD_OUTPUT": "",
		"need-sub-make": "",
		"abs_output":    rootDir,
		"abs_srctree":   rootDir,
		"objtree":       rootDir,
		"srcroot":       rootDir,
		"srctree":       rootDir,
	}
}

func linuxRootKconfigInvocationVariables(rootDir string) map[string]string {
	variables := linuxRootMakeInvocationVariables(rootDir)
	// Linux exports the source-derived compiler version fields to Kconfig only
	// for a %config goal. No recipe is executed here, so use one canonical goal
	// to select that source-owned environment independently of config mode.
	variables["MAKECMDGOALS"] = "olddefconfig"
	return variables
}

func readToolsetIdentity(root string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("identity directory is required")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	if len(entries) != 1 {
		return "", fmt.Errorf("identity directory %q contains %d entries, want exactly one", root, len(entries))
	}
	entry := entries[0]
	name := entry.Name()
	digest, ok := strings.CutPrefix(name, "sha256-")
	if !ok || len(digest) != 64 || strings.ToLower(digest) != digest {
		return "", fmt.Errorf("identity marker %q is not canonical sha256-<digest>", name)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", fmt.Errorf("identity marker %q is not canonical sha256-<digest>: %w", name, err)
	}
	info, err := os.Stat(filepath.Join(root, name))
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() != 0 {
		return "", fmt.Errorf("identity marker %q must be an empty regular file", name)
	}
	return name, nil
}

func kbuildVariablesForConfig(base map[string]string, tree *kconfig.Tree, config *kconfig.ResolvedConfig) (map[string]string, error) {
	// scripts/Kbuild.include defines this before the architecture Makefile is
	// evaluated by Kbuild. Kbuild action planning starts at that Makefile.
	vars := map[string]string{"comma": ","}
	for key, value := range base {
		vars[key] = value
	}
	for key, value := range config.Effective {
		if !config.ShouldWrite(key) || value == "n" {
			vars[key] = ""
			continue
		}
		vars[key] = kbuildConfigValue(tree, key, value)
	}
	if err := kconfig.ValidateKbuildOrdinaryVariables("resolved Kbuild configuration", vars); err != nil {
		return nil, err
	}
	return vars, nil
}

func workspacePath(path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	workspace := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	if workspace == "" {
		return path
	}
	return filepath.Join(workspace, path)
}

func workspaceDirectory(value string) (string, error) {
	resolved := workspacePath(value)
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return resolved, nil
	}
	if info.Mode().IsRegular() {
		return filepath.Dir(resolved), nil
	}
	return "", fmt.Errorf("%q is neither a directory nor a regular root marker", value)
}

func parseConfigPath(filename string) (map[string]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return kconfig.ParseConfig(file)
}
