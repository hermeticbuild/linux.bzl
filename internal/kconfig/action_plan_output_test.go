package kconfig

import (
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func readActionPlanFilesForTest(root string) (map[string][]byte, error) {
	out := map[string][]byte{}
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = data
		return nil
	})
	return out, err
}

func actionPlanNodeInputSetEntriesForTest(t *testing.T, plan *ActionPlan, node ActionPlanNode) []ActionPlanInputSetEntry {
	t.Helper()
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		t.Fatal(err)
	}
	entries := []ActionPlanInputSetEntry{}
	if err := store.Walk(node.InputSet, func(entry ActionPlanInputSetEntry) error {
		entries = append(entries, entry)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return entries
}

func actionPlanNodeInputSetEntryForPathForTest(
	t *testing.T,
	plan *ActionPlan,
	node ActionPlanNode,
	pathname string,
) (ActionPlanInputSetEntry, bool) {
	t.Helper()
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		t.Fatal(err)
	}
	entry, found, err := store.Lookup(node.InputSet, ActionPlanInputSetTarget{
		Kind: ActionPlanInputSetWorkTarget,
		Path: pathname,
	})
	if err != nil {
		t.Fatal(err)
	}
	return entry, found
}

func actionPlanStageOutputsForTest(root string) map[string]string {
	outputs := make(map[string]string, len(linuxKernelPlanStageOrder))
	for _, stage := range linuxKernelPlanStageOrder {
		outputs[stage] = filepath.Join(root, stage)
	}
	return outputs
}

const actionPlanTestProbeIdentity = "sha256-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestSelectedProductsOnlyPlanOmitsKernelAndSDKFacades(t *testing.T) {
	configFragment := map[string]string{"CONFIG_MODULES": "n"}
	metadata := &CompactMetadata{
		Config:                  CompactConfig{},
		configFragment:          configFragment,
		preconfiguredObjectTree: true,
		selectedProductsOnly:    true,
	}
	plan, err := metadata.ActionPlan(actionPlanTestProbeIdentity, actionPlanTestProbeIdentity)
	if err != nil {
		t.Fatal(err)
	}
	for _, product := range plan.Products {
		if product.Name == "sdk" || product.Name == "image" || product.Name == "vmlinux" {
			t.Fatalf("selected-products-only plan contains facade product %#v", product)
		}
	}
	for _, node := range plan.Nodes {
		for _, output := range node.Outputs {
			if output.Tree == "sdk" || output.Tree == "image" || output.Tree == "vmlinux" {
				t.Fatalf("selected-products-only plan contains facade output %#v", output)
			}
		}
	}
}

func TestActionPlanWritesDeterministicV4StageMarkers(t *testing.T) {
	hostInclude, err := toolaction.EncodeExecutionRootProvenancePath("host", "external/clang/include")
	if err != nil {
		t.Fatal(err)
	}
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments:        []string{"-I${tree:kernel}/include", "-c", "${source:src:00000000}", "-o", "${output:00000000}"},
		Environment:      map[string]string{"HOST_INCLUDE": hostInclude},
		WorkingDirectory: "compile",
		Sources:          []string{"src:00000000"}, Outputs: []string{"00000000"}, Trees: []string{"kernel"},
	}
	recipeID, err := recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	node := ActionPlanNode{
		Stage: "target", Kind: "compile", Recipe: recipeID, Tool: "cc", Product: "vmlinux",
		Sources: []ActionPlanSourceEdge{{Role: "src", SourceID: "src-00000001"}},
		Trees:   []string{"kernel"},
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "init/main.o"}},
	}
	node.ID = node.ContentID()
	plan := &ActionPlan{
		Toolsets: map[string]string{"host": actionPlanTestProbeIdentity, "target": actionPlanTestProbeIdentity},
		Sources:  []ActionPlanSource{{ID: "src-00000001", Namespace: "kernel", Path: "init/main.c"}},
		Recipes:  map[string]ActionRecipe{recipeID: recipe}, Nodes: []ActionPlanNode{node},
	}
	first := actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))
	second := actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))
	if err := os.MkdirAll(second["target"], 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(second["target"])
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.WriteStages(first); err != nil {
		t.Fatal(err)
	}
	if err := plan.WriteStages(second); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(second["target"])
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("WriteStages replaced the precreated TreeArtifact root")
	}
	a, err := readActionPlanFilesForTest(first["target"])
	if err != nil {
		t.Fatal(err)
	}
	b, err := readActionPlanFilesForTest(second["target"])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("plans differ:\n%v\n%v", a, b)
	}
	emptyBindings := ActionPlanInputBindings{
		Schema:   LinuxKernelInputBindingsSchema,
		Bindings: map[string]ActionPlanInputBinding{},
	}
	emptyBindingsID, err := emptyBindings.ID()
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		"schema/" + LinuxKernelPlanSchema,
		"toolsets/host/" + actionPlanTestProbeIdentity,
		"toolsets/target/" + actionPlanTestProbeIdentity,
		"index/00000000/" + node.ID,
		"sources/src-00000001/kernel/init/main.c",
		"recipes/" + recipeID + ".json",
		"nodes/target/" + node.ID + "/kind/compile",
		"nodes/target/" + node.ID + "/in/source/src/00000000/src-00000001",
		"nodes/target/" + node.ID + "/in/tree/kernel",
		"nodes/target/" + node.ID + "/in/toolset/host",
		"nodes/target/" + node.ID + "/in/bindings/" + emptyBindingsID + ".json",
		"nodes/target/" + node.ID + "/out/objects/00000000/init/main.o",
	} {
		if _, ok := a[marker]; !ok {
			t.Errorf("missing marker %s", marker)
		}
	}
}

func TestWriteActionPlanTreeRollsBackPartiallyPublishedPrecreatedRoot(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "plan")
	if err := os.Mkdir(output, 0o755); err != nil {
		t.Fatal(err)
	}
	entries := []actionPlanEntry{
		{path: "first/marker", data: []byte("first\n")},
		{path: "second/marker", data: []byte("second\n")},
		{path: "third/marker", data: []byte("third\n")},
	}
	renameCalls := 0
	err := writeActionPlanTreeUsingRename(output, entries, func(oldPath, newPath string) error {
		renameCalls++
		if renameCalls == 2 {
			return fs.ErrPermission
		}
		return os.Rename(oldPath, newPath)
	})
	if err == nil || !strings.Contains(err.Error(), "publish action-plan child") {
		t.Fatalf("partial publication error = %v, want injected child rename failure", err)
	}
	if renameCalls != 3 {
		t.Fatalf("rename calls = %d, want first publication, failed second publication, and rollback", renameCalls)
	}
	children, err := os.ReadDir(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 0 {
		t.Fatalf("precreated output retains children after rollback: %v", children)
	}
	temporary, err := filepath.Glob(filepath.Join(root, ".plan.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temporary) != 0 {
		t.Fatalf("action-plan staging directories remain after rollback: %v", temporary)
	}
}

func decodeActionPlanPackedInputMarkerForTest(marker string) (string, int, []actionPlanPackedInputTuple, error) {
	parts := strings.Split(marker, "/")
	pack := slices.Index(parts, "node-pack")
	if pack < 0 || pack+3 != len(parts) {
		return "", 0, nil, fmt.Errorf("invalid packed input marker %q", marker)
	}
	role := parts[pack+1]
	filename := parts[pack+2]
	if len(filename) > maximumActionPlanInputMarkerFilename {
		return "", 0, nil, fmt.Errorf("packed input marker filename has %d bytes", len(filename))
	}
	if len(filename) < 10 || filename[8] != '.' {
		return "", 0, nil, fmt.Errorf("invalid packed input marker filename %q", filename)
	}
	chunk, err := strconv.Atoi(filename[:8])
	if err != nil {
		return "", 0, nil, err
	}
	encoded := filename[9:]
	tuples := make([]actionPlanPackedInputTuple, 0, strings.Count(encoded, ",")+1)
	for _, value := range strings.Split(encoded, ",") {
		fields := strings.Split(value, ".")
		if len(fields) != 3 {
			return "", 0, nil, fmt.Errorf("invalid packed input tuple %q", value)
		}
		decoded := [3]int{}
		for index, field := range fields {
			value, err := strconv.ParseInt(field, 36, 64)
			if err != nil {
				return "", 0, nil, err
			}
			decoded[index] = int(value)
		}
		tuples = append(tuples, actionPlanPackedInputTuple{
			inputOrdinal: decoded[0], producerOrdinal: decoded[1], slot: decoded[2],
		})
	}
	return role, chunk, tuples, nil
}

func TestActionPlanNodeOrdinalIndexUsesLexicalNodeIDs(t *testing.T) {
	ids := []string{strings.Repeat("f", 64), strings.Repeat("0", 64), strings.Repeat("7", 64)}
	want := map[string]int{ids[1]: 0, ids[2]: 1, ids[0]: 2}
	for _, order := range [][]int{{0, 1, 2}, {2, 0, 1}, {1, 2, 0}} {
		nodes := make([]ActionPlanNode, len(order))
		for index, source := range order {
			nodes[index].ID = ids[source]
		}
		got, sortedIDs, err := actionPlanNodeOrdinalIndex(nodes)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) || !slices.Equal(sortedIDs, []string{ids[1], ids[2], ids[0]}) {
			t.Fatalf("node ordinal index = %#v/%q, want %#v", got, sortedIDs, want)
		}
	}
}

func TestActionPlanInputOrdinalRequiresEightDecimalDigits(t *testing.T) {
	if err := validateActionPlanInputOrdinal("consumer", maximumActionPlanOrdinal); err != nil {
		t.Fatalf("maximum input ordinal was rejected: %v", err)
	}
	want := "node consumer input ordinal 100000000 is out of range"
	if err := validateActionPlanInputOrdinal("consumer", maximumActionPlanOrdinal+1); err == nil || err.Error() != want {
		t.Fatalf("oversized input ordinal error = %v, want %q", err, want)
	}
}

func TestValidatePlanNameRejectsOversizedMarkerComponent(t *testing.T) {
	if err := validatePlanName("input role", strings.Repeat("a", maximumActionPlanNameComponent)); err != nil {
		t.Fatalf("maximum marker component was rejected: %v", err)
	}
	err := validatePlanName("input role", strings.Repeat("a", maximumActionPlanNameComponent+1))
	if err == nil || !strings.Contains(err.Error(), "exceeds 240-byte plan component limit") {
		t.Fatalf("oversized marker component error = %v", err)
	}
}

