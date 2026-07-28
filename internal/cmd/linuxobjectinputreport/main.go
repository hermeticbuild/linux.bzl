// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type aqueryOutput struct {
	Artifacts     []artifact     `json:"artifacts"`
	Actions       []action       `json:"actions"`
	Targets       []target       `json:"targets"`
	DepSetOfFiles []depSet       `json:"depSetOfFiles"`
	PathFragments []pathFragment `json:"pathFragments"`
}

type artifact struct {
	ID             int `json:"id"`
	PathFragmentID int `json:"pathFragmentId"`
}

type action struct {
	TargetID        int    `json:"targetId"`
	ConfigurationID int    `json:"configurationId"`
	Mnemonic        string `json:"mnemonic"`
	InputDepSetIDs  []int  `json:"inputDepSetIds"`
	OutputIDs       []int  `json:"outputIds"`
	PrimaryOutputID int    `json:"primaryOutputId"`
}

type target struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
}

type depSet struct {
	ID                  int   `json:"id"`
	DirectArtifactIDs   []int `json:"directArtifactIds"`
	TransitiveDepSetIDs []int `json:"transitiveDepSetIds"`
}

type pathFragment struct {
	ID       int    `json:"id"`
	Label    string `json:"label"`
	ParentID int    `json:"parentId"`
}

type reportOptions struct {
	inputPath string
	mnemonic  string
	top       int
	sharedPct int
}

type inputUse struct {
	path  string
	count int
}

type actionSummary struct {
	label string
	count int
}

type producerID int

type producer struct {
	actionIndex       int
	mnemonic          string
	target            string
	configurationID   int
	primaryOutputPath string
	outputCount       int
}

type producerUse struct {
	producer  producer
	consumers int
}

func main() {
	opts := reportOptions{}
	flag.StringVar(&opts.inputPath, "input", "", "Path to Bazel aquery JSON proto. Reads stdin when empty or -.")
	flag.StringVar(&opts.mnemonic, "mnemonic", "LinuxObjectCompile", "Action mnemonic to report.")
	flag.IntVar(&opts.top, "top", 20, "Number of top entries to print per section.")
	flag.IntVar(&opts.sharedPct, "shared_pct", 80, "Minimum action percentage for high-fanout input sections.")
	flag.Parse()

	if err := run(os.Stdout, opts); err != nil {
		fmt.Fprintf(os.Stderr, "linuxobjectinputreport: %v\n", err)
		os.Exit(1)
	}
}

func run(w io.Writer, opts reportOptions) error {
	if opts.mnemonic == "" {
		return fmt.Errorf("-mnemonic must not be empty")
	}
	if opts.top <= 0 {
		opts.top = 20
	}
	if opts.sharedPct <= 0 || opts.sharedPct > 100 {
		return fmt.Errorf("-shared_pct must be between 1 and 100")
	}
	data, err := readInput(opts.inputPath)
	if err != nil {
		return err
	}
	var aq aqueryOutput
	if err := json.Unmarshal(data, &aq); err != nil {
		return fmt.Errorf("parse aquery JSON: %w", err)
	}
	model, err := newModel(&aq)
	if err != nil {
		return fmt.Errorf("build aquery model: %w", err)
	}
	return writeReport(w, model, opts)
}

func readInput(path string) ([]byte, error) {
	if path == "" || path == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		return data, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return data, nil
}

type model struct {
	aq               *aqueryOutput
	artifactPath     map[int]string
	depSets          map[int]depSet
	targetLabels     map[int]string
	artifactProducer map[int]producerID
	producers        []producer
}

func newModel(aq *aqueryOutput) (*model, error) {
	m := &model{
		aq:               aq,
		artifactPath:     map[int]string{},
		depSets:          map[int]depSet{},
		targetLabels:     map[int]string{},
		artifactProducer: map[int]producerID{},
	}
	fragments := map[int]pathFragment{}
	for _, fragment := range aq.PathFragments {
		fragments[fragment.ID] = fragment
	}
	for _, artifact := range aq.Artifacts {
		m.artifactPath[artifact.ID] = resolvePathFragment(fragments, artifact.PathFragmentID)
	}
	for _, depSet := range aq.DepSetOfFiles {
		m.depSets[depSet.ID] = depSet
	}
	for _, target := range aq.Targets {
		m.targetLabels[target.ID] = target.Label
	}
	for actionIndex, action := range aq.Actions {
		outputIDs := uniqueInts(action.OutputIDs)
		if len(outputIDs) == 0 {
			continue
		}
		for _, outputID := range outputIDs {
			if existingID, ok := m.artifactProducer[outputID]; ok {
				existing := m.producers[existingID]
				return nil, fmt.Errorf(
					"artifact %d is output by multiple actions (%d and %d)",
					outputID,
					existing.actionIndex,
					actionIndex,
				)
			}
		}
		id := producerID(len(m.producers))
		p := producer{
			actionIndex:       actionIndex,
			mnemonic:          action.Mnemonic,
			target:            m.targetLabels[action.TargetID],
			configurationID:   action.ConfigurationID,
			primaryOutputPath: primaryOutputPath(action.PrimaryOutputID, outputIDs, m.artifactPath),
			outputCount:       len(outputIDs),
		}
		m.producers = append(m.producers, p)
		for _, outputID := range outputIDs {
			m.artifactProducer[outputID] = id
		}
	}
	return m, nil
}

