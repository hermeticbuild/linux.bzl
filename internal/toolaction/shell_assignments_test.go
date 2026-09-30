package toolaction

import "testing"

func TestValidateStaticConfigAssignments(t *testing.T) {
	for _, contents := range []string{
		"",
		"#\n# Automatically generated file; DO NOT EDIT.\n#\n\n \t\n\t# $(inert comment)\nCONFIG_EMPTY=\nCONFIG_LOCALVERSION=\"\"\n",
		"CONFIG_BPF=y\nCONFIG_SMP=n\nCONFIG_INIT_ENV_ARG_LIMIT=32\nCONFIG_BOOT_MAGIC=0x1234\n",
		`CONFIG_DEFAULT_HOSTNAME="(none)"` + "\n" + `CONFIG_ESCAPED="quote\" and backslash\\ and literal %%"` + "\n",
	} {
		if err := ValidateStaticConfigAssignments(contents); err != nil {
			t.Fatalf("safe generated config %q: %v", contents, err)
		}
	}
	for _, test := range []struct {
		name     string
		contents string
	}{
		{"substitution in quoted value", `CONFIG_LOCALVERSION="$(touch side-effect)"` + "\n"},
		{"command substitution with backticks", "CONFIG_LOCALVERSION=\"`touch side-effect`\"\n"},
		{"escaped dollar", `CONFIG_LOCALVERSION="\$(touch side-effect)"` + "\n"},
		{"extra shell statement", "CONFIG_BPF=y; touch side-effect\n"},
		{"embedded newline", "CONFIG_NAME=\"one\ntwo\"\n"},
		{"wrong shell name", "CONFIG_NAME;touch=bad\n"},
		{"quoted suffix", "CONFIG_NAME=\"hello\"; touch side-effect\n"},
		{"unquoted shell expansion", "CONFIG_NAME=${PATH}\n"},
		{"unterminated source", "CONFIG_BPF=y"},
		{"command after comment", "# native heading\ntouch side-effect\n"},
		{"quoted comment is a command", "'# inert-looking command'\n"},
		{"non-shell whitespace before comment", "\u00a0# not a shell comment\n"},
		{"unquoted whitespace", "CONFIG_CC_VERSION_TEXT=Clang version 22\n"},
		{"carriage return in comment", "# heading\r\nCONFIG_BPF=y\n"},
		{"NUL in comment", "# heading\x00\nCONFIG_BPF=y\n"},
		{"empty symbol name", "CONFIG_=y\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateStaticConfigAssignments(test.contents); err == nil {
				t.Fatalf("unsafe generated config accepted: %q", test.contents)
			}
		})
	}
}
