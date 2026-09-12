package kconfig

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestConfigDependencyLiteralLookaheadCandidates(t *testing.T) {
	for _, test := range []struct {
		name, includes, later string
		want                  []string
	}{
		{"angle", "#include <later.h>\n", "__LATER\n", []string{"__LATER"}},
		{"quote", "#include \"later.h\"\n", "__LATER\n", []string{"__LATER"}},
		{"digraph splice", "%:inc\\\nlude <later.h>\n", "__LATER\n", []string{"__LATER"}},
		{"inactive", "#if 0\n#include <later.h>\n#endif\n", "__LATER\n", []string{"__LATER"}},
		{"computed skipped", "#define NEXT <later.h>\n#include NEXT\n", "__LATER\n", nil},
		{"computed then literal", "#include UNKNOWN\n#include <later.h>\n", "__LATER\n", []string{"__LATER"}},
		{"missing then literal", "#include <missing.h>\n#include <later.h>\n", "__LATER\n", []string{"__LATER"}},
		{"escape then literal", "#include <../../outside.h>\n#include <later.h>\n", "__LATER\n", []string{"__LATER"}},
		{"transitive cycle", "#include <later.h>\n", "__LATER\n#include <deep.h>\n", []string{"__DEEP", "__LATER"}},
		{"conditional operands", "#include <later.h>\n", "#ifndef __LATER_GUARD\n#define __LATER_GUARD\n#if defined(__LATER_FEATURE)\n__LATER\n#endif\n#endif\n", []string{"__LATER", "__LATER_FEATURE", "__LATER_GUARD"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			const source = "drivers/example/driver.c"
			plan, node := compilerGuardObservationPlanForTest(t, map[string]string{
				source:                "#define PICK(x) CONFIG_ ## x\nPICK(PREFIX)\n#include <outer.h>\n",
				"include/outer.h":     "#include <first.h>\n" + test.includes,
				"include/first.h":     "__FIRST\n",
				"include/later.h":     test.later,
				"include/deep.h":      "__DEEP\n#include <later.h>\n",
				"include/unrelated.h": "__UNRELATED\n",
			}, []string{"-nostdinc", "-I${tree:kernel}/include", "-c", source})
			baseline, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil || !baseline.Opaque || !strings.Contains(baseline.Reason, "__FIRST") {
				t.Fatalf("wrong baseline stop: %#v, %v", baseline, err)
			}
			context := newConfigDependencyAnalysisContext(plan)
			for range 2 {
				seen := map[string]bool{}
				plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
					for _, name := range value.Names {
						if name == "__FIRST" {
							continue
						}
						if !value.OptionalTokenHints || !value.LiteralIncludeHints || value.OptionalDefinedness || value.Truncated || len(value.Calls) != 0 || !value.Origin.Source || value.Origin.ContentID == "" {
							t.Errorf("candidate escaped the weak tier or immutable origin: %#v", value)
						}
						seen[name] = true
					}
					return nil
				})
				got, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
				if err != nil || !reflect.DeepEqual(got, baseline) {
					t.Fatalf("lookahead changed proof: %#v != %#v, %v", got, baseline, err)
				}
				if got := slices.Sorted(maps.Keys(seen)); !slices.Equal(got, test.want) {
					t.Fatalf("candidate names = %v, want %v", got, test.want)
				}
			}
		})
	}
}

