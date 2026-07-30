// Package ccprofile defines the toolchain data shared by repository generation
// and Bazel actions.
package ccprofile

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	DriverContract         = "gnu-cc-response-v1"
	GraphProfileObjectRoot = "__LINUX_BZL_OBJTREE__"
)

var macroNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type AnalysisIdentity struct {
	Compiler            string `json:"compiler"`
	TargetGNUSystemName string `json:"target_gnu_system_name"`
}

func validateText(value, name string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s contains NUL", name)
	}
	return nil
}