func uniqueInts(values []int) []int {
	out := make([]int, 0, len(values))
	seen := make(map[int]bool, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func primaryOutputPath(primaryOutputID int, outputIDs []int, artifactPaths map[int]string) string {
	outputSet := make(map[int]bool, len(outputIDs))
	for _, outputID := range outputIDs {
		outputSet[outputID] = true
	}
	if outputSet[primaryOutputID] {
		return artifactDisplay(primaryOutputID, artifactPaths)
	}
	candidates := make([]string, 0, len(outputIDs))
	for _, outputID := range outputIDs {
		candidates = append(candidates, artifactDisplay(outputID, artifactPaths))
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0]
}

func artifactDisplay(artifactID int, artifactPaths map[int]string) string {
	if path := artifactPaths[artifactID]; path != "" {
		return path
	}
	return fmt.Sprintf("artifact#%d", artifactID)
}

func resolvePathFragment(fragments map[int]pathFragment, id int) string {
	if id == 0 {
		return ""
	}
	var parts []string
	seen := map[int]bool{}
	for id != 0 {
		if seen[id] {
			break
		}
		seen[id] = true
		fragment := fragments[id]
		if fragment.Label == "" {
			break
		}
		parts = append(parts, fragment.Label)
		id = fragment.ParentID
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, "/")
}

func writeReport(w io.Writer, m *model, opts reportOptions) error {
	actions := m.actionsByMnemonic(opts.mnemonic)
	if len(actions) == 0 {
		return fmt.Errorf("no actions with mnemonic %q", opts.mnemonic)
	}

	actionSummaries := make([]actionSummary, 0, len(actions))
	inputCounts := make([]int, 0, len(actions))
	inputUses := map[int]*inputUse{}
	producerConsumerCounts := make([]int, len(m.producers))
	producerSeenGeneration := make([]int, len(m.producers))

	for actionIndex, action := range actions {
		inputs := m.actionInputs(action)
		for artifactID := range inputs {
			path := m.artifactPath[artifactID]
			use := inputUses[artifactID]
			if use == nil {
				use = &inputUse{path: path}
				inputUses[artifactID] = use
			}
			use.count++
			if id, ok := m.artifactProducer[artifactID]; ok {
				if producerSeenGeneration[id] != actionIndex+1 {
					producerSeenGeneration[id] = actionIndex + 1
					producerConsumerCounts[id]++
				}
			}
		}
		inputCounts = append(inputCounts, len(inputs))
		actionSummaries = append(actionSummaries, actionSummary{
			label: m.targetLabels[action.TargetID],
			count: len(inputs),
		})
	}

	fmt.Fprintf(w, "Linux object input report\n")
	fmt.Fprintf(w, "mnemonic: %s\n", opts.mnemonic)
	fmt.Fprintf(w, "actions: %d\n", len(actions))
	fmt.Fprintf(w, "input count min/p50/p95/max: %s\n", intStats(inputCounts))
	fmt.Fprintf(w, "\n")

	threshold := ceilDiv(len(actions)*opts.sharedPct, 100)
	writeTopInputs(w, "high-fanout inputs", topInputs(inputUses, func(use *inputUse) bool {
		return use.count >= threshold
	}), opts.top, len(actions))
	writeTopInputs(w, "high-fanout non-tool inputs", topInputs(inputUses, func(use *inputUse) bool {
		return use.count >= threshold && !isToolchainLikeInput(use.path)
	}), opts.top, len(actions))
	writeTopInputs(w, "high-fanout non-header source inputs", topInputs(inputUses, func(use *inputUse) bool {
		return use.count >= threshold && isSourceLikeNonHeader(use.path)
	}), opts.top, len(actions))
	resolvedProducerInputs := 0
	for artifactID := range inputUses {
		if _, ok := m.artifactProducer[artifactID]; ok {
			resolvedProducerInputs++
		}
	}
	writeProducerUses(
		w,
		opts.mnemonic,
		topProducerUses(m.producers, producerConsumerCounts),
		opts.top,
		resolvedProducerInputs,
		len(inputUses),
	)
	writeActionSummaries(w, "largest actions by input count", topActionSummaries(actionSummaries), opts.top)
	return nil
}

func (m *model) actionsByMnemonic(mnemonic string) []action {
	var out []action
	for _, action := range m.aq.Actions {
		if action.Mnemonic == mnemonic {
			out = append(out, action)
		}
	}
	return out
}

func (m *model) actionInputs(action action) map[int]bool {
	out := map[int]bool{}
	seenDepSets := map[int]bool{}
	var visit func(id int)
	visit = func(id int) {
		if seenDepSets[id] {
			return
		}
		seenDepSets[id] = true
		depSet := m.depSets[id]
		for _, artifactID := range depSet.DirectArtifactIDs {
			out[artifactID] = true
		}
		for _, child := range depSet.TransitiveDepSetIDs {
			visit(child)
		}
	}
	for _, id := range action.InputDepSetIDs {
		visit(id)
	}
	return out
}

func topInputs(inputUses map[int]*inputUse, keep func(*inputUse) bool) []inputUse {
	out := []inputUse{}
	for _, use := range inputUses {
		if keep(use) {
			out = append(out, *use)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].path < out[j].path
	})
	return out
}

func topActionSummaries(actions []actionSummary) []actionSummary {
	out := append([]actionSummary(nil), actions...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].label < out[j].label
	})
	return out
}

