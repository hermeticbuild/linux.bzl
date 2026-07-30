package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/ccprofile"
)

func writeTestCommandTemplate(
	t *testing.T,
	dir string,
	compiler string,
	environment map[string]string,
) string {
	t.Helper()
	template := ccprofile.CommandTemplate{
		Schema:         ccprofile.CommandTemplateSchema,
		Architecture:   "x86_64",
		DriverContract: ccprofile.DriverContract,
		AnalysisIdentity: ccprofile.AnalysisIdentity{
			Compiler:            "clang",
			TargetGNUSystemName: "x86_64-unknown-linux-gnu",
		},
		Compiler:            compiler,
		MutableArgv:         []string{"__LINUX_BZL_KBUILD_FLAGS__"},
		Environment:         environment,
		KbuildFlagsSentinel: "__LINUX_BZL_KBUILD_FLAGS__",
	}
	data, err := ccprofile.CanonicalCommandTemplateJSON(template)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "template.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTestArgv(t *testing.T, path string, argv ...string) {
	t.Helper()
	if err := writeArgvFile(path, argv); err != nil {
		t.Fatal(err)
	}
}

func writeTestCompilerIdentity(
	t *testing.T,
	dir string,
) (ccprofile.CompilerIdentity, string) {
	t.Helper()
	identity := ccprofile.CompilerIdentity{
		Schema:         ccprofile.CompilerIdentitySchema,
		Architecture:   "x86_64",
		DriverContract: ccprofile.DriverContract,
		AnalysisIdentity: ccprofile.AnalysisIdentity{
			Compiler:            "clang",
			TargetGNUSystemName: "x86_64-unknown-linux-gnu",
		},
		CCName:        "Clang",
		CCVersion:     220108,
		CCVersionText: "clang version 22.1.8",
		BuiltinMacros: map[string]string{},
	}
	data, err := ccprofile.CanonicalCompilerIdentityJSON(identity)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "identity.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return identity, path
}

func TestResolveNodeSelectsBranchFromKbuildGraphProbe(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "compiler.argv")
	compiler := filepath.Join(dir, "compiler")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$LOG_PATH"
case " $* " in
  *" -funsupported "*) exit 1 ;;
