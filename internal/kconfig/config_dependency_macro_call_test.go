package kconfig

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Only test fixtures use this closed namespace. Production has no map default:
// its resolver must independently prove undefined/defined/unmodeled/unknown.
type configDependencyMacroCallFixtureCatalog map[string][]*configDependencyMacroCallDefinition

func configDependencyMacroCallFixtureCatalogForTest(lines ...string) (configDependencyMacroCallFixtureCatalog, error) {
	c := configDependencyMacroCallFixtureCatalog{}
	for i, line := range lines {
		d, reason := configDependencyMacroCallDefinitionFromText(line, fmt.Sprintf("fixture:%d", i))
		if reason != "" {
			return nil, fmt.Errorf("%s", reason)
		}
		if _, _, err := configDependencyMacroCallParse(line, configDependencyMacroCallMode{dollarAsPunctuation: true}); err != nil {
			return nil, err
		}
		c[d.name] = append(c[d.name], d)
	}
	return c, nil
}

func configDependencyMacroCallFixtureResolver(c configDependencyMacroCallFixtureCatalog) configDependencyMacroCallResolver {
	return func(name string) (configDependencyMacroCallBinding, string) {
		defs := c[name]
		if len(defs) == 0 {
			return configDependencyMacroCallBinding{state: configDependencyMacroUndefined}, ""
		}
		for _, other := range defs[1:] {
			if other.text != defs[0].text {
				return configDependencyMacroCallBinding{state: configDependencyMacroUnknown}, "ambiguous definition " + name
			}
		}
		return configDependencyMacroCallBinding{state: configDependencyMacroDefined, definition: defs[0]}, ""
	}
}

func configDependencyMacroCallFixtureExpandForTest(c configDependencyMacroCallFixtureCatalog, text string) (configDependencyMacroCallResult, error) {
	r, reason := configDependencyMacroCallExpand(text, configDependencyMacroCallFixtureResolver(c), configDependencyMacroCallMode{dollarAsPunctuation: true})
	if reason != "" {
		return r, fmt.Errorf("%s", reason)
	}
	return r, nil
}

func TestConfigDependencyMacroCallSelfReferenceSuppression(t *testing.T) {
	for _, test := range []struct {
		name        string
		definitions []string
		input, want string
		reads       []string
	}{
		{"fresh occurrences", []string{"A A + 1"}, "A A", "A + 1 A + 1", nil},
		{"prescan persistence", []string{"A A + 1", "ID(x) x"}, "ID(A)", "A + 1", nil},
		{"indirect", []string{"A B", "B A", "ID(x) x"}, "ID(A)", "A", nil},
		{"function self", []string{"F(x) F(x) + CONFIG_A", "CONFIG_A 7"}, "F(1)", "F ( 1 ) + 7", []string{"CONFIG_A"}},
		{"nested argument before disablement", []string{"F(x) F(x) + CONFIG_A", "CONFIG_A 7"}, "F(F(1))", "F ( F ( 1 ) + 7 ) + 7", []string{"CONFIG_A"}},
		{"indirect functions", []string{"F(x) G(x)", "G(x) F(x)"}, "F(1)", "F ( 1 )", nil},
		{"disabled function without parenthesis", []string{"F(x) x F", "CALL(f) f(7)"}, "CALL(F(0))", "0 F ( 7 )", nil},
		{"enabled function without parenthesis", []string{"F(x) x", "CALL(f) f(7)"}, "CALL(F)", "7", nil},
		{"disabled call has no arity check", []string{"F(x) F(x,x)"}, "F(1)", "F ( 1 , 1 )", nil},
		{"zero-argument function", []string{"F() F()"}, "F()", "F ( )", nil},
		{"fresh pasted self", []string{"SELF SE ## LF"}, "SELF", "SELF", nil},
		{"self config", []string{"CONFIG_SELF CONFIG_SELF + 1"}, "CONFIG_SELF", "CONFIG_SELF + 1", []string{"CONFIG_SELF"}},
		{"GNU raw tail", []string{"F(x,...) x,##__VA_ARGS__"}, "F(0,F(1,2))", "0 , F ( 1 , 2 )", nil},
		{"GNU raw tail through wrapper", []string{"F(x,...) x,##__VA_ARGS__", "ID(x) x"}, "F(0,ID(F(1,2)))", "0 , F ( 1 , 2 )", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog, err := configDependencyMacroCallFixtureCatalogForTest(test.definitions...)
			if err != nil {
				t.Fatal(err)
			}
			got, err := configDependencyMacroCallFixtureExpandForTest(catalog, test.input)
			if err != nil || strings.Join(got.Tokens, " ") != test.want || !slices.Equal(got.ConfigReads, test.reads) {
				t.Fatalf("self-reference = %#v, %v; want %q, reads %q", got, err, test.want, test.reads)
			}
			for name, definitions := range catalog {
				count := 0
				for _, read := range got.DefinitionReads {
					if read.Name != name {
						continue
					}
					count++
					if read.State != configDependencyMacroDefined || read.Origin != definitions[0].origin ||
						read.DefinitionID != definitions[0].identity {
						t.Fatalf("self-reference lost exact definition witness: %#v", read)
					}
				}
				if count != 1 {
					t.Fatalf("definition %s read count = %d, want exactly one authenticated binding", name, count)
				}
			}
		})
	}
}

