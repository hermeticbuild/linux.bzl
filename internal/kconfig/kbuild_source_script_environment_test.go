package kconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func compactKbuildUsageNames(usage compactKbuildSourceScriptEnvironmentUsage) []string {
	names := make([]string, 0, len(usage.Names))
	for name := range usage.Names {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func TestCompactKbuildSourceScriptEnvironmentUsageTracksActiveReads(t *testing.T) {
	scan, err := scanCompactKbuildSourceScript(`
printf '%s\n' "$CC" "${HOSTLD:-$LD}" "$((COUNT + OFFSET))"
# $COMMENT
printf '%s\n' '$SINGLE_QUOTED' \$ESCAPED
cat <<'QUOTED'
$QUOTED_HEREDOC
QUOTED
cat <<ACTIVE
'$ACTIVE_HEREDOC'
ACTIVE
printenv NAMED_QUERY
`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ACTIVE_HEREDOC", "CC", "COUNT", "HOSTLD", "LD", "NAMED_QUERY", "OFFSET"}
	if got := compactKbuildUsageNames(scan.usage); !reflect.DeepEqual(got, want) {
		t.Fatalf("environment names = %q, want %q", got, want)
	}
	if scan.usage.ObservesAll {
		t.Fatal("bounded direct environment reads unexpectedly observe all")
	}
}

func TestCompactKbuildSourceScriptEnvironmentUsageBoundsGeneratedObjectTreeProgram(t *testing.T) {
	scan, err := scanCompactKbuildSourceScript(`${objtree}/scripts/sorttable -s .tmp_vmlinux.nm-sort "$VMLINUX"` + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if scan.usage.ObservesAll {
		t.Fatal("statically named generated object-tree program unexpectedly observes all environment capabilities")
	}
	for _, name := range []string{"objtree", "VMLINUX"} {
		if !scan.usage.Names[name] {
			t.Errorf("generated object-tree program omitted environment path %s: %q", name, compactKbuildUsageNames(scan.usage))
		}
	}
}

func TestCompactKbuildSourceScriptEnvironmentUsageScansLegacyCommandSubstitution(t *testing.T) {
	scan, err := scanCompactKbuildSourceScript("location=input; captured=\"`printenv HOSTCC`\"; detail=\"`LC_ALL=C ls -l \"${location}\"`\"; printf '%s\\n' \"$captured\" \"$detail\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if !scan.usage.Names["HOSTCC"] {
		t.Fatalf("legacy command substitution omitted direct HOSTCC query: %q", compactKbuildUsageNames(scan.usage))
	}
	if scan.usage.ObservesAll {
		t.Fatal("literal named query in legacy command substitution unexpectedly observes all")
	}
}

func TestCompactKbuildSourceScriptEnvironmentUsageKeepsSingleQuoteLiteralInsideDoubleQuotes(t *testing.T) {
	scan, err := scanCompactKbuildSourceScript("printf \"the compiler won't change: ${CC}; can't open '$1'\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if !scan.usage.Names["CC"] || scan.usage.ObservesAll {
		t.Fatalf("double-quoted literal apostrophes changed environment use: usage=%q all=%t", compactKbuildUsageNames(scan.usage), scan.usage.ObservesAll)
	}
}

func TestCompactKbuildSourceScriptEnvironmentUsageKeepsLiteralDollarBeforeQuote(t *testing.T) {
	scan, err := scanCompactKbuildSourceScript(`if grep -q "^CONFIG_LOCALVERSION_AUTO=y$" include/config/auto.conf; then echo ok; fi` + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.usage.Names) != 0 || scan.usage.ObservesAll {
		t.Fatalf("literal regex anchor changed environment use: usage=%q all=%t", compactKbuildUsageNames(scan.usage), scan.usage.ObservesAll)
	}
}

func TestCompactKbuildSourceScriptEnvironmentUsageKeepsNestedExpansionQuotesLocal(t *testing.T) {
	scan, err := scanCompactKbuildSourceScript(`VALUE="$(echo "$input" | sed -n 's@.*/\* *\("[^"]*"\).*\*/@\1@p')"` + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if !scan.usage.Names["input"] || scan.usage.ObservesAll {
		t.Fatalf("nested command-substitution quotes changed environment use: usage=%q all=%t", compactKbuildUsageNames(scan.usage), scan.usage.ObservesAll)
	}
}

func TestCompactKbuildSourceScriptEnvironmentUsageScansMultilineCommandSubstitution(t *testing.T) {
	scan, err := scanCompactKbuildSourceScript(`guard=_UAPI_ASM_$(basename "$outfile" |
	sed -e 'y/abcdefghijklmnopqrstuvwxyz/ABCDEFGHIJKLMNOPQRSTUVWXYZ/' \
	-e 's/[^A-Z0-9_]/_/g')` + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if !scan.usage.Names["outfile"] || scan.usage.ObservesAll {
		t.Fatalf("multiline command substitution changed environment use: usage=%q all=%t", compactKbuildUsageNames(scan.usage), scan.usage.ObservesAll)
	}
}

func TestCompactKbuildSourceScriptEnvironmentUsageJoinsContinuedNames(t *testing.T) {
	scan, err := scanCompactKbuildSourceScript("printf '%s\\n' \"$HOST\\\nCC\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if !scan.usage.Names["HOSTCC"] || scan.usage.Names["HOST"] {
		t.Fatalf("continued environment name = %q, want only HOSTCC", compactKbuildUsageNames(scan.usage))
	}
}

func TestCompactKbuildSourceScriptEnvironmentUsageFailsClosedForDynamicObservation(t *testing.T) {
	for name, script := range map[string]string{
		"indirect parameter":  `printf '%s\n' "${!selector}"`,
		"unresolved eval":     `eval "$generated"`,
		"bare env":            `env`,
		"bare printenv":       `printenv`,
		"bare set":            `set`,
		"export listing":      `export -p`,
		"dynamic shell":       `sh -c "$program"`,
		"combined shell":      `sh -ec "$program"`,
		"split shell options": `busybox sh -e -c "$program"`,
		"env nested shell":    `env -u CC sh -ec "$program"`,
		"dynamic source":      `. "$fragment"`,
		"env unset listing":   `env -u CC`,
		"env attached unset":  `env -uCC`,
		"env long unset":      `env --unset=CC`,
		"awk environ spaced":  `awk 'BEGIN { print ENVIRON ["CC"] }'`,
		"awk environ for-in":  `awk 'BEGIN { for (name in ENVIRON) print name }'`,
	} {
		t.Run(name, func(t *testing.T) {
			scan, err := scanCompactKbuildSourceScript(script + "\n")
			if err != nil {
				t.Fatal(err)
			}
			if !scan.usage.ObservesAll {
				t.Fatalf("%q did not conservatively observe all environment capabilities", script)
			}
		})
	}
}

func TestCompactKbuildSourceScriptEnvironmentUsageEnvOptionOperandsWithProgramStayBounded(t *testing.T) {
	for _, script := range []string{
		`env -u CC printf bounded`,
		`env -uCC printf bounded`,
		`env --unset=CC printf bounded`,
		`env --unset CC printf bounded`,
		`$* -Wno-error -Wno-unused-macros -E -x c -`,
	} {
		scan, err := scanCompactKbuildSourceScript(script + "\n")
		if err != nil {
			t.Fatal(err)
		}
		if scan.usage.ObservesAll {
			t.Fatalf("env child command %q unexpectedly observes all", script)
		}
	}
}

func TestCompactKbuildSourceScriptEnvironmentUsageIdentifiesForwardedArgumentCommand(t *testing.T) {
	for _, script := range []string{
		`syscall_list() { grep "$1"; }; dirname "$0" >/dev/null; $* -Wno-error -E -x c -`,
		`"$@" -Wno-error -E -x c -`,
	} {
		scan, err := scanCompactKbuildSourceScript(script + "\n")
		if err != nil {
			t.Fatal(err)
		}
		wantPositional := 0
		if strings.Contains(script, "syscall_list") {
			wantPositional = 1
		}
		if scan.usage.argumentVectorUses != 1 || scan.usage.argumentVectorProgramUses != 1 || scan.usage.positionalArgumentUses != wantPositional {
			t.Fatalf("forwarded command %q argument usage = %#v", script, scan.usage)
		}
	}
	scan, err := scanCompactKbuildSourceScript("$* -E; printf '%s\\n' \"$2\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if scan.usage.argumentVectorUses != 1 || scan.usage.argumentVectorProgramUses != 1 || scan.usage.positionalArgumentUses != 1 {
		t.Fatalf("mixed positional argument usage = %#v", scan.usage)
	}
}

func TestCompactKbuildSourceScriptEnvironmentUsageInspectsBoundedWrappers(t *testing.T) {
	for name, script := range map[string]string{
		"env":     `env printenv HOSTCC`,
		"busybox": `busybox printenv HOSTCC`,
	} {
		t.Run(name+" named query", func(t *testing.T) {
			scan, err := scanCompactKbuildSourceScript(script + "\n")
			if err != nil {
				t.Fatal(err)
			}
			if !scan.usage.Names["HOSTCC"] {
				t.Fatalf("wrapped named query omitted HOSTCC: %q", compactKbuildUsageNames(scan.usage))
			}
			if scan.usage.ObservesAll {
				t.Fatalf("wrapped named query %q unexpectedly observes all", script)
			}
		})
	}
	for name, script := range map[string]string{
		"env":     `env sh scripts/child.sh`,
		"busybox": `busybox sh scripts/child.sh`,
	} {
		t.Run(name+" source child", func(t *testing.T) {
			scan, err := scanCompactKbuildSourceScript(script + "\n")
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"scripts/child.sh"}; !reflect.DeepEqual(scan.sources, want) {
				t.Fatalf("wrapped source children = %q, want %q", scan.sources, want)
			}
			if scan.usage.ObservesAll {
				t.Fatalf("wrapped source child %q unexpectedly observes all", script)
			}
		})
	}
}

func TestCompactKbuildSourceScriptEnvironmentUsageBoundsWrapperRecursion(t *testing.T) {
	scan, err := scanCompactKbuildSourceScript(strings.Repeat("env ", maxCompactKbuildSourceScriptWrapperDepth+2) + "printenv HOSTCC\n")
	if err != nil {
		t.Fatal(err)
	}
	if !scan.usage.ObservesAll {
		t.Fatal("over-depth wrapper chain did not fail closed")
	}
}

func TestCompactKbuildSourceScriptEnvironmentUsageResolvesBoundedEvalAndArithmetic(t *testing.T) {
	scan, err := scanCompactKbuildSourceScript(`
cmd='diff "$left" "$right"'
eval "$cmd"
sync_cmd="diff $* $file1 $file2 > /dev/null"
eval "$sync_cmd"
arithmetic_name=CC
value=$((arithmetic_name + OFFSET))
`)
	if err != nil {
		t.Fatal(err)
	}
	if scan.usage.ObservesAll {
		t.Fatal("literal local eval/arithmetic dataflow unexpectedly observes all")
	}
	// Local names remain conservative false positives. Projection only uses a
	// name when the effective exported value carries an action-role token, and
	// retaining them avoids erasing an earlier exported read after shadowing.
	for _, name := range []string{"CC", "OFFSET", "arithmetic_name", "cmd", "file1", "file2", "left", "right", "sync_cmd"} {
		if !scan.usage.Names[name] {
			t.Errorf("bounded local expansion omitted %s: %q", name, compactKbuildUsageNames(scan.usage))
		}
	}
}

func TestCompactKbuildSourceScriptEnvironmentUsageRejectsDynamicEvalProgram(t *testing.T) {
	for _, script := range []string{
		`eval "$generated"`,
		`cmd="$generated --flag"; eval "$cmd"`,
	} {
		scan, err := scanCompactKbuildSourceScript(script + "\n")
		if err != nil {
			t.Fatal(err)
		}
		if !scan.usage.ObservesAll {
			t.Fatalf("dynamic eval program %q did not observe all", script)
		}
	}
}

func TestCompactKbuildSourceScriptEnvironmentUsageDoesNotEraseReadBeforeLocalShadow(t *testing.T) {
	scan, err := scanCompactKbuildSourceScript("printf '%s\\n' \"$CC\"; CC=literal; eval \"$CC\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if !scan.usage.Names["CC"] {
		t.Fatalf("read-before-shadow CC was erased: %q", compactKbuildUsageNames(scan.usage))
	}
	if scan.usage.ObservesAll {
		t.Fatal("bounded eval of a literal local shadow unexpectedly observes all")
	}
}

func TestCompactKbuildSourceScriptEnvironmentUsageKeepsDynamicArithmeticLexical(t *testing.T) {
	scan, err := scanCompactKbuildSourceScript("arithmetic_name=\"$runtime_name\"\nvalue=$((arithmetic_name))\n")
	if err != nil {
		t.Fatal(err)
	}
	if scan.usage.ObservesAll {
		t.Fatal("runtime arithmetic data unexpectedly became wholesale environment observation")
	}
	for _, name := range []string{"arithmetic_name", "runtime_name"} {
		if !scan.usage.Names[name] {
			t.Errorf("lexical arithmetic dataflow omitted %s: %q", name, compactKbuildUsageNames(scan.usage))
		}
	}
	if scan.usage.Names["HOSTCC"] {
		t.Fatalf("runtime arithmetic invented an unobserved HOSTCC capability: %q", compactKbuildUsageNames(scan.usage))
	}
}

func TestCompactKbuildSourceScriptEnvironmentUsageRecursesIntoImmutableChildren(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{
		"Makefile":             "all:\n\t@true\n",
		"scripts/parent.sh":    `"$srctree/scripts/child.sh"` + "\n" + `. scripts/fragment.sh` + "\n" + `"${CONFIG_SHELL}" "$srctree/scripts/file-size.sh"` + "\n",
		"scripts/child.sh":     `printf '%s\n' "$CC"` + "\n",
		"scripts/file-size.sh": `printf '%s\n' "$OBJCOPY"` + "\n",
		"scripts/fragment.sh":  `printf '%s\n' "$HOSTCC"` + "\n",
	} {
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	kb, err := ParseKbuildFileTree(filepath.Join(root, "Makefile"), KbuildOptions{
		RootDir: root, ConfigVariablesComplete: true, MakeVariablesComplete: true, CaptureTargetEvaluator: true,
		SourceRoots: map[string]string{"__LINUX_BZL_SOURCE_TREE__": root},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("root", filepath.Join(root, "Makefile"), root, kb)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := compactKbuildSourceScriptUsage(profile, "scripts/parent.sh")
	if err != nil {
		t.Fatal(err)
	}
	if usage.ObservesAll {
		t.Fatal("exact immutable children unexpectedly observe all")
	}
	want := []string{"CC", "CONFIG_SHELL", "HOSTCC", "OBJCOPY", "srctree"}
	if got := compactKbuildUsageNames(usage); !reflect.DeepEqual(got, want) {
		t.Fatalf("recursive environment names = %q, want %q", got, want)
	}
}
