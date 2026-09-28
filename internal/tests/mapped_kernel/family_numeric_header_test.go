package mapped_kernel_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

const numericHeaderPath = "include/generated/mapped_numeric_offsets.h"

var numericVariants = []string{"base", "irrelevant", "relevant"}

type numericOutput struct {
	Tree, Path, ArtifactPath, ObservedPath string
}

type numericRecipe struct {
	Tool, Kind         string
	Arguments          []string               `json:"arguments"`
	CompilerInvocation *struct{ Tool string } `json:"compiler_invocation"`
}

func numericReadFile(t *testing.T, filename string, maximum int64) []byte {
	t.Helper()
	info, err := os.Stat(filename)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximum {
		t.Fatalf("numeric fixture input is missing, nonregular or oversized: %s (%v)", filename, err)
	}
	data, err := os.ReadFile(filename)
	if err != nil || int64(len(data)) > maximum {
		t.Fatalf("read bounded numeric fixture input: %v", err)
	}
	return data
}

func numericConfigValue(t *testing.T, variant string) byte {
	t.Helper()
	data := numericReadFile(t, runfileFromEnv(t, "FAMILY_SMOKE_"+strings.ToUpper(variant)+"_CONFIG"), 1<<20)
	set, unset := false, false
	for _, line := range strings.Split(string(data), "\n") {
		set = set || line == "CONFIG_USED_FAMILY_OPTION=y"
		unset = unset || line == "# CONFIG_USED_FAMILY_OPTION is not set"
	}
	if set == unset {
		t.Fatal("numeric fixture lacks one exact selected CONFIG value")
	}
	if set {
		return 1
	}
	return 0
}

func TestFamilyNumericHeaderInitialAnalysisIsOpaque(t *testing.T) {
	files := strings.Fields(os.Getenv("FAMILY_SMOKE_INITIAL_SNAPSHOTS"))
	if len(files) != len(numericVariants) {
		t.Fatal("want exactly three initial, not observed-replay, snapshots")
	}
	seen := map[string]bool{}
	for _, logical := range files {
		filename, err := runfiles.Rlocation(logical)
		if err != nil {
			t.Fatal(err)
		}
		variant := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(filename), "family_smoke."), ".action-plan.json.gz")
		if !slices.Contains(numericVariants, variant) || seen[variant] || filepath.Base(filename) != "family_smoke."+variant+".action-plan.json.gz" {
			t.Fatal("snapshot is not the uniquely named initial variant output")
		}
		seen[variant] = true
		reader, err := gzip.NewReader(bytes.NewReader(numericReadFile(t, filename, 16<<20)))
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(io.LimitReader(reader, (32<<20)+1))
		closeErr := reader.Close()
		if err != nil || closeErr != nil || len(data) > 32<<20 {
			t.Fatal("initial snapshot failed bounded gzip decoding")
		}
		var snapshot struct {
			Nodes []struct {
				ID, Kind, Recipe, Tool string
				Outputs                []numericOutput
			}
			Recipes      map[string]numericRecipe
			Dependencies map[string]struct {
				Opaque bool
				Reason string
			} `json:"config_dependencies"`
		}
		if err := json.Unmarshal(data, &snapshot); err != nil {
			t.Fatal(err)
		}
		consumers, writers, compilers := 0, 0, 0
		for _, node := range snapshot.Nodes {
			for _, output := range node.Outputs {
				if output.ObservedPath != "" {
					continue
				}
				recipe := snapshot.Recipes[node.Recipe]
				switch output.Path {
				case "numeric_smoke.o":
					consumers++
					dependency, found := snapshot.Dependencies[node.ID]
					if node.Kind != "compile" || node.Tool != "cc" || recipe.Tool != "cc" ||
						!slices.Contains(recipe.Arguments, "-U__MAPPED_NUMERIC_ABSENT") || !found || !dependency.Opaque ||
						!strings.Contains(dependency.Reason, "unresolved defined operand __MAPPED_NUMERIC_ABSENT") {
						t.Fatalf("%s initial numeric compile missed the exact wildcard failure: %s", variant, dependency.Reason)
					}
				case numericHeaderPath:
					writers++
					if node.Tool != "actionfile" || recipe.Tool != "actionfile" || !slices.Contains(recipe.Arguments, "-validate_config_independent_macro_header_v1") {
						t.Fatal("numeric writer lacks the actual offsets validation contract")
					}
				case "numeric-offsets.s":
					compilers++
					if node.Kind != "compile" || recipe.Tool != "cc" && (recipe.CompilerInvocation == nil || recipe.CompilerInvocation.Tool != "cc") {
						t.Fatal("numeric offsets lost its selected target compiler")
					}
				}
			}
		}
		if consumers != 1 || writers != 1 || compilers != 1 {
			t.Fatalf("%s has ambiguous/missing numeric consumer, validator or compiler: %d/%d/%d", variant, consumers, writers, compilers)
		}
	}
}