func TestConfigDependencyMacroCallUnavailableTokenDoesNotMutateDefinition(t *testing.T) {
	catalog, err := configDependencyMacroCallFixtureCatalogForTest("A A + 1")
	if err != nil {
		t.Fatal(err)
	}
	machine := configDependencyMacroCallMachine{
		resolve:         configDependencyMacroCallFixtureResolver(catalog),
		bindings:        map[string]configDependencyMacroCallBinding{},
		parsed:          map[*configDependencyMacroCallDefinition]configDependencyMacroCallParsed{},
		definitionReads: map[string]configDependencyMacroCallRead{},
		reads:           map[string]bool{},
	}
	tokens, err := configDependencyMacroCallLex("A", configDependencyMacroCallMode{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := machine.expand(tokens, map[string]bool{})
	if err != nil || len(first) != 3 || !first[0].unavailable || tokens[0].unavailable {
		t.Fatalf("suppression did not stay on the expanded occurrence: %#v, %v", first, err)
	}
	second, err := machine.expand(first, map[string]bool{})
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("rescan reactivated a suppressed token: %#v, %v", second, err)
	}
	fresh, err := machine.expand(tokens, map[string]bool{})
	if err != nil || !reflect.DeepEqual(first, fresh) {
		t.Fatalf("earlier suppression poisoned a fresh occurrence: %#v, %v", fresh, err)
	}
	for _, token := range machine.parsed[catalog["A"][0]].replacement {
		if token.unavailable {
			t.Fatal("parsed immutable replacement retained occurrence-specific suppression")
		}
	}
}

func TestConfigDependencyMacroCallPasteCreatesFreshOccurrence(t *testing.T) {
	for _, tc := range []struct {
		name        string
		definitions []string
		input, want string
		reads       []string
	}{
		{"left", []string{"A A", "JOIN(a,b) a##b", "FWD(a,b) JOIN(a,b)"}, "FWD(A,X)", "AX", nil},
		{"right", []string{"X X", "JOIN(a,b) a##b", "FWD(a,b) JOIN(a,b)"}, "FWD(A,X)", "AX", nil},
		{"both", []string{"A A", "X X", "AX CONFIG_DRIVER", "JOIN(a,b) a##b", "FWD(a,b) JOIN(a,b)"}, "FWD(A,X)", "CONFIG_DRIVER", []string{"CONFIG_DRIVER"}},
		{"fresh function", []string{"A A", "AX(v) CONFIG_DRIVER + v", "JOIN(a,b) a##b(3)", "FWD(a,b) JOIN(a,b)"}, "FWD(A,X)", "CONFIG_DRIVER + 3", []string{"CONFIG_DRIVER"}},
		{"active result", []string{"A A", "AX FWD(A,X)", "JOIN(a,b) a##b", "FWD(a,b) JOIN(a,b)"}, "FWD(A,X)", "FWD ( A , X )", nil},
		{"new self reference", []string{"A A", "AX AX + CONFIG_DRIVER", "JOIN(a,b) a##b", "FWD(a,b) JOIN(a,b)"}, "FWD(A,X)", "AX + CONFIG_DRIVER", []string{"CONFIG_DRIVER"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog, err := configDependencyMacroCallFixtureCatalogForTest(tc.definitions...)
			if err != nil {
				t.Fatal(err)
			}
			got, err := configDependencyMacroCallFixtureExpandForTest(catalog, tc.input)
			if err != nil || strings.Join(got.Tokens, " ") != tc.want || !slices.Equal(got.ConfigReads, tc.reads) {
				t.Fatalf("fresh paste = %#v, %v; want %q, reads %q", got, err, tc.want, tc.reads)
			}
			for name, definitions := range catalog {
				if !slices.ContainsFunc(got.DefinitionReads, func(read configDependencyMacroCallRead) bool {
					return read.Name == name && read.State == configDependencyMacroDefined &&
						read.Origin == definitions[0].origin && read.DefinitionID == definitions[0].identity
				}) {
					t.Fatalf("paste lost authenticated binding for %s: %#v", name, got.DefinitionReads)
				}
			}
		})
	}
}

func TestConfigDependencyMacroCallFreshPasteRequiresNewBinding(t *testing.T) {
	catalog, err := configDependencyMacroCallFixtureCatalogForTest("A A", "X X", "JOIN(a,b) a##b", "FWD(a,b) JOIN(a,b)")
	if err != nil {
		t.Fatal(err)
	}
	resolve := configDependencyMacroCallFixtureResolver(catalog)
	for _, state := range []configDependencyMacroDefinition{configDependencyMacroUnknown, configDependencyMacroDefined} {
		got, reason := configDependencyMacroCallExpand("CONFIG_BEFORE FWD(A,X)", func(name string) (configDependencyMacroCallBinding, string) {
			if name == "AX" {
				return configDependencyMacroCallBinding{state: state}, ""
			}
			return resolve(name)
		}, configDependencyMacroCallMode{dollarAsPunctuation: true})
		if reason == "" || !reflect.DeepEqual(got, configDependencyMacroCallResult{}) {
			t.Fatalf("fresh spelling reused operand authority: %#v, %q", got, reason)
		}
	}
}

func TestConfigDependencyMacroCallSelfReferenceKeepsUnsupportedBoundaries(t *testing.T) {
	for _, test := range []struct {
		definitions   []string
		input, reason string
	}{
		{[]string{"A A", "AX _Pragma(\"bad\")", "JOIN(a,b) a##b", "FWD(a,b) JOIN(a,b)"}, "CONFIG_BEFORE FWD(A,X)", "preprocessing effect"},
		{[]string{"F(x) F"}, "CONFIG_BEFORE F(0)(1)", "rescan boundary"},
		{[]string{"A A", "ALIAS JOIN", "JOIN(a,b) a##b"}, "CONFIG_BEFORE A ALIAS(1,UL)", "rescan boundary"},
		{[]string{"A A"}, "CONFIG_BEFORE A _Pragma(\"bad\")", "preprocessing effect"},
	} {
		catalog, err := configDependencyMacroCallFixtureCatalogForTest(test.definitions...)
		if err != nil {
			t.Fatal(err)
		}
		got, err := configDependencyMacroCallFixtureExpandForTest(catalog, test.input)
		if err == nil || !strings.Contains(err.Error(), test.reason) || !reflect.DeepEqual(got, configDependencyMacroCallResult{}) {
			t.Fatalf("unsupported self-reference boundary published a prefix: %#v, %v", got, err)
		}
	}
	var definitions []string
	for index := range 35 {
		definitions = append(definitions, fmt.Sprintf("D%d D%d", index, index+1))
	}
	catalog, err := configDependencyMacroCallFixtureCatalogForTest(definitions...)
	if err != nil {
		t.Fatal(err)
	}
	got, err := configDependencyMacroCallFixtureExpandForTest(catalog, "CONFIG_BEFORE D0")
	if err == nil || !strings.Contains(err.Error(), "depth budget") || !reflect.DeepEqual(got, configDependencyMacroCallResult{}) {
		t.Fatalf("acyclic depth budget changed: %#v, %v", got, err)
	}
}

func TestConfigDependencyMacroCallResolverFailClosed(t *testing.T) {
	valid, reason := configDependencyMacroCallDefinitionFromText("VALUE 7", "source:sha256:span1")
	if reason != "" {
		t.Fatal(reason)
	}
	other, reason := configDependencyMacroCallDefinitionFromText("OTHER 8", "source:sha256:span2")
	if reason != "" {
		t.Fatal(reason)
	}
	for _, tc := range []struct {
		name    string
		binding configDependencyMacroCallBinding
		reason  string
	}{
		{"zero", configDependencyMacroCallBinding{}, ""},
		{"unknown", configDependencyMacroCallBinding{state: configDependencyMacroUnknown}, ""},
		{"defined unmodeled", configDependencyMacroCallBinding{state: configDependencyMacroDefined}, ""},
		{"undefined with text", configDependencyMacroCallBinding{state: configDependencyMacroUndefined, definition: valid}, ""},
		{"wrong name", configDependencyMacroCallBinding{state: configDependencyMacroDefined, definition: other}, ""},
		{"resolver refusal", configDependencyMacroCallBinding{state: configDependencyMacroUndefined}, "no authenticated answer"},
		{"forged zero record", configDependencyMacroCallBinding{state: configDependencyMacroDefined, definition: &configDependencyMacroCallDefinition{}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolve := func(name string) (configDependencyMacroCallBinding, string) {
				if name == "CONFIG_BEFORE" {
					return configDependencyMacroCallBinding{state: configDependencyMacroUndefined}, ""
				}
				return tc.binding, tc.reason
			}
			got, reason := configDependencyMacroCallExpand("CONFIG_BEFORE VALUE", resolve, configDependencyMacroCallMode{})
			if reason == "" || !reflect.DeepEqual(got, configDependencyMacroCallResult{}) {
				t.Fatalf("%+v %q", got, reason)
			}
		})
	}
	if got, reason := configDependencyMacroCallExpand("VALUE", nil, configDependencyMacroCallMode{}); reason == "" || !reflect.DeepEqual(got, configDependencyMacroCallResult{}) {
		t.Fatalf("nil resolver accepted: %+v %q", got, reason)
	}
	// Neither ordinary nor reserved names acquire implicit undefined status.
	for _, name := range []string{"ordinary", "__UNMODELED", "__LINE__", "__has_builtin", "CONFIG_MISSING"} {
		resolve := func(string) (configDependencyMacroCallBinding, string) {
			return configDependencyMacroCallBinding{state: configDependencyMacroUnknown}, ""
		}
		if got, reason := configDependencyMacroCallExpand(name, resolve, configDependencyMacroCallMode{}); reason == "" || !reflect.DeepEqual(got, configDependencyMacroCallResult{}) {
			t.Fatalf("unknown %s accepted: %+v %q", name, got, reason)
		}
	}
}

func TestConfigDependencyMacroCallExactReadIdentities(t *testing.T) {
	spaced, reason := configDependencyMacroCallDefinitionFromText(" VALUE 7 ", "exact:origin")
	if reason != "" || spaced.text != " VALUE 7 " {
		t.Fatalf("factory lost exact input bytes: %+v %q", spaced, reason)
	}
	d1, reason := configDependencyMacroCallDefinitionFromText("VALUE CONFIG_READ", "source:first")
	if reason != "" {
		t.Fatal(reason)
	}
	d2, reason := configDependencyMacroCallDefinitionFromText("VALUE CONFIG_READ", "source:second")
	if reason != "" {
		t.Fatal(reason)
	}
	d3, reason := configDependencyMacroCallDefinitionFromText("VALUE 2", "source:first")
	if reason != "" {
		t.Fatal(reason)
	}
	var results []configDependencyMacroCallResult
	for _, d := range []*configDependencyMacroCallDefinition{d1, d2, d3} {
		counts := map[string]int{}
		resolve := func(name string) (configDependencyMacroCallBinding, string) {
			counts[name]++
			if name == "VALUE" {
				return configDependencyMacroCallBinding{state: configDependencyMacroDefined, definition: d}, ""
			}
			return configDependencyMacroCallBinding{state: configDependencyMacroUndefined}, ""
		}
		got, reason := configDependencyMacroCallExpand("VALUE VALUE", resolve, configDependencyMacroCallMode{})
		if reason != "" {
			t.Fatal(reason)
		}
		if counts["VALUE"] != 1 {
			t.Fatalf("lookup memo is not per expansion: %v", counts)
		}
		if got.DefinitionReads[len(got.DefinitionReads)-1] != (configDependencyMacroCallRead{Name: "VALUE", State: configDependencyMacroDefined, Origin: d.origin, DefinitionID: d.identity}) {
			t.Fatalf("lost exact modeled identity: %+v", got)
		}
		results = append(results, got)
	}
	for i := 1; i < len(results); i++ {
		if results[0].DefinitionReads[len(results[0].DefinitionReads)-1] == results[i].DefinitionReads[len(results[i].DefinitionReads)-1] {
			t.Fatal("origin or replacement identity was lost")
		}
	}
	if results[0].DefinitionReads[0] != (configDependencyMacroCallRead{Name: "CONFIG_READ", State: configDependencyMacroUndefined}) {
		t.Fatalf("proven-undefined read missing: %+v", results[0])
	}
	changed := *d1
	changed.text = "VALUE 9"
	resolve := func(string) (configDependencyMacroCallBinding, string) {
		return configDependencyMacroCallBinding{state: configDependencyMacroDefined, definition: &changed}, ""
	}
	if _, reason := configDependencyMacroCallExpand("VALUE", resolve, configDependencyMacroCallMode{}); reason == "" {
		t.Fatal("mutated record accepted")
	}
	changed = *d1
	changed.function = true
	if _, reason := configDependencyMacroCallExpand("VALUE", resolve, configDependencyMacroCallMode{}); reason == "" {
		t.Fatal("mutated shape accepted")
	}
}

func TestConfigDependencyMacroCallModeAndLazyDefinitions(t *testing.T) {
	undefined := func(string) (configDependencyMacroCallBinding, string) {
		return configDependencyMacroCallBinding{state: configDependencyMacroUndefined}, ""
	}
	if _, reason := configDependencyMacroCallExpand("$CONFIG_A", undefined, configDependencyMacroCallMode{}); reason == "" {
		t.Fatal("dollar enabled by default")
	}
	got, reason := configDependencyMacroCallExpand("$CONFIG_A", undefined, configDependencyMacroCallMode{dollarAsPunctuation: true})
	if reason != "" || !slices.Equal(got.Tokens, []string{"$", "CONFIG_A"}) || !slices.Equal(got.ConfigReads, []string{"CONFIG_A"}) {
		t.Fatalf("%+v %q", got, reason)
	}
	for _, literal := range []string{"\"$CONFIG_A\"", "'$'", "/* $CONFIG_A */ 1"} {
		if _, reason := configDependencyMacroCallExpand(literal, undefined, configDependencyMacroCallMode{}); reason != "" {
			t.Fatal(reason)
		}
	}
	bad, reason := configDependencyMacroCallDefinitionFromText("UNUSED(x) #not_a_formal", "source:unused")
	if reason != "" {
		t.Fatal("factory eagerly parsed replacement: " + reason)
	}
	resolve := func(name string) (configDependencyMacroCallBinding, string) {
		if name == "UNUSED" {
			return configDependencyMacroCallBinding{state: configDependencyMacroDefined, definition: bad}, ""
		}
		return undefined(name)
	}
	if got, reason := configDependencyMacroCallExpand("UNUSED", resolve, configDependencyMacroCallMode{}); reason != "" || !slices.Equal(got.Tokens, []string{"UNUSED"}) {
		t.Fatalf("uninvoked macro parsed: %+v %q", got, reason)
	}
	if got, reason := configDependencyMacroCallExpand("UNUSED(CONFIG_A)", resolve, configDependencyMacroCallMode{}); reason == "" || !reflect.DeepEqual(got, configDependencyMacroCallResult{}) {
		t.Fatalf("invalid stringification accepted: %+v %q", got, reason)
	}
	for _, tc := range []struct{ text, origin string }{
		{"", "origin"}, {"VALUE 1", ""}, {"VALUE 1\n#undef CONFIG_A", "origin"}, {"1VALUE body", "origin"},
	} {
		if d, reason := configDependencyMacroCallDefinitionFromText(tc.text, tc.origin); reason == "" || d != nil {
			t.Fatalf("invalid factory input: %+v %q", d, reason)
		}
	}
}

func TestConfigDependencyMacroCallCommentFormals(t *testing.T) {
	c, err := configDependencyMacroCallFixtureCatalogForTest("JOIN(a/**/,b) a##b", "SECOND(a/**/,b,tail...) b")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ input, want string }{
		{"JOIN(CONFIG_,HIDDEN)", "CONFIG_HIDDEN"},
		{"SECOND(ignored,7,_Pragma(\"bad\"))", "7"},
	} {
		got, err := configDependencyMacroCallFixtureExpandForTest(c, tc.input)
		if err != nil || strings.Join(got.Tokens, " ") != tc.want {
			t.Fatalf("%+v %v", got, err)
		}
	}
	if _, err := configDependencyMacroCallFixtureCatalogForTest("F(a/**/,a) a"); err == nil {
		t.Fatal("duplicate canonical formal accepted")
	}
}

