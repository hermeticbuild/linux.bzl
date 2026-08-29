package kconfig

import (
	"path"
	"slices"
	"strings"
	"testing"
)

func TestRewriteCompactKbuildCompilerRelativeIncludesUsesInvocationTree(t *testing.T) {
	for _, test := range []struct {
		name string
		tree CompactKbuildInvocationTree
		root string
	}{
		{name: "source", tree: CompactKbuildInvocationSourceTree, root: "${tree:kernel}"},
		{name: "object", tree: CompactKbuildInvocationObjectTree, root: "${work:root}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := CompactKbuildProfile{}
			if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
				Tree: test.tree, Directory: "arch/x86/kernel",
			}); err != nil {
				t.Fatal(err)
			}
			arguments := []string{
				"-I.", "-iquote", "../include", "-isystem=toolchain/include",
				"-include", "generated/autoconf.h", "-I-",
			}
			got, err := rewriteCompactKbuildCompilerRelativeIncludes(profile, "cc", ".", arguments)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{
				"-I" + test.root + "/arch/x86/kernel",
				"-iquote", test.root + "/arch/x86/include",
				"-isystem=toolchain/include",
				"-include", test.root + "/arch/x86/kernel/generated/autoconf.h",
				"-I-",
			}
			if !slices.Equal(got, want) {
				t.Fatalf("rewritten include argv = %#v, want %#v", got, want)
			}
		})
	}
}

func TestRewriteCompactKbuildCompilerRelativeIncludesAllowsRootAndRejectsEscape(t *testing.T) {
	profile := CompactKbuildProfile{}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := rewriteCompactKbuildCompilerRelativeIncludes(profile, "cc", ".", []string{"-I.", "-I=toolchain/include"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"-I${work:root}", "-I=toolchain/include"}; !slices.Equal(got, want) {
		t.Fatalf("root include argv = %#v, want %#v", got, want)
	}
	if _, err := rewriteCompactKbuildCompilerRelativeIncludes(profile, "cc", ".", []string{"-I../escape"}); err == nil {
		t.Fatal("include directory escaping its declared object tree was accepted")
	}
}

func TestRewriteCompactKbuildCompilerIncludesKeepsSourceOverlayPathsReplayable(t *testing.T) {
	const directory = ".linux-bzl/external/demo"
	profile := CompactKbuildProfile{Directory: directory}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	profile.evaluator = &kbuildTargetEvaluator{template: &kbuildParser{
		sourceRoots: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__":              t.TempDir(),
			"__LINUX_BZL_SOURCE_TREE__/" + directory: t.TempDir(),
		},
	}}
	arguments := []string{
		"-I__LINUX_BZL_OBJECT_TREE__/" + directory,
		"-include", "__LINUX_BZL_OBJECT_TREE__/" + directory + "/local.h",
		"-I__LINUX_BZL_OBJECT_TREE__/include/generated",
	}
	for _, test := range []struct {
		name       string
		objectRoot string
	}{
		{name: "private root", objectRoot: "."},
		{name: "typed nested cwd", objectRoot: "../../../.."},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := rewriteCompactKbuildCompilerRelativeIncludes(profile, "cc", test.objectRoot, arguments)
			if err != nil {
				t.Fatal(err)
			}
			rootedDirectory := path.Join(test.objectRoot, directory)
			want := []string{
				"-I" + rootedDirectory,
				"-include", path.Join(rootedDirectory, "local.h"),
				"-I__LINUX_BZL_OBJECT_TREE__/include/generated",
			}
			if !slices.Equal(got, want) {
				t.Fatalf("source-overlay include argv = %#v, want replayable %#v", got, want)
			}
		})
	}
}

func TestRewriteCompactKbuildCompilerRelativeIncludesStopsAtDoubleDash(t *testing.T) {
	profile := CompactKbuildProfile{}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "drivers/demo",
	}); err != nil {
		t.Fatal(err)
	}
	arguments := []string{"-Ibefore", "--", "-Iafter", "-include", "ignored.h"}
	got, err := rewriteCompactKbuildCompilerRelativeIncludes(profile, "cc", ".", arguments)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-I${work:root}/drivers/demo/before", "--", "-Iafter", "-include", "ignored.h"}
	if !slices.Equal(got, want) {
		t.Fatalf("rewritten include argv = %#v, want %#v", got, want)
	}
}