func TestConfigDependencyLiteralLookaheadSearchOrder(t *testing.T) {
	for _, test := range []struct {
		name, include, environment string
		flags                      []string
		want                       string
	}{
		{"I before system", "<later.h>", "", []string{"-isystem", "${tree:kernel}/system", "-I${tree:kernel}/include"}, "__LOCAL"},
		{"quote parent before I", "\"later.h\"", "", []string{"-I${tree:kernel}/system", "-I${tree:kernel}/include"}, "__LOCAL"},
		{"angle ignores quote root", "<later.h>", "", []string{"-iquote", "${tree:kernel}/quote", "-I${tree:kernel}/include"}, "__LOCAL"},
		{"external shadows system", "<missing-local.h>", "/uninspectable/compiler/root", []string{"-I${tree:kernel}/include", "-isystem", "${tree:kernel}/system"}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			const source = "drivers/example/driver.c"
			arguments := append([]string{"-nostdinc"}, test.flags...)
			arguments = append(arguments, "-c", source)
			plan, node := compilerGuardObservationPlanForTest(t, map[string]string{
				source:                   "#define PICK(x) CONFIG_ ## x\nPICK(PREFIX)\n#include <outer.h>\n",
				"include/outer.h":        "__FIRST\n#include " + test.include + "\n",
				"include/later.h":        "__LOCAL\n",
				"system/later.h":         "__SYSTEM\n",
				"system/missing-local.h": "__SHADOWED\n",
				"quote/later.h":          "__QUOTE\n",
			}, arguments)
			if test.environment != "" {
				recipe := plan.Recipes[node.Recipe]
				recipe.Environment = map[string]string{"CPATH": test.environment}
				plan.Recipes[node.Recipe] = recipe
			}
			baseline, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			wantReason := "__FIRST"
			if test.environment != "" {
				// Unsafe recipe environments fail before the scanner is entered.
				wantReason = "CPATH"
			}
			if err != nil || !baseline.Opaque || !strings.Contains(baseline.Reason, wantReason) {
				t.Fatalf("wrong baseline: %#v, %v", baseline, err)
			}
			seen := map[string]bool{}
			plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
				if value.OptionalTokenHints {
					for _, name := range value.Names {
						if name != "__FIRST" {
							seen[name] = true
						}
					}
				}
				return nil
			})
			got, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil || !reflect.DeepEqual(got, baseline) {
				t.Fatalf("candidate lookup altered real lookup state: %#v, %v", got, err)
			}
			var want []string
			if test.want != "" {
				want = []string{test.want}
			}
			if got := slices.Sorted(maps.Keys(seen)); !slices.Equal(got, want) {
				t.Fatalf("lookup order changed: %v, want %v", got, want)
			}
		})
	}
}

func literalLookaheadScannerForTest(t *testing.T, header string) *configDependencyClosureScanner {
	t.Helper()
	const source = "drivers/example/driver.c"
	plan, _ := compilerGuardObservationPlanForTest(t, map[string]string{
		source:            "__FIRST\n#include <later.h>\n",
		"include/later.h": header,
	}, []string{"-nostdinc", "-I${tree:kernel}/include", "-c", source})
	s := &configDependencyClosureScanner{
		sourceLookup: newConfigDependencySourceLookup(plan.selectionGraph.profiles["config-dependency"]), physicalFiles: newConfigDependencyPhysicalFileCache(),
		collectCompilerGuards: true, callCoverage: &configDependencyCallCoverage{},
		compilerIntrinsicInitialSnapshot: newConfigDependencyMacroSnapshot(),
		includeDirectories:               []configDependencyIncludeDirectory{{logical: "include", source: true}},
		opaqueReason:                     "original failure", sourcePaths: map[string]bool{"original": true},
		objectPaths: map[string]bool{"original-object": true}, headerTrace: &configDependencyHeaderTrace{},
	}
	file, found := s.sourceFile(source)
	if !found {
		t.Fatal("fixture root missing")
	}
	s.conditionalSyntaxForFile(file)
	return s
}

func TestConfigDependencyLiteralLookaheadWarmBudgetAndIsolation(t *testing.T) {
	s := literalLookaheadScannerForTest(t, "__LATER\n")
	entered := maps.Clone(s.compilerGuardFiles)
	var firstWork int
	for pass := range 2 {
		budget := &configDependencyOpenedHeaderHintBudget{}
		var names []string
		s.emitCompilerLiteralIncludeHintLookahead(configDependencyCompilerPredefineProbe{language: "c"}, func(_ configDependencyCompilerPredefineProbe, _ configDependencyScanFile, hints configDependencyCompilerGuardHints, _ string) {
			names = append(names, hints.names...)
		}, budget)
		if !slices.Equal(names, []string{"__LATER"}) || budget.disabled || budget.work == 0 || pass != 0 && budget.work != firstWork {
			t.Fatalf("cache changed budget/admission: names=%v budget=%#v first=%d", names, budget, firstWork)
		}
		firstWork = budget.work
		if s.opaqueReason != "original failure" || !reflect.DeepEqual(entered, s.compilerGuardFiles) ||
			!maps.Equal(s.sourcePaths, map[string]bool{"original": true}) || !maps.Equal(s.objectPaths, map[string]bool{"original-object": true}) ||
			!reflect.DeepEqual(s.headerTrace, &configDependencyHeaderTrace{}) || len(s.resolvedGeneratedFiles) != 0 {
			t.Fatal("lookahead mutated active source or lookup proof")
		}
	}
	budget := &configDependencyOpenedHeaderHintBudget{work: 65535, names: map[string]bool{"__KEPT": true}}
	s.emitCompilerLiteralIncludeHintLookahead(configDependencyCompilerPredefineProbe{language: "c"}, func(configDependencyCompilerPredefineProbe, configDependencyScanFile, configDependencyCompilerGuardHints, string) {
		t.Error("exhausted budget emitted a candidate")
	}, budget)
	if !budget.names["__KEPT"] || budget.work > 65536 {
		t.Fatal("lookahead overflow discarded prior names or exceeded work")
	}
}