func TestConfigDependencyMacroCallRealHelperShapes(t *testing.T) {
	// Exact replacement shapes from Linux kconfig.h and the assembler/x86_64
	// branch of asm/asm.h. These are fixture data, not production name policy.
	base := []string{
		"__ARG_PLACEHOLDER_1 0,",
		"__take_second_arg(__ignored,val,...) val",
		"__or(x,y) ___or(x,y)",
		"___or(x,y) ____or(__ARG_PLACEHOLDER_##x,y)",
		"____or(arg1_or_junk,y) __take_second_arg(arg1_or_junk 1,y)",
		"__is_defined(x) ___is_defined(x)",
		"___is_defined(val) ____is_defined(__ARG_PLACEHOLDER_##val)",
		"____is_defined(arg1_or_junk) __take_second_arg(arg1_or_junk 1,0)",
		"IS_BUILTIN(option) __is_defined(option)",
		"IS_MODULE(option) __is_defined(option##_MODULE)",
		"IS_ENABLED(option) __or(IS_BUILTIN(option),IS_MODULE(option))",
		"__ASM_FORM_RAW(x,...) x,##__VA_ARGS__",
		"__ASM_SEL_RAW(a,b) __ASM_FORM_RAW(b)",
		"__ASM_REG(reg) __ASM_SEL_RAW(e##reg,r##reg)",
		"_ASM_AX __ASM_REG(ax)",
	}
	for _, tc := range []struct{ name, definition, want string }{
		{"builtin", "CONFIG_X86_64 1", "1"},
		{"module", "CONFIG_X86_64_MODULE 1", "1"},
		{"undefined", "", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defs := slices.Clone(base)
			if tc.definition != "" {
				defs = append(defs, tc.definition)
			}
			c, err := configDependencyMacroCallFixtureCatalogForTest(defs...)
			if err != nil {
				t.Fatal(err)
			}
			got, err := configDependencyMacroCallFixtureExpandForTest(c, "IS_ENABLED(CONFIG_X86_64) _ASM_AX")
			if err != nil || !slices.Equal(got.Tokens, []string{tc.want, "rax"}) || len(got.ConfigReads) == 0 {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}

func TestConfigDependencyMacroCallSuccessiveImmutableSpans(t *testing.T) {
	first, reason := configDependencyMacroCallDefinitionFromText("VALUE 1", "origin:first")
	if reason != "" {
		t.Fatal(reason)
	}
	second, reason := configDependencyMacroCallDefinitionFromText("VALUE CONFIG_CHANGED", "origin:second")
	if reason != "" {
		t.Fatal(reason)
	}
	before := *first
	current := first
	resolve := func(name string) (configDependencyMacroCallBinding, string) {
		if name == "VALUE" {
			return configDependencyMacroCallBinding{state: configDependencyMacroDefined, definition: current}, ""
		}
		return configDependencyMacroCallBinding{state: configDependencyMacroUndefined}, ""
	}
	a, reason := configDependencyMacroCallExpand("VALUE", resolve, configDependencyMacroCallMode{})
	if reason != "" {
		t.Fatal(reason)
	}
	current = second // The external ordered state changes BETWEEN complete calls.
	b, reason := configDependencyMacroCallExpand("VALUE", resolve, configDependencyMacroCallMode{})
	if reason != "" || !slices.Equal(a.Tokens, []string{"1"}) || !slices.Equal(b.ConfigReads, []string{"CONFIG_CHANGED"}) || *first != before {
		t.Fatalf("state leaked or immutable definition mutated: %+v %+v %q", a, b, reason)
	}
	for _, d := range []*configDependencyMacroCallDefinition{first, second} {
		t.Run(d.origin, func(t *testing.T) {
			t.Parallel()
			resolve := func(name string) (configDependencyMacroCallBinding, string) {
				if name == "VALUE" {
					return configDependencyMacroCallBinding{state: configDependencyMacroDefined, definition: d}, ""
				}
				return configDependencyMacroCallBinding{state: configDependencyMacroUndefined}, ""
			}
			for range 25 {
				got, reason := configDependencyMacroCallExpand("VALUE VALUE", resolve, configDependencyMacroCallMode{})
				if reason != "" || got.DefinitionReads[len(got.DefinitionReads)-1].Origin != d.origin {
					t.Fatalf("%+v %q", got, reason)
				}
			}
		})
	}
}

func TestConfigDependencyMacroCallArgumentPrescanRawPasteAndRescan(t *testing.T) {
	for _, test := range []struct {
		name        string
		defs        []string
		input, want string
		reads       []string
	}{
		{"numeric literal", []string{"JOIN(a,b) a##b"}, "JOIN(1,UL)", "1UL", nil},
		{"raw parameter", []string{"JOIN(a,b) a##b", "VALUE 1"}, "JOIN(VALUE,UL)", "VALUEUL", nil},
		{"forwarded prescan", []string{"JOIN(a,b) a##b", "FWD(a,b) JOIN(a,b)", "VALUE 1"}, "FWD(VALUE,UL)", "1UL", nil},
		{"ordered parameters", []string{"JOIN(b,a) a##b"}, "JOIN(1,UL)", "UL1", nil},
		{"last actual token", []string{"JOIN(a,b) a##b", "CONFIG_HIDDEN 29"}, "JOIN(prefix CONFIG_,HIDDEN)", "prefix 29", []string{"CONFIG_HIDDEN"}},
		{"rescan object alias", []string{"JOIN(a,b) a##b", "READ_IT CONFIG_HIDDEN", "CONFIG_HIDDEN 29"}, "JOIN(READ_,IT)", "29", []string{"CONFIG_HIDDEN"}},
		{"callee supplied as actual", []string{"CALL(f) f(1,UL)", "JOIN(a,b) a##b"}, "CALL(JOIN)", "1UL", nil},
		{"unused argument not prescanned", []string{"DROP(x) 1", "EFFECT _Pragma(\"GCC poison bad\")"}, "DROP(EFFECT)", "1", nil},
		{"identical alternatives", []string{"JOIN(a,b) a##b", "JOIN(a,b) a##b"}, "JOIN(1,UL)", "1UL", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, err := configDependencyMacroCallFixtureCatalogForTest(test.defs...)
			if err != nil {
				t.Fatal(err)
			}
			got, err := configDependencyMacroCallFixtureExpandForTest(c, test.input)
			if err != nil || strings.Join(got.Tokens, " ") != test.want || !slices.Equal(got.ConfigReads, test.reads) {
				t.Fatalf("result=%+v error=%v, want %q reads=%v", got, err, test.want, test.reads)
			}
		})
	}
}

func TestConfigDependencyMacroCallEffectsAndUnknownsFailClosed(t *testing.T) {
	for _, test := range []struct {
		name          string
		defs          []string
		input, reason string
	}{
		{"direct builtin", []string{"JOIN(a,b) a##b"}, "JOIN(_Pr,agma)(\"GCC poison bad\")", "preprocessing effect"},
		{"alternate builtin", []string{"JOIN(a,b) a##b"}, "JOIN(__pr,agma)(x)", "preprocessing effect"},
		{"pasted alias", []string{"JOIN(a,b) a##b", "UL1 _Pragma(\"GCC poison bad\")"}, "JOIN(UL,1)", "preprocessing effect"},
		{"nested prefix", []string{"OUT(a,b) a##b", "JOIN(a,b) a##b", "PRE_effect JOIN(_Pr,agma)"}, "OUT(PRE_,effect)", "preprocessing effect"},
		{"prescanned effect", []string{"ID(x) x", "EFFECT _Pragma(\"GCC poison bad\")"}, "ID(EFFECT)", "preprocessing effect"},
		{"different alternatives", []string{"JOIN(a,b) a##b", "JOIN(b,a) a##b"}, "JOIN(1,UL)", "ambiguous definition"},
		{"alias across boundary", []string{"ALIAS JOIN", "JOIN(a,b) a##b"}, "ALIAS(1,UL)", "rescan boundary"},
		{"empty paste argument", []string{"JOIN(a,b) a##b"}, "JOIN(,x)", "placemarker"},
		{"invalid paste", []string{"JOIN(a,b) a##b"}, "JOIN(1,())", "invalid pasted token"},
		{"wrong arity", []string{"JOIN(a,b) a##b"}, "JOIN(1)", "argument arity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, err := configDependencyMacroCallFixtureCatalogForTest(test.defs...)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := configDependencyMacroCallFixtureExpandForTest(c, test.input); err == nil || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("result=%+v error=%v, want refusal %q", got, err, test.reason)
			}
		})
	}
}

