// Package toolaction defines the execution-time contract for invoking one
// configured Bazel tool action from another declared tool. The contract is
// role-based and deliberately independent of compiler family or executable
// filename.
package toolaction

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	executionRootProvenanceCapabilitySuffix  = "__LINUX_BZL_TOOLSET_PATH_CAPABILITY_V1__"
	executionRootProvenanceCapabilityKeySize = 32
	executionRootProvenanceCapabilityTagSize = sha256.Size * 2
)

const (
	KbuildArgumentsSentinel = "__LINUX_BZL_KBUILD_ARGS_V1__"
	ExecutionRootMarker     = "__LINUX_BZL_EXECROOT__"
	// ExecutionRootProvenanceMarker and ExecutionRootProvenanceTerminator frame
	// one evaluator-owned, scope-qualified canonical toolset path. Keeping the
	// complete path inside a self-delimiting token lets action runners replace
	// it without guessing where a flag or shell word ends. Both delimiter bytes
	// are reserved at every ordinary Kbuild ingress.
	ExecutionRootProvenanceMarker     = "\x07linux-bzl-toolset-path-v1:"
	ExecutionRootProvenanceTerminator = "\x08"
	DefaultDirectoryArgumentMarker    = "__LINUX_BZL_DEFAULT_DIRECTORY_ARGUMENT_V1__"
	EnvironmentName                   = "LINUX_BZL_TOOL_ACTION_CONTRACTS_V1"
	// RuntimeToolPathEnvironmentName is a runner-owned handoff to nested
	// source-script executors.  It names the private directory containing only
	// identity-bound tool-role aliases; source and configured action
	// environments may not set it.
	RuntimeToolPathEnvironmentName = "LINUX_BZL_RUNTIME_TOOL_PATH_V1"
)

const driverLinkContractSuffix = "-link"

const toolBindingScopeSeparator = "@"

// generatedProxyEnvironmentPrefix is reserved for shell state owned by
// InstallToolActionProxy. Contract environments are untrusted toolchain data;
// allowing them to reuse these names would let an export overwrite a derived
// conditional argument before the proxy invokes the selected tool.
const generatedProxyEnvironmentPrefix = "linux_bzl_"

type Contract struct {
	Arguments   []string          `json:"arguments"`
	Environment map[string]string `json:"environment"`
}

func Encode(contracts map[string]Contract) (string, error) {
	if err := Validate(contracts); err != nil {
		return "", err
	}
	data, err := json.Marshal(contracts)
	if err != nil {
		return "", fmt.Errorf("encode tool action contracts: %w", err)
	}
	return string(data), nil
}

func Decode(value string) (map[string]Contract, error) {
	if value == "" {
		return map[string]Contract{}, nil
	}
	contracts := map[string]Contract{}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&contracts); err != nil {
		return nil, fmt.Errorf("decode tool action contracts: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err == nil {
		return nil, fmt.Errorf("decode tool action contracts: trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode tool action contracts: %w", err)
	}
	if err := Validate(contracts); err != nil {
		return nil, err
	}
	return contracts, nil
}

func Validate(contracts map[string]Contract) error {
	for role, contract := range contracts {
		if !ValidBinding(role) {
			return fmt.Errorf("invalid tool action role %q", role)
		}
		if contract.Arguments == nil || contract.Environment == nil {
			return fmt.Errorf("tool action role %q must declare arguments and environment", role)
		}
		sentinels := 0
		seenSentinel := false
		defaultOptions := map[string]bool{}
		for _, argument := range contract.Arguments {
			if strings.ContainsRune(argument, 0) {
				return fmt.Errorf("tool action role %q has a NUL argument", role)
			}
			if argument == KbuildArgumentsSentinel {
				sentinels++
				seenSentinel = true
				continue
			}
			defaultArgument, encoded, err := parseDefaultDirectoryArgument(argument)
			if err != nil {
				return fmt.Errorf("tool action role %q: %w", role, err)
			}
			if encoded {
				if seenSentinel {
					return fmt.Errorf("tool action role %q has a default directory argument after its Kbuild argument marker", role)
				}
				if defaultOptions[defaultArgument.option] {
					return fmt.Errorf("tool action role %q repeats default option %q", role, defaultArgument.option)
				}
				defaultOptions[defaultArgument.option] = true
			}
		}
		if len(contract.Arguments) != 0 && sentinels != 1 {
			return fmt.Errorf("tool action role %q has %d Kbuild argument markers, want one", role, sentinels)
		}
		for option := range defaultOptions {
			if invocationHasOption(contract.Arguments, option) {
				return fmt.Errorf("tool action role %q configures both a default and an explicit %q option", role, option)
			}
		}
		for name, value := range contract.Environment {
			if !validEnvironmentName(name) ||
				strings.HasPrefix(name, generatedProxyEnvironmentPrefix) ||
				strings.ContainsRune(value, 0) {
				return fmt.Errorf("tool action role %q has invalid environment entry %q", role, name)
			}
		}
	}
	return nil
}

