package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

type repeated []string

func (r *repeated) String() string {
	return strings.Join(*r, ",")
}

func (r *repeated) Set(value string) error {
	if value == "" {
		return fmt.Errorf("empty assertion")
	}
	*r = append(*r, value)
	return nil
}

type metadata struct {
	Configs        []config        `json:"configs"`
	ObjectVariants []objectVariant `json:"object_variants"`
}

type config struct {
	Name          string   `json:"name"`
	ObjectTargets []string `json:"object_targets"`
}

type objectVariant struct {
	Target string `json:"target"`
	Object string `json:"object"`
}

func main() {
	var (
		metadataPath = flag.String("metadata", "", "Compact metadata JSON to validate")
		share        repeated
		differ       repeated
		present      repeated
		absent       repeated
	)
	flag.Var(&share, "share", "Assert CONFIG_A:CONFIG_B:OBJECT use the same object target. May be repeated")
	flag.Var(&differ, "differ", "Assert CONFIG_A:CONFIG_B:OBJECT use different object targets. May be repeated")
	flag.Var(&present, "present", "Assert CONFIG:OBJECT is present. May be repeated")
	flag.Var(&absent, "absent", "Assert CONFIG:OBJECT is absent. May be repeated")
	flag.Parse()

	if *metadataPath == "" {
		fmt.Fprintln(os.Stderr, "-metadata is required")
		os.Exit(2)
	}
	data, err := os.ReadFile(*metadataPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read metadata: %v\n", err)
		os.Exit(1)
	}
	var meta metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		fmt.Fprintf(os.Stderr, "parse metadata: %v\n", err)
		os.Exit(1)
	}
	index := newIndex(&meta)

	if err := checkPresence(index, present, true); err != nil {
		fail(err)
	}
	if err := checkPresence(index, absent, false); err != nil {
		fail(err)
	}
	if err := checkPair(index, share, true); err != nil {
		fail(err)
	}
	if err := checkPair(index, differ, false); err != nil {
		fail(err)
	}
	fmt.Println("compact metadata checks passed")
}

type metadataIndex struct {
	targetObject map[string]string
	objectsByCfg map[string]map[string]string
}

func newIndex(meta *metadata) *metadataIndex {
	idx := &metadataIndex{
		targetObject: map[string]string{},
		objectsByCfg: map[string]map[string]string{},
	}
	for _, variant := range meta.ObjectVariants {
		idx.targetObject[variant.Target] = variant.Object
	}
	for _, cfg := range meta.Configs {
		objects := map[string]string{}
		for _, target := range cfg.ObjectTargets {
			object := idx.targetObject[target]
			if object == "" {
				continue
			}
			objects[object] = target
		}
		idx.objectsByCfg[cfg.Name] = objects
	}
	return idx
}

func checkPresence(idx *metadataIndex, assertions []string, wantPresent bool) error {
	for _, assertion := range assertions {
		cfg, object, err := parsePresence(assertion)
		if err != nil {
			return err
		}
		target := idx.objectTarget(cfg, object)
		if wantPresent && target == "" {
			return fmt.Errorf("%s: object %q is absent", cfg, object)
		}
		if !wantPresent && target != "" {
			return fmt.Errorf("%s: object %q unexpectedly present as %s", cfg, object, target)
		}
	}
	return nil
}

func checkPair(idx *metadataIndex, assertions []string, wantSame bool) error {
	for _, assertion := range assertions {
		left, right, object, err := parsePair(assertion)
		if err != nil {
			return err
		}
		leftTarget := idx.objectTarget(left, object)
		rightTarget := idx.objectTarget(right, object)
		if leftTarget == "" || rightTarget == "" {
			return fmt.Errorf("%s: missing object %q in %q or %q", assertion, object, left, right)
		}
		if wantSame && leftTarget != rightTarget {
			return fmt.Errorf("%s: targets differ: %s != %s", assertion, leftTarget, rightTarget)
		}
		if !wantSame && leftTarget == rightTarget {
			return fmt.Errorf("%s: targets unexpectedly match: %s", assertion, leftTarget)
		}
	}
	return nil
}

func (idx *metadataIndex) objectTarget(config, object string) string {
	objects := idx.objectsByCfg[config]
	if objects == nil {
		return ""
	}
	return objects[object]
}

func parsePresence(value string) (string, string, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected CONFIG:OBJECT assertion, got %q", value)
	}
	return parts[0], parts[1], nil
}

func parsePair(value string) (string, string, string, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf("expected CONFIG_A:CONFIG_B:OBJECT assertion, got %q", value)
	}
	return parts[0], parts[1], parts[2], nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
