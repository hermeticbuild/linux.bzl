package kconfig

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Bound signature and argument width independently of expansion work, token
// output and nesting limits. Linux-style argument counters use 17 fixed
// parameters and pass up to 32 actual arguments through their helper macro.
const maxConfigDependencyMacroCallArity = 32

// A definition is immutable after construction. It retains normalized
// NAME(formals) replacement text, not an eagerly allocated token AST.
type configDependencyMacroCallDefinition struct {
	name, text, origin, identity string
	function                     bool
}

type configDependencyMacroCallBinding struct {
	state      configDependencyMacroDefinition
	definition *configDependencyMacroCallDefinition
	intrinsic  *configDependencyCompilerIntrinsicBinding
	counter    *configDependencyCompilerCounterBinding
}

// The resolver must describe one immutable effective state for this expansion.
// Missing entries are NOT implicitly undefined. The caller must prove Undefined
// separately from Unknown or Defined with an unmodeled replacement.
type configDependencyMacroCallResolver func(string) (configDependencyMacroCallBinding, string)

type configDependencyMacroCallMode struct {
	// Only a caller with proven compiler/language/options semantics may enable.
	dollarAsPunctuation bool
	intrinsic           func(CompilerIntrinsicCall) (token, identity, reason string)
	counter             compilerCounterCursor
	counterMissing      func(context string, required int)
	variadicComma       func(syntax string) (deleted bool, identity, reason string)
}

type configDependencyMacroCallRead struct {
	Name         string
	State        configDependencyMacroDefinition
	Origin       string
	DefinitionID string
}

type configDependencyMacroCallResult struct {
	Tokens          []string
	ConfigReads     []string
	DefinitionReads []configDependencyMacroCallRead
	IntrinsicReads  []configDependencyCompilerIntrinsicRead
	CounterReads    []compilerCounterRead
	VariadicReads   []compilerVariadicCommaRead
	Counter         compilerCounterCursor
	Work            int
}

type configDependencyMacroCallToken struct {
	text        string
	spelling    string // Original spelling, before digraph normalization.
	whitespace  bool
	spacing     configDependencyMacroCallSpacing
	identifier  bool
	unavailable bool // This occurrence was encountered while its macro was disabled.
}

// Padding around macro/argument expansion affects later stringification. A
// program maps the incoming state (none, nonwhite source, white source) to its
// outgoing state. Composition retains the first source until an expansion exit
// clears a nonwhite source. Zero is the identity program. No token AST or
// unbounded padding-event list is retained.
type configDependencyMacroCallSpacing uint8

const configDependencyMacroCallSpacingExit configDependencyMacroCallSpacing = 32

func configDependencyMacroCallSpacingEnter(white bool) configDependencyMacroCallSpacing {
	if white {
		return 38 // [white, nonwhite, white]
	}
	return 37 // [nonwhite, nonwhite, white]
}

func (p configDependencyMacroCallSpacing) apply(state uint8) uint8 {
	if p == 0 {
		return state
	}
	return uint8(p) >> (2 * state) & 3
}

func configDependencyMacroCallComposeSpacing(first, second configDependencyMacroCallSpacing) configDependencyMacroCallSpacing {
	var result configDependencyMacroCallSpacing
	for state := uint8(0); state < 3; state++ {
		result |= configDependencyMacroCallSpacing(second.apply(first.apply(state))) << (2 * state)
	}
	if result == 36 { // [none, nonwhite, white]
		return 0
	}
	return result
}

func (t configDependencyMacroCallToken) originalSpelling() string {
	if t.spelling != "" {
		return t.spelling
	}
	return t.text
}

func (m *configDependencyMacroCallMachine) stringify(tokens []configDependencyMacroCallToken) (configDependencyMacroCallToken, error) {
	var literal strings.Builder
	literal.WriteByte('"')
	for index, token := range tokens {
		m.work++
		if m.work > 16384 {
			return configDependencyMacroCallToken{}, fmt.Errorf("expansion work budget")
		}
		space := token.spacing.apply(0)
		if index != 0 && (space == 2 || space == 0 && token.whitespace) {
			literal.WriteByte(' ')
		}
		spelling := token.originalSpelling()
		quoted := strings.HasPrefix(spelling, "\"") || strings.HasPrefix(spelling, "'")
		for _, b := range []byte(spelling) {
			if literal.Len() > 65532 {
				return configDependencyMacroCallToken{}, fmt.Errorf("stringification byte budget")
			}
			if quoted && (b == '\\' || b == '"') {
				literal.WriteByte('\\')
			}
			literal.WriteByte(b)
		}
	}
	literal.WriteByte('"')
	m.generatedBytes += literal.Len()
	if m.generatedBytes > 1048576 {
		return configDependencyMacroCallToken{}, fmt.Errorf("generated macro byte budget")
	}
	return configDependencyMacroCallToken{text: literal.String()}, nil
}

