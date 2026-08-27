package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

const targetProbeScope = "target"

var (
	kconfigBlockPattern = regexp.MustCompile(`^(?:menu)?config[[:space:]]+([A-Z0-9_]+)$`)
	identityPattern     = regexp.MustCompile(`^def_bool[[:space:]]+\$\(success,test[[:space:]]+"\$\(([a-z][a-z0-9_-]*)-name\)"[[:space:]]*=[[:space:]]*([^[:space:])]+)\)$`)
	versionPattern      = regexp.MustCompile(`^default[[:space:]]+\$\(([a-z][a-z0-9_-]*)-version\)(?:[[:space:]]+if[[:space:]]+([A-Z0-9_]+))?$`)
	literalPattern      = regexp.MustCompile(`^default[[:space:]]+([0-9]+)$`)
	versionTextPattern  = regexp.MustCompile(`^default[[:space:]]+"\$\(([A-Z][A-Z0-9_]*)\)"$`)
	infoScriptPattern   = regexp.MustCompile(`^([a-z][a-z0-9_-]*)-info[[:space:]]*:=[[:space:]]*\$\(shell,\$\(srctree\)/([^[:space:])]+)`)
)

type toolClassOracle struct {
	identities          map[string]string
	conditionalVersions map[string]string
	directVersion       string
	script              string
}

type compilerKconfigOracle struct {
	classes           map[string]*toolClassOracle
	fallbacks         map[string]string
	compilerClass     string
	versionTextSymbol string
}

type probeRequest struct {
	Sources []string        `json:"sources"`
	Steps   []probeStepSpec `json:"steps"`
}

type probeStepSpec struct {
	Name      string   `json:"name"`
	Tool      string   `json:"tool"`
	Arguments []string `json:"arguments"`
}

type probeResult struct {
	RequestID string            `json:"request_id"`
	Scope     string            `json:"scope"`
	Kind      string            `json:"kind"`
	Text      string            `json:"text"`
	Steps     []probeStepResult `json:"steps"`
}

type probeStepResult struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
}

type probeEvidence struct {
	requests map[string]probeRequest
	results  []probeResult
}

type toolMeasurement struct {
	name    string
	version string
}

func TestResolvedKernelConfigUsesSelectedCompiler(t *testing.T) {
	expectedCompiler := os.Getenv("EXPECTED_COMPILER")
	if expectedCompiler == "" {
		t.Fatal("EXPECTED_COMPILER is empty")
	}

	configPath := resolveRunfile(t, "KERNEL_CONFIG")
	initKconfigPath := resolveRunfile(t, "KERNEL_INIT_KCONFIG")
	kconfigIncludePath := resolveRunfile(t, "KERNEL_KCONFIG_INCLUDE")
	probeRoots := resolveRunfileList(t, "KERNEL_PROBE_ARTIFACTS")

	config, err := readKernelConfig(configPath)
	if err != nil {
		t.Fatalf("read resolved kernel config: %v", err)
	}
	oracle, err := readCompilerKconfigOracle(initKconfigPath, kconfigIncludePath)
	if err != nil {
		t.Fatalf("derive compiler oracle from Linux sources: %v", err)
	}
	evidence, err := readProbeEvidence(probeRoots)
	if err != nil {
		t.Fatalf("read mapped compiler probes: %v", err)
	}

	measurements := make(map[string]toolMeasurement, len(oracle.classes))
	for _, className := range sortedKeys(oracle.classes) {
		class := oracle.classes[className]
		measurement, err := evidence.measurement(class.script)
		if err != nil {
			t.Fatalf("measure %s identity from %s: %v", className, class.script, err)
		}
		measurements[className] = measurement
		t.Run(className, func(t *testing.T) {
			validateToolClass(t, config, oracle, className, class, measurement)
		})
	}

	compiler := measurements[oracle.compilerClass]
	if !strings.EqualFold(compiler.name, expectedCompiler) {
		t.Fatalf("selected compiler probe reported %q, want matrix toolchain %q", compiler.name, expectedCompiler)
	}
	versionText, err := evidence.compilerVersionText(oracle.compilerClass)
	if err != nil {
		t.Fatalf("measure compiler version text: %v", err)
	}
	if got := config[oracle.versionTextSymbol]; got != versionText {
		t.Fatalf("%s = %q, want selected compiler --version first line %q", oracle.versionTextSymbol, got, versionText)
	}
}

