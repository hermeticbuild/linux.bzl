package kconfig

import (
	"fmt"
	"strings"
)

const (
	compactGeneratedHeaderFamilyAll         = "all"
	compactGeneratedHeaderFamilyStatic      = "static"
	compactGeneratedHeaderFamilyTimeconst   = "timeconst"
	compactGeneratedHeaderFamilyCompile     = "compile"
	compactGeneratedHeaderFamilyVersion     = "version"
	compactGeneratedHeaderFamilyUTSRelease  = "utsrelease"
	compactGeneratedHeaderFamilyUTSVersion  = "utsversion"
	compactGeneratedHeaderFamilyCPUFeatures = "cpufeatures"
	compactGeneratedHeaderFamilyBounds      = "bounds"
	compactGeneratedHeaderFamilyASMOffsets  = "asm_offsets"
	compactGeneratedHeaderFamilyRQOffsets   = "rq_offsets"
	compactGeneratedHeaderFamilyKVMOffsets  = "kvm_offsets"
	generatedHeaderProducerABIKey           = "COMPILE_ENVIRONMENT_ABI"
)

var generatedHeaderConfigSymbols = []string{
	"CONFIG_GCC_PLUGINS",
	"CONFIG_HZ",
	"CONFIG_LOCALVERSION",
	"CONFIG_PREEMPT_BUILD",
	"CONFIG_PREEMPT_DYNAMIC",
	"CONFIG_PREEMPT_RT",
	"CONFIG_RANDSTRUCT",
	"CONFIG_SMP",
	"CONFIG_STACKPROTECTOR_PER_TASK",
}

var generatedHeaderOffsetsForcedSources = []compactGeneratedHeaderSource{
	{path: "include/linux/compiler-version.h"},
	{path: "include/linux/kconfig.h"},
	{path: "include/linux/compiler_types.h"},
}

type compactGeneratedHeaderFamilyFootprint struct {
	name         string
	fragment     map[string]string
	dependencies []string
	sourceInputs []CompactSourceInput
}

type compactGeneratedHeaderSource = compactSourceAction

func generatedHeaderOffsetsSources(sources ...compactGeneratedHeaderSource) []compactGeneratedHeaderSource {
	out := append([]compactGeneratedHeaderSource(nil), generatedHeaderOffsetsForcedSources...)
	return append(out, sources...)
}

