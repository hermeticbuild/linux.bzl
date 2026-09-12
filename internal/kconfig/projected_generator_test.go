package kconfig

import (
	"bytes"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

const projectedGeneratorTestTarget = "arch/x86/include/generated/asm/cpufeaturemasks.h"
const projectedGeneratorTestScript = "arch/x86/tools/cpufeaturemasks.awk"
const projectedGeneratorTestFeatures = "arch/x86/include/asm/cpufeatures.h"

// This has the same input-tracking shape as arch/x86/tools/cpufeaturemasks.awk:
// the first positional file describes feature names, the final positional file
// is .config, and only anchored CONFIG_* records from that final file can affect
// the emitted guarded integer-macro header.
const projectedGeneratorTestAWK = `
BEGIN {
	file = 0
	printf "#ifndef _ASM_X86_CPUFEATUREMASKS_H\n"
	printf "#define _ASM_X86_CPUFEATUREMASKS_H\n"
}
FNR == 1 { ++file }
file == 1 {
	printf "#define FEATURE_%s %s\n", $1, $2
}
file == 2 && $1 ~ /^CONFIG_X86_(REQUIRED|DISABLED)_FEATURE_/ {
	printf "#define %s %s\n", $1, $2
}
END { printf "#endif\n" }
`

func TestCompactKbuildConfigSelectorPrefixesExpandAnchoredAlternation(t *testing.T) {
	got := compactKbuildConfigSelectorPrefixes([]byte(`
		/^CONFIG_ZETA_/
		/^CONFIG_X86_(REQUIRED|DISABLED)_FEATURE_/
		CONFIG_ZETA_
	`))
	want := []string{
		"CONFIG_X86_DISABLED_FEATURE_",
		"CONFIG_X86_REQUIRED_FEATURE_",
		"CONFIG_ZETA_",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("selector prefixes = %q, want %q", got, want)
	}
}

func TestConfigCapsulePreservesFirstConfigRecordBoundary(t *testing.T) {
	for _, test := range []struct {
		name, input, want string
	}{
		{"preamble", "# heading\n\nCONFIG_KEEP=y\nCONFIG_DROP=y\n", "# heading\n\nCONFIG_KEEP=y\n"},
		{"selected_first", "CONFIG_KEEP=y\nCONFIG_DROP=y\n", "CONFIG_KEEP=y\n"},
		{"omitted_first", "CONFIG_DROP=y\nCONFIG_KEEP=y\n", "\nCONFIG_KEEP=y\n"},
		{"unset_first", "# CONFIG_DROP is not set\nCONFIG_KEEP=y\n", "\nCONFIG_KEEP=y\n"},
		{"leading_blank", "\nCONFIG_DROP=y\nCONFIG_KEEP=y\n", "\nCONFIG_KEEP=y\n"},
		{"empty_selection", "CONFIG_DROP=y\n", "\n"},
		{"empty_file", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			full := resolvedConfigProjectionFixture()
			full[".config"] = test.input
			capsule, err := RenderConfigCapsule(full, ConfigDependencySet{Symbols: []string{"CONFIG_KEEP"}})
			if err != nil {
				t.Fatal(err)
			}
			if got := capsule.Files[".config"]; got != test.want {
				t.Fatalf("projected .config = %q, want %q", got, test.want)
			}
			opaque, err := RenderConfigCapsule(full, ConfigDependencySet{Opaque: true, Reason: "oracle"})
			if err != nil {
				t.Fatal(err)
			}
			if opaque.Files[".config"] != test.input {
				t.Fatal("opaque projection changed bytes")
			}
		})
	}
}

// This is the source-language shape that exposed the real comparator failure:
// the first file uses whitespace fields, and the first record of the second
// changes FS to '='. No generator or kernel path is special-cased.
const projectedGeneratorFirstRecordAWK = `
BEGIN {
	file = 0
	printf "#ifndef GENERATED_H\n#define GENERATED_H\n"
}
FNR == 1 {
	++file
	if (file == 1) FS = "[ \t()*+]+"
	if (file == 2) FS = "="
}
file == 1 { names[$1] = $2 }
file == 2 && $1 ~ /^CONFIG_SELECTED_/ {
	name = $1
	sub(/^CONFIG_SELECTED_/, "", name)
	selected[name] = ($2 == "y")
}
END {
	printf "#define FIRST %d\n", selected["FIRST"]
	printf "#define SECOND %d\n", selected["SECOND"]
	printf "#endif\n"
}
`

func TestProjectedGeneratorConfigRecordBoundaryWithDeclaredAwk(t *testing.T) {
	awk := projectedGeneratorAwkTestExecutable(t)
	if got := compactKbuildAwkConfigProjectionProof([]byte(projectedGeneratorFirstRecordAWK), 2); !slices.Equal(got, []string{"CONFIG_SELECTED_"}) {
		t.Fatalf("FS-switch source lost eligibility: %q", got)
	}
	for _, test := range []struct {
		name, config   string
		emptySelection bool
		first, second  string
	}{
		{"preamble", "# heading\n\nCONFIG_SELECTED_FIRST=y\nCONFIG_DROP=y\nCONFIG_SELECTED_SECOND=y\n", false, "1", "1"},
		{"selected_first", "CONFIG_SELECTED_FIRST=y\nCONFIG_DROP=y\nCONFIG_SELECTED_SECOND=y\n", false, "0", "1"},
		{"omitted_first", "CONFIG_DROP=y\nCONFIG_SELECTED_FIRST=y\nCONFIG_SELECTED_SECOND=y\n", false, "1", "1"},
		{"unset_first", "# CONFIG_DROP is not set\nCONFIG_SELECTED_FIRST=y\nCONFIG_SELECTED_SECOND=y\n", false, "1", "1"},
		{"leading_blank", "\nCONFIG_SELECTED_FIRST=y\nCONFIG_SELECTED_SECOND=y\n", false, "1", "1"},
		{"empty_selection", "CONFIG_DROP=y\nCONFIG_OTHER=y\n", true, "0", "0"},
		{"empty_file", "", true, "0", "0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			script := filepath.Join(directory, "generator.awk")
			features := filepath.Join(directory, "features")
			for filename, contents := range map[string]string{script: projectedGeneratorFirstRecordAWK, features: "FIRST 1\nSECOND 2\n"} {
				if err := os.WriteFile(filename, []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			full := resolvedConfigProjectionFixture()
			full[".config"] = test.config
			dependencies := ConfigDependencySet{Symbols: []string{"CONFIG_SELECTED_FIRST", "CONFIG_SELECTED_SECOND"}}
			if test.emptySelection {
				dependencies.Symbols = nil
			}
			capsule, err := RenderConfigCapsule(full, dependencies)
			if err != nil {
				t.Fatal(err)
			}
			run := func(name, config string) []byte {
				t.Helper()
				filename := filepath.Join(directory, name+".config")
				if err := os.WriteFile(filename, []byte(config), 0600); err != nil {
					t.Fatal(err)
				}
				command := exec.Command(awk, "-f", script, features, filename)
				command.Env = []string{"LC_ALL=C", "PATH="}
				var stderr bytes.Buffer
				command.Stderr = &stderr
				output, err := command.Output()
				if err != nil {
					t.Fatalf("AWK %s: %v: %s", name, err, stderr.String())
				}
				return output
			}
			fullOutput := run("full", test.config)
			projectedOutput := run("projected", capsule.Files[".config"])
			if !bytes.Equal(fullOutput, projectedOutput) {
				t.Fatalf("projected output differs:\nfull:\n%s\nprojected:\n%s", fullOutput, projectedOutput)
			}
			for _, expected := range []string{"#define FIRST " + test.first + "\n", "#define SECOND " + test.second + "\n"} {
				if !bytes.Contains(fullOutput, []byte(expected)) {
					t.Fatalf("full output %q lacks %q", fullOutput, expected)
				}
			}
		})
	}
}

func projectedGeneratorAwkTestExecutable(t *testing.T) string {
	t.Helper()
	if logical := os.Getenv("LINUX_BZL_TEST_AWK"); logical != "" {
		executable, err := runfiles.Rlocation(logical)
		if err != nil {
			t.Fatalf("resolve declared AWK runfile %q: %v", logical, err)
		}
		return executable
	}
	if os.Getenv("TEST_SRCDIR") != "" {
		t.Fatal("Bazel test must provide its declared AWK runfile")
	}
	executable, err := exec.LookPath("gawk")
	if err != nil {
		t.Skip("GNU AWK unavailable under direct go test; Bazel declares it")
	}
	return executable
}

func TestCompactKbuildAwkProjectionRejectsRecordAndPathObservations(t *testing.T) {
	awk := projectedGeneratorAwkTestExecutable(t)
	assertValidAWK := func(t *testing.T, source string) {
		t.Helper()
		command := exec.Command(awk, source)
		command.Env = []string{"LC_ALL=C", "PATH="}
		command.Stdin = strings.NewReader("")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("negative fixture is not valid AWK: %v\n%s\n%s", err, output, source)
		}
	}
	for name, replacement := range map[string]string{
		"NR config body":            "selected[name] = NR",
		"FNR config body":           "selected[name] = FNR",
		"division does not hide NR": "selected[name] = 4 / NR / 2",
		"path":                      "selected[name] = length(FILENAME)",
		"argument path":             "selected[name] = length(ARGV[2])",
		"argument count":            "selected[name] = ARGC",
		"record separator":          "RS = \";\"",
		"field pattern":             "FPAT = \"[A-Z]+\"",
	} {
		t.Run(name, func(t *testing.T) {
			source := strings.Replace(projectedGeneratorFirstRecordAWK, `selected[name] = ($2 == "y")`, replacement, 1)
			assertValidAWK(t, source)
			if got := compactKbuildAwkConfigProjectionProof([]byte(source), 2); len(got) != 0 {
				t.Fatalf("unsafe projection remains eligible: %q", got)
			}
		})
	}
	for _, body := range []string{
		`printf "#define RECORDS %d\n", NR`,
		`printf "#define RECORDS %d\n", FNR`,
		`printf "#define FIELD_COUNT %d\n", NF`,
		`printf "#define LAST %d\n", $1`,
		`printf "#define LAST %d\n", length()`,
		`printf "#define LAST %d\n", length`,
		`sub(/x/, "y")`,
		`if (/CONFIG_UNSELECTED/) flag = 1`,
	} {
		for _, pattern := range []string{"BEGIN", "END", "FNR == 1"} {
			source := strings.Replace(projectedGeneratorFirstRecordAWK, pattern+" {", pattern+" {\n"+body+"\n", 1)
			assertValidAWK(t, source)
			if got := compactKbuildAwkConfigProjectionProof([]byte(source), 2); len(got) != 0 {
				t.Fatalf("%s {%s} remains eligible: %q", pattern, body, got)
			}
		}
	}
	lateTracker := strings.Replace(projectedGeneratorFirstRecordAWK, "FNR == 1 {", "file == 1 { early = $1 }\nFNR == 1 {", 1)
	assertValidAWK(t, lateTracker)
	if got := compactKbuildAwkConfigProjectionProof([]byte(lateTracker), 2); len(got) != 0 {
		t.Fatalf("late first-record tracker remains eligible: %q", got)
	}
	changedPattern := strings.Replace(projectedGeneratorFirstRecordAWK, "file == 2 &&", "file == 2 && FNR > 1 &&", 1)
	assertValidAWK(t, changedPattern)
	if got := compactKbuildAwkConfigProjectionProof([]byte(changedPattern), 2); len(got) != 0 {
		t.Fatalf("record-sensitive pattern remains eligible: %q", got)
	}
}

