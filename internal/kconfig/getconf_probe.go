package kconfig

// This file lowers Linux's host large-file getconf queries into generic probe
// actions. The queried confstr constant remains source data: the planner only
// validates a bounded C identifier and mechanically prefixes it with _CS_.

import (
	"encoding/base64"
	"fmt"
	"strings"
)

const (
	linuxGetconfPrefix         = "getconf LFS_"
	linuxGetconfRedirect       = " 2>/dev/null"
	linuxGetconfMaximumNameLen = 128
	linuxGetconfRunScript      = "exec \"$1\"\n"
)

// linuxGetconfOutput recognizes the exact source-owned getconf form used to
// derive host large-file flags. The query is intrinsically host-owned, so even
// a target-first source phase records it against the configured host toolset.
// Malformed forms become owned errors without falling through to an ambient
// command implementation.
func (e *LinuxProbeEvaluator) linuxGetconfOutput(command string) (string, bool, error) {
	name, recognized, err := parseLinuxGetconfQuery(command)
	if !recognized {
		return "", false, nil
	}
	if err != nil {
		return "", true, fmt.Errorf("invalid source-owned Linux getconf query: %w", err)
	}
	request, err := linuxGetconfProbeRequest(name)
	if err != nil {
		return "", true, err
	}
	value, err := e.requestTextInScope("host", request)
	return value, true, err
}

func parseLinuxGetconfQuery(command string) (string, bool, error) {
	if !strings.HasPrefix(command, linuxGetconfPrefix) {
		return "", false, nil
	}
	query := strings.TrimPrefix(command, "getconf ")
	if strings.HasSuffix(query, linuxGetconfRedirect) {
		query = strings.TrimSuffix(query, linuxGetconfRedirect)
	}
	if err := validateLinuxGetconfName(query); err != nil {
		return "", true, err
	}
	return query, true, nil
}

func validateLinuxGetconfName(name string) error {
	if len(name) <= len("LFS_") || len(name) > linuxGetconfMaximumNameLen ||
		!strings.HasPrefix(name, "LFS_") || !linuxProbePreprocessorSymbol.MatchString(name) {
		return fmt.Errorf("getconf name %q is not a bounded LFS_ C identifier", name)
	}
	return nil
}

func linuxGetconfProbeRequest(name string) (ProbeRequest, error) {
	if err := validateLinuxGetconfName(name); err != nil {
		return ProbeRequest{}, err
	}
	constant := "_CS_" + name
	source := fmt.Sprintf(`#define _POSIX_C_SOURCE 200809L
#include <stddef.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>

int main(void) {
#ifdef %[1]s
	size_t size;
	char *value;
	int failed;

	size = confstr(%[1]s, NULL, 0);
	if (size == 0) {
		return 0;
	}
	value = malloc(size);
	if (value == NULL) {
		return 2;
	}
	if (confstr(%[1]s, value, size) == 0) {
		free(value);
		return 0;
	}
	failed = fputs(value, stdout) == EOF;
	free(value);
	return failed ? 3 : 0;
#else
	return 0;
#endif
}
`, constant)
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema,
		Scratch: []ProbeScratch{
			{Name: "getconf", Kind: "file"},
			{Name: "getconf.c", Kind: "file", Content: source},
			{Name: "getconf.o", Kind: "file"},
		},
		Steps: []ProbeStep{
			{
				Name: "compile", Tool: "cc",
				Arguments: []string{"-x", "c", "-c", "${scratch:getconf.c}", "-o", "${scratch:getconf.o}"},
			},
			{
				Name: "link", Tool: "cc",
				Arguments: []string{"${scratch:getconf.o}", "-o", "${scratch:getconf}"},
				When: &ProbePredicate{Operator: "all", Operands: []ProbePredicate{
					{Operator: "exit-zero", Step: "compile"},
					{Operator: "regular-file", Scratch: "getconf.o"},
				}},
			},
			{
				Name: "execute", Tool: linuxProbeScriptRunner,
				Arguments: []string{
					"-interpreter", "${tool:" + linuxProbeScriptRuntime + "}",
					"-interpreter_arg", "sh",
					"-multicall", "${tool:" + linuxProbeScriptRuntime + "}",
					"-script_content_base64", base64.StdEncoding.EncodeToString([]byte(linuxGetconfRunScript)),
					"--", "${scratch:getconf}",
				},
				When: &ProbePredicate{Operator: "all", Operands: []ProbePredicate{
					{Operator: "exit-zero", Step: "link"},
					{Operator: "regular-file", Scratch: "getconf"},
				}},
			},
		},
		Outcome: ProbeOutcome{Kind: "text", Step: "execute", Stream: "stdout", TrimSpace: true},
	}
	if err := request.Validate(); err != nil {
		return ProbeRequest{}, fmt.Errorf("build Linux getconf probe request: %w", err)
	}
	return request, nil
}
