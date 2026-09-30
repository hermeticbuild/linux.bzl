package kconfig

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// ParseConfig reads a Linux .config file into explicit CONFIG_* values.
// Canonical unset comments are explicit n values; other comments are ignored.
// This distinction is required when a resolved config is consumed again: an
// omitted symbol follows its Kconfig default, while an unset symbol must remain
// disabled.
func ParseConfig(r io.Reader) (map[string]string, error) {
	flags := map[string]string{}
	scanner := bufio.NewScanner(r)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if key, ok := parseUnsetConfig(line); ok {
			if err := setParsedConfig(flags, key, "n", lineNo); err != nil {
				return nil, err
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected CONFIG_* assignment", lineNo)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !isConfigKey(key) {
			return nil, fmt.Errorf("line %d: expected CONFIG_* key, got %q", lineNo, key)
		}
		if err := setParsedConfig(flags, key, value, lineNo); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return flags, nil
}

func parseUnsetConfig(line string) (string, bool) {
	const (
		prefix = "# "
		suffix = " is not set"
	)
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, suffix) {
		return "", false
	}
	key := strings.TrimSuffix(strings.TrimPrefix(line, prefix), suffix)
	return key, isConfigKey(key) && !strings.ContainsAny(key, " \t")
}

func setParsedConfig(flags map[string]string, key, value string, lineNo int) error {
	if _, ok := flags[key]; ok {
		return fmt.Errorf("line %d: duplicate config key %q", lineNo, key)
	}
	flags[key] = value
	return nil
}

func isConfigKey(key string) bool {
	return strings.HasPrefix(key, "CONFIG_") && len(key) > len("CONFIG_")
}

// ResolvedConfig contains native Kconfig values and the declared symbol inventory.
// Symbols absent from native .config are retained as n for dependency analysis.
type ResolvedConfig struct {
	Effective map[string]string
}