type configDependencyMacroCallParsed struct {
	function       bool
	formals        []string
	variadic       string
	variadicSyntax string
	replacement    []configDependencyMacroCallToken
}

type configDependencyMacroCallMachine struct {
	resolve                               configDependencyMacroCallResolver
	mode                                  configDependencyMacroCallMode
	bindings                              map[string]configDependencyMacroCallBinding
	parsed                                map[*configDependencyMacroCallDefinition]configDependencyMacroCallParsed
	definitionReads                       map[string]configDependencyMacroCallRead
	intrinsicReads                        []configDependencyCompilerIntrinsicRead
	counterReads                          []compilerCounterRead
	variadicReads                         []compilerVariadicCommaRead
	counter                               compilerCounterCursor
	reads                                 map[string]bool
	work, definitionBytes, generatedBytes int
}

func configDependencyMacroCallDefinitionIdentity(text, origin string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%q:%q", origin, text))))
}

func configDependencyMacroCallDefinitionFromText(text, origin string) (*configDependencyMacroCallDefinition, string) {
	exactText := text
	text = strings.TrimSpace(text)
	if len(text) == 0 || len(exactText) > 65536 || strings.ContainsAny(text, "\r\n") ||
		origin == "" || len(origin) > 4096 || !configDependencyMacroCallIdentifierStart(text[0]) {
		return nil, "invalid macro call definition text or origin"
	}
	end := 1
	for end < len(text) && configDependencyMacroCallIdentifierPart(text[end]) {
		end++
	}
	d := &configDependencyMacroCallDefinition{
		name: strings.Clone(text[:end]), text: strings.Clone(exactText), origin: strings.Clone(origin),
		function: end < len(text) && text[end] == '(',
	}
	d.identity = configDependencyMacroCallDefinitionIdentity(d.text, d.origin)
	return d, ""
}

func (m *configDependencyMacroCallMachine) binding(name string) (configDependencyMacroCallBinding, error) {
	// Boundary checks can inspect punctuation; it is never a macro lookup.
	if name == "" || !configDependencyMacroCallIdentifierStart(name[0]) {
		return configDependencyMacroCallBinding{state: configDependencyMacroUndefined}, nil
	}
	for i := 1; i < len(name); i++ {
		if !configDependencyMacroCallIdentifierPart(name[i]) {
			return configDependencyMacroCallBinding{state: configDependencyMacroUndefined}, nil
		}
	}
	if binding, ok := m.bindings[name]; ok {
		return binding, nil
	}
	binding, reason := m.resolve(name)
	if reason != "" {
		return configDependencyMacroCallBinding{}, fmt.Errorf("macro %s: %s", name, reason)
	}
	read := configDependencyMacroCallRead{Name: name, State: binding.state}
	switch binding.state {
	case configDependencyMacroUndefined:
		if binding.definition != nil || binding.intrinsic != nil || binding.counter != nil {
			return configDependencyMacroCallBinding{}, fmt.Errorf("undefined macro %s has a replacement", name)
		}
	case configDependencyMacroDefined:
		if binding.counter != nil {
			if binding.definition != nil || binding.intrinsic != nil || !binding.counter.valid(name) {
				return configDependencyMacroCallBinding{}, fmt.Errorf("macro %s has an invalid compiler counter binding", name)
			}
			read.Origin, read.DefinitionID = binding.counter.origin, binding.counter.identity
			break
		}
		if binding.intrinsic != nil {
			if binding.definition != nil || !binding.intrinsic.valid(name) {
				return configDependencyMacroCallBinding{}, fmt.Errorf("macro %s has an invalid compiler intrinsic binding", name)
			}
			read.Origin, read.DefinitionID = binding.intrinsic.origin, binding.intrinsic.identity
			break
		}
		d := binding.definition
		if d == nil {
			return configDependencyMacroCallBinding{}, fmt.Errorf("macro %s has unmodeled or invalid replacement", name)
		}
		text := strings.TrimSpace(d.text)
		if d.name != name || d.origin == "" || d.identity == "" ||
			!strings.HasPrefix(text, name) ||
			len(text) > len(name) && configDependencyMacroCallIdentifierPart(text[len(name)]) ||
			d.function != (len(text) > len(name) && text[len(name)] == '(') ||
			d.identity != configDependencyMacroCallDefinitionIdentity(d.text, d.origin) {
			return configDependencyMacroCallBinding{}, fmt.Errorf("macro %s has unmodeled or invalid replacement", name)
		}
		m.definitionBytes += len(d.text)
		if m.definitionBytes > 1048576 {
			return configDependencyMacroCallBinding{}, fmt.Errorf("macro definition byte budget")
		}
		read.Origin, read.DefinitionID = d.origin, d.identity
	case configDependencyMacroUnknown:
		return configDependencyMacroCallBinding{}, fmt.Errorf("unknown macro binding %s", name)
	default:
		return configDependencyMacroCallBinding{}, fmt.Errorf("invalid macro binding state for %s", name)
	}
	if len(m.bindings) >= 4096 {
		return configDependencyMacroCallBinding{}, fmt.Errorf("macro binding budget")
	}
	m.bindings[name] = binding
	m.definitionReads[name] = read
	return binding, nil
}

