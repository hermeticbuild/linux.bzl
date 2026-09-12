package mapped_kernel_test

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

func TestFamilyLiteralIncludeLookaheadBuildsAndShares(t *testing.T) {
	manifests := strings.Fields(os.Getenv("FAMILY_SMOKE_COMPILER_GUARDS"))
	if len(manifests) != 3 {
		t.Fatal("expected the unchanged three-round pipeline")
	}
	firstRounds, laterRounds := map[string]int{}, map[string]int{}
	for round, logical := range manifests {
		filename, err := runfiles.Rlocation(logical)
		if err != nil {
			t.Fatal(err)
		}
		var manifest struct {
			Truncated bool
			Variants  map[string][]struct {
				Kind  string
				Names []string
			}
		}
		readHelperJSON(t, filename, &manifest)
		if manifest.Truncated {
			t.Fatal("lookahead truncated the compiler frontier")
		}
		for variant, queries := range manifest.Variants {
			for _, query := range queries {
				if slices.Contains(query.Names, "__MAPPED_LITERAL_FIRST") {
					if _, seen := firstRounds[variant]; !seen {
						firstRounds[variant] = round
					}
				}
				if slices.Contains(query.Names, "__MAPPED_LITERAL_LATER") {
					if query.Kind != "optional-literal-hints" {
						t.Errorf("%s later-header candidate left the weak tier: %s", variant, query.Kind)
					}
					if _, seen := laterRounds[variant]; !seen {
						laterRounds[variant] = round
					}
				}
			}
		}
	}
	for _, variant := range []string{"base", "irrelevant", "relevant"} {
		first, foundFirst := firstRounds[variant]
		later, foundLater := laterRounds[variant]
		if !foundFirst || !foundLater || later > first {
			t.Errorf("%s did not probe the later header before advancing replay: first=%v later=%v", variant, firstRounds, laterRounds)
		}
		root := runfileFromEnv(t, "FAMILY_SMOKE_"+strings.ToUpper(variant)+"_OBJECTS")
		data := numericReadFile(t, filepath.Join(root, "literal_hint.o"), 1<<20)
		want := byte(46)
		if variant == "relevant" {
			want++
		}
		if got := compiledObjectSymbolByte(t, data, "mapped_literal_hint_value"); got != want {
			t.Errorf("%s actual compiled value = %d, want %d", variant, got, want)
		}
	}
	var report reuseReport
	readHelperJSON(t, runfileFromEnv(t, "FAMILY_SMOKE_REUSE_REPORT"), &report)
	assertCompilerOutputSharing(t, report, "literal_hint.o")
}

func TestFamilyVariadicStringificationBuildsAndShares(t *testing.T) {
	for _, variant := range []string{"base", "irrelevant", "relevant"} {
		root := runfileFromEnv(t, "FAMILY_SMOKE_"+strings.ToUpper(variant)+"_OBJECTS")
		data := numericReadFile(t, filepath.Join(root, "literal_hint.o"), 1<<20)
		selected := "0"
		if variant == "relevant" {
			selected = "1"
		}
		for name, text := range map[string]string{
			"mapped_stringified_config":  selected,
			"mapped_stringified_raw":     "CONFIG_UNUSED_FAMILY_OPTION",
			"mapped_stringified_spacing": "a+b, c",
			"mapped_stringified_escaped": `"a\\b"`,
			"mapped_stringified_empty":   "",
		} {
			want := append([]byte(text), 0)
			if got := compiledObjectSymbolBytes(t, data, name, uint64(len(want))); !bytes.Equal(got, want) {
				t.Errorf("%s %s emitted %q, want %q", variant, name, got, want)
			}
		}
	}
	var report reuseReport
	readHelperJSON(t, runfileFromEnv(t, "FAMILY_SMOKE_REUSE_REPORT"), &report)
	// Raw stringification must not add a dependency on the unused option, while
	// wrapper prescan and the selected source branch must retain the used one.
	assertCompilerOutputSharing(t, report, "literal_hint.o")
}

