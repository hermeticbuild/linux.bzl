package kconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestProbeRequestDigestMatchesCanonicalBytes(t *testing.T) {
	for _, text := range []string{"", "hello\n", "<>&\"\\\t\r\n", "é\u2028\u2029", strings.Repeat("#if defined(SOURCE_HEADER_GUARD)\n1\n#else\n0\n#endif\n", 16000)} {
		request := testProbeRequest()
		request.Steps[0].Stdin = text
		request.Steps[0].Environment = map[string]string{"Z_LAST": "value", "A_FIRST": "<>&\""}
		data, err := request.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		want := sha256.Sum256(data)
		got, err := request.ID()
		if err != nil || got != hex.EncodeToString(want[:]) {
			t.Fatalf("digest of %d input bytes = %q, %v; want exact canonical digest %x", len(text), got, err, want)
		}
		request.Steps[0].Stdin += "changed"
		changed, err := request.ID()
		if err != nil || got == changed {
			t.Fatalf("mutation reused identity: %q, %v", changed, err)
		}
	}
	invalid := testProbeRequest()
	invalid.Schema = "invalid"
	if got, err := invalid.ID(); err == nil || got != "" {
		t.Fatalf("invalid request got identity %q: %v", got, err)
	}
}

func TestProbeTemplateLiteralDoesNotCopyProgram(t *testing.T) {
	literal := strings.Repeat("#if defined(SOURCE_HEADER_GUARD)\n1\n#else\n0\n#endif\n", 16000)
	if allocations := testing.AllocsPerRun(10, func() {
		if err := validateProbeTemplate(literal, nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}); allocations != 0 {
		t.Fatalf("literal template copied its program: %.0f allocations", allocations)
	}
	for _, suffix := range []string{"${", "${unsupported:x}", "${scratch:missing}", "${result:00000000.text}"} {
		if err := validateProbeTemplate(literal+suffix, nil, nil, nil, 0); err == nil {
			t.Fatalf("literal prefix hid invalid template suffix %q", suffix)
		}
	}
	if err := validateProbeTemplate(literal+"${scratch:present}", map[string]bool{"present": true}, nil, nil, 0); err != nil {
		t.Fatalf("valid template suffix rejected: %v", err)
	}
}

func BenchmarkProbeRequestIdentity(b *testing.B) {
	names := make([]string, 16000)
	for i := range names {
		names[i] = fmt.Sprintf("SOURCE_HEADER_GUARD_%05d", i)
	}
	_, source, err := compilerDefinednessSource(names)
	if err != nil {
		b.Fatal(err)
	}
	request := testProbeRequest()
	request.Steps[0].Stdin = source
	for _, buffered := range []bool{true, false} {
		b.Run(fmt.Sprintf("canonical_copy_%t", buffered), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(source)))
			for b.Loop() {
				if buffered {
					data, err := request.CanonicalJSON()
					if err != nil {
						b.Fatal(err)
					}
					digest := sha256.Sum256(data)
					_ = hex.EncodeToString(digest[:])
				} else if _, err := request.ID(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func testProbeRequest() ProbeRequest {
	return ProbeRequest{
		Schema:  LinuxProbeRequestSchema,
		Scratch: []ProbeScratch{{Name: "object", Kind: "file"}},
		Steps: []ProbeStep{{
			Name: "compile", Tool: "cc",
			Arguments: []string{"-c", "-x", "c", "-", "-o", "${scratch:object}"},
			Stdin:     "int value;\n",
		}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "all", Operands: []ProbePredicate{
			{Operator: "exit-zero", Step: "compile"},
			{Operator: "regular-file", Scratch: "object"},
		}}},
	}
}

func TestProbeRequestCanonicalIdentity(t *testing.T) {
	if LinuxProbeRequestSchema != "linux-probe-request-v13" {
		t.Fatalf("probe request schema = %q, want v13", LinuxProbeRequestSchema)
	}
	request := testProbeRequest()
	id, err := request.ID()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 64 {
		t.Fatalf("ID = %q", id)
	}
	data, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(filename, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadProbeRequest(filename)
	if err != nil {
		t.Fatal(err)
	}
	gotID, _ := got.ID()
	if gotID != id {
		t.Fatalf("round-trip ID = %s, want %s", gotID, id)
	}
	if err := os.WriteFile(filename, append([]byte(" "), data...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadProbeRequest(filename); err == nil || !strings.Contains(err.Error(), "canonically") {
		t.Fatalf("noncanonical request error = %v", err)
	}
}

func TestProbeStepStdoutFallbackPathValidation(t *testing.T) {
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema,
		Steps: []ProbeStep{{
			Name: "query", Tool: "cc", StdoutExecrootRelative: true,
			StdoutFallbackPath: "plugin",
		}},
		Outcome: ProbeOutcome{Kind: "text", Step: "query", Stream: "stdout", TrimSpace: true},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid stdout fallback: %v", err)
	}
	withoutPathContract := request
	withoutPathContract.Steps = slices.Clone(request.Steps)
	withoutPathContract.Steps[0].StdoutExecrootRelative = false
	if err := withoutPathContract.Validate(); err == nil || !strings.Contains(err.Error(), "requires execroot-relative stdout") {
		t.Fatalf("fallback without path contract error = %v", err)
	}
	unsafe := request
	unsafe.Steps = slices.Clone(request.Steps)
	unsafe.Steps[0].StdoutFallbackPath = "../plugin"
	if err := unsafe.Validate(); err == nil || !strings.Contains(err.Error(), "safe path component") {
		t.Fatalf("unsafe stdout fallback error = %v", err)
	}
	indirect := request
	indirect.Outcome = ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: "query"}}
	if err := indirect.Validate(); err == nil || !strings.Contains(err.Error(), "direct text stdout outcome") {
		t.Fatalf("indirect stdout path outcome error = %v", err)
	}
}

func TestProbePlanWritesPathEncodedDAG(t *testing.T) {
	request := testProbeRequest()
	request.Sources = []string{"foo", "foo/path", "scripts/δ.sh"}
	request.SourceRoots = []string{"linux"}
	requestID, _ := request.ID()
	first := ProbePlanNode{Scope: "target", RequestID: requestID}
	first.ID = first.ContentID()
	dependent := request
	dependent.InputCount = 1
	dependentID, _ := dependent.ID()
	second := ProbePlanNode{Scope: "target", RequestID: dependentID, Inputs: []string{first.ID}}
	second.ID = second.ContentID()
	identity := "sha256-" + strings.Repeat("a", 64)
	plan := ProbePlan{
		Toolsets: map[string]string{"target": identity},
		Requests: map[string]ProbeRequest{requestID: request, dependentID: dependent},
		Nodes:    []ProbePlanNode{first, second}, Terminal: []string{second.ID},
	}
	root := filepath.Join(t.TempDir(), "plan")
	if err := plan.Write(root); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{
		"schema/" + LinuxProbePlanSchema,
		"requests/" + requestID + ".json",
		"nodes/" + first.ID + "/tool/cc",
		"nodes/" + first.ID + "/source/+foo/path",
		"nodes/" + first.ID + "/source/+foo/+path/path",
		"nodes/" + first.ID + "/source/+scripts/+δ.sh/path",
		"nodes/" + first.ID + "/source_root/linux",
		"nodes/" + second.ID + "/in/00000000/" + first.ID,
		"terminal/" + second.ID,
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(relative))); err != nil {
			t.Errorf("missing %s: %v", relative, err)
		}
	}
}

func testProbePlanUnionVariants(t *testing.T, identity string) (*ProbePlan, *ProbePlan) {
	t.Helper()
	build := func(uniqueSource string) *ProbePlan {
		builder, err := NewProbePlanBuilder(identity, "")
		if err != nil {
			t.Fatal(err)
		}
		common, err := builder.Request("target", testProbeRequest())
		if err != nil {
			t.Fatal(err)
		}
		uniqueRequest := testProbeRequest()
		uniqueRequest.InputCount = 1
		uniqueRequest.Steps[0].Stdin = uniqueSource
		unique, err := builder.Request("target", uniqueRequest, common)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := builder.Plan(unique)
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}
	return build("int left;\n"), build("int right;\n")
}

func TestReadProbePlanRoundTripsAndRejectsNoncanonicalTrees(t *testing.T) {
	identity := "sha256-" + strings.Repeat("4", 64)
	plan, _ := testProbePlanUnionVariants(t, identity)
	write := func(t *testing.T) string {
		t.Helper()
		root := filepath.Join(t.TempDir(), "plan")
		if err := plan.Write(root); err != nil {
			t.Fatal(err)
		}
		return root
	}

	root := write(t)
	roundTrip, err := ReadProbePlan(root)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip.Toolsets["target"] != identity || len(roundTrip.Requests) != 2 || len(roundTrip.Nodes) != 2 || len(roundTrip.Terminal) != 1 {
		t.Fatalf("round-trip probe plan = %#v", roundTrip)
	}
	if !slices.IsSortedFunc(roundTrip.Nodes, func(left, right ProbePlanNode) int {
		return strings.Compare(left.ID, right.ID)
	}) || !slices.IsSorted(roundTrip.Terminal) {
		t.Fatalf("round-trip probe plan is not deterministic: nodes=%#v terminals=%q", roundTrip.Nodes, roundTrip.Terminal)
	}
	originalEntries, err := plan.entries()
	if err != nil {
		t.Fatal(err)
	}
	roundTripEntries, err := roundTrip.entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(originalEntries) != len(roundTripEntries) {
		t.Fatalf("round-trip marker count = %d, want %d", len(roundTripEntries), len(originalEntries))
	}
	for index := range originalEntries {
		if originalEntries[index].path != roundTripEntries[index].path || !slices.Equal(originalEntries[index].data, roundTripEntries[index].data) {
			t.Fatalf("round-trip marker %d = %#v, want %#v", index, roundTripEntries[index], originalEntries[index])
		}
	}

	requestID := plan.Nodes[0].RequestID
	nodeID := plan.Nodes[0].ID
	for name, mutate := range map[string]func(*testing.T, string){
		"unknown file": func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "unexpected"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"empty directory": func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, "unexpected"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"missing derived marker": func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "nodes", nodeID, "tool", "cc")); err != nil {
				t.Fatal(err)
			}
		},
		"noncanonical request": func(t *testing.T, root string) {
			filename := filepath.Join(root, "requests", requestID+".json")
			data, err := os.ReadFile(filename)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filename, append([]byte(" "), data...), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"nonempty marker": func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "terminal", plan.Terminal[0]), []byte("unexpected"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, root string) {
			if err := os.Symlink(filepath.Join("schema", LinuxProbePlanSchema), filepath.Join(root, "unexpected-link")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := write(t)
			mutate(t, root)
			if _, err := ReadProbePlan(root); err == nil {
				t.Fatal("noncanonical probe plan was accepted")
			}
		})
	}
}