type defaultDirectoryArgument struct {
	option string
	anchor string
}

func parseDefaultDirectoryArgument(argument string) (defaultDirectoryArgument, bool, error) {
	encoded, found := strings.CutPrefix(argument, DefaultDirectoryArgumentMarker)
	if !found {
		return defaultDirectoryArgument{}, false, nil
	}
	option, anchor, ok := strings.Cut(encoded, "=")
	if !ok || !strings.HasPrefix(option, "--") || len(option) == 2 ||
		strings.ContainsAny(option, "=/\\\x00\r\n\t ") || anchor == "" || strings.ContainsRune(anchor, 0) {
		return defaultDirectoryArgument{}, true, fmt.Errorf("invalid default directory argument %q", argument)
	}
	return defaultDirectoryArgument{option: option, anchor: anchor}, true, nil
}

func invocationHasOption(arguments []string, option string) bool {
	for _, argument := range arguments {
		if argument == option || strings.HasPrefix(argument, option+"=") {
			return true
		}
	}
	return false
}

func resolveDefaultDirectoryArgument(value defaultDirectoryArgument) (string, error) {
	if !filepath.IsAbs(value.anchor) {
		return "", fmt.Errorf("default option %q has non-absolute artifact anchor %q", value.option, value.anchor)
	}
	return value.option + "=" + filepath.Dir(filepath.Clean(value.anchor)), nil
}

// SpliceArguments applies a configured action envelope to source-selected
// arguments. Default-directory arguments remain typed through their anchor
// File and are materialized only when the source invocation omitted the same
// option. This models compiler-driver defaults without overriding source-owned
// policy such as Linux's explicit --sysroot=/dev/null.
func SpliceArguments(action, invocation []string) ([]string, error) {
	if len(action) == 0 {
		return append([]string(nil), invocation...), nil
	}
	foundSentinel := false
	out := make([]string, 0, len(action)+len(invocation))
	seenOptions := map[string]bool{}
	configuredArguments := make([]string, 0, len(action))
	for _, argument := range action {
		if argument == KbuildArgumentsSentinel {
			continue
		}
		if _, encoded, err := parseDefaultDirectoryArgument(argument); err != nil {
			return nil, err
		} else if !encoded {
			configuredArguments = append(configuredArguments, argument)
		}
	}
	for _, argument := range action {
		if argument == KbuildArgumentsSentinel {
			if foundSentinel {
				return nil, errors.New("configured action repeats Kbuild argument sentinel")
			}
			foundSentinel = true
			out = append(out, invocation...)
			continue
		}
		defaultArgument, encoded, err := parseDefaultDirectoryArgument(argument)
		if err != nil {
			return nil, err
		}
		if !encoded {
			out = append(out, argument)
			continue
		}
		if foundSentinel {
			return nil, errors.New("configured default directory argument follows Kbuild argument sentinel")
		}
		if seenOptions[defaultArgument.option] {
			return nil, fmt.Errorf("configured action repeats default option %q", defaultArgument.option)
		}
		seenOptions[defaultArgument.option] = true
		if invocationHasOption(configuredArguments, defaultArgument.option) {
			return nil, fmt.Errorf("configured action provides both a default and an explicit %q option", defaultArgument.option)
		}
		resolved, err := resolveDefaultDirectoryArgument(defaultArgument)
		if err != nil {
			return nil, err
		}
		if invocationHasOption(invocation, defaultArgument.option) {
			continue
		}
		out = append(out, resolved)
	}
	if !foundSentinel {
		return nil, errors.New("configured action omits Kbuild argument sentinel")
	}
	return out, nil
}

// ExpandExecutionRootValue resolves the reserved action-contract marker
// against the original Bazel execution root. Configured tool actions can then
// retain absolute access to selected toolchain inputs after a runner changes
// its working directory.
func ExpandExecutionRootValue(value, executionRoot string) (string, error) {
	return expandExecutionRootMarker(value, executionRoot, ExecutionRootMarker, "configured toolchain action")
}

