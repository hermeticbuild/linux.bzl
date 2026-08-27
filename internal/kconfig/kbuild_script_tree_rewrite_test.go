package kconfig

import (
	"encoding/base64"
	"slices"
	"strings"
	"testing"
)

func TestKbuildScriptRewritesOnlyExactDeclaredTreeInputs(t *testing.T) {
	script := `if [ -f ${tree:objects}/vmlinux ]; then
tool --base "${tree:vmlinux}/vmlinux" ${tree:modules}/drivers/demo.ko
fi
cat ${tree:objects}/vmlinux.debug ${tree:modules}/drivers/demo.ko.sig
`
	rewritten, err := rewriteCompactKbuildScriptDeclaredTreeInputs(script, []compactKbuildRuleInput{
		{path: "vmlinux", producer: "vmlinux-producer"},
		{path: "drivers/demo.ko", producer: "module-producer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, exact := range []string{"[ -f vmlinux ]", `--base "vmlinux" drivers/demo.ko`} {
		if !strings.Contains(rewritten, exact) {
			t.Errorf("rewritten script omits %q: %q", exact, rewritten)
		}
	}
	for _, undeclared := range []string{
		"${tree:objects}/vmlinux.debug",
		"${tree:modules}/drivers/demo.ko.sig",
	} {
		if !strings.Contains(rewritten, undeclared) {
			t.Errorf("rewritten script broadened declared input to %q: %q", undeclared, rewritten)
		}
	}
}

func TestKbuildScriptRewritesOnlyExactInvocationRelativeInputs(t *testing.T) {
	root := t.TempDir()
	profile := mustCompactKbuildProfileForTest(t, "tool", "tools/tool/Makefile", "tools/tool", "", nil)
	profile.evaluator.template.sourceRoots = map[string]string{"__LINUX_BZL_SOURCE_TREE__": root}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "tools/tool",
	}); err != nil {
		t.Fatal(err)
	}
	script := `awk -f ../arch/tools/generate.awk ../arch/lib/opcode-map.txt > tools/tool/generated.c
awk --source=../arch/tools/generate.awk ./local-map.txt
printf '%s\n' ../arch/tools/generate.awk.extra prefix../arch/tools/generate.awk
# ../arch/tools/generate.awk
printf '%s\n' '$local-map.txt' 'local-map.txt${suffix}' 'local-map.txt*'
cat <<'EOF'
../arch/tools/generate.awk
EOF
`
	rewritten, err := rewriteCompactKbuildScriptInvocationInputs(script, profile, []compactKbuildRuleInput{
		{path: "tools/arch/tools/generate.awk", sourceID: "src-00000001"},
		{path: "tools/arch/lib/opcode-map.txt", sourceID: "src-00000002"},
		{path: "tools/tool/local-map.txt", producer: "producer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, exact := range []string{
		"awk -f tools/arch/tools/generate.awk tools/arch/lib/opcode-map.txt",
		"awk --source=tools/arch/tools/generate.awk tools/tool/local-map.txt",
	} {
		if !strings.Contains(rewritten, exact) {
			t.Errorf("rewritten script omits %q: %q", exact, rewritten)
		}
	}
	for _, adjacent := range []string{
		"../arch/tools/generate.awk.extra",
		"prefix../arch/tools/generate.awk",
		"# ../arch/tools/generate.awk",
		"$local-map.txt",
		"local-map.txt${suffix}",
		"local-map.txt*",
		"cat <<'EOF'\n../arch/tools/generate.awk\nEOF",
	} {
		if !strings.Contains(rewritten, adjacent) {
			t.Errorf("rewritten script broadened declared input to %q: %q", adjacent, rewritten)
		}
	}
}

func TestKbuildCommandRewritesExactInvocationRelativeInputArguments(t *testing.T) {
	root := t.TempDir()
	profile := mustCompactKbuildProfileForTest(t, "tool", "tools/tool/Makefile", "tools/tool", "", nil)
	profile.evaluator.template.sourceRoots = map[string]string{"__LINUX_BZL_SOURCE_TREE__": root}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "tools/tool",
	}); err != nil {
		t.Fatal(err)
	}
	command := rewriteCompactKbuildCommandInvocationInputs(profile, compactKbuildRecipeCommand{
		arguments:   []string{"-f", "../arch/tools/generate.awk", "--map=../arch/lib/opcode-map.txt", "../arch/lib/undeclared.txt"},
		environment: map[string]string{"MANIFEST": "../arch/lib/opcode-map.txt"},
		stdin:       "../arch/lib/opcode-map.txt",
	}, []compactKbuildRuleInput{
		{path: "tools/arch/tools/generate.awk", sourceID: "src-00000001"},
		{path: "tools/arch/lib/opcode-map.txt", sourceID: "src-00000002"},
	})
	wantArguments := []string{"-f", "tools/arch/tools/generate.awk", "--map=tools/arch/lib/opcode-map.txt", "../arch/lib/undeclared.txt"}
	if strings.Join(command.arguments, "\x00") != strings.Join(wantArguments, "\x00") {
		t.Fatalf("arguments = %q, want %q", command.arguments, wantArguments)
	}
	if got, want := command.environment["MANIFEST"], "tools/arch/lib/opcode-map.txt"; got != want {
		t.Fatalf("MANIFEST = %q, want %q", got, want)
	}
	if got, want := command.stdin, "tools/arch/lib/opcode-map.txt"; got != want {
		t.Fatalf("stdin = %q, want %q", got, want)
	}
}

