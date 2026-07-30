package main

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"
)

func runGrep(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var (
		ignoreCase bool
		invert     bool
		quiet      bool
		pattern    string
	)
	for len(args) > 0 {
		arg := args[0]
		if arg == "--" {
			args = args[1:]
			break
		}
		if arg == "-e" {
			if len(args) < 2 || pattern != "" {
				fmt.Fprintln(stderr, "grep: expected exactly one pattern")
				return 2
			}
			pattern = args[1]
			args = args[2:]
			continue
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			break
		}
		for _, option := range strings.TrimPrefix(arg, "-") {
			switch option {
			case 'i':
				ignoreCase = true
			case 'q':
				quiet = true
			case 'v':
				invert = true
			default:
				fmt.Fprintf(stderr, "grep: unsupported option -%c\n", option)
				return 2
			}
		}
		args = args[1:]
	}
	if pattern == "" {
		if len(args) == 0 {
			fmt.Fprintln(stderr, "grep: missing pattern")
			return 2
		}
		pattern = args[0]
		args = args[1:]
	}
	if len(args) > 1 || (len(args) == 1 && args[0] != "-") {
		fmt.Fprintln(stderr, "grep: file operands are unsupported")
		return 2
	}
	if ignoreCase {
		pattern = "(?i:" + pattern + ")"
	}
	expression, err := regexp.Compile(pattern)
	if err != nil {
		fmt.Fprintf(stderr, "grep: invalid pattern: %v\n", err)
		return 2
	}

	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	matched := false
	for scanner.Scan() {
		selected := expression.MatchString(scanner.Text())
		if invert {
			selected = !selected
		}
		if !selected {
			continue
		}
		matched = true
		if quiet {
			return 0
		}
		fmt.Fprintln(stdout, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(stderr, "grep: read stdin: %v\n", err)
		return 2
	}
	if matched {
		return 0
	}
	return 1
}