func TestConfigDependencyMacroCallUnsupportedGrammarAndBudgets(t *testing.T) {
	for _, definition := range []string{"F(x,x) x", "F(x) #not_a_formal", "F(x,y,z) x##y##z", "F(x) x/* comment"} {
		if _, err := configDependencyMacroCallFixtureCatalogForTest(definition); err == nil {
			t.Fatalf("accepted unsupported definition %q", definition)
		}
	}
	for _, input := range []string{"L\"wide\"", "1'2", "<::x", "/* x", "x??=y", "x##y"} {
		if _, err := configDependencyMacroCallFixtureExpandForTest(configDependencyMacroCallFixtureCatalog{}, input); err == nil {
			t.Fatalf("accepted unsupported input %q", input)
		}
	}
	c, err := configDependencyMacroCallFixtureCatalogForTest("DUP(x) " + strings.Repeat("x ", 100))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := configDependencyMacroCallFixtureExpandForTest(c, "DUP("+strings.Repeat("t ", 100)+")"); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("missing bounded substitution refusal: %v", err)
	}
}

func TestConfigDependencyMacroCallVariadicStringification(t *testing.T) {
	for _, tc := range []struct {
		name        string
		defs        []string
		input, want string
		reads       []string
	}{
		{"raw fixed argument", []string{"RAW(x) #x", "CONFIG_A 7"}, "RAW(CONFIG_A)", `"CONFIG_A"`, nil},
		{"raw standard tail", []string{"RAW(...) #__VA_ARGS__", "CONFIG_A 7"}, "RAW(CONFIG_A)", `"CONFIG_A"`, nil},
		{"raw named tail", []string{"RAW(args...) #args", "CONFIG_A 7"}, "RAW(CONFIG_A)", `"CONFIG_A"`, nil},
		{"expanded named tail", []string{"RAW(args...) #args", "WRAP(args...) RAW(args)", "CONFIG_A 7"}, "WRAP(CONFIG_A)", `"7"`, []string{"CONFIG_A"}},
		{"expanded standard tail", []string{"RAW(...) #__VA_ARGS__", "WRAP(...) RAW(__VA_ARGS__)", "CONFIG_A 7"}, "WRAP(CONFIG_A)", `"7"`, []string{"CONFIG_A"}},
		{"adjacent punctuation", []string{"RAW(...) #__VA_ARGS__"}, "RAW(a+b)", `"a+b"`, nil},
		{"normalized spaces", []string{"RAW(...) #__VA_ARGS__"}, "RAW(  a  +\t b  )", `"a + b"`, nil},
		{"comma spacing", []string{"RAW(args...) #args"}, "RAW(a,b, c)", `"a,b, c"`, nil},
		{"empty tail", []string{"RAW(args...) #args"}, "RAW()", `""`, nil},
		{"escaped literal", []string{"RAW(args...) #args"}, `RAW("a\\b")`, `"\"a\\\\b\""`, nil},
		{"digraph spelling", []string{"RAW(args...) #args"}, "RAW(%: %:%: <: <%)", `"%: %:%: <: <%"`, nil},
		{"unexpanded effect", []string{"RAW(args...) #args"}, `RAW(_Pragma("bad"))`, `"_Pragma(\"bad\")"`, nil},
		{"comments become spaces", []string{"RAW(args...) #args"}, "RAW(a/**/+b)", `"a +b"`, nil},
		{"empty macro spacing", []string{"RAW(args...) #args", "WRAP(args...) RAW(args)", "EMPTY"}, "WRAP(a EMPTY+b)", `"a +b"`, nil},
		{"adjacent expansion", []string{"RAW(args...) #args", "WRAP(args...) RAW(args)", "M() x"}, "WRAP(M()y)", `"xy"`, nil},
		{"nested empty boundary", []string{"RAW(args...) #args", "WRAP(args...) RAW(args)", "foo bar", "bar EMPTY baz", "EMPTY"}, "WRAP([foo] EMPTY;)", `"[ baz] ;"`, nil},
		{"pasted argument", []string{"RAW(args...) #args", "WRAP(args...) RAW(args)", "P(a,b) a##b"}, "WRAP(P(con,fig))", `"config"`, nil},
		{"GNU raw tail spacing", []string{"RAW(args...) #args", "WRAP(args...) RAW(args)", "G(x,...) x,##__VA_ARGS__"}, "WRAP(G(0, a,b))", `"0, a,b"`, nil},
		{"GNU omitted tail string", []string{"RAW(args...) #args", "WRAP(args...) RAW(args)", "G(x,...) x,##__VA_ARGS__"}, "WRAP(G(0))", `"0"`, nil},
		{"GNU empty tail string", []string{"RAW(args...) #args", "WRAP(args...) RAW(args)", "G(x,...) x,##__VA_ARGS__"}, "WRAP(G(0,))", `"0,"`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog, err := configDependencyMacroCallFixtureCatalogForTest(tc.defs...)
			if err != nil {
				t.Fatal(err)
			}
			got, err := configDependencyMacroCallFixtureExpandForTest(catalog, tc.input)
			if err != nil || !slices.Equal(got.Tokens, []string{tc.want}) || !slices.Equal(got.ConfigReads, tc.reads) {
				t.Fatalf("got %+v %v; want literal %q and config reads %v", got, err, tc.want, tc.reads)
			}
		})
	}
}