// EncodeExecutionRootProvenancePath returns the private runtime wire
// representation of one canonical artifact in the selected host or target
// toolset closure. Planning code which exposes a token to source-owned Kbuild
// must use ExecutionRootProvenanceCapabilityCodec instead: this deterministic
// representation is intentionally not an authentication mechanism.
func EncodeExecutionRootProvenancePath(scope, canonical string) (string, error) {
	if scope != "host" && scope != "target" {
		return "", fmt.Errorf("probed toolset path has invalid scope %q", scope)
	}
	if err := ValidateCanonicalArtifactPath(canonical); err != nil {
		return "", fmt.Errorf("probed toolset path %q: %w", canonical, err)
	}
	if strings.ContainsAny(canonical, "\x00\x07\x08\r\n") {
		return "", fmt.Errorf("probed toolset path %q contains a reserved byte", canonical)
	}
	return ExecutionRootProvenanceMarker + scope + ":" + canonical + ExecutionRootProvenanceTerminator, nil
}

// ExecutionRootProvenanceCapabilityCodec authenticates transient toolset-path
// tokens while source-owned Kbuild text transformations are evaluated. Each
// workload must use its own randomly keyed codec. Authenticated tokens remain
// syntactically valid provenance tokens, but must be normalized before they
// enter a stable action recipe or a runtime runner.
type ExecutionRootProvenanceCapabilityCodec struct {
	key [executionRootProvenanceCapabilityKeySize]byte
}

// NewExecutionRootProvenanceCapabilityCodec creates a codec with a fresh
// workload-local key.
func NewExecutionRootProvenanceCapabilityCodec() (*ExecutionRootProvenanceCapabilityCodec, error) {
	key := make([]byte, executionRootProvenanceCapabilityKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("create toolset-path capability key: %w", err)
	}
	return newExecutionRootProvenanceCapabilityCodec(key)
}

// newExecutionRootProvenanceCapabilityCodec constructs a codec from a fixed
// key for focused deterministic tests. Production callers must use the random
// constructor above.
func newExecutionRootProvenanceCapabilityCodec(key []byte) (*ExecutionRootProvenanceCapabilityCodec, error) {
	if len(key) != executionRootProvenanceCapabilityKeySize {
		return nil, fmt.Errorf("toolset-path capability key has size %d, want %d", len(key), executionRootProvenanceCapabilityKeySize)
	}
	codec := &ExecutionRootProvenanceCapabilityCodec{}
	copy(codec.key[:], key)
	return codec, nil
}

// EncodePath returns an authenticated, transient planning representation of
// one canonical toolset path. The deterministic core comes first so Make's
// lexical operations order distinct capabilities exactly like their runtime
// paths; a fixed printable suffix carries the tag binding scope and path.
func (c *ExecutionRootProvenanceCapabilityCodec) EncodePath(scope, canonical string) (string, error) {
	if c == nil {
		return "", errors.New("toolset-path capability codec is nil")
	}
	core, err := EncodeExecutionRootProvenancePath(scope, canonical)
	if err != nil {
		return "", err
	}
	tag := c.capabilityTag(scope, canonical)
	return core + executionRootProvenanceCapabilitySuffix + hex.EncodeToString(tag), nil
}

// NormalizeValue verifies every provenance token in value was issued by this
// codec, then replaces it with the deterministic runtime representation. Raw
// deterministic tokens and tokens altered by Kbuild fail closed. Ordinary
// surrounding bytes and multiple intact tokens are preserved.
func (c *ExecutionRootProvenanceCapabilityCodec) NormalizeValue(value string) (string, error) {
	if c == nil {
		return "", errors.New("toolset-path capability codec is nil")
	}
	return rewriteExecutionRootProvenanceCapabilityValue(value, true, true, func(scope, canonical string, tag []byte) error {
		want := c.capabilityTag(scope, canonical)
		if !hmac.Equal(tag, want) {
			return errors.New("authenticated planning capability tag does not match its scope and path")
		}
		return nil
	})
}

// CanonicalizeExecutionRootProvenanceCapabilityIdentity removes ephemeral
// capability tags from stable hash and equality projections. It verifies only
// the capability's structure, not its authenticity; callers must still apply
// the workload's keyed NormalizeValue before granting path authority. Ordinary
// deterministic runtime tokens are preserved unchanged. Identity text is not
// granted path authority, so this projection deliberately does not require a
// runtime token boundary: it strips only a complete structural capability tag
// and preserves every trailing byte. For example, capability+".a" projects to
// core+".a", which remains distinct from core.
func CanonicalizeExecutionRootProvenanceCapabilityIdentity(value string) (string, error) {
	return rewriteExecutionRootProvenanceCapabilityValue(value, false, false, nil)
}