func TestFamilyNumericHeaderAndConsumerAreActuallyBuilt(t *testing.T) {
	headers, objects := map[string][]byte{}, map[string][]byte{}
	record := regexp.MustCompile(`(?m)^[ \t]*\.ascii[ \t]+"->MAPPED_NUMERIC_VALUE[ \t]+[$#]?([0-9]+)[ \t]+used_family_option"`)
	definition := regexp.MustCompile(`(?m)^#define MAPPED_NUMERIC_VALUE ([0-9]+)(?:[ \t]|$)`)
	for _, variant := range numericVariants {
		root := runfileFromEnv(t, "FAMILY_SMOKE_"+strings.ToUpper(variant)+"_OBJECTS")
		assembly := numericReadFile(t, filepath.Join(root, "numeric-offsets.s"), 1<<20)
		headers[variant] = numericReadFile(t, filepath.Join(root, numericHeaderPath), 1<<20)
		objects[variant] = numericReadFile(t, filepath.Join(root, "numeric_smoke.o"), 1<<20)
		asmMatches, headerMatches := record.FindAllSubmatch(assembly, -1), definition.FindAllSubmatch(headers[variant], -1)
		value := numericConfigValue(t, variant)
		want := strconv.Itoa(17 + int(value))
		if len(asmMatches) != 1 || len(headerMatches) != 1 || string(asmMatches[0][1]) != want || string(headerMatches[0][1]) != want {
			t.Fatalf("%s assembly/header do not contain the exact generated numeric value %s", variant, want)
		}
		for symbol, want := range map[string]byte{"mapped_numeric_header_value": 17 + value, "mapped_numeric_absence_value": 1, "mapped_numeric_pasted_value": 1 + value} {
			if got := compiledObjectSymbolByte(t, objects[variant], symbol); got != want {
				t.Fatalf("%s actual %s byte=%d, want %d", variant, symbol, got, want)
			}
		}
	}
	if !bytes.Equal(headers["base"], headers["irrelevant"]) || !bytes.Equal(objects["base"], objects["irrelevant"]) || bytes.Equal(headers["base"], headers["relevant"]) || bytes.Equal(objects["base"], objects["relevant"]) {
		t.Fatal("numeric header/object bytes lost irrelevant reuse or relevant CONFIG invalidation")
	}
}

func TestFamilyExecutedNumericHeaderRecoversPreciseSharing(t *testing.T) {
	var report reuseReport
	readHelperJSON(t, runfileFromEnv(t, "FAMILY_SMOKE_REUSE_REPORT"), &report)
	assertCompilerOutputSharing(t, report, "numeric_smoke.o")
}

func numericFinalPlanRoots(t *testing.T) []string {
	t.Helper()
	var roots []string
	for _, logical := range strings.Fields(os.Getenv("FAMILY_SMOKE_PLAN")) {
		root, err := runfiles.Rlocation(logical)
		if err != nil {
			t.Fatal(err)
		}
		roots = append(roots, root)
	}
	if len(roots) != 4 {
		t.Fatal("numeric fixture requires all four final plan shards")
	}
	return roots
}