func TestHermeticKbuildScriptStagesExactTreePrerequisite(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
OBJCOPY = /selected/objcopy
cmd_mutate = if [ "$$(printf ready)" = ready ] && [ -f $(objtree)/vmlinux ]; then $(OBJCOPY) --strip-debug $(objtree)/vmlinux $@; fi
drivers/demo.ko: vmlinux FORCE
	$(call if_changed,mutate)
`, map[string]string{"OBJCOPY": KbuildActionRoleToken("target", "objcopy")})
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	seedRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "ld",
		Arguments: []string{"-o", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	seed, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "target", Kind: "generate", Tool: "ld", Product: "vmlinux",
		Outputs: []ActionPlanOutput{{Tree: "vmlinux", Path: "vmlinux"}},
	}, seedRecipe)
	if err != nil {
		t.Fatal(err)
	}

	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forProfile(profile).
		forOutput("target", "modules", "modules")
	producer, err := builder.build("drivers/demo.ko")
	if err != nil {
		t.Fatal(err)
	}
	var node ActionPlanNode
	for _, candidate := range plan.Nodes {
		if candidate.ID == producer {
			node = candidate
		}
	}
	foundInput := false
	for _, input := range node.Inputs {
		if input.ProducerID == seed {
			foundInput = true
		}
	}
	if !foundInput {
		t.Fatalf("script inputs = %#v, want exact vmlinux producer", node.Inputs)
	}
	recipe := plan.Recipes[node.Recipe]
	encoded := -1
	for index, argument := range recipe.Arguments {
		if argument == "-script_content_base64" {
			encoded = index + 1
			break
		}
	}
	if encoded <= 0 || encoded >= len(recipe.Arguments) {
		t.Fatalf("script recipe arguments = %q", recipe.Arguments)
	}
	decoded, err := base64.StdEncoding.DecodeString(recipe.Arguments[encoded])
	if err != nil {
		t.Fatal(err)
	}
	script := string(decoded)
	if strings.Contains(script, "${tree:kernel}/vmlinux") || !strings.Contains(script, "[ -f vmlinux ]") {
		t.Fatalf("staged script = %q, want exact relative prerequisite", decoded)
	}
	if !strings.Contains(script, "${tree:prep}/drivers/demo.ko") || !slices.Contains(recipe.Arguments, "prep=${work:root}") {
		t.Fatalf("staged script = %q arguments=%q, want authorized writable target binding", decoded, recipe.Arguments)
	}
}
