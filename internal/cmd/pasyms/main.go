package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

func main() {
	nm := flag.String("nm", "", "llvm-nm executable")
	out := flag.String("out", "", "Generated pasyms.h output")
	flag.Parse()

	if *nm == "" || *out == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "-nm, -out and at least one object input are required")
		os.Exit(2)
	}
	if err := run(*nm, *out, flag.Args()); err != nil {
		fmt.Fprintf(os.Stderr, "pasyms: %v\n", err)
		os.Exit(1)
	}
}

func run(nm, out string, objects []string) error {
	cmd := exec.Command(nm, objects...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%s failed: %w\n%s", nm, err, stderr.String())
	}

	lines := map[string]struct{}{}
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		if !isPASYMType(fields[1]) {
			continue
		}
		name := fields[2]
		lines[fmt.Sprintf("pa_%s = %s;", name, name)] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	sorted := slices.Sorted(maps.Keys(lines))

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return os.WriteFile(out, []byte(strings.Join(sorted, "\n")+"\n"), 0o644)
}

func isPASYMType(value string) bool {
	if len(value) != 1 {
		return false
	}
	switch value[0] {
	case 'A', 'B', 'C', 'D', 'G', 'R', 'S', 'T', 'V', 'W':
		return true
	default:
		return false
	}
}
