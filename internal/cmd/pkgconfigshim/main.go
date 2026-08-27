package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

const pkgConfigManifestSchema = "linux.bzl/pkg-config-manifest/v1"

const (
	maxPkgConfigPackages      = 256
	maxPkgConfigFlags         = 4096
	maxPkgConfigManifestBytes = 1 << 20
)

type pkgConfigPackage struct {
	CFlags []string `json:"cflags"`
	Libs   []string `json:"libs"`
}

type pkgConfigManifest struct {
	Schema   string                      `json:"schema"`
	Packages map[string]pkgConfigPackage `json:"packages"`
}

func readPkgConfigManifest(filename string) (*pkgConfigManifest, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect manifest: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxPkgConfigManifestBytes {
		return nil, fmt.Errorf("manifest is not a regular file or exceeds %d bytes", maxPkgConfigManifestBytes)
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxPkgConfigManifestBytes+1))
	decoder.DisallowUnknownFields()
	var manifest pkgConfigManifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("decode manifest: trailing JSON value")
		}
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := validatePkgConfigManifest(manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func validatePkgConfigManifest(manifest pkgConfigManifest) error {
	if manifest.Schema != pkgConfigManifestSchema {
		return fmt.Errorf("manifest schema = %q, want %q", manifest.Schema, pkgConfigManifestSchema)
	}
	if manifest.Packages == nil {
		return fmt.Errorf("manifest packages are required")
	}
	if len(manifest.Packages) > maxPkgConfigPackages {
		return fmt.Errorf("manifest contains more than %d packages", maxPkgConfigPackages)
	}
	names := make([]string, 0, len(manifest.Packages))
	for name := range manifest.Packages {
		names = append(names, name)
	}
	sort.Strings(names)
	flags := 0
	for _, name := range names {
		if !validPkgConfigPackageName(name) {
			return fmt.Errorf("manifest has invalid package name %q", name)
		}
		pkg := manifest.Packages[name]
		if pkg.CFlags == nil || pkg.Libs == nil {
			return fmt.Errorf("manifest package %q must define cflags and libs", name)
		}
		for _, family := range []struct {
			kind   string
			values []string
		}{
			{kind: "cflags", values: pkg.CFlags},
			{kind: "libs", values: pkg.Libs},
		} {
			kind, values := family.kind, family.values
			flags += len(values)
			if flags > maxPkgConfigFlags {
				return fmt.Errorf("manifest contains more than %d flags", maxPkgConfigFlags)
			}
			for index, value := range values {
				if value == "" || len(value) > 1<<16 || strings.ContainsAny(value, "\x00\r\n") {
					return fmt.Errorf("manifest package %q %s flag %d is empty, invalid, or exceeds 64 KiB", name, kind, index)
				}
			}
		}
	}
	return nil
}

func validPkgConfigPackageName(value string) bool {
	if value == "" || len(value) > 255 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("_.+-", character) {
			continue
		}
		return false
	}
	return true
}

type pkgConfigQuery struct {
	kind     string
	packages []string
}

func parsePkgConfigQuery(arguments []string) (pkgConfigQuery, error) {
	query := pkgConfigQuery{}
	for _, argument := range arguments {
		switch argument {
		case "--cflags", "--libs", "--exists":
			if query.kind != "" {
				return pkgConfigQuery{}, fmt.Errorf("pkg-config query repeats or combines output modes %q and %q", query.kind, argument)
			}
			query.kind = argument
		default:
			if !validPkgConfigPackageName(argument) {
				return pkgConfigQuery{}, fmt.Errorf("pkg-config query has unsupported argument %q", argument)
			}
			query.packages = append(query.packages, argument)
		}
	}
	if query.kind == "" {
		return pkgConfigQuery{}, fmt.Errorf("pkg-config query requires exactly one of --cflags, --libs, or --exists")
	}
	if len(query.packages) == 0 {
		return pkgConfigQuery{}, fmt.Errorf("pkg-config query requires at least one package")
	}
	if len(query.packages) > maxPkgConfigPackages {
		return pkgConfigQuery{}, fmt.Errorf("pkg-config query contains more than %d packages", maxPkgConfigPackages)
	}
	return query, nil
}

func pkgConfigShellWord(value string) string {
	safe := value != ""
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("_@%+=:,./-", character) {
			continue
		}
		safe = false
		break
	}
	if safe {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func executePkgConfigQuery(manifest *pkgConfigManifest, query pkgConfigQuery, stdout, stderr io.Writer) int {
	values := []string{}
	for _, name := range query.packages {
		pkg, ok := manifest.Packages[name]
		if !ok {
			fmt.Fprintf(stderr, "package %q is unavailable\n", name)
			return 1
		}
		switch query.kind {
		case "--cflags":
			values = append(values, pkg.CFlags...)
		case "--libs":
			values = append(values, pkg.Libs...)
		case "--exists":
		default:
			fmt.Fprintf(stderr, "unsupported pkg-config query mode %q\n", query.kind)
			return 2
		}
	}
	if query.kind != "--exists" && len(values) != 0 {
		words := make([]string, len(values))
		for index, value := range values {
			words[index] = pkgConfigShellWord(value)
		}
		fmt.Fprintln(stdout, strings.Join(words, " "))
	}
	return 0
}

func run(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("pkgconfigshim", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestFilename := flags.String("manifest", "", "declared pkg-config package manifest")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *manifestFilename == "" {
		fmt.Fprintln(stderr, "-manifest is required")
		return 2
	}
	manifest, err := readPkgConfigManifest(*manifestFilename)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	query, err := parsePkgConfigQuery(flags.Args())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	return executePkgConfigQuery(manifest, query, stdout, stderr)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