func TestMergeProbePlansIsDeterministicAndSupportsPerVariantReplay(t *testing.T) {
	identity := "sha256-" + strings.Repeat("5", 64)
	left, right := testProbePlanUnionVariants(t, identity)
	forward, err := MergeProbePlans([]ProbePlanVariant{{Name: "left", Plan: left}, {Name: "right", Plan: right}})
	if err != nil {
		t.Fatal(err)
	}
	reverse, err := MergeProbePlans([]ProbePlanVariant{{Name: "right", Plan: right}, {Name: "left", Plan: left}})
	if err != nil {
		t.Fatal(err)
	}
	forwardEntries, _ := forward.entries()
	reverseEntries, _ := reverse.entries()
	if len(forwardEntries) != len(reverseEntries) {
		t.Fatalf("reverse union has %d markers, want %d", len(reverseEntries), len(forwardEntries))
	}
	for index := range forwardEntries {
		if forwardEntries[index].path != reverseEntries[index].path || !slices.Equal(forwardEntries[index].data, reverseEntries[index].data) {
			t.Fatalf("reverse union marker %d differs: %#v != %#v", index, reverseEntries[index], forwardEntries[index])
		}
	}
	if len(forward.Nodes) != 3 || len(forward.Requests) != 3 || len(forward.Terminal) != 2 {
		t.Fatalf("merged plan = %d nodes, %d requests, %d terminals; want 3, 3, 2", len(forward.Nodes), len(forward.Requests), len(forward.Terminal))
	}
	if !slices.IsSortedFunc(forward.Nodes, func(left, right ProbePlanNode) int {
		return strings.Compare(left.ID, right.ID)
	}) || !slices.IsSorted(forward.Terminal) {
		t.Fatalf("merged plan is not canonically ordered: nodes=%#v terminals=%q", forward.Nodes, forward.Terminal)
	}

	value := true
	oracle := &ProbeResultOracle{results: map[string]ProbeResult{}, toolsets: maps.Clone(forward.Toolsets)}
	for _, node := range forward.Nodes {
		oracle.results[node.ID] = ProbeResult{
			Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
			Scope: node.Scope, ToolsetIdentity: identity, Kind: "boolean", Boolean: &value,
		}
	}
	for name, variant := range map[string]*ProbePlan{"left": left, "right": right} {
		if err := oracle.ValidatePlan(variant); err != nil {
			t.Errorf("%s replay rejected merged result superset: %v", name, err)
		}
	}
	delete(oracle.results, left.Terminal[0])
	if err := oracle.ValidatePlan(left); err == nil || !strings.Contains(err.Error(), "missing result") {
		t.Fatalf("replay with missing demanded result error = %v", err)
	}
}

