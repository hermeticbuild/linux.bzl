package kconfig

import (
	"fmt"
	"sort"
	"strings"
)

var generatedHeaderConfigSymbols = []string{
	"CONFIG_HZ",
	"CONFIG_LOCALVERSION",
	"CONFIG_PREEMPT_BUILD",
	"CONFIG_PREEMPT_DYNAMIC",
	"CONFIG_PREEMPT_RT",
	"CONFIG_SMP",
	"CONFIG_STACKPROTECTOR_PER_TASK",
}

func generatedHeaderFootprint(
	config *ResolvedConfig,
	opts CompactMetadataOptions,
	scanner *configSourceScanner,
) (map[string]string, []CompactSourceInput, string, error) {
	refs := map[string]bool{}
	for _, symbol := range generatedHeaderConfigSymbols {
		refs[symbol] = true
	}
	for _, symbol := range KernelFlagsConfigSymbols() {
		refs[symbol] = true
	}
	if opts.Srcarch == "x86" {
		for symbol := range config.Effective {
			if strings.HasPrefix(symbol, "CONFIG_X86_REQUIRED_FEATURE_") ||
				strings.HasPrefix(symbol, "CONFIG_X86_DISABLED_FEATURE_") {
				refs[symbol] = true
			}
		}
	}

	sourceInputs := []CompactSourceInput{}
	sourcePaths := []string{
		"include/linux/compiler-version.h",
		"include/linux/compiler_types.h",
		"include/linux/kconfig.h",
		"kernel/bounds.c",
		"kernel/sched/rq-offsets.c",
	}
	digestOnlyPaths := []string{
		"Makefile",
		"scripts/setlocalversion",
	}
	switch opts.Srcarch {
	case "x86":
		sourcePaths = append(sourcePaths,
			"arch/x86/kernel/asm-offsets.c",
			"arch/x86/kvm/kvm-asm-offsets.c",
		)
		digestOnlyPaths = append(digestOnlyPaths,
			"arch/x86/Makefile",
			"arch/x86/include/asm/Kbuild",
			"arch/x86/include/asm/cpufeatures.h",
			"arch/x86/include/asm/orc_types.h",
			"arch/x86/include/asm/required-features.h",
			"arch/x86/include/uapi/asm/Kbuild",
			"arch/x86/entry/syscalls/syscall_32.tbl",
			"arch/x86/entry/syscalls/syscall_64.tbl",
			"arch/x86/kvm/Makefile",
			"arch/x86/tools/cpufeaturemasks.awk",
			"include/xen/interface/xen-mca.h",
			"include/xen/interface/xen.h",
			"include/xen/interface/xenpmu.h",
		)
	case "arm64":
		sourcePaths = append(sourcePaths,
			"arch/arm64/kernel/asm-offsets.c",
			"arch/arm64/kvm/hyp/hyp-constants.c",
		)
		digestOnlyPaths = append(digestOnlyPaths,
			"arch/arm/vdso/vdsomunge.c",
			"arch/arm64/tools/cpucaps",
			"arch/arm64/tools/syscall_32.tbl",
			"arch/arm64/tools/syscall_64.tbl",
			"arch/arm64/tools/sysreg",
		)
		if _, ok := scanner.absForTreePath("arch/arm64/include/asm/cfi.h"); ok {
			digestOnlyPaths = append(digestOnlyPaths, "arch/arm64/include/asm/cfi.h")
		}
	}
	for _, source := range sourcePaths {
		if _, ok := scanner.absForTreePath(source); !ok {
			continue
		}
		var includeRoots []string
		if source == "arch/x86/kvm/kvm-asm-offsets.c" {
			includeRoots = []string{"arch/x86/kvm"}
		}
		closure, err := scanner.closureForSourceConfig(source, includeRoots, config)
		if err != nil {
			return nil, nil, "", fmt.Errorf("scan generated-header input %s: %w", source, err)
		}
		for _, ref := range closure.refs {
			refs[ref] = true
		}
		sourceInputs = appendUniqueSourceInputs(sourceInputs, closure.sourceInputs...)
	}
	for _, path := range digestOnlyPaths {
		if _, ok := scanner.absForTreePath(path); !ok {
			continue
		}
		input, err := scanner.inputForTreePath(path)
		if err != nil {
			return nil, nil, "", fmt.Errorf("digest generated-header input %s: %w", path, err)
		}
		sourceInputs = appendUniqueSourceInputs(sourceInputs, input)
	}
	if opts.Srcarch == "arm64" {
		for _, scan := range []struct {
			dir     string
			profile sourceScanProfile
		}{
			{dir: "arch/arm64/kernel/vdso", profile: sourceScanArm64VDSO},
			{dir: "arch/arm64/kernel/vdso32", profile: sourceScanArm32CompatVDSO},
		} {
			closure, err := scanner.closureForSourceDirConfigProfile(scan.dir, config, scan.profile)
			if err != nil {
				return nil, nil, "", fmt.Errorf("scan generated-header input directory %s: %w", scan.dir, err)
			}
			for _, ref := range closure.refs {
				refs[ref] = true
			}
			sourceInputs = appendUniqueSourceInputs(sourceInputs, closure.sourceInputs...)
		}
		for _, scan := range []struct {
			source  string
			profile sourceScanProfile
		}{
			{source: "lib/vdso/getrandom.c", profile: sourceScanArm64VDSO},
			{source: "lib/vdso/gettimeofday.c", profile: sourceScanArm64VDSO},
			{source: "lib/vdso/gettimeofday.c", profile: sourceScanArm32CompatVDSO},
		} {
			if _, ok := scanner.absForTreePath(scan.source); !ok {
				continue
			}
			closure, err := scanner.closureForSourceConfigProfile(scan.source, nil, config, scan.profile)
			if err != nil {
				return nil, nil, "", fmt.Errorf(
					"scan generated-header forced input %s (%s): %w",
					scan.source,
					scan.profile,
					err,
				)
			}
			for _, ref := range closure.refs {
				refs[ref] = true
			}
			sourceInputs = appendUniqueSourceInputs(sourceInputs, closure.sourceInputs...)
		}
	}

	keys := make([]string, 0, len(refs))
	for key := range refs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fragment := make(map[string]string, len(keys))
	for _, key := range keys {
		if config.ShouldWrite(key) {
			fragment[key] = config.Value(key)
		} else {
			fragment[key] = "n"
		}
	}
	return fragment, sourceInputs, "exact", nil
}
