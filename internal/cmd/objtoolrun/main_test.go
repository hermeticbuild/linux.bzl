package main

import (
	_ "embed"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

//go:embed main.go
var objtoolrunSource string

func TestObjtoolConfigSymbolsCoverSource(t *testing.T) {
	declared := map[string]bool{}
	for _, symbol := range kconfig.ObjtoolConfigSymbols() {
		declared[symbol] = true
	}
	for _, symbol := range regexp.MustCompile(`CONFIG_[A-Z0-9_]+`).FindAllString(objtoolrunSource, -1) {
		if !declared[symbol] {
			t.Errorf("objtoolrun reads %s, but ObjtoolConfigSymbols does not declare it", symbol)
		}
	}
}

func TestObjtoolArgsDisabled(t *testing.T) {
	for _, mode := range []string{"builtin", "builtin-always", "module", "module-member", "module-single", "vmlinux"} {
		t.Run(mode, func(t *testing.T) {
			args, run, err := objtoolArgs(map[string]string{}, mode, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			if run || args != nil {
				t.Fatalf("objtoolArgs() = (%q, %t), want (nil, false)", args, run)
			}
		})
	}
}

func TestObjtoolArgsBuiltin(t *testing.T) {
	config := commonArgsConfig()
	args, run, err := objtoolArgs(config, "builtin", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := commonArgs()
	if !run || !reflect.DeepEqual(args, want) {
		t.Fatalf("objtoolArgs() = (%q, %t), want (%q, true)", args, run, want)
	}

	for _, delayedBy := range []string{"CONFIG_LTO_CLANG", "CONFIG_X86_KERNEL_IBT"} {
		t.Run("delayed_by_"+delayedBy, func(t *testing.T) {
			config := commonArgsConfig()
			config[delayedBy] = "y"
			args, run, err := objtoolArgs(config, "builtin", false, nil)
			if err != nil {
				t.Fatal(err)
			}
			if run || args != nil {
				t.Fatalf("objtoolArgs() = (%q, %t), want (nil, false)", args, run)
			}
		})
	}

	t.Run("forced_delayed_translation_unit", func(t *testing.T) {
		config := commonArgsConfig()
		config["CONFIG_X86_KERNEL_IBT"] = "y"
		args, run, err := objtoolArgs(config, "builtin", true, []string{"--noabs"})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"--noabs"}
		if !run || !reflect.DeepEqual(args, want) {
			t.Fatalf("objtoolArgs() = (%q, %t), want (%q, true)", args, run, want)
		}
	})
}

func TestObjtoolArgsBuiltinAlways(t *testing.T) {
	for _, tc := range []struct {
		name    string
		delayed bool
	}{
		{name: "translation_unit"},
		{name: "delayed_link", delayed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := commonArgsConfig()
			if tc.delayed {
				config["CONFIG_X86_KERNEL_IBT"] = "y"
			}
			args, run, err := objtoolArgs(config, "builtin-always", false, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := commonArgs()
			if tc.delayed {
				want = append(commonArgsWithIBT(), "--link")
			}
			if !run || !reflect.DeepEqual(args, want) {
				t.Fatalf("objtoolArgs() = (%q, %t), want (%q, true)", args, run, want)
			}
		})
	}
}

func TestObjtoolArgsModule(t *testing.T) {
	for _, tc := range []struct {
		name    string
		delayed bool
	}{
		{name: "translation_unit"},
		{name: "delayed_link", delayed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := commonArgsConfig()
			if tc.delayed {
				config["CONFIG_X86_KERNEL_IBT"] = "y"
			}
			args, run, err := objtoolArgs(config, "module", false, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := commonArgs()
			if tc.delayed {
				want = commonArgsWithIBT()
				want = append(want, "--link")
			}
			want = append(want, "--module")
			if !run || !reflect.DeepEqual(args, want) {
				t.Fatalf("objtoolArgs() = (%q, %t), want (%q, true)", args, run, want)
			}
		})
	}
}

func TestObjtoolArgsModuleMember(t *testing.T) {
	t.Run("translation_unit", func(t *testing.T) {
		config := commonArgsConfig()
		args, run, err := objtoolArgs(config, "module-member", false, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := append(commonArgs(), "--module")
		if !run || !reflect.DeepEqual(args, want) {
			t.Fatalf("objtoolArgs() = (%q, %t), want (%q, true)", args, run, want)
		}
	})

	t.Run("delayed", func(t *testing.T) {
		config := commonArgsConfig()
		config["CONFIG_LTO_CLANG"] = "y"
		args, run, err := objtoolArgs(config, "module-member", false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if run || args != nil {
			t.Fatalf("objtoolArgs() = (%q, %t), want (nil, false)", args, run)
		}
	})

	t.Run("forced_delayed", func(t *testing.T) {
		config := commonArgsConfig()
		config["CONFIG_LTO_CLANG"] = "y"
		args, run, err := objtoolArgs(config, "module-member", true, []string{"--custom"})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"--custom", "--module"}
		if !run || !reflect.DeepEqual(args, want) {
			t.Fatalf("objtoolArgs() = (%q, %t), want (%q, true)", args, run, want)
		}
	})
}

func TestObjtoolArgsModuleSingleDelayed(t *testing.T) {
	config := commonArgsConfig()
	config["CONFIG_LTO_CLANG"] = "y"
	args, run, err := objtoolArgs(config, "module-single", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := append(commonArgs(), "--link", "--module")
	if !run || !reflect.DeepEqual(args, want) {
		t.Fatalf("objtoolArgs() = (%q, %t), want (%q, true)", args, run, want)
	}
}

func TestObjtoolArgsVmlinux(t *testing.T) {
	t.Run("not_needed", func(t *testing.T) {
		config := commonArgsConfig()
		delete(config, "CONFIG_FTRACE_MCOUNT_USE_OBJTOOL")
		args, run, err := objtoolArgs(config, "vmlinux", false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if run || args != nil {
			t.Fatalf("objtoolArgs() = (%q, %t), want (nil, false)", args, run)
		}
	})

	t.Run("mcount_without_delay_already_processed", func(t *testing.T) {
		config := commonArgsConfig()
		args, run, err := objtoolArgs(config, "vmlinux", false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if run || args != nil {
			t.Fatalf("objtoolArgs() = (%q, %t), want (nil, false)", args, run)
		}
	})

	t.Run("noinstr_without_delay", func(t *testing.T) {
		config := commonArgsConfig()
		config["CONFIG_NOINSTR_VALIDATION"] = "y"
		config["CONFIG_MITIGATION_UNRET_ENTRY"] = "y"
		args, run, err := objtoolArgs(config, "vmlinux", false, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"--Werror", "--noinstr", "--unret", "--link"}
		if !run || !reflect.DeepEqual(args, want) {
			t.Fatalf("objtoolArgs() = (%q, %t), want (%q, true)", args, run, want)
		}
	})

	t.Run("delayed_link", func(t *testing.T) {
		config := commonArgsConfig()
		config["CONFIG_X86_KERNEL_IBT"] = "y"
		config["CONFIG_NOINSTR_VALIDATION"] = "y"
		config["CONFIG_MITIGATION_SRSO"] = "y"
		args, run, err := objtoolArgs(config, "vmlinux", false, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := append(commonArgsWithIBT(), "--noinstr", "--unret", "--link")
		if !run || !reflect.DeepEqual(args, want) {
			t.Fatalf("objtoolArgs() = (%q, %t), want (%q, true)", args, run, want)
		}
	})
}

func TestConfigValueFlag(t *testing.T) {
	values := configValueFlag{}
	if err := values.Set("CONFIG_OBJTOOL=y"); err != nil {
		t.Fatal(err)
	}
	if got := values["CONFIG_OBJTOOL"]; got != "y" {
		t.Fatalf("CONFIG_OBJTOOL = %q, want y", got)
	}
	if err := values.Set("invalid"); err == nil {
		t.Fatal("Set() accepted a value without '='")
	}
}

func TestObjtoolArgsRejectsUnknownMode(t *testing.T) {
	_, _, err := objtoolArgs(map[string]string{"CONFIG_OBJTOOL": "y"}, "invalid", false, nil)
	if err == nil || !strings.Contains(err.Error(), `unsupported -mode "invalid"`) {
		t.Fatalf("objtoolArgs() error = %v, want unsupported mode", err)
	}
}

func commonArgsConfig() map[string]string {
	return map[string]string{
		"CONFIG_OBJTOOL":                        "y",
		"CONFIG_HAVE_JUMP_LABEL_HACK":           "y",
		"CONFIG_HAVE_NOINSTR_HACK":              "y",
		"CONFIG_MITIGATION_CALL_DEPTH_TRACKING": "y",
		"CONFIG_FINEIBT":                        "y",
		"CONFIG_UNWINDER_ORC":                   "y",
		"CONFIG_MITIGATION_RETPOLINE":           "y",
		"CONFIG_MITIGATION_RETHUNK":             "y",
		"CONFIG_MITIGATION_SLS":                 "y",
		"CONFIG_STACK_VALIDATION":               "y",
		"CONFIG_HAVE_STATIC_CALL_INLINE":        "y",
		"CONFIG_HAVE_UACCESS_VALIDATION":        "y",
		"CONFIG_KCOV":                           "y",
		"CONFIG_PREFIX_SYMBOLS":                 "y",
		"CONFIG_FUNCTION_PADDING_BYTES":         "16",
		"CONFIG_FTRACE_MCOUNT_USE_OBJTOOL":      "y",
		"CONFIG_HAVE_OBJTOOL_NOP_MCOUNT":        "y",
		"CONFIG_OBJTOOL_WERROR":                 "y",
	}
}

func commonArgs() []string {
	return []string{
		"--hacks=jump_label",
		"--hacks=noinstr",
		"--hacks=skylake",
		"--cfi",
		"--orc",
		"--retpoline",
		"--rethunk",
		"--sls",
		"--stackval",
		"--static-call",
		"--uaccess",
		"--no-unreachable",
		"--prefix=16",
		"--mcount",
		"--mnop",
		"--Werror",
	}
}

func commonArgsWithIBT() []string {
	args := commonArgs()
	return append(args[:3], append([]string{"--ibt"}, args[3:]...)...)
}
