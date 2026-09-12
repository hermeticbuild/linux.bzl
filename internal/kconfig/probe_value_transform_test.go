package kconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Retain the old per-word implementation as an independent equivalence and
// benchmark oracle. Production normalizes a complete request with one snapshot.
func canonicalSourceCommandPerWordForTest(sourceRoot, command string) string {
	if sourceRoot == "" || !strings.ContainsRune(command, '/') {
		return command
	}
	roots := []string{sourceRoot}
	if absolute, err := filepath.Abs(sourceRoot); err == nil {
		roots = append(roots, absolute)
		if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
			roots = append(roots, resolved)
		}
	}
	seen := map[string]bool{}
	for len(roots) != 0 {
		longest := 0
		for index := 1; index < len(roots); index++ {
			if len(roots[index]) > len(roots[longest]) {
				longest = index
			}
		}
		root := filepath.ToSlash(filepath.Clean(roots[longest]))
		roots = append(roots[:longest], roots[longest+1:]...)
		if root == "" || root == "." || root == "/" || seen[root] {
			continue
		}
		seen[root] = true
		command = strings.ReplaceAll(command, root+"/", "__LINUX_BZL_SOURCE_TREE__/")
	}
	return command
}

func TestProbeSourceNormalizerMatchesPerWordOracle(t *testing.T) {
	directory := t.TempDir()
	physical, alias := filepath.Join(directory, "linux"), filepath.Join(directory, "linux", "alias")
	if err := os.Mkdir(physical, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"", ".", "/", "relative/missing/root", directory + "/linux/../missing", physical, alias} {
		normalizer := (&LinuxProbeEvaluator{sourceRoot: root}).sourceNormalizer()
		for _, value := range []string{"", "PLAIN", "-DVALUE=1", "$" + "{result:00000000.text}", root, root + "/include", "-I" + root + "/include", alias + "/header.h", physical + "/header.h", root + "-sibling/include", "/unrelated/header.h"} {
			if got, want := normalizer.command(value), canonicalSourceCommandPerWordForTest(root, value); got != want {
				t.Errorf("root=%q value=%q: got %q, want %q", root, value, got, want)
			}
		}
	}
}

func TestProbeSourceNormalizerCapturesRootsOnceAndLazily(t *testing.T) {
	directory := t.TempDir()
	first, second, alias := filepath.Join(directory, "first"), filepath.Join(directory, "second"), filepath.Join(directory, "alias")
	for _, root := range []string{first, second} {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(first, alias); err != nil {
		t.Fatal(err)
	}
	evaluator := &LinuxProbeEvaluator{sourceRoot: alias}
	normalizer := evaluator.sourceNormalizer()
	for _, value := range []string{"", "PLAIN", "-DVALUE=1"} {
		if normalizer.command(value) != value || normalizer.rootsReady {
			t.Fatal("non-path operand resolved roots or changed bytes")
		}
	}
	const want = "__LINUX_BZL_SOURCE_TREE__/include"
	if got := normalizer.command(first + "/include"); got != want || !normalizer.rootsReady {
		t.Fatalf("initial root capture = %q, ready=%t", got, normalizer.rootsReady)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, alias); err != nil {
		t.Fatal(err)
	}
	if got := normalizer.command(first + "/include"); got != want {
		t.Fatalf("request snapshot re-resolved its source alias: %q", got)
	}
	if got := normalizer.command(second + "/include"); got != second+"/include" {
		t.Fatalf("request snapshot adopted a new physical root: %q", got)
	}
	fresh := evaluator.sourceNormalizer()
	if got := fresh.command(second + "/include"); got != want {
		t.Fatalf("fresh request retained an earlier root snapshot: %q", got)
	}
}

func TestProbeSourceNormalizerPreservesRecursiveRequestShape(t *testing.T) {
	root := filepath.ToSlash(t.TempDir())
	requestForRoot := func(prefix string) ProbeRequest {
		fragment := ProbeValueFragment{
			Value:     prefix + "/literal",
			When:      &ProbePredicate{Operator: "and", Operands: []ProbePredicate{{Value: prefix + "/predicate"}}},
			Fragments: []ProbeValueFragment{{Value: prefix + "/nested"}},
			Transforms: []ProbeValueTransform{{
				Function: "subst", Arguments: []string{prefix + "/transform"},
				ArgumentFragments: []ProbeValueTransformArgumentFragments{{Index: 0, Fragments: []ProbeValueFragment{{Value: prefix + "/dynamic"}}}},
			}},
		}
		return ProbeRequest{
			Scratch: []ProbeScratch{{Content: prefix + "/scratch"}, {Content: root + "/opaque", ContentIsOpaque: true}},
			Steps: []ProbeStep{{
				WorkingDirectory: prefix + "/cwd", Arguments: []string{prefix + "/arg", "PLAIN"},
				ConditionalArguments: []ProbeConditionalArguments{{Arguments: []string{prefix + "/conditional"}}},
				ArgumentFragments:    []ProbeArgumentFragments{{Index: 0, Fragments: []ProbeValueFragment{fragment}}},
				Environment:          map[string]string{"INCLUDE": prefix + "/environment"},
				EnvironmentFragments: []ProbeEnvironmentFragments{{Name: "FRAG", Fragments: []ProbeValueFragment{fragment}}},
				Stdin:                prefix + "/stdin", StdinFragments: []ProbeValueFragment{fragment},
			}},
			Outcome: ProbeOutcome{Fragments: []ProbeValueFragment{fragment}},
		}
	}
	request := requestForRoot(root)
	got := (&LinuxProbeEvaluator{sourceRoot: root}).canonicalSourceRequest(request)
	if want := requestForRoot("__LINUX_BZL_SOURCE_TREE__"); !reflect.DeepEqual(got, want) {
		t.Fatalf("recursive canonical request changed shape: got %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(request, requestForRoot(root)) {
		t.Fatal("normalization mutated the caller-owned request")
	}
}

func BenchmarkProbeSourceNormalizationBatch(b *testing.B) {
	root := b.TempDir()
	arguments := make([]string, 128)
	for index := range arguments {
		arguments[index] = fmt.Sprintf("-I%s/include/group-%03d", root, index)
	}
	for _, perWord := range []bool{true, false} {
		name := "request-snapshot"
		if perWord {
			name = "previous-per-word"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				normalizer := (&LinuxProbeEvaluator{sourceRoot: root}).sourceNormalizer()
				for _, argument := range arguments {
					var result string
					if perWord {
						result = canonicalSourceCommandPerWordForTest(root, argument)
					} else {
						result = normalizer.command(argument)
					}
					if !strings.HasPrefix(result, "-I__LINUX_BZL_SOURCE_TREE__/include/") {
						b.Fatal(result)
					}
				}
			}
		})
	}
}