func TestFamilyPendingQueriesShareWithOriginalCompilerValues(t *testing.T) {
	counts := map[string]map[string]int{}
	manifests := strings.Fields(os.Getenv("FAMILY_SMOKE_COMPILER_GUARDS"))
	if len(manifests) != 3 {
		t.Fatal("expected all three production compiler-guard rounds")
	}
	for _, logical := range manifests {
		filename, err := runfiles.Rlocation(logical)
		if err != nil {
			t.Fatal(err)
		}
		var manifest struct {
			Schema    string
			Truncated bool
			Strings   []string
			Contexts  map[string]struct{ Arguments []int }
			Variants  map[string][]struct {
				Context, Kind string
				Names         []string
			}
		}
		readHelperJSON(t, filename, &manifest)
		if manifest.Schema != "linux-kbuild-compiler-guards-v5" || manifest.Truncated {
			t.Fatal("pending fixture has an unsupported schema or truncated compiler frontier")
		}
		for variant, queries := range manifest.Variants {
			if counts[variant] == nil {
				counts[variant] = map[string]int{}
			}
			for _, query := range queries {
				for _, name := range query.Names {
					if !strings.HasPrefix(name, "__MAPPED_PENDING_") {
						continue
					}
					context, found := manifest.Contexts[query.Context]
					if !found {
						t.Fatal("pending fixture has a dangling context reference")
					}
					arguments := compilerGuardStrings(t, manifest.Strings, context.Arguments)
					if query.Kind != "optional-definedness" && query.Kind != "optional-token-hints" ||
						!slices.Contains(arguments, "-DMAPPED_PENDING_CONTEXT=7") && !slices.Contains(arguments, "-DMAPPED_PENDING_CONTEXT=11") {
						t.Fatalf("%s lost its original compiler context or optional tier: %#v", name, query)
					}
					counts[variant][name]++
				}
			}
		}
	}
	var report reuseReport
	readHelperJSON(t, runfileFromEnv(t, "FAMILY_SMOKE_REUSE_REPORT"), &report)
	for _, variant := range []string{"base", "irrelevant", "relevant"} {
		for _, name := range []string{"__MAPPED_PENDING_A", "__MAPPED_PENDING_B", "__MAPPED_PENDING_C", "__MAPPED_PENDING_D", "__MAPPED_PENDING_E", "__MAPPED_PENDING_F"} {
			if counts[variant][name] != 1 {
				t.Errorf("%s scheduled %s %d times, want once across both original contexts", variant, name, counts[variant][name])
			}
		}
		root := runfileFromEnv(t, "FAMILY_SMOKE_"+strings.ToUpper(variant)+"_OBJECTS")
		for _, side := range []string{"left", "right"} {
			want := byte(68)
			if side == "right" {
				want = 72
			}
			if variant == "relevant" {
				want++
			}
			data := numericReadFile(t, filepath.Join(root, "pending_"+side+".o"), 1<<20)
			if got := compiledObjectSymbolByte(t, data, "mapped_pending_"+side+"_value"); got != want {
				t.Errorf("%s/%s lost original macro/config values: %d, want %d", variant, side, got, want)
			}
		}
	}
	assertCompilerOutputSharing(t, report, "pending_left.o")
	assertCompilerOutputSharing(t, report, "pending_right.o")
}

func TestFamilyOpenedHeaderHintsBuildAndShareWithMeasuredBindings(t *testing.T) {
	batched := map[string]bool{}
	manifests := strings.Fields(os.Getenv("FAMILY_SMOKE_COMPILER_GUARDS"))
	if len(manifests) != 3 {
		t.Fatal("expected the unchanged three-round pipeline")
	}
	for _, logical := range manifests {
		filename, err := runfiles.Rlocation(logical)
		if err != nil {
			t.Fatal(err)
		}
		var manifest struct {
			Truncated bool
			Variants  map[string][]struct {
				Kind  string
				Names []string
			}
		}
		readHelperJSON(t, filename, &manifest)
		if manifest.Truncated {
			t.Fatal("hint fixture truncated the existing frontier")
		}
		for variant, queries := range manifest.Variants {
			for _, query := range queries {
				if query.Kind != "optional-token-hints" {
					continue
				}
				complete := true
				for _, name := range []string{"__MAPPED_HINT_B", "__MAPPED_HINT_C", "__MAPPED_HINT_D", "__MAPPED_HINT_E", "__MAPPED_HINT_F"} {
					complete = complete && slices.Contains(query.Names, name)
				}
				batched[variant] = batched[variant] || complete
			}
		}
	}
	for _, variant := range []string{"base", "irrelevant", "relevant"} {
		if !batched[variant] {
			t.Errorf("%s never batched the directive-separated names", variant)
		}
		root := runfileFromEnv(t, "FAMILY_SMOKE_"+strings.ToUpper(variant)+"_OBJECTS")
		data := numericReadFile(t, filepath.Join(root, "smoke.o"), 1<<20)
		if got := compiledObjectSymbolByte(t, data, "mapped_entered_hint_value"); got != 21 {
			t.Errorf("%s actual compiler witness = %d, want 21", variant, got)
		}
	}
	var report reuseReport
	readHelperJSON(t, runfileFromEnv(t, "FAMILY_SMOKE_REUSE_REPORT"), &report)
	assertCompilerOutputSharing(t, report, "smoke.o")
}