func TestCompactKbuildAwkProjectionRejectsShiftedFileOrdinalWithDeclaredAwk(t *testing.T) {
	awk := projectedGeneratorAwkTestExecutable(t)
	for _, test := range []struct {
		name, initializer, tracker, extraRule, conditionExtra string
		unguardedOrdinal, fullCount, projectedCount           int
	}{
		{"assignment", "file = 0", "file = 3", "", "", 3, 4, 3},
		{"conditional_prefix", "file = 0", "if (file == 0) ++file", "", "", 1, 4, 3},
		{"conditional_postfix", "file = 0", "if (file == 0) file++", "", "", 1, 4, 3},
		{"nonzero_initializer", "file = 01", "++file", "", "", 3, 3, 2},
		{"initializer_expression", "file = 0 + 1", "++file", "", "", 3, 3, 2},
		{"initializer_concatenation", `file = 0 "1"`, "++file", "", "", 3, 3, 2},
		{"prefix_decrement", "file = 0", "++file; if (file == 2) --file", "", "", 1, 4, 3},
		{"postfix_decrement", "file = 0", "++file; if (file == 2) file--", "", "", 1, 4, 3},
		{"begin_decrement", "file = 0; --file", "++file", "", "", 1, 3, 2},
		{"exponent_assignment", "file = 0", "++file; if (file == 2) file ^= 0", "", "", 1, 4, 3},
		{"for_in_tracker", "file = 0; slots[1] = 1", "++file; for (file in slots) {}", "", "", 1, 4, 3},
		{"for_in_record", "file = 0", "++file", "file == 1 { slots[3] = 1; for (file in slots) {} }", "", 4, 3, 2},
		{"sub_destination", "file = 0", "++file", `file == 1 { sub(/1/, "3", file) }`, "", 4, 3, 2},
		{"gsub_destination", "file = 0", "++file", `file == 1 { gsub(/1/, "3", file) }`, "", 4, 3, 2},
		{"pattern_increment", "file = 0", "++file", "", "++file > 0 && ", 3, 3, 2},
		{"division_masked_increment", "file = 0", "++file", "file == 1 { other = 1 / file++ / 2 }", "", 3, 3, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := "BEGIN {\n" + test.initializer + `
	printf "#ifndef GENERATED_H\n#define GENERATED_H\n"
}
FNR == 1 { ` + test.tracker + ` }
` + test.extraRule + `
file == 2 && ` + test.conditionExtra + `$1 ~ /^CONFIG_KEEP_/ { kept++ }
file == ` + strconv.Itoa(test.unguardedOrdinal) + ` { seen++ }
END { printf "#define OBSERVED %d\n#endif\n", seen }
`
			directory := t.TempDir()
			script, first := filepath.Join(directory, "generator.awk"), filepath.Join(directory, "first")
			for filename, contents := range map[string]string{script: source, first: "seed\n"} {
				if err := os.WriteFile(filename, []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			full := resolvedConfigProjectionFixture()
			full[".config"] = "# heading\nCONFIG_KEEP_A=y\nCONFIG_DROP=y\n"
			capsule, err := RenderConfigCapsule(full, ConfigDependencySet{Symbols: []string{"CONFIG_KEEP_A"}})
			if err != nil {
				t.Fatal(err)
			}
			run := func(name, contents string) []byte {
				t.Helper()
				config := filepath.Join(directory, name+".config")
				if err := os.WriteFile(config, []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
				command := exec.Command(awk, "-f", script, first, config)
				command.Env = []string{"LC_ALL=C", "PATH="}
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("valid AWK %s: %v\n%s", name, err, output)
				}
				return output
			}
			fullOutput, projectedOutput := run("full", full[".config"]), run("projected", capsule.Files[".config"])
			if bytes.Equal(fullOutput, projectedOutput) {
				t.Fatal("counterexample no longer distinguishes the filtered input")
			}
			for _, expected := range []struct {
				output []byte
				count  int
			}{{fullOutput, test.fullCount}, {projectedOutput, test.projectedCount}} {
				if !bytes.Contains(expected.output, []byte("#define OBSERVED "+strconv.Itoa(expected.count)+"\n")) {
					t.Fatalf("unexpected oracle output %q", expected.output)
				}
			}
			t.Logf("actual AWK: full=%d projected=%d", test.fullCount, test.projectedCount)
			if got := compactKbuildAwkConfigProjectionProof([]byte(source), 2); len(got) != 0 {
				t.Fatalf("incorrect file ordinal remains eligible: %q", got)
			}
		})
	}
}

func TestCompactKbuildAwkProjectionRequiresStandaloneCounterAndZero(t *testing.T) {
	awk := projectedGeneratorAwkTestExecutable(t)
	for _, tracker := range []string{
		"++file", "file++", "++ file;", "file ++;", "# comment with \"quotes\"\n++file; # after counter\n",
	} {
		source := strings.Replace(projectedGeneratorFirstRecordAWK, "++file", tracker, 1)
		if got := compactKbuildAwkConfigProjectionProof([]byte(source), 2); !slices.Equal(got, []string{"CONFIG_SELECTED_"}) {
			t.Errorf("standalone tracker %q lost eligibility: %q", tracker, got)
		}
	}
	for _, initializer := range []string{"file = 0", "file=0;", "file = 0 # comment\n"} {
		// The actual upstream BEGIN prints its header before initializing file.
		source := strings.Replace(projectedGeneratorFirstRecordAWK, "file = 0", "printf \"/* before initializer */\\n\"; "+initializer, 1)
		if got := compactKbuildAwkConfigProjectionProof([]byte(source), 2); len(got) == 0 {
			t.Errorf("zero initializer %q lost eligibility", initializer)
		}
	}
	for _, test := range []struct{ name, old, replacement string }{
		{"assignment", "++file", "file = 3"},
		{"compound_assignment", "++file", "file += 1"},
		{"conditional", "++file", "if (file == 0) ++file"},
		{"nested", "++file", "{ ++file }"},
		{"value_expression", "++file", "other = ++file"},
		{"suffix_expression", "++file", "file++ + 1"},
		{"suffix_concatenation", "++file", `file++ "1"`},
		{"late", "++file", "other = 1; ++file"},
		{"nonzero", "file = 0", "file = 01"},
		{"expression", "file = 0", "file = 0 + 1"},
		{"decimal", "file = 0", "file = 0.5"},
		{"concatenation", "file = 0", `file = 0 "1"`},
		{"quoted_initializer", "file = 0", `other = "file = 0"; file = 1`},
		{"other_variable", "file = 0", "otherfile = 0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := strings.Replace(projectedGeneratorFirstRecordAWK, test.old, test.replacement, 1)
			command := exec.Command(awk, source)
			command.Env = []string{"LC_ALL=C", "PATH="}
			command.Stdin = strings.NewReader("")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("negative fixture is not valid AWK: %v\n%s", err, output)
			}
			if got := compactKbuildAwkConfigProjectionProof([]byte(source), 2); len(got) != 0 {
				t.Fatalf("unsupported counter remains eligible: %q", got)
			}
		})
	}
}