// selected only observes the immutable macro's shape. Unsupported replacement
// syntax is parsed lazily only if this occurrence actually invokes the macro.
func (m *configDependencyMacroCallMachine) selected(name string) (configDependencyMacroCallParsed, bool, error) {
	binding, err := m.binding(name)
	if err != nil {
		return configDependencyMacroCallParsed{}, false, err
	}
	if binding.state == configDependencyMacroUndefined {
		return configDependencyMacroCallParsed{}, false, nil
	}
	if binding.intrinsic != nil {
		return configDependencyMacroCallParsed{function: true}, true, nil
	}
	if binding.counter != nil {
		return configDependencyMacroCallParsed{}, true, nil
	}
	return configDependencyMacroCallParsed{function: binding.definition.function}, true, nil
}

func (m *configDependencyMacroCallMachine) replacement(name string) (configDependencyMacroCallParsed, error) {
	binding, err := m.binding(name)
	if err != nil {
		return configDependencyMacroCallParsed{}, err
	}
	d := binding.definition
	if parsed, ok := m.parsed[d]; ok {
		return parsed, nil
	}
	parsedName, parsed, err := configDependencyMacroCallParse(d.text, m.mode)
	if err != nil {
		return configDependencyMacroCallParsed{}, fmt.Errorf("macro %s: %w", name, err)
	}
	if parsedName != name {
		return configDependencyMacroCallParsed{}, fmt.Errorf("macro replacement name mismatch")
	}
	m.parsed[d] = parsed
	return parsed, nil
}

func configDependencyMacroCallIdentifierStart(b byte) bool {
	return b == '_' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
}
func configDependencyMacroCallIdentifierPart(b byte) bool {
	return configDependencyMacroCallIdentifierStart(b) || b >= '0' && b <= '9'
}