func rewriteExecutionRootProvenanceCapabilityValue(
	value string,
	requireCapability bool,
	requireRuntimeBoundary bool,
	verify func(scope, canonical string, tag []byte) error,
) (string, error) {
	if !strings.ContainsAny(value, "\x07\x08") && !strings.Contains(value, executionRootProvenanceCapabilitySuffix) {
		return value, nil
	}
	var out strings.Builder
	for cursor := 0; cursor < len(value); {
		relativeDelimiter := strings.IndexAny(value[cursor:], "\x07\x08")
		relativeSuffix := strings.Index(value[cursor:], executionRootProvenanceCapabilitySuffix)
		if relativeSuffix >= 0 && (relativeDelimiter < 0 || relativeSuffix < relativeDelimiter) {
			return "", fmt.Errorf("stray toolset-path capability suffix at byte %d", cursor+relativeSuffix)
		}
		if relativeDelimiter < 0 {
			out.WriteString(value[cursor:])
			break
		}
		start := cursor + relativeDelimiter
		out.WriteString(value[cursor:start])
		scope, canonical, next, err := parseExecutionRootProvenanceToken(value, start)
		if err != nil {
			return "", err
		}
		core := value[start:next]
		capability, err := parseExecutionRootProvenanceCapability(value, start, next, requireRuntimeBoundary)
		if err != nil {
			return "", err
		}
		if !capability.present {
			if requireCapability {
				return "", fmt.Errorf("probed toolset path token at byte %d omits its authenticated capability suffix", start)
			}
			out.WriteString(core)
			cursor = next
			continue
		}
		if verify != nil {
			if err := verify(scope, canonical, capability.tag); err != nil {
				return "", fmt.Errorf("verify %s toolset path %q: %w", scope, canonical, err)
			}
		}
		out.WriteString(core)
		cursor = capability.end
	}
	return out.String(), nil
}

func (c *ExecutionRootProvenanceCapabilityCodec) capabilityTag(scope, canonical string) []byte {
	mac := hmac.New(sha256.New, c.key[:])
	_, _ = mac.Write([]byte("linux-bzl-toolset-path-capability-v1\x00"))
	_, _ = mac.Write([]byte(scope))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(canonical))
	return mac.Sum(nil)
}

func isLowerHex(value string) bool {
	for i := 0; i < len(value); i++ {
		if !isLowerHexByte(value[i]) {
			return false
		}
	}
	return true
}

func isLowerHexByte(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'f')
}

type executionRootProvenanceCapability struct {
	present bool
	tag     []byte
	end     int
}

// parseExecutionRootProvenanceCapability classifies the bytes immediately
// after one deterministic token core. For authority-bearing values, the
// printable authenticated suffix is the only non-boundary continuation of a
// core and every complete token variant must then end or be followed by ASCII
// whitespace. Punctuation is not a universal runtime boundary: quotes
// concatenate shell words, braces expand, and most punctuation can extend a
// single argv element. Stable identity projection disables that boundary check
// because it preserves, rather than grants authority to, all trailing bytes.
func parseExecutionRootProvenanceCapability(
	value string,
	start, coreEnd int,
	requireRuntimeBoundary bool,
) (executionRootProvenanceCapability, error) {
	if !strings.HasPrefix(value[coreEnd:], executionRootProvenanceCapabilitySuffix) {
		if requireRuntimeBoundary && !isExecutionRootProvenanceBoundary(value, coreEnd) {
			return executionRootProvenanceCapability{}, fmt.Errorf("probed toolset path token at byte %d has a suffix outside its provenance envelope", start)
		}
		return executionRootProvenanceCapability{end: coreEnd}, nil
	}

	tagStart := coreEnd + len(executionRootProvenanceCapabilitySuffix)
	tagEnd := tagStart + executionRootProvenanceCapabilityTagSize
	if tagEnd > len(value) {
		return executionRootProvenanceCapability{}, fmt.Errorf("toolset-path capability suffix at byte %d has a truncated tag", coreEnd)
	}
	tagHex := value[tagStart:tagEnd]
	if !isLowerHex(tagHex) {
		return executionRootProvenanceCapability{}, fmt.Errorf("toolset-path capability suffix at byte %d has a malformed tag", coreEnd)
	}
	tag, err := hex.DecodeString(tagHex)
	if err != nil {
		return executionRootProvenanceCapability{}, fmt.Errorf("toolset-path capability suffix at byte %d has a malformed tag", coreEnd)
	}
	if requireRuntimeBoundary && !isExecutionRootProvenanceBoundary(value, tagEnd) {
		return executionRootProvenanceCapability{}, fmt.Errorf("probed toolset path capability at byte %d has a suffix outside its provenance envelope", start)
	}
	return executionRootProvenanceCapability{present: true, tag: tag, end: tagEnd}, nil
}