func TestCompactKbuildAwkProjectionRejectsEveryCounterAssignmentOperator(t *testing.T) {
	awk := projectedGeneratorAwkTestExecutable(t)
	for _, mutation := range []string{
		"--file", "file--", "++file", "file++", "file = 1", "file += 1", "file -= 1",
		"file *= 2", "file /= 2", "file %= 2", "file ^= 2", "file **= 2",
	} {
		t.Run(mutation, func(t *testing.T) {
			if !compactKbuildAwkFileAssignment.MatchString(mutation) {
				t.Fatalf("mutation detector missed %q", mutation)
			}
			source := strings.Replace(projectedGeneratorFirstRecordAWK, "++file", "++file; "+mutation, 1)
			command := exec.Command(awk, source)
			command.Env = []string{"LC_ALL=C", "PATH="}
			command.Stdin = strings.NewReader("")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("negative fixture is not valid GNU AWK: %v\n%s", err, output)
			}
			if got := compactKbuildAwkConfigProjectionProof([]byte(source), 2); len(got) != 0 {
				t.Fatalf("extra counter write remains eligible: %q", got)
			}
		})
	}
}

func TestCompactKbuildAwkProjectionRequiresAffirmativeConjunctsWithDeclaredAwk(t *testing.T) {
	awk := projectedGeneratorAwkTestExecutable(t)
	for _, test := range []struct {
		name, pattern             string
		fullCount, projectedCount int
		eligible                  bool
	}{
		{"negated_ordinal", `!(file == 1)`, 3, 2, false},
		{"ordinal_comparison_negation", `(file == 1) == 0`, 3, 2, false},
		{"ordinal_arithmetic", `file == 1 + 1`, 3, 2, false},
		{"negated_selector", `file == 2 && !($1 ~ /^CONFIG_KEEP_/)`, 2, 1, false},
		{"selector_comparison_negation", `file == 2 && ($1 ~ /^CONFIG_KEEP_/) == 0`, 2, 1, false},
		{"ternary", `file == 2 && $1 ~ /^CONFIG_KEEP_/ ? 1 : 1`, 4, 3, false},
		{"range", `file == 2 && $1 ~ /^CONFIG_KEEP_/, /^NO_END$/`, 2, 1, false},
		{"or", `file == 2 && $1 ~ /^CONFIG_KEEP_/ || 1`, 4, 3, false},
		{"division_hidden_or", `file == 2 && 1 / 1 || 1 / 1 && $1 ~ /^CONFIG_KEEP_/`, 3, 2, false},
		{"regex_punctuation", `file == 1 && $1 ~ /^seed(||[,?]|&&)$/`, 1, 1, true},
		{"literal_punctuation", `file == 1 && $1 == "seed&&||?,value"`, 0, 0, true},
		{"selector_then_literal", `file == 2 && $1 ~ /^CONFIG_KEEP_/ && $1 == "CONFIG_KEEP_A=y"`, 1, 1, true},
		{"literal_then_selector", `file == 2 && $1 == "CONFIG_KEEP_A=y" && $1 ~ /^CONFIG_KEEP_/`, 1, 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := `BEGIN { file = 0; printf "#ifndef GENERATED_H\n#define GENERATED_H\n" }
FNR == 1 { ++file }
file == 2 && $1 ~ /^CONFIG_KEEP_/ { kept++ }
` + test.pattern + ` { seen++ }
END { printf "#define OBSERVED %d\n#endif\n", seen }
`
			directory := t.TempDir()
			script, first := filepath.Join(directory, "generator.awk"), filepath.Join(directory, "first")
			for filename, contents := range map[string]string{script: source, first: "seed\n"} {
				if err := os.WriteFile(filename, []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			full := resolvedConfigProjectionFixture()
			full[".config"] = "# heading\nCONFIG_KEEP_A=y\nCONFIG_DROP=y\n"
			capsule, err := RenderConfigCapsule(full, ConfigDependencySet{Symbols: []string{"CONFIG_KEEP_A"}})
			if err != nil {
				t.Fatal(err)
			}
			outputs := [][]byte{}
			for index, config := range []string{full[".config"], capsule.Files[".config"]} {
				filename := filepath.Join(directory, strconv.Itoa(index)+".config")
				if err := os.WriteFile(filename, []byte(config), 0o600); err != nil {
					t.Fatal(err)
				}
				command := exec.Command(awk, "-f", script, first, filename)
				command.Env = []string{"LC_ALL=C", "PATH="}
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("invalid GNU AWK oracle: %v\n%s", err, output)
				}
				count := []int{test.fullCount, test.projectedCount}[index]
				if !bytes.Contains(output, []byte("#define OBSERVED "+strconv.Itoa(count)+"\n")) {
					t.Fatalf("unexpected oracle output %q", output)
				}
				outputs = append(outputs, output)
			}
			if bytes.Equal(outputs[0], outputs[1]) != test.eligible {
				t.Fatal("oracle did not distinguish unsafe pattern from supported conjunction")
			}
			t.Logf("actual AWK: full=%d projected=%d", test.fullCount, test.projectedCount)
			prefixes := compactKbuildAwkConfigProjectionProof([]byte(source), 2)
			if test.eligible {
				if !slices.Equal(prefixes, []string{"CONFIG_KEEP_"}) {
					t.Fatalf("supported affirmative conjunction lost eligibility: %q", prefixes)
				}
			} else if len(prefixes) != 0 {
				t.Fatalf("non-affirmative pattern remains eligible: %q", prefixes)
			}
		})
	}
}

func TestCompactKbuildAwkProjectionProofRequiresClosedLiteralEmitter(t *testing.T) {
	if !compactKbuildLiteralClosedMacroEmitterEvidence([]byte(projectedGeneratorTestAWK)) {
		t.Fatal("cpufeaturemasks-shaped AWK did not prove a literal closed-macro emitter")
	}
	if got, want := compactKbuildAwkConfigProjectionProof([]byte(projectedGeneratorTestAWK), 2), []string{
		"CONFIG_X86_DISABLED_FEATURE_",
		"CONFIG_X86_REQUIRED_FEATURE_",
	}; !slices.Equal(got, want) {
		t.Fatalf("AWK config projection = %q, want %q", got, want)
	}

	unsafe := []struct {
		name   string
		source string
	}{
		{
			name:   "print primitive",
			source: strings.Replace(projectedGeneratorTestAWK, `printf "#define _ASM_X86_CPUFEATUREMASKS_H\n"`, `print "#define _ASM_X86_CPUFEATUREMASKS_H"`, 1),
		},
		{
			name:   "nonliteral printf format",
			source: strings.Replace(projectedGeneratorTestAWK, `printf "#ifndef _ASM_X86_CPUFEATUREMASKS_H\n"`, `printf header`, 1),
		},
		{
			name: "system call",
			source: projectedGeneratorTestAWK + `
END { system("true") }
`,
		},
		{
			name: "getline",
			source: projectedGeneratorTestAWK + `
END { getline extra }
`,
		},
	}
	for _, test := range unsafe {
		t.Run(test.name, func(t *testing.T) {
			if compactKbuildLiteralClosedMacroEmitterEvidence([]byte(test.source)) {
				t.Fatalf("unsafe emitter unexpectedly proved: %s", test.source)
			}
		})
	}

	unbounded := []struct {
		name   string
		source string
	}{
		{
			name:   "unanchored selector",
			source: strings.Replace(projectedGeneratorTestAWK, `/^CONFIG_X86_(REQUIRED|DISABLED)_FEATURE_/`, `/CONFIG_X86_(REQUIRED|DISABLED)_FEATURE_/`, 1),
		},
		{
			name:   "unguarded config records",
			source: strings.Replace(projectedGeneratorTestAWK, `file == 2 && $1 ~ /^CONFIG_X86_(REQUIRED|DISABLED)_FEATURE_/`, `file == 2`, 1),
		},
		{
			name:   "disjunctive file guard",
			source: strings.Replace(projectedGeneratorTestAWK, `file == 2 && $1 ~`, `file == 2 || $1 ~`, 1),
		},
		{
			name:   "unknown call",
			source: strings.Replace(projectedGeneratorTestAWK, `printf "#define %s %s\n", $1, $2`, `emit($1, $2)`, 1),
		},
	}
	for _, test := range unbounded {
		t.Run(test.name, func(t *testing.T) {
			if got := compactKbuildAwkConfigProjectionProof([]byte(test.source), 2); len(got) != 0 {
				t.Fatalf("unbounded AWK projection = %q, want ineligible", got)
			}
		})
	}
}

func projectedFilechkSourceFixture(t *testing.T) (*compactKbuildRulePlanBuilder, compactKbuildRecipeCommand, []compactKbuildRuleInput) {
	t.Helper()
	const script = projectedGeneratorTestScript
	profile := mustCompactKbuildProfileForTest(t, "prep:featuremasks", "arch/x86/Makefile", "", "all:\n", nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, script, projectedGeneratorTestFeatures)
	physical, ok := ResolveCompactKbuildProfileSourcePath(profile, script)
	if !ok {
		t.Fatalf("cannot resolve immutable AWK source %q", script)
	}
	if err := os.WriteFile(physical, []byte(projectedGeneratorTestAWK), 0o644); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{configFragment: map[string]string{"CONFIG_X86_REQUIRED_FEATURE_FPU": "y"}}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}, metadata: metadata}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forProfile(profile).
		forOutput("prep", "prep", "sdk")
	command := compactKbuildRecipeCommand{
		program:         "awk",
		programToolRole: "awk",
		arguments:       []string{"-f", script, projectedGeneratorTestFeatures, ".config"},
	}
	inputs := []compactKbuildRuleInput{}
	for _, pathname := range []string{script, projectedGeneratorTestFeatures} {
		id, err := metadata.ensureActionPlanSource(plan, pathname)
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, compactKbuildRuleInput{path: pathname, sourceID: id})
	}
	return &builder, command, inputs
}

