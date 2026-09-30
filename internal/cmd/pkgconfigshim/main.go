package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/pkgconfigmanifest"
)

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
			if !pkgconfigmanifest.ValidPackageName(argument) {
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
	if len(query.packages) > pkgconfigmanifest.MaxPackages {
		return pkgConfigQuery{}, fmt.Errorf("pkg-config query contains more than %d packages", pkgconfigmanifest.MaxPackages)
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

func executePkgConfigQuery(manifest *pkgconfigmanifest.Manifest, query pkgConfigQuery, stdout, stderr io.Writer) int {
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
	manifest, err := pkgconfigmanifest.Read(*manifestFilename)
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