func isExecutionRootProvenanceBoundary(value string, end int) bool {
	if end == len(value) {
		return true
	}
	if end < 0 || end > len(value) {
		return false
	}
	switch value[end] {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	default:
		return false
	}
}

// DecodeExecutionRootProvenancePath parses one value consisting exclusively
// of a single private toolset-path token.
func DecodeExecutionRootProvenancePath(value string) (scope, canonical string, err error) {
	const decoded = "__LINUX_BZL_DECODED_TOOLSET_PATH_V1__"
	seen := false
	rewritten, err := RewriteExecutionRootProvenanceValue(value, func(gotScope, gotCanonical string) (string, error) {
		if seen {
			return "", errors.New("contains more than one toolset-path token")
		}
		seen = true
		scope, canonical = gotScope, gotCanonical
		return decoded, nil
	})
	if err != nil {
		return "", "", err
	}
	if !seen || rewritten != decoded {
		return "", "", errors.New("value is not exactly one toolset-path token")
	}
	return scope, canonical, nil
}

// RewriteExecutionRootProvenanceValue validates and replaces every complete
// private toolset-path token in value. Ordinary bytes are copied verbatim;
// stray, truncated, nested, or suffixed tokens fail closed. A structurally
// valid capability spelling is accepted at this syntax layer, but its printable
// suffix is preserved verbatim so this unkeyed operation cannot silently turn
// it into runtime authority. Callers must use keyed NormalizeValue verification
// before granting the underlying deterministic token that authority. This is
// the shared parser hook used by runners which resolve canonical paths through
// their declared toolset closure.
func RewriteExecutionRootProvenanceValue(
	value string,
	resolve func(scope, canonical string) (string, error),
) (string, error) {
	if !strings.ContainsAny(value, "\x07\x08") {
		return value, nil
	}
	if resolve == nil {
		return "", errors.New("probed toolset path resolver is nil")
	}
	var out strings.Builder
	for cursor := 0; cursor < len(value); {
		relative := strings.IndexAny(value[cursor:], "\x07\x08")
		if relative < 0 {
			out.WriteString(value[cursor:])
			break
		}
		start := cursor + relative
		out.WriteString(value[cursor:start])
		scope, canonical, next, err := parseExecutionRootProvenanceToken(value, start)
		if err != nil {
			return "", err
		}
		capability, err := parseExecutionRootProvenanceCapability(value, start, next, true)
		if err != nil {
			return "", err
		}
		replacement, err := resolve(scope, canonical)
		if err != nil {
			return "", fmt.Errorf("resolve %s toolset path %q: %w", scope, canonical, err)
		}
		if replacement == "" || strings.ContainsAny(replacement, "\x00\x07\x08") {
			return "", fmt.Errorf("resolved %s toolset path %q is empty or contains a reserved byte", scope, canonical)
		}
		out.WriteString(replacement)
		out.WriteString(value[next:capability.end])
		cursor = capability.end
	}
	return out.String(), nil
}

func parseExecutionRootProvenanceToken(value string, start int) (scope, canonical string, next int, err error) {
	if start < 0 || start >= len(value) || !strings.HasPrefix(value[start:], ExecutionRootProvenanceMarker) {
		return "", "", 0, fmt.Errorf("probed toolset path value contains a stray provenance delimiter at byte %d", start)
	}
	payloadStart := start + len(ExecutionRootProvenanceMarker)
	relativeEnd := strings.Index(value[payloadStart:], ExecutionRootProvenanceTerminator)
	if relativeEnd < 0 {
		return "", "", 0, fmt.Errorf("probed toolset path token at byte %d is unterminated", start)
	}
	end := payloadStart + relativeEnd
	payload := value[payloadStart:end]
	if strings.ContainsRune(payload, '\x07') {
		return "", "", 0, fmt.Errorf("probed toolset path token at byte %d contains a nested provenance delimiter", start)
	}
	scope, canonical, ok := strings.Cut(payload, ":")
	if !ok {
		return "", "", 0, fmt.Errorf("probed toolset path token at byte %d omits its scope", start)
	}
	encoded, err := EncodeExecutionRootProvenancePath(scope, canonical)
	if err != nil {
		return "", "", 0, fmt.Errorf("probed toolset path token at byte %d: %w", start, err)
	}
	next = end + len(ExecutionRootProvenanceTerminator)
	if encoded != value[start:next] {
		return "", "", 0, fmt.Errorf("probed toolset path token at byte %d is not canonical", start)
	}
	return scope, canonical, next, nil
}