func TestMergeProbePlansRejectsInvalidMembersAndToolsetMismatch(t *testing.T) {
	identity := "sha256-" + strings.Repeat("6", 64)
	left, right := testProbePlanUnionVariants(t, identity)
	if _, err := MergeProbePlans(nil); err == nil || !strings.Contains(err.Error(), "no variants") {
		t.Fatalf("empty union error = %v", err)
	}
	if _, err := MergeProbePlans([]ProbePlanVariant{{Name: "same", Plan: left}, {Name: "same", Plan: right}}); err == nil || !strings.Contains(err.Error(), "repeats variant") {
		t.Fatalf("duplicate variant error = %v", err)
	}

	mismatched := *right
	mismatched.Toolsets = maps.Clone(right.Toolsets)
	mismatched.Toolsets["target"] = "sha256-" + strings.Repeat("7", 64)
	if _, err := MergeProbePlans([]ProbePlanVariant{{Name: "left", Plan: left}, {Name: "right", Plan: &mismatched}}); err == nil || !strings.Contains(err.Error(), "right") || !strings.Contains(err.Error(), "target toolset") {
		t.Fatalf("toolset mismatch error = %v", err)
	}

	invalid := *left
	invalid.Nodes = slices.Clone(left.Nodes)
	invalid.Nodes[0].ID = strings.Repeat("8", 64)
	if _, err := MergeProbePlans([]ProbePlanVariant{{Name: "broken", Plan: &invalid}}); err == nil || !strings.Contains(err.Error(), "broken") || !strings.Contains(err.Error(), "does not match canonical content") {
		t.Fatalf("invalid member error = %v", err)
	}
}

func TestProbePlanSourceMarkerRoundTrip(t *testing.T) {
	for _, source := range []string{"Kconfig", "foo/path", "+leading/δ file"} {
		marker := probePlanSourceMarker("nodes/id", source)
		parts := strings.Split(marker, "/")
		if len(parts) < 5 || parts[0] != "nodes" || parts[1] != "id" || parts[2] != "source" || parts[len(parts)-1] != "path" {
			t.Fatalf("source marker %q has invalid envelope", marker)
		}
		decoded := make([]string, 0, len(parts)-4)
		for _, component := range parts[3 : len(parts)-1] {
			if !strings.HasPrefix(component, "+") {
				t.Fatalf("source marker %q has unescaped component %q", marker, component)
			}
			decoded = append(decoded, strings.TrimPrefix(component, "+"))
		}
		if got := strings.Join(decoded, "/"); got != source {
			t.Fatalf("source marker %q decodes to %q, want %q", marker, got, source)
		}
	}
	if first, second := probePlanSourceMarker("nodes/id", "foo"), probePlanSourceMarker("nodes/id", "foo/path"); first == second || strings.HasPrefix(second, first+"/") {
		t.Fatalf("prefix sources collide: %q and %q", first, second)
	}
}

