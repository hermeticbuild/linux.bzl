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
	"sort"
	"strconv"
	"strings"
)

var (
	ncapintsRE = regexp.MustCompile(`^\s*#\s*define\s+NCAPINTS\s+([0-9]+)\b`)
	featureRE  = regexp.MustCompile(`^\s*#\s*define\s+X86_FEATURE_([A-Za-z0-9_]+)\s+\(\s*([0-9]+)\s*\*\s*32\s*\+\s*([0-9]+)\s*\)`)
)

func main() {
	cpufeatures := flag.String("cpufeatures", "", "arch/x86/include/asm/cpufeatures.h input")
	config := flag.String("config", "", "Resolved Linux .config input")
	out := flag.String("out", "", "Generated cpufeaturemasks.h output")
	flag.Parse()

	if *cpufeatures == "" || *config == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "-cpufeatures, -config, and -out are required")
		os.Exit(2)
	}
	if err := run(*cpufeatures, *config, *out); err != nil {
		fmt.Fprintf(os.Stderr, "cpufeaturemasks: %v\n", err)
		os.Exit(1)
	}
}

func run(cpufeaturesPath, configPath, outPath string) error {
	features, ncapints, err := parseCPUFeatures(cpufeaturesPath)
	if err != nil {
		return err
	}
	if ncapints == 0 {
		return fmt.Errorf("%s: NCAPINTS not found", cpufeaturesPath)
	}
	enabled, err := parseFeatureConfig(configPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	var out strings.Builder
	out.WriteString("#ifndef _ASM_X86_CPUFEATUREMASKS_H\n")
	out.WriteString("#define _ASM_X86_CPUFEATUREMASKS_H\n\n")
	for _, set := range []string{"REQUIRED", "DISABLED"} {
		writeMaskSet(&out, set, ncapints, features, enabled[set])
	}
	out.WriteString("#endif /* _ASM_X86_CPUFEATUREMASKS_H */\n")
	return os.WriteFile(outPath, []byte(out.String()), 0o644)
}

func parseCPUFeatures(path string) (map[int]string, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()

	features := map[int]string{}
	ncapints := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if match := ncapintsRE.FindStringSubmatch(line); match != nil {
			ncapints, err = strconv.Atoi(match[1])
			if err != nil {
				return nil, 0, err
			}
			continue
		}
		match := featureRE.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		word, err := strconv.Atoi(match[2])
		if err != nil {
			return nil, 0, err
		}
		bit, err := strconv.Atoi(match[3])
		if err != nil {
			return nil, 0, err
		}
		if bit < 0 || bit >= 32 {
			return nil, 0, fmt.Errorf("%s: feature %s has invalid bit %d", path, match[1], bit)
		}
		features[word*32+bit] = match[1]
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, err
	}
	return features, ncapints, nil
}

func parseFeatureConfig(path string) (map[string]map[string]bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	out := map[string]map[string]bool{
		"REQUIRED": {},
		"DISABLED": {},
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		if !ok || value != "y" || !strings.HasPrefix(key, "CONFIG_X86_") {
			continue
		}
		rest := strings.TrimPrefix(key, "CONFIG_X86_")
		set, feature, ok := strings.Cut(rest, "_FEATURE_")
		if !ok {
			continue
		}
		if _, valid := out[set]; valid {
			out[set][feature] = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func writeMaskSet(out *strings.Builder, set string, ncapints int, features map[int]string, enabled map[string]bool) {
	masks := make([]uint32, ncapints)
	var names []string
	for i := 0; i < ncapints; i++ {
		for bit := 0; bit < 32; bit++ {
			feature := features[i*32+bit]
			if feature == "" || !enabled[feature] {
				continue
			}
			masks[i] |= uint32(1) << bit
			names = append(names, feature)
		}
	}
	sort.Strings(names)

	fmt.Fprintf(out, "/*\n * %s features:\n *\n", set)
	writeFeatureComment(out, names)
	out.WriteString(" */\n")
	for i, mask := range masks {
		fmt.Fprintf(out, "#define %s_MASK%d\t0x%08xU\n", set, i, mask)
	}
	fmt.Fprintf(out, "\n#define %s_MASK_BIT_SET(x)\t\t\t\\\n", set)
	out.WriteString("\t((\t\t\t\t\t")
	for i, mask := range masks {
		if mask != 0 {
			fmt.Fprintf(out, "\t\\\n\t\t((x) >> 5) == %2d ? %s_MASK%d :", i, set, i)
		}
	}
	out.WriteString(" 0\t\\\n\t) & (1U << ((x) & 31)))\n\n")
}

func writeFeatureComment(out *strings.Builder, names []string) {
	if len(names) == 0 {
		out.WriteString(" *\n")
		return
	}
	line := " *"
	for _, name := range names {
		next := line + " " + name
		if len(next) > 75 {
			out.WriteString(line + "\n")
			line = " *   " + name
			continue
		}
		line = next
	}
	out.WriteString(line + "\n")
}
