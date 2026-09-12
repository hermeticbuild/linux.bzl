package kconfig

import (
	"fmt"
	"maps"
	"slices"
	"sync"
)

// CompactKbuildResolvedPrerequisite retains both identities of one evaluated
// Make prerequisite. Target is the canonical action-graph path; MakeTarget is
// the exact source spelling used for GNU Make automatic variables and implicit
// rule lookup.
type CompactKbuildResolvedPrerequisite struct {
	Target     string
	MakeTarget string
}

// CompactKbuildResolvedTargetContext is the immutable public projection of an
// already-resolved target's prerequisite context. Each accessor returns fresh
// slices so callers cannot mutate the retained resolution.
type CompactKbuildResolvedTargetContext struct {
	Normal    []CompactKbuildResolvedPrerequisite
	OrderOnly []CompactKbuildResolvedPrerequisite
	Stem      string
}

// CompactKbuildResolvedTarget is an opaque process-local handle for one exact
// GNU Make target resolution. It lets graph discovery consume prerequisite and
// execution provenance from the same selected rule without repeating command
// expansion and implicit-rule viability checks.
//
// The source CompactKbuildProfile remains immutable while a handle is live.
// Discovery and replay construct independent profiles, so a handle can never
// carry symbolic probe state across that boundary.
type CompactKbuildResolvedTarget struct {
	profile CompactKbuildProfile
	target  string
	match   compactKbuildRuleMatch
	matched bool
	effects *compactKbuildResolvedTargetEffectsMemo
}

// Keep synchronization behind a pointer so an opaque handle copied by a
// caller still shares one memo instead of copying a used sync.Once.
type compactKbuildResolvedTargetEffectsMemo struct {
	once     sync.Once
	effects  CompactKbuildSelectedTargetEffects
	selected bool
	err      error
}

func cloneCompactKbuildSelectedTargetEffects(
	effects CompactKbuildSelectedTargetEffects,
) CompactKbuildSelectedTargetEffects {
	effects.ActionRoles = slices.Clone(effects.ActionRoles)
	effects.PrimaryActionRoles = slices.Clone(effects.PrimaryActionRoles)
	effects.DeferredContentQueries = slices.Clone(effects.DeferredContentQueries)
	for index := range effects.DeferredContentQueries {
		query := &effects.DeferredContentQueries[index]
		query.Profile = cloneCompactKbuildProfilePublicState(query.Profile)
		query.Environment = maps.Clone(query.Environment)
		query.ActionRoles = slices.Clone(query.ActionRoles)
		query.ObjectTree.References = slices.Clone(query.ObjectTree.References)
	}
	return effects
}

// cloneCompactKbuildProfilePublicState gives callers ownership of every
// exported collection reachable through a deferred-content query. Private
// evaluator state remains shared and immutable for the lifetime of a resolved
// target, just as it does in the source profile retained by the handle.
func cloneCompactKbuildProfilePublicState(profile CompactKbuildProfile) CompactKbuildProfile {
	profile.InvocationPredecessors = slices.Clone(profile.InvocationPredecessors)
	profile.TargetInvocationDependencies = slices.Clone(profile.TargetInvocationDependencies)
	for index := range profile.TargetInvocationDependencies {
		dependency := &profile.TargetInvocationDependencies[index]
		dependency.Goals = slices.Clone(dependency.Goals)
		dependency.ReplayArguments = slices.Clone(dependency.ReplayArguments)
	}
	profile.EntryTargets = slices.Clone(profile.EntryTargets)
	profile.Generated = slices.Clone(profile.Generated)
	for index := range profile.Generated {
		profile.Generated[index].Condition = cloneKbuildCondition(profile.Generated[index].Condition)
	}
	profile.Rules = cloneKbuildRules(profile.Rules)
	profile.TargetVariables = cloneKbuildTargetVariables(profile.TargetVariables)
	return profile
}

// ResolveCompactKbuildTargetForMakeTarget selects one target while preserving
// the lexical filename GNU Make used for implicit-rule and target-variable
// lookup. The returned handle is non-nil even when no executable rule matches:
// prerequisite-only declarations still have a target context.
func ResolveCompactKbuildTargetForMakeTarget(
	profile CompactKbuildProfile,
	target, makeTarget string,
) (*CompactKbuildResolvedTarget, error) {
	target = compactKbuildGraphTargetPath(target)
	match, matched, err := (&CompactMetadata{}).compactKbuildRuleForProfileMakeTarget(
		profile, target, makeTarget,
	)
	if err != nil {
		return nil, err
	}
	return &CompactKbuildResolvedTarget{
		profile: profile,
		target:  target,
		match:   match,
		matched: matched,
		effects: &compactKbuildResolvedTargetEffectsMemo{},
	}, nil
}

// HasSelectedRule reports whether resolution found an executable rule. A
// false result can still expose prerequisite-only context through Context.
func (r *CompactKbuildResolvedTarget) HasSelectedRule() bool {
	return r != nil && r.matched
}

// Context returns the exact selected prerequisite context without re-running
// rule selection. Returned values are owned by the caller.
func (r *CompactKbuildResolvedTarget) Context() (CompactKbuildResolvedTargetContext, error) {
	if r == nil {
		return CompactKbuildResolvedTargetContext{}, fmt.Errorf("nil resolved Kbuild target")
	}
	var selected *compactKbuildRuleMatch
	if r.matched {
		match := r.match
		selected = &match
	}
	context, err := evaluatedKbuildSelectedTargetMakeContext(r.profile, r.target, selected)
	if err != nil {
		return CompactKbuildResolvedTargetContext{}, err
	}
	project := func(values []compactKbuildEvaluatedPath) []CompactKbuildResolvedPrerequisite {
		out := make([]CompactKbuildResolvedPrerequisite, len(values))
		for index, value := range values {
			out[index] = CompactKbuildResolvedPrerequisite{
				Target: value.graphPath, MakeTarget: value.makeWord,
			}
		}
		return out
	}
	return CompactKbuildResolvedTargetContext{
		Normal: project(context.normal), OrderOnly: project(context.orderOnly), Stem: context.stem,
	}, nil
}
