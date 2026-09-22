package kconfig

import (
	"fmt"
	"path"
	"strings"
)

// optionalObjectTreeShellRead interprets the source's optional cat of a
// generated config file against the same immutable Make-visible object-tree
// snapshot used by $(file < ...) and wildcard. The source deliberately
// discards stderr because the generated file can be absent in an early Make
// invocation. Never fall through to the analysis worker's filesystem: a later
// invocation may instead observe exact bytes produced by an earlier action.
func (p *kbuildParser) optionalObjectTreeShellRead(command string) (string, bool, error) {
	fields := strings.Fields(command)
	if len(fields) < 3 || len(fields) > 4 || fields[0] != "cat" ||
		!strings.HasPrefix(fields[1], "include/config/") {
		return "", false, nil
	}
	if (len(fields) == 3 && fields[2] != "2>/dev/null") ||
		(len(fields) == 4 && (fields[2] != "2>" || fields[3] != "/dev/null")) {
		return "", false, nil
	}
	filename := fields[1]
	if _, owned := p.sourceRoots["__LINUX_BZL_OBJECT_TREE__"]; !owned {
		return "", false, nil
	}
	if p.invocationLocationSet && p.invocationLocation.Tree != CompactKbuildInvocationObjectTree {
		return "", false, nil
	}
	if filename == "include/config/" || path.Clean(filename) != filename ||
		strings.ContainsAny(filename, "\\\x00$*?[]{}~`'\";|&<>#") {
		return "", true, fmt.Errorf("%s: optional object-tree read has an escaping or dynamic path %q", p.currentPos, filename)
	}
	if p.virtualFileView == nil {
		return "", true, nil
	}
	query := path.Join("__LINUX_BZL_OBJECT_TREE__", p.invocationLocation.Directory, filename)
	contents, exists, exact, err := p.virtualFileView.Read(query)
	if err != nil {
		return "", true, fmt.Errorf("%s: optional object-tree read %q: %w", p.currentPos, query, err)
	}
	if !exists {
		return "", true, nil
	}
	if !exact {
		return "", true, fmt.Errorf("optional object-tree read %q has no exact contents", query)
	}
	contents, err = normalizeKbuildShellOutput(contents)
	if err != nil {
		return "", true, fmt.Errorf("optional object-tree read %q: %w", query, err)
	}
	return contents, true, nil
}

// exactObjectTreeShellCat is the selected recursive Make read of an already
// completed object output. Its bytes may select new targets, so a stale physical
// file, an absent writer, or a pending/opaque frontier entry cannot stand in
// for the exact version visible at this invocation's source position.
func (p *kbuildParser) exactObjectTreeShellCat(command string) (string, bool, error) {
	if !strings.HasPrefix(command, "cat ") || !p.invocationLocationSet ||
		p.invocationLocation.Tree != CompactKbuildInvocationObjectTree {
		return "", false, nil
	}
	fields := strings.Fields(command)
	if len(fields) != 2 || fields[0] != "cat" {
		return "", false, nil
	}
	name := fields[1]
	if path.Clean(name) != name || name == "." || strings.ContainsAny(name, "\\\x00$*?[]{}~`'\";|&<>#") {
		return "", true, fmt.Errorf("%s: exact object-tree cat has an escaping or dynamic path %q", p.currentPos, name)
	}
	if err := validatePlanRelativePath("exact object-tree cat", name); err != nil {
		return "", true, fmt.Errorf("%s: %w", p.currentPos, err)
	}
	for _, variable := range []string{"PATH", "ENV", "BASH_ENV"} {
		if value, defined := p.lookupRawVar(variable); defined && value != "" {
			return "", true, fmt.Errorf("%s: exact object-tree cat has unsupported selected %s override", p.currentPos, variable)
		}
	}
	if shell, defined := p.lookupRawVar("SHELL"); defined && shell != "/bin/sh" {
		return "", true, fmt.Errorf("%s: exact object-tree cat has unsupported selected SHELL override", p.currentPos)
	}
	if p.virtualFileView == nil || p.sourceRoots["__LINUX_BZL_OBJECT_TREE__"] == "" {
		return "", true, fmt.Errorf("%s: exact object-tree cat has no declared object frontier", p.currentPos)
	}
	query := path.Join("__LINUX_BZL_OBJECT_TREE__", p.invocationLocation.Directory, name)
	content, exists, exact, err := p.virtualFileView.Read(query)
	if err != nil {
		return "", true, fmt.Errorf("%s: exact object-tree cat %q: %w", p.currentPos, query, err)
	}
	if !exists {
		return "", true, fmt.Errorf("%s: exact object-tree cat %q has no completed producer", p.currentPos, query)
	}
	if !exact {
		return "", true, fmt.Errorf("%s: exact object-tree cat %q has a producer with opaque contents", p.currentPos, query)
	}
	content, err = normalizeKbuildShellOutput(content)
	if err != nil {
		return "", true, fmt.Errorf("%s: exact object-tree cat %q: %w", p.currentPos, query, err)
	}
	return content, true, nil
}
