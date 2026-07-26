package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	config := flag.String("config", "", "resolved Linux .config")
	objtool := flag.String("objtool", "", "objtool executable")
	in := flag.String("in", "", "input vmlinux.o")
	mode := flag.String("mode", "vmlinux", "objtool mode: builtin, module, or vmlinux")
	out := flag.String("out", "", "output vmlinux.o")
	flag.Parse()

	if *config == "" || *objtool == "" || *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "-config, -objtool, -in, and -out are required")
		os.Exit(2)
	}
	if err := run(*config, *objtool, *in, *out, *mode); err != nil {
		fmt.Fprintf(os.Stderr, "objtoolrun: %v\n", err)
		os.Exit(1)
	}
}

func run(configPath, objtoolPath, inPath, outPath, mode string) error {
	config, err := readConfig(configPath)
	if err != nil {
		return err
	}
	args, enabled, err := objtoolArgs(config, mode)
	if err != nil {
		return err
	}
	if !enabled {
		return copyFile(inPath, outPath)
	}
	if err := copyFile(inPath, outPath); err != nil {
		return err
	}
	if err := os.Chmod(outPath, 0o600); err != nil {
		return err
	}

	args = append(args, outPath)
	cmd := exec.Command(objtoolPath, args...)
	cmd.Env = []string{}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func readConfig(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	config := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		config[key] = value
	}
	return config, scanner.Err()
}

func objtoolArgs(config map[string]string, mode string) ([]string, bool, error) {
	if !enabled(config, "CONFIG_OBJTOOL") {
		return nil, false, nil
	}

	delayObjtool := enabled(config, "CONFIG_LTO_CLANG") || enabled(config, "CONFIG_X86_KERNEL_IBT")
	mcountObjtool := enabled(config, "CONFIG_FTRACE_MCOUNT_USE_OBJTOOL")
	noinstrValidation := enabled(config, "CONFIG_NOINSTR_VALIDATION")
	switch mode {
	case "builtin":
		if delayObjtool {
			return nil, false, nil
		}
		return commonObjtoolArgs(config), true, nil
	case "module":
		args := commonObjtoolArgs(config)
		if delayObjtool {
			args = append(args, "--link")
		}
		args = append(args, "--module")
		return args, true, nil
	case "vmlinux":
		if !delayObjtool && !mcountObjtool && !noinstrValidation {
			return nil, false, nil
		}
		args := []string{}
		if delayObjtool {
			args = commonObjtoolArgs(config)
		} else {
			args = append(args, mcountObjtoolArgs(config)...)
			if enabled(config, "CONFIG_OBJTOOL_WERROR") {
				args = append(args, "--Werror")
			}
		}
		if noinstrValidation {
			args = append(args, "--noinstr")
			if enabled(config, "CONFIG_MITIGATION_UNRET_ENTRY") || enabled(config, "CONFIG_MITIGATION_SRSO") {
				args = append(args, "--unret")
			}
		}
		args = append(args, "--link")
		return args, true, nil
	default:
		return nil, false, fmt.Errorf("unsupported -mode %q", mode)
	}
}

func commonObjtoolArgs(config map[string]string) []string {
	var args []string
	if enabled(config, "CONFIG_HAVE_JUMP_LABEL_HACK") {
		args = append(args, "--hacks=jump_label")
	}
	if enabled(config, "CONFIG_HAVE_NOINSTR_HACK") {
		args = append(args, "--hacks=noinstr")
	}
	if enabled(config, "CONFIG_MITIGATION_CALL_DEPTH_TRACKING") {
		args = append(args, "--hacks=skylake")
	}
	if enabled(config, "CONFIG_X86_KERNEL_IBT") {
		args = append(args, "--ibt")
	}
	if enabled(config, "CONFIG_FINEIBT") {
		args = append(args, "--cfi")
	}
	if enabled(config, "CONFIG_UNWINDER_ORC") {
		args = append(args, "--orc")
	}
	if enabled(config, "CONFIG_MITIGATION_RETPOLINE") {
		args = append(args, "--retpoline")
	}
	if enabled(config, "CONFIG_MITIGATION_RETHUNK") {
		args = append(args, "--rethunk")
	}
	if enabled(config, "CONFIG_MITIGATION_SLS") {
		args = append(args, "--sls")
	}
	if enabled(config, "CONFIG_STACK_VALIDATION") {
		args = append(args, "--stackval")
	}
	if enabled(config, "CONFIG_HAVE_STATIC_CALL_INLINE") {
		args = append(args, "--static-call")
	}
	if enabled(config, "CONFIG_HAVE_UACCESS_VALIDATION") {
		args = append(args, "--uaccess")
	}
	if enabled(config, "CONFIG_GCOV_KERNEL") || enabled(config, "CONFIG_KCOV") {
		args = append(args, "--no-unreachable")
	}
	if enabled(config, "CONFIG_PREFIX_SYMBOLS") {
		args = append(args, "--prefix="+config["CONFIG_FUNCTION_PADDING_BYTES"])
	}

	args = append(args, mcountObjtoolArgs(config)...)

	if enabled(config, "CONFIG_OBJTOOL_WERROR") {
		args = append(args, "--Werror")
	}
	return args
}

func mcountObjtoolArgs(config map[string]string) []string {
	var args []string
	if enabled(config, "CONFIG_FTRACE_MCOUNT_USE_OBJTOOL") {
		args = append(args, "--mcount")
		if enabled(config, "CONFIG_HAVE_OBJTOOL_NOP_MCOUNT") {
			args = append(args, "--mnop")
		}
	}
	return args
}

func enabled(config map[string]string, key string) bool {
	return config[key] == "y"
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
