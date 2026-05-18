// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	build "github.com/bazelbuild/buildtools/build"

	"linux.bzl/internal/kconfig/buildgen"
)

var (
	kconfigPath       = flag.String("kconfig", "", "Path to the input Linux .config file")
	buildfilePath     = flag.String("buildfile", "", "Path to the BUILD file to update")
	ruleName          = flag.String("rule", "", "Name attribute of the kconfig_import rule or generated kconfig_file target")
	outPath           = flag.String("out", "", "Path to write the updated BUILD file. Defaults to updating -buildfile in place")
	generateBuildfile = flag.Bool("generate_buildfile", false, "Generate a standalone BUILD file containing a kconfig_file target")
	configLabel       = flag.String("config_label", "", "Bazel label for the source .config in generated BUILD-file mode")
	stripConfigFlags  = flag.Bool("strip_config_flags", false, "Remove config_flags from the named kconfig_import rule instead of updating it")
	visibilityFlags   stringListFlag
)

func main() {
	flag.Var(&visibilityFlags, "visibility", "Visibility label to emit in generated BUILD-file mode. May be repeated")
	flag.Parse()

	if *stripConfigFlags {
		if *buildfilePath == "" || *ruleName == "" {
			flag.PrintDefaults()
			os.Exit(2)
		}
		if err := stripConfigFlagsInBuildfile(*buildfilePath, *ruleName, *outPath); err != nil {
			fmt.Fprintf(os.Stderr, "failed to strip config_flags: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *generateBuildfile {
		if *kconfigPath == "" || *ruleName == "" || *configLabel == "" || *outPath == "" {
			flag.PrintDefaults()
			os.Exit(2)
		}
		if err := generateConfigBuildfile(*kconfigPath, *outPath, *ruleName, *configLabel, visibilityFlags); err != nil {
			fmt.Fprintf(os.Stderr, "failed to generate config BUILD file: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *kconfigPath == "" || *buildfilePath == "" || *ruleName == "" {
		flag.PrintDefaults()
		os.Exit(2)
	}

	flags, err := readKconfigFile(*kconfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to parse kconfig: %v\n", err)
		os.Exit(1)
	}

	buildfile, err := os.ReadFile(*buildfilePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read buildfile: %v\n", err)
		os.Exit(1)
	}

	updated, err := updateBuildfile(buildfile, *buildfilePath, *ruleName, flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to update buildfile: %v\n", err)
		os.Exit(1)
	}

	dst := *outPath
	if dst == "" {
		dst = *buildfilePath
	}
	if err := os.WriteFile(dst, updated, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write buildfile: %v\n", err)
		os.Exit(1)
	}
}

type stringListFlag []string

func (f *stringListFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *stringListFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func readKconfigFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	return parseKconfig(file)
}

func parseKconfig(r io.Reader) (map[string]string, error) {
	flags := map[string]string{}
	scanner := bufio.NewScanner(r)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if _, ok := parseUnset(line); ok {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected CONFIG_* assignment or unset comment", lineNo)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !isConfigKey(key) {
			return nil, fmt.Errorf("line %d: expected CONFIG_* key, got %q", lineNo, key)
		}
		if err := setConfig(flags, key, value, lineNo); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return flags, nil
}

func parseUnset(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, "# ")
	if !ok {
		return "", false
	}
	key, ok := strings.CutSuffix(rest, " is not set")
	if !ok || !isConfigKey(key) {
		return "", false
	}
	return key, true
}

func setConfig(flags map[string]string, key, value string, lineNo int) error {
	if _, ok := flags[key]; ok {
		return fmt.Errorf("line %d: duplicate config key %q", lineNo, key)
	}
	flags[key] = value
	return nil
}

func isConfigKey(key string) bool {
	return strings.HasPrefix(key, "CONFIG_") && len(key) > len("CONFIG_")
}

func updateBuildfile(buildfile []byte, filename, rule string, flags map[string]string) ([]byte, error) {
	if filename == "" {
		filename = "BUILD.bazel"
	}
	file, err := build.ParseBuild(filename, buildfile)
	if err != nil {
		return nil, err
	}

	var matches []*build.Rule
	for _, candidate := range file.Rules("kconfig_import") {
		if candidate.Name() == rule {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("kconfig_import rule %q not found", rule)
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("kconfig_import rule %q found %d times", rule, len(matches))
	}

	matches[0].SetAttr("config_flags", configFlagsExpr(flags))
	return build.Format(file), nil
}

func stripConfigFlagsInBuildfile(path, rule, out string) error {
	buildfile, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	updated, err := removeConfigFlags(buildfile, path, rule)
	if err != nil {
		return err
	}
	if out == "" {
		out = path
	}
	return os.WriteFile(out, updated, 0o644)
}

func removeConfigFlags(buildfile []byte, filename, rule string) ([]byte, error) {
	if filename == "" {
		filename = "BUILD.bazel"
	}
	file, err := build.ParseBuild(filename, buildfile)
	if err != nil {
		return nil, err
	}

	var matches []*build.Rule
	for _, candidate := range file.Rules("kconfig_import") {
		if candidate.Name() == rule {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("kconfig_import rule %q not found", rule)
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("kconfig_import rule %q found %d times", rule, len(matches))
	}
	matches[0].DelAttr("config_flags")
	return build.Format(file), nil
}

func generateConfigBuildfile(kconfig, out, rule, label string, visibility []string) error {
	flags, err := readKconfigFile(kconfig)
	if err != nil {
		return fmt.Errorf("parse kconfig: %w", err)
	}
	generated, err := configBuildfile(out, rule, label, flags, visibility)
	if err != nil {
		return err
	}
	return os.WriteFile(out, generated, 0o644)
}

func configBuildfile(filename, rule, label string, flags map[string]string, visibility []string) ([]byte, error) {
	if len(visibility) == 0 {
		visibility = []string{"//visibility:public"}
	}

	file := buildgen.NewBuildFile(filename, "# Generated by kconfig. Do not edit.")
	file.AddLoad("@linux.bzl//internal:kconfig.bzl", "kconfig_file")
	file.AddExportsFiles([]string{"BUILD.bazel"})
	target := file.AddRule("kconfig_file", rule)
	target.SetAttr("config", label)
	target.SetAttr("config_flags", configFlagsExpr(flags))
	target.SetAttr("visibility", visibility)
	return file.Format(), nil
}

func configFlagsExpr(flags map[string]string) *build.DictExpr {
	keys := make([]string, 0, len(flags))
	for key := range flags {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	expr := &build.DictExpr{
		ForceMultiLine: true,
	}
	for _, key := range keys {
		expr.List = append(expr.List, &build.KeyValueExpr{
			Key:   &build.StringExpr{Value: key},
			Value: &build.StringExpr{Value: flags[key]},
		})
	}
	return expr
}