func TestProjectedFilechkProofIsBoundToImmutableFinalConfigAwkInput(t *testing.T) {
	builder, command, inputs := projectedFilechkSourceFixture(t)
	want := []string{"CONFIG_X86_DISABLED_FEATURE_", "CONFIG_X86_REQUIRED_FEATURE_"}
	if got := builder.projectedFilechkConfigPrefixes([]compactKbuildRecipeCommand{command}, inputs); !slices.Equal(got, want) {
		t.Fatalf("immutable AWK projection = %q, want %q", got, want)
	}

	generatedScript := slices.Clone(inputs)
	generatedScript[0].sourceID = ""
	generatedScript[0].producer = "generated-script"
	if got := builder.projectedFilechkConfigPrefixes([]compactKbuildRecipeCommand{command}, generatedScript); len(got) != 0 {
		t.Fatalf("generated AWK script projection = %q, want ineligible", got)
	}
	configNotFinal := command
	configNotFinal.arguments = []string{"-f", projectedGeneratorTestScript, ".config", projectedGeneratorTestFeatures}
	if got := builder.projectedFilechkConfigPrefixes([]compactKbuildRecipeCommand{configNotFinal}, inputs); len(got) != 0 {
		t.Fatalf("non-final config operand projection = %q, want ineligible", got)
	}
}

func TestProjectedFilechkProofRequiresNonemptyImmutablePrecedingInputs(t *testing.T) {
	for _, name := range []string{
		"nonempty", "empty", "missing", "directory", "unbound", "generated", "object_tree", "working_only",
		"recipe_local", "overwrite", "unknown_source", "wrong_source", "wrong_namespace", "duplicate_binding", "generated_shadow",
	} {
		t.Run(name, func(t *testing.T) {
			builder, command, inputs := projectedFilechkSourceFixture(t)
			physical, ok := ResolveCompactKbuildProfileSourcePath(*builder.profile, projectedGeneratorTestFeatures)
			if !ok {
				t.Fatal("cannot resolve immutable preceding source")
			}
			switch name {
			case "empty":
				if err := os.WriteFile(physical, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing", "directory":
				if err := os.Remove(physical); err != nil {
					t.Fatal(err)
				}
				if name == "directory" {
					if err := os.Mkdir(physical, 0o700); err != nil {
						t.Fatal(err)
					}
				}
			case "unbound":
				inputs = inputs[:1]
			case "generated":
				inputs[1].producer, inputs[1].sourceID = "generated-data", ""
			case "object_tree":
				inputs[1].objectTree = true
			case "working_only":
				inputs[1].workingOnly = true
			case "recipe_local":
				inputs[1].recipeLocal = true
			case "overwrite":
				inputs[1].overwriteLineage = true
			case "unknown_source":
				inputs[1].sourceID = "src-99999999"
			case "wrong_source":
				inputs[1].sourceID = inputs[0].sourceID
			case "wrong_namespace":
				id, err := ensureActionPlanSource(builder.plan, "different-source", projectedGeneratorTestFeatures)
				if err != nil {
					t.Fatal(err)
				}
				inputs[1].sourceID = id
			case "duplicate_binding":
				inputs = append(inputs, inputs[1])
			case "generated_shadow":
				inputs = append(inputs, compactKbuildRuleInput{path: projectedGeneratorTestFeatures, producer: "generated-shadow"})
			}
			got := builder.projectedFilechkConfigPrefixes([]compactKbuildRecipeCommand{command}, inputs)
			if name == "nonempty" {
				if !slices.Equal(got, []string{"CONFIG_X86_DISABLED_FEATURE_", "CONFIG_X86_REQUIRED_FEATURE_"}) {
					t.Fatalf("ordinary nonempty source lost eligibility: %q", got)
				}
			} else if len(got) != 0 {
				t.Fatalf("unproven preceding input remains eligible: %q", got)
			}
		})
	}
	t.Run("second_preceding_operand", func(t *testing.T) {
		builder, command, inputs := projectedFilechkSourceFixture(t)
		const second = "arch/x86/include/asm/empty-features.h"
		physical, ok := ResolveCompactKbuildProfileSourcePath(*builder.profile, second)
		if !ok {
			t.Fatal("cannot resolve second source")
		}
		if err := os.WriteFile(physical, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		id, err := builder.metadata.ensureActionPlanSource(builder.plan, second)
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, compactKbuildRuleInput{path: second, sourceID: id})
		command.arguments = []string{"-f", projectedGeneratorTestScript, projectedGeneratorTestFeatures, second, ".config"}
		script, ok := ResolveCompactKbuildProfileSourcePath(*builder.profile, projectedGeneratorTestScript)
		if !ok {
			t.Fatal("cannot resolve script")
		}
		source := strings.Replace(projectedGeneratorTestAWK, "file == 2 &&", "file == 3 &&", 1)
		if err := os.WriteFile(script, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := builder.projectedFilechkConfigPrefixes([]compactKbuildRecipeCommand{command}, inputs); len(got) != 0 {
			t.Fatalf("empty second operand remains eligible: %q", got)
		}
		if err := os.WriteFile(physical, []byte("nonempty\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := builder.projectedFilechkConfigPrefixes([]compactKbuildRecipeCommand{command}, inputs); len(got) == 0 {
			t.Fatal("two proven nonempty sources lost eligibility")
		}
	})
}

func TestProjectedFilechkEmptyFirstInputMismatchWithDeclaredAwk(t *testing.T) {
	awk := projectedGeneratorAwkTestExecutable(t)
	builder, command, inputs := projectedFilechkSourceFixture(t)
	const source = `BEGIN { file = 0; printf "#ifndef GENERATED_H\n#define GENERATED_H\n" }
FNR == 1 { ++file }
file == 1 { seen++ }
file == 2 && $1 ~ /^CONFIG_KEEP_/ { kept++ }
END { printf "#define OBSERVED %d\n#endif\n", seen }
`
	script, _ := ResolveCompactKbuildProfileSourcePath(*builder.profile, projectedGeneratorTestScript)
	first, _ := ResolveCompactKbuildProfileSourcePath(*builder.profile, projectedGeneratorTestFeatures)
	if err := os.WriteFile(script, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := compactKbuildAwkConfigProjectionProof([]byte(source), 2); !slices.Equal(got, []string{"CONFIG_KEEP_"}) {
		t.Fatalf("oracle must have valid pattern syntax: %q", got)
	}
	full := resolvedConfigProjectionFixture()
	full[".config"] = "# heading\nCONFIG_KEEP_A=y\nCONFIG_DROP=y\n"
	capsule, err := RenderConfigCapsule(full, ConfigDependencySet{Symbols: []string{"CONFIG_KEEP_A"}})
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	outputs := [][]byte{}
	for index, contents := range []string{full[".config"], capsule.Files[".config"]} {
		config := filepath.Join(directory, strconv.Itoa(index)+".config")
		if err := os.WriteFile(config, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(awk, "-f", script, first, config)
		cmd.Env = []string{"LC_ALL=C", "PATH="}
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("GNU AWK oracle failed: %v\n%s", err, output)
		}
		if !bytes.Contains(output, []byte("#define OBSERVED "+strconv.Itoa(3-index)+"\n")) {
			t.Fatalf("unexpected oracle output %q", output)
		}
		outputs = append(outputs, output)
	}
	if bytes.Equal(outputs[0], outputs[1]) {
		t.Fatal("empty preceding file no longer exposes ordinal mismatch")
	}
	t.Log("actual AWK with empty first input: full=3 projected=2")
	if got := builder.projectedFilechkConfigPrefixes([]compactKbuildRecipeCommand{command}, inputs); len(got) != 0 {
		t.Fatalf("empty preceding input remains eligible: %q", got)
	}
}

func projectedGeneratorPlanForTest(t testing.TB, used, other string) (*ActionPlan, string, string) {
	t.Helper()
	fragment := map[string]string{"CONFIG_OTHER": other}
	if used != "n" {
		fragment["CONFIG_USED"] = used
	}
	metadata := &CompactMetadata{
		configFragment:       fragment,
		configSymbolUniverse: []string{"CONFIG_OTHER", "CONFIG_USED"},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": "sha256-" + strings.Repeat("1", 64)},
		Sources: []ActionPlanSource{
			{ID: "src-00000001", Namespace: "kernel", Path: "arch/x86/tools/cpufeaturemasks.awk"},
			{ID: "src-00000002", Namespace: "kernel", Path: "arch/x86/include/asm/cpufeatures.h"},
			{ID: "src-00000003", Namespace: "config", Path: ".config"},
		},
		Recipes:  map[string]ActionRecipe{},
		metadata: metadata,
	}
	configCopyRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
		Arguments: []string{"-input", "${source:input:00000000}", "-out", "${output:00000000}"},
		Sources:   []string{"input:00000000"}, Outputs: []string{"00000000"},
	}
	configCopy, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "prep", Kind: "copy", Tool: "actionfile", Product: "sdk",
		Sources: []ActionPlanSourceEdge{{Role: "input", SourceID: "src-00000003"}},
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: ".config"}},
	}, configCopyRecipe)
	if err != nil {
		t.Fatal(err)
	}

	generatorRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "awk",
		Arguments: []string{
			"-f", "${source:script:00000000}",
			"${source:features:00000001}", "${input:config:00000000}",
		},
		WorkingDirectory: "feature-masks",
		WorkingInputs: map[string]string{
			"source:script:00000000":   "arch/x86/tools/cpufeaturemasks.awk",
			"source:features:00000001": "arch/x86/include/asm/cpufeatures.h",
			"input:config:00000000":    ".config",
		},
		Sources: []string{"script:00000000", "features:00000001"},
		Inputs:  []string{"config:00000000"}, Outputs: []string{"00000000"},
		Stdout: "00000000",
	}
	generator, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "prep", Kind: "generate", Tool: "awk", Product: "sdk",
		Sources: []ActionPlanSourceEdge{
			{Role: "script", SourceID: "src-00000001"},
			{Role: "features", SourceID: "src-00000002"},
		},
		Inputs:  []ActionPlanNodeEdge{{Role: "config", ProducerID: configCopy, Slot: 0}},
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: projectedGeneratorTestTarget}},
	}, generatorRecipe)
	if err != nil {
		t.Fatal(err)
	}
	plan.projectedGeneratorCandidates = map[string]projectedGeneratorCandidate{generator: {
		TargetPath: projectedGeneratorTestTarget, TargetSlot: 0,
		ConfigProjectionPrefixes: []string{"CONFIG_USED"},
	}}

	consumerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
		Arguments: []string{"-input", "${input:header:00000000}", "-out", "${output:00000000}"},
		Inputs:    []string{"header:00000000"}, Outputs: []string{"00000000"},
	}
	consumer, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "target", Kind: "copy", Tool: "actionfile", Product: "image",
		Inputs:  []ActionPlanNodeEdge{{Role: "header", ProducerID: generator, Slot: 0}},
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/projected-generator.o"}},
	}, consumerRecipe)
	if err != nil {
		t.Fatal(err)
	}
	plan.Products = []ActionPlanProduct{{Name: "image", Tree: "objects", Path: LinuxKernelTreeRootMarker}}
	return plan, generator, consumer
}