func validateToolClass(
	t *testing.T,
	config map[string]string,
	oracle *compilerKconfigOracle,
	className string,
	class *toolClassOracle,
	measurement toolMeasurement,
) {
	t.Helper()
	selectedIdentity, ok := class.identities[measurement.name]
	if !ok {
		t.Fatalf("%s probe reported identity %q, which Linux Kconfig does not declare (known: %s)", className, measurement.name, strings.Join(sortedKeys(class.identities), ", "))
	}
	for _, reportedName := range sortedKeys(class.identities) {
		symbol := class.identities[reportedName]
		want := "n"
		if symbol == selectedIdentity {
			want = "y"
		}
		if got := config[symbol]; got != want {
			t.Errorf("%s = %q, want %q from %s probe identity %q", symbol, got, want, className, measurement.name)
		}
	}

	if class.directVersion != "" {
		if got := config[class.directVersion]; got != measurement.version {
			t.Errorf("%s = %q, want %s probe version %q", class.directVersion, got, className, measurement.version)
		}
	}
	for identity, versionSymbol := range class.conditionalVersions {
		want := measurement.version
		if identity != selectedIdentity {
			var ok bool
			want, ok = oracle.fallbacks[versionSymbol]
			if !ok {
				t.Errorf("Linux Kconfig gives unselected %s no literal fallback", versionSymbol)
				continue
			}
		}
		if got := config[versionSymbol]; got != want {
			t.Errorf("%s = %q, want %q from source-derived %s version rule", versionSymbol, got, want, className)
		}
	}
}

func readCompilerKconfigOracle(initKconfigPath, kconfigIncludePath string) (*compilerKconfigOracle, error) {
	file, err := os.Open(initKconfigPath)
	if err != nil {
		return nil, err
	}
	oracle, err := parseCompilerKconfig(file)
	closeErr := file.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}

	file, err = os.Open(kconfigIncludePath)
	if err != nil {
		return nil, err
	}
	err = parseKconfigInfoScripts(file, oracle)
	closeErr = file.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err := oracle.validate(); err != nil {
		return nil, err
	}
	return oracle, nil
}

func parseCompilerKconfig(r io.Reader) (*compilerKconfigOracle, error) {
	oracle := &compilerKconfigOracle{
		classes:   map[string]*toolClassOracle{},
		fallbacks: map[string]string{},
	}
	versionTextSymbols := map[string]string{}
	currentSymbol := ""
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if match := kconfigBlockPattern.FindStringSubmatch(line); match != nil {
			currentSymbol = "CONFIG_" + match[1]
			continue
		}
		if currentSymbol == "" {
			continue
		}
		if match := identityPattern.FindStringSubmatch(line); match != nil {
			class := oracle.class(match[1])
			if old := class.identities[match[2]]; old != "" && old != currentSymbol {
				return nil, fmt.Errorf("tool identity %s %q maps to both %s and %s", match[1], match[2], old, currentSymbol)
			}
			class.identities[match[2]] = currentSymbol
			continue
		}
		if match := versionPattern.FindStringSubmatch(line); match != nil {
			class := oracle.class(match[1])
			if match[2] == "" {
				if class.directVersion != "" && class.directVersion != currentSymbol {
					return nil, fmt.Errorf("tool class %s has direct version symbols %s and %s", match[1], class.directVersion, currentSymbol)
				}
				class.directVersion = currentSymbol
			} else {
				identity := "CONFIG_" + match[2]
				if old := class.conditionalVersions[identity]; old != "" && old != currentSymbol {
					return nil, fmt.Errorf("tool identity %s selects version symbols %s and %s", identity, old, currentSymbol)
				}
				class.conditionalVersions[identity] = currentSymbol
			}
			continue
		}
		if match := literalPattern.FindStringSubmatch(line); match != nil {
			oracle.fallbacks[currentSymbol] = match[1]
			continue
		}
		if match := versionTextPattern.FindStringSubmatch(line); match != nil && "CONFIG_"+match[1] == currentSymbol && strings.HasSuffix(match[1], "_VERSION_TEXT") {
			className := strings.ToLower(strings.TrimSuffix(match[1], "_VERSION_TEXT"))
			versionTextSymbols[className] = currentSymbol
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	for className, class := range oracle.classes {
		if len(class.identities) == 0 {
			delete(oracle.classes, className)
			continue
		}
		if symbol := versionTextSymbols[className]; symbol != "" {
			if oracle.versionTextSymbol != "" {
				return nil, fmt.Errorf("multiple identity-bearing compiler version text symbols: %s and %s", oracle.versionTextSymbol, symbol)
			}
			oracle.compilerClass = className
			oracle.versionTextSymbol = symbol
		}
	}
	return oracle, nil
}

func parseKconfigInfoScripts(r io.Reader, oracle *compilerKconfigOracle) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		match := infoScriptPattern.FindStringSubmatch(strings.TrimSpace(scanner.Text()))
		if match == nil {
			continue
		}
		class, ok := oracle.classes[match[1]]
		if !ok {
			continue
		}
		if class.script != "" && class.script != match[2] {
			return fmt.Errorf("tool class %s uses both %s and %s", match[1], class.script, match[2])
		}
		class.script = match[2]
	}
	return scanner.Err()
}