func TestConfigDependencyMacroCallStringificationLimitsAndRawOperators(t *testing.T) {
	catalog, err := configDependencyMacroCallFixtureCatalogForTest("DUP(x) " + strings.Repeat("#x ", 200))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := configDependencyMacroCallFixtureExpandForTest(catalog, "CONFIG_BEFORE DUP("+strings.Repeat("a", 6000)+")"); err == nil ||
		!strings.Contains(err.Error(), "generated macro byte budget") || !reflect.DeepEqual(got, configDependencyMacroCallResult{}) {
		t.Fatalf("stringification allocation overflow published partial evidence: %+v %v", got, err)
	}
	for _, fixture := range []struct{ definition, input string }{
		{"P(a,b) a##b", "P(a##b,c)"},
		{"G(x,...) x,##__VA_ARGS__", "G(0,a##b)"},
	} {
		catalog, err := configDependencyMacroCallFixtureCatalogForTest(fixture.definition)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := configDependencyMacroCallFixtureExpandForTest(catalog, fixture.input); err == nil ||
			!strings.Contains(err.Error(), "argument paste token") || !reflect.DeepEqual(got, configDependencyMacroCallResult{}) {
			t.Fatalf("raw argument operator acquired replacement authority: %+v %v", got, err)
		}
	}
}