func TestActionPlanPacksLargeInputSetAndRoundTripsTuples(t *testing.T) {
	producerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
		Arguments: []string{"${output:00000000}", "${output:00000001}"},
		Outputs:   []string{"00000000", "00000001"},
	}
	producerRecipeID, err := producerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	producers := make([]ActionPlanNode, 65)
	inputs := make([]ActionPlanNodeEdge, 0, 130)
	inputBindings := make([]string, 0, 130)
	for producerIndex := range producers {
		producer := ActionPlanNode{
			Stage: "target", Kind: "generate", Recipe: producerRecipeID, Tool: "cc", Product: "vmlinux",
			Outputs: []ActionPlanOutput{
				{Tree: "objects", Path: fmt.Sprintf("generated/%03d-a", producerIndex)},
				{Tree: "objects", Path: fmt.Sprintf("generated/%03d-b", producerIndex)},
			},
		}
		producer.ID = producer.ContentID()
		producers[producerIndex] = producer
		for slot := range producer.Outputs {
			ordinal := len(inputs)
			inputs = append(inputs, ActionPlanNodeEdge{Role: "payload", ProducerID: producer.ID, Slot: slot})
			inputBindings = append(inputBindings, "payload:"+planOrdinal(ordinal))
		}
	}
	consumerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
		Arguments: []string{"${output:00000000}"}, Inputs: inputBindings, Outputs: []string{"00000000"},
	}
	consumerRecipeID, err := consumerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	consumer := ActionPlanNode{
		Stage: "target", Kind: "generate", Recipe: consumerRecipeID, Tool: "cc", Product: "vmlinux",
		Inputs: inputs, Outputs: []ActionPlanOutput{{Tree: "objects", Path: "generated/consumer"}},
	}
	consumer.ID = consumer.ContentID()
	nodes := append(slices.Clone(producers), consumer)
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes: map[string]ActionRecipe{
			producerRecipeID: producerRecipe,
			consumerRecipeID: consumerRecipe,
		},
		Nodes: nodes,
	}
	outputs := actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))
	if err := plan.WriteStages(outputs); err != nil {
		t.Fatal(err)
	}
	files, err := readActionPlanFilesForTest(outputs["target"])
	if err != nil {
		t.Fatal(err)
	}
	prefix := "nodes/target/" + consumer.ID + "/in/node-pack/payload/"
	markers := []string{}
	for marker := range files {
		if strings.HasPrefix(marker, prefix) {
			markers = append(markers, marker)
		}
		if strings.HasPrefix(marker, "nodes/target/"+consumer.ID+"/in/node/") {
			t.Fatalf("plan retained unpacked node-input marker %q", marker)
		}
	}
	sort.Strings(markers)
	if len(markers) < 3 || len(markers) >= len(inputs) {
		t.Fatalf("packed input markers = %d, want a bounded multi-marker packing of %d edges: %q", len(markers), len(inputs), markers)
	}
	nodeOrdinals, _, err := actionPlanNodeOrdinalIndex(nodes)
	if err != nil {
		t.Fatal(err)
	}
	wantTuples := make([]actionPlanPackedInputTuple, len(inputs))
	for ordinal, input := range inputs {
		wantTuples[ordinal] = actionPlanPackedInputTuple{
			inputOrdinal: ordinal, producerOrdinal: nodeOrdinals[input.ProducerID], slot: input.Slot,
		}
	}
	gotTuples := []actionPlanPackedInputTuple{}
	for chunk, marker := range markers {
		role, gotChunk, tuples, err := decodeActionPlanPackedInputMarkerForTest(marker)
		if err != nil {
			t.Fatal(err)
		}
		if role != "payload" || gotChunk != chunk {
			t.Fatalf("packed marker %q decoded role/chunk %q/%d, want payload/%d", marker, role, gotChunk, chunk)
		}
		if len(tuples) > maximumActionPlanInputTuplesPerMarker {
			t.Fatalf("packed marker %q contains %d tuples, want at most %d", marker, len(tuples), maximumActionPlanInputTuplesPerMarker)
		}
		if marker != strings.ToLower(marker) {
			t.Fatalf("packed marker contains uppercase base36 data: %q", marker)
		}
		gotTuples = append(gotTuples, tuples...)
	}
	if !reflect.DeepEqual(gotTuples, wantTuples) {
		t.Fatalf("packed input tuples = %#v, want %#v", gotTuples, wantTuples)
	}
}

func TestActionPlanWritesCanonicalTwelveThousandInputBindingManifest(t *testing.T) {
	producerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
		Arguments: []string{"${output:00000000}"}, Outputs: []string{"00000000"},
	}
	producerRecipeID, err := producerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	producer := ActionPlanNode{
		Stage: "target", Kind: "generate", Recipe: producerRecipeID, Tool: "cc", Product: "vmlinux",
		Outputs: []ActionPlanOutput{{
			Tree:         "objects",
			Path:         "generated/logical-input.o",
			ArtifactPath: ".linux-bzl-versions/producer/generated/physical-input.o",
		}},
	}
	producer.ID = producer.ContentID()

	const bindingCount = 12000
	inputEdges := make([]ActionPlanNodeEdge, bindingCount)
	inputNames := make([]string, bindingCount)
	arguments := make([]string, 0, bindingCount+1)
	arguments = append(arguments, "${output:00000000}")
	for ordinal := range bindingCount {
		name := "payload:" + planOrdinal(ordinal)
		inputEdges[ordinal] = ActionPlanNodeEdge{Role: "payload", ProducerID: producer.ID, Slot: 0}
		inputNames[ordinal] = name
		arguments = append(arguments, "${input:"+name+"}")
	}
	consumerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
		Arguments: arguments, Inputs: inputNames, Outputs: []string{"00000000"},
	}
	consumerRecipeID, err := consumerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	consumer := ActionPlanNode{
		Stage: "target", Kind: "generate", Recipe: consumerRecipeID, Tool: "cc", Product: "vmlinux",
		Inputs: inputEdges, Outputs: []ActionPlanOutput{{Tree: "objects", Path: "generated/consumer"}},
	}
	consumer.ID = consumer.ContentID()
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes: map[string]ActionRecipe{
			producerRecipeID: producerRecipe,
			consumerRecipeID: consumerRecipe,
		},
		Nodes: []ActionPlanNode{producer, consumer},
	}
	outputs := actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))
	if err := plan.WriteStages(outputs); err != nil {
		t.Fatal(err)
	}
	files, err := readActionPlanFilesForTest(outputs["target"])
	if err != nil {
		t.Fatal(err)
	}
	prefix := "nodes/target/" + consumer.ID + "/in/bindings/"
	manifestPaths := []string{}
	packedMarkers := 0
	for filename := range files {
		if strings.HasPrefix(filename, prefix) {
			manifestPaths = append(manifestPaths, filename)
		}
		if strings.HasPrefix(filename, "nodes/target/"+consumer.ID+"/in/node-pack/") {
			packedMarkers++
		}
	}
	if len(manifestPaths) != 1 {
		t.Fatalf("input-binding manifests = %q, want exactly one", manifestPaths)
	}
	if packedMarkers == 0 {
		t.Fatal("large input node omitted its packed graph-edge markers")
	}
	data := files[manifestPaths[0]]
	bindings, err := DecodeActionPlanInputBindings(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings.Bindings) != bindingCount {
		t.Fatalf("decoded input bindings = %d, want %d", len(bindings.Bindings), bindingCount)
	}
	wantBinding := ActionPlanInputBinding{
		Tree: "objects",
		Path: ".linux-bzl-versions/producer/generated/physical-input.o",
	}
	for _, ordinal := range []int{0, bindingCount / 2, bindingCount - 1} {
		key := "payload:" + planOrdinal(ordinal)
		if got := bindings.Bindings[key]; got != wantBinding {
			t.Fatalf("binding %s = %#v, want %#v", key, got, wantBinding)
		}
	}
	manifestID, err := bindings.ID()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSuffix(strings.TrimPrefix(manifestPaths[0], prefix), ".json"); got != manifestID {
		t.Fatalf("manifest filename ID = %q, want %q", got, manifestID)
	}
	canonical, err := bindings.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(data, canonical) {
		t.Fatal("written input bindings differ from their canonical encoding")
	}
}