func projectedGeneratorNodeForTest(t testing.TB, plan *ActionPlan, id string) ActionPlanNode {
	t.Helper()
	for _, node := range plan.Nodes {
		if node.ID == id {
			return node
		}
	}
	t.Fatalf("missing action-plan node %s", id)
	return ActionPlanNode{}
}

func projectedGeneratorBatchPlanForTest(
	t testing.TB,
	candidateCount int,
	extraConsumerCount int,
) (*ActionPlan, []string, []string) {
	t.Helper()
	if candidateCount < 1 {
		t.Fatalf("projected-generator batch requires at least one candidate, got %d", candidateCount)
	}
	plan, firstGeneratorID, firstConsumerID := projectedGeneratorPlanForTest(t, "y", "n")
	generatorTemplate := projectedGeneratorNodeForTest(t, plan, firstGeneratorID)
	generatorRecipe := cloneActionRecipe(plan.Recipes[generatorTemplate.Recipe])
	consumerTemplate := projectedGeneratorNodeForTest(t, plan, firstConsumerID)
	consumerRecipe := cloneActionRecipe(plan.Recipes[consumerTemplate.Recipe])
	generatorIDs := []string{firstGeneratorID}
	consumerIDs := []string{firstConsumerID}

	for index := 1; index < candidateCount; index++ {
		target := "arch/x86/include/generated/asm/cpufeaturemasks-" + strconv.Itoa(index) + ".h"
		generator := generatorTemplate
		generator.ID = ""
		generator.Sources = slices.Clone(generatorTemplate.Sources)
		generator.Inputs = slices.Clone(generatorTemplate.Inputs)
		generator.Outputs = []ActionPlanOutput{{Tree: "prep", Path: target}}
		generatorID, err := appendActionPlanNode(plan, generator, generatorRecipe)
		if err != nil {
			t.Fatalf("append projected generator %d: %v", index, err)
		}
		plan.projectedGeneratorCandidates[generatorID] = projectedGeneratorCandidate{
			TargetPath: target, TargetSlot: 0,
			ConfigProjectionPrefixes: []string{"CONFIG_USED"},
		}

		consumer := consumerTemplate
		consumer.ID = ""
		consumer.Inputs = []ActionPlanNodeEdge{{Role: "header", ProducerID: generatorID, Slot: 0}}
		consumer.Outputs = []ActionPlanOutput{{
			Tree: "objects", Path: "drivers/projected-generator-" + strconv.Itoa(index) + ".o",
		}}
		consumerID, err := appendActionPlanNode(plan, consumer, consumerRecipe)
		if err != nil {
			t.Fatalf("append projected generator consumer %d: %v", index, err)
		}
		generatorIDs = append(generatorIDs, generatorID)
		consumerIDs = append(consumerIDs, consumerID)
	}

	for index := 0; index < extraConsumerCount; index++ {
		consumer := consumerTemplate
		consumer.ID = ""
		consumer.Inputs = []ActionPlanNodeEdge{{Role: "header", ProducerID: firstGeneratorID, Slot: 0}}
		consumer.Outputs = []ActionPlanOutput{{
			Tree: "objects", Path: "drivers/projected-generator-extra-" + strconv.Itoa(index) + ".o",
		}}
		if _, err := appendActionPlanNode(plan, consumer, consumerRecipe); err != nil {
			t.Fatalf("append extra projected generator consumer %d: %v", index, err)
		}
	}
	return plan, generatorIDs, consumerIDs
}

func TestProjectedGeneratorSnapshotLoweringBatchesMultipleCandidates(t *testing.T) {
	const candidateCount = 12
	plan, generatorIDs, consumerIDs := projectedGeneratorBatchPlanForTest(t, candidateCount, 128)
	originalCount := len(plan.Nodes)
	plan.ensureNodeLookupIndexes()
	if plan.nodeLookupCount != originalCount {
		t.Fatalf("pre-lowering lookup count = %d, want %d", plan.nodeLookupCount, originalCount)
	}

	if err := lowerProjectedGeneratorsForFamilySnapshot(plan); err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), originalCount+3*candidateCount; got != want {
		t.Fatalf("lowered batch node count = %d, want %d", got, want)
	}
	if got := len(plan.projectedGeneratorValidations); got != candidateCount {
		t.Fatalf("lowered batch validation count = %d, want %d", got, candidateCount)
	}
	for index := range generatorIDs {
		raw := projectedGeneratorNodeForTest(t, plan, generatorIDs[index])
		if !strings.HasPrefix(raw.Outputs[0].ArtifactPath, compactKbuildProjectedFilechkRoot+"/") {
			t.Fatalf("projected generator %d retained canonical output %#v", index, raw.Outputs[0])
		}
		consumer := projectedGeneratorNodeForTest(t, plan, consumerIDs[index])
		if got := consumer.Inputs[0].ProducerID; got == generatorIDs[index] || got == "" {
			t.Fatalf("consumer %d producer = %q, want canonical validator", index, got)
		}
		validationIndex := originalCount + index*3
		full, canonical, stamp := plan.Nodes[validationIndex], plan.Nodes[validationIndex+1], plan.Nodes[validationIndex+2]
		if stamp.ID != plan.projectedGeneratorValidations[index] ||
			consumer.Inputs[0].ProducerID != canonical.ID ||
			stamp.Inputs[0].ProducerID != raw.ID || stamp.Inputs[1].ProducerID != full.ID {
			t.Fatalf("candidate %d validation suffix is inconsistent: full=%#v canonical=%#v stamp=%#v", index, full, canonical, stamp)
		}
	}

	// Edge rewrites deliberately invalidate the cache after lowering. Its one
	// final rebuild must cover both the original graph and the whole appended
	// validation suffix without stale pre-lowering output publishers.
	plan.ensureNodeLookupIndexes()
	if plan.nodeLookupCount != len(plan.Nodes) || len(plan.nodesByID) != len(plan.Nodes) {
		t.Fatalf("post-lowering lookup index has %d/%d nodes for %d-plan-node graph", plan.nodeLookupCount, len(plan.nodesByID), len(plan.Nodes))
	}
	for _, node := range plan.Nodes {
		if _, ok := plan.nodesByID[node.ID]; !ok {
			t.Fatalf("post-lowering lookup index omits node %s", node.ID)
		}
	}
}