func TestConfigDependencyLiteralLookaheadExternalRootShadowsSource(t *testing.T) {
	s := literalLookaheadScannerForTest(t, "__SHADOWED\n")
	// The root is already entered. An uninspectable earlier search directory
	// must prevent lookahead from treating a later local header as its target.
	s.includeDirectories = append([]configDependencyIncludeDirectory{{external: true}}, s.includeDirectories...)
	s.emitCompilerLiteralIncludeHintLookahead(configDependencyCompilerPredefineProbe{language: "c"}, func(configDependencyCompilerPredefineProbe, configDependencyScanFile, configDependencyCompilerGuardHints, string) {
		t.Error("lookahead bypassed an uninspectable search root")
	}, &configDependencyOpenedHeaderHintBudget{})
	if s.opaqueReason != "original failure" {
		t.Fatal("candidate lookup overwrote the real scanner failure")
	}
}

func TestConfigDependencyLiteralLookaheadBoundedReads(t *testing.T) {
	directory := t.TempDir()
	filename := filepath.Join(directory, "header.h")
	if err := os.WriteFile(filename, []byte("__LATER\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, file := range []configDependencyScanFile{
		{physical: filename}, {exactIdentity: "generated", exactContents: "__LATER\n"},
	} {
		for _, maximum := range []int{1, 8} {
			contents, work, ok := readCompilerHintCandidate(file, maximum)
			if ok != (maximum == 8) || work > maximum || ok && string(contents) != "__LATER\n" {
				t.Fatalf("unbounded/incomplete candidate read: bytes=%q work=%d ok=%t", contents, work, ok)
			}
		}
	}
	if _, work, ok := readCompilerHintCandidate(configDependencyScanFile{physical: directory}, 65536); ok || work != 0 {
		t.Fatal("directory admitted as candidate source")
	}
	s := literalLookaheadScannerForTest(t, strings.Repeat(" ", 65536)+"__OVERSIZED\n")
	budget := &configDependencyOpenedHeaderHintBudget{names: map[string]bool{"__KEPT": true}}
	s.emitCompilerLiteralIncludeHintLookahead(configDependencyCompilerPredefineProbe{language: "c"}, func(configDependencyCompilerPredefineProbe, configDependencyScanFile, configDependencyCompilerGuardHints, string) {
		t.Error("oversized candidate retained a prefix")
	}, budget)
	if !budget.names["__KEPT"] || budget.work > 65536 {
		t.Fatal("oversized candidate discarded earlier hints")
	}
}

func TestConfigDependencyLiteralLookaheadTraversalBound(t *testing.T) {
	s := literalLookaheadScannerForTest(t, "")
	s.includeDirectories = []configDependencyIncludeDirectory{{logical: "include"}}
	s.generated = map[string]bool{}
	for index := range 5000 {
		s.generated[fmt.Sprintf("include/g%d.h", index)] = true
	}
	s.generated["include/later.h"] = true
	calls := 0
	s.resolveGeneratedText = func(logical string) (configDependencyGeneratedText, bool) {
		calls++
		index := 0
		if logical != "include/later.h" {
			if _, err := fmt.Sscanf(logical, "include/g%d.h", &index); err != nil {
				t.Fatal(err)
			}
			index++
		}
		return configDependencyGeneratedText{contents: fmt.Sprintf("__CANDIDATE\n#include <g%d.h>\n", index), identity: logical}, true
	}
	budget := &configDependencyOpenedHeaderHintBudget{}
	s.emitCompilerLiteralIncludeHintLookahead(configDependencyCompilerPredefineProbe{language: "c"}, func(configDependencyCompilerPredefineProbe, configDependencyScanFile, configDependencyCompilerGuardHints, string) {
	}, budget)
	if calls < 2 || calls > 4096 || budget.work > 65536 || len(s.generatedText) != 0 || len(s.resolvedGeneratedFiles) != 0 {
		t.Fatalf("unbounded traversal or borrowed generated state: calls=%d work=%d generated=%d resolved=%d", calls, budget.work, len(s.generatedText), len(s.resolvedGeneratedFiles))
	}
}