func TestActionPlanInputBindingsCanonicalCodecAndTamperDetection(t *testing.T) {
	bindings := ActionPlanInputBindings{
		Schema: LinuxKernelInputBindingsSchema,
		Bindings: map[string]ActionPlanInputBinding{
			"alpha:00000000":   {Tree: "objects", Path: "generated/alpha.o"},
			"payload:00000001": {Tree: "host", Path: "bin/generator"},
		},
	}
	canonical, err := bindings.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	wantCanonical := `{"schema":"linux-kernel-input-bindings-v1","bindings":{"alpha:00000000":{"tree":"objects","path":"generated/alpha.o"},"payload:00000001":{"tree":"host","path":"bin/generator"}}}` + "\n"
	if string(canonical) != wantCanonical {
		t.Fatalf("canonical input bindings = %s, want %s", canonical, wantCanonical)
	}
	decoded, err := DecodeActionPlanInputBindings(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, bindings) {
		t.Fatalf("decoded input bindings = %#v, want %#v", decoded, bindings)
	}
	originalID, err := bindings.ID()
	if err != nil {
		t.Fatal(err)
	}
	tampered := ActionPlanInputBindings{
		Schema:   bindings.Schema,
		Bindings: maps.Clone(bindings.Bindings),
	}
	tampered.Bindings["payload:00000001"] = ActionPlanInputBinding{Tree: "host", Path: "bin/other-generator"}
	tamperedData, err := tampered.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeActionPlanInputBindings(tamperedData); err != nil {
		t.Fatalf("canonical tampered manifest should decode before its expected ID is checked: %v", err)
	}
	tamperedID, err := tampered.ID()
	if err != nil {
		t.Fatal(err)
	}
	if tamperedID == originalID {
		t.Fatal("physical-path tampering did not change the canonical manifest ID")
	}

	for _, test := range []struct {
		name string
		data []byte
		want string
	}{
		{name: "missing canonical newline", data: canonical[:len(canonical)-1], want: "not canonically encoded"},
		{name: "trailing whitespace", data: append(slices.Clone(canonical), ' '), want: "not canonically encoded"},
		{name: "unknown top-level field", data: []byte(`{"schema":"linux-kernel-input-bindings-v1","bindings":{},"extra":true}` + "\n"), want: "unknown field"},
		{name: "unknown binding field", data: []byte(`{"schema":"linux-kernel-input-bindings-v1","bindings":{"payload:00000000":{"tree":"objects","path":"one.o","extra":true}}}` + "\n"), want: "unknown field"},
		{name: "wrong schema", data: []byte(`{"schema":"linux-kernel-input-bindings-v2","bindings":{}}` + "\n"), want: "schema"},
		{name: "malformed key", data: []byte(`{"schema":"linux-kernel-input-bindings-v1","bindings":{"payload:0":{"tree":"objects","path":"one.o"}}}` + "\n"), want: "eight-digit ordinal"},
		{name: "noncontiguous ordinal", data: []byte(`{"schema":"linux-kernel-input-bindings-v1","bindings":{"payload:00000001":{"tree":"objects","path":"one.o"}}}` + "\n"), want: "non-contiguous ordinal"},
		{name: "repeated ordinal", data: []byte(`{"schema":"linux-kernel-input-bindings-v1","bindings":{"alpha:00000000":{"tree":"objects","path":"one.o"},"payload:00000000":{"tree":"objects","path":"two.o"}}}` + "\n"), want: "repeat ordinal"},
		{name: "unknown tree", data: []byte(`{"schema":"linux-kernel-input-bindings-v1","bindings":{"payload:00000000":{"tree":"unknown","path":"one.o"}}}` + "\n"), want: "unknown tree"},
		{name: "noncanonical path", data: []byte(`{"schema":"linux-kernel-input-bindings-v1","bindings":{"payload:00000000":{"tree":"objects","path":"../one.o"}}}` + "\n"), want: "canonical relative path"},
		{name: "trailing value", data: append(slices.Clone(canonical), []byte("{}\n")...), want: "trailing JSON value"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeActionPlanInputBindings(test.data); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodeActionPlanInputBindings() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestActionPlanWritesSelfContainedStageShards(t *testing.T) {
	producerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
		Arguments: []string{"-o", "${output:00000000}", "--also", "${output:00000001}"},
		Outputs:   []string{"00000000", "00000001"},
	}
	producerRecipeID, err := producerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	consumerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments: []string{"${source:src:00000000}", "${input:payload:00000000}", "-o", "${output:00000000}"},
		Sources:   []string{"src:00000000"}, Inputs: []string{"payload:00000000"}, Outputs: []string{"00000000"},
	}
	consumerRecipeID, err := consumerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	producer := ActionPlanNode{
		Stage: "prehost", Kind: "generate", Recipe: producerRecipeID, Tool: "cc", Product: "sdk",
		Outputs: []ActionPlanOutput{
			{Tree: "prehost", Path: "bin/helper"},
			{Tree: "prehost", Path: "bin/unreferenced-helper"},
		},
	}
	producer.ID = producer.ContentID()
	consumer := ActionPlanNode{
		Stage: "target", Kind: "compile", Recipe: consumerRecipeID, Tool: "cc", Product: "vmlinux",
		Sources: []ActionPlanSourceEdge{{Role: "src", SourceID: "src-00000001"}},
		Inputs:  []ActionPlanNodeEdge{{Role: "payload", ProducerID: producer.ID, Slot: 0}},
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "consumer.o"}},
	}
	consumer.ID = consumer.ContentID()
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
		Sources:  []ActionPlanSource{{ID: "src-00000001", Namespace: "kernel", Path: "consumer.c"}},
		Recipes: map[string]ActionRecipe{
			producerRecipeID: producerRecipe,
			consumerRecipeID: consumerRecipe,
		},
		Nodes: []ActionPlanNode{producer, consumer},
	}
	outputs := actionPlanStageOutputsForTest(t.TempDir())
	if err := plan.WriteStages(outputs); err != nil {
		t.Fatal(err)
	}
	prehost, err := readActionPlanFilesForTest(outputs["prehost"])
	if err != nil {
		t.Fatal(err)
	}
	target, err := readActionPlanFilesForTest(outputs["target"])
	if err != nil {
		t.Fatal(err)
	}
	nodeOrdinals, _, err := actionPlanNodeOrdinalIndex(plan.Nodes)
	if err != nil {
		t.Fatal(err)
	}
	assertIndexes := func(stage string, shard map[string][]byte) {
		t.Helper()
		for _, node := range plan.Nodes {
			marker := "index/" + planOrdinal(nodeOrdinals[node.ID]) + "/" + node.ID
			if _, ok := shard[marker]; !ok {
				t.Errorf("%s shard omits node index %s", stage, marker)
			}
		}
	}
	assertIndexes("prehost", prehost)
	assertIndexes("target", target)
	producerRoot := "nodes/prehost/" + producer.ID
	consumerRoot := "nodes/target/" + consumer.ID
	producerOutput := producerRoot + "/out/prehost/00000000/bin/helper"
	producerSiblingOutput := producerRoot + "/out/prehost/00000001/bin/unreferenced-helper"
	for _, marker := range []string{
		"schema/" + LinuxKernelPlanSchema,
		"recipes/" + producerRecipeID + ".json",
		producerRoot + "/kind/generate",
		producerOutput,
		producerSiblingOutput,
	} {
		if _, ok := prehost[marker]; !ok {
			t.Errorf("prehost shard omits %s", marker)
		}
	}
	for _, marker := range []string{
		"schema/" + LinuxKernelPlanSchema,
		"recipes/" + consumerRecipeID + ".json",
		"sources/src-00000001/kernel/consumer.c",
		consumerRoot + "/kind/compile",
		producerOutput,
	} {
		if _, ok := target[marker]; !ok {
			t.Errorf("target shard omits %s", marker)
		}
	}
	for marker := range target {
		if strings.HasPrefix(marker, producerRoot+"/") && marker != producerOutput {
			t.Errorf("target shard retains unrelated producer metadata %s", marker)
		}
		if marker == "recipes/"+producerRecipeID+".json" {
			t.Errorf("target shard retains producer-only recipe %s", marker)
		}
	}
	if _, ok := target[producerSiblingOutput]; ok {
		t.Errorf("target shard retains unreferenced producer output %s", producerSiblingOutput)
	}
	for _, stage := range []string{"bootstrap", "host", "prep"} {
		shard, err := readActionPlanFilesForTest(outputs[stage])
		if err != nil {
			t.Fatal(err)
		}
		assertIndexes(stage, shard)
		for marker := range shard {
			if strings.HasPrefix(marker, "nodes/") || strings.HasPrefix(marker, "recipes/") || strings.HasPrefix(marker, "sources/") {
				t.Errorf("empty %s shard retains action marker %s", stage, marker)
			}
		}
	}
}

func TestActionPlanWritesStageShardsWithSharedInputSets(t *testing.T) {
	producerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
		Arguments: []string{"${output:00000000}", "${output:00000001}", "${output:00000002}"},
		Outputs:   []string{"00000000", "00000001", "00000002"},
	}
	producerRecipeID, err := producerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	consumerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
		Arguments: []string{"${output:00000000}"}, Outputs: []string{"00000000"},
	}
	consumerRecipeID, err := consumerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	producer := ActionPlanNode{
		Stage: "prehost", Kind: "generate", Recipe: producerRecipeID, Tool: "cc", Product: "sdk",
		Outputs: []ActionPlanOutput{
			{Tree: "prehost", Path: "shared"},
			{Tree: "prehost", Path: "target-only"},
			{Tree: "prehost", Path: "unused"},
		},
	}
	producer.ID = producer.ContentID()
	store := NewActionPlanInputSetStore()
	insert := func(root string, entry ActionPlanInputSetEntry) string {
		t.Helper()
		next, err := store.Insert(root, entry)
		if err != nil {
			t.Fatal(err)
		}
		return next
	}
	const sharedSourceCount = 40
	sources := make([]ActionPlanSource, sharedSourceCount+3)
	sharedRoot := ""
	for index := range sources {
		sources[index] = ActionPlanSource{
			ID: "src-" + planOrdinal(index+1), Namespace: "kernel", Path: fmt.Sprintf("include/%d.h", index),
		}
		if index < sharedSourceCount {
			sharedRoot = insert(sharedRoot, ActionPlanInputSetEntry{
				Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: sources[index].Path},
				SourceID: sources[index].ID,
			})
		}
	}
	sharedRoot = insert(sharedRoot, ActionPlanInputSetEntry{
		Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "shared"}, ProducerID: producer.ID,
	})
	newConsumer := func(stage, root, output string) ActionPlanNode {
		tree := "objects"
		if stage == "prep" {
			tree = "prep"
		}
		node := ActionPlanNode{
			Stage: stage, Kind: "generate", Recipe: consumerRecipeID, Tool: "cc", Product: "sdk", InputSet: root,
			Outputs: []ActionPlanOutput{{Tree: tree, Path: output}},
		}
		node.ID = node.ContentID()
		return node
	}
	prep := newConsumer("prep", sharedRoot, "prep.o")
	targetShared := newConsumer("target", sharedRoot, "shared.o")
	targetRoots := []string{sharedRoot}
	for index := range 2 {
		source := sources[sharedSourceCount+index]
		root := insert(sharedRoot, ActionPlanInputSetEntry{
			Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: source.Path}, SourceID: source.ID,
		})
		if index == 0 {
			root = insert(root, ActionPlanInputSetEntry{
				Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "target-only"},
				ProducerID: producer.ID, Slot: 1,
			})
		} else {
			for _, dependency := range []ActionPlanNode{prep, targetShared} {
				root = insert(root, ActionPlanInputSetEntry{
					Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: dependency.Outputs[0].Path},
					ProducerID: dependency.ID,
				})
			}
		}
		targetRoots = append(targetRoots, root)
	}
	sharedClosure, err := store.ReachableNodes(sharedRoot)
	if err != nil {
		t.Fatal(err)
	}
	targetClosure, err := store.ReachableNodesForRoots(targetRoots)
	if err != nil {
		t.Fatal(err)
	}
	if len(sharedClosure) < 2 || len(targetClosure) <= len(sharedClosure) {
		t.Fatalf("fixture must share a branched root and add target-only nodes: shared=%d target=%d", len(sharedClosure), len(targetClosure))
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
		Sources:  sources, InputSets: targetClosure,
		Recipes: map[string]ActionRecipe{producerRecipeID: producerRecipe, consumerRecipeID: consumerRecipe},
		// Producers deliberately follow consumers, including the same-stage edge.
		Nodes: []ActionPlanNode{
			newConsumer("target", targetRoots[2], "right.o"), newConsumer("target", targetRoots[1], "left.o"),
			targetShared, prep, producer,
		},
	}
	allEntries, err := plan.entries()
	if err != nil {
		t.Fatal(err)
	}
	outputs := actionPlanStageOutputsForTest(t.TempDir())
	if err := plan.WriteStages(outputs); err != nil {
		t.Fatal(err)
	}
	producerOutput := func(node ActionPlanNode, slot int) string {
		output := node.Outputs[slot]
		return "nodes/" + node.Stage + "/" + node.ID + "/out/" + output.Tree + "/" + planOrdinal(slot) + "/" + output.Path
	}
	for _, stage := range linuxKernelPlanStageOrder {
		files, err := readActionPlanFilesForTest(outputs[stage])
		if err != nil {
			t.Fatal(err)
		}
		wantedInputSets := map[string]ActionPlanInputSetNode{}
		if stage == "prep" {
			wantedInputSets = sharedClosure
		} else if stage == "target" {
			wantedInputSets = targetClosure
		}
		wantedSources := map[string]bool{}
		for index, source := range sources {
			wantedSources[source.ID] = stage == "prep" && index < sharedSourceCount || stage == "target" && index < sharedSourceCount+2
		}
		wantedCrossStageOutputs := map[string]bool{
			producerOutput(producer, 0): stage == "prep" || stage == "target",
			producerOutput(producer, 1): stage == "target",
			producerOutput(prep, 0):     stage == "target",
		}
		for _, entry := range allEntries {
			parts := strings.Split(entry.path, "/")
			want := false
			switch parts[0] {
			case "schema", "toolsets", "index":
				want = true
			case "recipes":
				want = stage == "prehost" && parts[1] == producerRecipeID+".json" ||
					(stage == "prep" || stage == "target") && parts[1] == consumerRecipeID+".json"
			case "sources":
				want = wantedSources[parts[1]]
			case "input-sets":
				_, want = wantedInputSets[parts[1]]
			case "nodes":
				want = parts[1] == stage || wantedCrossStageOutputs[entry.path]
			default:
				t.Fatalf("unexpected fixture marker %s", entry.path)
			}
			data, exists := files[entry.path]
			if exists != want {
				t.Errorf("%s shard contains %s = %v, want %v", stage, entry.path, exists, want)
			} else if exists && !slices.Equal(data, entry.data) {
				t.Errorf("%s shard changed marker contents for %s", stage, entry.path)
			}
			delete(files, entry.path)
		}
		for marker := range files {
			t.Errorf("%s shard added unknown marker %s", stage, marker)
		}
	}
}