// Deliberately strict C preprocessing-token subset. Unsupported lexical forms
// fail rather than being split into misleading identifier fragments.
func configDependencyMacroCallLex(text string, mode configDependencyMacroCallMode) ([]configDependencyMacroCallToken, error) {
	if len(text) > 65536 {
		return nil, fmt.Errorf("lexical byte budget")
	}
	if strings.Contains(text, "\\\n") || strings.Contains(text, "\\\r\n") {
		return nil, fmt.Errorf("unsupported unnormalized line splice")
	}
	if strings.Contains(text, "??") || strings.Contains(text, "<::") {
		return nil, fmt.Errorf("unsupported trigraph or dialect-dependent token")
	}
	var out []configDependencyMacroCallToken
	white := false
	for i := 0; i < len(text); {
		if strings.ContainsRune(" \t\r\n", rune(text[i])) {
			white = true
			i++
			continue
		}
		start := i
		if text[i] == '$' && !mode.dollarAsPunctuation {
			return nil, fmt.Errorf("dollar token requires a proven punctuation mode")
		}
		if text[i] == '\\' && i+1 < len(text) &&
			(strings.ContainsRune("uUN", rune(text[i+1])) || text[i+1] == '\n' || text[i+1] == '\r') {
			return nil, fmt.Errorf("unsupported UCN or unnormalized line splice")
		}
		if strings.HasPrefix(text[i:], "/*") {
			end := strings.Index(text[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("unterminated comment")
			}
			i += end + 4
			white = true
			continue
		}
		if strings.HasPrefix(text[i:], "//") {
			end := strings.IndexByte(text[i:], '\n')
			if end < 0 {
				break
			}
			i += end + 1
			white = true
			continue
		}
		if text[i] >= '0' && text[i] <= '9' || text[i] == '.' && i+1 < len(text) && text[i+1] >= '0' && text[i+1] <= '9' {
			i++
			for i < len(text) {
				if configDependencyMacroCallIdentifierPart(text[i]) || text[i] == '.' ||
					(text[i] == '+' || text[i] == '-') && strings.ContainsRune("eEpP", rune(text[i-1])) {
					i++
				} else {
					break
				}
			}
			if i < len(text) && text[i] == '\'' {
				return nil, fmt.Errorf("unsupported number separator")
			}
			out = append(out, configDependencyMacroCallToken{text: text[start:i]})
		} else if configDependencyMacroCallIdentifierStart(text[i]) {
			i++
			for i < len(text) && configDependencyMacroCallIdentifierPart(text[i]) {
				i++
			}
			if i < len(text) && (text[i] == '"' || text[i] == '\'') {
				return nil, fmt.Errorf("unsupported literal prefix")
			}
			out = append(out, configDependencyMacroCallToken{text: text[start:i], identifier: true})
		} else if text[i] == '"' || text[i] == '\'' {
			quote := text[i]
			i++
			for i < len(text) && text[i] != quote {
				if text[i] < 32 || text[i] >= 127 {
					return nil, fmt.Errorf("unsupported literal")
				}
				if text[i] == '\\' {
					i++
					if i == len(text) {
						return nil, fmt.Errorf("unterminated escape")
					}
				}
				i++
			}
			if i == len(text) {
				return nil, fmt.Errorf("unterminated literal")
			}
			i++
			out = append(out, configDependencyMacroCallToken{text: text[start:i]})
		} else {
			punct := ""
			for _, candidate := range []string{"%:%:", ">>=", "<<=", "...", "##", "->", "++", "--", "<<", ">>", "<=", ">=", "==", "!=", "&&", "||", "*=", "/=", "%=", "+=", "-=", "&=", "^=", "|=", "<:", ":>", "<%", "%>", "%:"} {
				if strings.HasPrefix(text[i:], candidate) {
					punct = candidate
					break
				}
			}
			if punct == "" {
				if text[i] < 33 || text[i] >= 127 {
					return nil, fmt.Errorf("unsupported byte at %d", i)
				}
				punct = text[i : i+1]
			}
			i += len(punct)
			// Normalize preprocessing digraph punctuators, not arbitrary text.
			switch punct {
			case "%:%:":
				punct = "##"
			case "%:":
				punct = "#"
			case "<:":
				punct = "["
			case ":>":
				punct = "]"
			case "<%":
				punct = "{"
			case "%>":
				punct = "}"
			}
			out = append(out, configDependencyMacroCallToken{text: punct})
		}
		out[len(out)-1].spelling = text[start:i]
		out[len(out)-1].whitespace = white
		white = false
		if len(out) > 4096 {
			return nil, fmt.Errorf("lexical token budget")
		}
	}
	return out, nil
}

func configDependencyMacroCallParse(text string, mode configDependencyMacroCallMode) (string, configDependencyMacroCallParsed, error) {
	text = strings.TrimSpace(text)
	if text == "" || !configDependencyMacroCallIdentifierStart(text[0]) {
		return "", configDependencyMacroCallParsed{}, fmt.Errorf("missing macro name")
	}
	end := 1
	for end < len(text) && configDependencyMacroCallIdentifierPart(text[end]) {
		end++
	}
	name, rest := text[:end], text[end:]
	d := configDependencyMacroCallParsed{}
	if strings.HasPrefix(rest, "(") {
		d.function = true
		close := strings.IndexByte(rest, ')')
		if close < 0 {
			return "", d, fmt.Errorf("unterminated formals")
		}
		formals := strings.TrimSpace(rest[1:close])
		if formals != "" {
			seen := map[string]bool{}
			parts := strings.Split(formals, ",")
			if len(parts) > maxConfigDependencyMacroCallArity {
				return "", d, fmt.Errorf("formal budget")
			}
			for n, formal := range parts {
				formal = strings.TrimSpace(formal)
				variadic := strings.HasSuffix(formal, "...")
				if variadic {
					if n != len(parts)-1 {
						return "", d, fmt.Errorf("unsupported variadic signature")
					}
					formal = strings.TrimSpace(strings.TrimSuffix(formal, "..."))
					d.variadicSyntax = "named"
					if formal == "" {
						d.variadicSyntax = "standard"
						formal = "__VA_ARGS__"
					}
				}
				tokens, err := configDependencyMacroCallLex(formal, mode)
				if err != nil || len(tokens) != 1 || !tokens[0].identifier {
					return "", d, fmt.Errorf("unsupported formals")
				}
				formal = tokens[0].text
				if seen[formal] || formal == "__VA_OPT__" || formal == "__VA_ARGS__" && !variadic {
					return "", d, fmt.Errorf("unsupported formals")
				}
				seen[formal] = true
				if variadic {
					d.variadic = formal
				} else {
					d.formals = append(d.formals, formal)
				}
			}
		}
		rest = rest[close+1:]
	}
	var err error
	d.replacement, err = configDependencyMacroCallLex(rest, mode)
	if err != nil {
		return "", d, err
	}
	for i, tok := range d.replacement {
		if tok.text == "#" {
			if !d.function || i+1 == len(d.replacement) ||
				!slices.Contains(d.formals, d.replacement[i+1].text) &&
					(d.variadic == "" || d.replacement[i+1].text != d.variadic) {
				return "", d, fmt.Errorf("unsupported stringification operand")
			}
		}
		if tok.text == "__VA_OPT__" || tok.text == "__VA_ARGS__" && d.variadic != "__VA_ARGS__" {
			return "", d, fmt.Errorf("unsupported variadic token")
		}
		if tok.text != "##" {
			continue
		}
		if i == 0 || i+1 == len(d.replacement) ||
			i >= 2 && d.replacement[i-2].text == "##" ||
			i+2 < len(d.replacement) && d.replacement[i+2].text == "##" {
			return "", d, fmt.Errorf("unsupported paste chain")
		}
		if d.variadic != "" && (d.replacement[i-1].text == d.variadic || d.replacement[i+1].text == d.variadic) &&
			!(d.replacement[i-1].text == "," && d.replacement[i+1].text == d.variadic) {
			return "", d, fmt.Errorf("unsupported variadic paste")
		}
	}
	return name, d, nil
}

func configDependencyMacroCallArguments(tokens []configDependencyMacroCallToken, open int) ([][]configDependencyMacroCallToken, int, error) {
	args, _, next, err := configDependencyMacroCallArgumentsWithSeparators(tokens, open)
	return args, next, err
}

func configDependencyMacroCallArgumentsWithSeparators(tokens []configDependencyMacroCallToken, open int) ([][]configDependencyMacroCallToken, []configDependencyMacroCallToken, int, error) {
	depth, start := 1, open+1
	var args [][]configDependencyMacroCallToken
	var separators []configDependencyMacroCallToken
	for i := start; i < len(tokens); i++ {
		switch tokens[i].text {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return append(args, slices.Clone(tokens[start:i])), separators, i + 1, nil
			}
		case ",":
			if depth == 1 {
				args = append(args, slices.Clone(tokens[start:i]))
				separators = append(separators, tokens[i])
				start = i + 1
			}
		}
		// The final argument is appended when the matching ')' is reached.
		// Seeing the limit-th separator would already require one more slot.
		if depth > 32 || len(args) >= maxConfigDependencyMacroCallArity {
			return nil, nil, 0, fmt.Errorf("argument budget")
		}
	}
	return nil, nil, 0, fmt.Errorf("unterminated call")
}

