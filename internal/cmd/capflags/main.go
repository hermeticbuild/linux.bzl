// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var defineRE = regexp.MustCompile(`^\s*#\s*define\s+([A-Za-z0-9_]+)\b(.*)$`)

type flagEntry struct {
	name  string
	value string
}

func main() {
	cpufeatures := flag.String("cpufeatures", "", "arch/x86/include/asm/cpufeatures.h input")
	vmxfeatures := flag.String("vmxfeatures", "", "arch/x86/include/asm/vmxfeatures.h input")
	out := flag.String("out", "", "Generated capflags.c output")
	flag.Parse()

	if *cpufeatures == "" || *vmxfeatures == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "-cpufeatures, -vmxfeatures, and -out are required")
		os.Exit(2)
	}
	if err := run(*cpufeatures, *vmxfeatures, *out); err != nil {
		fmt.Fprintf(os.Stderr, "capflags: %v\n", err)
		os.Exit(1)
	}
}

func run(cpufeaturesPath, vmxfeaturesPath, outPath string) error {
	capFlags, err := parseFlags(cpufeaturesPath, "X86_FEATURE_")
	if err != nil {
		return err
	}
	bugFlags, err := parseFlags(cpufeaturesPath, "X86_BUG_")
	if err != nil {
		return err
	}
	vmxFlags, err := parseFlags(vmxfeaturesPath, "VMX_FEATURE_")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	var out strings.Builder
	out.WriteString("#ifndef _ASM_X86_CPUFEATURES_H\n")
	out.WriteString("#include <asm/cpufeatures.h>\n")
	out.WriteString("#endif\n\n")
	writeArray(&out, "x86_cap_flags", "NCAPINTS*32", "X86_FEATURE_", "", capFlags)
	out.WriteByte('\n')
	writeArray(&out, "x86_bug_flags", "NBUGINTS*32", "X86_BUG_", "NCAPINTS*32", bugFlags)
	out.WriteByte('\n')
	out.WriteString("#ifdef CONFIG_X86_VMX_FEATURE_NAMES\n")
	out.WriteString("#ifndef _ASM_X86_VMXFEATURES_H\n")
	out.WriteString("#include <asm/vmxfeatures.h>\n")
	out.WriteString("#endif\n")
	writeArray(&out, "x86_vmx_flags", "NVMXINTS*32", "VMX_FEATURE_", "", vmxFlags)
	out.WriteString("#endif /* CONFIG_X86_VMX_FEATURE_NAMES */\n")
	return os.WriteFile(outPath, []byte(out.String()), 0o644)
}

func parseFlags(path, prefix string) ([]flagEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var out []flagEntry
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.ReplaceAll(scanner.Text(), "\t", " ")
		match := defineRE.FindStringSubmatch(line)
		if match == nil || !strings.HasPrefix(match[1], prefix) {
			continue
		}
		value := quotedCommentValue(match[2])
		if value == "" {
			continue
		}
		out = append(out, flagEntry{
			name:  strings.TrimPrefix(match[1], prefix),
			value: strings.ToLower(value),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func quotedCommentValue(rest string) string {
	start := strings.Index(rest, "/*")
	end := strings.LastIndex(rest, "*/")
	if start < 0 || end < start {
		return ""
	}
	comment := rest[start+2 : end]
	first := strings.IndexByte(comment, '"')
	if first < 0 {
		return ""
	}
	comment = comment[first+1:]
	second := strings.IndexByte(comment, '"')
	if second < 0 {
		return ""
	}
	return comment[:second]
}

func writeArray(out *strings.Builder, name, size, prefix, postfix string, entries []flagEntry) {
	fmt.Fprintf(out, "const char * const %s[%s] = {\n", name, size)
	for _, entry := range entries {
		if postfix != "" {
			fmt.Fprintf(out, "\t[%s%s - %s]\t\t\t\t = \"%s\",\n", prefix, entry.name, postfix, entry.value)
		} else {
			fmt.Fprintf(out, "\t[%s%s]\t\t\t\t = \"%s\",\n", prefix, entry.name, entry.value)
		}
	}
	out.WriteString("};\n")
}