func BenchmarkProjectedGeneratorSnapshotLowering(b *testing.B) {
	const extraConsumerCount = 4096
	for _, candidateCount := range []int{1, 16, 64} {
		b.Run("candidates_"+strconv.Itoa(candidateCount), func(b *testing.B) {
			template, _, _ := projectedGeneratorBatchPlanForTest(b, candidateCount, extraConsumerCount)
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				b.StopTimer()
				plan := cloneActionPlan(template)
				b.StartTimer()
				if err := lowerProjectedGeneratorsForFamilySnapshot(plan); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestProjectedGeneratorCandidateIsAbsentFromOrdinaryPlanIdentity(t *testing.T) {
	plan, generatorID, _ := projectedGeneratorPlanForTest(t, "y", "n")
	if got, want := len(plan.Nodes), 3; got != want {
		t.Fatalf("ordinary candidate node count = %d, want %d", got, want)
	}
	generator := projectedGeneratorNodeForTest(t, plan, generatorID)
	recipe := plan.Recipes[generator.Recipe]
	if len(recipe.ConfigProjectionPrefixes) != 0 {
		t.Fatalf("ordinary generator serialized projection marker = %q, want none", recipe.ConfigProjectionPrefixes)
	}
	if got, err := recipe.ID(); err != nil || got != generator.Recipe {
		t.Fatalf("ordinary generator recipe ID = %q, %v; want %q", got, err, generator.Recipe)
	}
	if got := generator.ContentID(); got != generator.ID {
		t.Fatalf("ordinary generator action ID = %q, want %q", generator.ID, got)
	}
	if got, want := plan.projectedGeneratorCandidates[generatorID], (projectedGeneratorCandidate{
		TargetPath: projectedGeneratorTestTarget, TargetSlot: 0,
		ConfigProjectionPrefixes: []string{"CONFIG_USED"},
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("planner-only candidate = %#v, want %#v", got, want)
	}
	if len(plan.projectedGeneratorValidations) != 0 || len(plan.projectedGeneratorInternalNodes) != 0 {
		t.Fatalf("ordinary plan unexpectedly contains replay state: validations=%q internals=%v", plan.projectedGeneratorValidations, plan.projectedGeneratorInternalNodes)
	}

	withoutCandidate := cloneActionPlan(plan)
	withoutCandidate.projectedGeneratorCandidates = nil
	withEntries, err := plan.entries()
	if err != nil {
		t.Fatal(err)
	}
	withoutEntries, err := withoutCandidate.entries()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(withEntries, withoutEntries) {
		t.Fatal("planner-only projected-generator provenance changed serialized v4 plan entries")
	}
}

func TestProjectedGeneratorSnapshotLoweringBuildsDifferentialGraph(t *testing.T) {
	plan, generatorID, consumerID := projectedGeneratorPlanForTest(t, "y", "n")
	originalGenerator := projectedGeneratorNodeForTest(t, plan, generatorID)
	originalRecipe := cloneActionRecipe(plan.Recipes[originalGenerator.Recipe])
	if err := lowerProjectedGeneratorsForFamilySnapshot(plan); err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 6; got != want {
		t.Fatalf("lowered node count = %d, want %d", got, want)
	}
	raw := projectedGeneratorNodeForTest(t, plan, generatorID)
	rawRecipe := plan.Recipes[raw.Recipe]
	if got, want := rawRecipe.ConfigProjectionPrefixes, []string{"CONFIG_USED"}; !slices.Equal(got, want) {
		t.Fatalf("projected recipe prefixes = %q, want %q", got, want)
	}
	if raw.Outputs[0].Path != projectedGeneratorTestTarget ||
		!strings.HasPrefix(raw.Outputs[0].ArtifactPath, compactKbuildProjectedFilechkRoot+"/") {
		t.Fatalf("projected raw output = %#v", raw.Outputs[0])
	}

	if got, want := len(plan.projectedGeneratorValidations), 1; got != want {
		t.Fatalf("validation roots = %d, want %d", got, want)
	}
	comparator := projectedGeneratorNodeForTest(t, plan, plan.projectedGeneratorValidations[0])
	if comparator.Kind != "metadata" || comparator.Tool != "actionfile" || len(comparator.Inputs) != 2 ||
		comparator.Inputs[0].Role != "projected" || comparator.Inputs[0].ProducerID != raw.ID ||
		comparator.Inputs[1].Role != "full" {
		t.Fatalf("projected/full comparator = %#v", comparator)
	}
	full := projectedGeneratorNodeForTest(t, plan, comparator.Inputs[1].ProducerID)
	fullRecipe := plan.Recipes[full.Recipe]
	if !reflect.DeepEqual(fullRecipe, originalRecipe) {
		t.Fatalf("full replay changed tool-visible recipe:\n got %#v\nwant %#v", fullRecipe, originalRecipe)
	}
	expectedFull := cloneActionRecipe(rawRecipe)
	expectedFull.ConfigProjectionPrefixes = nil
	if !reflect.DeepEqual(expectedFull, fullRecipe) ||
		!reflect.DeepEqual(raw.Sources, full.Sources) || !reflect.DeepEqual(raw.Inputs, full.Inputs) ||
		!slices.Equal(raw.Trees, full.Trees) || !slices.Equal(raw.AuxiliaryTools, full.AuxiliaryTools) {
		t.Fatalf("projected/full execution contracts diverged: projected=%#v full=%#v", raw, full)
	}
	if full.Outputs[0].Path != projectedGeneratorTestTarget ||
		!strings.HasPrefix(full.Outputs[0].ArtifactPath, compactKbuildProjectionValidateRoot+"/") {
		t.Fatalf("full replay output allocation = %#v", full.Outputs[0])
	}

	consumer := projectedGeneratorNodeForTest(t, plan, consumerID)
	if len(consumer.Inputs) != 1 || consumer.Inputs[0].ProducerID == raw.ID {
		t.Fatalf("consumer was not rewired through canonical validation: %#v", consumer.Inputs)
	}
	canonical := projectedGeneratorNodeForTest(t, plan, consumer.Inputs[0].ProducerID)
	canonicalRecipe := plan.Recipes[canonical.Recipe]
	if canonical.Tool != "actionfile" || canonical.Outputs[0].Path != projectedGeneratorTestTarget ||
		!slices.Contains(canonicalRecipe.Arguments, "-validate_closed_integer_macro_header_v1") ||
		len(canonical.Inputs) != 1 || canonical.Inputs[0].ProducerID != raw.ID {
		t.Fatalf("canonical projected header node=%#v recipe=%#v", canonical, canonicalRecipe)
	}
	wantInternals := map[string]bool{raw.ID: true, full.ID: true, comparator.ID: true}
	if !maps.Equal(plan.projectedGeneratorInternalNodes, wantInternals) {
		t.Fatalf("internal projected-generator nodes = %v, want %v", plan.projectedGeneratorInternalNodes, wantInternals)
	}

	rawDependencies, err := AnalyzeActionPlanNodeConfigDependencies(plan, raw)
	if err != nil {
		t.Fatal(err)
	}
	if rawDependencies.Opaque || !slices.Equal(rawDependencies.Symbols, []string{"CONFIG_USED"}) {
		t.Fatalf("projected dependencies = %#v, want CONFIG_USED subset", rawDependencies)
	}
	fullDependencies, err := AnalyzeActionPlanNodeConfigDependencies(plan, full)
	if err != nil {
		t.Fatal(err)
	}
	if !fullDependencies.Opaque {
		t.Fatalf("full replay dependencies = %#v, want full-config fallback", fullDependencies)
	}
	canonicalDependencies, err := AnalyzeActionPlanNodeConfigDependencies(plan, canonical)
	if err != nil {
		t.Fatal(err)
	}
	if canonicalDependencies.Opaque || len(canonicalDependencies.Symbols) != 0 {
		t.Fatalf("canonical validator dependencies = %#v, want config-free", canonicalDependencies)
	}
}

func TestProjectedGeneratorSnapshotLoweringUsesAndRewritesPersistentInputSets(t *testing.T) {
	plan, generatorID, consumerID := projectedGeneratorPlanForTest(t, "y", "n")
	generator := projectedGeneratorNodeForTest(t, plan, generatorID)
	configInput := generator.Inputs[0]
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		t.Fatal(err)
	}
	generator.InputSet, err = store.Insert(generator.InputSet, ActionPlanInputSetEntry{
		Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: ".config"},
		ProducerID: configInput.ProducerID,
		Slot:       configInput.Slot,
	})
	if err != nil {
		t.Fatal(err)
	}
	generator.Inputs = nil
	generatorRecipe := plan.Recipes[generator.Recipe]
	generatorRecipe.Arguments[len(generatorRecipe.Arguments)-1] = ".config"
	generatorRecipe.Inputs = nil
	delete(generatorRecipe.WorkingInputs, "input:config:00000000")
	plan.Recipes[generator.Recipe] = generatorRecipe

	consumer := projectedGeneratorNodeForTest(t, plan, consumerID)
	consumer.InputSet, err = store.Insert(consumer.InputSet, ActionPlanInputSetEntry{
		Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: projectedGeneratorTestTarget},
		ProducerID: generatorID,
		Slot:       0,
	})
	if err != nil {
		t.Fatal(err)
	}
	for index := range plan.Nodes {
		switch plan.Nodes[index].ID {
		case generatorID:
			plan.Nodes[index] = generator
		case consumerID:
			plan.Nodes[index] = consumer
		}
	}
	plan.invalidateLookupIndexes()

	if err := lowerProjectedGeneratorsForFamilySnapshot(plan); err != nil {
		t.Fatal(err)
	}
	raw := projectedGeneratorNodeForTest(t, plan, generatorID)
	rawDependencies, err := AnalyzeActionPlanNodeConfigDependencies(plan, raw)
	if err != nil {
		t.Fatal(err)
	}
	if rawDependencies.Opaque || !slices.Equal(rawDependencies.Symbols, []string{"CONFIG_USED"}) {
		t.Fatalf("persistent projected-generator dependencies = %#v, want CONFIG_USED subset", rawDependencies)
	}

	consumer = projectedGeneratorNodeForTest(t, plan, consumerID)
	if len(consumer.Inputs) != 1 || consumer.Inputs[0].ProducerID == generatorID {
		t.Fatalf("direct projected-generator consumer was not rewritten: %#v", consumer.Inputs)
	}
	persistent, found, err := store.Lookup(consumer.InputSet, ActionPlanInputSetTarget{
		Kind: ActionPlanInputSetWorkTarget, Path: projectedGeneratorTestTarget,
	})
	if err != nil || !found {
		t.Fatalf("lookup rewritten persistent consumer: found=%t err=%v", found, err)
	}
	if persistent.ProducerID != consumer.Inputs[0].ProducerID || persistent.Slot != consumer.Inputs[0].Slot {
		t.Fatalf("persistent consumer binding = %#v, want direct replacement %#v", persistent, consumer.Inputs[0])
	}
}

func TestProjectedGeneratorSnapshotLoweringOnlyProjectsCanonicalOverwriteWinner(t *testing.T) {
	plan, originalGeneratorID, originalConsumerID := projectedGeneratorPlanForTest(t, "y", "n")
	originalGenerator := projectedGeneratorNodeForTest(t, plan, originalGeneratorID)
	originalConsumer := projectedGeneratorNodeForTest(t, plan, originalConsumerID)
	generatorRecipe := cloneActionRecipe(plan.Recipes[originalGenerator.Recipe])
	consumerRecipe := cloneActionRecipe(plan.Recipes[originalConsumer.Recipe])

	// Rebuild the pair in source overwrite order: the first selected writer is
	// retained at an immutable private ArtifactPath, while the final writer owns
	// the canonical logical header.  Both were recognized as projection
	// candidates before overwrite ownership was assigned.
	plan.Nodes = slices.Clone(plan.Nodes[:1])
	plan.invalidateLookupIndexes()
	historicalNode := originalGenerator
	historicalNode.ID = ""
	historicalNode.Outputs = slices.Clone(originalGenerator.Outputs)
	historicalNode.Outputs[0].ArtifactPath = ".linux-bzl-versions/historical/" + projectedGeneratorTestTarget
	historicalRecipe := cloneActionRecipe(generatorRecipe)
	if err := bindVersionedActionPlanWorkingOutputs(historicalNode, &historicalRecipe); err != nil {
		t.Fatal(err)
	}
	historicalID, err := appendActionPlanNode(plan, historicalNode, historicalRecipe)
	if err != nil {
		t.Fatal(err)
	}
	historicalConsumer := originalConsumer
	historicalConsumer.ID = ""
	historicalConsumer.Inputs = []ActionPlanNodeEdge{{Role: "header", ProducerID: historicalID, Slot: 0}}
	historicalConsumer.Outputs = []ActionPlanOutput{{Tree: "objects", Path: "drivers/projected-generator-historical.o"}}
	historicalConsumerID, err := appendActionPlanNode(plan, historicalConsumer, consumerRecipe)
	if err != nil {
		t.Fatal(err)
	}

	winnerNode := originalGenerator
	winnerNode.ID = ""
	winnerID, err := appendActionPlanNode(plan, winnerNode, generatorRecipe)
	if err != nil {
		t.Fatal(err)
	}
	winnerConsumer := originalConsumer
	winnerConsumer.ID = ""
	winnerConsumer.Inputs = []ActionPlanNodeEdge{{Role: "header", ProducerID: winnerID, Slot: 0}}
	winnerConsumer.Outputs = []ActionPlanOutput{{Tree: "objects", Path: "drivers/projected-generator-winner.o"}}
	winnerConsumerID, err := appendActionPlanNode(plan, winnerConsumer, consumerRecipe)
	if err != nil {
		t.Fatal(err)
	}
	plan.projectedGeneratorCandidates = map[string]projectedGeneratorCandidate{
		historicalID: {TargetPath: projectedGeneratorTestTarget, TargetSlot: 0, ConfigProjectionPrefixes: []string{"CONFIG_USED"}},
		winnerID:     {TargetPath: projectedGeneratorTestTarget, TargetSlot: 0, ConfigProjectionPrefixes: []string{"CONFIG_USED"}},
	}

	if err := lowerProjectedGeneratorsForFamilySnapshot(plan); err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 8; got != want {
		t.Fatalf("lowered overwrite node count = %d, want %d", got, want)
	}
	if got, want := len(plan.projectedGeneratorValidations), 1; got != want {
		t.Fatalf("overwrite validation roots = %d, want %d", got, want)
	}

	historical := projectedGeneratorNodeForTest(t, plan, historicalID)
	if got, want := historical.Outputs[0].ArtifactPath, historicalNode.Outputs[0].ArtifactPath; got != want {
		t.Fatalf("historical artifact path = %q, want unchanged %q", got, want)
	}
	if prefixes := plan.Recipes[historical.Recipe].ConfigProjectionPrefixes; len(prefixes) != 0 {
		t.Fatalf("historical generator acquired projection prefixes %q", prefixes)
	}
	historicalConsumer = projectedGeneratorNodeForTest(t, plan, historicalConsumerID)
	if got := historicalConsumer.Inputs[0].ProducerID; got != historicalID {
		t.Fatalf("historical consumer producer = %q, want unchanged %q", got, historicalID)
	}
	historicalDependencies, err := AnalyzeActionPlanNodeConfigDependencies(plan, historical)
	if err != nil {
		t.Fatal(err)
	}
	if !historicalDependencies.Opaque {
		t.Fatalf("historical generator dependencies = %#v, want full config", historicalDependencies)
	}

	winner := projectedGeneratorNodeForTest(t, plan, winnerID)
	if got, want := plan.Recipes[winner.Recipe].ConfigProjectionPrefixes, []string{"CONFIG_USED"}; !slices.Equal(got, want) {
		t.Fatalf("canonical winner projection prefixes = %q, want %q", got, want)
	}
	if !strings.HasPrefix(winner.Outputs[0].ArtifactPath, compactKbuildProjectedFilechkRoot+"/") {
		t.Fatalf("canonical winner was not lowered to a private projected output: %#v", winner.Outputs[0])
	}
	winnerConsumer = projectedGeneratorNodeForTest(t, plan, winnerConsumerID)
	if got := winnerConsumer.Inputs[0].ProducerID; got == winnerID || got == "" {
		t.Fatalf("canonical winner consumer was not rewired through validation: %#v", winnerConsumer.Inputs)
	}
}

func TestProjectedGeneratorSnapshotLoweringRejectsMalformedHistoricalCandidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ActionPlan, string, *ActionPlanNode)
		want   string
	}{
		{
			name: "absent target slot",
			mutate: func(plan *ActionPlan, generatorID string, node *ActionPlanNode) {
				node.Outputs[0].ArtifactPath = ".linux-bzl-versions/historical/" + node.Outputs[0].Path
				candidate := plan.projectedGeneratorCandidates[generatorID]
				candidate.TargetSlot = len(node.Outputs)
				plan.projectedGeneratorCandidates[generatorID] = candidate
			},
			want: "target slot 1 is absent",
		},
		{
			name: "target path mismatch",
			mutate: func(plan *ActionPlan, generatorID string, node *ActionPlanNode) {
				node.Outputs[0].ArtifactPath = ".linux-bzl-versions/historical/" + node.Outputs[0].Path
				candidate := plan.projectedGeneratorCandidates[generatorID]
				candidate.TargetPath = "different"
				plan.projectedGeneratorCandidates[generatorID] = candidate
			},
			want: "disagrees with provenance",
		},
		{
			name: "observed output",
			mutate: func(_ *ActionPlan, _ string, node *ActionPlanNode) {
				node.Outputs[0].ArtifactPath = ".linux-bzl-versions/historical/" + node.Outputs[0].Path
				node.Outputs[0].ObservedPath = node.Outputs[0].Path
			},
			want: "is not an ordinary output",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, generatorID, _ := projectedGeneratorPlanForTest(t, "y", "n")
			for index := range plan.Nodes {
				if plan.Nodes[index].ID == generatorID {
					test.mutate(plan, generatorID, &plan.Nodes[index])
				}
			}
			err := lowerProjectedGeneratorsForFamilySnapshot(plan)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("lowering error = %v, want %q", err, test.want)
			}
		})
	}
}