func TestActionPlanSeparatesLogicalAndPhysicalOutputPaths(t *testing.T) {
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
		Arguments: []string{"-o", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	recipeID, err := recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	versioned := ActionPlanNode{
		Stage: "target", Kind: "generate", Recipe: recipeID, Tool: "cc", Product: "vmlinux",
		Outputs: []ActionPlanOutput{{
			Tree: "objects", Path: "generated/shared.o",
			ArtifactPath: ".linux-bzl-versions/first/generated/shared.o",
		}},
	}
	versioned.ID = versioned.ContentID()
	canonical := ActionPlanNode{
		Stage: "target", Kind: "generate", Recipe: recipeID, Tool: "cc", Product: "vmlinux",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "generated/shared.o"}},
	}
	canonical.ID = canonical.ContentID()
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{recipeID: recipe},
		Nodes:    []ActionPlanNode{versioned, canonical},
		Products: []ActionPlanProduct{{Name: "vmlinux", Tree: "objects", Path: "generated/shared.o"}},
	}
	producer, slot, ok := planProducerByOutput(plan, "objects", "generated/shared.o")
	if !ok || producer != canonical.ID || slot != 0 {
		t.Fatalf("canonical logical producer = (%q, %d, %t), want %s slot 0", producer, slot, ok, canonical.ID)
	}
	entries, err := plan.entries()
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, entry := range entries {
		paths[entry.path] = true
	}
	versionedMarker := "nodes/target/" + versioned.ID + "/out/objects/00000000/.linux-bzl-versions/first/generated/shared.o"
	if !paths[versionedMarker] {
		t.Fatalf("plan omits physical version marker %q", versionedMarker)
	}
	logicalMarker := "nodes/target/" + versioned.ID + "/out/objects/00000000/generated/shared.o"
	if paths[logicalMarker] {
		t.Fatalf("versioned node was serialized at logical path %q", logicalMarker)
	}
}

func TestActionPlanRejectsDuplicatePhysicalOutputPaths(t *testing.T) {
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
		Arguments: []string{"-o", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	recipeID, err := recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	nodes := []ActionPlanNode{
		{
			Stage: "target", Kind: "generate", Recipe: recipeID, Tool: "cc", Product: "vmlinux",
			Outputs: []ActionPlanOutput{{Tree: "objects", Path: "first.o", ArtifactPath: ".linux-bzl-versions/shared/output.o"}},
		},
		{
			Stage: "target", Kind: "generate", Recipe: recipeID, Tool: "cc", Product: "vmlinux",
			Outputs: []ActionPlanOutput{{Tree: "objects", Path: "second.o", ArtifactPath: ".linux-bzl-versions/shared/output.o"}},
		},
	}
	for index := range nodes {
		nodes[index].ID = nodes[index].ContentID()
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{recipeID: recipe},
		Nodes:    nodes,
	}
	if _, err := plan.entries(); err == nil || !strings.Contains(err.Error(), "physical output") || !strings.Contains(err.Error(), "owned by both") {
		t.Fatalf("duplicate physical output error = %v", err)
	}
}

func TestActionPlanOutputArtifactPathParticipatesInContentIdentity(t *testing.T) {
	base := ActionPlanNode{
		Stage: "target", Kind: "generate", Recipe: strings.Repeat("a", 64), Tool: "cc", Product: "vmlinux",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "generated/shared.o"}},
	}
	explicitCanonical := base
	explicitCanonical.Outputs = []ActionPlanOutput{{
		Tree: "objects", Path: "generated/shared.o", ArtifactPath: "generated/shared.o",
	}}
	versioned := base
	versioned.Outputs = []ActionPlanOutput{{
		Tree: "objects", Path: "generated/shared.o", ArtifactPath: ".linux-bzl-versions/first/generated/shared.o",
	}}
	if base.ContentID() != explicitCanonical.ContentID() {
		t.Fatal("empty ArtifactPath and an explicit logical ArtifactPath have different identities")
	}
	if base.ContentID() == versioned.ContentID() {
		t.Fatal("physical ArtifactPath does not participate in node content identity")
	}
}

func TestActionRecipeWorkingRootPlaceholderRequiresPrivateDirectory(t *testing.T) {
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
		Arguments: []string{"${work:root}", "${output:00000000}"},
		Outputs:   []string{"00000000"},
	}
	if _, err := recipe.CanonicalJSON(); err == nil || !strings.Contains(err.Error(), `undeclared work binding "root"`) {
		t.Fatalf("CanonicalJSON() error = %v", err)
	}
	recipe.WorkingDirectory = "private"
	if _, err := recipe.CanonicalJSON(); err != nil {
		t.Fatalf("CanonicalJSON() with a private working root failed: %v", err)
	}
}

func TestActionRecipeExecutionDirectoryRequiresCanonicalRelativePath(t *testing.T) {
	base := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
		Arguments: []string{"${output:00000000}"},
		Outputs:   []string{"00000000"},
	}
	tests := []struct {
		name             string
		workingDirectory string
		executionDir     string
		want             string
	}{
		{name: "working directory required", executionDir: "drivers/example", want: "requires working_directory"},
		{name: "absolute", workingDirectory: "private", executionDir: "/outside", want: "not a canonical relative path"},
		{name: "parent escape", workingDirectory: "private", executionDir: "../outside", want: "not a canonical relative path"},
		{name: "cleaned parent escape", workingDirectory: "private", executionDir: "drivers/../outside", want: "not a canonical relative path"},
		{name: "working root spelling", workingDirectory: "private", executionDir: ".", want: "not a canonical relative path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recipe := base
			recipe.WorkingDirectory = test.workingDirectory
			recipe.ExecutionDirectory = test.executionDir
			if _, err := recipe.CanonicalJSON(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("CanonicalJSON() error = %v, want %q", err, test.want)
			}
		})
	}

	valid := base
	valid.WorkingDirectory = "private"
	valid.ExecutionDirectory = "drivers/example"
	if _, err := valid.CanonicalJSON(); err != nil {
		t.Fatalf("CanonicalJSON() rejected canonical execution directory: %v", err)
	}
}

func TestActionRecipeWorkingDirectoriesRequireCanonicalDistinctPaths(t *testing.T) {
	base := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
		Arguments: []string{"${output:00000000}"},
		Inputs:    []string{"source"},
		Outputs:   []string{"00000000"},
	}
	for _, test := range []struct {
		name             string
		workingDirectory string
		executionDir     string
		directories      []string
		workingInputs    map[string]string
		workingOutputs   map[string]string
		want             string
	}{
		{name: "working directory required", directories: []string{"scripts"}, want: "require working_directory"},
		{name: "empty", workingDirectory: "private", directories: []string{""}, want: "not a canonical relative path"},
		{name: "absolute", workingDirectory: "private", directories: []string{"/outside"}, want: "not a canonical relative path"},
		{name: "parent escape", workingDirectory: "private", directories: []string{"../outside"}, want: "not a canonical relative path"},
		{name: "cleaned parent escape", workingDirectory: "private", directories: []string{"scripts/../outside"}, want: "not a canonical relative path"},
		{name: "working root spelling", workingDirectory: "private", directories: []string{"."}, want: "not a canonical relative path"},
		{name: "duplicate", workingDirectory: "private", directories: []string{"scripts", "scripts"}, want: "repeats path"},
		{
			name: "file collision", workingDirectory: "private", directories: []string{"scripts"},
			workingOutputs: map[string]string{"00000000": "scripts"}, want: "working path",
		},
		{
			name: "execution directory input collision", workingDirectory: "private", executionDir: "scripts",
			workingInputs: map[string]string{"input:source": "scripts"}, want: "working path",
		},
		{
			name: "execution directory output collision", workingDirectory: "private", executionDir: "scripts",
			workingOutputs: map[string]string{"00000000": "scripts"}, want: "working path",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			recipe := base
			recipe.WorkingDirectory = test.workingDirectory
			recipe.ExecutionDirectory = test.executionDir
			recipe.WorkingDirectories = test.directories
			recipe.WorkingInputs = test.workingInputs
			recipe.WorkingOutputs = test.workingOutputs
			if _, err := recipe.CanonicalJSON(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("CanonicalJSON() error = %v, want %q", err, test.want)
			}
		})
	}

	valid := base
	valid.WorkingDirectory = "private"
	valid.ExecutionDirectory = "scripts"
	valid.WorkingDirectories = []string{"scripts", "include/generated"}
	canonical, err := valid.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON() rejected canonical working directories: %v", err)
	}
	if !strings.Contains(string(canonical), `"working_directories":["scripts","include/generated"]`) {
		t.Fatalf("CanonicalJSON() omitted working directories: %s", canonical)
	}
}

