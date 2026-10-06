package kconfig

import (
	"fmt"
	"path"
	"strings"
)

// The generator registry.
//
// kbuild_generated_source.go derives which rule produces an object's source.
// Execution cannot be derived: the only interpreter available to an action is
// Perl, everything else is a Go port under internal/cmd/. So a rule can be
// described precisely and still have no way to run, and a miss fails naming
// the Makefile line.
//
// Keyed on canonicalised command text, never on the command's name: 6.12
// spells the same generator "cmd_perlasm" on x86 and "cmd_perl" on arm.

// GeneratedSourceKind names an executor this project can actually run.
type GeneratedSourceKind string

const (
	// GeneratedSourcePerlStdout runs a perlasm script and captures stdout.
	// 6.12 arch/x86/crypto and arch/arm/crypto, 6.18 lib/crypto/{arm,x86}.
	GeneratedSourcePerlStdout GeneratedSourceKind = "perl_stdout"
	// GeneratedSourcePerlArgOut passes the output path as an argument after a
	// literal flavour word. 6.12 arch/arm64/crypto, 6.18 lib/crypto/arm64.
	GeneratedSourcePerlArgOut GeneratedSourceKind = "perl_arg_out"
	// GeneratedSourceRaid6Unroll expands lib/raid6's ".uc" templates.
	GeneratedSourceRaid6Unroll GeneratedSourceKind = "raid6_unroll"
	// GeneratedSourceMkcapflags builds arch/x86/kernel/cpu/capflags.c.
	GeneratedSourceMkcapflags GeneratedSourceKind = "mkcapflags"
	// GeneratedSourceConmakehash builds drivers/tty/vt/consolemap_deftbl.c.
	GeneratedSourceConmakehash GeneratedSourceKind = "conmakehash"
	// GeneratedSourceRaid6Mktables builds lib/raid6/tables.c.
	GeneratedSourceRaid6Mktables GeneratedSourceKind = "raid6_mktables"
)

// generatedSourceKindSpec describes one executable generator. Every field is
// declared rather than inferred: guessing which prerequisite the executor
// opens produces either a sandbox failure or a stale-cache bug.
type generatedSourceKindSpec struct {
	kind GeneratedSourceKind
	// canonical is the canonicalised command text this kind recognises.
	canonical string
	// primary picks the nominal source recorded for the object. It is not
	// always $<: mkcapflags binds $< to cpufeatures.h, but the source that
	// stands for the object is the script. Nil means $<.
	primary func(KbuildGeneratedSource) string
	// actionInputs are prerequisites the executor opens at run time and that
	// therefore must be declared as action inputs. Nil means just the primary.
	actionInputs func(KbuildGeneratedSource) []string
	// noActionInputs declares that the executor opens nothing, rather than
	// leaving the "just the primary" default to apply.
	noActionInputs bool
	// digestOnlyInputs are files whose contents must invalidate the object but
	// which the action never reads, because a Go port stands in for them.
	digestOnlyInputs func(KbuildGeneratedSource) []string
	// args are the executor arguments beyond the input and output paths.
	args func(KbuildGeneratedSource) []string
	// closureInputs are files scanned for includes on the generated file's
	// behalf. They are needed when the nominal source's own include lines do
	// not describe the generated output.
	closureInputs []string
	// skipPrimaryScan suppresses scanning the nominal source for includes.
	// Needed only for a host C program, whose includes are the host's; every
	// other input has no include lines or exactly the generated file's.
	skipPrimaryScan bool
}