func TestCanonicalSourceCommandSourceAndUnrelatedOperands(t *testing.T) {
	directory := t.TempDir()
	root := filepath.Join(directory, "linux")
	alias := filepath.Join(directory, "alias")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	evaluator := &LinuxProbeEvaluator{sourceRoot: alias}
	for _, test := range []struct{ input, want string }{
		{"", ""},
		{"__KERNEL__", "__KERNEL__"},
		{"-DNAME=value", "-DNAME=value"},
		{"${result:00000000.text}", "${result:00000000.text}"},
		{alias, alias},
		{alias + "/include", "__LINUX_BZL_SOURCE_TREE__/include"},
		{root + "/include", "__LINUX_BZL_SOURCE_TREE__/include"},
		{"-I" + root + "/include", "-I__LINUX_BZL_SOURCE_TREE__/include"},
		{alias + "-sibling/include", alias + "-sibling/include"},
		{"/unrelated/include", "/unrelated/include"},
	} {
		if got := evaluator.canonicalSourceCommand(test.input); got != test.want {
			t.Errorf("canonicalSourceCommand(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func BenchmarkCanonicalSourceCommandUnrelatedOperand(b *testing.B) {
	evaluator := &LinuxProbeEvaluator{sourceRoot: b.TempDir()}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if got := evaluator.canonicalSourceCommand("__KERNEL__"); got != "__KERNEL__" {
			b.Fatal(got)
		}
	}
}

func TestApplyProbeValueTransformSupportsPureMakeFunctionChains(t *testing.T) {
	tests := []struct {
		name      string
		transform ProbeValueTransform
		input     string
		want      string
	}{
		{"subst", ProbeValueTransform{Function: "subst", Arguments: []string{"a", "x", ""}, InputArgument: 2}, "a ba", "x bx"},
		{"addprefix", ProbeValueTransform{Function: "addprefix", Arguments: []string{"pre", ""}, InputArgument: 1}, "a b", "prea preb"},
		{"addsuffix", ProbeValueTransform{Function: "addsuffix", Arguments: []string{".o", ""}, InputArgument: 1}, "a b", "a.o b.o"},
		{"dir", ProbeValueTransform{Function: "dir", Arguments: []string{""}, InputArgument: 0}, "rust/core.o generated", "rust/ ./"},
		{"filter", ProbeValueTransform{Function: "filter", Arguments: []string{"%.c", ""}, InputArgument: 1}, "a.c b.o", "a.c"},
		{"filter-out", ProbeValueTransform{Function: "filter-out", Arguments: []string{"%.c", ""}, InputArgument: 1}, "a.c b.o", "b.o"},
		{"dynamic-filter-pattern", ProbeValueTransform{Function: "filter", Arguments: []string{"", "a.c b.o"}, InputArgument: 0}, "%.c", "a.c"},
		{"findstring", ProbeValueTransform{Function: "findstring", Arguments: []string{"-pg", ""}, InputArgument: 1}, "-Wall -pg -O2", "-pg"},
		{"patsubst", ProbeValueTransform{Function: "patsubst", Arguments: []string{"%.c", "%.o", ""}, InputArgument: 2}, "a.c b.h", "a.o b.h"},
		{"patsubst-first-percent", ProbeValueTransform{Function: "patsubst", Arguments: []string{"%", "x%y%", ""}, InputArgument: 2}, "a", "xay%"},
		{"patsubst-drops-empty", ProbeValueTransform{Function: "patsubst", Arguments: []string{"%.c", "", ""}, InputArgument: 2}, "a.c b.c z", "z"},
		{"sort", ProbeValueTransform{Function: "sort", Arguments: []string{""}, InputArgument: 0}, "b a b", "a b"},
		{"strip", ProbeValueTransform{Function: "strip", Arguments: []string{""}, InputArgument: 0}, "  a\t b  ", "a b"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ApplyProbeValueTransform(test.transform, test.input)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("ApplyProbeValueTransform() = %q, want %q", got, test.want)
			}
		})
	}

	value := " -Wall   -DREMOVE -g "
	chain := []ProbeValueTransform{
		{Function: "filter-out", Arguments: []string{"-DREMOVE", ""}, InputArgument: 1},
		{Function: "filter-out", Arguments: []string{"-DOTHER", ""}, InputArgument: 1},
		{Function: "strip", Arguments: []string{""}, InputArgument: 0},
	}
	var err error
	for _, transform := range chain {
		value, err = ApplyProbeValueTransform(transform, value)
		if err != nil {
			t.Fatal(err)
		}
	}
	if value != "-Wall -g" {
		t.Fatalf("filter/strip chain = %q, want %q", value, "-Wall -g")
	}
	_, err = ApplyProbeValueTransform(ProbeValueTransform{
		Function: "filter-out", Arguments: []string{"", ""}, InputArgument: 1,
		ArgumentFragments: []ProbeValueTransformArgumentFragments{{Index: 0, Fragments: []ProbeValueFragment{{Value: "-fdrop"}}}},
	}, "-fdrop -fkeep")
	if err == nil || !strings.Contains(err.Error(), "unresolved dynamic arguments") {
		t.Fatalf("unrendered dynamic transform error = %v", err)
	}
}