func TestRewriteCompactKbuildBindgenRelativeIncludesUsesPostDelimiterCompilerArguments(t *testing.T) {
	profile := CompactKbuildProfile{}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "rust/bindings",
	}); err != nil {
		t.Fatal(err)
	}
	arguments := []string{
		"bindings_helper.h", "-o", "bindings_generated.rs",
		"--", "-I.", "-include", "generated/autoconf.h", "-I=toolchain/include",
	}
	got, err := rewriteCompactKbuildCompilerRelativeIncludes(profile, "bindgen", ".", arguments)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"bindings_helper.h", "-o", "bindings_generated.rs",
		"--", "-I${work:root}/rust/bindings", "-include",
		"${work:root}/rust/bindings/generated/autoconf.h", "-I=toolchain/include",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rewritten bindgen argv = %#v, want %#v", got, want)
	}

	for _, malformed := range [][]string{
		{"bindings_helper.h", "-Ibefore"},
		{"bindings_helper.h", "--", "-Ione", "--", "-Itwo"},
	} {
		got, err := rewriteCompactKbuildCompilerRelativeIncludes(profile, "bindgen", ".", malformed)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, malformed) {
			t.Fatalf("malformed bindgen argv = %#v, want fail-closed %#v", got, malformed)
		}
	}
}

