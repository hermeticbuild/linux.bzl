package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

func main() {
	configPath := flag.String("config", "", "resolved upstream Linux .config")
	outPath := flag.String("out", "", "validation stamp")
	flag.Parse()
	if *configPath == "" || *outPath == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: upstream_validator -config FILE -out FILE")
		os.Exit(2)
	}

	values, err := readConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read resolved upstream config: %v\n", err)
		os.Exit(1)
	}
	for _, key := range []string{
		"CONFIG_AS_IS_LLVM",
		"CONFIG_CC_CAN_LINK",
		"CONFIG_CC_IS_CLANG",
		"CONFIG_LD_IS_LLD",
	} {
		if values[key] == "" {
			values[key] = "n"
		}
	}
	want := map[string]string{
		"CONFIG_64BIT":                             "y",
		"CONFIG_AS_HAS_NON_CONST_ULEB128":          "y",
		"CONFIG_AS_IS_GNU":                         "y",
		"CONFIG_AS_IS_LLVM":                        "n",
		"CONFIG_AS_VERSION":                        "24600",
		"CONFIG_AS_WRUSS":                          "y",
		"CONFIG_CC_HAS_ASM_GOTO_OUTPUT":            "y",
		"CONFIG_CC_HAS_ASM_GOTO_TIED_OUTPUT":       "y",
		"CONFIG_CC_HAS_ASM_INLINE":                 "y",
		"CONFIG_CC_HAS_ASSUME":                     "y",
		"CONFIG_CC_HAS_AUTO_VAR_INIT_PATTERN":      "y",
		"CONFIG_CC_HAS_AUTO_VAR_INIT_ZERO":         "y",
		"CONFIG_CC_HAS_AUTO_VAR_INIT_ZERO_BARE":    "y",
		"CONFIG_CC_HAS_COUNTED_BY":                 "y",
		"CONFIG_CC_HAS_ENTRY_PADDING":              "y",
		"CONFIG_CC_HAS_INT128":                     "y",
		"CONFIG_CC_HAS_KASAN_GENERIC":              "y",
		"CONFIG_CC_HAS_KASAN_SW_TAGS":              "y",
		"CONFIG_CC_HAS_MARCH_NATIVE":               "y",
		"CONFIG_CC_HAS_MIN_FUNCTION_ALIGNMENT":     "y",
		"CONFIG_CC_HAS_MULTIDIMENSIONAL_NONSTRING": "y",
		"CONFIG_CC_HAS_NAMED_AS":                   "y",
		"CONFIG_CC_HAS_NAMED_AS_FIXED_SANITIZERS":  "y",
		"CONFIG_CC_HAS_NO_PROFILE_FN_ATTR":         "y",
		"CONFIG_CC_HAS_RETURN_THUNK":               "y",
		"CONFIG_CC_HAS_SANE_FUNCTION_ALIGNMENT":    "y",
		"CONFIG_CC_HAS_WORKING_NOSANITIZE_ADDRESS": "y",
		"CONFIG_CC_HAS_ZERO_CALL_USED_REGS":        "y",
		"CONFIG_CC_IMPLICIT_FALLTHROUGH":           "-Wimplicit-fallthrough=5",
		"CONFIG_CC_NO_ARRAY_BOUNDS":                "y",
		"CONFIG_CC_NO_STRINGOP_OVERFLOW":           "y",
		// Keep the feasibility test executor-independent until gcc_toolchain's
		// sysroot can be materialized without absolute symlinks in Bazel sandboxes.
		"CONFIG_CC_CAN_LINK":              "n",
		"CONFIG_CC_IS_CLANG":              "n",
		"CONFIG_CC_IS_GCC":                "y",
		"CONFIG_CLANG_VERSION":            "0",
		"CONFIG_GCC_NO_STRINGOP_OVERFLOW": "y",
		"CONFIG_GCC_VERSION":              "150200",
		// gcc_toolchain does not expose its plugin header as a declared target;
		// sandboxed probing must therefore reject that ambient capability.
		"CONFIG_GCC_PLUGINS":                "n",
		"CONFIG_LD_CAN_USE_KEEP_IN_OVERLAY": "y",
		"CONFIG_LD_IS_BFD":                  "y",
		"CONFIG_LD_IS_LLD":                  "n",
		"CONFIG_LD_VERSION":                 "24600",
		"CONFIG_LLD_VERSION":                "0",
		"CONFIG_TOOLS_SUPPORT_RELR":         "y",
		"CONFIG_X86":                        "y",
		"CONFIG_X86_64":                     "y",
	}
	if values["CONFIG_CC_HAS_IBT"] != "y" && values["CONFIG_CC_HAS_IBT"] != "n" {
		fmt.Fprintf(os.Stderr, "CONFIG_CC_HAS_IBT=%q, want a resolved compiler capability\n", values["CONFIG_CC_HAS_IBT"])
		os.Exit(1)
	}
	if values["CONFIG_CC_HAS_SLS"] != "y" && values["CONFIG_CC_HAS_SLS"] != "n" {
		fmt.Fprintf(os.Stderr, "CONFIG_CC_HAS_SLS=%q, want a resolved compiler capability\n", values["CONFIG_CC_HAS_SLS"])
		os.Exit(1)
	}
	var mismatches []string
	for key, expected := range want {
		actual := values[key]
		// The resolver omits disabled hidden symbols from .config. Kconfig's
		// effective value for those absent bools is still n.
		if actual == "" && expected == "n" {
			actual = "n"
		}
		if actual != expected {
			mismatches = append(mismatches, fmt.Sprintf("%s=%q, want %q", key, actual, expected))
		}
	}
	if got := values["CONFIG_CC_VERSION_TEXT"]; !strings.Contains(got, "15.2.0") || !strings.Contains(strings.ToLower(got), "gcc") {
		mismatches = append(mismatches, fmt.Sprintf("CONFIG_CC_VERSION_TEXT=%q, want GCC 15.2.0 identity", got))
	}
	if len(mismatches) != 0 {
		sort.Strings(mismatches)
		fmt.Fprintln(os.Stderr, strings.Join(mismatches, "\n"))
		os.Exit(1)
	}
	if err := os.WriteFile(*outPath, []byte("upstream Linux 6.18.39 Kconfig resolved with GCC 15.2.0\n"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write validation stamp: %v\n", err)
		os.Exit(1)
	}
}

func readConfig(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "# CONFIG_") && strings.HasSuffix(line, " is not set") {
			key := strings.TrimSuffix(strings.TrimPrefix(line, "# "), " is not set")
			values[key] = "n"
			continue
		}
		if !strings.HasPrefix(line, "CONFIG_") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = strings.Trim(value, `"`)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}