func TestCanonicalSourceRequestDeepCopiesValueTransformArguments(t *testing.T) {
	root := filepath.Join(t.TempDir(), "linux")
	evaluator := &LinuxProbeEvaluator{sourceRoot: root}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: 1,
		Steps: []ProbeStep{{
			Name: "consume", Tool: "cc", Arguments: []string{""},
			ArgumentFragments: []ProbeArgumentFragments{{
				Index: 0, Fragments: []ProbeValueFragment{{
					Fragments: []ProbeValueFragment{{Value: "${result:00000000.text}"}},
					Transforms: []ProbeValueTransform{{
						Function: "filter-out", Arguments: []string{filepath.Join(root, "include") + "/%", ""}, InputArgument: 1,
					}, {
						Function: "filter-out", Arguments: []string{"", ""}, InputArgument: 1,
						ArgumentFragments: []ProbeValueTransformArgumentFragments{{
							Index: 0, Fragments: []ProbeValueFragment{{Value: filepath.Join(root, "generated") + "/%"}},
						}},
					}},
				}},
			}},
		}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: "consume"}},
	}
	canonical := evaluator.canonicalSourceRequest(request)
	got := canonical.Steps[0].ArgumentFragments[0].Fragments[0].Transforms[0].Arguments[0]
	if got != "__LINUX_BZL_SOURCE_TREE__/include/%" {
		t.Fatalf("canonical transform argument = %q", got)
	}
	original := request.Steps[0].ArgumentFragments[0].Fragments[0].Transforms[0].Arguments[0]
	if !strings.Contains(original, filepath.ToSlash(root)) {
		t.Fatalf("canonicalization mutated original transform argument: %q", original)
	}
	canonical.Steps[0].ArgumentFragments[0].Fragments[0].Fragments[0].Value = "mutated"
	if got := request.Steps[0].ArgumentFragments[0].Fragments[0].Fragments[0].Value; got != "${result:00000000.text}" {
		t.Fatalf("canonicalization retained nested fragment alias: %q", got)
	}
	dynamic := canonical.Steps[0].ArgumentFragments[0].Fragments[0].Transforms[1].ArgumentFragments[0].Fragments[0].Value
	if dynamic != "__LINUX_BZL_SOURCE_TREE__/generated/%" {
		t.Fatalf("canonical dynamic transform argument = %q", dynamic)
	}
	canonical.Steps[0].ArgumentFragments[0].Fragments[0].Transforms[1].ArgumentFragments[0].Fragments[0].Value = "mutated"
	originalDynamic := request.Steps[0].ArgumentFragments[0].Fragments[0].Transforms[1].ArgumentFragments[0].Fragments[0].Value
	if !strings.Contains(originalDynamic, filepath.ToSlash(root)) {
		t.Fatalf("canonicalization retained dynamic transform argument alias: %q", originalDynamic)
	}
}