// generatedSourceKinds is the closed set of executable generators.
//
// Deliberately absent: "PERL $< <flavour> > $@", used by powerpc and mips
// perlasm. Those scripts spawn a "*-xlate.pl" child, which the sandbox has no
// way to declare. Only x86, arm64 and arm are supported architectures, so the
// shape is unreachable; it stays UNKNOWN rather than half-implemented.
var generatedSourceKinds = []generatedSourceKindSpec{
	{
		kind:      GeneratedSourcePerlStdout,
		canonical: "PERL $< > $@",
	},
	{
		kind:      GeneratedSourcePerlArgOut,
		canonical: "PERL $< void $@",
		args:      func(KbuildGeneratedSource) []string { return []string{"void"} },
	},
	{
		kind:      GeneratedSourceRaid6Unroll,
		canonical: "AWK -v N=$* -f %[unroll] < $< > $@",
		// unroll.awk is reimplemented by internal/cmd/unroll, so the action
		// never opens it, but a change to it changes the generated source.
		digestOnlyInputs: func(gs KbuildGeneratedSource) []string {
			return generatedSourcePrerequisitesExcept(gs, gs.Primary)
		},
		args: func(gs KbuildGeneratedSource) []string { return []string{"-n", gs.Stem} },
	},
	{
		kind:      GeneratedSourceMkcapflags,
		canonical: "SHELL %[script] $@ $^",
		// $< here is cpufeatures.h, but the source that stands for capflags.o
		// is the script. Preserving that keeps the object's identity stable
		// across the move onto this resolver.
		primary: func(gs KbuildGeneratedSource) string {
			return generatedSourceScriptPrerequisite(gs, ".sh")
		},
		// The Go port reads all three: both feature headers and the script.
		actionInputs: func(gs KbuildGeneratedSource) []string {
			return append([]string{}, gs.Prerequisites...)
		},
	},
	{
		kind:      GeneratedSourceConmakehash,
		canonical: "HOSTPROG(conmakehash) $< > $@",
		// conmakehash is a hostprog reimplemented in Go. Nothing currently
		// invalidates consolemap_deftbl.o when the upstream hostprog changes;
		// digesting its source closes that hole.
		digestOnlyInputs: func(gs KbuildGeneratedSource) []string {
			return generatedSourceHostprogSources(gs)
		},
	},
	{
		kind:      GeneratedSourceRaid6Mktables,
		canonical: "HOSTPROG(mktables) > $@",
		// The generator reads nothing: it writes tables.c from compiled-in
		// arithmetic. So the hostprog's own source is the only file that can
		// change the output, and the only one that can stand for the object.
		primary: func(gs KbuildGeneratedSource) string {
			sources := generatedSourceHostprogSources(gs)
			if len(sources) == 0 {
				return ""
			}
			return sources[0]
		},
		noActionInputs: true,
		digestOnlyInputs: func(gs KbuildGeneratedSource) []string {
			return generatedSourceHostprogSources(gs)
		},
		// mktables.c includes <stdio.h> and friends, which do not resolve in
		// the kernel tree; the tables.c it prints includes only these two.
		skipPrimaryScan: true,
		closureInputs:   []string{"include/linux/export.h", "include/linux/raid/pq.h"},
	},
}

// GeneratedSourceExecutor is a resolved generator: a kind plus the concrete
// inputs and arguments the action needs.
type GeneratedSourceExecutor struct {
	Kind GeneratedSourceKind
	// Target is the source-relative path of the generated file, for example
	// "lib/raid6/int1.c". It is the path whose extension decides how the
	// object is compiled.
	Target string
	// Primary is the nominal source recorded for the object. It always names a
	// real checked-in file, never the generated target, because several
	// invariants require the object's source to exist in the tree and be part
	// of the exact-input group.
	Primary string
	// ActionInputs are files the action opens, in rule order.
	ActionInputs []string
	// DigestOnlyInputs are files that must invalidate the object without being
	// action inputs.
	DigestOnlyInputs []string
	// ClosureInputs are scanned for includes on the generated file's behalf.
	ClosureInputs []string
	// SkipPrimaryScan suppresses scanning Primary for includes.
	SkipPrimaryScan bool
	// Args are executor arguments beyond input and output.
	Args []string
}