func numericFinalNode(t *testing.T, roots []string, id string) (string, string) {
	t.Helper()
	var found, shard string
	for _, root := range roots {
		matches, err := filepath.Glob(filepath.Join(root, "nodes", "*", id))
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range matches {
			if found != "" {
				t.Fatal("numeric final node occurs in multiple shards")
			}
			found, shard = candidate, root
		}
	}
	if found == "" {
		t.Fatal("numeric final node is absent")
	}
	return found, shard
}

func numericFinalView(t *testing.T, roots []string, variant, logical string) (string, string, string) {
	t.Helper()
	var match, shard string
	for _, root := range roots {
		matches, err := filepath.Glob(filepath.Join(root, "variants", variant, "view", "objects", "from", "*", "*", "at", logical))
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range matches {
			if match != "" {
				t.Fatal("numeric public view has multiple exact writers")
			}
			match, shard = candidate, root
		}
	}
	if match == "" {
		t.Fatal("numeric public view has no exact writer")
	}
	relative, err := filepath.Rel(shard, match)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	id, slot := parts[5], parts[6]
	node, ownerShard := numericFinalNode(t, roots, id)
	if _, err := os.Stat(filepath.Join(node, "out", "objects", slot, "at", logical)); err != nil {
		t.Fatal("numeric public view does not match its exact final output slot")
	}
	return node, ownerShard, slot
}

func numericFinalRecipe(t *testing.T, node, shard string) numericRecipe {
	t.Helper()
	id := filepath.Base(oneHelperPlanPath(t, filepath.Join(node, "recipe", "*")))
	var recipe numericRecipe
	readHelperJSON(t, filepath.Join(shard, "recipes", id+".json"), &recipe)
	return recipe
}

func numericObservedContentID(t *testing.T, variant string) string {
	t.Helper()
	filename := filepath.Join(runfileFromEnv(t, "FAMILY_SMOKE_"+strings.ToUpper(variant)+"_OBJECTS"), numericHeaderPath)
	data := numericReadFile(t, filename, 1<<20)
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	_, _ = io.WriteString(hash, "linux-kernel-observed-header-content-v1\x00"+strconv.FormatUint(uint64(info.Mode().Perm()&0111), 8)+"\x00")
	_, _ = hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}

func numericRequireObservedInput(t *testing.T, node, shard, expected string) {
	t.Helper()
	root := filepath.Base(oneHelperPlanPath(t, filepath.Join(node, "in", "input-set", "*")))
	pending, seen, matches := []string{root}, map[string]bool{}, 0
	bytesRead := 0
	for len(pending) != 0 {
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[id] {
			continue
		}
		if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" || len(seen) >= 4096 {
			t.Fatal("numeric input-set closure is malformed or oversized")
		}
		seen[id] = true
		var set struct {
			Children []struct{ ID string }
			Entries  []struct {
				Target     struct{ Kind, Tree, Path string }
				SourceID   string `json:"source_id"`
				ProducerID string `json:"producer_id"`
			}
		}
		data := numericReadFile(t, filepath.Join(shard, "input-sets", id, "manifest", id+".json"), 1<<20)
		bytesRead += len(data)
		if bytesRead > 16<<20 {
			t.Fatal("numeric input-set manifests exceed the aggregate fixture bound")
		}
		if err := json.Unmarshal(data, &set); err != nil {
			t.Fatal(err)
		}
		for _, child := range set.Children {
			pending = append(pending, child.ID)
		}
		for _, entry := range set.Entries {
			if entry.Target.Path != numericHeaderPath {
				continue
			}
			if entry.Target.Kind != "work" && entry.Target.Kind != "tree" {
				continue
			}
			matches++
			if entry.ProducerID != "" || entry.SourceID == "" || filepath.Base(entry.SourceID) != entry.SourceID {
				t.Fatal("precise numeric compile retained a full-config header producer")
			}
			if _, err := os.Stat(filepath.Join(shard, "sources", entry.SourceID, "observed-headers", "content", expected)); err != nil {
				t.Fatal("numeric compile lacks the exact observed bytes/mode source identity")
			}
		}
	}
	if matches == 0 {
		t.Fatal("numeric compile has no observed work/tree header binding")
	}
	var direct struct {
		Bindings map[string]struct {
			ProjectionPath string `json:"projection_path"`
		}
	}
	readHelperJSON(t, oneHelperPlanPath(t, filepath.Join(node, "in", "bindings", "*.json")), &direct)
	for _, binding := range direct.Bindings {
		if binding.ProjectionPath == numericHeaderPath {
			t.Fatal("numeric header still has a direct full-config producer edge")
		}
	}
}

