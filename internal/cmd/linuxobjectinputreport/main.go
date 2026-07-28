// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

type aqueryOutput struct {
	Artifacts     []artifact     `json:"artifacts"`
	Actions       []action       `json:"actions"`
	Targets       []target       `json:"targets"`
	DepSetOfFiles []depSet       `json:"depSetOfFiles"`
	PathFragments []pathFragment `json:"pathFragments"`
}

type artifact struct {
	ID             int  `json:"id"`
	PathFragmentID int  `json:"pathFragmentId"`
	IsTreeArtifact bool `json:"isTreeArtifact"`
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
	mnemonics []string
	top       int
	sharedPct int
	execroot  string
}

type inputUse struct {
	path        string
	count       int
	measurement *byteMeasurement
}

type actionSummary struct {
	label         string
	count         int
	bytes         int64
	bytesComplete bool
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

const defaultCompileMnemonic = "LinuxObjectCompile"

type mnemonicFlag []string

func (m *mnemonicFlag) String() string {
	return strings.Join(*m, ",")
}

func (m *mnemonicFlag) Set(value string) error {
	*m = append(*m, value)
	return nil
}

func main() {
	opts, err := parseOptions(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "linuxobjectinputreport: %v\n", err)
		os.Exit(2)
	}

	if err := run(os.Stdout, opts); err != nil {
		fmt.Fprintf(os.Stderr, "linuxobjectinputreport: %v\n", err)
		os.Exit(1)
	}
}

