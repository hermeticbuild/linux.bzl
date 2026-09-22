package toolaction

import (
	"fmt"
	"strings"
)

// ValidateStaticConfigAssignments proves that sourcing a generated CONFIG_
// file only assigns static shell words. A quoted value with command substitution
// would run each time a source-derived shell phase imports that file, so the
// check must also be repeated against the bytes staged for actual execution.
func ValidateStaticConfigAssignments(contents string) error {
	if contents == "" {
		return nil
	}
	if !strings.HasSuffix(contents, "\n") || strings.ContainsAny(contents, "\x00\r") {
		return fmt.Errorf("auto.conf requires LF-terminated assignment lines")
	}
	for index, line := range strings.Split(strings.TrimSuffix(contents, "\n"), "\n") {
		key, value, assigned := strings.Cut(line, "=")
		if !assigned || !strings.HasPrefix(key, "CONFIG_") || len(key) == len("CONFIG_") {
			return fmt.Errorf("auto.conf line %d is not a CONFIG_ assignment", index+1)
		}
		for _, ch := range key[len("CONFIG_"):] {
			if ch != '_' && (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') {
				return fmt.Errorf("auto.conf line %d has an invalid shell assignment name", index+1)
			}
		}
		if strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) && len(value) >= 2 {
			for offset := 1; offset < len(value)-1; offset++ {
				ch := value[offset]
				if ch == '$' || ch == '`' || ch == '"' {
					return fmt.Errorf("auto.conf line %d has an executable or unquoted string value", index+1)
				}
				if ch == '\\' {
					offset++
					if offset == len(value)-1 || value[offset] == '$' || value[offset] == '`' {
						return fmt.Errorf("auto.conf line %d has an unsafe string escape", index+1)
					}
				}
			}
			continue
		}
		if value == "" {
			return fmt.Errorf("auto.conf line %d has an empty unquoted value", index+1)
		}
		for _, ch := range value {
			if ch != '_' && ch != '.' && ch != '+' && ch != '-' && ch != '/' && ch != ':' &&
				(ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') {
				return fmt.Errorf("auto.conf line %d has an unsafe unquoted value", index+1)
			}
		}
	}
	return nil
}