// ValidateExecutionRootProvenanceValue verifies the private tokens in value
// without resolving them or changing their bytes.
func ValidateExecutionRootProvenanceValue(value string) error {
	_, err := RewriteExecutionRootProvenanceValue(value, func(_, _ string) (string, error) {
		return "validated-toolset-path", nil
	})
	return err
}

func expandExecutionRootMarker(value, executionRoot, marker, description string) (string, error) {
	prefix := marker + "/"
	if !strings.Contains(value, marker) {
		return value, nil
	}
	if !filepath.IsAbs(executionRoot) {
		return "", fmt.Errorf("execution root %q is not absolute", executionRoot)
	}
	root := filepath.ToSlash(filepath.Clean(executionRoot))
	if root != "/" {
		root += "/"
	}
	expanded := strings.ReplaceAll(value, prefix, root)
	if strings.Contains(expanded, marker) {
		return "", fmt.Errorf("%s value contains malformed execution-root marker", description)
	}
	return expanded, nil
}

// LinkContractRole returns the semantic link-action contract paired with a C
// or C++ compiler-driver role.  The executable role does not change: cc-link
// and cxx-link are contracts for invoking the same source-selected cc/cxx
// executable in link mode.
func LinkContractRole(role string) (string, bool) {
	scope, base, scoped, valid := SplitBinding(role)
	if !valid || (base != "cc" && base != "cxx") {
		return "", false
	}
	base += driverLinkContractSuffix
	if scoped {
		return ScopedBinding(scope, base)
	}
	return base, true
}

// BaseContractRole returns the compiler-driver role paired with a semantic
// link contract.
func BaseContractRole(role string) (string, bool) {
	scope, base, scoped, valid := SplitBinding(role)
	base, ok := strings.CutSuffix(base, driverLinkContractSuffix)
	if !valid || !ok || (base != "cc" && base != "cxx") {
		return "", false
	}
	if scoped {
		return ScopedBinding(scope, base)
	}
	return base, true
}

// InvocationContractRole selects the configured action contract for one
// concrete argv.  It recognizes only the compiler driver's stable,
// family-independent mode interface.  A link invocation must have an explicit
// output and no compile/preprocess/dependency-only mode, which prevents
// queries such as `cc --version` from accidentally acquiring link flags.
type compilerInvocationProperties struct {
	hasOutput   bool
	compileOnly bool
	textOnly    bool
}

func inspectCompilerInvocation(arguments []string) compilerInvocationProperties {
	properties := compilerInvocationProperties{}
	expectOutput := false
	for _, argument := range arguments {
		if expectOutput {
			if argument != "" {
				properties.hasOutput = true
			}
			expectOutput = false
			continue
		}
		switch argument {
		case "-c":
			properties.compileOnly = true
		case "-S", "-E", "-M", "-MM", "-fsyntax-only":
			properties.textOnly = true
		case "-o":
			expectOutput = true
		default:
			if strings.HasPrefix(argument, "-o") && len(argument) > len("-o") {
				properties.hasOutput = true
			}
		}
	}
	return properties
}

func InvocationContractRole(role string, arguments []string) string {
	linkRole, compilerDriver := LinkContractRole(role)
	if !compilerDriver {
		return role
	}
	properties := inspectCompilerInvocation(arguments)
	if properties.compileOnly || properties.textOnly {
		return role
	}
	if properties.hasOutput {
		return linkRole
	}
	return role
}

// CompilerInvocationProducesBinaryOutput reports whether a configured C or
// C++ driver invocation's primary -o output is an object or linked image.
// Stable driver modes are interpreted independently of compiler family. Side
// dependency modes such as -MD/-MMD do not change a -c primary output, while
// preprocessing, dependency-only, assembly-text, and syntax-only modes are
// deliberately retained as potential generated source data.
func CompilerInvocationProducesBinaryOutput(role string, arguments []string) bool {
	_, base, _, valid := SplitBinding(role)
	if !valid || (base != "cc" && base != "cxx") {
		return false
	}
	properties := inspectCompilerInvocation(arguments)
	if properties.textOnly {
		return false
	}
	return properties.compileOnly || properties.hasOutput
}