func generatedHeaderFamilyFootprints(
	config *ResolvedConfig,
	opts CompactMetadataOptions,
	scanner *configSourceScanner,
) ([]compactGeneratedHeaderFamilyFootprint, error) {
	all, err := generatedHeaderAllFootprint(config, opts, scanner)
	if err != nil {
		return nil, err
	}
	if opts.Srcarch != "x86" {
		return bindGeneratedHeaderProducerABI(
			[]compactGeneratedHeaderFamilyFootprint{all},
			opts.CompileEnvironmentABI,
		), nil
	}

	kernelFlagSymbols := KernelFlagsConfigSymbols(opts.Srcarch)
	static, err := generatedHeaderFamilyFootprint(
		config,
		scanner,
		compactGeneratedHeaderFamilyStatic,
		nil,
		nil,
		[]string{
			"arch/x86/include/asm/Kbuild",
			"arch/x86/include/asm/orc_types.h",
			"arch/x86/include/uapi/asm/Kbuild",
			"arch/x86/entry/syscalls/syscall_32.tbl",
			"arch/x86/entry/syscalls/syscall_64.tbl",
			"include/xen/interface/xen-mca.h",
			"include/xen/interface/xen.h",
			"include/xen/interface/xenpmu.h",
		},
		nil,
		nil,
	)
	if err != nil {
		return nil, err
	}
	timeconst, err := generatedHeaderFamilyFootprint(
		config,
		scanner,
		compactGeneratedHeaderFamilyTimeconst,
		[]string{"CONFIG_HZ"},
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		return nil, err
	}
	compile, err := generatedHeaderFamilyFootprint(
		config,
		scanner,
		compactGeneratedHeaderFamilyCompile,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		return nil, err
	}
	version, err := generatedHeaderFamilyFootprint(
		config,
		scanner,
		compactGeneratedHeaderFamilyVersion,
		nil,
		nil,
		nil,
		nil,
		map[string]string{"KERNEL_VERSION": opts.KernelVersion},
	)
	if err != nil {
		return nil, err
	}
	utsrelease, err := generatedHeaderFamilyFootprint(
		config,
		scanner,
		compactGeneratedHeaderFamilyUTSRelease,
		[]string{"CONFIG_LOCALVERSION"},
		nil,
		nil,
		nil,
		map[string]string{"KERNEL_VERSION": opts.KernelVersion},
	)
	if err != nil {
		return nil, err
	}
	utsversion, err := generatedHeaderFamilyFootprint(
		config,
		scanner,
		compactGeneratedHeaderFamilyUTSVersion,
		[]string{
			"CONFIG_PREEMPT_BUILD",
			"CONFIG_PREEMPT_DYNAMIC",
			"CONFIG_PREEMPT_RT",
			"CONFIG_SMP",
		},
		nil,
		nil,
		nil,
		map[string]string{
			"KBUILD_BUILD_TIMESTAMP": "1970-01-01T00:00:00Z",
			"KBUILD_BUILD_VERSION":   "1",
		},
	)
	if err != nil {
		return nil, err
	}

	cpufeatureSymbols := []string{}
	for symbol := range config.Effective {
		if strings.HasPrefix(symbol, "CONFIG_X86_REQUIRED_FEATURE_") ||
			strings.HasPrefix(symbol, "CONFIG_X86_DISABLED_FEATURE_") {
			cpufeatureSymbols = append(cpufeatureSymbols, symbol)
		}
	}
	cpufeatures, err := generatedHeaderFamilyFootprint(
		config,
		scanner,
		compactGeneratedHeaderFamilyCPUFeatures,
		cpufeatureSymbols,
		nil,
		[]string{
			"arch/x86/include/asm/cpufeatures.h",
			"arch/x86/include/asm/required-features.h",
		},
		nil,
		nil,
	)
	if err != nil {
		return nil, err
	}

	bounds, err := generatedHeaderFamilyFootprint(
		config,
		scanner,
		compactGeneratedHeaderFamilyBounds,
		kernelFlagSymbols,
		generatedHeaderOffsetsSources(compactGeneratedHeaderSource{path: "kernel/bounds.c"}),
		nil,
		nil,
		nil,
	)
	if err != nil {
		return nil, err
	}
	asmOffsets, err := generatedHeaderFamilyFootprint(
		config,
		scanner,
		compactGeneratedHeaderFamilyASMOffsets,
		kernelFlagSymbols,
		generatedHeaderOffsetsSources(compactGeneratedHeaderSource{path: "arch/x86/kernel/asm-offsets.c"}),
		nil,
		nil,
		nil,
	)
	if err != nil {
		return nil, err
	}
	rqOffsets, err := generatedHeaderFamilyFootprint(
		config,
		scanner,
		compactGeneratedHeaderFamilyRQOffsets,
		kernelFlagSymbols,
		generatedHeaderOffsetsSources(compactGeneratedHeaderSource{path: "kernel/sched/rq-offsets.c"}),
		nil,
		nil,
		nil,
	)
	if err != nil {
		return nil, err
	}
	kvmOffsets, err := generatedHeaderFamilyFootprint(
		config,
		scanner,
		compactGeneratedHeaderFamilyKVMOffsets,
		kernelFlagSymbols,
		generatedHeaderOffsetsSources(compactGeneratedHeaderSource{
			path:         "arch/x86/kvm/kvm-asm-offsets.c",
			includeRoots: []string{"arch/x86/kvm"},
		}),
		nil,
		nil,
		nil,
	)
	if err != nil {
		return nil, err
	}

	return bindGeneratedHeaderProducerABI([]compactGeneratedHeaderFamilyFootprint{
		static,
		timeconst,
		compile,
		version,
		utsrelease,
		utsversion,
		cpufeatures,
		bounds,
		asmOffsets,
		rqOffsets,
		kvmOffsets,
		all,
	}, opts.CompileEnvironmentABI), nil
}