// GeneratedSourceExecutorFor classifies a resolved rule into an executor. It
// fails rather than guessing; the error names the object, the rule position,
// the canonical command text and the files that need editing.
func GeneratedSourceExecutorFor(object string, gs KbuildGeneratedSource) (GeneratedSourceExecutor, error) {
	canonical, err := canonicalGeneratedSourceCommand(gs)
	if err != nil {
		return GeneratedSourceExecutor{}, generatedSourceKindError(object, gs, canonical, err)
	}
	for _, spec := range generatedSourceKinds {
		if spec.canonical != canonical {
			continue
		}
		executor := GeneratedSourceExecutor{
			Kind:          spec.kind,
			Target:        gs.Target,
			Primary:       gs.Primary,
			ClosureInputs: spec.closureInputs,
		}
		if spec.primary != nil {
			executor.Primary = spec.primary(gs)
		}
		switch {
		case spec.actionInputs != nil:
			executor.ActionInputs = spec.actionInputs(gs)
		case spec.noActionInputs:
		case executor.Primary != "":
			executor.ActionInputs = []string{executor.Primary}
		}
		if spec.digestOnlyInputs != nil {
			executor.DigestOnlyInputs = spec.digestOnlyInputs(gs)
		}
		if spec.args != nil {
			executor.Args = spec.args(gs)
		}
		executor.SkipPrimaryScan = spec.skipPrimaryScan
		if executor.Primary == "" {
			return GeneratedSourceExecutor{}, generatedSourceKindError(
				object, gs, canonical,
				fmt.Errorf("the rule has no prerequisite that can stand for the object's source"))
		}
		return executor, nil
	}
	return GeneratedSourceExecutor{}, generatedSourceKindError(object, gs, canonical, nil)
}

func generatedSourceKindError(object string, gs KbuildGeneratedSource, canonical string, cause error) error {
	// Position.String() degrades to a bare line number when the filename is
	// unset; the directory is the only thing that makes such a report
	// actionable, so it is substituted in instead.
	position := gs.Position
	if position.Filename == "" {
		position.Filename = gs.Directory + "/Makefile"
	}
	detail := ""
	if cause != nil {
		detail = ": " + cause.Error()
	}
	return fmt.Errorf(
		"no generator is implemented for leaf object %q, whose source %q is produced by "+
			"cmd_%s at %s%s\n"+
			"  command:   %s\n"+
			"  canonical: %s\n"+
			"Add the kind to internal/kconfig/generated_source_kinds.go and a matching "+
			"branch to _linux_generated_source in internal/linux_objects.bzl.",
		object, gs.Target, gs.CommandName, position, detail,
		gs.CommandTemplate, canonical)
}

// canonicalGeneratedSourceCommand reduces a command body to a comparable form.
//
// Whitespace collapses, the parenthesised automatic-variable spellings fold
// into the short ones, and the leading program word becomes a token so that a
// literal "$(PERL)" and a resolved interpreter path classify identically.
// In-tree script paths become placeholders, because the same generator lives
// at a different path in every kernel version.
//
// It fails closed: any "$(...)" that is not an automatic variable and survives
// this reduction means the command was not fully understood.
func canonicalGeneratedSourceCommand(gs KbuildGeneratedSource) (string, error) {
	fields := strings.Fields(foldMakeAutomaticVariables(gs.CommandTemplate))
	if len(fields) == 0 {
		return "", fmt.Errorf("the command is empty")
	}
	program, err := canonicalGeneratedSourceProgram(fields[0])
	if err != nil {
		return "", err
	}
	out := make([]string, 0, len(fields))
	out = append(out, program)
	for _, field := range fields[1:] {
		out = append(out, canonicalGeneratedSourceWord(field))
	}
	canonical := strings.Join(out, " ")
	for _, reference := range makeVariableRefs(canonical) {
		if isMakeAutomaticVariable(reference) {
			continue
		}
		return canonical, fmt.Errorf(
			"the command still references $(%s), which is not an automatic variable", reference)
	}
	return canonical, nil
}

