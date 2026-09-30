package kconfig

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// This witness is distinct from both a textual #define and a definedness cell.
// It is installed only in the measured initial namespace, before actual argv
// and source effects. Ordinary replacement lifecycle operations revoke it.
type configDependencyCompilerIntrinsicBinding struct {
	operator, context, origin, identity string
}

type configDependencyCompilerIntrinsicRead struct {
	Call                       CompilerIntrinsicCall
	BindingID, AnswerID, Token string
}

func configDependencyCompilerIntrinsicBindingID(operator, context, origin string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("compiler-intrinsic-binding-v1:%q:%q:%q", operator, context, origin))))
}

func (b *configDependencyCompilerIntrinsicBinding) valid(name string) bool {
	return b != nil && b.operator == name && isCompilerIntrinsicOperator(name) && b.context != "" && b.origin != "" &&
		b.identity == configDependencyCompilerIntrinsicBindingID(b.operator, b.context, b.origin)
}

func (s *configDependencyMacroState) installInitialCompilerIntrinsics(
	definitions map[string]bool, context configDependencyCompilerPredefineRequestKey, predefines, definednessID string,
) {
	if !s.validSnapshotLineage() || s.macroReplacements == nil || context == "" || definednessID == "" {
		return
	}
	// This is an admitted operator grammar, not a capability answer. Positive
	// definedness and every reached call result must be measured independently.
	for operator, defined := range definitions {
		if !defined || !isCompilerIntrinsicOperator(operator) || s.definition(operator) != configDependencyMacroDefined {
			continue
		}
		if _, present := s.macroReplacements[operator]; present {
			continue // A textual or already installed binding is never promoted.
		}
		binding := &configDependencyCompilerIntrinsicBinding{
			operator: operator,
			context:  fmt.Sprintf("%x", sha256.Sum256([]byte(context))),
			origin:   fmt.Sprintf("compiler-initial:%x:%s", sha256.Sum256([]byte(predefines)), definednessID),
		}
		binding.identity = configDependencyCompilerIntrinsicBindingID(binding.operator, binding.context, binding.origin)
		s.macroReplacements[operator] = configDependencyMacroReplacement{intrinsic: binding}
		s.snapshotRevision++
	}
}

func (m *configDependencyMacroCallMachine) expandIntrinsic(
	binding *configDependencyCompilerIntrinsicBinding, tokens []configDependencyMacroCallToken, opening int,
) (configDependencyMacroCallToken, int, error) {
	args, next, err := configDependencyMacroCallArguments(tokens, opening)
	if err != nil {
		return configDependencyMacroCallToken{}, 0, err
	}
	if len(args) != 1 || len(args[0]) != 1 || !args[0][0].identifier {
		return configDependencyMacroCallToken{}, 0, fmt.Errorf("compiler intrinsic requires one unexpanded identifier operand")
	}
	call := CompilerIntrinsicCall{Operator: binding.operator, Operand: args[0][0].text}
	if err := ValidateCompilerIntrinsicCall(call); err != nil {
		return configDependencyMacroCallToken{}, 0, err
	}
	// Do not assume an operator's argument prescan matches a function macro.
	// Admit only a terminal identifier requiring no current expansion. Ordinary
	// surrounding wrappers have already recorded any prescan and CONFIG reads.
	operand, err := m.binding(call.Operand)
	if err != nil {
		return configDependencyMacroCallToken{}, 0, err
	}
	if operand.state != configDependencyMacroUndefined {
		return configDependencyMacroCallToken{}, 0, fmt.Errorf("compiler intrinsic operand %s needs unproved expansion", call.Operand)
	}
	if strings.HasPrefix(call.Operand, "CONFIG_") {
		m.reads[call.Operand] = true
	}
	if m.mode.intrinsic == nil {
		return configDependencyMacroCallToken{}, 0, fmt.Errorf("compiler intrinsic has no measured call oracle")
	}
	token, identity, reason := m.mode.intrinsic(call)
	if reason != "" {
		return configDependencyMacroCallToken{}, 0, fmt.Errorf("compiler intrinsic %s(%s): %s", call.Operator, call.Operand, reason)
	}
	validated, err := parseCompilerIntrinsicIntegerResult(token)
	if err != nil || validated != token || identity == "" {
		return configDependencyMacroCallToken{}, 0, fmt.Errorf("compiler intrinsic has an invalid measured integer or witness")
	}
	if len(m.intrinsicReads) >= 4096 {
		return configDependencyMacroCallToken{}, 0, fmt.Errorf("compiler intrinsic read budget")
	}
	m.intrinsicReads = append(m.intrinsicReads, configDependencyCompilerIntrinsicRead{
		Call: call, BindingID: binding.identity, AnswerID: identity, Token: token,
	})
	return configDependencyMacroCallToken{text: token}, next, nil
}
