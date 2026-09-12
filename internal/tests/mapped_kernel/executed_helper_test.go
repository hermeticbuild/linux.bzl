package mapped_kernel_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

type helperRecipe struct {
	Tool               string            `json:"tool"`
	Arguments          []string          `json:"arguments"`
	Sources            []string          `json:"sources"`
	ExecutableInputs   []string          `json:"executable_inputs"`
	WorkingInputs      map[string]string `json:"working_inputs"`
	CompilerInvocation *struct {
		Complete  bool     `json:"working_input_uses_complete"`
		Auxiliary []string `json:"auxiliary_working_input_uses"`
	} `json:"compiler_invocation"`
}

func oneHelperPlanPath(t *testing.T, pattern string) string {
	t.Helper()
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) != 1 {
		t.Fatalf("want one plan path matching %q, got %q: %v", pattern, matches, err)
	}
	return matches[0]
}

func readHelperJSON(t *testing.T, filename string, value any) {
	t.Helper()
	content, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, value); err != nil {
		t.Fatalf("read %s: %v", filename, err)
	}
}

func TestExecutedHelperContentFeedsSharedGeneratedHeaderCompile(t *testing.T) {
	// Inspect the actual final shards, not initial snapshots: only final source
	// bindings can establish that the executed helper is no longer a full-config
	// producer identity in smoke.o's compiler action.
	var roots []string
	for _, logical := range strings.Fields(os.Getenv("FAMILY_SMOKE_PLAN")) {
		root, err := runfiles.Rlocation(logical)
		if err != nil {
			t.Fatal(err)
		}
		roots = append(roots, root)
	}
	if len(roots) != 4 {
		t.Fatalf("want four final plan shards, got %q", roots)
	}
	findNode := func(id string) (string, string) {
		var found, shard string
		for _, root := range roots {
			matches, err := filepath.Glob(filepath.Join(root, "nodes", "*", id))
			if err != nil {
				t.Fatal(err)
			}
			for _, match := range matches {
				if found != "" {
					t.Fatalf("node %s occurs in more than one final shard", id)
				}
				found, shard = match, root
			}
		}
		if found == "" {
			t.Fatalf("final plan has no node %s", id)
		}
		return found, shard
	}
	readRecipe := func(node, shard string) helperRecipe {
		id := filepath.Base(oneHelperPlanPath(t, filepath.Join(node, "recipe", "*")))
		var recipe helperRecipe
		readHelperJSON(t, filepath.Join(shard, "recipes", id+".json"), &recipe)
		return recipe
	}
	var report reuseReport
	readHelperJSON(t, runfileFromEnv(t, "FAMILY_SMOKE_REUSE_REPORT"), &report)
	assertGeneratedHeaderCompilerSharing(t, report)
	memberships := map[string]string{}
	for _, node := range report.Nodes {
		memberships[node.NodeID] = strings.Join(node.Memberships, ",")
	}
	helpers := map[string]string{}
	contents := map[string]string{}
	for _, compiler := range report.PreciseCompiles {
		smoke := false
		for _, output := range compiler.OutputDetails {
			smoke = smoke || output.Tree == "objects" && output.LogicalPath == "smoke.o"
		}
		if !smoke {
			continue
		}
		node, shard := findNode(compiler.NodeID)
		recipe := readRecipe(node, shard)
		if recipe.Tool != "scriptrun" || recipe.CompilerInvocation == nil || !recipe.CompilerInvocation.Complete {
			t.Fatalf("smoke.o lost its complete atomic compiler envelope: %#v", recipe)
		}
		binding := ""
		for _, candidate := range recipe.ExecutableInputs {
			if recipe.WorkingInputs["input:"+candidate] == "scripts/basic/fixdep" {
				if binding != "" {
					t.Fatal("smoke.o has repeated helper executable bindings")
				}
				binding = candidate
			}
		}
		if binding == "" {
			t.Fatal("smoke.o has no staged compiled-helper executable")
		}
		auxiliary := false
		for _, use := range recipe.CompilerInvocation.Auxiliary {
			auxiliary = auxiliary || use == "input:"+binding
		}
		if !auxiliary {
			t.Fatal("helper is not a proven auxiliary use of the compiler envelope")
		}
		var inputs struct {
			Bindings map[string]struct {
				Path string `json:"path"`
			} `json:"bindings"`
		}
		readHelperJSON(t, oneHelperPlanPath(t, filepath.Join(node, "in", "bindings", "*.json")), &inputs)
		parts := strings.Split(inputs.Bindings[binding].Path, "/")
		if len(parts) != 3 || parts[0] != "nodes" || len(parts[1]) != 64 || parts[2] != "00000000" {
			t.Fatalf("helper binding has no exact ordinary producer slot: %#v", inputs.Bindings[binding])
		}
		copyID := parts[1]
		copyNode, copyShard := findNode(copyID)
		copyRecipe := readRecipe(copyNode, copyShard)
		if copyRecipe.Tool != "actionfile" ||
			!slices.Equal(copyRecipe.Arguments, []string{"-input", "${source:content:00000000}", "-preserve_mode", "-out", "${output:00000000}"}) ||
			!slices.Equal(copyRecipe.Sources, []string{"content:00000000"}) {
			t.Fatalf("helper is not the exact executable-mode-preserving copy: %#v", copyRecipe)
		}
		sourceID := filepath.Base(oneHelperPlanPath(t, filepath.Join(copyNode, "in", "source", "content", "00000000", "*")))
		contentID := filepath.Base(oneHelperPlanPath(t, filepath.Join(copyShard, "sources", sourceID, "observed-artifacts", "content", "*")))
		if len(contentID) != 64 || memberships[copyID] != "base,irrelevant,relevant" {
			t.Fatalf("equal helper bytes/mode are not shared by all configurations: %s %s %s", copyID, contentID, memberships[copyID])
		}
		membership := strings.Join(compiler.Memberships, ",")
		helpers[membership], contents[membership] = copyID, contentID
	}
	if len(helpers) != 2 || helpers["base,irrelevant"] == "" ||
		helpers["base,irrelevant"] != helpers["relevant"] ||
		contents["base,irrelevant"] != contents["relevant"] {
		t.Fatalf("shared and config-sensitive smoke.o do not consume the same measured helper: helpers=%v contents=%v", helpers, contents)
	}
}