func TestActionRecipeWorkingTreesRequireDeclaredDistinctBindings(t *testing.T) {
	base := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
		Arguments: []string{"${output:00000000}"},
		Outputs:   []string{"00000000"},
		Trees:     []string{"external", "vendor"},
	}
	for _, test := range []struct {
		name             string
		workingDirectory string
		workingTrees     []string
		want             string
	}{
		{name: "working directory required", workingTrees: []string{"external"}, want: "working_trees require working_directory"},
		{name: "declared binding required", workingDirectory: "private", workingTrees: []string{"missing"}, want: "undeclared tree binding"},
		{name: "duplicate", workingDirectory: "private", workingTrees: []string{"external", "external"}, want: "repeats tree binding"},
		{name: "canonical order", workingDirectory: "private", workingTrees: []string{"vendor", "external"}, want: "canonical lexical order"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recipe := base
			recipe.WorkingDirectory = test.workingDirectory
			recipe.WorkingTrees = test.workingTrees
			if _, err := recipe.CanonicalJSON(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("CanonicalJSON() error = %v, want %q", err, test.want)
			}
		})
	}

	valid := base
	valid.WorkingDirectory = "private"
	valid.WorkingTrees = []string{"external", "vendor"}
	canonical, err := valid.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON() rejected declared working trees: %v", err)
	}
	if !strings.Contains(string(canonical), `"working_trees":["external","vendor"]`) {
		t.Fatalf("CanonicalJSON() omitted working trees: %s", canonical)
	}
}

func TestActionRecipeRejectsFileAncestorPathConflicts(t *testing.T) {
	base := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		WorkingDirectory: "private",
		Inputs:           []string{"source"},
		Outputs:          []string{"result"},
	}
	for _, test := range []struct {
		name               string
		executionDirectory string
		workingDirectories []string
		workingInputs      map[string]string
		workingOutputs     map[string]string
		observedOutputs    map[string]string
	}{
		{
			name:           "input contains output",
			workingInputs:  map[string]string{"input:source": "tree"},
			workingOutputs: map[string]string{"result": "tree/result"},
		},
		{
			name:           "output contains input",
			workingInputs:  map[string]string{"input:source": "tree/input"},
			workingOutputs: map[string]string{"result": "tree"},
		},
		{
			name:            "observed output contains direct output",
			workingOutputs:  map[string]string{"result": "tree/result"},
			observedOutputs: map[string]string{"observed": "tree"},
		},
		{
			name:               "output contains execution directory",
			executionDirectory: "tree/run",
			workingOutputs:     map[string]string{"result": "tree"},
		},
		{
			name:               "input contains declared directory",
			workingDirectories: []string{"tree/generated"},
			workingInputs:      map[string]string{"input:source": "tree"},
			workingOutputs:     map[string]string{"result": "result"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			recipe := base
			recipe.ExecutionDirectory = test.executionDirectory
			recipe.WorkingDirectories = test.workingDirectories
			recipe.WorkingInputs = test.workingInputs
			recipe.WorkingOutputs = test.workingOutputs
			recipe.ObservedOutputs = test.observedOutputs
			if len(test.observedOutputs) != 0 {
				recipe.Outputs = append(recipe.Outputs, "observed")
			}
			if err := recipe.Validate(); err == nil || !strings.Contains(err.Error(), "is an ancestor of") {
				t.Fatalf("Validate error = %v, want structural ancestor rejection", err)
			}
		})
	}

	t.Run("directory ancestors are valid", func(t *testing.T) {
		recipe := base
		recipe.ExecutionDirectory = "tree/run"
		recipe.WorkingDirectories = []string{"tree", "tree/generated"}
		recipe.WorkingInputs = map[string]string{"input:source": "tree/input"}
		recipe.WorkingOutputs = map[string]string{"result": "tree/generated/result"}
		if err := recipe.Validate(); err != nil {
			t.Fatalf("Validate rejected directory ancestors of files: %v", err)
		}
	})

	t.Run("exact input output alias is valid", func(t *testing.T) {
		recipe := base
		recipe.WorkingInputs = map[string]string{"input:source": "tree/value"}
		recipe.WorkingOutputs = map[string]string{"result": "tree/value"}
		if err := recipe.Validate(); err != nil {
			t.Fatalf("Validate rejected an in-place input/output alias: %v", err)
		}
	})
}

