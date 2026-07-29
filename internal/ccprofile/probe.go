package ccprofile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type StructuralProbeTools struct {
	Compiler   string
	Linker     string
	SourceRoot string
	ObjectRoot string
}

func EvaluateStructuralProbe(
	ctx context.Context,
	profile Profile,
	request StructuralProbe,
	tools StructuralProbeTools,
) (bool, error) {
	if tools.Compiler == "" {
		return false, fmt.Errorf("structural probe compiler must be non-empty")
	}
	if tools.Linker == "" {
		return false, fmt.Errorf("structural probe linker must be non-empty")
	}
	if request.ID != StructuralProbeID(request) {
		return false, fmt.Errorf(
			"structural probe request ID %q, want %q",
			request.ID,
			StructuralProbeID(request),
		)
	}

	prefix, err := expandStructuralProbePaths(request.PrefixArgv, tools)
	if err != nil {
		return false, err
	}
	argv, err := expandStructuralProbePaths(request.Argv, tools)
	if err != nil {
		return false, err
	}

	var executable string
	switch request.Kind {
	case "cc-disable-warning":
		if request.Language != "c" || len(argv) != 1 {
			return false, fmt.Errorf(
				"cc-disable-warning structural probe requires c language and one warning name",
			)
		}
		executable = tools.Compiler
		argv = append(prefix, "-W"+argv[0])
		argv = compilerStructuralProbeArgv(profile, argv, "c")
	case "cc-option", "cc-option-yn":
		if request.Language != "c" || len(argv) == 0 {
			return false, fmt.Errorf(
				"%s structural probe requires c language and non-empty argv",
				request.Kind,
			)
		}
		executable = tools.Compiler
		for index, arg := range argv {
			if strings.HasPrefix(arg, "-Wno-") {
				argv[index] = "-W" + strings.TrimPrefix(arg, "-Wno-")
			}
		}
		argv = compilerStructuralProbeArgv(profile, append(prefix, argv...), "c")
	case "as-option":
		if request.Language != "asm" || len(argv) == 0 {
			return false, fmt.Errorf(
				"as-option structural probe requires asm language and non-empty argv",
			)
		}
		executable = tools.Compiler
		argv = compilerStructuralProbeArgv(profile, append(prefix, argv...), "assembler-with-cpp")
	case "ld-option":
		if request.Language != "link" || len(argv) == 0 {
			return false, fmt.Errorf(
				"ld-option structural probe requires link language and non-empty argv",
			)
		}
		executable = tools.Linker
		argv = append(append(prefix, argv...), "-v")
	default:
		return false, fmt.Errorf("unsupported structural probe kind %q", request.Kind)
	}

	command := exec.CommandContext(ctx, executable, argv...)
	err = command.Run()
	if err == nil {
		return true, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return false, nil
	}
	return false, fmt.Errorf("execute %s structural probe %s: %w", request.Kind, request.ID, err)
}

func compilerStructuralProbeArgv(profile Profile, argv []string, language string) []string {
	out := make([]string, 0, len(argv)+8)
	if target := profile.AnalysisIdentity.TargetGNUSystemName; target != "" {
		out = append(out, "--target="+target)
	}
	out = append(out, argv...)
	out = append(out, "-c", "-x", language, os.DevNull)
	out = append(out, "-o", os.DevNull)
	return out
}

func expandStructuralProbePaths(
	argv []string,
	tools StructuralProbeTools,
) ([]string, error) {
	out := make([]string, len(argv))
	for index, arg := range argv {
		for _, replacement := range []struct {
			token string
			path  string
		}{
			{token: StructuralProbeSourceRoot, path: tools.SourceRoot},
			{token: StructuralProbeObjectRoot, path: tools.ObjectRoot},
		} {
			if !strings.Contains(arg, replacement.token) {
				continue
			}
			if replacement.path == "" {
				return nil, fmt.Errorf(
					"structural probe argument %q requires a path for %s",
					arg,
					replacement.token,
				)
			}
			arg = strings.ReplaceAll(
				arg,
				replacement.token,
				filepath.ToSlash(filepath.Clean(replacement.path)),
			)
		}
		out[index] = arg
	}
	return out, nil
}