func projectedGeneratorSnapshotForTest(t *testing.T, used, other string) ActionPlanSnapshot {
	t.Helper()
	plan, _, _ := projectedGeneratorPlanForTest(t, used, other)
	if err := lowerProjectedGeneratorsForFamilySnapshot(plan); err != nil {
		t.Fatal(err)
	}
	analysis, err := BuildActionPlanConfigDependencyAnalysis(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	dependencies, err := analysis.ByNodeID(plan)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, familyTestConfig(used, other))
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.validate(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

type projectedGeneratorFamilyNodesForTest struct {
	raw        []ActionPlanNode
	full       []ActionPlanNode
	canonical  []ActionPlanNode
	comparator []ActionPlanNode
	downstream []ActionPlanNode
}

func classifyProjectedGeneratorFamilyNodesForTest(family *ActionPlanFamily) projectedGeneratorFamilyNodesForTest {
	result := projectedGeneratorFamilyNodesForTest{}
	for _, node := range family.Nodes {
		recipe := family.Recipes[node.Recipe]
		switch {
		case len(recipe.ConfigProjectionPrefixes) != 0:
			result.raw = append(result.raw, node)
		case recipe.Tool == "awk":
			result.full = append(result.full, node)
		case slices.Contains(recipe.Arguments, "-validate_closed_integer_macro_header_v1"):
			result.canonical = append(result.canonical, node)
		case len(recipe.Arguments) != 0 && recipe.Arguments[0] == "-compare_input":
			result.comparator = append(result.comparator, node)
		default:
			for _, output := range node.Outputs {
				if output.Path == "drivers/projected-generator.o" {
					result.downstream = append(result.downstream, node)
				}
			}
		}
	}
	return result
}

func TestProjectedGeneratorFamilyReusesProjectionAcrossIrrelevantConfig(t *testing.T) {
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "base", Snapshot: projectedGeneratorSnapshotForTest(t, "y", "n")},
		{Name: "irrelevant", Snapshot: projectedGeneratorSnapshotForTest(t, "y", "y")},
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes := classifyProjectedGeneratorFamilyNodesForTest(family)
	wantShared := []string{"base", "irrelevant"}
	for name, candidates := range map[string][]ActionPlanNode{
		"projected raw": nodes.raw, "canonical header": nodes.canonical, "downstream": nodes.downstream,
	} {
		if len(candidates) != 1 || !slices.Equal(family.Memberships[candidates[0].ID], wantShared) {
			t.Fatalf("%s nodes/memberships = %#v/%#v, want one shared node", name, candidates, family.Memberships)
		}
	}
	for name, candidates := range map[string][]ActionPlanNode{
		"full replay": nodes.full, "comparator": nodes.comparator,
	} {
		if len(candidates) != 2 {
			t.Fatalf("%s nodes = %d, want two variant-local nodes: %#v", name, len(candidates), candidates)
		}
		seen := []string{}
		for _, node := range candidates {
			members := family.Memberships[node.ID]
			if len(members) != 1 {
				t.Fatalf("%s node %s memberships = %q, want one", name, node.ID, members)
			}
			seen = append(seen, members[0])
		}
		sort.Strings(seen)
		if !slices.Equal(seen, wantShared) {
			t.Fatalf("%s variants = %q, want %q", name, seen, wantShared)
		}
	}

	producers := make(map[string]ActionPlanNode, len(family.Nodes))
	sources := make(map[string]ActionPlanSource, len(family.Sources))
	for _, node := range family.Nodes {
		producers[node.ID] = node
	}
	for _, source := range family.Sources {
		sources[source.ID] = source
	}
	projected := nodes.raw[0]
	if len(projected.Inputs) != 1 {
		t.Fatalf("projected generator inputs = %#v, want one capsule-backed .config", projected.Inputs)
	}
	projectionCopy := producers[projected.Inputs[0].ProducerID]
	if projectionCopy.Kind != "copy" || len(projectionCopy.Sources) != 1 ||
		!slices.Equal(family.Memberships[projectionCopy.ID], wantShared) {
		t.Fatalf("projected config clone = %#v memberships=%q", projectionCopy, family.Memberships[projectionCopy.ID])
	}
	projectedSource := sources[projectionCopy.Sources[0].SourceID]
	digest, projectionPath, ok := strings.Cut(projectedSource.Path, "/")
	if projectedSource.Namespace != "capsule" || !ok || projectionPath != ".config" {
		t.Fatalf("projected config source = %#v", projectedSource)
	}
	projectedConfig := family.Capsules[digest][".config"]
	if !strings.Contains(projectedConfig, "CONFIG_USED=y") || strings.Contains(projectedConfig, "CONFIG_OTHER") {
		t.Fatalf("projected config capsule = %q, want only CONFIG_USED", projectedConfig)
	}

	for _, full := range nodes.full {
		if len(full.Inputs) != 1 {
			t.Fatalf("full replay inputs = %#v, want full .config copy", full.Inputs)
		}
		copyNode := producers[full.Inputs[0].ProducerID]
		if len(copyNode.Sources) != 1 {
			t.Fatalf("full replay config copy = %#v", copyNode)
		}
		source := sources[copyNode.Sources[0].SourceID]
		fullDigest, projection, ok := strings.Cut(source.Path, "/")
		if source.Namespace != "capsule" || !ok || projection != ".config" {
			t.Fatalf("full replay config source = %#v", source)
		}
		config := family.Capsules[fullDigest][".config"]
		variant := family.Memberships[full.ID][0]
		wantOther := "n"
		if variant == "irrelevant" {
			wantOther = "y"
		}
		if !strings.Contains(config, "CONFIG_USED=y") || !strings.Contains(config, "CONFIG_OTHER="+wantOther) {
			t.Fatalf("%s full replay config = %q", variant, config)
		}
	}

	if got, want := len(family.Validations), 2; got != want {
		t.Fatalf("family validations = %d, want %d", got, want)
	}
	comparatorIDs := map[string]bool{}
	for _, node := range nodes.comparator {
		comparatorIDs[node.ID] = true
	}
	seenValidationVariants := []string{}
	for _, validation := range family.Validations {
		if !comparatorIDs[validation.NodeID] || validation.Slot != 0 {
			t.Fatalf("family validation = %#v, want comparator output", validation)
		}
		seenValidationVariants = append(seenValidationVariants, validation.Variant)
	}
	sort.Strings(seenValidationVariants)
	if !slices.Equal(seenValidationVariants, wantShared) {
		t.Fatalf("validation variants = %q, want %q", seenValidationVariants, wantShared)
	}

	internal := maps.Clone(comparatorIDs)
	for _, node := range append(slices.Clone(nodes.raw), nodes.full...) {
		internal[node.ID] = true
	}
	for _, view := range family.Views {
		if internal[view.NodeID] {
			t.Fatalf("internal projected-generator output escaped into public view: %#v", view)
		}
	}
	// Prep-tree outputs are deliberately not public family views. The canonical
	// header is nevertheless retained by the shared downstream edge; only the
	// product-tree consumer itself must have one view per variant.
	for _, public := range []ActionPlanNode{nodes.downstream[0]} {
		variants := []string{}
		for _, view := range family.Views {
			if view.NodeID == public.ID {
				variants = append(variants, view.Variant)
			}
		}
		sort.Strings(variants)
		if !slices.Equal(variants, wantShared) {
			t.Fatalf("public node %s view variants = %q, want %q", public.ID, variants, wantShared)
		}
	}
}

func TestProjectedGeneratorFamilyRetainsAllNSelectorUniverse(t *testing.T) {
	// CONFIG_USED is deliberately absent from the synthetic written fragment
	// when its value is n. The resolver-derived symbol universe must still put it
	// in the projected dependency set, producing one stable n-valued capsule in
	// both variants while CONFIG_OTHER remains irrelevant.
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "first", Snapshot: projectedGeneratorSnapshotForTest(t, "n", "n")},
		{Name: "second", Snapshot: projectedGeneratorSnapshotForTest(t, "n", "y")},
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes := classifyProjectedGeneratorFamilyNodesForTest(family)
	want := []string{"first", "second"}
	for name, candidates := range map[string][]ActionPlanNode{
		"projected raw": nodes.raw, "canonical header": nodes.canonical, "downstream": nodes.downstream,
	} {
		if len(candidates) != 1 || !slices.Equal(family.Memberships[candidates[0].ID], want) {
			t.Fatalf("%s nodes/memberships = %#v/%#v, want one all-n shared node", name, candidates, family.Memberships)
		}
	}
}

