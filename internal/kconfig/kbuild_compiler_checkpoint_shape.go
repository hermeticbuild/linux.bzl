package kconfig

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// ProbeRequest intentionally omits empty protocol fields. Its in-process
// registry additionally checks exact Go values, including nil versus empty.
// Preserve that information in the checkpoint without changing request bytes,
// IDs, or the registry's collision checks. Paths name exported Go fields, map
// keys and canonical decimal slice indexes; they carry no filesystem authority.
func compilerCheckpointEmptyContainers(record kbuildCompilerCheckpoint) ([][]string, error) {
	var paths [][]string
	remaining := 1 << 20
	// Walk only types which can contain omitted maps/slices. Scalar payloads,
	// explicit containers, and interned request references cannot lose shape in
	// JSON; enumerating all their words wastes the bounded traversal budget.
	shapeTypes := map[reflect.Type]bool{}
	var needsShape func(reflect.Type) bool
	needsShape = func(typ reflect.Type) bool {
		if needed, found := shapeTypes[typ]; found {
			return needed
		}
		// Recursive protocol types are conservatively visited, never pruned
		// based on an unfinished schema walk.
		shapeTypes[typ] = true
		needed := false
		switch typ.Kind() {
		case reflect.Pointer, reflect.Map, reflect.Slice:
			needed = needsShape(typ.Elem())
		case reflect.Struct:
			for index := 0; index < typ.NumField(); index++ {
				field := typ.Field(index)
				tag := strings.Split(field.Tag.Get("json"), ",")
				if tag[0] == "-" {
					continue
				}
				container := field.Type.Kind() == reflect.Map || field.Type.Kind() == reflect.Slice
				if container && slices.Contains(tag[1:], "omitempty") || needsShape(field.Type) {
					needed = true
				}
			}
		}
		shapeTypes[typ] = needed
		return needed
	}
	var walk func(reflect.Value, []string, bool) error
	walk = func(v reflect.Value, path []string, omitted bool) error {
		container := v.Kind() == reflect.Map || v.Kind() == reflect.Slice
		if !(container && omitted) && !needsShape(v.Type()) {
			return nil
		}
		remaining--
		if remaining < 0 || len(path) > 64 {
			return fmt.Errorf("compiler checkpoint shape exceeds structural budget")
		}
		if (v.Kind() == reflect.Map || v.Kind() == reflect.Slice) && omitted && !v.IsNil() && v.Len() == 0 {
			paths = append(paths, slices.Clone(path))
		}
		child := func(value reflect.Value, name string, omit bool) error {
			return walk(value, append(slices.Clone(path), name), omit)
		}
		switch v.Kind() {
		case reflect.Pointer:
			if !v.IsNil() {
				return walk(v.Elem(), path, false)
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				field := v.Type().Field(i)
				if field.Tag.Get("json") == "-" {
					continue
				}
				if field.PkgPath != "" {
					return fmt.Errorf("compiler checkpoint contains an unexported field")
				}
				if err := child(v.Field(i), field.Name, slices.Contains(strings.Split(field.Tag.Get("json"), ",")[1:], "omitempty")); err != nil {
					return err
				}
			}
		case reflect.Map:
			if !needsShape(v.Type().Elem()) {
				return nil
			}
			if v.Type().Key().Kind() != reflect.String {
				return fmt.Errorf("compiler checkpoint has a non-string map key")
			}
			keys := v.MapKeys()
			slices.SortFunc(keys, func(a, b reflect.Value) int { return strings.Compare(a.String(), b.String()) })
			for _, key := range keys {
				if err := child(v.MapIndex(key), key.String(), false); err != nil {
					return err
				}
			}
		case reflect.Slice:
			if !needsShape(v.Type().Elem()) {
				return nil
			}
			for i := 0; i < v.Len(); i++ {
				if err := child(v.Index(i), strconv.Itoa(i), false); err != nil {
					return err
				}
			}
		}
		return nil
	}
	// Shape metadata describes only the payload, not itself.
	record.EmptyContainers = nil
	if err := walk(reflect.ValueOf(record), nil, false); err != nil {
		return nil, err
	}
	return paths, nil
}

func restoreCompilerCheckpointEmptyContainers(record *kbuildCompilerCheckpoint) error {
	if len(record.EmptyContainers) > 1<<20 {
		return fmt.Errorf("compiler checkpoint has too many empty containers")
	}
	var restore func(reflect.Value, []string) error
	restore = func(v reflect.Value, path []string) error {
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return fmt.Errorf("compiler checkpoint shape crosses a nil pointer")
			}
			return restore(v.Elem(), path)
		}
		if len(path) == 0 {
			if !v.CanSet() || (v.Kind() != reflect.Map && v.Kind() != reflect.Slice) || v.Len() != 0 {
				return fmt.Errorf("compiler checkpoint shape does not name an empty container")
			}
			if v.Kind() == reflect.Map {
				v.Set(reflect.MakeMap(v.Type()))
			} else {
				v.Set(reflect.MakeSlice(v.Type(), 0, 0))
			}
			return nil
		}
		switch v.Kind() {
		case reflect.Struct:
			field, ok := v.Type().FieldByName(path[0])
			if !ok || field.PkgPath != "" {
				return fmt.Errorf("compiler checkpoint shape names an unknown field")
			}
			return restore(v.FieldByIndex(field.Index), path[1:])
		case reflect.Map:
			if v.Type().Key().Kind() != reflect.String {
				return fmt.Errorf("compiler checkpoint shape has an invalid map")
			}
			key := reflect.ValueOf(path[0]).Convert(v.Type().Key())
			entry := v.MapIndex(key)
			if !entry.IsValid() {
				return fmt.Errorf("compiler checkpoint shape names an unknown map entry")
			}
			copy := reflect.New(entry.Type()).Elem()
			copy.Set(entry)
			if err := restore(copy, path[1:]); err != nil {
				return err
			}
			v.SetMapIndex(key, copy)
			return nil
		case reflect.Slice:
			i, err := strconv.Atoi(path[0])
			if err != nil || i < 0 || i >= v.Len() || strconv.Itoa(i) != path[0] {
				return fmt.Errorf("compiler checkpoint shape has an invalid slice index")
			}
			return restore(v.Index(i), path[1:])
		}
		return fmt.Errorf("compiler checkpoint shape traverses a scalar")
	}
	for _, path := range record.EmptyContainers {
		if len(path) == 0 || len(path) > 64 || path[0] == "EmptyContainers" {
			return fmt.Errorf("compiler checkpoint has an invalid shape path")
		}
		if err := restore(reflect.ValueOf(record), path); err != nil {
			return err
		}
	}
	actual, err := compilerCheckpointEmptyContainers(*record)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, record.EmptyContainers) {
		return fmt.Errorf("compiler checkpoint shape is redundant or noncanonical")
	}
	return nil
}