func bindGeneratedHeaderProducerABI(
	families []compactGeneratedHeaderFamilyFootprint,
	abi string,
) []compactGeneratedHeaderFamilyFootprint {
	for i := range families {
		families[i].fragment[generatedHeaderProducerABIKey] = abi
	}
	return families
}

func generatedHeaderFamilyFootprint(
	config *ResolvedConfig,
	scanner *configSourceScanner,
	name string,
	symbols []string,
	sources []compactGeneratedHeaderSource,
	digestOnlyPaths []string,
	dependencies []string,
	synthetic map[string]string,
) (compactGeneratedHeaderFamilyFootprint, error) {
	refs := map[string]bool{}
	dependencySet := map[string]bool{}
	for _, dependency := range dependencies {
		dependencySet[dependency] = true
	}
	for _, symbol := range symbols {
		refs[symbol] = true
	}
	sourceInputs := []CompactSourceInput{}
	for _, source := range sources {
		if _, ok := scanner.absForTreePath(source.path); !ok {
			continue
		}
		search, err := compactSourceActionIncludeSearch(
			scanner,
			source.path,
			source.flags,
			source.includeRoots,
		)
		if err != nil {
			return compactGeneratedHeaderFamilyFootprint{}, fmt.Errorf(
				"model generated-header family %s input %s include search: %w",
				name,
				source.path,
				err,
			)
		}
		closure, err := scanner.closureForSourceConfigInputsSearchProfile(
			source.path,
			search,
			config,
			isAssemblySourcePath(source.path),
			nil,
			source.profile,
		)
		if err != nil {
			return compactGeneratedHeaderFamilyFootprint{}, fmt.Errorf(
				"scan generated-header family %s input %s: %w",
				name,
				source.path,
				err,
			)
		}
		for _, ref := range closure.refs {
			refs[ref] = true
		}
		for _, include := range closure.generatedIncludes {
			dependency, precise := generatedHeaderFamilyNameForInclude(include)
			if dependency == "" {
				continue
			}
			if !precise || dependency == compactGeneratedHeaderFamilyAll {
				return compactGeneratedHeaderFamilyFootprint{}, fmt.Errorf(
					"generated-header family %s input %s has unclassified generated include %q",
					name,
					source.path,
					include,
				)
			}
			if generatedHeaderFamilyGenerationOrder(dependency) >=
				generatedHeaderFamilyGenerationOrder(name) {
				continue
			}
			dependencySet[dependency] = true
		}
		sourceInputs = appendUniqueSourceInputs(sourceInputs, closure.sourceInputs...)
	}
	for _, path := range digestOnlyPaths {
		if _, ok := scanner.absForTreePath(path); !ok {
			continue
		}
		input, err := scanner.inputForTreePath(path)
		if err != nil {
			return compactGeneratedHeaderFamilyFootprint{}, fmt.Errorf(
				"digest generated-header family %s input %s: %w",
				name,
				path,
				err,
			)
		}
		sourceInputs = appendUniqueSourceInputs(sourceInputs, input)
	}
	fragment := generatedHeaderConfigFragment(config, refs)
	for key, value := range synthetic {
		fragment[key] = value
	}
	dependencies = sortedStringSet(dependencySet)
	return compactGeneratedHeaderFamilyFootprint{
		name:         name,
		fragment:     fragment,
		dependencies: dependencies,
		sourceInputs: sourceInputs,
	}, nil
}