func TestConfigDependencyMacroCallVariadicExpansion(t *testing.T) {
	for _, tc := range []struct {
		name        string
		defs        []string
		input, want string
		reads       []string
	}{
		{"unused omitted tail", []string{"SECOND(a,b,...) b"}, "SECOND(junk,0)", "0", nil},
		{"unused populated tail", []string{"SECOND(a,b,...) b"}, "SECOND(junk,0,_Pragma(\"bad\"))", "0", nil},
		{"comma prescan dispatch", []string{"SECOND(a,b,...) b", "DISPATCH(x) SECOND(x 1,0)", "PAIR 0,"}, "DISPATCH(PAIR)", "1", nil},
		{"named unused tail", []string{"SECOND(a,b,tail...) b"}, "SECOND(junk,7,_Pragma(\"bad\"))", "7", nil},
		{"ordinary tail prescan", []string{"F(x,...) x, __VA_ARGS__"}, "F(0,F(1,2))", "0 , 1 , 2", nil},
		{"gnu omitted", []string{"F(x,...) x,##__VA_ARGS__"}, "F(0)", "0", nil},
		{"gnu explicit empty", []string{"F(x,...) x,##__VA_ARGS__"}, "F(0,)", "0 ,", nil},
		{"gnu populated", []string{"F(x,...) x,##__VA_ARGS__"}, "F(0,1,2)", "0 , 1 , 2", nil},
		{"gnu expanded empty", []string{"F(x,...) x,##__VA_ARGS__", "EMPTY"}, "F(0,EMPTY)", "0 ,", nil},
		{"gnu named", []string{"F(x,args...) x,##args"}, "F(0,1,2)", "0 , 1 , 2", nil},
		{"gnu config rescan", []string{"F(x,...) x,##__VA_ARGS__", "CONFIG_A 3"}, "F(0,CONFIG_A)", "0 , 3", []string{"CONFIG_A"}},
		{"gnu nested other callee", []string{"F(x,...) x,##__VA_ARGS__", "ID(x) x"}, "F(0,ID(2))", "0 , 2", nil},
		{"gnu callee supplied", []string{"F(x,...) x,##__VA_ARGS__", "CALL(f) f(1)", "ID(x) x"}, "F(0,CALL(ID))", "0 , 1", nil},
		{"only variadic populated", []string{"F(...) 0,##__VA_ARGS__"}, "F(1,2)", "0 , 1 , 2", nil},
		{"only named variadic", []string{"F(args...) 0,##args"}, "F(1,2)", "0 , 1 , 2", nil},
		{"only variadic expanded empty", []string{"F(...) 0,##__VA_ARGS__", "EMPTY"}, "F(EMPTY)", "0 ,", nil},
		{"only variadic empty pair", []string{"F(...) 0,##__VA_ARGS__"}, "F(,)", "0 , ,", nil},
		{"only variadic config rescan", []string{"F(...) 0,##__VA_ARGS__", "CONFIG_A 3"}, "F(CONFIG_A)", "0 , 3", []string{"CONFIG_A"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := configDependencyMacroCallFixtureCatalogForTest(tc.defs...)
			if err != nil {
				t.Fatal(err)
			}
			got, err := configDependencyMacroCallFixtureExpandForTest(c, tc.input)
			if err != nil || strings.Join(got.Tokens, " ") != tc.want || !slices.Equal(got.ConfigReads, tc.reads) {
				t.Fatalf("got %+v %v; want %q reads %v", got, err, tc.want, tc.reads)
			}
		})
	}
}