func (m *configDependencyMacroCallMachine) expand(tokens []configDependencyMacroCallToken, active map[string]bool) ([]configDependencyMacroCallToken, error) {
	out, _, err := m.expandWithSpacing(tokens, active)
	return out, err
}

// A context remains active while its last token is being considered. It is
// popped only when another token is requested, including raw call lookahead.
// This distinguishes temporary macro disablement from a token's permanent
// unavailability, and lets a replacement consume following invocation tokens.
type configDependencyMacroCallContext struct {
	tokens   []configDependencyMacroCallToken
	next     int
	disabled string
	tail     configDependencyMacroCallSpacing
}

type configDependencyMacroCallStream struct {
	contexts []configDependencyMacroCallContext
	active   map[string]bool
	pending  configDependencyMacroCallSpacing
}

func (s *configDependencyMacroCallStream) peek() (configDependencyMacroCallToken, bool) {
	for len(s.contexts) != 0 {
		context := &s.contexts[len(s.contexts)-1]
		if context.next < len(context.tokens) {
			return context.tokens[context.next], true
		}
		s.pending = configDependencyMacroCallComposeSpacing(s.pending, context.tail)
		if context.disabled != "" {
			delete(s.active, context.disabled)
		}
		s.contexts = s.contexts[:len(s.contexts)-1]
	}
	return configDependencyMacroCallToken{}, false
}

func (s *configDependencyMacroCallStream) take() (configDependencyMacroCallToken, bool) {
	token, ok := s.peek()
	if !ok {
		return token, false
	}
	s.contexts[len(s.contexts)-1].next++
	token.spacing = configDependencyMacroCallComposeSpacing(s.pending, token.spacing)
	s.pending = 0
	return token, true
}

// Collect only a raw, balanced invocation. Argument expansion gets its own
// bounded stream and cannot borrow tokens after the closing parenthesis.
func (s *configDependencyMacroCallStream) invocation() ([]configDependencyMacroCallToken, error) {
	opening, ok := s.take()
	if !ok || opening.text != "(" {
		return nil, fmt.Errorf("missing invocation parenthesis")
	}
	tokens := []configDependencyMacroCallToken{opening}
	depth := 1
	for depth != 0 {
		token, ok := s.take()
		if !ok {
			return nil, fmt.Errorf("unterminated call")
		}
		if token.identifier && s.active[token.text] {
			token.unavailable = true
		}
		switch token.text {
		case "(":
			depth++
		case ")":
			depth--
		}
		if depth > 32 || len(tokens) >= 4096 {
			return nil, fmt.Errorf("argument budget")
		}
		tokens = append(tokens, token)
	}
	return tokens, nil
}

