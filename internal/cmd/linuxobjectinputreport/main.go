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
	TargetID       int    `json:"targetId"`
	Mnemonic       string `json:"mnemonic"`
	InputDepSetIDs []int  `json:"inputDepSetIds"`
	OutputIDs      []int  `json:"outputIds"`
	PrimaryOutput  int    `json:"primaryOutputId"`
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
	execroot  string
}

type inputUse struct {
	path       string
	count      int
	bytes      int64
	producer   string
	producerOK bool
}

type producerUse struct {
	key   string
	count int
	bytes int64
}

type actionSummary struct {
	label string
	count int
	bytes int64
}

func main() {
	opts := reportOptions{}
	flag.StringVar(&opts.inputPath, "input", "", "Path to Bazel aquery JSON proto. Reads stdin when empty or -.")
	flag.StringVar(&opts.mnemonic, "mnemonic", "LinuxObjectCompile", "Action mnemonic to report.")
	flag.IntVar(&opts.top, "top", 20, "Number of top entries to print per section.")
	flag.IntVar(&opts.sharedPct, "shared_pct", 80, "Minimum action percentage for high-fanout input sections.")
	flag.StringVar(&opts.execroot, "execroot", "", "Optional Bazel execution root used to stat input byte sizes.")
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
	model := newModel(&aq, opts.execroot)
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
	aq           *aqueryOutput
	artifactPath map[int]string
	depSets      map[int]depSet
	targetLabels map[int]string
	producers    map[int]producer
	statCache    map[string]int64
	execroot     string
}

type producer struct {
	mnemonic string
	target   string
}

func newModel(aq *aqueryOutput, execroot string) *model {
	m := &model{
		aq:           aq,
		artifactPath: map[int]string{},
		depSets:      map[int]depSet{},
		targetLabels: map[int]string{},
		producers:    map[int]producer{},
		statCache:    map[string]int64{},
		execroot:     execroot,
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
	for _, action := range aq.Actions {
		p := producer{
			mnemonic: action.Mnemonic,
			target:   m.targetLabels[action.TargetID],
		}
		for _, outputID := range action.OutputIDs {
			m.producers[outputID] = p
		}
	}
	return m
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
	inputBytes := make([]int64, 0, len(actions))
	inputUses := map[int]*inputUse{}
	producerUses := map[string]*producerUse{}

	for _, action := range actions {
		inputs := m.actionInputs(action)
		var bytes int64
		for artifactID := range inputs {
			path := m.artifactPath[artifactID]
			size := m.inputSize(path)
			bytes += size
			use := inputUses[artifactID]
			if use == nil {
				use = &inputUse{path: path, bytes: size}
				if producer, ok := m.producers[artifactID]; ok {
					use.producer = producerKey(producer)
					use.producerOK = true
				}
				inputUses[artifactID] = use
			}
			use.count++
			if producer, ok := m.producers[artifactID]; ok {
				key := producerKey(producer)
				pu := producerUses[key]
				if pu == nil {
					pu = &producerUse{key: key}
					producerUses[key] = pu
				}
				pu.count++
				pu.bytes += size
			}
		}
		inputCounts = append(inputCounts, len(inputs))
		inputBytes = append(inputBytes, bytes)
		actionSummaries = append(actionSummaries, actionSummary{
			label: m.targetLabels[action.TargetID],
			count: len(inputs),
			bytes: bytes,
		})
	}

	fmt.Fprintf(w, "Linux object input report\n")
	fmt.Fprintf(w, "mnemonic: %s\n", opts.mnemonic)
	fmt.Fprintf(w, "actions: %d\n", len(actions))
	fmt.Fprintf(w, "input count min/p50/p95/max: %s\n", intStats(inputCounts))
	if opts.execroot != "" {
		fmt.Fprintf(w, "input bytes min/p50/p95/max: %s\n", byteStats(inputBytes))
	} else {
		fmt.Fprintf(w, "input bytes: unavailable (pass -execroot)\n")
	}
	fmt.Fprintf(w, "\n")

	threshold := ceilDiv(len(actions)*opts.sharedPct, 100)
	writeTopInputs(w, "high-fanout inputs", topInputs(inputUses, func(use *inputUse) bool {
		return use.count >= threshold
	}), opts.top, len(actions), opts.execroot != "")
	writeTopInputs(w, "high-fanout non-tool inputs", topInputs(inputUses, func(use *inputUse) bool {
		return use.count >= threshold && !isToolchainLikeInput(use.path)
	}), opts.top, len(actions), opts.execroot != "")
	writeTopInputs(w, "high-fanout non-header source inputs", topInputs(inputUses, func(use *inputUse) bool {
		return use.count >= threshold && isSourceLikeNonHeader(use.path)
	}), opts.top, len(actions), opts.execroot != "")
	writeProducerUses(w, "producer fanout", topProducerUses(producerUses), opts.top, opts.execroot != "")
	writeActionSummaries(w, "largest actions by input count", topActionSummaries(actionSummaries, false), opts.top, opts.execroot != "")
	if opts.execroot != "" {
		writeActionSummaries(w, "largest actions by input bytes", topActionSummaries(actionSummaries, true), opts.top, true)
	}
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

func (m *model) inputSize(path string) int64 {
	if m.execroot == "" || path == "" {
		return 0
	}
	if size, ok := m.statCache[path]; ok {
		return size
	}
	fullPath := filepath.Join(m.execroot, filepath.FromSlash(path))
	info, err := os.Stat(fullPath)
	if err != nil || info.IsDir() {
		m.statCache[path] = 0
		return 0
	}
	m.statCache[path] = info.Size()
	return info.Size()
}

func producerKey(producer producer) string {
	if producer.target == "" {
		return producer.mnemonic
	}
	return producer.mnemonic + " " + producer.target
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
		if out[i].bytes != out[j].bytes {
			return out[i].bytes > out[j].bytes
		}
		return out[i].path < out[j].path
	})
	return out
}