func parseOptions(args []string) (reportOptions, error) {
	opts := reportOptions{}
	flags := flag.NewFlagSet("linuxobjectinputreport", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&opts.inputPath, "input", "", "Path to Bazel aquery JSON proto. Reads stdin when empty or -.")
	flags.Var((*mnemonicFlag)(&opts.mnemonics), "mnemonic", "Action mnemonic to include. Repeat to report a union; defaults to LinuxObjectCompile.")
	flags.IntVar(&opts.top, "top", 20, "Number of top entries to print per section.")
	flags.IntVar(&opts.sharedPct, "shared_pct", 80, "Minimum action percentage for high-fanout input sections.")
	flags.StringVar(&opts.execroot, "execroot", "", "Optional Bazel execution root used to measure materialized input bytes.")
	if err := flags.Parse(args); err != nil {
		return reportOptions{}, err
	}
	if flags.NArg() != 0 {
		return reportOptions{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	var err error
	opts.mnemonics, err = normalizeMnemonics(opts.mnemonics)
	if err != nil {
		return reportOptions{}, err
	}
	return opts, nil
}

func normalizeMnemonics(values []string) ([]string, error) {
	if len(values) == 0 {
		return []string{defaultCompileMnemonic}, nil
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if value == "" {
			return nil, fmt.Errorf("-mnemonic must not be empty")
		}
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out, nil
}

func run(w io.Writer, opts reportOptions) error {
	var err error
	opts.mnemonics, err = normalizeMnemonics(opts.mnemonics)
	if err != nil {
		return err
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
	artifacts        map[int]artifact
	depSets          map[int]depSet
	targetLabels     map[int]string
	artifactProducer map[int]producerID
	producers        []producer
}

func newModel(aq *aqueryOutput) (*model, error) {
	m := &model{
		aq:               aq,
		artifactPath:     map[int]string{},
		artifacts:        map[int]artifact{},
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
		m.artifacts[artifact.ID] = artifact
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
	mnemonics, err := normalizeMnemonics(opts.mnemonics)
	if err != nil {
		return err
	}
	actions := m.actionsByMnemonics(mnemonics)
	if len(actions) == 0 {
		return fmt.Errorf("no actions with selected mnemonics %q", strings.Join(mnemonics, ", "))
	}

	var measurer *byteMeasurer
	if opts.execroot != "" {
		measurer, err = newByteMeasurer(opts.execroot, m)
		if err != nil {
			return err
		}
	}

	actionSummaries := make([]actionSummary, 0, len(actions))
	inputCounts := make([]int, 0, len(actions))
	completeActionBytes := make([]int64, 0, len(actions))
	inputUses := map[int]*inputUse{}
	producerConsumerCounts := make([]int, len(m.producers))
	producerSeenGeneration := make([]int, len(m.producers))
	actionByteOverflows := 0

	for actionIndex, action := range actions {
		inputs := m.actionInputs(action)
		actionFiles := uniqueFiles{}
		bytesComplete := measurer != nil
		actionOverflow := false
		for artifactID := range inputs {
			path := m.artifactPath[artifactID]
			use := inputUses[artifactID]
			if use == nil {
				use = &inputUse{path: path}
				inputUses[artifactID] = use
			}
			use.count++
			if measurer != nil {
				measurement := measurer.measureArtifact(artifactID)
				use.measurement = measurement
				if !measurement.complete {
					bytesComplete = false
				}
				if err := actionFiles.merge(&measurement.files); err != nil {
					bytesComplete = false
					if !actionOverflow {
						actionByteOverflows++
						actionOverflow = true
					}
				}
			}
			if id, ok := m.artifactProducer[artifactID]; ok {
				if producerSeenGeneration[id] != actionIndex+1 {
					producerSeenGeneration[id] = actionIndex + 1
					producerConsumerCounts[id]++
				}
			}
		}
		inputCounts = append(inputCounts, len(inputs))
		actionSummaries = append(actionSummaries, actionSummary{
			label:         m.actionName(action),
			count:         len(inputs),
			bytes:         actionFiles.bytes,
			bytesComplete: bytesComplete,
		})
		if bytesComplete {
			completeActionBytes = append(completeActionBytes, actionFiles.bytes)
		}
	}

	fmt.Fprintf(w, "Linux object input report\n")
	fmt.Fprintf(w, "mnemonics: %s\n", strings.Join(mnemonics, ", "))
	fmt.Fprintf(w, "actions: %d\n", len(actions))
	fmt.Fprintf(w, "input count min/p50/p95/max: %s\n", intStats(inputCounts))
	if measurer == nil {
		fmt.Fprintf(w, "materialized input bytes: disabled (pass -execroot)\n")
	} else {
		unavailableInputs, unavailableUses, issues := byteCoverage(inputUses)
		fmt.Fprintf(w, "byte accounting: materialized file bytes deduplicated by local file identity under %s\n", measurer.execroot)
		fmt.Fprintf(
			w,
			"byte coverage: %d / %d actions complete; %d distinct inputs unavailable (%d action uses)\n",
			len(completeActionBytes),
			len(actions),
			unavailableInputs,
			unavailableUses,
		)
		if actionByteOverflows != 0 {
			issues[byteIssueOverflow] += actionByteOverflows
		}
		if len(issues) != 0 {
			fmt.Fprintf(w, "byte issues: %s\n", formatByteIssues(issues))
		}
		if len(completeActionBytes) == 0 {
			fmt.Fprintf(w, "materialized input bytes: unavailable (no complete actions)\n")
		} else {
			fmt.Fprintf(
				w,
				"materialized input bytes min/p50/p95/max (%d complete actions): %s\n",
				len(completeActionBytes),
				byteStats(completeActionBytes),
			)
		}
	}
	fmt.Fprintf(w, "\n")

	threshold := ceilDiv(len(actions)*opts.sharedPct, 100)
	writeTopInputs(w, "high-fanout inputs", topInputs(inputUses, func(use *inputUse) bool {
		return use.count >= threshold
	}), opts.top, len(actions), measurer != nil)
	writeTopInputs(w, "high-fanout non-tool inputs", topInputs(inputUses, func(use *inputUse) bool {
		return use.count >= threshold && !isToolchainLikeInput(use.path)
	}), opts.top, len(actions), measurer != nil)
	writeTopInputs(w, "high-fanout non-header source inputs", topInputs(inputUses, func(use *inputUse) bool {
		return use.count >= threshold && isSourceLikeNonHeader(use.path)
	}), opts.top, len(actions), measurer != nil)
	resolvedProducerInputs := 0
	for artifactID := range inputUses {
		if _, ok := m.artifactProducer[artifactID]; ok {
			resolvedProducerInputs++
		}
	}
	writeProducerUses(
		w,
		topProducerUses(m.producers, producerConsumerCounts),
		opts.top,
		resolvedProducerInputs,
		len(inputUses),
	)
	writeActionSummaries(w, "largest actions by input count", topActionSummaries(actionSummaries), opts.top)
	if measurer != nil {
		writeByteActionSummaries(
			w,
			"largest complete actions by materialized input bytes",
			topActionSummariesByBytes(actionSummaries),
			opts.top,
		)
	}
	return nil
}

func (m *model) actionsByMnemonics(mnemonics []string) []action {
	selected := make(map[string]bool, len(mnemonics))
	for _, mnemonic := range mnemonics {
		selected[mnemonic] = true
	}
	var out []action
	for _, action := range m.aq.Actions {
		if selected[action.Mnemonic] {
			out = append(out, action)
		}
	}
	return out
}

func (m *model) actionName(action action) string {
	name := action.Mnemonic
	if label := m.targetLabels[action.TargetID]; label != "" {
		name += " " + label
	}
	outputIDs := uniqueInts(action.OutputIDs)
	if len(outputIDs) != 0 {
		name += " [primary: " + primaryOutputPath(action.PrimaryOutputID, outputIDs, m.artifactPath) + "]"
	}
	return name
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

func topActionSummariesByBytes(actions []actionSummary) []actionSummary {
	out := make([]actionSummary, 0, len(actions))
	for _, action := range actions {
		if action.bytesComplete {
			out = append(out, action)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].bytes != out[j].bytes {
			return out[i].bytes > out[j].bytes
		}
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

func writeTopInputs(w io.Writer, title string, inputs []inputUse, limit int, actionCount int, showBytes bool) {
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
		if showBytes {
			fmt.Fprintf(
				w,
				"  %5d %6.1f%% %10s  %s\n",
				input.count,
				pct,
				measurementByteDisplay(input.measurement),
				input.path,
			)
		} else {
			fmt.Fprintf(w, "  %5d %6.1f%%  %s\n", input.count, pct, input.path)
		}
	}
	fmt.Fprintf(w, "\n")
}

func writeProducerUses(
	w io.Writer,
	producers []producerUse,
	limit int,
	resolvedInputCount int,
	inputCount int,
) {
	fmt.Fprintf(
		w,
		"producer fanout (unique selected consumer actions; producers present in aquery):\n",
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

func writeByteActionSummaries(w io.Writer, title string, actions []actionSummary, limit int) {
	fmt.Fprintf(w, "%s:\n", title)
	if len(actions) == 0 {
		fmt.Fprintf(w, "  none\n\n")
		return
	}
	for i, action := range actions {
		if i >= limit {
			break
		}
		fmt.Fprintf(w, "  %10s %5d inputs  %s\n", formatBytes(action.bytes), action.count, action.label)
	}
	fmt.Fprintf(w, "\n")
}

func byteCoverage(inputUses map[int]*inputUse) (int, int, map[byteIssue]int) {
	unavailableInputs := 0
	unavailableUses := 0
	issues := map[byteIssue]int{}
	for _, use := range inputUses {
		if use.measurement == nil || use.measurement.complete {
			continue
		}
		unavailableInputs++
		unavailableUses += use.count
		for issue, count := range use.measurement.issues {
			issues[issue] += count
		}
	}
	return unavailableInputs, unavailableUses, issues
}

func formatByteIssues(issues map[byteIssue]int) string {
	names := make([]string, 0, len(issues))
	for issue, count := range issues {
		names = append(names, fmt.Sprintf("%s=%d", issue, count))
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}

func measurementByteDisplay(measurement *byteMeasurement) string {
	if measurement == nil {
		return "?"
	}
	if measurement.complete {
		return formatBytes(measurement.files.bytes)
	}
	if measurement.files.bytes == 0 {
		return "?"
	}
	return ">=" + formatBytes(measurement.files.bytes)
}

func isSourceLikeNonHeader(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".asn1", ".c", ".c_shipped", ".cc", ".cpp", ".dts", ".dtsi", ".dtso", ".inc", ".lds", ".pl", ".rs", ".s":
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

func byteStats(values []int64) string {
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return fmt.Sprintf(
		"%s / %s / %s / %s",
		formatBytes(sorted[0]),
		formatBytes(percentileInt64(sorted, 50)),
		formatBytes(percentileInt64(sorted, 95)),
		formatBytes(sorted[len(sorted)-1]),
	)
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

func percentileInt64(values []int64, percentile int) int64 {
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

func formatBytes(value int64) string {
	const unit = int64(1024)
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	divisor := unit
	unitIndex := 0
	for value/divisor >= unit && unitIndex < len(units)-1 {
		divisor *= unit
		unitIndex++
	}
	return fmt.Sprintf("%.2f %s", float64(value)/float64(divisor), units[unitIndex])
}

type byteIssue string

const (
	byteIssueBrokenSymlink  byteIssue = "broken-symlink"
	byteIssueIO             byteIssue = "io-error"
	byteIssueNotFound       byteIssue = "not-found"
	byteIssueOverflow       byteIssue = "overflow"
	byteIssuePermission     byteIssue = "permission"
	byteIssueSpecialFile    byteIssue = "special-file"
	byteIssueSymlinkCycle   byteIssue = "symlink-cycle"
	byteIssueTypeMismatch   byteIssue = "type-mismatch"
	byteIssueUnmaterialized byteIssue = "unmaterialized"
)

type sameFileSet struct {
	bySize map[int64][]os.FileInfo
}

func (s *sameFileSet) contains(info os.FileInfo) bool {
	for _, existing := range s.bySize[info.Size()] {
		if os.SameFile(existing, info) {
			return true
		}
	}
	return false
}

func (s *sameFileSet) add(info os.FileInfo) bool {
	if s.contains(info) {
		return false
	}
	if s.bySize == nil {
		s.bySize = map[int64][]os.FileInfo{}
	}
	size := info.Size()
	s.bySize[size] = append(s.bySize[size], info)
	return true
}

type uniqueFiles struct {
	identities sameFileSet
	files      []os.FileInfo
	bytes      int64
}

func (f *uniqueFiles) add(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", info.Name())
	}
	if !f.identities.add(info) {
		return nil
	}
	size := info.Size()
	if size < 0 || size > math.MaxInt64-f.bytes {
		return fmt.Errorf("materialized byte count overflows int64")
	}
	f.files = append(f.files, info)
	f.bytes += size
	return nil
}

func (f *uniqueFiles) merge(other *uniqueFiles) error {
	for _, info := range other.files {
		if err := f.add(info); err != nil {
			return err
		}
	}
	return nil
}

type byteMeasurement struct {
	files    uniqueFiles
	complete bool
	issues   map[byteIssue]int
}

func newByteMeasurement() *byteMeasurement {
	return &byteMeasurement{
		complete: true,
		issues:   map[byteIssue]int{},
	}
}

func (m *byteMeasurement) addIssue(issue byteIssue) {
	m.complete = false
	m.issues[issue]++
}

type byteMeasurer struct {
	execroot string
	model    *model
	cache    map[int]*byteMeasurement
}

func newByteMeasurer(execroot string, model *model) (*byteMeasurer, error) {
	absolute, err := filepath.Abs(execroot)
	if err != nil {
		return nil, fmt.Errorf("resolve -execroot %q: %w", execroot, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, fmt.Errorf("stat -execroot %q: %w", absolute, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("-execroot %q is not a directory", absolute)
	}
	return &byteMeasurer{
		execroot: absolute,
		model:    model,
		cache:    map[int]*byteMeasurement{},
	}, nil
}

func (m *byteMeasurer) measureArtifact(artifactID int) *byteMeasurement {
	if measurement, ok := m.cache[artifactID]; ok {
		return measurement
	}

	measurement := newByteMeasurement()
	m.cache[artifactID] = measurement
	path := m.model.artifactPath[artifactID]
	if path == "" {
		measurement.addIssue(byteIssueNotFound)
		return measurement
	}
	relative := filepath.Clean(filepath.FromSlash(path))
	if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		measurement.addIssue(byteIssueIO)
		return measurement
	}

	missingIssue := byteIssueNotFound
	if _, ok := m.model.artifactProducer[artifactID]; ok {
		missingIssue = byteIssueUnmaterialized
	}
	state := scanState{}
	artifact := m.model.artifacts[artifactID]
	m.scanPath(
		filepath.Join(m.execroot, relative),
		artifact.IsTreeArtifact,
		missingIssue,
		measurement,
		&state,
	)
	return measurement
}

type scanState struct {
	activeDirectories  []os.FileInfo
	visitedDirectories sameFileSet
}

func (m *byteMeasurer) scanPath(
	path string,
	expectDirectory bool,
	missingIssue byteIssue,
	measurement *byteMeasurement,
	state *scanState,
) {
	info, err := os.Stat(path)
	if err != nil {
		measurement.addIssue(classifyPathError(path, err, missingIssue))
		return
	}
	if info.Mode().IsRegular() {
		if expectDirectory {
			measurement.addIssue(byteIssueTypeMismatch)
			return
		}
		if err := measurement.files.add(info); err != nil {
			measurement.addIssue(byteIssueOverflow)
		}
		return
	}
	if info.IsDir() {
		m.scanDirectory(path, info, measurement, state)
		return
	}
	if expectDirectory {
		measurement.addIssue(byteIssueTypeMismatch)
	} else {
		measurement.addIssue(byteIssueSpecialFile)
	}
}

func (m *byteMeasurer) scanDirectory(
	path string,
	info os.FileInfo,
	measurement *byteMeasurement,
	state *scanState,
) {
	for _, active := range state.activeDirectories {
		if os.SameFile(active, info) {
			measurement.addIssue(byteIssueSymlinkCycle)
			return
		}
	}
	if !state.visitedDirectories.add(info) {
		return
	}

	state.activeDirectories = append(state.activeDirectories, info)
	defer func() {
		state.activeDirectories = state.activeDirectories[:len(state.activeDirectories)-1]
	}()

	entries, err := os.ReadDir(path)
	if err != nil {
		measurement.addIssue(classifyPathError(path, err, byteIssueNotFound))
		return
	}
	for _, entry := range entries {
		m.scanPath(
			filepath.Join(path, entry.Name()),
			false,
			byteIssueNotFound,
			measurement,
			state,
		)
	}
}

func classifyPathError(path string, err error, missingIssue byteIssue) byteIssue {
	if errors.Is(err, syscall.ELOOP) {
		return byteIssueSymlinkCycle
	}
	if errors.Is(err, fs.ErrNotExist) {
		if info, lstatErr := os.Lstat(path); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return byteIssueBrokenSymlink
		}
		return missingIssue
	}
	if errors.Is(err, fs.ErrPermission) {
		return byteIssuePermission
	}
	if info, lstatErr := os.Lstat(path); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return byteIssueBrokenSymlink
	}
	return byteIssueIO
}