func TestFamilyNumericHeaderRetainsVisibleProvenance(t *testing.T) {
	roots := numericFinalPlanRoots(t)
	contents := map[string]string{}
	for _, variant := range numericVariants {
		writer, shard, _ := numericFinalView(t, roots, variant, numericHeaderPath)
		recipe := numericFinalRecipe(t, writer, shard)
		if recipe.Tool != "actionfile" || !slices.Contains(recipe.Arguments, "-validate_config_independent_macro_header_v1") {
			t.Fatal("final numeric view lost its real validation writer")
		}
		compiler, compilerShard, _ := numericFinalView(t, roots, variant, "numeric-offsets.s")
		compilerRecipe := numericFinalRecipe(t, compiler, compilerShard)
		if compilerRecipe.Tool != "cc" && (compilerRecipe.CompilerInvocation == nil || compilerRecipe.CompilerInvocation.Tool != "cc") {
			t.Fatal("final numeric offsets writer is not an actual target compiler")
		}
		if _, err := os.Stat(filepath.Join(compiler, "kind", "compile")); err != nil {
			t.Fatal("numeric offsets producer lost its typed compile kind")
		}
		contents[variant] = numericObservedContentID(t, variant)
	}
	if contents["base"] != contents["irrelevant"] || contents["base"] == contents["relevant"] {
		t.Fatal("observed numeric content identity lost exact CONFIG invalidation")
	}
	var report reuseReport
	readHelperJSON(t, runfileFromEnv(t, "FAMILY_SMOKE_REUSE_REPORT"), &report)
	assertCompilerOutputSharing(t, report, "numeric_smoke.o")
	matched := 0
	for _, compile := range report.PreciseCompiles {
		for _, output := range compile.OutputDetails {
			if output.Tree != "objects" || output.LogicalPath != "numeric_smoke.o" {
				continue
			}
			matched++
			node, shard := numericFinalNode(t, roots, compile.NodeID)
			recipe := numericFinalRecipe(t, node, shard)
			if recipe.Tool != "cc" || !slices.Contains(recipe.Arguments, "-U__MAPPED_NUMERIC_ABSENT") {
				t.Fatal("precise numeric compile lost its source-owned initial absence")
			}
			for _, variant := range compile.Memberships {
				numericRequireObservedInput(t, node, shard, contents[variant])
			}
		}
	}
	if matched != 2 {
		t.Fatal("numeric provenance did not cover both precise physical compiles")
	}
}