func generatedHeaderAllFootprint(
	config *ResolvedConfig,
	opts CompactMetadataOptions,
	scanner *configSourceScanner,
) (compactGeneratedHeaderFamilyFootprint, error) {
	refs := map[string]bool{}
	for _, symbol := range generatedHeaderConfigSymbols {
		refs[symbol] = true
	}
	for _, symbol := range KernelFlagsConfigSymbols(opts.Srcarch) {
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
	sourcePaths := generatedHeaderOffsetsSources(
		compactGeneratedHeaderSource{path: "kernel/bounds.c"},
		compactGeneratedHeaderSource{path: "kernel/sched/rq-offsets.c"},
	)
	digestOnlyPaths := []string{}
	switch opts.Srcarch {
	case "x86":
		sourcePaths = append(sourcePaths,
			compactGeneratedHeaderSource{path: "arch/x86/kernel/asm-offsets.c"},
			compactGeneratedHeaderSource{
				path:         "arch/x86/kvm/kvm-asm-offsets.c",
				includeRoots: []string{"arch/x86/kvm"},
			},
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
			compactGeneratedHeaderSource{path: "arch/arm64/kernel/asm-offsets.c"},
			compactGeneratedHeaderSource{
				path:         "arch/arm64/kvm/hyp/hyp-constants.c",
				includeRoots: []string{"arch/arm64/kvm/hyp/include"},
			},
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
	case "arm":
		for _, path := range []string{
			"arch/arm/vdso/note.c",
			"arch/arm/vdso/vgettimeofday.c",
			"arch/arm/vdso/vdso.lds.S",
			"lib/vdso/gettimeofday.c",
		} {
			sourcePaths = append(sourcePaths, compactGeneratedHeaderSource{
				path:    path,
				profile: sourceScanARMVDSO,
			})
		}
		digestOnlyPaths = append(digestOnlyPaths,
			"arch/arm/vdso/vdsomunge.c",
			"arch/arm/tools/syscall.tbl",
		)
	case "riscv":
		// The purgatory link omits its local string routines while either KASAN
		// implementation is enabled, so these symbols are part of the generated
		// purgatory.ro action identity even though no producer source references
		// them directly.
		refs["CONFIG_KASAN_GENERIC"] = true
		refs["CONFIG_KASAN_SW_TAGS"] = true
		for _, path := range []string{
			"arch/riscv/kernel/vdso/flush_icache.S",
			"arch/riscv/kernel/vdso/getcpu.S",
			"arch/riscv/kernel/vdso/getrandom.c",
			"arch/riscv/kernel/vdso/hwprobe.c",
			"arch/riscv/kernel/vdso/note.S",
			"arch/riscv/kernel/vdso/rt_sigreturn.S",
			"arch/riscv/kernel/vdso/sys_hwprobe.S",
			"arch/riscv/kernel/vdso/vdso.lds.S",
			"arch/riscv/kernel/vdso/vgetrandom-chacha.S",
			"arch/riscv/kernel/vdso/vgettimeofday.c",
			"lib/vdso/getrandom.c",
			"lib/vdso/gettimeofday.c",
		} {
			sourcePaths = append(sourcePaths, compactGeneratedHeaderSource{
				path:    path,
				profile: sourceScanRISCVVDSO,
			})
		}
		for _, path := range []string{
			"arch/riscv/kernel/compat_vdso/compat_vdso.lds.S",
			"arch/riscv/kernel/compat_vdso/flush_icache.S",
			"arch/riscv/kernel/compat_vdso/getcpu.S",
			"arch/riscv/kernel/compat_vdso/note.S",
			"arch/riscv/kernel/compat_vdso/rt_sigreturn.S",
		} {
			sourcePaths = append(sourcePaths, compactGeneratedHeaderSource{
				path:    path,
				profile: sourceScanRISCVCompatVDSO,
			})
		}
		sourcePaths = append(sourcePaths, compactPurgatorySourceActions("riscv")...)
	case "powerpc":
		for _, symbol := range []string{
			"CONFIG_PPC64",
			"CONFIG_VDSO32",
			"CONFIG_GENERIC_GETTIMEOFDAY",
			"CONFIG_VDSO_GETRANDOM",
		} {
			// These symbols select vDSO output images or producer objects in
			// arch/powerpc/kernel/vdso/Makefile rather than in source #if gates.
			refs[symbol] = true
		}
		shared := []string{
			"arch/powerpc/kernel/vdso/cacheflush.S",
			"arch/powerpc/kernel/vdso/datapage.S",
			"arch/powerpc/kernel/vdso/getcpu.S",
			"arch/powerpc/kernel/vdso/getrandom.S",
			"arch/powerpc/kernel/vdso/gettimeofday.S",
			"arch/powerpc/kernel/vdso/note.S",
			"arch/powerpc/kernel/vdso/vgetrandom-chacha.S",
			"arch/powerpc/kernel/vdso/vgetrandom.c",
			"arch/powerpc/kernel/vdso/vgettimeofday.c",
			"lib/vdso/getrandom.c",
			"lib/vdso/gettimeofday.c",
		}
		for _, profile := range []sourceScanProfile{sourceScanPPC64VDSO, sourceScanPPC32VDSO} {
			for _, path := range shared {
				sourcePaths = append(sourcePaths, compactGeneratedHeaderSource{
					path:    path,
					profile: profile,
				})
			}
		}
		// CONFIG_KEXEC_FILE is only available for PPC64, whose purgatory
		// image is linked from trampoline_64.o and embedded by the wrapper.
		sourcePaths = append(sourcePaths, compactPurgatorySourceActions("powerpc")...)
		for _, source := range []compactGeneratedHeaderSource{
			{path: "arch/powerpc/kernel/vdso/sigtramp64.S", profile: sourceScanPPC64VDSO},
			{path: "arch/powerpc/kernel/vdso/vdso64.lds.S", profile: sourceScanPPC64VDSO},
			{path: "arch/powerpc/kernel/vdso/sigtramp32.S", profile: sourceScanPPC32VDSO},
			{path: "arch/powerpc/kernel/vdso/vdso32.lds.S", profile: sourceScanPPC32VDSO},
			{path: "arch/powerpc/lib/crtsavres.S", profile: sourceScanPPC32VDSO},
		} {
			sourcePaths = append(sourcePaths, source)
		}
	}
	for _, source := range sourcePaths {
		if _, ok := scanner.absForTreePath(source.path); !ok {
			continue
		}
		search, err := compactSourceActionIncludeSearch(
			scanner,
			source.path,
			source.flags,
			source.includeRoots,
		)
		if err != nil {
			return compactGeneratedHeaderFamilyFootprint{}, fmt.Errorf(
				"model generated-header all-family input %s include search: %w",
				source.path,
				err,
			)
		}
		closure, err := scanner.closureForSourceConfigInputsSearchProfile(
			source.path,
			search,
			config,
			isAssemblySourcePath(source.path),
			nil,
			source.profile,
		)
		if err != nil {
			return compactGeneratedHeaderFamilyFootprint{}, fmt.Errorf(
				"scan generated-header all-family input %s: %w",
				source.path,
				err,
			)
		}
		for _, ref := range closure.refs {
			refs[ref] = true
		}
		for _, include := range closure.generatedIncludes {
			if _, precise := generatedHeaderFamilyNameForInclude(include); !precise {
				return compactGeneratedHeaderFamilyFootprint{}, fmt.Errorf(
					"generated-header all-family input %s has unclassified generated include %q",
					source.path,
					include,
				)
			}
		}
		sourceInputs = appendUniqueSourceInputs(sourceInputs, closure.sourceInputs...)
	}
	for _, path := range digestOnlyPaths {
		if _, ok := scanner.absForTreePath(path); !ok {
			continue
		}
		input, err := scanner.inputForTreePath(path)
		if err != nil {
			return compactGeneratedHeaderFamilyFootprint{}, fmt.Errorf(
				"digest generated-header all-family input %s: %w",
				path,
				err,
			)
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
				return compactGeneratedHeaderFamilyFootprint{}, fmt.Errorf(
					"scan generated-header all-family input directory %s: %w",
					scan.dir,
					err,
				)
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
			closure, err := scanner.closureForSourceConfigProfile(
				scan.source,
				nil,
				config,
				scan.profile,
			)
			if err != nil {
				return compactGeneratedHeaderFamilyFootprint{}, fmt.Errorf(
					"scan generated-header all-family forced input %s (%s): %w",
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

	fragment := generatedHeaderConfigFragment(config, refs)
	fragment["KBUILD_BUILD_TIMESTAMP"] = "1970-01-01T00:00:00Z"
	fragment["KBUILD_BUILD_VERSION"] = "1"
	fragment["KERNEL_VERSION"] = opts.KernelVersion
	return compactGeneratedHeaderFamilyFootprint{
		name:         compactGeneratedHeaderFamilyAll,
		fragment:     fragment,
		sourceInputs: sourceInputs,
	}, nil
}

func generatedHeaderConfigFragment(config *ResolvedConfig, refs map[string]bool) map[string]string {
	keys := sortedStringSet(refs)
	fragment := make(map[string]string, len(keys))
	for _, key := range keys {
		if config.ShouldWrite(key) {
			fragment[key] = config.Value(key)
		} else {
			fragment[key] = "n"
		}
	}
	return fragment
}

func generatedHeaderFamilyNameForInclude(path string) (string, bool) {
	path = strings.TrimSpace(strings.TrimPrefix(strings.ReplaceAll(path, "\\", "/"), "./"))
	switch path {
	case "generated/autoconf.h",
		"generated/integer-wrap.h",
		"generated/rustc_cfg":
		return "", true
	case "generated/timeconst.h":
		return compactGeneratedHeaderFamilyTimeconst, true
	case "generated/compile.h":
		return compactGeneratedHeaderFamilyCompile, true
	case "linux/version.h", "generated/uapi/linux/version.h":
		return compactGeneratedHeaderFamilyVersion, true
	case "linux/utsrelease.h", "generated/utsrelease.h":
		return compactGeneratedHeaderFamilyUTSRelease, true
	case "generated/utsversion.h":
		return compactGeneratedHeaderFamilyUTSVersion, true
	case "asm/cpufeaturemasks.h", "generated/asm/cpufeaturemasks.h":
		return compactGeneratedHeaderFamilyCPUFeatures, true
	case "generated/bounds.h":
		return compactGeneratedHeaderFamilyBounds, true
	case "generated/asm-offsets.h":
		return compactGeneratedHeaderFamilyASMOffsets, true
	case "generated/rq-offsets.h":
		return compactGeneratedHeaderFamilyRQOffsets, true
	case "kvm-asm-offsets.h", "generated/kvm-asm-offsets.h":
		return compactGeneratedHeaderFamilyKVMOffsets, true
	case "calls-eabi.S", "calls-oabi.S", "hyp_constants.h":
		return compactGeneratedHeaderFamilyAll, true
	}
	if strings.HasPrefix(path, "asm/") || strings.HasPrefix(path, "uapi/asm/") {
		return compactGeneratedHeaderFamilyStatic, true
	}
	if generatedHeaderInclude(path) {
		return compactGeneratedHeaderFamilyAll, false
	}
	return "", false
}

func generatedHeaderFamilyGenerationOrder(name string) int {
	switch name {
	case compactGeneratedHeaderFamilyStatic:
		return 0
	case compactGeneratedHeaderFamilyTimeconst:
		return 1
	case compactGeneratedHeaderFamilyCompile,
		compactGeneratedHeaderFamilyVersion,
		compactGeneratedHeaderFamilyUTSRelease,
		compactGeneratedHeaderFamilyUTSVersion:
		return 2
	case compactGeneratedHeaderFamilyCPUFeatures:
		return 3
	case compactGeneratedHeaderFamilyBounds:
		return 4
	case compactGeneratedHeaderFamilyASMOffsets:
		return 5
	case compactGeneratedHeaderFamilyRQOffsets:
		return 6
	case compactGeneratedHeaderFamilyKVMOffsets:
		return 7
	default:
		return -1
	}
}