func TestValidateActionRecipeWorkingPathStructurePreservesConflictOrder(t *testing.T) {
	for _, test := range []struct {
		name        string
		files       map[string]string
		directories map[string]string
		want        string
	}{
		{
			name: "first parent and file descendant",
			files: map[string]string{
				"zero":         "first parent",
				"zero/z-last":  "later child",
				"zero/a-first": "first child",
				"zeta":         "later parent",
				"zeta/child":   "later-parent child",
			},
			directories: map[string]string{"zero/0-directory": "earlier directory"},
			want:        `recipe working file path "zero" used by first parent is an ancestor of file path "zero/a-first" used by first child`,
		},
		{
			name: "first directory descendant",
			files: map[string]string{
				"tree": "parent",
				"zeta": "unrelated",
			},
			directories: map[string]string{
				"tree/z-last":  "later directory",
				"tree/a-first": "first directory",
			},
			want: `recipe working file path "tree" used by parent is an ancestor of directory path "tree/a-first" used by first directory`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateActionRecipeWorkingPathStructure(test.files, test.directories)
			if err == nil || err.Error() != test.want {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateActionRecipeWorkingPathStructureScalesAcrossIndependentFiles(t *testing.T) {
	const pathCount = 16384
	files := make(map[string]string, pathCount)
	directories := make(map[string]string, pathCount/4)
	for index := 0; index < pathCount; index++ {
		pathname := fmt.Sprintf("files/%08d.value", index)
		files[pathname] = pathname
		if index%4 == 0 {
			directory := fmt.Sprintf("directories/%08d", index)
			directories[directory] = directory
		}
	}
	if err := validateActionRecipeWorkingPathStructure(files, directories); err != nil {
		t.Fatalf("large independent working path set: %v", err)
	}
}

func TestActionRecipeValidatesObservedOutputs(t *testing.T) {
	valid := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		WorkingDirectory: "private",
		WorkingInputs:    map[string]string{"input:candidate": "drivers/example/generated.mod.c"},
		ObservedOutputs:  map[string]string{"capture": "drivers/example/generated.mod.c"},
		ObservedOutputBases: map[string][]string{
			"capture": {"base-first", "base-second"},
		},
		Inputs:  []string{"candidate", "base-first", "base-second"},
		Outputs: []string{"capture"},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate rejected an observed staged input: %v", err)
	}

	for _, test := range []struct {
		name string
		want string
		edit func(*ActionRecipe)
	}{
		{name: "working directory required", want: "require working_directory", edit: func(recipe *ActionRecipe) {
			recipe.WorkingDirectory = ""
		}},
		{name: "declared binding required", want: "not a declared output binding", edit: func(recipe *ActionRecipe) {
			recipe.ObservedOutputs = map[string]string{"missing": "generated.mod.c"}
		}},
		{name: "canonical path required", want: "not a canonical relative path", edit: func(recipe *ActionRecipe) {
			recipe.ObservedOutputs = map[string]string{"capture": "../generated.mod.c"}
		}},
		{name: "direct output conflict", want: "both a working output and an observed output", edit: func(recipe *ActionRecipe) {
			recipe.WorkingOutputs = map[string]string{"capture": "other/generated.mod.c"}
		}},
		{name: "working path conflict", want: "working path", edit: func(recipe *ActionRecipe) {
			recipe.Outputs = append(recipe.Outputs, "direct")
			recipe.WorkingOutputs = map[string]string{"direct": "drivers/example/generated.mod.c"}
		}},
		{name: "duplicate observation", want: "working path", edit: func(recipe *ActionRecipe) {
			recipe.Outputs = append(recipe.Outputs, "second")
			recipe.ObservedOutputs["second"] = "drivers/example/generated.mod.c"
		}},
		{name: "stdout conflict", want: "cannot also capture stdout", edit: func(recipe *ActionRecipe) {
			recipe.Stdout = "capture"
		}},
		{name: "path placeholder conflict", want: "cannot be referenced as a path placeholder", edit: func(recipe *ActionRecipe) {
			recipe.Arguments = []string{"${output:capture}"}
		}},
		{name: "base must name observed output", want: "is not an observed output binding", edit: func(recipe *ActionRecipe) {
			recipe.ObservedOutputBases = map[string][]string{
				"missing": {"base-first"},
			}
		}},
		{name: "base state must be declared", want: "references undeclared input binding", edit: func(recipe *ActionRecipe) {
			recipe.ObservedOutputBases["capture"][0] = "missing"
		}},
		{name: "base state must be unique", want: "repeats state input binding", edit: func(recipe *ActionRecipe) {
			recipe.ObservedOutputBases["capture"][1] = "base-first"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			recipe := cloneActionRecipe(valid)
			test.edit(&recipe)
			if err := recipe.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestActionRecipeValidatesArgumentsFileProtocol(t *testing.T) {
	valid := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments:     []string{"-out", "${output:result}", "-line", "generated"},
		ArgumentsFile: true,
		Outputs:       []string{"result"},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate rejected actionfile response-file recipe: %v", err)
	}
	invalid := valid
	invalid.Tool = "helper"
	if err := invalid.Validate(); err == nil || !strings.Contains(err.Error(), "arguments_file requires actionfile tool") {
		t.Fatalf("Validate error = %v", err)
	}
}

func TestActionPlanRejectsInvalidGraphs(t *testing.T) {
	hostInclude, err := toolaction.EncodeExecutionRootProvenancePath("host", "external/clang/include")
	if err != nil {
		t.Fatal(err)
	}
	valid := func() *ActionPlan {
		recipe := ActionRecipe{Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "objcopy", Arguments: []string{"${input:src:00000000}", "${output:00000000}"}, Inputs: []string{"src:00000000"}, Outputs: []string{"00000000"}}
		rid, _ := recipe.ID()
		producerRecipe := ActionRecipe{Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc", Arguments: []string{"-o", "${output:00000000}"}, Outputs: []string{"00000000"}}
		pid, _ := producerRecipe.ID()
		producer := ActionPlanNode{Stage: "target", Kind: "generate", Recipe: pid, Tool: "cc", Product: "vmlinux", Outputs: []ActionPlanOutput{{Tree: "objects", Path: "producer.o"}}}
		producer.ID = producer.ContentID()
		consumer := ActionPlanNode{Stage: "target", Kind: "copy", Recipe: rid, Tool: "objcopy", Product: "vmlinux", Inputs: []ActionPlanNodeEdge{{Role: "src", ProducerID: producer.ID}}, Outputs: []ActionPlanOutput{{Tree: "objects", Path: "consumer.o"}}}
		consumer.ID = consumer.ContentID()
		return &ActionPlan{Toolsets: map[string]string{"target": actionPlanTestProbeIdentity}, Recipes: map[string]ActionRecipe{rid: recipe, pid: producerRecipe}, Nodes: []ActionPlanNode{producer, consumer}}
	}
	tests := []struct {
		name, want string
		mutate     func(*ActionPlan)
	}{
		{"bad node hash", "does not match canonical content", func(p *ActionPlan) { p.Nodes[0].ID = strings.Repeat("a", 64) }},
		{"unknown producer", "unknown producer", func(p *ActionPlan) {
			p.Nodes[1].Inputs[0].ProducerID = strings.Repeat("b", 64)
			p.Nodes[1].ID = p.Nodes[1].ContentID()
		}},
		{"duplicate output", "owned by both", func(p *ActionPlan) {
			p.Nodes[1].Outputs[0] = p.Nodes[0].Outputs[0]
			p.Nodes[1].ID = p.Nodes[1].ContentID()
		}},
		{"host without toolset", "no host toolset", func(p *ActionPlan) {
			p.Nodes[0].Stage = "host"
			p.Nodes[0].Outputs[0].Tree = "host"
			p.Nodes[0].ID = p.Nodes[0].ContentID()
		}},
		{"recipe host provenance without toolset", "recipe requires unavailable host toolset", func(p *ActionPlan) {
			oldID := p.Nodes[1].Recipe
			recipe := p.Recipes[oldID]
			recipe.Environment = map[string]string{"HOST_INCLUDE": hostInclude}
			newID, err := recipe.ID()
			if err != nil {
				t.Fatal(err)
			}
			delete(p.Recipes, oldID)
			p.Recipes[newID] = recipe
			p.Nodes[1].Recipe = newID
			p.Nodes[1].ID = p.Nodes[1].ContentID()
		}},
		{"stage output tree mismatch", "target stage cannot write prep tree", func(p *ActionPlan) {
			p.Nodes[0].Outputs[0].Tree = "prep"
			p.Nodes[0].ID = p.Nodes[0].ContentID()
		}},
		{"backward stage edge", "backward dependency", func(p *ActionPlan) {
			p.Nodes[1].Stage = "prep"
			p.Nodes[1].Outputs[0].Tree = "prep"
			p.Nodes[1].ID = p.Nodes[1].ContentID()
		}},
		{"prep dependency from host", "backward dependency", func(p *ActionPlan) {
			p.Toolsets["host"] = actionPlanTestProbeIdentity
			p.Nodes[0].Stage = "prep"
			p.Nodes[0].Outputs[0].Tree = "prep"
			p.Nodes[0].ID = p.Nodes[0].ContentID()
			p.Nodes[1].Stage = "host"
			p.Nodes[1].Outputs[0].Tree = "host"
			p.Nodes[1].Inputs[0].ProducerID = p.Nodes[0].ID
			p.Nodes[1].ID = p.Nodes[1].ContentID()
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := valid()
			test.mutate(p)
			err := p.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan")))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestActionPlanAcceptsHostDependencyFromPrep(t *testing.T) {
	recipe := ActionRecipe{Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "objcopy", Arguments: []string{"${input:src:00000000}", "${output:00000000}"}, Inputs: []string{"src:00000000"}, Outputs: []string{"00000000"}}
	recipeID, err := recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	producerRecipe := ActionRecipe{Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc", Arguments: []string{"-o", "${output:00000000}"}, Outputs: []string{"00000000"}}
	producerRecipeID, err := producerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	producer := ActionPlanNode{Stage: "host", Kind: "generate", Recipe: producerRecipeID, Tool: "cc", Product: "sdk", Outputs: []ActionPlanOutput{{Tree: "host", Path: "bin/helper"}}}
	producer.ID = producer.ContentID()
	consumer := ActionPlanNode{Stage: "prep", Kind: "copy", Recipe: recipeID, Tool: "objcopy", Product: "sdk", Inputs: []ActionPlanNodeEdge{{Role: "src", ProducerID: producer.ID}}, Outputs: []ActionPlanOutput{{Tree: "prep", Path: "include/generated/header.h"}}}
	consumer.ID = consumer.ContentID()
	plan := &ActionPlan{
		Toolsets: map[string]string{"host": actionPlanTestProbeIdentity, "target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{producerRecipeID: producerRecipe, recipeID: recipe},
		Nodes:    []ActionPlanNode{producer, consumer},
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("WriteStages rejected host -> prep dependency: %v", err)
	}
}

func TestActionRecipeRejectsUnknownAndUnusedBindings(t *testing.T) {
	for name, recipe := range map[string]ActionRecipe{
		"unknown":            {Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc", Arguments: []string{"${source:missing}"}, Outputs: []string{"out"}},
		"unused output":      {Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc", Arguments: []string{"-c"}, Outputs: []string{"out"}},
		"bad generated tool": {Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "input:generator", Arguments: []string{"${output:out}"}, Outputs: []string{"out"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := recipe.Validate(); err == nil {
				t.Fatal("Validate succeeded")
			}
		})
	}
}

func TestActionRecipeRejectsRemovedSpecializedKinds(t *testing.T) {
	for _, kind := range []string{"objtool", "genksyms", "modpost", "module-link"} {
		t.Run(kind, func(t *testing.T) {
			recipe := ActionRecipe{
				Schema: LinuxKernelPlanSchema,
				Kind:   kind,
				Tool:   "actionfile",
				Arguments: []string{
					"${output:out}",
				},
				Outputs: []string{"out"},
			}
			err := recipe.Validate()
			if err == nil || !strings.Contains(err.Error(), "unsupported kind") {
				t.Fatalf("Validate error = %v, want unsupported kind", err)
			}
		})
	}
}

func TestActionRecipeAcceptsDependencyOnlyTree(t *testing.T) {
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments: []string{"-c", "${source:src}", "-o", "${output:out}"},
		Sources:   []string{"src"}, Outputs: []string{"out"}, Trees: []string{"kernel"},
	}
	if err := recipe.Validate(); err != nil {
		t.Fatalf("dependency-only tree rejected: %v", err)
	}
}

func TestActionRecipeValidatesCompleteCompoundCompilerWorkingInputUses(t *testing.T) {
	base := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: compactKbuildScriptRunnerRole,
		Arguments:        []string{"${output:out}"},
		WorkingDirectory: "compound",
		WorkingInputs: map[string]string{
			"source:source": "scripts/basic/fixdep.c",
			"input:helper":  "scripts/basic/fixdep",
		},
		CompilerInvocation: &ActionRecipeCompilerInvocation{
			Tool:                      "cc",
			Arguments:                 []string{"scripts/basic/fixdep.c", "-o", "scripts/basic/fixdep"},
			WorkingInputUses:          []string{"input:helper", "source:source"},
			WorkingInputUsesComplete:  true,
			AuxiliaryWorkingInputUses: []string{"input:helper"},
		},
		Sources: []string{"source"}, Inputs: []string{"helper"}, Outputs: []string{"out"},
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("Validate rejected binary driver-link projection: %v", err)
	}

	for _, test := range []struct {
		name string
		edit func(*ActionRecipe)
		want string
	}{
		{name: "uses without completeness", edit: func(recipe *ActionRecipe) {
			recipe.CompilerInvocation.WorkingInputUsesComplete = false
		}, want: "without a complete projection"},
		{name: "unstaged use", edit: func(recipe *ActionRecipe) {
			delete(recipe.WorkingInputs, "input:helper")
		}, want: "is not staged"},
		{name: "noncanonical uses", edit: func(recipe *ActionRecipe) {
			recipe.CompilerInvocation.WorkingInputUses = []string{"source:source", "input:helper"}
		}, want: "canonical lexical order"},
		{name: "auxiliary use outside complete set", edit: func(recipe *ActionRecipe) {
			recipe.CompilerInvocation.AuxiliaryWorkingInputUses = []string{"input:missing"}
		}, want: "absent from the complete working input projection"},
		{name: "noncanonical auxiliary uses", edit: func(recipe *ActionRecipe) {
			recipe.CompilerInvocation.AuxiliaryWorkingInputUses = []string{"source:source", "input:helper"}
		}, want: "canonical lexical order"},
		{name: "text output", edit: func(recipe *ActionRecipe) {
			recipe.CompilerInvocation.Arguments = []string{"-E", "scripts/basic/fixdep.c", "-o", "scripts/basic/fixdep.i"}
		}, want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := cloneActionRecipe(base)
			test.edit(&invalid)
			err := invalid.Validate()
			if test.want == "" {
				if err != nil {
					t.Fatalf("Validate rejected existing cc text-output compile: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestActionRecipeAcceptsTypedGeneratedContentSubstitution(t *testing.T) {
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments: []string{"-DSTACK_OFFSET=${content:stack-offset}", "-o", "${output:out}"},
		Inputs:    []string{"offsets"}, Outputs: []string{"out"},
		ContentSubstitutions: map[string]ActionRecipeContentSubstitution{
			"stack-offset": {Input: "input:offsets", Transform: ActionRecipeContentTransformMakeShellWord},
		},
	}
	if err := recipe.Validate(); err != nil {
		t.Fatalf("typed generated content substitution rejected: %v", err)
	}

	for name, mutate := range map[string]func(*ActionRecipe){
		"undeclared input": func(recipe *ActionRecipe) {
			recipe.ContentSubstitutions["stack-offset"] = ActionRecipeContentSubstitution{Input: "input:missing", Transform: ActionRecipeContentTransformMakeShellWord}
		},
		"unknown transform": func(recipe *ActionRecipe) {
			recipe.ContentSubstitutions["stack-offset"] = ActionRecipeContentSubstitution{Input: "input:offsets", Transform: "execute-shell"}
		},
		"unused content": func(recipe *ActionRecipe) {
			recipe.Arguments[0] = "-DSTACK_OFFSET=constant"
		},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := recipe
			invalid.Arguments = slices.Clone(recipe.Arguments)
			invalid.ContentSubstitutions = map[string]ActionRecipeContentSubstitution{}
			for key, value := range recipe.ContentSubstitutions {
				invalid.ContentSubstitutions[key] = value
			}
			mutate(&invalid)
			if err := invalid.Validate(); err == nil {
				t.Fatal("Validate succeeded")
			}
		})
	}
}

func TestActionRecipeValidatesCanonicalArgumentTransforms(t *testing.T) {
	valid := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments: []string{
			"printf generated=${content:value} ${tree:kernel}",
			"second-template",
			"${output:out}",
		},
		ArgumentTransforms: []ActionRecipeArgumentTransform{
			{Index: 0, Transform: ActionRecipeArgumentTransformContentTemplateBase64},
			{Index: 1, Transform: ActionRecipeArgumentTransformContentTemplateBase64},
		},
		Inputs: []string{"query"}, Outputs: []string{"out"}, Trees: []string{"kernel"},
		ContentSubstitutions: map[string]ActionRecipeContentSubstitution{
			"value": {Input: "input:query", Transform: ActionRecipeContentTransformMakeShellWord},
		},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate rejected canonical argument transforms: %v", err)
	}

	for _, test := range []struct {
		name       string
		transforms []ActionRecipeArgumentTransform
		want       string
	}{
		{name: "negative index", transforms: []ActionRecipeArgumentTransform{{Index: -1, Transform: ActionRecipeArgumentTransformContentTemplateBase64}}, want: "out of range"},
		{name: "index at length", transforms: []ActionRecipeArgumentTransform{{Index: len(valid.Arguments), Transform: ActionRecipeArgumentTransformContentTemplateBase64}}, want: "out of range"},
		{name: "duplicate index", transforms: []ActionRecipeArgumentTransform{{Index: 0, Transform: ActionRecipeArgumentTransformContentTemplateBase64}, {Index: 0, Transform: ActionRecipeArgumentTransformContentTemplateBase64}}, want: "repeats argument transform index"},
		{name: "noncanonical order", transforms: []ActionRecipeArgumentTransform{{Index: 1, Transform: ActionRecipeArgumentTransformContentTemplateBase64}, {Index: 0, Transform: ActionRecipeArgumentTransformContentTemplateBase64}}, want: "canonical numeric order"},
		{name: "unsupported transform", transforms: []ActionRecipeArgumentTransform{{Index: 0, Transform: "execute-template"}}, want: "unsupported transform"},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := valid
			invalid.ArgumentTransforms = test.transforms
			err := invalid.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestActionRecipeValidatesContentTemplateShellPlacements(t *testing.T) {
	base := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments: []string{"printf ${content:value}", "${output:out}"},
		ArgumentTransforms: []ActionRecipeArgumentTransform{{
			Index: 0, Transform: ActionRecipeArgumentTransformContentTemplateBase64,
		}},
		Inputs: []string{"query"}, Outputs: []string{"out"},
		ContentSubstitutions: map[string]ActionRecipeContentSubstitution{
			"value": {Input: "input:query", Transform: ActionRecipeContentTransformMakeShellSingleWord},
		},
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("Validate rejected unquoted single-word literal: %v", err)
	}

	for _, test := range []struct {
		name      string
		template  string
		transform string
		want      string
	}{
		{name: "environment value in script", template: "printf ${content:value}", transform: ActionRecipeContentTransformMakeShellValue, want: "environment-only"},
		{name: "single word inside single quote", template: "printf '${content:value}'", transform: ActionRecipeContentTransformMakeShellSingleWord, want: "not unquoted"},
		{name: "single word inside double quote", template: `printf "${content:value}"`, transform: ActionRecipeContentTransformMakeShellSingleWord, want: "not unquoted"},
		{name: "single quoted segment unquoted", template: "printf ${content:value}", transform: ActionRecipeContentTransformMakeShellSingleQuotedSegment, want: "not inside a source single quote"},
		{name: "single quoted segment inside double quote", template: `printf "${content:value}"`, transform: ActionRecipeContentTransformMakeShellSingleQuotedSegment, want: "not inside a source single quote"},
		{name: "command program", template: "${content:value} argument", transform: ActionRecipeContentTransformMakeShellWord, want: "selects a command program"},
		{name: "redirection operand", template: "printf result > ${content:value}", transform: ActionRecipeContentTransformMakeShellWord, want: "redirection operand"},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := base
			invalid.Arguments = []string{test.template, "${output:out}"}
			invalid.ContentSubstitutions = map[string]ActionRecipeContentSubstitution{
				"value": {Input: "input:query", Transform: test.transform},
			}
			err := invalid.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate error = %v, want %q", err, test.want)
			}
		})
	}

	for _, test := range []struct {
		name      string
		template  string
		transform string
	}{
		{name: "shell safe word", template: "printf prefix-${content:value}-suffix", transform: ActionRecipeContentTransformMakeShellWord},
		{name: "single quoted segment", template: "printf 'savedcmd := emit ${content:value}'", transform: ActionRecipeContentTransformMakeShellSingleQuotedSegment},
	} {
		t.Run(test.name, func(t *testing.T) {
			valid := base
			valid.Arguments = []string{test.template, "${output:out}"}
			valid.ContentSubstitutions = map[string]ActionRecipeContentSubstitution{
				"value": {Input: "input:query", Transform: test.transform},
			}
			if err := valid.Validate(); err != nil {
				t.Fatalf("Validate rejected valid placement: %v", err)
			}
		})
	}
}

func TestActionPlanAcceptsGeneratedExecutableBinding(t *testing.T) {
	producerRecipe := ActionRecipe{Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc", Arguments: []string{"-o", "${output:00000000}"}, Outputs: []string{"00000000"}}
	producerRecipeID, err := producerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	producer := ActionPlanNode{Stage: "host", Kind: "generate", Recipe: producerRecipeID, Tool: "cc", Product: "sdk", Outputs: []ActionPlanOutput{{Tree: "host", Path: "bin/helper"}}}
	producer.ID = producer.ContentID()
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "input:helper:00000000",
		Arguments: []string{"${output:00000000}"}, Inputs: []string{"helper:00000000"}, Outputs: []string{"00000000"},
	}
	recipeID, err := recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	node := ActionPlanNode{
		Stage: "target", Kind: "generate", Recipe: recipeID, Tool: "generated", Product: "vmlinux",
		Inputs:  []ActionPlanNodeEdge{{Role: "helper", ProducerID: producer.ID, Slot: 0}},
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "generated.o"}},
	}
	node.ID = node.ContentID()
	plan := &ActionPlan{
		Toolsets: map[string]string{"host": actionPlanTestProbeIdentity, "target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{producerRecipeID: producerRecipe, recipeID: recipe},
		Nodes:    []ActionPlanNode{producer, node},
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("WriteStages rejected generated executable binding: %v", err)
	}
}

func TestActionPlanBindsAuxiliaryToolRolesExplicitly(t *testing.T) {
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "pahole",
		AuxiliaryTools: []string{"objcopy", "target@nm"},
		Environment: map[string]string{
			"LLVM_NM":      "${tool:target@nm}",
			"LLVM_OBJCOPY": "${tool:objcopy}",
		},
		Arguments: []string{"-J", "${input:vmlinux:00000000}", "-o", "${output:00000000}"},
		Inputs:    []string{"vmlinux:00000000"}, Outputs: []string{"00000000"},
	}
	recipeID, err := recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	producerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
		Arguments: []string{"-o", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	producerRecipeID, _ := producerRecipe.ID()
	producer := ActionPlanNode{
		Stage: "target", Kind: "generate", Recipe: producerRecipeID, Tool: "cc", Product: "vmlinux",
		Outputs: []ActionPlanOutput{{Tree: "vmlinux", Path: "input"}},
	}
	producer.ID = producer.ContentID()
	node := ActionPlanNode{
		Stage: "target", Kind: "generate", Recipe: recipeID, Tool: "pahole", Product: "vmlinux",
		AuxiliaryTools: []string{"objcopy", "target@nm"},
		Inputs:         []ActionPlanNodeEdge{{Role: "vmlinux", ProducerID: producer.ID}},
		Outputs:        []ActionPlanOutput{{Tree: "vmlinux", Path: "output"}},
	}
	node.ID = node.ContentID()
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{producerRecipeID: producerRecipe, recipeID: recipe},
		Nodes:    []ActionPlanNode{producer, node},
	}
	outputs := actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))
	if err := plan.WriteStages(outputs); err != nil {
		t.Fatal(err)
	}
	files, err := readActionPlanFilesForTest(outputs["target"])
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		"nodes/target/" + node.ID + "/in/tool/target/objcopy/unscoped",
		"nodes/target/" + node.ID + "/in/tool/target/nm/scoped",
	} {
		if _, ok := files[marker]; !ok {
			t.Fatalf("plan omits identity-bound auxiliary marker %s", marker)
		}
	}

	plan.Nodes[1].AuxiliaryTools = nil
	plan.Nodes[1].ID = plan.Nodes[1].ContentID()
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "mismatch"))); err == nil || !strings.Contains(err.Error(), "bindings do not match recipe") {
		t.Fatalf("mismatched auxiliary tools error = %v", err)
	}
}

func TestEvaluatedKbuildPatternRuleLowersCompileObjtoolAndObjcopy(t *testing.T) {
	const (
		directory = "arch/x86/boot/startup"
		target    = directory + "/gdt_idt.pi.o"
		source    = directory + "/gdt_idt.c"
	)
	fragment := map[string]string{"CONFIG_OBJTOOL": "y"}
	configFragment := fragment
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
cmd_objtool = ; $(objtool) --noabs $@
cmd_cc_o_c = $(CC) -DMEASURED -c -o $@ $< $(cmd_objtool)
cmd_objcopy = $(OBJCOPY) $(OBJCOPYFLAGS) $< $@
$(obj)/%.o: $(src)/%.c
	$(call if_changed,cc_o_c)
$(obj)/%.pi.o: OBJCOPYFLAGS := --prefix-symbols=__pi_
$(obj)/%.pi.o: $(obj)/%.o FORCE
	$(call if_changed,objcopy)
`, map[string]string{
		"CC": KbuildActionRoleToken("target", "cc"), "OBJCOPY": KbuildActionRoleToken("target", "objcopy"),
		"objtool": "__LINUX_BZL_OBJECT_TREE__/tools/objtool/objtool", "obj": directory, "src": directory,
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config: CompactConfig{
			KbuildProfiles:   []CompactKbuildProfile{profile},
			KbuildSelections: []CompactKbuildSelection{{Profile: profile.Name, Target: target, MakeTarget: target, Lifecycle: "target", Scope: "target", Stage: "target"}},
		},
		configFragment: configFragment,
	}
	plan := &ActionPlan{
		Recipes: map[string]ActionRecipe{},
		Sources: []ActionPlanSource{{ID: "src-00000001", Namespace: "kernel", Path: source}},
		Nodes: []ActionPlanNode{{
			ID: strings.Repeat("c", 64), Stage: "host", Kind: "generate", Tool: "cc", Product: "sdk",
			Outputs: []ActionPlanOutput{{Tree: "host", Path: "tools/objtool/objtool"}},
		}},
	}
	if _, err := metadata.appendGeneratedActionPlan(plan); err != nil {
		t.Fatal(err)
	}
	objcopyID, _, ok := planProducerByOutput(plan, "objects", target)
	if !ok {
		t.Fatalf("plan omits selected target %q: %#v", target, plan.Nodes)
	}
	objcopy, ok := compactKbuildPlanNode(plan, objcopyID)
	if !ok || len(objcopy.Inputs) == 0 {
		t.Fatalf("selected objcopy node=%#v, found=%t", objcopy, ok)
	}
	objtoolID, _, ok := planProducerByOutput(plan, "objects", directory+"/gdt_idt.o")
	if !ok {
		t.Fatalf("plan omits canonical objtool prerequisite: %#v", plan.Nodes)
	}
	objtool, ok := compactKbuildPlanNode(plan, objtoolID)
	if !ok || len(objtool.Inputs) == 0 {
		t.Fatalf("objtool node=%#v, found=%t", objtool, ok)
	}
	compile := ActionPlanNode{}
	for _, input := range objtool.Inputs {
		candidate, found := compactKbuildPlanNode(plan, input.ProducerID)
		if found && candidate.Kind == "compile" {
			compile = candidate
			break
		}
	}
	ok = compile.ID != ""
	if !ok || len(compile.Outputs) == 0 || compile.Kind != "compile" ||
		compile.Outputs[0].Path != directory+"/gdt_idt.o" ||
		!strings.HasPrefix(compile.Outputs[0].ArtifactPath, ".linux-bzl-intermediate/") ||
		actionPlanOutputIsCanonical(compile.Outputs[0]) {
		t.Fatalf("compile node=%#v, want private gdt_idt.o", compile)
	}
	if objtool.Kind != "generate" || objtool.Tool != "generated" || objtool.Outputs[0].Path != "arch/x86/boot/startup/gdt_idt.o" ||
		!slices.ContainsFunc(objtool.Inputs, func(input ActionPlanNodeEdge) bool { return input.ProducerID == compile.ID }) {
		t.Fatalf("objtool node=%#v, want compile -> canonical prerequisite", objtool)
	}
	objtoolRecipe := plan.Recipes[objtool.Recipe]
	for _, argument := range []string{"--noabs", "../../../../arch/x86/boot/startup/gdt_idt.o"} {
		if !slices.Contains(objtoolRecipe.Arguments, argument) {
			t.Fatalf("objtool arguments=%q, missing %q", objtoolRecipe.Arguments, argument)
		}
	}
	if objtoolRecipe.WorkingOutputs["00000000"] != "arch/x86/boot/startup/gdt_idt.o" {
		t.Fatalf("objtool working outputs=%#v", objtoolRecipe.WorkingOutputs)
	}
	if objcopy.Kind != "copy" || objcopy.Tool != "objcopy" ||
		objcopy.Outputs[0].Path != target || objcopy.Inputs[0].ProducerID != objtool.ID {
		t.Fatalf("objcopy node=%#v, want objtool -> %s", objcopy, target)
	}
	if got, want := plan.Recipes[objcopy.Recipe].Arguments, []string{
		"--prefix-symbols=__pi_", "${input:object:00000000}", "${output:00000000}",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("objcopy arguments=%q, want %q", got, want)
	}
}

func TestEvaluatedKbuildRulesLowerVDSOGeneratedSourceGraph(t *testing.T) {
	fragment := map[string]string{}
	configFragment := fragment
	directory := "arch/x86/entry/vdso"
	target := directory + "/vdso-image-64.o"
	sources := []string{
		directory + "/vdso-note.S",
		directory + "/vclock_gettime.c",
		directory + "/vgetcpu.c",
		directory + "/vgetrandom.c",
		directory + "/vgetrandom-chacha.S",
		directory + "/vdso.lds.S",
		directory + "/vdso2c.c",
		"arch/x86/include/asm/vdso.h",
	}
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
hostprogs-y += vdso2c
cmd_host-csingle = $(HOSTCC) $(KBUILD_HOSTCFLAGS) -o $@ $<
cmd_cc_o_c = $(CC) $(KBUILD_CFLAGS) -c -o $@ $<
cmd_as_o_S = $(CC) $(KBUILD_AFLAGS) -c -o $@ $<
cmd_cpp_lds_S = $(CC) $(CPPFLAGS_$(@F:.S=)) -E -o $@ $<
cmd_objcopy = $(OBJCOPY) $(OBJCOPYFLAGS) $< $@
cmd_vdso_and_check = $(LD) $(VDSO_LDFLAGS) $(VDSO_LDFLAGS_vdso.lds) -o $@ -T $(word 1,$^) $(filter %.o,$^)
cmd_vdso2c = $(obj)/vdso2c $@ $(word 1,$^) $(word 2,$^)
$(obj)/vdso2c: $(src)/vdso2c.c FORCE
	$(call if_changed,host-csingle)
$(obj)/%.o: $(src)/%.c
	$(call if_changed,cc_o_c)
$(obj)/%.o: $(src)/%.S
	$(call if_changed,as_o_S)
$(obj)/%.lds: $(src)/%.lds.S
	$(call if_changed,cpp_lds_S)
$(obj)/vdso64.so.dbg: $(obj)/vdso.lds $(obj)/vdso-note.o $(obj)/vclock_gettime.o $(obj)/vgetcpu.o $(obj)/vgetrandom.o $(obj)/vgetrandom-chacha.o FORCE
	$(call if_changed,vdso_and_check)
$(obj)/%.so: OBJCOPYFLAGS := -S --remove-section __ex_table
$(obj)/%.so: $(obj)/%.so.dbg FORCE
	$(call if_changed,objcopy)
$(obj)/vdso-image-%.c: $(obj)/vdso%.so.dbg $(obj)/vdso%.so $(obj)/vdso2c FORCE
	$(call if_changed,vdso2c)
$(obj)/vclock_gettime.o: KBUILD_CFLAGS := -std=gnu11 -DBUILD_VDSO -fno-stack-protector
`, map[string]string{
		"CC": KbuildActionRoleToken("target", "cc"), "HOSTCC": KbuildActionRoleToken("host", "cc"), "LD": KbuildActionRoleToken("target", "ld"), "OBJCOPY": KbuildActionRoleToken("target", "objcopy"),
		"obj": directory, "src": directory,
		"KBUILD_CFLAGS": "-std=gnu11 -DBUILD_VDSO", "KBUILD_AFLAGS": "-D__ASSEMBLY__ -DBUILD_VDSO",
		"CPPFLAGS_vdso.lds": "-P -C", "VDSO_LDFLAGS": "-shared --hash-style=both -Bsymbolic",
		"VDSO_LDFLAGS_vdso.lds": "-m elf_x86_64 -soname linux-vdso.so.1",
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, sources...)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildSelections: []CompactKbuildSelection{
				{Profile: profile.Name, Target: directory + "/vdso2c", MakeTarget: directory + "/vdso2c", Lifecycle: "target", Scope: "host", Stage: "host"},
				{Profile: profile.Name, Target: target, MakeTarget: target, Lifecycle: "target", Scope: "target", Stage: "target"},
			},
		},
		configFragment: configFragment,
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if _, err := metadata.appendGeneratedActionPlan(plan); err != nil {
		t.Fatal(err)
	}
	generatedSourceProducer, _, ok := planProducerByOutput(plan, "objects", directory+"/vdso-image-64.c")
	if !ok {
		t.Fatalf("plan omits generated vdso-image-64.c: %#v", plan.Nodes)
	}
	objectProducer, _, ok := planProducerByOutput(plan, "objects", target)
	if !ok {
		t.Fatalf("plan omits final %s: %#v", target, plan.Nodes)
	}
	var final ActionPlanNode
	for _, node := range plan.Nodes {
		if node.ID == objectProducer {
			final = node
			break
		}
	}
	if final.Kind != "compile" || len(final.Inputs) == 0 || final.Inputs[0].ProducerID != generatedSourceProducer {
		t.Fatalf("final generated-source compile=%#v, want input from %s", final, generatedSourceProducer)
	}
	for _, output := range []string{
		directory + "/vdso.lds",
		directory + "/vdso-note.o",
		directory + "/vclock_gettime.o",
		directory + "/vgetcpu.o",
		directory + "/vgetrandom.o",
		directory + "/vgetrandom-chacha.o",
		directory + "/vdso64.so.dbg",
		directory + "/vdso64.so",
	} {
		if _, _, ok := planProducerByOutput(plan, "objects", output); !ok {
			t.Errorf("plan omits dynamically derived prerequisite %q", output)
		}
	}
	var measuredCompile ActionRecipe
	for _, node := range plan.Nodes {
		if node.Outputs[0].Path == directory+"/vclock_gettime.o" {
			measuredCompile = plan.Recipes[node.Recipe]
		}
	}
	if !slices.Contains(measuredCompile.Arguments, "-fno-stack-protector") {
		t.Errorf("vclock_gettime compile arguments=%q, want evaluated target-specific KBUILD_CFLAGS", measuredCompile.Arguments)
	}
	vdso2cProducer, _, ok := planProducerByOutput(plan, "host", directory+"/vdso2c")
	if !ok {
		t.Fatalf("plan omits Kbuild-declared vdso2c host program: %#v", plan.Nodes)
	}
	var generatedSource ActionPlanNode
	for _, node := range plan.Nodes {
		if node.ID == generatedSourceProducer {
			generatedSource = node
			break
		}
	}
	generatedRecipe := plan.Recipes[generatedSource.Recipe]
	if generatedSource.Tool != "generated" || !strings.HasPrefix(generatedRecipe.Tool, "input:program:") ||
		len(generatedRecipe.ExecutableInputs) != 1 {
		t.Fatalf("vdso2c lowering=%#v recipe=%#v, want generated executable input", generatedSource, generatedRecipe)
	}
	if !slices.ContainsFunc(generatedSource.Inputs, func(input ActionPlanNodeEdge) bool {
		return input.ProducerID == vdso2cProducer
	}) {
		t.Fatalf("vdso2c inputs=%#v, want host producer %s", generatedSource.Inputs, vdso2cProducer)
	}
	linkerScriptProducer, _, _ := planProducerByOutput(plan, "objects", directory+"/vdso.lds")
	for _, node := range plan.Nodes {
		if node.ID != linkerScriptProducer {
			continue
		}
		recipe := plan.Recipes[node.Recipe]
		if node.Tool != "cc" || node.Kind != "compile" || !slices.Contains(recipe.Arguments, "-P") ||
			!slices.Contains(recipe.Arguments, "-C") || !slices.Contains(recipe.Arguments, "-E") {
			t.Fatalf("cpp_lds_S lowering=%#v recipe=%#v, want evaluated generic compiler recipe", node, recipe)
		}
	}
}

func TestExpandPlanKbuildFlagRejectsUnresolvedCompilerPolicyVariable(t *testing.T) {
	for _, variable := range []string{"CC_FLAGS_CFI", "CLANG_FLAGS", "RANDSTRUCT_CFLAGS", "cflags-nogcse-example"} {
		if _, err := expandPlanKbuildFlag("$("+variable+")", nil, "kernel/example.o"); err == nil ||
			!strings.Contains(err.Error(), "unknown make variable") {
			t.Fatalf("unresolved %s expansion error = %v, want fail-closed source evaluation", variable, err)
		}
	}
}
