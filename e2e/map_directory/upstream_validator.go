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
	compilerFamily := flag.String("compiler_family", "", "selected compiler family (clang or gcc)")
	outPath := flag.String("out", "", "validation stamp")
	flag.Parse()
	if *configPath == "" || (*compilerFamily != "clang" && *compilerFamily != "gcc") || *outPath == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: upstream_validator -config FILE -compiler_family clang|gcc -out FILE")
		os.Exit(2)
	}

	values, err := readConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read resolved upstream config: %v\n", err)
		os.Exit(1)
	}
	want := map[string]string{
		"CONFIG_64BIT":                             "y",
		"CONFIG_AS_HAS_NON_CONST_ULEB128":          "y",
		"CONFIG_AS_WRUSS":                          "y",
		"CONFIG_CC_HAS_AUTO_VAR_INIT_ZERO_ENABLER": "n",
		"CONFIG_CC_HAS_ASM_GOTO_OUTPUT":            "y",
		"CONFIG_CC_HAS_ASM_GOTO_TIED_OUTPUT":       "y",
		"CONFIG_CC_HAS_ASM_INLINE":                 "y",
		"CONFIG_CC_HAS_ASSUME":                     "y",
		"CONFIG_CC_HAS_AUTO_VAR_INIT_PATTERN":      "y",
		"CONFIG_CC_HAS_AUTO_VAR_INIT_ZERO":         "y",
		"CONFIG_CC_HAS_AUTO_VAR_INIT_ZERO_BARE":    "y",
		"CONFIG_CC_HAS_COUNTED_BY":                 "y",
		"CONFIG_CC_HAS_ENTRY_PADDING":              "y",
		"CONFIG_CC_HAS_IBT":                        "y",
		"CONFIG_CC_HAS_INT128":                     "y",
		"CONFIG_CC_HAS_KASAN_GENERIC":              "y",
		"CONFIG_CC_HAS_KASAN_SW_TAGS":              "y",
		"CONFIG_CC_HAS_MARCH_NATIVE":               "y",
		"CONFIG_CC_HAS_MULTIDIMENSIONAL_NONSTRING": "y",
		"CONFIG_CC_HAS_NAMED_AS_FIXED_SANITIZERS":  "y",
		"CONFIG_CC_HAS_NO_PROFILE_FN_ATTR":         "y",
		"CONFIG_CC_HAS_RETURN_THUNK":               "y",
		"CONFIG_CC_HAS_SANE_FUNCTION_ALIGNMENT":    "y",
		"CONFIG_CC_HAS_SLS":                        "y",
		"CONFIG_CC_HAS_WORKING_NOSANITIZE_ADDRESS": "y",
		"CONFIG_CC_HAS_ZERO_CALL_USED_REGS":        "y",
		"CONFIG_GCC_NO_STRINGOP_OVERFLOW":          "y",
		"CONFIG_GCC_PLUGINS":                       "n",
		"CONFIG_HAVE_KCSAN_COMPILER":               "y",
		"CONFIG_LD_CAN_USE_KEEP_IN_OVERLAY":        "y",
		"CONFIG_STACKPROTECTOR":                    "y",
		"CONFIG_STACKPROTECTOR_STRONG":             "y",
		"CONFIG_TOOLS_SUPPORT_RELR":                "y",
		"CONFIG_X86":                               "y",
		"CONFIG_X86_64":                            "y",
	}
	if *compilerFamily == "clang" {
		addExpected(valuesForClang(), want)
	} else {
		addExpected(valuesForGCC(), want)
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
	if len(mismatches) != 0 {
		sort.Strings(mismatches)
		fmt.Fprintln(os.Stderr, strings.Join(mismatches, "\n"))
		os.Exit(1)
	}
	if err := os.WriteFile(*outPath, []byte(fmt.Sprintf("upstream Linux 6.18.39 Kconfig resolved with %s\n", want["CONFIG_CC_VERSION_TEXT"])), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write validation stamp: %v\n", err)
		os.Exit(1)
	}
}

