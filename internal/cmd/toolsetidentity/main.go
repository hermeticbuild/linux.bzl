package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

const toolsetIdentitySchema = toolaction.KbuildToolsetManifestSchema

type toolsetManifest = toolaction.KbuildToolsetManifest

func readManifest(path string) (toolsetManifest, error) {
	return toolaction.ReadKbuildToolsetManifest(path)
}

func validateManifest(manifest toolsetManifest) error {
	return manifest.Validate()
}

func manifestIdentity(manifest toolsetManifest) (string, error) {
	return manifest.Identity()
}

func writeIdentityDirectory(path, identity string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("create identity directory: %w", err)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("read identity directory: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("identity directory %q is not empty", path)
	}
	marker := filepath.Join(path, identity)
	file, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create identity marker: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close identity marker: %w", err)
	}
	return nil
}

func run(manifestPath, outputPath string) error {
	manifest, err := readManifest(manifestPath)
	if err != nil {
		return err
	}
	identity, err := manifestIdentity(manifest)
	if err != nil {
		return err
	}
	return writeIdentityDirectory(outputPath, identity)
}

func main() {
	manifestPath := flag.String("manifest", "", "path to the toolset manifest")
	outputPath := flag.String("out", "", "output TreeArtifact directory")
	flag.Parse()
	if flag.NArg() != 0 || *manifestPath == "" || *outputPath == "" {
		fmt.Fprintln(os.Stderr, "usage: toolsetidentity -manifest FILE -out DIR")
		os.Exit(2)
	}
	if err := run(*manifestPath, *outputPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