func topProducerUses(producers []producer, consumerCounts []int) []producerUse {
	out := make([]producerUse, 0, len(producers))
	for i, producer := range producers {
		if consumerCounts[i] == 0 {
			continue
		}
		out = append(out, producerUse{
			producer:  producer,
			consumers: consumerCounts[i],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].consumers != out[j].consumers {
			return out[i].consumers > out[j].consumers
		}
		if out[i].producer.mnemonic != out[j].producer.mnemonic {
			return out[i].producer.mnemonic < out[j].producer.mnemonic
		}
		if out[i].producer.target != out[j].producer.target {
			return out[i].producer.target < out[j].producer.target
		}
		if out[i].producer.primaryOutputPath != out[j].producer.primaryOutputPath {
			return out[i].producer.primaryOutputPath < out[j].producer.primaryOutputPath
		}
		if out[i].producer.configurationID != out[j].producer.configurationID {
			return out[i].producer.configurationID < out[j].producer.configurationID
		}
		return out[i].producer.actionIndex < out[j].producer.actionIndex
	})
	return out
}

func writeTopInputs(w io.Writer, title string, inputs []inputUse, limit int, actionCount int) {
	fmt.Fprintf(w, "%s:\n", title)
	if len(inputs) == 0 {
		fmt.Fprintf(w, "  none\n\n")
		return
	}
	for i, input := range inputs {
		if i >= limit {
			break
		}
		pct := float64(input.count) * 100 / float64(actionCount)
		fmt.Fprintf(w, "  %5d %6.1f%%  %s\n", input.count, pct, input.path)
	}
	fmt.Fprintf(w, "\n")
}

func writeProducerUses(
	w io.Writer,
	consumerMnemonic string,
	producers []producerUse,
	limit int,
	resolvedInputCount int,
	inputCount int,
) {
	fmt.Fprintf(
		w,
		"producer fanout (unique %s consumer actions; producers present in aquery):\n",
		consumerMnemonic,
	)
	fmt.Fprintf(w, "  producer-resolved input artifacts: %d / %d unique inputs\n", resolvedInputCount, inputCount)
	if len(producers) == 0 {
		fmt.Fprintf(w, "  none resolved\n")
		fmt.Fprintf(
			w,
			"  hint: query deps(<target>) and let -mnemonic filter consumers; mnemonic(...) omits producer actions\n\n",
		)
		return
	}
	fmt.Fprintf(w, "  consumers  producer\n")
	for i, use := range producers {
		if i >= limit {
			break
		}
		fmt.Fprintf(
			w,
			"  %9d  %s [primary: %s; outputs: %d]\n",
			use.consumers,
			producerName(use.producer),
			use.producer.primaryOutputPath,
			use.producer.outputCount,
		)
	}
	fmt.Fprintf(w, "\n")
}

func producerName(producer producer) string {
	if producer.target == "" {
		return producer.mnemonic
	}
	return producer.mnemonic + " " + producer.target
}

func writeActionSummaries(w io.Writer, title string, actions []actionSummary, limit int) {
	fmt.Fprintf(w, "%s:\n", title)
	for i, action := range actions {
		if i >= limit {
			break
		}
		fmt.Fprintf(w, "  %5d  %s\n", action.count, action.label)
	}
	fmt.Fprintf(w, "\n")
}

func isSourceLikeNonHeader(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".c", ".cc", ".cpp", ".s", ".lds", ".dts", ".dtsi", ".asn1":
		return true
	default:
		return false
	}
}

func isToolchainLikeInput(path string) bool {
	return strings.HasPrefix(path, "external/llvm") ||
		strings.Contains(path, "/external/llvm") ||
		strings.Contains(path, "llvm-toolchain") ||
		strings.Contains(path, "llvm++") ||
		strings.Contains(path, "rules_cc++") ||
		strings.Contains(path, "rules_go++")
}

func intStats(values []int) string {
	sort.Ints(values)
	return fmt.Sprintf("%d / %d / %d / %d", values[0], percentileInt(values, 50), percentileInt(values, 95), values[len(values)-1])
}

func percentileInt(values []int, percentile int) int {
	if len(values) == 0 {
		return 0
	}
	index := ceilDiv(len(values)*percentile, 100) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(values) {
		index = len(values) - 1
	}
	return values[index]
}

func ceilDiv(n, d int) int {
	return (n + d - 1) / d
}