func (o *compilerKconfigOracle) class(name string) *toolClassOracle {
	class := o.classes[name]
	if class == nil {
		class = &toolClassOracle{
			identities:          map[string]string{},
			conditionalVersions: map[string]string{},
		}
		o.classes[name] = class
	}
	return class
}

func (o *compilerKconfigOracle) validate() error {
	if o.versionTextSymbol == "" || o.compilerClass == "" {
		return fmt.Errorf("Linux Kconfig does not declare a compiler version text symbol")
	}
	if _, ok := o.classes[o.compilerClass]; !ok {
		return fmt.Errorf("compiler version text refers to tool class %q, which has no identity rules", o.compilerClass)
	}
	for _, name := range sortedKeys(o.classes) {
		class := o.classes[name]
		if len(class.identities) == 0 {
			return fmt.Errorf("tool class %s has version rules but no identity rules", name)
		}
		if class.script == "" {
			return fmt.Errorf("tool class %s has no source-defined info script", name)
		}
		if class.directVersion == "" && len(class.conditionalVersions) == 0 {
			return fmt.Errorf("tool class %s has identity rules but no version rule", name)
		}
		if class.directVersion == "" {
			for _, identity := range class.identities {
				versionSymbol, ok := class.conditionalVersions[identity]
				if !ok {
					return fmt.Errorf("tool identity %s has no conditional version rule", identity)
				}
				if _, ok := o.fallbacks[versionSymbol]; !ok {
					return fmt.Errorf("conditional version symbol %s has no literal fallback", versionSymbol)
				}
			}
		}
	}
	return nil
}

func readKernelConfig(filename string) (map[string]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if name, ok := strings.CutSuffix(strings.TrimPrefix(line, "# "), " is not set"); ok && strings.HasPrefix(name, "CONFIG_") {
			values[name] = "n"
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || !strings.HasPrefix(name, "CONFIG_") {
			continue
		}
		if strings.HasPrefix(value, `"`) {
			unquoted, err := strconv.Unquote(value)
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", name, err)
			}
			value = unquoted
		}
		values[name] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func readProbeEvidence(roots []string) (*probeEvidence, error) {
	evidence := &probeEvidence{requests: map[string]probeRequest{}}
	seenRoots := map[string]bool{}
	for _, root := range roots {
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, fmt.Errorf("resolve probe artifact %s: %w", root, err)
		}
		if seenRoots[realRoot] {
			continue
		}
		seenRoots[realRoot] = true
		if err := filepath.WalkDir(realRoot, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || filepath.Ext(path) != ".json" {
				return nil
			}
			switch filepath.Base(filepath.Dir(path)) {
			case "requests":
				var request probeRequest
				if err := readJSON(path, &request); err != nil {
					return err
				}
				requestID := strings.TrimSuffix(filepath.Base(path), ".json")
				evidence.requests[requestID] = request
			case "results":
				var result probeResult
				if err := readJSON(path, &result); err != nil {
					return err
				}
				evidence.results = append(evidence.results, result)
			}
			return nil
		}); err != nil {
			return nil, fmt.Errorf("walk probe artifact %s: %w", root, err)
		}
	}
	if len(evidence.requests) == 0 || len(evidence.results) == 0 {
		return nil, fmt.Errorf("probe output group contained %d requests and %d results", len(evidence.requests), len(evidence.results))
	}
	return evidence, nil
}