func TestProbeRequestRejectsForwardConditionAndUnknownPlaceholder(t *testing.T) {
	request := testProbeRequest()
	request.Steps[0].When = &ProbePredicate{Operator: "exit-zero", Step: "later"}
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "unavailable step") {
		t.Fatalf("forward condition error = %v", err)
	}
	request = testProbeRequest()
	request.Steps[0].Arguments = append(request.Steps[0].Arguments, "${unknown:x}")
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "unsupported placeholder") {
		t.Fatalf("placeholder error = %v", err)
	}
}

func TestProbeRequestValidatesConditionalEnvironmentAndStdinFragments(t *testing.T) {
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: 1,
		Steps: []ProbeStep{{
			Name: "consume", Tool: "cc",
			Environment: map[string]string{"STATIC": "literal"},
			EnvironmentFragments: []ProbeEnvironmentFragments{{
				Name: "SELECTED",
				Fragments: []ProbeValueFragment{
					{Value: "prefix-"},
					{Value: "yes", When: &ProbePredicate{Operator: "result-true", Result: "00000000"}},
					{Value: "no", When: &ProbePredicate{Operator: "result-false", Result: "00000000"}},
				},
			}},
			StdinFragments: []ProbeValueFragment{
				{Value: "value="},
				{Value: "${result:00000000.boolean}"},
			},
		}},
		Outcome: ProbeOutcome{Kind: "text", Step: "consume", Stream: "stdout"},
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"environment_fragments"`) || !strings.Contains(string(data), `"stdin_fragments"`) {
		t.Fatalf("canonical request omits fragments: %s", data)
	}

	for name, mutate := range map[string]func(*ProbeRequest){
		"literal overlap": func(value *ProbeRequest) {
			value.Steps[0].Environment["SELECTED"] = "duplicate"
		},
		"unsorted environment": func(value *ProbeRequest) {
			value.Steps[0].EnvironmentFragments = append([]ProbeEnvironmentFragments{{Name: "Z", Fragments: []ProbeValueFragment{{Value: "z"}}}}, value.Steps[0].EnvironmentFragments...)
		},
		"literal stdin overlap": func(value *ProbeRequest) {
			value.Steps[0].Stdin = "duplicate"
		},
		"unknown result": func(value *ProbeRequest) {
			value.Steps[0].StdinFragments[0].When = &ProbePredicate{Operator: "result-true", Result: "00000001"}
		},
		"malformed template": func(value *ProbeRequest) {
			value.Steps[0].EnvironmentFragments[0].Fragments[0].Value = "${result:broken}"
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := request
			candidate.Steps = slices.Clone(request.Steps)
			candidate.Steps[0].Environment = maps.Clone(request.Steps[0].Environment)
			candidate.Steps[0].EnvironmentFragments = slices.Clone(request.Steps[0].EnvironmentFragments)
			for index := range candidate.Steps[0].EnvironmentFragments {
				candidate.Steps[0].EnvironmentFragments[index].Fragments = slices.Clone(candidate.Steps[0].EnvironmentFragments[index].Fragments)
			}
			candidate.Steps[0].StdinFragments = slices.Clone(request.Steps[0].StdinFragments)
			mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("invalid fragment request was accepted")
			}
		})
	}
}

func TestProbeRequestValidatesNestedAggregateFragmentsRecursively(t *testing.T) {
	aggregate := func(value string) ProbeValueFragment {
		return ProbeValueFragment{
			Fragments:  []ProbeValueFragment{{Value: value}},
			Transforms: []ProbeValueTransform{{Function: "strip", Arguments: []string{""}, InputArgument: 0}},
		}
	}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: 1,
		Steps: []ProbeStep{{
			Name: "consume", Tool: "cc", Arguments: []string{""},
			ArgumentFragments:    []ProbeArgumentFragments{{Index: 0, Fragments: []ProbeValueFragment{aggregate("${tool:ld}")}}},
			EnvironmentFragments: []ProbeEnvironmentFragments{{Name: "SELECTED", Fragments: []ProbeValueFragment{aggregate("${result:00000000.text}")}}},
			StdinFragments:       []ProbeValueFragment{aggregate("stdin")},
		}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: "consume"}},
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	if got, want := request.ToolRoles(), []string{"cc", "ld"}; !slices.Equal(got, want) {
		t.Fatalf("nested aggregate tool roles = %q, want %q", got, want)
	}

	for _, test := range []struct {
		name string
		edit func(*ProbeRequest)
		want string
	}{
		{
			name: "value and aggregate",
			edit: func(value *ProbeRequest) {
				value.Steps[0].EnvironmentFragments[0].Fragments[0].Value = "also-literal"
			},
			want: "exactly one nonempty value or aggregate",
		},
		{
			name: "nested unavailable result",
			edit: func(value *ProbeRequest) {
				value.Steps[0].EnvironmentFragments[0].Fragments[0].Fragments[0].Value = "${result:00000001.text}"
			},
			want: "references unavailable result",
		},
		{
			name: "aggregate depth",
			edit: func(value *ProbeRequest) {
				leaf := ProbeValueFragment{Value: "leaf"}
				for range MaxProbeValueFragmentDepth + 1 {
					leaf = ProbeValueFragment{Fragments: []ProbeValueFragment{leaf}}
				}
				value.Steps[0].StdinFragments = []ProbeValueFragment{leaf}
			},
			want: "aggregate depth",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var cloneFragments func([]ProbeValueFragment) []ProbeValueFragment
			cloneFragments = func(values []ProbeValueFragment) []ProbeValueFragment {
				values = slices.Clone(values)
				for index := range values {
					values[index].Fragments = cloneFragments(values[index].Fragments)
					values[index].Transforms = slices.Clone(values[index].Transforms)
				}
				return values
			}
			candidate := request
			candidate.Steps = slices.Clone(request.Steps)
			candidate.Steps[0].ArgumentFragments = slices.Clone(request.Steps[0].ArgumentFragments)
			for index := range candidate.Steps[0].ArgumentFragments {
				candidate.Steps[0].ArgumentFragments[index].Fragments = cloneFragments(candidate.Steps[0].ArgumentFragments[index].Fragments)
			}
			candidate.Steps[0].EnvironmentFragments = slices.Clone(request.Steps[0].EnvironmentFragments)
			for index := range candidate.Steps[0].EnvironmentFragments {
				candidate.Steps[0].EnvironmentFragments[index].Fragments = cloneFragments(candidate.Steps[0].EnvironmentFragments[index].Fragments)
			}
			candidate.Steps[0].StdinFragments = cloneFragments(request.Steps[0].StdinFragments)
			test.edit(&candidate)
			if err := candidate.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestProbeRequestValidatesDynamicTransformArgumentsRecursively(t *testing.T) {
	newRequest := func() ProbeRequest {
		return ProbeRequest{
			Schema: LinuxProbeRequestSchema, InputCount: 1,
			Steps: []ProbeStep{{
				Name: "consume", Tool: "cc", Arguments: []string{""},
				ArgumentFragments: []ProbeArgumentFragments{{Index: 0, Fragments: []ProbeValueFragment{{
					Fragments: []ProbeValueFragment{{Value: "${result:00000000.text}"}},
					Transforms: []ProbeValueTransform{{
						Function: "filter-out", Arguments: []string{"", ""}, InputArgument: 1,
						ArgumentFragments: []ProbeValueTransformArgumentFragments{{
							Index: 0, Fragments: []ProbeValueFragment{{Value: "${tool:ld}"}},
						}},
					}},
				}}}},
			}},
			Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: "consume"}},
		}
	}
	request := newRequest()
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	if got, want := request.ToolRoles(), []string{"cc", "ld"}; !slices.Equal(got, want) {
		t.Fatalf("dynamic transform argument tool roles = %q, want %q", got, want)
	}

	for _, test := range []struct {
		name string
		edit func(*ProbeValueTransform)
		want string
	}{
		{
			name: "input argument",
			edit: func(transform *ProbeValueTransform) { transform.ArgumentFragments[0].Index = 1 },
			want: "replaces the transform input",
		},
		{
			name: "out of range argument",
			edit: func(transform *ProbeValueTransform) { transform.ArgumentFragments[0].Index = 2 },
			want: "invalid or unsorted index",
		},
		{
			name: "unsorted arguments",
			edit: func(transform *ProbeValueTransform) {
				transform.Function = "patsubst"
				transform.Arguments = []string{"", "", ""}
				transform.InputArgument = 2
				transform.ArgumentFragments = []ProbeValueTransformArgumentFragments{
					{Index: 1, Fragments: []ProbeValueFragment{{Value: "replacement"}}},
					{Index: 0, Fragments: []ProbeValueFragment{{Value: "pattern"}}},
				}
			},
			want: "invalid or unsorted index",
		},
		{
			name: "nonempty backing argument",
			edit: func(transform *ProbeValueTransform) { transform.Arguments[0] = "literal" },
			want: "must replace one empty argument",
		},
		{
			name: "empty fragments",
			edit: func(transform *ProbeValueTransform) { transform.ArgumentFragments[0].Fragments = nil },
			want: "must replace one empty argument",
		},
		{
			name: "unavailable nested result",
			edit: func(transform *ProbeValueTransform) {
				transform.ArgumentFragments[0].Fragments[0].Value = "${result:00000001.text}"
			},
			want: "references unavailable result",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := newRequest()
			transform := &candidate.Steps[0].ArgumentFragments[0].Fragments[0].Transforms[0]
			test.edit(transform)
			if err := candidate.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.want)
			}
		})
	}

	deep := newRequest()
	leaf := ProbeValueFragment{Value: "leaf"}
	for range MaxProbeValueFragmentDepth + 1 {
		leaf = ProbeValueFragment{Fragments: []ProbeValueFragment{leaf}}
	}
	deep.Steps[0].ArgumentFragments[0].Fragments[0].Transforms[0].ArgumentFragments[0].Fragments = []ProbeValueFragment{leaf}
	if err := deep.Validate(); err == nil || !strings.Contains(err.Error(), "aggregate depth") {
		t.Fatalf("dynamic transform argument depth error = %v", err)
	}
}

func TestProbeOutcomeLastLineIsAValidatedTextOnlyReduction(t *testing.T) {
	request := ProbeRequest{
		Schema:  LinuxProbeRequestSchema,
		Steps:   []ProbeStep{{Name: "preprocess", Tool: "cc"}},
		Outcome: ProbeOutcome{Kind: "text", Step: "preprocess", Stream: "stdout", TrimSpace: true, LastLine: true},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid last-line text outcome: %v", err)
	}
	request.Outcome.FirstLine = true
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "both first and last line") {
		t.Fatalf("first+last line validation error = %v", err)
	}
	request.Outcome = ProbeOutcome{
		Kind: "boolean", LastLine: true,
		Predicate: &ProbePredicate{Operator: "exit-zero", Step: "preprocess"},
	}
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "only predicate") {
		t.Fatalf("boolean last-line validation error = %v", err)
	}
}

func TestProbeRequestValidatesDeclaredSourcesRootsAndAuxiliaryTools(t *testing.T) {
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema,
		Sources: []string{
			"Kconfig",
			"scripts/compiler probe.sh",
		},
		SourceRoots: []string{"linux"},
		Steps: []ProbeStep{{
			Name:             "script",
			Tool:             "runner",
			AuxiliaryTools:   []string{"cc", "ld"},
			WorkingDirectory: "${source_root:linux}",
			Arguments: []string{
				"${source:scripts/compiler probe.sh}",
				"${tool:cc}",
				"${tool:ld}",
			},
		}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: "script"}},
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	request.Steps[0].WorkingDirectory = "${source_root:linux}/scripts"
	if err := request.Validate(); err != nil {
		t.Fatalf("valid source-root subdirectory working directory: %v", err)
	}
	request.Steps[0].WorkingDirectory = "${source_root:linux}"
	if got := strings.Join(request.ToolRoles(), ","); got != "cc,ld,runner" {
		t.Fatalf("ToolRoles() = %q; source placeholders leaked into tool roles", got)
	}

	for name, mutate := range map[string]func(*ProbeRequest){
		"unlisted source": func(value *ProbeRequest) {
			value.Steps[0].Arguments[0] = "${source:scripts/unlisted.sh}"
		},
		"unlisted source root": func(value *ProbeRequest) {
			value.Steps[0].WorkingDirectory = "${source_root:other}"
		},
		"escaping working directory": func(value *ProbeRequest) {
			value.Steps[0].WorkingDirectory = "${source_root:linux}/../subdir"
		},
		"second working directory root": func(value *ProbeRequest) {
			value.Steps[0].WorkingDirectory = "${source_root:linux}/${source_root:linux}"
		},
		"unsorted auxiliaries": func(value *ProbeRequest) {
			value.Steps[0].AuxiliaryTools = []string{"ld", "cc"}
		},
		"primary auxiliary": func(value *ProbeRequest) {
			value.Steps[0].AuxiliaryTools = []string{"runner"}
			value.Steps[0].Arguments[1] = "${tool:runner}"
		},
		"unreferenced auxiliary": func(value *ProbeRequest) {
			value.Steps[0].Arguments = value.Steps[0].Arguments[:2]
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := request
			candidate.Sources = append([]string(nil), request.Sources...)
			candidate.SourceRoots = append([]string(nil), request.SourceRoots...)
			candidate.Steps = append([]ProbeStep(nil), request.Steps...)
			candidate.Steps[0].Arguments = append([]string(nil), request.Steps[0].Arguments...)
			candidate.Steps[0].AuxiliaryTools = append([]string(nil), request.Steps[0].AuxiliaryTools...)
			mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("invalid request was accepted")
			}
		})
	}
}

func TestProbeRequestValidatesScratchWorkingDirectory(t *testing.T) {
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema,
		Scratch: []ProbeScratch{
			{Name: "file", Kind: "file"},
			{Name: "work", Kind: "directory"},
		},
		Steps: []ProbeStep{{
			Name: "inspect", Tool: "runner", WorkingDirectory: "${scratch:work}",
		}},
		Outcome: ProbeOutcome{
			Kind:      "boolean",
			Predicate: &ProbePredicate{Operator: "exit-zero", Step: "inspect"},
		},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid scratch working directory: %v", err)
	}

	for _, test := range []struct {
		name, workingDirectory, want string
	}{
		{name: "file scratch", workingDirectory: "${scratch:file}", want: "is not a directory"},
		{name: "undeclared scratch", workingDirectory: "${scratch:other}", want: "undeclared scratch"},
		{name: "suffix", workingDirectory: "${scratch:work}/nested", want: "exactly one"},
		{name: "mixed placeholders", workingDirectory: "${scratch:work}${source_root:linux}", want: "exactly one"},
		{name: "embedded scratch", workingDirectory: "prefix/${scratch:work}", want: "must start"},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := request
			candidate.Steps = append([]ProbeStep(nil), request.Steps...)
			candidate.Steps[0].WorkingDirectory = test.workingDirectory
			if err := candidate.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("working directory %q error = %v; want substring %q", test.workingDirectory, err, test.want)
			}
		})
	}
}

func TestProbeRequestRejectsUnsafeOrUnboundedSourcePaths(t *testing.T) {
	for _, source := range []string{
		"",
		"/absolute",
		"../escape",
		"directory/../../escape",
		"directory\\file",
		"directory//file",
		"directory/./file",
		"nul\x00file",
		strings.Repeat("x", maxProbeSourceComponent+1),
	} {
		request := testProbeRequest()
		request.Sources = []string{source}
		if err := request.Validate(); err == nil {
			t.Errorf("unsafe source %q was accepted", source)
		}
	}
	request := testProbeRequest()
	request.Sources = []string{"z", "a"}
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "strictly sorted") {
		t.Fatalf("unsorted sources error = %v", err)
	}
	request = testProbeRequest()
	request.Sources = make([]string, maxProbeSources+1)
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("unbounded sources error = %v", err)
	}
}

func TestProbeRequestAllowsPureDependentReduction(t *testing.T) {
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: 1,
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{
			Operator: "result-text-empty", Result: "00000000",
		}},
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	request.InputCount = 0
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "neither steps nor inputs") {
		t.Fatalf("root request without a process was accepted: %v", err)
	}
}

func TestProbeRequestValidatesInlineScratchAndStreamMatchPredicate(t *testing.T) {
	request := ProbeRequest{
		Schema:  LinuxProbeRequestSchema,
		Scratch: []ProbeScratch{{Name: "header", Kind: "file", Content: "#define VALUE 1\n"}},
		Steps:   []ProbeStep{{Name: "inspect", Tool: "tool", Arguments: []string{"${scratch:header}"}}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{
			Operator: "stream-matches", Step: "inspect", Stream: "stdout", Value: `(?m)^tool available$`,
		}},
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := request.CanonicalJSON()
	if err != nil || !strings.Contains(string(data), `"content":"#define VALUE 1\n"`) {
		t.Fatalf("canonical inline scratch = %s, %v", data, err)
	}
	request.Scratch[0].Kind = "directory"
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "has content") {
		t.Fatalf("directory content error = %v", err)
	}
	request.Scratch[0].Kind = "file"
	request.Outcome.Predicate.Value = `([0-9]+`
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "pattern is invalid") {
		t.Fatalf("invalid stream match error = %v", err)
	}
}

func TestNormalizeProbeExecrootRelativePath(t *testing.T) {
	execroot := filepath.Join(t.TempDir(), "execroot")
	if err := os.MkdirAll(filepath.Join(execroot, "external", "toolchain"), 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(execroot, "external", "toolchain", "include")
	for name, input := range map[string]string{
		"absolute": inside + "\n",
		"relative": "external/toolchain/include\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := NormalizeProbeExecrootRelativePath(execroot, filepath.Join(execroot, "bin", "cc"), input)
			if err != nil || got != "external/toolchain/include" {
				t.Fatalf("NormalizeProbeExecrootRelativePath() = %q, %v", got, err)
			}
			if err := ValidateProbeExecrootRelativePath(got); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, input := range []string{
		filepath.Join(filepath.Dir(execroot), "outside"),
		"../outside",
		"/outside",
		".",
		"",
	} {
		if _, err := NormalizeProbeExecrootRelativePath(execroot, filepath.Join(execroot, "bin", "cc"), input); err == nil {
			t.Errorf("unsafe path %q was normalized", input)
		}
	}
	for _, input := range []string{"../outside", "/absolute", "a/../b", " a", "a\\b"} {
		if err := ValidateProbeExecrootRelativePath(input); err == nil {
			t.Errorf("noncanonical relative path %q was accepted", input)
		}
	}
}

func TestNormalizeProbeExecrootRelativePathRebasesConfiguredToolSymlink(t *testing.T) {
	root := t.TempDir()
	execroot := filepath.Join(root, "execroot")
	repository := filepath.Join(root, "repository-cache", "toolchain")
	for _, directory := range []string{
		filepath.Join(execroot, "external"),
		filepath.Join(repository, "bin"),
		filepath.Join(repository, "lib", "gcc", "include"),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "bin", "cc"), nil, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repository, filepath.Join(execroot, "external", "toolchain")); err != nil {
		t.Fatal(err)
	}
	reported := filepath.Join(repository, "lib", "gcc", "include")
	configuredTool := filepath.Join(execroot, "external", "toolchain", "bin", "cc")
	got, err := NormalizeProbeExecrootRelativePath(execroot, configuredTool, reported)
	if err != nil || got != "external/toolchain/lib/gcc/include" {
		t.Fatalf("NormalizeProbeExecrootRelativePath() = %q, %v", got, err)
	}

	unrelated := filepath.Join(root, "unrelated", "include")
	if err := os.MkdirAll(unrelated, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NormalizeProbeExecrootRelativePath(execroot, configuredTool, unrelated); err == nil || !strings.Contains(err.Error(), "unrelated") {
		t.Fatalf("unrelated outside path error = %v", err)
	}
}

func TestProbeResultCanonicalValidation(t *testing.T) {
	value := true
	result := ProbeResult{
		Schema: LinuxProbeResultSchema, NodeID: strings.Repeat("b", 64), RequestID: strings.Repeat("c", 64),
		Scope: "target", ToolsetIdentity: "sha256-" + strings.Repeat("d", 64), Kind: "boolean", Boolean: &value,
		Steps: []ProbeStepResult{{Name: "compile", Status: "success", ExitCode: 0}},
	}
	if _, err := result.CanonicalJSON(); err != nil {
		t.Fatal(err)
	}
}

func TestProbeResultStdoutPathKindValidation(t *testing.T) {
	base := ProbeResult{
		Schema: LinuxProbeResultSchema, NodeID: strings.Repeat("b", 64), RequestID: strings.Repeat("c", 64),
		Scope: "target", ToolsetIdentity: "sha256-" + strings.Repeat("d", 64), Kind: "text", Text: "include",
		Steps: []ProbeStepResult{{
			Name: "query", Status: "success", ExitCode: 0, Stdout: "include", StdoutPathKind: ProbeStdoutPathToolset,
		}},
	}
	if _, err := base.CanonicalJSON(); err != nil {
		t.Fatalf("canonical toolset path result: %v", err)
	}
	fallback := base
	fallback.Steps = slices.Clone(base.Steps)
	fallback.Steps[0].StdoutPathKind = ProbeStdoutPathFallback
	if _, err := fallback.CanonicalJSON(); err != nil {
		t.Fatalf("canonical fallback result: %v", err)
	}
	invalid := base
	invalid.Steps = slices.Clone(base.Steps)
	invalid.Steps[0].StdoutPathKind = "guessed"
	if err := invalid.Validate(); err == nil || !strings.Contains(err.Error(), "invalid stdout path kind") {
		t.Fatalf("invalid stdout path kind error = %v", err)
	}
}

func probeTerminalSelectionFixture(t *testing.T) (*ProbePlan, ProbeReference, ProbeReference, []string) {
	t.Helper()
	builder, err := NewProbePlanBuilder("sha256-"+strings.Repeat("a", 64), "sha256-"+strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	host, err := builder.Request("host", testProbeRequest())
	if err != nil {
		t.Fatal(err)
	}
	target, err := builder.Request("target", testProbeRequest())
	if err != nil {
		t.Fatal(err)
	}
	request := testProbeRequest()
	request.InputCount = 3
	request.Sources = []string{"source.c"}
	request.SourceRoots = []string{"linux"}
	request.Steps[0].Stdin = "int selected;\n"
	request.Steps[0].Environment = map[string]string{"MODE": "unchanged"}
	root, err := builder.Request("target", request, target, host, target)
	if err != nil {
		t.Fatal(err)
	}
	otherRequest := testProbeRequest()
	otherRequest.Steps[0].Stdin = "int excluded;\n"
	other, err := builder.Request("target", otherRequest)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := builder.Plan(root, other)
	if err != nil {
		t.Fatal(err)
	}
	return plan, root, other, []string{target.NodeID, host.NodeID, target.NodeID}
}

func TestSelectProbePlanTerminalsExactClosureAndDefensiveCopies(t *testing.T) {
	plan, root, other, inputs := probeTerminalSelectionFixture(t)
	before, err := plan.entries()
	if err != nil {
		t.Fatal(err)
	}
	selection := []string{root.NodeID, root.NodeID}
	selected, err := SelectProbePlanTerminals(plan, selection)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Nodes) != 3 || len(selected.Requests) != 2 || !slices.Equal(selected.Terminal, []string{root.NodeID}) ||
		!maps.Equal(selected.Toolsets, plan.Toolsets) || !slices.Equal(selection, []string{root.NodeID, root.NodeID}) {
		t.Fatalf("selected closure changed exact roots, toolsets or caller selection: %#v", selected)
	}
	if _, found := selected.Requests[other.RequestID]; found {
		t.Fatal("discarded request survived terminal selection")
	}
	for index, node := range selected.Nodes {
		if index != 0 && selected.Nodes[index-1].ID >= node.ID {
			t.Fatal("selected nodes are not canonical")
		}
		if node.ID == other.NodeID {
			t.Fatal("unselected terminal survived")
		}
		if node.ID == root.NodeID {
			if !slices.Equal(node.Inputs, inputs) {
				t.Fatal("ordered/repeated dependency inputs changed")
			}
			selected.Nodes[index].Inputs[0] = "changed"
		}
	}
	request := selected.Requests[root.RequestID]
	request.Sources[0], request.SourceRoots[0] = "changed", "changed"
	request.Steps[0].Arguments[0] = "changed"
	request.Steps[0].Environment["MODE"] = "changed"
	request.Outcome.Predicate.Operands[0].Operator = "changed"
	selected.Toolsets["target"] = "changed"
	selected.Terminal[0] = "changed"
	after, err := plan.entries()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("selection mutation changed its original plan: %v", err)
	}
}

func TestSelectProbePlanTerminalsCanonicalFullAndEmptySelection(t *testing.T) {
	plan, root, other, _ := probeTerminalSelectionFixture(t)
	forward, err := SelectProbePlanTerminals(plan, []string{root.NodeID, other.NodeID, root.NodeID})
	if err != nil {
		t.Fatal(err)
	}
	reverse, err := SelectProbePlanTerminals(plan, []string{other.NodeID, root.NodeID})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(forward, reverse) || !slices.IsSorted(forward.Terminal) {
		t.Fatal("terminal selection depends on order/duplication")
	}
	before, err := plan.entries()
	if err != nil {
		t.Fatal(err)
	}
	after, err := forward.entries()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("all-terminal selection changed the original request/node bytes: %v", err)
	}
	empty, err := SelectProbePlanTerminals(plan, nil)
	if err != nil || len(empty.Nodes) != 0 || len(empty.Requests) != 0 || len(empty.Terminal) != 0 || !maps.Equal(empty.Toolsets, plan.Toolsets) {
		t.Fatalf("empty selection lost full configured toolsets: %#v %v", empty, err)
	}
	empty.Toolsets["host"] = "changed"
	if plan.Toolsets["host"] == "changed" {
		t.Fatal("empty selection aliases original toolset map")
	}
}

func TestSelectProbePlanTerminalsRejectsInvalidWholePlansAndRoots(t *testing.T) {
	if selected, err := SelectProbePlanTerminals(nil, nil); err == nil || selected != nil {
		t.Fatalf("nil plan accepted: %#v %v", selected, err)
	}
	for _, name := range []string{"unknown root", "nonterminal dependency", "invalid discarded request", "invalid discarded node", "repeated terminal", "unknown toolset", "extra malformed request"} {
		t.Run(name, func(t *testing.T) {
			plan, root, other, inputs := probeTerminalSelectionFixture(t)
			selection := []string{root.NodeID}
			switch name {
			case "unknown root":
				selection = []string{strings.Repeat("0", 64)}
			case "nonterminal dependency":
				selection = []string{inputs[0]}
			case "invalid discarded request":
				request := plan.Requests[other.RequestID]
				request.Steps[0].Tool = ""
				plan.Requests[other.RequestID] = request
				selection = nil
			case "invalid discarded node":
				for index := range plan.Nodes {
					if plan.Nodes[index].ID == other.NodeID {
						plan.Nodes[index].ID = strings.Repeat("0", 64)
					}
				}
				selection = nil
			case "repeated terminal":
				plan.Terminal = append(plan.Terminal, other.NodeID)
				selection = nil
			case "unknown toolset":
				plan.Toolsets["untrusted"] = plan.Toolsets["host"]
				selection = nil
			case "extra malformed request":
				plan.Requests[strings.Repeat("f", 64)] = testProbeRequest()
				selection = nil
			}
			if selected, err := SelectProbePlanTerminals(plan, selection); err == nil || selected != nil {
				t.Fatalf("invalid whole plan or root accepted: %#v %v", selected, err)
			}
		})
	}
}