esac
exit 0
`
	if err := os.WriteFile(compiler, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	template := writeTestCommandTemplate(
		t,
		dir,
		compiler,
		map[string]string{"LOG_PATH": logPath},
	)
	validation := writeValidationStamp(t, dir)
	contextPath := filepath.Join(dir, "context.argv")
	truePath := filepath.Join(dir, "true.argv")
	falsePath := filepath.Join(dir, "false.argv")
	writeTestArgv(t, contextPath, "-Werror")
	writeTestArgv(t, truePath, "-DSELECTED_TRUE")
	writeTestArgv(t, falsePath, "-DSELECTED_FALSE")

	for _, test := range []struct {
		name      string
		kind      string
		candidate string
		want      string
	}{
		{
			name:      "supported compact kind",
			kind:      "cc_option",
			candidate: "-fno-pic",
			want:      "-DSELECTED_TRUE\n",
		},
		{
			name:      "unsupported",
			kind:      "cc_option",
			candidate: "-funsupported",
			want:      "-DSELECTED_FALSE\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			out := filepath.Join(dir, test.name+".argv")
			err := run([]string{
				"resolve-node",
				"-template", template,
				"-validation", validation,
				"-linker", compiler,
				"-kind", test.kind,
				"-language", "c",
				"-srcarch", "x86",
				"-candidate_arg=" + test.candidate,
				"-context", contextPath,
				"-when_true", truePath,
				"-when_false", falsePath,
				"-source_root", dir,
				"-out", out,
			})
			if err != nil {
				t.Fatalf("resolve-node failed: %v", err)
			}
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(data); got != test.want {
				t.Fatalf("selected branch = %q, want %q", got, test.want)
			}
		})
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "--target=x86_64-unknown-linux-gnu") ||
		!strings.Contains(string(log), "-Werror") {
		t.Fatalf("probe argv omitted template identity/context:\n%s", log)
	}
}

func TestResolveNodeRejectsInvalidArgvFileBeforeExecution(t *testing.T) {
	dir := t.TempDir()
	invoked := filepath.Join(dir, "invoked")
	compiler := filepath.Join(dir, "compiler")
	if err := os.WriteFile(
		compiler,
		[]byte("#!/bin/sh\n: > "+invoked+"\n"),
		0o755,
	); err != nil {
		t.Fatal(err)
	}
	template := writeTestCommandTemplate(t, dir, compiler, map[string]string{})
	contextPath := filepath.Join(dir, "context.argv")
	if err := os.WriteFile(contextPath, []byte("-Werror\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	truePath := filepath.Join(dir, "true.argv")
	falsePath := filepath.Join(dir, "false.argv")
	writeTestArgv(t, truePath, "-DTRUE")
	writeTestArgv(t, falsePath, "-DFALSE")
	err := run([]string{
		"resolve-node",
		"-template", template,
		"-validation", writeValidationStamp(t, dir),
		"-linker", compiler,
		"-kind", "cc-option",
		"-language", "c",
		"-srcarch", "x86",
		"-candidate_arg=-fno-pic",
		"-context", contextPath,
		"-when_true", truePath,
		"-when_false", falsePath,
		"-source_root", dir,
		"-out", filepath.Join(dir, "out.argv"),
	})
	if err == nil || !strings.Contains(err.Error(), "CR or NUL") {
		t.Fatalf("resolve-node error = %v", err)
	}
	if _, err := os.Stat(invoked); !os.IsNotExist(err) {
		t.Fatalf("compiler was invoked after invalid argv: %v", err)
	}
}

func TestConcatNodePreservesInputOrder(t *testing.T) {
	dir := t.TempDir()
	left := filepath.Join(dir, "left.argv")
	right := filepath.Join(dir, "right.argv")
	out := filepath.Join(dir, "out.argv")
	writeTestArgv(t, left, "-DLEFT=1", "-DORDER=left")
	writeTestArgv(t, right, "-DRIGHT=1", "-DORDER=right")
	if err := run([]string{
		"concat-node",
		"-input", left,
		"-input", right,
		"-out", out,
	}); err != nil {
		t.Fatalf("concat-node failed: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := "-DLEFT=1\n-DORDER=left\n-DRIGHT=1\n-DORDER=right\n"
	if got := string(data); got != want {
		t.Fatalf("concatenated argv = %q, want %q", got, want)
	}
}

func TestLinkFiltersExactArgumentsAndUsesRawLDOrder(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "link.argv")
	linker := filepath.Join(dir, "ld")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\n", logPath)
	if err := os.WriteFile(linker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	add := filepath.Join(dir, "add.argv")
	remove := filepath.Join(dir, "remove.argv")
	writeTestArgv(t, add, "--add-keep", "--drop")
	writeTestArgv(t, remove, "--drop")
	out := filepath.Join(dir, "linked.o")
	linkerScript := filepath.Join(dir, "layout.lds")
	inputA := filepath.Join(dir, "a.o")
	inputB := filepath.Join(dir, "b.o")
	if err := run([]string{
		"link",
		"-linker", linker,
		"-validation", writeValidationStamp(t, dir),
		"-flags_file", add,
		"-remove_flags_file", remove,
		"-base_arg=-r",
		"-base_arg=--drop",
		"-linker_script", linkerScript,
		"-output", out,
		"-input", inputA,
		"-input", inputB,
	}); err != nil {
		t.Fatalf("link failed: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	want := []string{
		"-r",
		"--add-keep",
		"-T",
		linkerScript,
		"-o",
		out,
		inputA,
		inputB,
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("link argv = %#v, want %#v", got, want)
	}
}

func TestCompileExpandsFileProgramsAndNestedResponses(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "compile.argv")
	compiler := filepath.Join(dir, "compiler")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$LOG_PATH"
output=
next=
for arg in "$@"; do
  if [ "$next" = 1 ]; then
    output=$arg
    next=
  elif [ "$arg" = "-o" ]; then
    next=1
  fi
done
: > "$output"
`
	if err := os.WriteFile(compiler, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	template := writeTestCommandTemplate(
		t,
		dir,
		compiler,
		map[string]string{"LOG_PATH": logPath},
	)
	response := filepath.Join(dir, "config.argv")
	writeTestArgv(t, response, "-DRESPONSE_KEEP", "-DRESPONSE_DROP")
	flagsPath := filepath.Join(dir, "flags.argv")
	writeTestArgv(
		t,
		flagsPath,
		"-I$(src)",
		"-I${srctree}/include",
		"-I$(obj)",
		"$(CLANG_FLAGS)",
		"@"+response,
	)
	removePath := filepath.Join(dir, "remove.argv")
	writeTestArgv(t, removePath, "-DRESPONSE_DROP")
	sourceDir := filepath.Join(dir, "source", "kernel")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(sourceDir, "value.c")
	if err := os.WriteFile(source, []byte("int value;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "value.o")
	if err := run([]string{
		"compile",
		"-template", template,
		"-validation", writeValidationStamp(t, dir),
		"-source", source,
		"-output", output,
		"-flags_file", flagsPath,
		"-remove_flags_file", removePath,
		"-source_root", filepath.Join(dir, "source"),
		"-object_path", "kernel/value.o",
	}); err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	want := []string{
		"-I" + sourceDir,
		"-I" + filepath.Join(dir, "source", "include"),
		"-Ikernel",
		"-DRESPONSE_KEEP",
		"-c",
		source,
		"-o",
		output,
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("compile argv = %#v, want %#v", got, want)
	}
}

func TestValidateGraphAcceptsConsumedProjectionThroughProfileFlag(t *testing.T) {
	dir := t.TempDir()
	identity, identityPath := writeTestCompilerIdentity(t, dir)
	compiler := filepath.Join(dir, "compiler")
	if err := os.WriteFile(compiler, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	template := writeTestCommandTemplate(t, dir, compiler, map[string]string{})
	success := true
	command := ccprofile.KconfigCommand{
		Kind:        ccprofile.KconfigCommandKindSuccess,
		Command:     "test -e include/generated/autoconf.h",
		Environment: map[string]string{},
		Inputs:      map[string]string{},
		Success:     &success,
	}
	command.ID = ccprofile.KconfigCommandID(command)
	projection := ccprofile.GraphProjection{
		Architecture:     identity.Architecture,
		DriverContract:   identity.DriverContract,
		AnalysisIdentity: identity.AnalysisIdentity,
		KconfigCommands:  []ccprofile.KconfigCommand{command},
	}
	projectionData, err := ccprofile.CanonicalGraphProjectionJSON(projection)
	if err != nil {
		t.Fatal(err)
	}
	projectionPath := filepath.Join(dir, "projection.json")
	if err := os.WriteFile(projectionPath, projectionData, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "validated")
	if err := run([]string{
		"validate-graph",
		"-profile", projectionPath,
		"-identity", identityPath,
		"-template", template,
		"-linker", compiler,
		"-shell", "/bin/sh",
		"-source_root", dir,
		"-out", out,
	}); err != nil {
		t.Fatalf("validate-graph failed: %v", err)
	}
	digest, err := ccprofile.GraphProjectionDigest(projection)
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(stamp), "profile_digest="+digest+"\n") {
		t.Fatalf("validation stamp does not pin projection digest:\n%s", stamp)
	}
	if _, err := ccprofile.DecodeValidationStamp(stamp); err != nil {
		t.Fatalf("validation stamp is invalid: %v", err)
	}
}

func TestValidateGraphReplaysKbuildGraphProbe(t *testing.T) {
	dir := t.TempDir()
	identity, identityPath := writeTestCompilerIdentity(t, dir)
	compiler := filepath.Join(dir, "compiler")
	script := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "-fgraph-probe" ]; then
    exit 0
  fi
done
exit 1
`
	if err := os.WriteFile(compiler, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	template := writeTestCommandTemplate(t, dir, compiler, map[string]string{})
	request, err := ccprofile.NewKbuildGraphProbeIdentity(
		ccprofile.KbuildGraphProbeKindCCOption,
		"c",
		[]string{"-Werror"},
		[]string{"-fgraph-probe"},
		map[string]string{},
	)
	if err != nil {
		t.Fatal(err)
	}
	probe := ccprofile.KbuildGraphProbe{
		ID:            request.ID,
		Kind:          request.Kind,
		Language:      request.Language,
		ContextArgv:   request.ContextArgv,
		CandidateArgv: request.CandidateArgv,
		Inputs:        request.Inputs,
		Supported:     true,
	}
	writeProjection := func(name string, supported bool) string {
		t.Helper()
		probe.Supported = supported
		data, err := ccprofile.CanonicalGraphProjectionJSON(ccprofile.GraphProjection{
			Architecture:      identity.Architecture,
			DriverContract:    identity.DriverContract,
			AnalysisIdentity:  identity.AnalysisIdentity,
			KbuildGraphProbes: []ccprofile.KbuildGraphProbe{probe},
		})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	validate := func(projection, out string) error {
		t.Helper()
		return run([]string{
			"validate-graph",
			"-projection", projection,
			"-identity", identityPath,
			"-template", template,
			"-linker", compiler,
			"-shell", "/bin/sh",
			"-source_root", dir,
			"-out", out,
		})
	}

	if err := validate(
		writeProjection("matching", true),
		filepath.Join(dir, "matching.stamp"),
	); err != nil {
		t.Fatalf("validate matching graph probe: %v", err)
	}

	err = validate(
		writeProjection("mismatched", false),
		filepath.Join(dir, "mismatched.stamp"),
	)
	if err == nil ||
		!strings.Contains(err.Error(), probe.ID) ||
		!strings.Contains(err.Error(), "result mismatch") ||
		!strings.Contains(err.Error(), "recorded supported=false") ||
		!strings.Contains(err.Error(), "returned supported=true") {
		t.Fatalf("validate mismatched graph probe error = %v", err)
	}
}

func TestRefreshGraphCommandWritesConfiguredResults(t *testing.T) {
	dir := t.TempDir()
	identity, identityPath := writeTestCompilerIdentity(t, dir)
	compiler := filepath.Join(dir, "compiler")
	if err := os.WriteFile(
		compiler,
		[]byte("#!/bin/sh\ncase \" $* \" in *\" -fgood \"*) exit 0;; esac\nexit 1\n"),
		0o755,
	); err != nil {
		t.Fatal(err)
	}
	template := writeTestCommandTemplate(t, dir, compiler, map[string]string{})
	unsupported := false
	command := ccprofile.KconfigCommand{
		Kind:        ccprofile.KconfigCommandKindSuccess,
		Command:     "clang -fgood",
		Environment: map[string]string{"CC": "clang", "LD": "ld.lld"},
		Inputs:      map[string]string{},
		Success:     &unsupported,
	}
	command.ID = ccprofile.KconfigCommandID(command)
	profile := ccprofile.GraphProfile{
		Schema:           ccprofile.GraphProfileSchema,
		Architecture:     identity.Architecture,
		DriverContract:   identity.DriverContract,
		AnalysisIdentity: identity.AnalysisIdentity,
		KconfigCommands:  []ccprofile.KconfigCommand{command},
	}
	data, err := ccprofile.CanonicalGraphProfileJSON(profile)
	if err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(dir, "recorded.json")
	if err := os.WriteFile(profilePath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "refreshed.json")
	if err := run([]string{
		"refresh-graph",
		"-profile", profilePath,
		"-identity", identityPath,
		"-template", template,
		"-linker", compiler,
		"-shell", "/bin/sh",
		"-source_root", dir,
		"-out", out,
	}); err != nil {
		t.Fatalf("refresh-graph failed: %v", err)
	}
	refreshedData, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := ccprofile.DecodeGraphProfile(refreshedData)
	if err != nil {
		t.Fatalf("decode refreshed graph: %v", err)
	}
	if refreshed.KconfigCommands[0].ID != command.ID ||
		refreshed.KconfigCommands[0].Success == nil ||
		!*refreshed.KconfigCommands[0].Success {
		t.Fatalf("refreshed command = %#v", refreshed.KconfigCommands[0])
	}
}

func TestMergeGraphCommandWritesCanonicalUnion(t *testing.T) {
	dir := t.TempDir()
	identity := ccprofile.AnalysisIdentity{
		Compiler:            "clang",
		TargetGNUSystemName: "x86_64-unknown-linux-gnu",
	}
	makeProfile := func(path, commandText string) ccprofile.KconfigCommand {
		success := true
		command := ccprofile.KconfigCommand{
			Kind:        ccprofile.KconfigCommandKindSuccess,
			Command:     commandText,
			Environment: map[string]string{},
			Inputs:      map[string]string{},
			Success:     &success,
		}
		command.ID = ccprofile.KconfigCommandID(command)
		data, err := ccprofile.CanonicalGraphProfileJSON(ccprofile.GraphProfile{
			Schema:           ccprofile.GraphProfileSchema,
			Architecture:     "x86_64",
			DriverContract:   ccprofile.DriverContract,
			AnalysisIdentity: identity,
			KconfigCommands:  []ccprofile.KconfigCommand{command},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return command
	}
	leftPath := filepath.Join(dir, "left.json")
	rightPath := filepath.Join(dir, "right.json")
	left := makeProfile(leftPath, "test left")
	right := makeProfile(rightPath, "test right")
	out := filepath.Join(dir, "merged.json")
	if err := run([]string{
		"merge-graph",
		"-input", leftPath,
		"-input", rightPath,
		"-out", out,
	}); err != nil {
		t.Fatalf("merge-graph failed: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := ccprofile.DecodeGraphProfile(data)
	if err != nil {
		t.Fatalf("merged output is not canonical: %v", err)
	}
	if got, want := len(merged.KconfigCommands), 2; got != want {
		t.Fatalf("merged command count = %d, want %d", got, want)
	}
	ids := map[string]bool{
		merged.KconfigCommands[0].ID: true,
		merged.KconfigCommands[1].ID: true,
	}
	if !ids[left.ID] || !ids[right.ID] {
		t.Fatalf("merged IDs = %#v, want %s and %s", ids, left.ID, right.ID)
	}
}