func TestProjectedGeneratorFamilySplitsProjectionOnRelevantConfig(t *testing.T) {
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "enabled", Snapshot: projectedGeneratorSnapshotForTest(t, "y", "n")},
		{Name: "disabled", Snapshot: projectedGeneratorSnapshotForTest(t, "n", "n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes := classifyProjectedGeneratorFamilyNodesForTest(family)
	for name, candidates := range map[string][]ActionPlanNode{
		"projected raw": nodes.raw, "canonical header": nodes.canonical, "downstream": nodes.downstream,
	} {
		if len(candidates) != 2 {
			t.Fatalf("%s nodes = %d, want relevant-config split: %#v", name, len(candidates), candidates)
		}
		for _, node := range candidates {
			if len(family.Memberships[node.ID]) != 1 {
				t.Fatalf("%s node %s memberships = %q, want one", name, node.ID, family.Memberships[node.ID])
			}
		}
	}
}

func TestProjectedGeneratorPrivatePathsAreCanonical(t *testing.T) {
	identity := compactKbuildProjectedGeneratorIdentity([]ActionPlanOutput{{
		Tree: "prep", Path: projectedGeneratorTestTarget,
	}}, 0)
	for _, pathname := range []string{
		path.Join(compactKbuildProjectedFilechkRoot, identity, "projected", planOrdinal(0)),
		path.Join(compactKbuildProjectionValidateRoot, identity, "full", planOrdinal(0)),
		path.Join(compactKbuildProjectionValidateRoot, identity, "validated"),
	} {
		if err := validatePlanRelativePath("projected-generator private path", pathname); err != nil {
			t.Fatalf("private path %q: %v", pathname, err)
		}
	}
}