func TestConfigDependencyMacroCallVariadicFailsClosed(t *testing.T) {
	for _, def := range []string{
		"F(x,...,y) x", "F(x,args...,y) x",
		"F(x,x...) x", "F(x,__VA_ARGS__) x", "F(x,args...) __VA_ARGS__",
		"F(x,...) __VA_OPT__(x)", "F(x,...) x##__VA_ARGS__",
		"F(x,...) __VA_ARGS__##x", "F(x,...) x,##__VA_ARGS__##x",
		"F(x,...) #not_a_formal",
	} {
		if _, err := configDependencyMacroCallFixtureCatalogForTest(def); err == nil {
			t.Fatalf("accepted %q", def)
		}
	}
	for _, tc := range []struct {
		defs          []string
		input, reason string
	}{
		{[]string{"F(...) 0,##__VA_ARGS__"}, "F()", "dialect-dependent variadic comma deletion"},
		{[]string{"F(args...) 0,##args"}, "F(/**/)", "dialect-dependent variadic comma deletion"},
		{[]string{"F(x,...) x,##__VA_ARGS__"}, "F(0,_Pragma(\"bad\"))", "preprocessing effect"},
		{[]string{"F(x,...) x,##__VA_ARGS__", "JOIN(a,b) a##b"}, "F(0,JOIN(_Pr,agma))", "preprocessing effect"},
		{[]string{"F(x,...) x,##__VA_ARGS__", "OUT(a,b) a##b", "JOIN(a,b) a##b", "PRE_effect JOIN(__pr,agma)"}, "F(0,OUT(PRE_,effect))", "preprocessing effect"},
		{[]string{"F(x,...) x,##__VA_ARGS__", "F(x,args...) x,args"}, "F(0,1)", "ambiguous"},
		{[]string{"SECOND(a,b,...) b"}, "SECOND(1)", "arity"},
	} {
		c, err := configDependencyMacroCallFixtureCatalogForTest(tc.defs...)
		if err != nil {
			t.Fatal(err)
		}
		got, err := configDependencyMacroCallFixtureExpandForTest(c, tc.input)
		if err == nil || !strings.Contains(err.Error(), tc.reason) || !reflect.DeepEqual(got, configDependencyMacroCallResult{}) {
			t.Fatalf("%q: %+v %v, want %s and no partial certificate", tc.input, got, err, tc.reason)
		}
	}
}