func TestGenericKbuildCompilerLowersRelativeIncludesFromTypedInvocationTree(t *testing.T) {
	const (
		directory = "drivers/demo"
		target    = directory + "/demo.o"
		source    = directory + "/demo.c"
	)
	for _, test := range []struct {
		name string
		tree CompactKbuildInvocationTree
		root string
	}{
		{name: "source", tree: CompactKbuildInvocationSourceTree, root: "${tree:kernel}"},
		{name: "object", tree: CompactKbuildInvocationObjectTree, root: "${work:root}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
cmd_cc_o_c = $(CC) -I. -iquote ../shared -I=toolchain/include -c -o $@ $<
`, map[string]string{"CC": KbuildActionRoleToken("target", "cc")})
			profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
			if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
				Tree: test.tree, Directory: directory,
			}); err != nil {
				t.Fatal(err)
			}
			metadata := &CompactMetadata{actionRoles: testConfiguredScopedActionRoles}
			plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
			sourceID, err := metadata.ensureActionPlanSource(plan, source)
			if err != nil {
				t.Fatal(err)
			}
			builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
			producer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
				profile: profile, stem: "demo", command: "cc_o_c",
			}, []compactKbuildRuleInput{{path: source, sourceID: sourceID}})
			if err != nil {
				t.Fatal(err)
			}
			node, ok := compactKbuildPlanNode(plan, producer)
			if !ok {
				t.Fatalf("compiler producer %q not found", producer)
			}
			recipe := plan.Recipes[node.Recipe]
			for _, want := range []string{
				"-I" + test.root + "/drivers/demo",
				test.root + "/drivers/shared",
				"-I=toolchain/include",
			} {
				if !slices.Contains(recipe.Arguments, want) {
					t.Fatalf("compiler arguments = %#v, want %q", recipe.Arguments, want)
				}
			}
		})
	}
}

func TestAnalyzeCompactKbuildCCompilerOutputsUsesTypedInvocationLocation(t *testing.T) {
	profile := CompactKbuildProfile{Directory: "wrong/profile/directory"}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "nested/invocation",
	}); err != nil {
		t.Fatal(err)
	}
	arguments := []string{
		"-o", "plain.o",
		"-MF${tree:prep}/generated/joined.d",
		"-MF", "../shared/separate.d",
		"-Wp,-MMD,relative/dependency.d,-MT,plain.o",
		"-I${tree:prep}/include/generated",
	}
	analysis, err := analyzeCompactKbuildCompilerOutputs(profile, "cc", arguments)
	if err != nil {
		t.Fatal(err)
	}
	wantArguments := []string{
		"-o", "${work:root}/nested/invocation/plain.o",
		"-MF${work:root}/generated/joined.d",
		"-MF", "${work:root}/nested/shared/separate.d",
		"-Wp,-MMD,${work:root}/nested/invocation/relative/dependency.d,-MT,plain.o",
		"-I${tree:prep}/include/generated",
	}
	if !slices.Equal(analysis.Arguments, wantArguments) {
		t.Fatalf("rewritten compiler argv = %#v, want %#v", analysis.Arguments, wantArguments)
	}
	if got, want := analysis.PrimaryOutput, "nested/invocation/plain.o"; got != want {
		t.Fatalf("primary output = %q, want %q", got, want)
	}
	if got, want := analysis.WorkingDirectories, []string{"generated", "nested/invocation", "nested/invocation/relative", "nested/shared"}; !slices.Equal(got, want) {
		t.Fatalf("working directories = %#v, want %#v", got, want)
	}
	if !slices.Equal(arguments, []string{
		"-o", "plain.o",
		"-MF${tree:prep}/generated/joined.d",
		"-MF", "../shared/separate.d",
		"-Wp,-MMD,relative/dependency.d,-MT,plain.o",
		"-I${tree:prep}/include/generated",
	}) {
		t.Fatalf("compiler output analysis mutated its input: %#v", arguments)
	}
}

func TestAnalyzeCompactKbuildCCompilerInputsUseEvaluatedObjectIncludeRoots(t *testing.T) {
	profile := CompactKbuildProfile{}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: ".linux-bzl/external/demo",
	}); err != nil {
		t.Fatal(err)
	}
	arguments := []string{
		"-I${tree:kernel}/include",
		"-I${tree:prep}/arch/x86/include/generated",
		"-isystem", "__LINUX_BZL_OBJECT_TREE__/include/generated",
		"-iquoterelative/include",
		"-idirafter${work:root}/arch/x86/include/generated/uapi",
		"-include", "${tree:prep}/include/generated/autoconf.h",
		"-imacros__LINUX_BZL_OBJECT_TREE__/include/generated/rustc_cfg",
		"--", "-I${tree:prep}/ignored",
	}
	analysis, err := analyzeCompactKbuildCompilerOutputs(profile, "cc", arguments)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := analysis.PreparedObjectDirectories, []string{
		".linux-bzl/external/demo/relative/include",
		"arch/x86/include/generated",
		"arch/x86/include/generated/uapi",
		"include/generated",
	}; !slices.Equal(got, want) {
		t.Fatalf("prepared object directories = %#v, want %#v", got, want)
	}
	if got, want := analysis.PreparedObjectIncludeFiles, []string{
		"include/generated/autoconf.h",
		"include/generated/rustc_cfg",
	}; !slices.Equal(got, want) {
		t.Fatalf("prepared object inputs = %#v, want %#v", got, want)
	}
	if !slices.Equal(analysis.Arguments, arguments) {
		t.Fatalf("compiler input analysis mutated argv: %#v", analysis.Arguments)
	}
}

func TestAnalyzeCompactKbuildCompilerOutputsAvoidsDoubleScopingAutomaticPath(t *testing.T) {
	profile := CompactKbuildProfile{Directory: "unrelated/logical/obj"}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "nested/invocation",
	}); err != nil {
		t.Fatal(err)
	}
	analysis, err := analyzeCompactKbuildCompilerOutputs(profile, "cxx", []string{
		"-onested/invocation/automatic.o",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := analysis.Arguments, []string{"-onested/invocation/automatic.o"}; !slices.Equal(got, want) {
		t.Fatalf("already-scoped compiler argv = %#v, want %#v", got, want)
	}
	if got, want := analysis.PrimaryOutput, "nested/invocation/automatic.o"; got != want {
		t.Fatalf("primary output = %q, want %q", got, want)
	}
	if got, want := analysis.WorkingDirectories, []string{"nested/invocation"}; !slices.Equal(got, want) {
		t.Fatalf("working directories = %#v, want %#v", got, want)
	}
}

func TestAnalyzeCompactKbuildCompilerOutputsPreservesExplicitlyRootedTargetAcrossSplitRoots(t *testing.T) {
	const target = "tools/objtool/libsubcmd/exec-cmd.o"
	profile := CompactKbuildProfile{Directory: "libsubcmd"}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationSourceTree, Directory: "tools/lib/subcmd",
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		role      string
		arguments []string
		want      []string
	}{
		{
			name: "separate C output", role: "cc",
			arguments: []string{"-c", "exec-cmd.c", "-o", "${tree:prep}/" + target},
			want:      []string{"-c", "exec-cmd.c", "-o", "${work:root}/" + target},
		},
		{
			name: "joined C++ output", role: "cxx",
			arguments: []string{"-c", "exec-cmd.cc", "-o${tree:prep}/" + target},
			want:      []string{"-c", "exec-cmd.cc", "-o${work:root}/" + target},
		},
		{
			name: "structured Rust output", role: "rustc",
			arguments: []string{"--emit=obj=${tree:prep}/" + target, "exec-cmd.rs"},
			want:      []string{"--emit=obj=${work:root}/" + target, "exec-cmd.rs"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			analysis, err := analyzeCompactKbuildCompilerOutputs(profile, test.role, test.arguments, target)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(analysis.Arguments, test.want) {
				t.Fatalf("compiler argv = %#v, want %#v", analysis.Arguments, test.want)
			}
			if analysis.PrimaryOutput != target {
				t.Fatalf("primary output = %q, want %q", analysis.PrimaryOutput, target)
			}
			if want := []string{"tools/objtool/libsubcmd"}; !slices.Equal(analysis.WorkingDirectories, want) {
				t.Fatalf("working directories = %#v, want %#v", analysis.WorkingDirectories, want)
			}
		})
	}
}

func TestAnalyzeCompactKbuildRustCompilerOutputsMatchesKernelHostGenerator(t *testing.T) {
	profile := CompactKbuildProfile{}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	arguments := []string{
		"--target=${tree:prep}/scripts/target.json",
		"--out-dir", "${tree:prep}/scripts/",
		"--emit=dep-info=${tree:prep}/scripts/.generate_rust_target.d",
		"--emit=link=scripts/generate_rust_target",
		"scripts/generate_rust_target.rs",
	}
	analysis, err := analyzeCompactKbuildCompilerOutputs(profile, "rustc", arguments)
	if err != nil {
		t.Fatal(err)
	}
	wantArguments := []string{
		"--target=${tree:prep}/scripts/target.json",
		"--out-dir", "${work:root}/scripts/",
		"--emit=dep-info=${work:root}/scripts/.generate_rust_target.d",
		"--emit=link=scripts/generate_rust_target",
		"scripts/generate_rust_target.rs",
	}
	if !slices.Equal(analysis.Arguments, wantArguments) {
		t.Fatalf("Rust host-generator argv = %#v, want %#v", analysis.Arguments, wantArguments)
	}
	if got, want := analysis.PrimaryOutput, "scripts/generate_rust_target"; got != want {
		t.Fatalf("primary output = %q, want %q", got, want)
	}
	if got, want := analysis.WorkingDirectories, []string{"scripts"}; !slices.Equal(got, want) {
		t.Fatalf("working directories = %#v, want %#v", got, want)
	}
	if got, want := analysis.PreparedObjectInputs, []string{"scripts/target.json"}; !slices.Equal(got, want) {
		t.Fatalf("prepared object inputs = %#v, want %#v", got, want)
	}
	if arguments[0] != "--target=${tree:prep}/scripts/target.json" {
		t.Fatalf("Rust target-spec input was mutated: %#v", arguments)
	}
}

func TestAnalyzeCompactKbuildRustCompilerTargetSpecInputForms(t *testing.T) {
	profile := CompactKbuildProfile{}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: ".linux-bzl/external/demo",
	}); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"rustc", "clippy"} {
		for _, test := range []struct {
			name      string
			arguments []string
			want      []string
		}{
			{
				name:      "joined typed target",
				arguments: []string{"--target=${tree:prep}/scripts/target.json"},
				want:      []string{"scripts/target.json"},
			},
			{
				name:      "separate evaluator target",
				arguments: []string{"--target", "__LINUX_BZL_OBJECT_TREE__/arch/arm64/target.json"},
				want:      []string{"arch/arm64/target.json"},
			},
			{
				name:      "joined builtin triple",
				arguments: []string{"--target=x86_64-unknown-linux-gnu"},
			},
			{
				name:      "separate builtin triple",
				arguments: []string{"--target", "aarch64-unknown-linux-gnu"},
			},
			{
				name:      "after option terminator",
				arguments: []string{"--", "--target=${tree:prep}/ignored.json"},
			},
		} {
			t.Run(role+"/"+test.name, func(t *testing.T) {
				analysis, err := analyzeCompactKbuildCompilerOutputs(profile, role, test.arguments)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(analysis.PreparedObjectInputs, test.want) {
					t.Fatalf("prepared object inputs = %#v, want %#v", analysis.PreparedObjectInputs, test.want)
				}
				if !slices.Equal(analysis.Arguments, test.arguments) {
					t.Fatalf("target-spec argv = %#v, want unchanged %#v", analysis.Arguments, test.arguments)
				}
			})
		}
	}
}

func TestAnalyzeCompactKbuildRustCompilerOutputsHandlesJoinedAndSeparateForms(t *testing.T) {
	for _, role := range []string{"rustc", "clippy"} {
		t.Run(role, func(t *testing.T) {
			analysis, err := analyzeCompactKbuildCompilerOutputs(CompactKbuildProfile{}, role, []string{
				"--out-dir=joined/out",
				"--emit", "llvm-ir=separate/demo.ll,obj=separate/demo.o,metadata",
				"-ojoined/demo",
			})
			if err != nil {
				t.Fatal(err)
			}
			if got, want := analysis.PrimaryOutput, "joined/demo"; got != want {
				t.Fatalf("primary output = %q, want %q", got, want)
			}
			if got, want := analysis.WorkingDirectories, []string{"joined", "joined/out", "separate"}; !slices.Equal(got, want) {
				t.Fatalf("working directories = %#v, want %#v", got, want)
			}
		})
	}
}

func TestAnalyzeCompactKbuildRustCompilerDepInfoNeverReplacesPrimaryEmit(t *testing.T) {
	for _, emissions := range []string{
		"link=bin/tool,dep-info=deps/tool.d",
		"dep-info=deps/tool.d,link=bin/tool",
		"obj=obj/tool.o,dep-info=deps/tool.d",
		"dep-info=deps/tool.d,obj=obj/tool.o",
	} {
		t.Run(emissions, func(t *testing.T) {
			analysis, err := analyzeCompactKbuildCompilerOutputs(CompactKbuildProfile{}, "rustc", []string{"--emit=" + emissions})
			if err != nil {
				t.Fatal(err)
			}
			want := "bin/tool"
			if strings.Contains(emissions, "obj=") {
				want = "obj/tool.o"
			}
			if analysis.PrimaryOutput != want {
				t.Fatalf("primary output = %q, want %q", analysis.PrimaryOutput, want)
			}
		})
	}
}

func TestAnalyzeCompactKbuildRustCompilerMetadataNeverReplacesSelectedObject(t *testing.T) {
	const target = "rust/ffi.o"
	analysis, err := analyzeCompactKbuildCompilerOutputs(CompactKbuildProfile{}, "rustc", []string{
		"--emit=dep-info=rust/.ffi.o.d",
		"--emit=obj=" + target,
		"--emit=metadata=rust/libffi.rmeta",
		"-L${work:root}/${tree:prep}/rust",
	}, target)
	if err != nil {
		t.Fatal(err)
	}
	if analysis.PrimaryOutput != target {
		t.Fatalf("primary output = %q, want selected object %q", analysis.PrimaryOutput, target)
	}
	if want := []string{"rust"}; !slices.Equal(analysis.WorkingDirectories, want) {
		t.Fatalf("working directories = %#v, want %#v", analysis.WorkingDirectories, want)
	}
	if want := []string{"rust/libffi.rmeta"}; !slices.Equal(analysis.PersistentOutputs, want) {
		t.Fatalf("persistent outputs = %#v, want %#v", analysis.PersistentOutputs, want)
	}
	if want := []string{"rust"}; !slices.Equal(analysis.LibrarySearchDirectories, want) {
		t.Fatalf("library search directories = %#v, want %#v", analysis.LibrarySearchDirectories, want)
	}
	if got, want := analysis.Arguments[len(analysis.Arguments)-1], "-L${work:root}/rust"; got != want {
		t.Fatalf("library search argument = %q, want %q", got, want)
	}
}

func TestAnalyzeCompactKbuildCompilerOutputsHonorsOptionTerminator(t *testing.T) {
	for _, test := range []struct {
		role      string
		arguments []string
	}{
		{role: "cc", arguments: []string{"--", "-o", "ignored.o", "-MF${tree:prep}/ignored.d"}},
		{role: "rustc", arguments: []string{"--", "--out-dir=${tree:prep}/ignored", "--emit=link=ignored"}},
	} {
		t.Run(test.role, func(t *testing.T) {
			analysis, err := analyzeCompactKbuildCompilerOutputs(CompactKbuildProfile{}, test.role, test.arguments)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(analysis.Arguments, test.arguments) || analysis.PrimaryOutput != "" || len(analysis.WorkingDirectories) != 0 {
				t.Fatalf("terminated %s analysis = %#v", test.role, analysis)
			}
		})
	}
}

func TestAnalyzeCompactKbuildCompilerOutputsRejectsObjectTreeEscape(t *testing.T) {
	for _, test := range []struct {
		role      string
		arguments []string
	}{
		{role: "cc", arguments: []string{"-MF${tree:prep}/../outside.d"}},
		{role: "rustc", arguments: []string{"--emit=dep-info=${tree:prep}/../outside.d"}},
	} {
		t.Run(test.role, func(t *testing.T) {
			if _, err := analyzeCompactKbuildCompilerOutputs(CompactKbuildProfile{}, test.role, test.arguments); err == nil {
				t.Fatal("compiler output escaping its object tree was accepted")
			}
		})
	}
}