// InstallToolActionProxy materializes one private executable which applies the
// configured action contract for role before invoking executable. C/C++ driver
// roles may supply their semantic link companion; the proxy selects it from
// the caller's stable driver-mode argv without identifying a compiler family.
// multicall must provide a POSIX sh applet and is written into the proxy's
// shebang so execution never searches an ambient shell.
func InstallToolActionProxy(
	directory, multicall, role, executable string,
	contract Contract,
	linkContract *Contract,
) (string, error) {
	if !ValidBinding(role) {
		return "", fmt.Errorf("invalid tool action proxy role %q", role)
	}
	if _, companion := BaseContractRole(role); companion {
		return "", fmt.Errorf("tool action proxy role %q is a semantic contract, not an executable role", role)
	}
	if directory == "" || multicall == "" || executable == "" {
		return "", fmt.Errorf("tool action proxy %q requires a directory, multicall runtime, and executable", role)
	}
	if !filepath.IsAbs(multicall) || strings.ContainsAny(multicall, "\x00\r\n\t ") || strings.ContainsRune(executable, 0) {
		return "", fmt.Errorf("tool action proxy %q has an invalid runtime or executable path", role)
	}
	contracts := map[string]Contract{role: contract}
	linkRole := ""
	if linkContract != nil {
		var ok bool
		linkRole, ok = LinkContractRole(role)
		if !ok {
			return "", fmt.Errorf("tool action proxy role %q cannot have a driver-link companion contract", role)
		}
		contracts[linkRole] = *linkContract
	}
	if err := Validate(contracts); err != nil {
		return "", err
	}

	destination := filepath.Join(directory, role)
	if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var script strings.Builder
	script.WriteString("#!")
	script.WriteString(multicall)
	script.WriteString(" sh\n")
	if linkContract != nil {
		script.WriteString("linux_bzl_link=\nlinux_bzl_expect_output=\n")
		script.WriteString("for linux_bzl_arg do\n")
		script.WriteString("  if [ -n \"$linux_bzl_expect_output\" ]; then\n")
		script.WriteString("    if [ -n \"$linux_bzl_arg\" ]; then linux_bzl_link=1; fi\n")
		script.WriteString("    linux_bzl_expect_output=\n    continue\n  fi\n")
		script.WriteString("  case \"$linux_bzl_arg\" in\n")
		script.WriteString("    -c|-S|-E|-M|-MM|-fsyntax-only) linux_bzl_link=; break ;;\n")
		script.WriteString("    -o) linux_bzl_expect_output=1 ;;\n")
		script.WriteString("    -o?*) linux_bzl_link=1 ;;\n")
		script.WriteString("  esac\ndone\n")
		script.WriteString("if [ \"$linux_bzl_link\" = 1 ]; then\n")
		if err := writeToolActionContractInvocation(&script, executable, *linkContract, "  "); err != nil {
			return "", err
		}
		script.WriteString("fi\n")
	}
	if err := writeToolActionContractInvocation(&script, executable, contract, ""); err != nil {
		return "", err
	}
	if err := os.WriteFile(destination, []byte(script.String()), 0o700); err != nil {
		return "", err
	}
	return destination, nil
}

func writeToolActionContractInvocation(script *strings.Builder, executable string, contract Contract, indent string) error {
	defaultVariables := map[string]string{}
	for index, argument := range contract.Arguments {
		value, encoded, err := parseDefaultDirectoryArgument(argument)
		if err != nil {
			return err
		}
		if !encoded {
			continue
		}
		resolved, err := resolveDefaultDirectoryArgument(value)
		if err != nil {
			return err
		}
		variable := fmt.Sprintf("linux_bzl_default_directory_argument_%d", index)
		defaultVariables[argument] = variable
		script.WriteString(indent)
		script.WriteString(variable)
		script.WriteString("=")
		script.WriteString(shellQuote(resolved))
		script.WriteByte('\n')
		script.WriteString(indent)
		script.WriteString("for linux_bzl_arg do\n")
		script.WriteString(indent)
		script.WriteString("  case \"$linux_bzl_arg\" in\n")
		script.WriteString(indent)
		script.WriteString("    ")
		script.WriteString(shellQuote(value.option))
		script.WriteString("|")
		script.WriteString(shellQuote(value.option + "="))
		script.WriteString("*) ")
		script.WriteString(variable)
		script.WriteString("= ;;\n")
		script.WriteString(indent)
		script.WriteString("  esac\n")
		script.WriteString(indent)
		script.WriteString("done\n")
	}
	for _, environmentName := range sortedEnvironmentNames(contract.Environment) {
		script.WriteString(indent)
		script.WriteString("export ")
		script.WriteString(environmentName)
		script.WriteString("=")
		script.WriteString(shellQuote(contract.Environment[environmentName]))
		script.WriteByte('\n')
	}
	script.WriteString(indent)
	script.WriteString("exec ")
	script.WriteString(shellQuote(executable))
	if len(contract.Arguments) == 0 {
		script.WriteString(" \"$@\"")
	} else {
		for _, argument := range contract.Arguments {
			if argument == KbuildArgumentsSentinel {
				script.WriteString(" \"$@\"")
			} else if variable := defaultVariables[argument]; variable != "" {
				script.WriteString(" ${")
				script.WriteString(variable)
				script.WriteString(":+\"$")
				script.WriteString(variable)
				script.WriteString("\"}")
			} else {
				script.WriteByte(' ')
				script.WriteString(shellQuote(argument))
			}
		}
	}
	script.WriteByte('\n')
	return nil
}

