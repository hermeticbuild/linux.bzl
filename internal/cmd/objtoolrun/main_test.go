package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestObjtoolArgsDisabled(t *testing.T) {
	for _, mode := range []string{"builtin", "module", "vmlinux"} {
		t.Run(mode, func(t *testing.T) {
			args, run, err := objtoolArgs(map[string]string{}, mode)
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
	args, run, err := objtoolArgs(config, "builtin")
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
			args, run, err := objtoolArgs(config, "builtin")
			if err != nil {
				t.Fatal(err)
			}
			if run || args != nil {
				t.Fatalf("objtoolArgs() = (%q, %t), want (nil, false)", args, run)
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
			args, run, err := objtoolArgs(config, "module")
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

func TestObjtoolArgsVmlinux(t *testing.T) {
	t.Run("not_needed", func(t *testing.T) {
		config := commonArgsConfig()
		delete(config, "CONFIG_FTRACE_MCOUNT_USE_OBJTOOL")
		args, run, err := objtoolArgs(config, "vmlinux")
		if err != nil {
			t.Fatal(err)
		}
		if run || args != nil {
			t.Fatalf("objtoolArgs() = (%q, %t), want (nil, false)", args, run)
		}
	})

	t.Run("mcount_without_delay", func(t *testing.T) {
		config := commonArgsConfig()
		args, run, err := objtoolArgs(config, "vmlinux")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"--mcount", "--mnop", "--Werror", "--link"}
		if !run || !reflect.DeepEqual(args, want) {
			t.Fatalf("objtoolArgs() = (%q, %t), want (%q, true)", args, run, want)
		}
	})

	t.Run("noinstr_without_delay", func(t *testing.T) {
		config := commonArgsConfig()
		delete(config, "CONFIG_FTRACE_MCOUNT_USE_OBJTOOL")
		config["CONFIG_NOINSTR_VALIDATION"] = "y"
		config["CONFIG_MITIGATION_UNRET_ENTRY"] = "y"
		args, run, err := objtoolArgs(config, "vmlinux")
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
		args, run, err := objtoolArgs(config, "vmlinux")
		if err != nil {
			t.Fatal(err)
		}
		want := append(commonArgsWithIBT(), "--noinstr", "--unret", "--link")
		if !run || !reflect.DeepEqual(args, want) {
			t.Fatalf("objtoolArgs() = (%q, %t), want (%q, true)", args, run, want)
		}
	})
}

func TestObjtoolArgsRejectsUnknownMode(t *testing.T) {
	_, _, err := objtoolArgs(map[string]string{"CONFIG_OBJTOOL": "y"}, "invalid")
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
