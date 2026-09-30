package kconfig

import (
	"slices"
	"testing"
)

func TestInitialGuardInventoryLiteralDefinedSyntax(t *testing.T) {
	for _, test := range []struct {
		name, text string
		want       []string
	}{
		{"parenthesized", "#if defined(__PAREN)\n#endif\n", []string{"__PAREN"}},
		{"bare", "#if 0\n#elif defined __BARE\n#endif\n", []string{"__BARE"}},
		{"no-space", "#if(defined(__NO_SPACE))\n#endif\n", []string{"__NO_SPACE"}},
		{"intrinsic-neighbor", "#if __has_attribute(__attr__) && defined(__OPERAND)\n#endif\n", []string{"__OPERAND"}},
		{"digraph-splice-comment", "%:if de\\\nfined(/* comment */ __SPLICED)\n#endif\n", []string{"__SPLICED"}},
		{"duplicate", "#ifdef __DUP\n#endif\n#if defined(__DUP) || defined __DUP\n#endif\n", []string{"__DUP"}},
		{"ordinary", "#if defined(ORDINARY)\n#endif\n", nil},
		{"malformed", "#if defined(__PREFIX) && defined(, __BAD)\n#endif\n", nil},
		{"macro-operand", "#define TEST(x) defined(x)\n#if TEST(__INDIRECT)\n#endif\n", nil},
		{"string-comment", "/* #if defined(__COMMENT) */\nconst char *s = \"#if defined(__STRING)\";\n", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			mustWriteSource(t, root, "header.h", test.text)
			profile := configDependencyDefinednessProfileForTest(t, map[string]string{"__LINUX_BZL_SOURCE_TREE__": root})
			inventory := &configDependencyGuardInventory{}
			if got := inventory.names(profile); !slices.Equal(got, test.want) {
				t.Fatalf("initial names %v; want %v", got, test.want)
			}
			// The initial scan and later entered-file scan must agree on syntax,
			// while both remain hints awaiting a configured compiler query.
			if got := configDependencyCompilerGuardHintsForContents([]byte(test.text)); got.truncated || !slices.Equal(got.names, test.want) {
				t.Fatalf("entered-file hints %v; want %v", got, test.want)
			}
		})
	}
}

func TestInitialDefinedInventoryRequiresCompilerAnswer(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "header.h", "#if defined(__INITIAL_QUERY)\n#endif\n")
	profile := configDependencyDefinednessProfileForTest(t, map[string]string{"__LINUX_BZL_SOURCE_TREE__": root})
	plan := &ActionPlan{metadata: &CompactMetadata{
		Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}}
	available, measuredValue, queried := false, false, false
	plan.metadata.compilerDefinedness = func(_, _, _ string, _, _, queries []string, _ map[string]string) (map[string]bool, bool, error) {
		queried = slices.Contains(queries, "__INITIAL_QUERY")
		if !available {
			return nil, false, nil
		}
		result := map[string]bool{}
		for _, name := range queries {
			result[name] = measuredValue
		}
		return result, true, nil
	}
	definitions, _, ready, err := actionPlanCompilerDefinedness(plan, "target", "cc", "c", nil, nil, nil)
	if err != nil || ready || len(definitions) != 0 || !queried {
		t.Fatalf("unmeasured hint: definitions=%v ready=%v queried=%v err=%v", definitions, ready, queried, err)
	}
	available = true
	for _, measuredValue = range []bool{false, true} {
		definitions, _, ready, err = actionPlanCompilerDefinedness(plan, "target", "cc", "c", nil, nil, nil)
		value, found := definitions["__INITIAL_QUERY"]
		if err != nil || !ready || !found || value != measuredValue {
			t.Fatalf("compiler answer %v: definitions=%v ready=%v err=%v", measuredValue, definitions, ready, err)
		}
	}
}