// assertOnlyPinnedNumericProducersUnshared accounts for the fixture's one
// intentionally initial-cut compiler per variant. It authenticates the exact
// public-view producer IDs before allowing any aggregate sharing difference;
// another unshared compiler or archive is never an interchangeable exception.
func assertOnlyPinnedNumericProducersUnshared(t *testing.T, report reuseReport, pair reusePair) {
	t.Helper()
	if pair.Left != "base" || pair.Right != "irrelevant" {
		t.Fatal("numeric accounting requires the exact base/irrelevant pair")
	}
	byID := map[string]reuseNode{}
	for _, node := range report.Nodes {
		if _, duplicate := byID[node.NodeID]; node.NodeID == "" || duplicate {
			t.Fatal("reuse report contains an empty or repeated node identity")
		}
		byID[node.NodeID] = node
	}
	roots := numericFinalPlanRoots(t)
	producers, producerVariants := map[string]string{}, map[string]string{}
	for _, variant := range numericVariants {
		path, shard, _ := numericFinalView(t, roots, variant, "numeric-offsets.s")
		id := filepath.Base(path)
		if previous := producerVariants[id]; previous != "" {
			t.Fatalf("numeric cut producer unexpectedly shared by %s and %s", previous, variant)
		}
		producerVariants[id], producers[variant] = variant, id
		node, found := byID[id]
		if !found || node.Kind != "compile" || !node.TypedCompiler || node.PreciseCompile ||
			!slices.Equal(node.Memberships, []string{variant}) {
			t.Fatalf("%s exact numeric producer is not a singleton typed, non-precise compile", variant)
		}
		if _, err := os.Stat(filepath.Join(path, "kind", "compile")); err != nil {
			t.Fatal("numeric public-view producer lacks its final typed compile kind")
		}
		recipe := numericFinalRecipe(t, path, shard)
		if recipe.Tool != "cc" && (recipe.CompilerInvocation == nil || recipe.CompilerInvocation.Tool != "cc") {
			t.Fatal("numeric public-view producer is not the selected target compiler")
		}
		for _, precise := range report.PreciseCompiles {
			if precise.NodeID == id {
				t.Fatal("initial-cut numeric producer incorrectly appears in precise compiles")
			}
		}
	}
	left, right := map[string]bool{}, map[string]bool{}
	preciseLeft, preciseRight, preciseShared := 0, 0, 0
	for _, node := range report.Nodes {
		hasLeft, hasRight := slices.Contains(node.Memberships, pair.Left), slices.Contains(node.Memberships, pair.Right)
		if node.PreciseCompile && (hasLeft || hasRight) {
			if node.Kind != "compile" || !node.TypedCompiler {
				t.Fatal("precise pair membership is not an actual typed compiler")
			}
			if hasLeft {
				preciseLeft++
			}
			if hasRight {
				preciseRight++
			}
			if hasLeft && hasRight {
				preciseShared++
			}
		}
		// Match the report's raw eligible denominator, including archives.
		if node.Kind == "compile" || node.Kind == "archive" {
			if hasLeft {
				left[node.NodeID] = true
			}
			if hasRight {
				right[node.NodeID] = true
			}
		}
	}
	shared, leftOnly, rightOnly := 0, 0, 0
	for id := range left {
		if right[id] {
			shared++
		} else {
			leftOnly++
			if id != producers[pair.Left] {
				t.Fatal("an unshared base compiler/archive is not the exact numeric cut producer")
			}
		}
	}
	for id := range right {
		if !left[id] {
			rightOnly++
			if id != producers[pair.Right] {
				t.Fatal("an unshared irrelevant compiler/archive is not the exact numeric cut producer")
			}
		}
	}
	if leftOnly != 1 || rightOnly != 1 || len(left) != preciseLeft+1 || len(right) != preciseRight+1 ||
		shared != len(left)-1 || shared != len(right)-1 || shared == 0 ||
		pair.EligibleLeftNodes != len(left) || pair.EligibleRightNodes != len(right) || pair.EligibleSharedNodes != shared ||
		pair.PreciseLeftCompileNodes != preciseLeft || pair.PreciseRightCompileNodes != preciseRight ||
		pair.PreciseSharedCompileNodes != preciseShared {
		t.Fatalf("raw reuse counts do not equal precise compiles plus the exact numeric cut producers: %#v", pair)
	}
	if pair.EligibleLeftReuseBasisPoints != shared*10000/len(left) ||
		pair.EligibleRightReuseBasisPoints != shared*10000/len(right) {
		t.Fatalf("raw eligible reuse basis points disagree with exact node memberships: %#v", pair)
	}
}