func TestCanonicalSourceRequestPreservesOpaqueScratchContent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "linux")
	evaluator := &LinuxProbeEvaluator{sourceRoot: root}
	physical := filepath.ToSlash(filepath.Join(root, "literal-config-value"))
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema,
		Scratch: []ProbeScratch{
			{Name: "command", Kind: "file", Content: physical},
			{Name: "config", Kind: "file", Content: physical, ContentIsOpaque: true},
		},
		Steps: []ProbeStep{{Name: "consume", Tool: "cc"}},
		Outcome: ProbeOutcome{
			Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: "consume"},
		},
	}
	canonical := evaluator.canonicalSourceRequest(request)
	if got, want := canonical.Scratch[0].Content, "__LINUX_BZL_SOURCE_TREE__/literal-config-value"; got != want {
		t.Fatalf("canonical command scratch = %q, want %q", got, want)
	}
	if got := canonical.Scratch[1].Content; got != physical {
		t.Fatalf("opaque config scratch = %q, want exact bytes %q", got, physical)
	}
	if got := request.Scratch[0].Content; got != physical {
		t.Fatalf("canonicalization mutated original scratch content: %q", got)
	}
}

func TestApplyProbeValueTransformBoundsWordsWorkAndOutput(t *testing.T) {
	tests := []struct {
		name      string
		transform ProbeValueTransform
		input     string
		want      string
	}{
		{
			name:      "word count",
			transform: ProbeValueTransform{Function: "strip", Arguments: []string{""}, InputArgument: 0},
			input:     strings.Repeat("x ", MaxProbeDynamicArgumentWords+1),
			want:      "Make words",
		},
		{
			name:      "filter comparisons",
			transform: ProbeValueTransform{Function: "filter", Arguments: []string{strings.Repeat("x ", 2048), ""}, InputArgument: 1},
			input:     strings.Repeat("x ", 2048),
			want:      "pattern comparisons",
		},
		{
			name:      "expanding output",
			transform: ProbeValueTransform{Function: "addprefix", Arguments: []string{strings.Repeat("p", 32), ""}, InputArgument: 1},
			input:     strings.Repeat("x ", 40000),
			want:      "result may exceed",
		},
		{
			name:      "empty subst search",
			transform: ProbeValueTransform{Function: "subst", Arguments: []string{"", "x", ""}, InputArgument: 2},
			input:     "abc",
			want:      "nonempty search string",
		},
		{
			name:      "unsupported pure function",
			transform: ProbeValueTransform{Function: "intcmp", Arguments: []string{"", "2"}, InputArgument: 0},
			input:     "1",
			want:      "proven probe value transform subset",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ApplyProbeValueTransform(test.transform, test.input)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ApplyProbeValueTransform() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestPureKbuildMakeFunctionMatchesGNUEmptyAndPercentEdges(t *testing.T) {
	tests := []struct {
		function  string
		arguments []string
		want      string
	}{
		{"subst", []string{"", "x", "abc"}, "abcx"},
		{"patsubst", []string{"%", "x%y%", "a"}, "xay%"},
		{"patsubst", []string{"%.c", "", "a.c b.c z"}, "z"},
		{"suffix", []string{"a.c b"}, ".c"},
		{"addprefix", []string{"-i ", "one.symvers two.symvers"}, "-i one.symvers -i two.symvers"},
		{"addsuffix", []string{" .stamp", "one two"}, "one .stamp two .stamp"},
	}
	for _, test := range tests {
		got, recognized, err := evalPureKbuildMakeFunction(test.function, test.arguments, "original")
		if err != nil || !recognized || got != test.want {
			t.Errorf("%s(%q) = %q, recognized=%v, error=%v; want %q", test.function, test.arguments, got, recognized, err, test.want)
		}
	}
}
