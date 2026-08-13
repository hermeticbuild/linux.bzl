package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strconv"
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
		fatalf("read resolved upstream config: %v", err)
	}
	if values["CONFIG_X86_64"] != "y" || values["CONFIG_64BIT"] != "y" {
		fatalf("resolved config is not the requested x86_64 configuration")
	}

	compiler := exactlyOne(values, "CONFIG_CC_IS_CLANG", "CONFIG_CC_IS_GCC")
	assembler := exactlyOne(values, "CONFIG_AS_IS_LLVM", "CONFIG_AS_IS_GNU")
	linker := exactlyOne(values, "CONFIG_LD_IS_LLD", "CONFIG_LD_IS_BFD")
	versionText := values["CONFIG_CC_VERSION_TEXT"]
	if versionText == "" {
		fatalf("CONFIG_CC_VERSION_TEXT was not measured")
	}

	if compiler == "CONFIG_CC_IS_CLANG" {
		requirePositive(values, "CONFIG_CLANG_VERSION")
		requireZero(values, "CONFIG_GCC_VERSION")
	} else {
		requirePositive(values, "CONFIG_GCC_VERSION")
		requireZero(values, "CONFIG_CLANG_VERSION")
	}
	if assembler == "CONFIG_AS_IS_LLVM" {
		requirePositive(values, "CONFIG_AS_VERSION")
	} else {
		requirePositive(values, "CONFIG_AS_VERSION")
	}
	if linker == "CONFIG_LD_IS_LLD" {
		requirePositive(values, "CONFIG_LLD_VERSION")
		requireZero(values, "CONFIG_LD_VERSION")
	} else {
		requirePositive(values, "CONFIG_LD_VERSION")
		requireZero(values, "CONFIG_LLD_VERSION")
	}

	stamp := fmt.Sprintf("compiler=%s\nassembler=%s\nlinker=%s\nversion=%s\n", compiler, assembler, linker, versionText)
	if err := os.WriteFile(*outPath, []byte(stamp), 0o644); err != nil {
		fatalf("write validation stamp: %v", err)
	}
}

func exactlyOne(values map[string]string, names ...string) string {
	selected := ""
	for _, name := range names {
		if values[name] == "y" {
			if selected != "" {
				fatalf("both %s and %s were selected", selected, name)
			}
			selected = name
		}
	}
	if selected == "" {
		fatalf("none of %s was selected", strings.Join(names, ", "))
	}
	return selected
}

func requirePositive(values map[string]string, name string) {
	value, err := strconv.ParseUint(values[name], 10, 64)
	if err != nil || value == 0 {
		fatalf("%s=%q, want a positive measured version", name, values[name])
	}
}

func requireZero(values map[string]string, name string) {
	if values[name] != "0" {
		fatalf("%s=%q, want 0 for the unselected tool family", name, values[name])
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
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