// The producing verifier replays unmodified results from the production probe
// map using the family-selected compiler, not an ambient test executable.
func TestFamilyOptionalDefinednessUsesRealCompilerResults(t *testing.T) {
	data, err := os.ReadFile(runfileFromEnv(t, "FAMILY_SMOKE_OPTIONAL_DEFINEDNESS"))
	if err != nil {
		t.Fatal(err)
	}
	type caseResult struct {
		NodeID   string          `json:"node_id"`
		State    string          `json:"state"`
		Values   map[string]bool `json:"values"`
		ExitCode int             `json:"exit_code"`
	}
	var got struct {
		Schema          string                      `json:"schema"`
		Toolset         string                      `json:"toolset"`
		CompilerVersion string                      `json:"compiler_version"`
		Cases           map[string]caseResult       `json:"cases"`
		Checks          []string                    `json:"checks"`
		MacroWrites     map[string]macroWriteResult `json:"macro_writes"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("receipt trailer: %v", err)
	}
	if got.Schema != "linux-optional-definedness-fixture-v1" ||
		!regexp.MustCompile(`^sha256-[0-9a-f]{64}$`).MatchString(got.Toolset) || strings.TrimSpace(got.CompilerVersion) == "" {
		t.Fatal("missing real configured compiler identity")
	}
	checks := []string{"empty-failure-snapshot", "exact-vector-identity", "rejected-only-singleton-miss", "fresh-singleton-answer", "mandatory-success", "mandatory-optional-separation", "ordinary-discovery-unchanged"}
	if !slices.Equal(got.Checks, checks) {
		t.Fatalf("incomplete runtime verification: %v", got.Checks)
	}
	const present, absent = "__LINUX_BZL_OPTIONAL_PRESENT", "__LINUX_BZL_OPTIONAL_ABSENT"
	want := map[string]caseResult{
		"rejected-singleton": {State: "unqueryable"},
		"rejected-pair":      {State: "unqueryable"},
		"valid-singleton":    {State: "answered", Values: map[string]bool{present: true}},
		"valid-vector":       {State: "answered", Values: map[string]bool{present: true, absent: false}},
		"mandatory-vector":   {State: "mandatory-answered", Values: map[string]bool{present: true, absent: false}},
	}
	if len(got.Cases) != len(want) {
		t.Fatal("runtime query cases missing or duplicated")
	}
	seen := map[string]bool{}
	for name, expected := range want {
		value, exists := got.Cases[name]
		if !exists || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(value.NodeID) || seen[value.NodeID] {
			t.Fatalf("%s has missing, malformed or aliased request identity", name)
		}
		seen[value.NodeID] = true
		if value.State != expected.State || !maps.Equal(value.Values, expected.Values) {
			t.Fatalf("%s returned wrong measured values/state", name)
		}
		if expected.State == "unqueryable" {
			if value.Values != nil || value.ExitCode < 1 || value.ExitCode > 255 {
				t.Fatalf("%s granted failed-process facts", name)
			}
		} else if value.ExitCode != 0 {
			t.Fatalf("%s compiler did not succeed", name)
		}
	}
	t.Logf("optional definedness verified against %s (%s)", got.CompilerVersion, got.Toolset)
}

type macroWriteResult struct {
	NodeID   string `json:"node_id"`
	Success  bool   `json:"success"`
	ExitCode int    `json:"exit_code"`
}

func TestFamilyConfigMacroWritesPreserveCompilerDiagnostics(t *testing.T) {
	var got struct {
		CompilerVersion string                      `json:"compiler_version"`
		MacroWrites     map[string]macroWriteResult `json:"macro_writes"`
	}
	readHelperJSON(t, runfileFromEnv(t, "FAMILY_SMOKE_OPTIONAL_DEFINEDNESS"), &got)
	if len(got.MacroWrites) != 3 || strings.TrimSpace(got.CompilerVersion) == "" {
		t.Fatal("missing actual compiler macro-write results")
	}
	seen := map[string]bool{}
	for name, success := range map[string]bool{"conflicting": false, "identical": true, "omitted": true} {
		result, ok := got.MacroWrites[name]
		if !ok || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(result.NodeID) || seen[result.NodeID] {
			t.Fatalf("%s has missing or aliased compiler request", name)
		}
		seen[result.NodeID] = true
		if result.Success != success || success && result.ExitCode != 0 || !success && (result.ExitCode < 1 || result.ExitCode > 255) {
			t.Fatalf("%s compiler outcome: %+v", name, result)
		}
	}
	t.Logf("CONFIG macro-write diagnostics verified against %s", got.CompilerVersion)
}
