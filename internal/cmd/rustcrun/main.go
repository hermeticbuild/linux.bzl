package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
	"github.com/hermeticbuild/linux.bzl/internal/rusttoolchain"
)

const configFlagsMarker = "--linux-bzl-config-flags"

type versionPredicate struct {
	AtLeast    string   `json:"at_least"`
	Add        []string `json:"add"`
	Remove     []string `json:"remove"`
	ElseAdd    []string `json:"else_add"`
	ElseRemove []string `json:"else_remove"`
	Config     string   `json:"config,omitempty"`
	Equals     string   `json:"equals,omitempty"`
	Unless     string   `json:"unless_config,omitempty"`
}

type configCondition struct {
	Config    string   `json:"config"`
	Equals    string   `json:"equals,omitempty"`
	Unless    string   `json:"unless_config,omitempty"`
	Flags     []string `json:"flags"`
	ElseFlags []string `json:"else_flags"`
}

type predicateFlags []versionPredicate

func (f *predicateFlags) String() string {
	return fmt.Sprintf("%d predicates", len(*f))
}

func (f *predicateFlags) Set(value string) error {
	var predicate versionPredicate
	if err := json.Unmarshal([]byte(value), &predicate); err != nil {
		return err
	}
	if predicate.AtLeast == "" {
		return errors.New("version predicate requires at_least")
	}
	if err := validateConfigSelector(predicate.Config, predicate.Equals, predicate.Unless, false); err != nil {
		return fmt.Errorf("version predicate: %w", err)
	}
	*f = append(*f, predicate)
	return nil
}

type conditionFlags []configCondition

func (f *conditionFlags) String() string {
	return fmt.Sprintf("%d conditions", len(*f))
}

func (f *conditionFlags) Set(value string) error {
	var condition configCondition
	if err := json.Unmarshal([]byte(value), &condition); err != nil {
		return err
	}
	if err := validateConfigSelector(condition.Config, condition.Equals, condition.Unless, true); err != nil {
		return fmt.Errorf("config condition: %w", err)
	}
	*f = append(*f, condition)
	return nil
}

func validateConfigSelector(config, equals, unless string, required bool) error {
	if config == "" {
		if required {
			return errors.New("config is required")
		}
		if equals != "" || unless != "" {
			return errors.New("equals and unless_config require config")
		}
		return nil
	}
	if equals == "" {
		equals = "y"
	}
	if !isConfigKey(config) {
		return fmt.Errorf("invalid config key %q", config)
	}
	if unless != "" && !isConfigKey(unless) {
		return fmt.Errorf("invalid unless_config key %q", unless)
	}
	return nil
}

func isConfigKey(value string) bool {
	return len(value) > len("CONFIG_") && value[:len("CONFIG_")] == "CONFIG_"
}

type runOptions struct {
	Probe      string
	Config     string
	Predicates []versionPredicate
	Conditions []configCondition
	Command    []string
}

func parseArgs(args []string) (runOptions, error) {
	separator := -1
	for i, arg := range args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(args) {
		return runOptions{}, errors.New("expected -- RUSTC [ARG...]")
	}
	var predicates predicateFlags
	var conditions conditionFlags
	flags := flag.NewFlagSet("rustcrun", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	probe := flags.String("probe", "", "Rust toolchain probe JSON")
	config := flags.String("config", "", "action-resolved Linux .config")
	flags.Var(&predicates, "predicate", "JSON version predicate")
	flags.Var(&conditions, "config-condition", "JSON Kconfig-dependent flag condition")
	if err := flags.Parse(args[:separator]); err != nil {
		return runOptions{}, err
	}
	if *probe == "" {
		return runOptions{}, errors.New("-probe is required")
	}
	return runOptions{
		Probe:      *probe,
		Config:     *config,
		Predicates: predicates,
		Conditions: conditions,
		Command:    args[separator+1:],
	}, nil
}

func applyPredicates(probe rusttoolchain.Probe, args []string, predicates []versionPredicate) ([]string, error) {
	return applyPredicatesWithConfig(probe, nil, args, predicates)
}

func applyPredicatesWithConfig(probe rusttoolchain.Probe, config map[string]string, args []string, predicates []versionPredicate) ([]string, error) {
	out := append([]string(nil), args...)
	for _, predicate := range predicates {
		if predicate.Config != "" && !configMatches(config, predicate.Config, predicate.Equals, predicate.Unless) {
			continue
		}
		matches, err := probe.AtLeast(predicate.AtLeast)
		if err != nil {
			return nil, fmt.Errorf("invalid at_least %q: %w", predicate.AtLeast, err)
		}
		add, remove := predicate.ElseAdd, predicate.ElseRemove
		if matches {
			add, remove = predicate.Add, predicate.Remove
		}
		for _, removed := range remove {
			filtered := out[:0]
			for _, arg := range out {
				if arg != removed {
					filtered = append(filtered, arg)
				}
			}
			out = filtered
		}
		out = append(out, add...)
	}
	return out, nil
}

func configMatches(config map[string]string, symbol, equals, unless string) bool {
	if equals == "" {
		equals = "y"
	}
	value, ok := config[symbol]
	if !ok {
		value = "n"
	}
	if value != equals {
		return false
	}
	return unless == "" || config[unless] != "y"
}

func applyConfigConditions(config map[string]string, args []string, conditions []configCondition) ([]string, error) {
	marker := -1
	for i, arg := range args {
		if arg != configFlagsMarker {
			continue
		}
		if marker >= 0 {
			return nil, fmt.Errorf("multiple %s markers", configFlagsMarker)
		}
		marker = i
	}
	if marker < 0 {
		return nil, fmt.Errorf("config conditions require a %s marker", configFlagsMarker)
	}
	selected := []string{}
	for _, condition := range conditions {
		flags := condition.ElseFlags
		if configMatches(config, condition.Config, condition.Equals, condition.Unless) {
			flags = condition.Flags
		}
		selected = append(selected, flags...)
	}
	out := make([]string, 0, len(args)-1+len(selected))
	out = append(out, args[:marker]...)
	out = append(out, selected...)
	out = append(out, args[marker+1:]...)
	return out, nil
}

func readConfig(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	config, decodeErr := kconfig.ParseConfig(file)
	closeErr := file.Close()
	if decodeErr != nil {
		return nil, decodeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return config, nil
}

func run(args []string) error {
	options, err := parseArgs(args)
	if err != nil {
		return err
	}
	probeFile, err := os.Open(options.Probe)
	if err != nil {
		return err
	}
	probe, decodeErr := rusttoolchain.Decode(probeFile)
	closeErr := probeFile.Close()
	if decodeErr != nil {
		return decodeErr
	}
	if closeErr != nil {
		return closeErr
	}
	needsConfig := len(options.Conditions) > 0
	if !needsConfig {
		for _, predicate := range options.Predicates {
			if predicate.Config != "" {
				needsConfig = true
				break
			}
		}
	}
	config := map[string]string{}
	if needsConfig {
		if options.Config == "" {
			return errors.New("-config is required by config-dependent flags")
		}
		config, err = readConfig(options.Config)
		if err != nil {
			return fmt.Errorf("read config: %w", err)
		}
	}
	command := options.Command
	if len(options.Conditions) > 0 {
		command, err = applyConfigConditions(config, command, options.Conditions)
		if err != nil {
			return err
		}
	}
	command, err = applyPredicatesWithConfig(probe, config, command, options.Predicates)
	if err != nil {
		return err
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = os.Environ()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		return err
	}
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "rustcrun: %v\n", err)
		os.Exit(1)
	}
}