// canonicalGeneratedSourceProgram classifies the leading word of a command.
//
// PERL, AWK and CONFIG_SHELL are not part of the default variable environment,
// because only arch/<srcarch>/Makefile is parsed, so they normally survive
// expansion as literal references. That is not an invariant: "-var" is
// user-populated and module_make_vars are merged in, so a consumer can define
// them, in which case they arrive as a resolved interpreter path. Both forms
// have to classify the same, which is why this tokenises instead of comparing
// strings.
func canonicalGeneratedSourceProgram(word string) (string, error) {
	switch word {
	case "$(PERL)", "${PERL}":
		return "PERL", nil
	case "$(AWK)", "${AWK}":
		return "AWK", nil
	case "$(CONFIG_SHELL)", "${CONFIG_SHELL}", "$(SHELL)", "${SHELL}":
		return "SHELL", nil
	}
	if containsMakeReference(word) {
		return "", fmt.Errorf("the leading program word %q is an unrecognised make reference", word)
	}
	switch strings.ToLower(path.Base(word)) {
	case "perl":
		return "PERL", nil
	case "awk", "gawk", "mawk":
		return "AWK", nil
	case "sh", "bash", "dash":
		return "SHELL", nil
	}
	// An in-tree path with no extension is a hostprog built from the kernel
	// tree, which this project reimplements in Go. Its basename is part of the
	// token, because which hostprog it is determines what runs.
	if strings.ContainsRune(word, '/') && path.Ext(word) == "" {
		return "HOSTPROG(" + path.Base(word) + ")", nil
	}
	return "", fmt.Errorf("the leading program word %q is not a recognised interpreter or hostprog", word)
}

// canonicalGeneratedSourceWord replaces in-tree paths with placeholders so the
// canonical form is version- and architecture-independent.
func canonicalGeneratedSourceWord(word string) string {
	if !strings.ContainsRune(word, '/') {
		return word
	}
	base := path.Base(word)
	switch {
	case base == "unroll.awk":
		return "%[unroll]"
	case strings.HasSuffix(base, ".sh"):
		return "%[script]"
	}
	return word
}

// foldMakeAutomaticVariables rewrites the parenthesised spellings of make's
// automatic variables into the short ones. 6.12 writes "$(<)" on arm and arm64
// but "$<" on x86 for what is otherwise the same command.
func foldMakeAutomaticVariables(command string) string {
	return makeAutomaticVariableFolder.Replace(command)
}

var makeAutomaticVariableFolder = strings.NewReplacer(
	"$(<)", "$<", "${<}", "$<",
	"$(@)", "$@", "${@}", "$@",
	"$(*)", "$*", "${*}", "$*",
	"$(^)", "$^", "${^}", "$^",
)

// generatedSourcePrerequisitesExcept returns the non-primary prerequisites.
func generatedSourcePrerequisitesExcept(gs KbuildGeneratedSource, exclude string) []string {
	out := make([]string, 0, len(gs.Prerequisites))
	for _, prerequisite := range gs.Prerequisites {
		if prerequisite == exclude {
			continue
		}
		out = append(out, prerequisite)
	}
	return out
}

// generatedSourceScriptPrerequisite picks the prerequisite with the given
// extension, which is how a script-driven generator names its own script.
func generatedSourceScriptPrerequisite(gs KbuildGeneratedSource, extension string) string {
	for _, prerequisite := range gs.Prerequisites {
		if strings.HasSuffix(prerequisite, extension) {
			return prerequisite
		}
	}
	return ""
}

// generatedSourceHostprogSources maps extension-less hostprog prerequisites to
// the C source they are built from, which is the file that must be digested.
func generatedSourceHostprogSources(gs KbuildGeneratedSource) []string {
	var out []string
	for _, prerequisite := range gs.Prerequisites {
		if path.Ext(prerequisite) != "" {
			continue
		}
		out = append(out, prerequisite+".c")
	}
	return out
}
