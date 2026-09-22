package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

// Each round owns only the guard terminals selected after the previous round's
// results were measured. Identical content-addressed nodes may occur in more
// than one round, but a newly selected terminal must be added to the union
// before its answer can influence the next source evaluation.
func nextKbuildSelectedProbeRound(previous, selected *kconfig.ProbePlan, stage string, requireConverged bool) (*kconfig.ProbePlan, error) {
	if selected == nil {
		return nil, fmt.Errorf("%s discovery has no selected probe plan", stage)
	}
	if previous == nil {
		if requireConverged {
			return nil, fmt.Errorf("%s convergence requires a measured prior round", stage)
		}
		return selected, nil
	}
	newTerminals := []string{}
	for _, id := range selected.Terminal {
		if !slices.Contains(previous.Terminal, id) {
			newTerminals = append(newTerminals, id)
		}
	}
	if requireConverged && len(newTerminals) != 0 {
		return nil, fmt.Errorf("%s rounds exhausted with %d new selected probe terminals (first %s)", stage, len(newTerminals), newTerminals[0])
	}
	return kconfig.MergeProbePlans([]kconfig.ProbePlanVariant{
		{Name: "previous", Plan: previous},
		{Name: "selected", Plan: selected},
	})
}

func nextKbuildGraphGuardRound(previous, selected *kconfig.ProbePlan, requireConverged bool) (*kconfig.ProbePlan, error) {
	return nextKbuildSelectedProbeRound(previous, selected, "source graph guard", requireConverged)
}

func validateEarlierKbuildSelectedProbeRound(earlier, current *kconfig.ProbePlan, stage string) error {
	if earlier == nil || current == nil {
		return fmt.Errorf("prior and earlier %s rounds are required", stage)
	}
	union, err := kconfig.MergeProbePlans([]kconfig.ProbePlanVariant{
		{Name: "earlier", Plan: earlier},
		{Name: "current", Plan: current},
	})
	if err != nil {
		return err
	}
	if !kconfig.SameProbePlanContent(union, current) {
		return fmt.Errorf("the measured prior %s round lost an earlier request, dependency, or terminal", stage)
	}
	return nil
}

func validateEarlierKbuildGraphGuardRound(earlier, current *kconfig.ProbePlan) error {
	return validateEarlierKbuildSelectedProbeRound(earlier, current, "graph guard")
}

type kbuildSelectedProbeRoundEvidence struct {
	plan                       *kconfig.ProbePlan
	hostResults, targetResults string
}

// Equal selected plans alone do not prove a paired source/feature frontier has
// stabilized: a different measured compiler answer can select a new writer in
// the next round. Compare the sealed bytes for every node in both scopes before
// carrying either plan forward. The caller has already validated the current
// result tree against its exact plan; equal earlier bytes then carry the same
// result authority without reinterpreting an unverified answer.
func sameKbuildSelectedProbeRoundEvidence(
	stage string, earlier, current kbuildSelectedProbeRoundEvidence,
) (bool, error) {
	if earlier.plan == nil || current.plan == nil {
		return false, fmt.Errorf("%s paired convergence requires two measured plans", stage)
	}
	if err := validateEarlierKbuildSelectedProbeRound(earlier.plan, current.plan, stage); err != nil {
		return false, err
	}
	if !kconfig.SameProbePlanContent(earlier.plan, current.plan) {
		return false, nil
	}
	if earlier.hostResults == "" || earlier.targetResults == "" ||
		current.hostResults == "" || current.targetResults == "" {
		return false, fmt.Errorf("%s paired convergence requires both result scopes in both rounds", stage)
	}
	for _, node := range current.plan.Nodes {
		var earlierRoot, currentRoot string
		switch node.Scope {
		case "host":
			earlierRoot, currentRoot = earlier.hostResults, current.hostResults
		case "target":
			earlierRoot, currentRoot = earlier.targetResults, current.targetResults
		default:
			return false, fmt.Errorf("%s paired convergence has invalid probe scope %q", stage, node.Scope)
		}
		name := filepath.Join("results", node.ID+".json")
		earlierBytes, err := os.ReadFile(filepath.Join(earlierRoot, name))
		if err != nil {
			return false, fmt.Errorf("read earlier %s result %s: %w", stage, node.ID, err)
		}
		currentBytes, err := os.ReadFile(filepath.Join(currentRoot, name))
		if err != nil {
			return false, fmt.Errorf("read current %s result %s: %w", stage, node.ID, err)
		}
		if !bytes.Equal(earlierBytes, currentBytes) {
			return false, nil
		}
	}
	return true, nil
}

func sameKbuildSelectedPairedRounds(
	earlierSource, currentSource, earlierFeature, currentFeature kbuildSelectedProbeRoundEvidence,
) (bool, error) {
	sourceSame, err := sameKbuildSelectedProbeRoundEvidence("source-output", earlierSource, currentSource)
	if err != nil || !sourceSame {
		return false, err
	}
	return sameKbuildSelectedProbeRoundEvidence("feature-dump", earlierFeature, currentFeature)
}