func topProducerUses(producerUses map[string]*producerUse) []producerUse {
	out := []producerUse{}
	for _, use := range producerUses {
		out = append(out, *use)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		if out[i].bytes != out[j].bytes {
			return out[i].bytes > out[j].bytes
		}
		return out[i].key < out[j].key
	})
	return out
}

func topActionSummaries(actions []actionSummary, byBytes bool) []actionSummary {
	out := append([]actionSummary(nil), actions...)
	sort.Slice(out, func(i, j int) bool {
		if byBytes && out[i].bytes != out[j].bytes {
			return out[i].bytes > out[j].bytes
		}
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].label < out[j].label
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
			fmt.Fprintf(w, "  %5d %6.1f%% %10s  %s", input.count, pct, formatBytes(input.bytes), input.path)
		} else {
			fmt.Fprintf(w, "  %5d %6.1f%%  %s", input.count, pct, input.path)
		}
		if input.producerOK {
			fmt.Fprintf(w, "  [%s]", input.producer)
		}
		fmt.Fprintf(w, "\n")
	}
	fmt.Fprintf(w, "\n")
}

func writeProducerUses(w io.Writer, title string, producers []producerUse, limit int, showBytes bool) {
	fmt.Fprintf(w, "%s:\n", title)
	if len(producers) == 0 {
		fmt.Fprintf(w, "  none (aquery did not include producer actions for compile inputs)\n\n")
		return
	}
	for i, producer := range producers {
		if i >= limit {
			break
		}
		if showBytes {
			fmt.Fprintf(w, "  %5d %10s  %s\n", producer.count, formatBytes(producer.bytes), producer.key)
		} else {
			fmt.Fprintf(w, "  %5d  %s\n", producer.count, producer.key)
		}
	}
	fmt.Fprintf(w, "\n")
}

func writeActionSummaries(w io.Writer, title string, actions []actionSummary, limit int, showBytes bool) {
	fmt.Fprintf(w, "%s:\n", title)
	for i, action := range actions {
		if i >= limit {
			break
		}
		if showBytes {
			fmt.Fprintf(w, "  %5d %10s  %s\n", action.count, formatBytes(action.bytes), action.label)
		} else {
			fmt.Fprintf(w, "  %5d  %s\n", action.count, action.label)
		}
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

func byteStats(values []int64) string {
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return fmt.Sprintf("%s / %s / %s / %s", formatBytes(values[0]), formatBytes(percentileInt64(values, 50)), formatBytes(percentileInt64(values, 95)), formatBytes(values[len(values)-1]))
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
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	div := int64(unit)
	exp := 0
	for n := value / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(value)/float64(div), "KMGTPE"[exp])
}