func addExpected(from, to map[string]string) {
	for key, value := range from {
		to[key] = value
	}
}

func valuesForClang() map[string]string {
	return map[string]string{
		"CONFIG_AS_IS_GNU":  "n",
		"CONFIG_AS_IS_LLVM": "y",
		"CONFIG_AS_VERSION": "220108",
		// The hermetic LLVM toolchain does not expose a hosted libc link closure
		// to this freestanding Linux probe action, so this capability resolves n.
		"CONFIG_CC_CAN_LINK":                        "n",
		"CONFIG_CC_HAS_KCFI_ARITY":                  "y",
		"CONFIG_CC_HAS_MIN_FUNCTION_ALIGNMENT":      "n",
		"CONFIG_CC_HAS_NAMED_AS":                    "n",
		"CONFIG_CC_HAS_RANDSTRUCT":                  "y",
		"CONFIG_CC_HAS_SANCOV_STACK_DEPTH_CALLBACK": "y",
		"CONFIG_CC_IMPLICIT_FALLTHROUGH":            "-Wimplicit-fallthrough",
		"CONFIG_CC_IS_CLANG":                        "y",
		"CONFIG_CC_IS_GCC":                          "n",
		"CONFIG_CC_NO_ARRAY_BOUNDS":                 "n",
		"CONFIG_CC_NO_STRINGOP_OVERFLOW":            "n",
		"CONFIG_CC_VERSION_TEXT":                    "clang version 22.1.8None",
		"CONFIG_CLANG_VERSION":                      "220108",
		"CONFIG_GCC_VERSION":                        "0",
		"CONFIG_HAVE_KMSAN_COMPILER":                "y",
		"CONFIG_LD_IS_BFD":                          "n",
		"CONFIG_LD_IS_LLD":                          "y",
		"CONFIG_LD_VERSION":                         "0",
		"CONFIG_LLD_VERSION":                        "220108",
	}
}

func valuesForGCC() map[string]string {
	return map[string]string{
		"CONFIG_AS_IS_GNU":  "y",
		"CONFIG_AS_IS_LLVM": "n",
		"CONFIG_AS_VERSION": "24600",
		// gcc_toolchain's libc linker scripts contain absolute symlinks and
		// are not a relocatable action input closure. The fixture deliberately
		// fails this capability closed on every executor.
		"CONFIG_CC_CAN_LINK":                        "n",
		"CONFIG_CC_HAS_KCFI_ARITY":                  "n",
		"CONFIG_CC_HAS_MIN_FUNCTION_ALIGNMENT":      "y",
		"CONFIG_CC_HAS_NAMED_AS":                    "y",
		"CONFIG_CC_HAS_RANDSTRUCT":                  "n",
		"CONFIG_CC_HAS_SANCOV_STACK_DEPTH_CALLBACK": "n",
		"CONFIG_CC_IMPLICIT_FALLTHROUGH":            "-Wimplicit-fallthrough=5",
		"CONFIG_CC_IS_CLANG":                        "n",
		"CONFIG_CC_IS_GCC":                          "y",
		"CONFIG_CC_NO_ARRAY_BOUNDS":                 "y",
		"CONFIG_CC_NO_STRINGOP_OVERFLOW":            "y",
		"CONFIG_CC_VERSION_TEXT":                    "x86_64-linux-gcc (GCC) 15.2.0",
		"CONFIG_CLANG_VERSION":                      "0",
		"CONFIG_GCC_VERSION":                        "150200",
		"CONFIG_HAVE_KMSAN_COMPILER":                "n",
		"CONFIG_LD_IS_BFD":                          "y",
		"CONFIG_LD_IS_LLD":                          "n",
		"CONFIG_LD_VERSION":                         "24600",
		"CONFIG_LLD_VERSION":                        "0",
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
