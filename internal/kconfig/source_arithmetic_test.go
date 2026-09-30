package kconfig

import (
	"context"
	"strings"
	"testing"
)

func TestKbuildIntegerExpressionPreservesExprPrecedenceAndVersionCap(t *testing.T) {
	for _, test := range []struct {
		command, want string
	}{
		{`expr 5 \* 65536 + 10 \* 256 + 255`, "330495"},
		{`expr 5 \* 65536 + 10 \* 256 + 221`, "330461"},
		{`expr 8 / 2 \* 3 + 1`, "13"},
		{`expr 1 + 2 \* 3 - 4`, "3"},
		{`expr -3 % 2`, "-1"},
		{`expr 0`, "0"},
	} {
		got, err := EvaluateKbuildIntegerExpression(test.command)
		if err != nil || got != test.want {
			t.Errorf("source %q = %q, %v; want %q", test.command, got, err, test.want)
		}
		// Target action planning disables shell execution while expanding
		// recipes. The same grammar must remain usable through SourceShell.
		got, err = (&KbuildProbeScopes{}).kbuildSourceShell(context.Background(), test.command, "")
		if err != nil || got != test.want {
			t.Errorf("recipe source %q = %q, %v; want %q", test.command, got, err, test.want)
		}
	}
}

func TestKbuildIntegerExpressionRejectsShellAuthorityAndOverflow(t *testing.T) {
	for _, test := range []struct {
		command, want string
	}{
		{`expr 5 * 256`, "active shell syntax"},
		{`expr 5 \* 256; date`, "active shell syntax"},
		{`expr 5 \* $AMBIENT`, "active shell syntax"},
		{"expr 5 \\* `uname -r`", "unsupported Kbuild integer expression"},
		{`expr 9223372036854775807 + 1`, "overflows"},
		{`expr -9223372036854775808 \* -1`, "overflows"},
		{`expr -9223372036854775808 / -1`, "overflows"},
		{`expr 1 / 0`, "divides by zero"},
		{`expr 5 \* 2 | cat`, "active shell syntax"},
		{`expr 5 \* 2 + broken`, "outside signed 64-bit decimal"},
	} {
		_, err := EvaluateKbuildIntegerExpression(test.command)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("source %q error = %v, want %q", test.command, err, test.want)
		}
	}
}

func TestKbuildNumericShellPredicateProjectsStatusIntoSourceText(t *testing.T) {
	for _, test := range []struct {
		command, want string
	}{
		{`[ 2201080 -ge  1800000 ] && echo y`, "y"},
		{`[ 1700000 -ge  1800000 ] && echo y`, ""},
		{`[ -3 -lt -2 ] && echo yes`, "yes"},
		{`[ 5 -eq 5 ] && echo true`, "true"},
		{`[ 5 -ne 5 ] && echo true`, ""},
	} {
		got, err := EvaluateKbuildNumericShellPredicate(test.command)
		if err != nil || got != test.want {
			t.Errorf("source %q = %q, %v; want %q", test.command, got, err, test.want)
		}
		got, err = (&KbuildProbeScopes{}).kbuildSourceShell(context.Background(), test.command, "")
		if err != nil || got != test.want {
			t.Errorf("Kbuild source callback %q = %q, %v; want %q", test.command, got, err, test.want)
		}
	}
}

func TestKbuildNumericShellPredicateRejectsInvalidStatusAndShellAuthority(t *testing.T) {
	for _, command := range []string{
		`[ +1 -ge 0 ] && echo y`,
		`[ 9223372036854775808 -ge 0 ] && echo y`,
		`[ $AMBIENT -ge 0 ] && echo y`,
		`[ 2 -ge 1 ] && echo -n`,
		`[ 2 -ge 1 ] && echo y; touch owned`,
		`[ 2 -ge 1 ] && echo y >/dev/null`,
		`[ 2 -ge 1 ] || echo y`,
		`[ 2 -ge 1 ] && echo '$(touch owned)'`,
		`[ 2 -before 1 ] && echo y`,
	} {
		if _, err := EvaluateKbuildNumericShellPredicate(command); err == nil {
			t.Errorf("invalid source comparator %q accepted", command)
		}
		if _, err := (&KbuildProbeScopes{}).kbuildSourceShell(context.Background(), command, ""); err == nil {
			t.Errorf("invalid comparator %q admitted by source callback", command)
		}
	}
}