func sortedEnvironmentNames(environment map[string]string) []string {
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// PrepareRuntimeToolDirectory creates a private PATH component containing one
// symlink per configured tool role.  Callers own privateRoot and must invoke
// the returned cleanup function.  No ambient PATH entries are inspected.
func PrepareRuntimeToolDirectory(privateRoot string, tools map[string]string) (string, func(), error) {
	noop := func() {}
	if len(tools) == 0 {
		return "", noop, nil
	}
	if privateRoot == "" {
		return "", noop, fmt.Errorf("runtime tools require a private working-directory root")
	}
	privateRoot, err := filepath.Abs(privateRoot)
	if err != nil {
		return "", noop, fmt.Errorf("resolve private working-directory root: %w", err)
	}

	roles := make([]string, 0, len(tools))
	absoluteTools := make(map[string]string, len(tools))
	for role, tool := range tools {
		if !ValidBinding(role) {
			return "", noop, fmt.Errorf("invalid runtime tool role %q", role)
		}
		if tool == "" {
			return "", noop, fmt.Errorf("runtime tool %q is empty", role)
		}
		absolute, err := filepath.Abs(tool)
		if err != nil {
			return "", noop, fmt.Errorf("resolve runtime tool %q: %w", role, err)
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return "", noop, fmt.Errorf("inspect runtime tool %q: %w", role, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return "", noop, fmt.Errorf("runtime tool %q is not an executable regular file", role)
		}
		roles = append(roles, role)
		absoluteTools[role] = absolute
	}
	sort.Strings(roles)

	runtimeRoot := filepath.Join(privateRoot, ".linux-bzl-tool-runtime")
	if err := os.Mkdir(runtimeRoot, 0o700); err != nil {
		return "", noop, fmt.Errorf("create private runtime-tool root: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(runtimeRoot) }
	toolDirectory := filepath.Join(runtimeRoot, "bin")
	if err := os.Mkdir(toolDirectory, 0o700); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("create private runtime-tool bin: %w", err)
	}
	for _, role := range roles {
		if err := os.Symlink(absoluteTools[role], filepath.Join(toolDirectory, role)); err != nil {
			cleanup()
			return "", noop, fmt.Errorf("install runtime tool %q: %w", role, err)
		}
	}
	return toolDirectory, cleanup, nil
}

func Roles(contracts map[string]Contract) []string {
	roles := make([]string, 0, len(contracts))
	for role := range contracts {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles
}

// ValidRole reports whether value is a canonical action-role identifier shared
// by toolset manifests, source-time Make tokens, and execution contracts.
func ValidRole(value string) bool {
	if value == "" || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("_.+-", character) {
			continue
		}
		return false
	}
	return true
}

// ScopedBinding returns the collision-free execution binding for role from an
// explicitly selected host or target toolset. '@' is deliberately excluded
// from ValidRole, so a scoped binding cannot alias any source-owned role.
func ScopedBinding(scope, role string) (string, bool) {
	if (scope != "host" && scope != "target") || !ValidRole(role) {
		return "", false
	}
	return scope + toolBindingScopeSeparator + role, true
}

// SplitBinding validates a configured tool binding and returns its optional
// scope plus underlying source-owned role.
func SplitBinding(value string) (scope, role string, scoped, valid bool) {
	if ValidRole(value) {
		return "", value, false, true
	}
	scope, role, found := strings.Cut(value, toolBindingScopeSeparator)
	if !found || strings.Contains(role, toolBindingScopeSeparator) ||
		(scope != "host" && scope != "target") || !ValidRole(role) {
		return "", "", false, false
	}
	return scope, role, true, true
}

func ValidBinding(value string) bool {
	_, _, _, valid := SplitBinding(value)
	return valid
}

func validEnvironmentName(value string) bool {
	if value == "" || !asciiLetterOrUnderscore(value[0]) {
		return false
	}
	for index := 1; index < len(value); index++ {
		if !asciiLetterOrUnderscore(value[index]) && (value[index] < '0' || value[index] > '9') {
			return false
		}
	}
	return true
}

func asciiLetterOrUnderscore(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}