func TestConfigDependencyMacroCallPastedConfigAndEffectRescanAcrossVariadics(t *testing.T) {
	c, err := configDependencyMacroCallFixtureCatalogForTest("FWD(a,b) JOIN(a,b)", "JOIN(a,b) a##b", "UL1 BUILD(CONFIG_,HIDDEN)", "BUILD(a,b) a##b", "CONFIG_HIDDEN 29", "WRAP(x,...) x,##__VA_ARGS__")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		input, want string
		reads       []string
	}{
		{"WRAP(0,FWD(1,UL))", "0 , 1UL", nil},
		{"WRAP(0,FWD(UL,1))", "0 , 29", []string{"CONFIG_HIDDEN"}},
	} {
		got, err := configDependencyMacroCallFixtureExpandForTest(c, tc.input)
		if err != nil || strings.Join(got.Tokens, " ") != tc.want || !slices.Equal(got.ConfigReads, tc.reads) {
			t.Fatalf("%+v %v", got, err)
		}
	}
}

func TestConfigDependencyMacroCallAssemblerPreprocessingTokenSubset(t *testing.T) {
	for _, tc := range []struct {
		input  string
		tokens []string
	}{
		{"$CONFIG_A", []string{"$", "CONFIG_A"}},
		{"\\reg", []string{"\\", "reg"}},
		{"1e+2 0x1p-2 1b .25", []string{"1e+2", "0x1p-2", "1b", ".25"}},
		{"x/* CONFIG_HIDDEN */y", []string{"x", "y"}},
		{"x// CONFIG_HIDDEN\ny", []string{"x", "y"}},
		{"%: %:%: <: :> <% %>", []string{"#", "##", "[", "]", "{", "}"}},
		{"a >>= 2 && b != 0", []string{"a", ">>=", "2", "&&", "b", "!=", "0"}},
	} {
		tokens, err := configDependencyMacroCallLex(tc.input, configDependencyMacroCallMode{dollarAsPunctuation: true})
		if err != nil {
			t.Fatal(err)
		}
		var actual []string
		for _, token := range tokens {
			actual = append(actual, token.text)
		}
		if !slices.Equal(actual, tc.tokens) {
			t.Fatalf("%q: %v", tc.input, actual)
		}
	}
	for _, bad := range []string{"<::_Pragma", "R\"raw(a)raw\"", "u8\"x\"", "'unterminated", "é", "a\\\nb"} {
		if _, err := configDependencyMacroCallLex(bad, configDependencyMacroCallMode{dollarAsPunctuation: true}); err == nil {
			t.Fatalf("accepted unknown grammar %q", bad)
		}
	}
	for _, def := range []string{"F(x) %:not_a_formal", "F(a,b,c) a %:%: b %:%: c"} {
		if _, err := configDependencyMacroCallFixtureCatalogForTest(def); err == nil {
			t.Fatalf("accepted unsupported digraph effect %q", def)
		}
	}
}