func readJSON(filename string, value any) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := json.NewDecoder(file).Decode(value); err != nil {
		return fmt.Errorf("decode %s: %w", filename, err)
	}
	return nil
}

func (e *probeEvidence) measurement(script string) (toolMeasurement, error) {
	requestIDs := map[string]bool{}
	for requestID, request := range e.requests {
		for _, source := range request.Sources {
			if filepath.ToSlash(source) == script {
				requestIDs[requestID] = true
				break
			}
		}
	}
	if len(requestIDs) == 0 {
		return toolMeasurement{}, fmt.Errorf("no probe request declares source %s", script)
	}

	measurements := map[toolMeasurement]bool{}
	for _, result := range e.results {
		if result.Scope != targetProbeScope || !requestIDs[result.RequestID] || result.Kind != "text" {
			continue
		}
		fields := strings.Fields(result.Text)
		if len(fields) != 2 {
			return toolMeasurement{}, fmt.Errorf("probe result %s is %q, want identity and numeric version", result.RequestID, result.Text)
		}
		version, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || version == 0 {
			return toolMeasurement{}, fmt.Errorf("probe result %s has non-positive version %q", result.RequestID, fields[1])
		}
		measurements[toolMeasurement{name: fields[0], version: fields[1]}] = true
	}
	if len(measurements) != 1 {
		values := make([]string, 0, len(measurements))
		for measurement := range measurements {
			values = append(values, measurement.name+" "+measurement.version)
		}
		sort.Strings(values)
		return toolMeasurement{}, fmt.Errorf("got %d distinct target measurements (%s), want exactly one", len(values), strings.Join(values, ", "))
	}
	for measurement := range measurements {
		return measurement, nil
	}
	panic("unreachable")
}

func (e *probeEvidence) compilerVersionText(compilerClass string) (string, error) {
	versionSteps := map[string]map[string]bool{}
	for requestID, request := range e.requests {
		for _, step := range request.Steps {
			if step.Tool == compilerClass && len(step.Arguments) == 1 && step.Arguments[0] == "--version" {
				if versionSteps[requestID] == nil {
					versionSteps[requestID] = map[string]bool{}
				}
				versionSteps[requestID][step.Name] = true
			}
		}
	}
	if len(versionSteps) == 0 {
		return "", fmt.Errorf("no %s --version probe request", compilerClass)
	}

	values := map[string]bool{}
	for _, result := range e.results {
		steps, ok := versionSteps[result.RequestID]
		if !ok || result.Scope != targetProbeScope {
			continue
		}
		for _, step := range result.Steps {
			if !steps[step.Name] || step.Status != "success" || step.ExitCode != 0 {
				continue
			}
			line, _, _ := strings.Cut(step.Stdout, "\n")
			line = strings.TrimSuffix(line, "\r")
			if line != "" {
				values[line] = true
			}
		}
	}
	if len(values) != 1 {
		found := sortedKeys(values)
		return "", fmt.Errorf("got %d distinct successful target version lines (%s), want exactly one", len(found), strings.Join(found, ", "))
	}
	for value := range values {
		return value, nil
	}
	panic("unreachable")
}

func resolveRunfile(t *testing.T, environment string) string {
	t.Helper()
	value := os.Getenv(environment)
	if value == "" {
		t.Fatalf("%s is empty", environment)
	}
	path, err := runfiles.Rlocation(value)
	if err != nil {
		t.Fatalf("resolve %s=%q: %v", environment, value, err)
	}
	return path
}

func resolveRunfileList(t *testing.T, environment string) []string {
	t.Helper()
	values := strings.Fields(os.Getenv(environment))
	if len(values) == 0 {
		t.Fatalf("%s is empty", environment)
	}
	paths := make([]string, 0, len(values))
	for _, value := range values {
		path, err := runfiles.Rlocation(value)
		if err != nil {
			t.Fatalf("resolve %s entry %q: %v", environment, value, err)
		}
		paths = append(paths, path)
	}
	return paths
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