func (m *configDependencyMacroCallMachine) expandWithSpacing(tokens []configDependencyMacroCallToken, active map[string]bool) ([]configDependencyMacroCallToken, configDependencyMacroCallSpacing, error) {
	if len(active) > 32 {
		return nil, 0, fmt.Errorf("expansion depth budget")
	}
	stream := configDependencyMacroCallStream{
		contexts: []configDependencyMacroCallContext{{tokens: tokens}},
		active:   maps.Clone(active),
	}
	if stream.active == nil {
		stream.active = map[string]bool{}
	}
	var out []configDependencyMacroCallToken
	for {
		tok, ok := stream.take()
		if !ok {
			break
		}
		m.work++
		if m.work > 16384 {
			return nil, 0, fmt.Errorf("expansion work budget")
		}
		if tok.text == "##" || tok.text == "#" {
			return nil, 0, fmt.Errorf("unsupported nonreplacement paste token")
		}
		if tok.identifier && strings.HasPrefix(tok.text, "CONFIG_") {
			m.reads[tok.text] = true
		}
		if tok.text == "_Pragma" || tok.text == "__pragma" {
			return nil, 0, fmt.Errorf("reachable preprocessing effect %s", tok.text)
		}
		d, found, err := m.selected(tok.text)
		if err != nil {
			return nil, 0, err
		}
		// Disablement is recorded on this token occurrence, not on the
		// immutable definition or namespace. It must precede the function
		// shape check: a disabled function name without '(' is permanently
		// unavailable too, even if argument substitution later supplies '('.
		if tok.identifier && found && (tok.unavailable || stream.active[tok.text]) {
			tok.unavailable = true
			if len(out) >= 4096 {
				return nil, 0, fmt.Errorf("output token budget")
			}
			out = append(out, tok)
			continue
		}
		invoked := true
		if found && d.function {
			following, ok := stream.peek()
			invoked = ok && following.text == "("
		}
		if !tok.identifier || !found || !invoked {
			if len(out) >= 4096 {
				return nil, 0, fmt.Errorf("output token budget")
			}
			out = append(out, tok)
			continue
		}
		if binding := m.bindings[tok.text]; binding.counter != nil {
			read, next, err := m.counter.expand(binding.counter.context)
			if err != nil {
				if m.mode.counterMissing != nil {
					if m.counter == (compilerCounterCursor{}) {
						m.mode.counterMissing(binding.counter.context, 1)
					} else if m.counter.validPosition() && m.counter.sequence.context == binding.counter.context && m.counter.next == len(m.counter.sequence.values) {
						m.mode.counterMissing(binding.counter.context, m.counter.next+1)
					}
				}
				return nil, 0, err
			}
			if len(out) >= 4096 || len(m.counterReads) >= maxCompilerCounterExpansions {
				return nil, 0, fmt.Errorf("counter expansion output budget")
			}
			expanded := configDependencyMacroCallToken{text: read.Token}
			expanded.spacing = configDependencyMacroCallComposeSpacing(tok.spacing, configDependencyMacroCallSpacingEnter(tok.whitespace))
			out = append(out, expanded)
			stream.pending = configDependencyMacroCallSpacingExit
			m.counter, m.counterReads = next, append(m.counterReads, read)
			continue
		}
		if binding := m.bindings[tok.text]; binding.intrinsic != nil {
			invocation, err := stream.invocation()
			if err != nil {
				return nil, 0, err
			}
			expanded, _, err := m.expandIntrinsic(binding.intrinsic, invocation, 0)
			if err != nil {
				return nil, 0, err
			}
			if len(out) == 4096 {
				return nil, 0, fmt.Errorf("output token budget")
			}
			expanded.spacing = configDependencyMacroCallComposeSpacing(tok.spacing, configDependencyMacroCallSpacingEnter(tok.whitespace))
			out = append(out, expanded)
			stream.pending = configDependencyMacroCallSpacingExit
			continue
		}
		d, err = m.replacement(tok.text)
		if err != nil {
			return nil, 0, err
		}
		raw, prescanned := map[string][]configDependencyMacroCallToken{}, map[string][]configDependencyMacroCallToken{}
		prescannedTail := map[string]configDependencyMacroCallSpacing{}
		omittedVariadic := false
		if d.function {
			invocation, err := stream.invocation()
			if err != nil {
				return nil, 0, err
			}
			args, separators, _, err := configDependencyMacroCallArgumentsWithSeparators(invocation, 0)
			if err != nil {
				return nil, 0, err
			}
			if len(d.formals) == 0 && len(args) == 1 && len(args[0]) == 0 {
				args = nil
			}
			if d.variadic == "" && len(args) != len(d.formals) || d.variadic != "" && len(args) < len(d.formals) {
				return nil, 0, fmt.Errorf("argument arity %s", tok.text)
			}
			for n, name := range d.formals {
				raw[name] = args[n]
			}
			if d.variadic != "" {
				omittedVariadic = len(args) == len(d.formals)
				var tail []configDependencyMacroCallToken
				for n, arg := range args[len(d.formals):] {
					if n != 0 {
						tail = append(tail, separators[len(d.formals)+n-1])
					}
					tail = append(tail, arg...)
				}
				raw[d.variadic] = tail
			}
		}
		var substituted []configDependencyMacroCallToken
		var substitutionTail configDependencyMacroCallSpacing
		appendSubstitution := func(values []configDependencyMacroCallToken, before, after configDependencyMacroCallSpacing) error {
			if len(values) > 4096-len(substituted) {
				return fmt.Errorf("substitution token budget")
			}
			prefix := configDependencyMacroCallComposeSpacing(substitutionTail, before)
			if len(values) == 0 {
				substitutionTail = configDependencyMacroCallComposeSpacing(prefix, after)
				return nil
			}
			first := values[0]
			first.spacing = configDependencyMacroCallComposeSpacing(prefix, first.spacing)
			substituted = append(substituted, first)
			substituted = append(substituted, values[1:]...)
			substitutionTail = after
			return nil
		}
		for n := 0; n < len(d.replacement); n++ {
			replacement := d.replacement[n]
			if replacement.text == "#" {
				literal, err := m.stringify(raw[d.replacement[n+1].text])
				if err != nil {
					return nil, 0, err
				}
				if err := appendSubstitution([]configDependencyMacroCallToken{literal}, configDependencyMacroCallSpacingEnter(replacement.whitespace), configDependencyMacroCallSpacingExit); err != nil {
					return nil, 0, err
				}
				n++
				continue
			}
			if replacement.text == "," && n+2 < len(d.replacement) &&
				d.replacement[n+1].text == "##" && d.variadic != "" && d.replacement[n+2].text == d.variadic {
				// With no fixed formal, F() cannot distinguish an omitted
				// tail from an explicitly empty one without dialect evidence.
				// A nonempty raw tail has neither ambiguity; expanded-empty
				// arguments still count as supplied, as they do below.
				if len(d.formals) == 0 && omittedVariadic {
					if m.mode.variadicComma == nil {
						return nil, 0, fmt.Errorf("dialect-dependent variadic comma deletion")
					}
					deleted, identity, reason := m.mode.variadicComma(d.variadicSyntax)
					if reason != "" || identity == "" {
						return nil, 0, fmt.Errorf("dialect-dependent variadic comma deletion: %s", reason)
					}
					if len(m.variadicReads) >= 4096 {
						return nil, 0, fmt.Errorf("variadic comma read budget")
					}
					m.variadicReads = append(m.variadicReads, compilerVariadicCommaRead{d.variadicSyntax, identity, deleted})
					omittedVariadic = deleted
				}
				// GNU comma deletion is not ordinary token concatenation.
				// A supplied tail, even explicitly empty, preserves the comma.
				// Its raw tokens rescan with this macro disabled, unlike
				// ordinary formal substitution's argument prescan.
				if !omittedVariadic {
					if slices.ContainsFunc(raw[d.variadic], func(t configDependencyMacroCallToken) bool { return t.text == "##" }) {
						return nil, 0, fmt.Errorf("unsupported argument paste token")
					}
					if err := appendSubstitution([]configDependencyMacroCallToken{replacement}, 0, 0); err != nil {
						return nil, 0, err
					}
					if err := appendSubstitution(raw[d.variadic], 0, configDependencyMacroCallSpacingExit); err != nil {
						return nil, 0, err
					}
				}
				n += 2
				continue
			}
			actual, formal := raw[replacement.text]
			if !formal {
				if err := appendSubstitution([]configDependencyMacroCallToken{replacement}, 0, 0); err != nil {
					return nil, 0, err
				}
				continue
			}
			if slices.ContainsFunc(actual, func(t configDependencyMacroCallToken) bool { return t.text == "##" }) {
				return nil, 0, fmt.Errorf("unsupported argument paste token")
			}
			pasted := n > 0 && d.replacement[n-1].text == "##" || n+1 < len(d.replacement) && d.replacement[n+1].text == "##"
			var actualTail configDependencyMacroCallSpacing
			if pasted {
				if len(actual) == 0 {
					return nil, 0, fmt.Errorf("unsupported placemarker")
				}
			} else {
				var exists bool
				actual, exists = prescanned[replacement.text]
				if !exists {
					actual, actualTail, err = m.expandWithSpacing(raw[replacement.text], stream.active)
					if err != nil {
						return nil, 0, err
					}
					prescanned[replacement.text] = actual
					prescannedTail[replacement.text] = actualTail
				}
				actualTail = prescannedTail[replacement.text]
			}
			if err := appendSubstitution(actual, configDependencyMacroCallSpacingEnter(replacement.whitespace), configDependencyMacroCallComposeSpacing(actualTail, configDependencyMacroCallSpacingExit)); err != nil {
				return nil, 0, err
			}
		}
		for n := 0; n < len(substituted); n++ {
			if substituted[n].text != "##" {
				continue
			}
			if n == 0 || n+1 == len(substituted) {
				return nil, 0, fmt.Errorf("missing paste operand")
			}
			// A valid paste creates a fresh occurrence: neither operand's
			// permanent suppression belongs to the newly lexed token. Rescan
			// still authenticates its new binding and applies the active macro
			// context, so forming an active macro does not re-enable it.
			joined, err := configDependencyMacroCallLex(substituted[n-1].originalSpelling()+substituted[n+1].originalSpelling(), m.mode)
			if err != nil || len(joined) != 1 || joined[0].text == "##" ||
				strings.Contains("\\$@`", joined[0].text) {
				return nil, 0, fmt.Errorf("invalid pasted token")
			}
			joined[0].spacing = substituted[n-1].spacing
			joined[0].whitespace = substituted[n-1].whitespace
			substituted = append(append(slices.Clone(substituted[:n-1]), joined[0]), substituted[n+2:]...)
			n--
		}
		if len(stream.active) >= 32 || len(stream.contexts) >= 33 {
			return nil, 0, fmt.Errorf("expansion depth budget")
		}
		prefix := configDependencyMacroCallComposeSpacing(tok.spacing, configDependencyMacroCallSpacingEnter(tok.whitespace))
		if len(substituted) != 0 {
			substituted[0].spacing = configDependencyMacroCallComposeSpacing(prefix, substituted[0].spacing)
		} else {
			substitutionTail = configDependencyMacroCallComposeSpacing(prefix, substitutionTail)
		}
		stream.active[tok.text] = true
		stream.contexts = append(stream.contexts, configDependencyMacroCallContext{
			tokens: substituted, disabled: tok.text,
			tail: configDependencyMacroCallComposeSpacing(substitutionTail, configDependencyMacroCallSpacingExit),
		})
	}
	return out, stream.pending, nil
}

