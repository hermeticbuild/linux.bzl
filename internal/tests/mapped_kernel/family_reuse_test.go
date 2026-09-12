package mapped_kernel_test

import (
	"bytes"
	"debug/elf"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

type reuseReport struct {
	Nodes           []reuseNode           `json:"nodes"`
	PreciseCompiles []reusePreciseCompile `json:"precise_compiles"`
	Schema          string                `json:"schema"`
	Variants        []reuseVariant        `json:"variants"`
	Pairs           []reusePair           `json:"pairs"`
	Membership      []reuseMembership     `json:"membership_groups"`
}

type reuseVariant struct {
	Name                string `json:"name"`
	Nodes               int    `json:"nodes"`
	SharedNodes         int    `json:"shared_nodes"`
	ExclusiveNodes      int    `json:"exclusive_nodes"`
	PreciseCompileNodes int    `json:"precise_compile_nodes"`
}

type reusePair struct {
	Left                          string `json:"left"`
	Right                         string `json:"right"`
	EligibleLeftNodes             int    `json:"eligible_left_nodes"`
	EligibleRightNodes            int    `json:"eligible_right_nodes"`
	EligibleSharedNodes           int    `json:"eligible_shared_nodes"`
	EligibleLeftReuseBasisPoints  int    `json:"eligible_left_reuse_basis_points"`
	EligibleRightReuseBasisPoints int    `json:"eligible_right_reuse_basis_points"`
	PreciseLeftCompileNodes       int    `json:"precise_left_compile_nodes"`
	PreciseRightCompileNodes      int    `json:"precise_right_compile_nodes"`
	PreciseSharedCompileNodes     int    `json:"precise_shared_compile_nodes"`
	PreciseLeftReuseBasisPoints   int    `json:"precise_left_reuse_basis_points"`
	PreciseRightReuseBasisPoints  int    `json:"precise_right_reuse_basis_points"`
}

type reuseMembership struct {
	Variants []string    `json:"variants"`
	Kinds    []reuseKind `json:"kinds"`
}

type reuseKind struct {
	Kind  string `json:"kind"`
	Nodes int    `json:"nodes"`
}

type reuseNode struct {
	NodeID         string   `json:"node_id"`
	Kind           string   `json:"kind"`
	Memberships    []string `json:"memberships"`
	TypedCompiler  bool     `json:"typed_compiler"`
	PreciseCompile bool     `json:"precise_compile"`
}

type reusePreciseCompile struct {
	NodeID        string               `json:"node_id"`
	Memberships   []string             `json:"memberships"`
	OutputDetails []reuseCompileOutput `json:"output_details"`
}

type reuseCompileOutput struct {
	Slot        int    `json:"slot"`
	Tree        string `json:"tree"`
	LogicalPath string `json:"logical_path"`
	StorePath   string `json:"store_path"`
}

func runfileFromEnv(t *testing.T, name string) string {
	t.Helper()
	logical := os.Getenv(name)
	if logical == "" {
		t.Fatalf("%s is not set", name)
	}
	path, err := runfiles.Rlocation(logical)
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	return path
}

// The public projection must be byte-identical to the configuration carried by
// the kernel/SDK that actually built the fixture, despite its earlier producer.
func TestFamilyPublicConfigMatchesExecutionConfig(t *testing.T) {
	for _, variant := range []string{"BASE", "IRRELEVANT", "RELEVANT"} {
		t.Run(variant, func(t *testing.T) {
			prefix := "FAMILY_SMOKE_" + variant
			earlyPath := runfileFromEnv(t, prefix+"_CONFIG")
			executedPath := runfileFromEnv(t, prefix+"_EXECUTION_CONFIG")
			if earlyPath == executedPath {
				t.Fatal("public configuration still aliases final execution planning")
			}
			early, err := os.ReadFile(earlyPath)
			if err != nil {
				t.Fatal(err)
			}
			executed, err := os.ReadFile(executedPath)
			if err != nil {
				t.Fatal(err)
			}
			if len(early) == 0 || !bytes.Equal(early, executed) {
				t.Fatal("early public config differs from actual kernel/SDK config")
			}
		})
	}
}

// Decode only the fixture-owned fields under test. The production decoder tests
// separately authenticate complete descriptors, canonical bytes and budgets.
func compilerGuardStrings(t *testing.T, words []string, indices []int) []string {
	t.Helper()
	if indices == nil {
		return nil
	}
	result := make([]string, len(indices))
	for i, index := range indices {
		if index < 0 || index >= len(words) {
			t.Fatal("compiler guard fixture has a dangling string reference")
		}
		result[i] = words[index]
	}
	return result
}

func TestFamilyDefaultCompilerGuardPipelineBatchesIntrinsicCalls(t *testing.T) {
	manifests := strings.Fields(os.Getenv("FAMILY_SMOKE_COMPILER_GUARDS"))
	if len(manifests) != 3 {
		t.Fatalf("want exactly three default compiler guard manifests, got %d", len(manifests))
	}
	seen := map[string]bool{}
	batched := map[string]bool{"base": false, "irrelevant": false, "relevant": false}
	generatedOffsets := map[string]bool{}
	for index, logical := range manifests {
		if seen[logical] {
			t.Fatalf("compiler guard manifest %d repeats an earlier runfile", index)
		}
		seen[logical] = true
		filename, err := runfiles.Rlocation(logical)
		if err != nil {
			t.Fatalf("resolve compiler guard manifest %d: %v", index, err)
		}
		content, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		// Inspect only the fixture-owned macro in compiler arguments; never
		// include complete compiler contexts or environments in diagnostics.
		var manifest struct {
			Schema    string
			Truncated bool
			Strings   []string
			Contexts  map[string]struct {
				Scope, Role, Language       string
				Arguments, TranslationUnits []int
			}
			Variants map[string][]struct {
				Context string
				Kind    string
				Calls   []struct {
					Operator string `json:"operator"`
					Operand  string `json:"operand"`
				}
			}
		}
		if err := json.Unmarshal(content, &manifest); err != nil {
			t.Fatalf("decode compiler guard manifest %d: %v", index, err)
		}
		if manifest.Schema != "linux-kbuild-compiler-guards-v5" || manifest.Truncated {
			t.Fatalf("compiler guard manifest %d has unsupported schema or a truncated frontier", index)
		}
		if len(manifest.Variants) != len(batched) {
			t.Fatalf("compiler guard manifest %d has %d variants, want %d", index, len(manifest.Variants), len(batched))
		}
		for variant := range batched {
			queries, found := manifest.Variants[variant]
			if !found {
				t.Fatalf("compiler guard manifest %d omits variant %s", index, variant)
			}
			for _, query := range queries {
				context, found := manifest.Contexts[query.Context]
				if !found {
					t.Fatalf("compiler guard manifest %d has a dangling context reference", index)
				}
				arguments := compilerGuardStrings(t, manifest.Strings, context.Arguments)
				units := compilerGuardStrings(t, manifest.Strings, context.TranslationUnits)
				if query.Kind != "intrinsic-integer" {
					continue
				}
				deprecated, unrecognized := false, false
				dynamicBuiltin, unknownBuiltin := false, false
				for _, call := range query.Calls {
					switch call.Operator {
					case "__has_attribute":
						deprecated = deprecated || call.Operand == "deprecated"
						unrecognized = unrecognized || call.Operand == "linux_bzl_unrecognized_attribute"
					case "__has_builtin":
						dynamicBuiltin = dynamicBuiltin || call.Operand == "__builtin_dynamic_object_size"
						unknownBuiltin = unknownBuiltin || call.Operand == "__builtin_linux_bzl_unrecognized"
					}
				}
				// Reset membership per query: two independent singleton queries
				// must not satisfy this default-pipeline batching regression.
				quotedMacro := false
				for _, argument := range arguments {
					quotedMacro = quotedMacro || strings.HasPrefix(argument, `-DMAPPED_OBJECT_FILE="`) && strings.HasSuffix(argument, `/smoke"`)
				}
				batched[variant] = batched[variant] || deprecated && unrecognized && dynamicBuiltin && unknownBuiltin && quotedMacro
				// TranslationUnits contains only residual source ownership that
				// could be hidden inside dynamic argv, not all translation units.
				// The explicit asm-offsets.c operand is already removed. Identify
				// its exact fixture-owned context instead, and require c_flags to
				// remain deferred: a direct-argv query does not exercise the shell
				// word mode needed by the quoted KBUILD_MODFILE replacement.
				deferredExpressions := 0
				for _, argument := range arguments {
					digest, symbolic := strings.CutPrefix(argument, "LINUX_BZL_PROBE_")
					if !symbolic || len(digest) != 64 {
						continue
					}
					valid := true
					for _, character := range digest {
						valid = valid && (character >= '0' && character <= '9' || character >= 'a' && character <= 'f')
					}
					if valid {
						deferredExpressions++
					}
				}
				generatedOffsets[variant] = generatedOffsets[variant] || deprecated && dynamicBuiltin && unknownBuiltin &&
					context.Scope == "target" && context.Role == "cc" && context.Language == "c" &&
					len(units) == 0 && len(arguments) == 3 && deferredExpressions == 1 &&
					slices.Contains(arguments, "-DMAPPED_DEFERRED_ASSEMBLY_CONTEXT=1") &&
					slices.Contains(arguments, "-fverbose-asm")
			}
		}
	}
	for _, variant := range []string{"base", "irrelevant", "relevant"} {
		if !batched[variant] {
			t.Errorf("%s default compiler guard rounds never batch all four mixed smoke.c intrinsic calls with the quoted path-like macro", variant)
		}
		if !generatedOffsets[variant] {
			t.Errorf("%s default compiler guard rounds never measure the deferred generated-assembly compiler context", variant)
		}
	}
}

func TestFamilyReuseReportSharesConfigIndependentCompile(t *testing.T) {
	content, err := os.ReadFile(runfileFromEnv(t, "FAMILY_SMOKE_REUSE_REPORT"))
	if err != nil {
		t.Fatal(err)
	}
	var report reuseReport
	if err := json.Unmarshal(content, &report); err != nil {
		t.Fatal(err)
	}
	if report.Schema != "linux-kernel-family-reuse-report-v2" {
		t.Fatalf("unexpected reuse-report schema %q", report.Schema)
	}
	if len(report.Variants) != 3 || report.Variants[0].Name != "base" || report.Variants[1].Name != "irrelevant" || report.Variants[2].Name != "relevant" {
		t.Fatalf("unexpected variants: %#v", report.Variants)
	}
	if len(report.Pairs) != 3 {
		t.Fatalf("got %d reuse pairs, want 3", len(report.Pairs))
	}
	pairs := map[string]reusePair{}
	for _, pair := range report.Pairs {
		pairs[pair.Left+"/"+pair.Right] = pair
	}
	irrelevant := pairs["base/irrelevant"]
	assertOnlyPinnedNumericProducersUnshared(t, report, irrelevant)
	if irrelevant.PreciseLeftCompileNodes == 0 || irrelevant.PreciseLeftCompileNodes != irrelevant.PreciseRightCompileNodes ||
		irrelevant.PreciseSharedCompileNodes != irrelevant.PreciseLeftCompileNodes ||
		irrelevant.PreciseLeftReuseBasisPoints != 10000 || irrelevant.PreciseRightReuseBasisPoints != 10000 {
		t.Fatalf("precisely analyzed compile reuse was not 100%%: %#v", irrelevant)
	}
	relevant := pairs["base/relevant"]
	if relevant.EligibleLeftNodes == 0 || relevant.EligibleLeftNodes != relevant.EligibleRightNodes || relevant.EligibleSharedNodes != 0 {
		t.Fatalf("config-dependent compile nodes were incorrectly shared: %#v", relevant)
	}
	sharedCompile := false
	for _, membership := range report.Membership {
		if len(membership.Variants) != 2 || membership.Variants[0] != "base" || membership.Variants[1] != "irrelevant" {
			continue
		}
		for _, kind := range membership.Kinds {
			if kind.Kind == "compile" && kind.Nodes > 0 {
				sharedCompile = true
			}
		}
	}
	if !sharedCompile {
		t.Fatal("reuse report contains no compile node shared by base and irrelevant overlay")
	}
	assertGeneratedHeaderCompilerSharing(t, report)
	assertCompilerOutputSharing(t, report, "dollar_smoke.o")
	assertCompilerOutputSharing(t, report, "asm-offsets.s")
}

func TestFamilyGeneratedAssemblyHeaderIsActuallyBuilt(t *testing.T) {
	headers := map[string][]byte{}
	measurements := familyIntrinsicMeasurements(t)
	for _, variant := range []struct {
		name, environment string
		configValue       int
	}{
		{name: "base", environment: "FAMILY_SMOKE_BASE_OBJECTS"},
		{name: "irrelevant", environment: "FAMILY_SMOKE_IRRELEVANT_OBJECTS"},
		{name: "relevant", environment: "FAMILY_SMOKE_RELEVANT_OBJECTS", configValue: 1},
	} {
		root := runfileFromEnv(t, variant.environment)
		assembly, err := os.ReadFile(filepath.Join(root, "asm-offsets.s"))
		if err != nil {
			t.Fatal(err)
		}
		header, err := os.ReadFile(filepath.Join(root, "include/generated/mapped_offsets.h"))
		if err != nil {
			t.Fatal(err)
		}
		headers[variant.name] = header
		if !bytes.Contains(assembly, []byte("mapped_offsets_modfile")) ||
			!bytes.Contains(assembly, []byte("mapped_asm_offsets")) ||
			bytes.Contains(assembly, []byte("linux-bzl-action-object-tree")) {
			t.Fatalf("%s generated assembly lacks compiled witnesses or contains a private tree marker", variant.name)
		}
		values := map[string]int{}
		for _, line := range strings.Split(string(header), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 || fields[0] != "#define" || !strings.HasPrefix(fields[1], "MAPPED_OFFSETS_") {
				continue
			}
			value, err := strconv.Atoi(fields[2])
			if _, duplicate := values[fields[1]]; duplicate || err != nil || value < 0 || value > 1 {
				t.Fatalf("%s generated header has a duplicate or invalid Boolean witness", variant.name)
			}
			values[fields[1]] = value
		}
		for _, name := range []string{
			"MAPPED_OFFSETS_EXPRESSION", "MAPPED_OFFSETS_CONDITION", "MAPPED_OFFSETS_CONFIG",
			"MAPPED_OFFSETS_BUILTIN_EXPRESSION", "MAPPED_OFFSETS_BUILTIN_CONDITION",
			"MAPPED_OFFSETS_UNKNOWN_BUILTIN_EXPRESSION", "MAPPED_OFFSETS_UNKNOWN_BUILTIN_CONDITION",
		} {
			if _, found := values[name]; !found {
				t.Fatalf("%s generated header omits %s", variant.name, name)
			}
			// The header helper must consume real compiler assembly records, not
			// reproduce fixture constants or substitute preprocessed C output.
			matches := 0
			for _, line := range strings.Split(string(assembly), "\n") {
				record, found := strings.CutPrefix(strings.TrimSpace(line), ".ascii")
				if !found {
					continue
				}
				record, found = strings.CutPrefix(strings.TrimSpace(record), `"->`+name+` `)
				if !found {
					continue
				}
				fields := strings.Fields(record)
				if len(fields) == 0 {
					t.Fatalf("%s assembly has an empty %s record", variant.name, name)
				}
				value, err := strconv.Atoi(strings.TrimLeft(fields[0], "$#"))
				if err != nil || value != values[name] {
					t.Fatalf("%s generated header disagrees with its assembly %s record", variant.name, name)
				}
				matches++
			}
			if matches != 1 {
				t.Fatalf("%s assembly has %d %s records, want one", variant.name, matches, name)
			}
		}
		if len(values) != 7 || values["MAPPED_OFFSETS_CONFIG"] != variant.configValue {
			t.Fatalf("%s generated header has unexpected witnesses or a stale CONFIG value", variant.name)
		}
		for _, intrinsic := range []struct {
			measurement, expression, condition string
		}{
			{"measured_attribute_deprecated", "MAPPED_OFFSETS_EXPRESSION", "MAPPED_OFFSETS_CONDITION"},
			{"measured_builtin_dynamic_object_size", "MAPPED_OFFSETS_BUILTIN_EXPRESSION", "MAPPED_OFFSETS_BUILTIN_CONDITION"},
			{"measured_builtin_unknown", "MAPPED_OFFSETS_UNKNOWN_BUILTIN_EXPRESSION", "MAPPED_OFFSETS_UNKNOWN_BUILTIN_CONDITION"},
		} {
			want := int(measurements[intrinsic.measurement])
			if values[intrinsic.expression] != want || values[intrinsic.condition] != want {
				t.Fatalf("%s generated %s #if/expression witnesses disagree with independent compiler measurement %d", variant.name, intrinsic.expression, want)
			}
			t.Logf("%s generated %s: %d", variant.name, intrinsic.expression, want)
		}
	}
	if !bytes.Equal(headers["base"], headers["irrelevant"]) || bytes.Equal(headers["base"], headers["relevant"]) {
		t.Fatal("generated offsets header lost irrelevant CONFIG reuse or relevant CONFIG invalidation")
	}
}

func TestFamilyGeneratedAssemblyAndHeaderKeepTypedProducers(t *testing.T) {
	var report reuseReport
	readHelperJSON(t, runfileFromEnv(t, "FAMILY_SMOKE_REUSE_REPORT"), &report)
	memberships := map[string][]string{}
	for _, node := range report.Nodes {
		memberships[node.NodeID] = node.Memberships
	}
	seen := map[string]map[string]bool{
		"asm-offsets.s":                      {},
		"include/generated/mapped_offsets.h": {},
	}
	for _, logical := range strings.Fields(os.Getenv("FAMILY_SMOKE_PLAN")) {
		root, err := runfiles.Rlocation(logical)
		if err != nil {
			t.Fatal(err)
		}
		nodes, err := filepath.Glob(filepath.Join(root, "nodes", "*", "*"))
		if err != nil {
			t.Fatal(err)
		}
		for _, node := range nodes {
			recipeID := filepath.Base(oneHelperPlanPath(t, filepath.Join(node, "recipe", "*")))
			var recipe struct {
				Kind, Tool     string
				Arguments      []string
				WorkingOutputs map[string]string `json:"working_outputs"`
			}
			readHelperJSON(t, filepath.Join(root, "recipes", recipeID+".json"), &recipe)
			output := recipe.WorkingOutputs["00000000"]
			switch output {
			case "asm-offsets.s":
				if recipe.Kind != "compile" || recipe.Tool != "cc" || !slices.Contains(recipe.Arguments, "-S") ||
					!slices.Contains(recipe.Arguments, "-DMAPPED_DEFERRED_ASSEMBLY_CONTEXT=1") ||
					slices.Contains(recipe.Arguments, "-flto") || slices.Contains(recipe.Arguments, "-g") {
					t.Fatal("generated assembly lost its typed compiler or source-selected flag filtering")
				}
			case "include/generated/mapped_offsets.h":
				if recipe.Kind != "generate" || recipe.Tool != "actionfile" ||
					!slices.Contains(recipe.Arguments, "-validate_config_independent_macro_header_v1") {
					// filechk has intermediate stdout producers too; only the
					// validated final header satisfies the typed-output witness.
					continue
				}
			default:
				continue
			}
			for _, variant := range memberships[filepath.Base(node)] {
				seen[output][variant] = true
			}
		}
	}
	for output, variants := range seen {
		for _, variant := range []string{"base", "irrelevant", "relevant"} {
			if !variants[variant] {
				t.Errorf("%s has no typed final producer for %s", variant, output)
			}
		}
	}
}

func assertGeneratedHeaderCompilerSharing(t *testing.T, report reuseReport) {
	t.Helper()
	assertCompilerOutputSharing(t, report, "smoke.o")
}

func assertCompilerOutputSharing(t *testing.T, report reuseReport, logicalPath string) {
	t.Helper()
	nodes := map[string]reuseNode{}
	for _, node := range report.Nodes {
		if _, exists := nodes[node.NodeID]; exists {
			t.Fatalf("duplicate reuse node %q", node.NodeID)
		}
		nodes[node.NodeID] = node
	}
	compileByMembership := map[string]string{}
	for _, compile := range report.PreciseCompiles {
		matchingOutputs := 0
		for _, output := range compile.OutputDetails {
			if output.LogicalPath != logicalPath || output.Tree != "objects" {
				continue
			}
			matchingOutputs++
			if output.Slot != 0 || output.StorePath == "" {
				t.Fatalf("%s has invalid physical output identity: %#v", logicalPath, output)
			}
		}
		if matchingOutputs == 0 {
			continue
		}
		membership := strings.Join(compile.Memberships, ",")
		if matchingOutputs != 1 || compile.NodeID == "" ||
			(membership != "base,irrelevant" && membership != "relevant") ||
			compileByMembership[membership] != "" {
			t.Fatalf("%s does not have the exact expected sharing: %#v", logicalPath, compile)
		}
		node, exists := nodes[compile.NodeID]
		if !exists || node.Kind != "compile" || !node.TypedCompiler || !node.PreciseCompile ||
			strings.Join(node.Memberships, ",") != membership {
			t.Fatalf("%s precise record disagrees with execution membership: %#v / %#v", logicalPath, compile, node)
		}
		compileByMembership[membership] = compile.NodeID
	}
	if len(compileByMembership) != 2 || compileByMembership["base,irrelevant"] == "" ||
		compileByMembership["relevant"] == "" ||
		compileByMembership["base,irrelevant"] == compileByMembership["relevant"] {
		t.Fatalf("want shared base/irrelevant %s and distinct relevant %s: %#v", logicalPath, logicalPath, compileByMembership)
	}
}

func TestFamilyDollarAssemblyObjectsAreActuallyBuilt(t *testing.T) {
	objects := map[string][]byte{}
	for _, variant := range []struct {
		name, environment string
		value             byte
	}{
		{name: "base", environment: "FAMILY_SMOKE_BASE_OBJECTS", value: 113},
		{name: "irrelevant", environment: "FAMILY_SMOKE_IRRELEVANT_OBJECTS", value: 113},
		{name: "relevant", environment: "FAMILY_SMOKE_RELEVANT_OBJECTS", value: 114},
	} {
		// Read an explicitly declared variant objects view, never infer an
		// execution-store path or substitute preprocessed assembly text.
		filename := filepath.Join(runfileFromEnv(t, variant.environment), "dollar_smoke.o")
		content, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		objects[variant.name] = content
		object, err := elf.NewFile(bytes.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		if object.Type != elf.ET_REL || object.Machine != elf.EM_X86_64 {
			object.Close()
			t.Fatalf("%s dollar fixture is not an x86-64 relocatable object", variant.name)
		}
		symbols, err := object.Symbols()
		if err != nil {
			object.Close()
			t.Fatal(err)
		}
		found := false
		// x86-64 movl $imm32, %eax followed by ret. Checking the immediate
		// bytes proves the macro next to '$' actually reached the assembler.
		want := []byte{0xb8, variant.value, 0, 0, 0, 0xc3}
		for _, symbol := range symbols {
			if symbol.Name != "mapped_dollar_config_value" {
				continue
			}
			if found || elf.ST_TYPE(symbol.Info) != elf.STT_FUNC || symbol.Size != uint64(len(want)) ||
				symbol.Section == elf.SHN_UNDEF || symbol.Section >= elf.SHN_LORESERVE || int(symbol.Section) >= len(object.Sections) {
				object.Close()
				t.Fatalf("%s has invalid dollar witness symbol: %#v", variant.name, symbol)
			}
			found = true
			for _, relocation := range object.Sections {
				if (relocation.Type == elf.SHT_REL || relocation.Type == elf.SHT_RELA) &&
					relocation.Info == uint32(symbol.Section) && relocation.Size != 0 {
					object.Close()
					t.Fatalf("%s dollar immediate still requires linker relocations", variant.name)
				}
			}
			section := object.Sections[symbol.Section]
			data, err := section.Data()
			if err != nil || section.Type != elf.SHT_PROGBITS || section.Flags&elf.SHF_EXECINSTR == 0 || symbol.Value < section.Addr {
				object.Close()
				t.Fatalf("%s dollar witness has no executable bytes: %v", variant.name, err)
			}
			offset := symbol.Value - section.Addr
			if offset > uint64(len(data)) || symbol.Size > uint64(len(data))-offset ||
				!bytes.Equal(data[offset:offset+symbol.Size], want) {
				object.Close()
				t.Fatalf("%s dollar witness does not encode movl $%d, %%eax; ret", variant.name, variant.value)
			}
		}
		object.Close()
		if !found {
			t.Fatalf("%s has no compiled dollar witness", variant.name)
		}
	}
	if !bytes.Equal(objects["base"], objects["irrelevant"]) {
		t.Fatal("irrelevant CONFIG changed the assembled dollar object")
	}
	if bytes.Equal(objects["base"], objects["relevant"]) {
		t.Fatal("pasted CONFIG change did not change the assembled dollar object")
	}
}

func TestGeneratedHeaderReuseRetainsDifferentFullConfigurations(t *testing.T) {
	configs := map[string][]byte{}
	for _, variant := range []struct {
		name, environment string
		unused, used      bool
	}{
		{name: "base", environment: "FAMILY_SMOKE_BASE_CONFIG"},
		{name: "irrelevant", environment: "FAMILY_SMOKE_IRRELEVANT_CONFIG", unused: true},
		{name: "relevant", environment: "FAMILY_SMOKE_RELEVANT_CONFIG", used: true},
	} {
		content, err := os.ReadFile(runfileFromEnv(t, variant.environment))
		if err != nil {
			t.Fatal(err)
		}
		values := map[string]bool{}
		for _, line := range strings.Split(string(content), "\n") {
			values[line] = true
		}
		if values["CONFIG_UNUSED_FAMILY_OPTION=y"] != variant.unused ||
			values["CONFIG_USED_FAMILY_OPTION=y"] != variant.used {
			t.Fatalf("%s does not retain its expected full CONFIG values", variant.name)
		}
		configs[variant.name] = content
	}
	if bytes.Equal(configs["base"], configs["irrelevant"]) ||
		bytes.Equal(configs["base"], configs["relevant"]) {
		t.Fatal("generated-header reuse fixture lost its differing full .config inputs")
	}
}

func generatedHeaderObjectByte(t *testing.T, image []byte) byte {
	t.Helper()
	return compiledObjectSymbolByte(t, image, "mapped_generated_header_value")
}

func compiledObjectSymbolByte(t *testing.T, image []byte, name string) byte {
	t.Helper()
	return compiledObjectSymbolBytes(t, image, name, 1)[0]
}

func compiledObjectSymbolBytes(t *testing.T, image []byte, name string, size uint64) []byte {
	t.Helper()
	if size == 0 || size > 1<<20 {
		t.Fatal("invalid bounded object symbol size")
	}
	object, err := elf.NewFile(bytes.NewReader(image))
	if err != nil {
		t.Fatal(err)
	}
	defer object.Close()
	symbols, err := object.Symbols()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	var value []byte
	for _, symbol := range symbols {
		if symbol.Name != name {
			continue
		}
		if found || elf.ST_TYPE(symbol.Info) != elf.STT_OBJECT || symbol.Size != size ||
			symbol.Section == elf.SHN_UNDEF || symbol.Section >= elf.SHN_LORESERVE ||
			int(symbol.Section) >= len(object.Sections) {
			t.Fatalf("invalid generated-header object symbol: %#v", symbol)
		}
		found = true
		section := object.Sections[symbol.Section]
		if section.Type == elf.SHT_NOBITS || symbol.Value < section.Addr {
			t.Fatalf("generated-header symbol has no file-backed byte: %#v", symbol)
		}
		content, err := section.Data()
		if err != nil {
			t.Fatal(err)
		}
		offset := symbol.Value - section.Addr
		if offset > uint64(len(content)) || size > uint64(len(content))-offset {
			t.Fatalf("generated-header symbol exceeds its ELF section: %#v", symbol)
		}
		value = slices.Clone(content[offset : offset+size])
	}
	if !found {
		t.Fatal("compiled smoke.o image has no generated-header witness symbol")
	}
	return value
}

func TestFamilyVariantImagesAreActuallyBuilt(t *testing.T) {
	base, err := os.ReadFile(runfileFromEnv(t, "FAMILY_SMOKE_BASE_IMAGE"))
	if err != nil {
		t.Fatal(err)
	}
	irrelevant, err := os.ReadFile(runfileFromEnv(t, "FAMILY_SMOKE_IRRELEVANT_IMAGE"))
	if err != nil {
		t.Fatal(err)
	}
	relevant, err := os.ReadFile(runfileFromEnv(t, "FAMILY_SMOKE_RELEVANT_IMAGE"))
	if err != nil {
		t.Fatal(err)
	}
	if len(base) < 4 || !bytes.Equal(base[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		t.Fatal("base output is not an ELF image")
	}
	if !bytes.Equal(base, irrelevant) {
		t.Fatal("unused Kconfig overlay changed the projected image")
	}
	if bytes.Equal(base, relevant) {
		t.Fatal("used Kconfig overlay did not change the projected image")
	}
	measurements := familyIntrinsicMeasurements(t)
	measurementObject, err := os.ReadFile(runfileFromEnv(t, "FAMILY_SMOKE_INTRINSIC_MEASUREMENT"))
	if err != nil {
		t.Fatal(err)
	}
	selfReference := compiledObjectSymbolByte(t, measurementObject, "measured_self_reference_prescan")
	if selfReference != 18 {
		t.Fatalf("independent compiler self-reference prescan byte = %d, want enum value 17 plus one", selfReference)
	}
	for _, variant := range []struct {
		name  string
		image []byte
		value string
	}{
		{name: "base", image: base, value: "0"},
		{name: "irrelevant", image: irrelevant, value: "0"},
		{name: "relevant", image: relevant, value: "1"},
	} {
		// The image contains the actual smoke.o bytes followed by the independent
		// host marker. Inspect an ELF data symbol so that suffix cannot mask a
		// stale, uncompiled, or missing target-stage generated header.
		if got, want := generatedHeaderObjectByte(t, variant.image), byte(73+variant.value[0]-'0'); got != want {
			t.Errorf("%s compiled generated-header value = %d, want %d", variant.name, got, want)
		}
		if got, want := compiledObjectSymbolByte(t, variant.image, "mapped_pasted_config_value"), byte(91+variant.value[0]-'0'); got != want {
			t.Errorf("%s compiled pasted CONFIG value = %d, want %d", variant.name, got, want)
		}
		if got := compiledObjectSymbolByte(t, variant.image, "mapped_self_reference_prescan"); got != selfReference {
			t.Errorf("%s self-reference prescan byte = %d, independent compiler measurement = %d", variant.name, got, selfReference)
		}
		// These mixed calls belong to smoke.c's one exact compiler invocation.
		// Compare C expressions and source-wrapper #if branches with an actual
		// independently compiled object, never a compiler-family answer table.
		for _, intrinsic := range []struct {
			comparison, expressionSymbol, conditionSymbol, measurement string
			expressionOffset, conditionOffset                          byte
		}{
			{
				comparison:       "__has_attribute(deprecated) > 1",
				expressionSymbol: "mapped_intrinsic_expression", conditionSymbol: "mapped_intrinsic_condition",
				measurement:     "measured_attribute_deprecated",
				conditionOffset: 113,
			},
			{
				comparison:       "__has_attribute(linux_bzl_unrecognized_attribute) != 0",
				expressionSymbol: "mapped_second_intrinsic_expression", conditionSymbol: "mapped_second_intrinsic_condition",
				measurement:      "measured_attribute_unknown",
				expressionOffset: 127, conditionOffset: 149,
			},
			{
				comparison:       "__has_builtin(__builtin_dynamic_object_size) != 0",
				expressionSymbol: "mapped_builtin_expression", conditionSymbol: "mapped_builtin_condition",
				measurement:      "measured_builtin_dynamic_object_size",
				expressionOffset: 161, conditionOffset: 173,
			},
			{
				comparison:       "__has_builtin(__builtin_linux_bzl_unrecognized) != 0",
				expressionSymbol: "mapped_unknown_builtin_expression", conditionSymbol: "mapped_unknown_builtin_condition",
				measurement:      "measured_builtin_unknown",
				expressionOffset: 181, conditionOffset: 193,
			},
		} {
			encoded := compiledObjectSymbolByte(t, variant.image, intrinsic.expressionSymbol)
			if encoded < intrinsic.expressionOffset || encoded-intrinsic.expressionOffset > 1 {
				t.Fatalf("%s compiler emitted an invalid encoded Boolean %d for %s", variant.name, encoded, intrinsic.comparison)
			}
			expression := encoded - intrinsic.expressionOffset
			if got, want := compiledObjectSymbolByte(t, variant.image, intrinsic.conditionSymbol), intrinsic.conditionOffset+expression; got != want {
				t.Errorf("%s compiler condition for %s = %d, expression witness requires %d", variant.name, intrinsic.comparison, got, want)
			}
			if want := measurements[intrinsic.measurement]; expression != want {
				t.Errorf("%s compiler comparison %s = %d, independent compiler measurement = %d", variant.name, intrinsic.comparison, expression, want)
			}
			t.Logf("%s actual compiler %s: %d", variant.name, intrinsic.comparison, expression)
		}
		if got, want := compiledObjectSymbolByte(t, variant.image, "mapped_builtin_pasted_config_value"),
			byte(211)+measurements["measured_builtin_dynamic_object_size"]*(variant.value[0]-'0'); got != want {
			t.Errorf("%s builtin-dependent pasted CONFIG byte = %d, independent measurement/config require %d", variant.name, got, want)
		}
		if got := bytes.Count(variant.image, []byte("# mapped-executed-helper:elf-depfile\n")); got != 1 {
			t.Errorf("%s image contains %d compiled-helper witnesses, want exactly one", variant.name, got)
		}
		if got := bytes.Count(variant.image, []byte("# mapped-build-owner:bazel@bazel\n")); got != 1 {
			t.Errorf("%s image contains %d reproducible build-owner witnesses, want exactly one", variant.name, got)
		}
		// This suffix is emitted by the compiled host program, not smoke.c.
		// Its constant comes from a quoted ../../include/linux header and its
		// value from a quoted ../../include/generated/autoconf.h include found
		// through the host command's source-derived object include directory.
		want := []byte("mapped-relative-host:73:" + variant.value + "\n")
		if !bytes.HasSuffix(variant.image, want) || bytes.Count(variant.image, []byte("mapped-relative-host:")) != 1 {
			t.Errorf("%s image does not contain exactly its compiled host result %q", variant.name, want)
		}
	}
}

func TestFamilyVariantExternalModuleIsActuallyBuilt(t *testing.T) {
	module, err := os.ReadFile(runfileFromEnv(t, "FAMILY_SMOKE_OVERLAY_MODULE"))
	if err != nil {
		t.Fatal(err)
	}
	if len(module) < 4 || !bytes.Equal(module[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		t.Fatal("overlay SDK external-module output is not an ELF kernel module")
	}
}