// configDependencyMacroCallExpand consumes one complete normalized text span.
// The external ordered interpreter owns source/branch/include evidence and
// cross-span invocation boundaries. A failure returns no partial read set.
func configDependencyMacroCallExpand(text string, resolve configDependencyMacroCallResolver, mode configDependencyMacroCallMode) (configDependencyMacroCallResult, string) {
	if resolve == nil {
		return configDependencyMacroCallResult{}, "nil macro call resolver"
	}
	tokens, err := configDependencyMacroCallLex(text, mode)
	if err != nil {
		return configDependencyMacroCallResult{}, err.Error()
	}
	return configDependencyMacroCallExpandTokens(tokens, resolve, mode)
}

func configDependencyMacroCallExpandTokens(tokens []configDependencyMacroCallToken, resolve configDependencyMacroCallResolver, mode configDependencyMacroCallMode) (configDependencyMacroCallResult, string) {
	if resolve == nil {
		return configDependencyMacroCallResult{}, "nil macro call resolver"
	}
	m := configDependencyMacroCallMachine{
		counter: mode.counter,
		resolve: resolve, mode: mode, bindings: map[string]configDependencyMacroCallBinding{},
		parsed:          map[*configDependencyMacroCallDefinition]configDependencyMacroCallParsed{},
		definitionReads: map[string]configDependencyMacroCallRead{}, reads: map[string]bool{},
	}
	expanded, err := m.expand(tokens, map[string]bool{})
	if err != nil {
		return configDependencyMacroCallResult{}, err.Error()
	}
	r := configDependencyMacroCallResult{ConfigReads: slices.Sorted(maps.Keys(m.reads)), IntrinsicReads: m.intrinsicReads, Work: m.work}
	r.Counter, r.CounterReads = m.counter, m.counterReads
	r.VariadicReads = m.variadicReads
	for _, name := range slices.Sorted(maps.Keys(m.definitionReads)) {
		r.DefinitionReads = append(r.DefinitionReads, m.definitionReads[name])
	}
	for _, token := range expanded {
		r.Tokens = append(r.Tokens, token.text)
	}
	return r, ""
}
