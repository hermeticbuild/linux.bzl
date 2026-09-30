package pkgconfigmanifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

const Schema = "linux.bzl/pkg-config-manifest/v1"

const (
	MaxPackages      = 256
	maxFlags         = 4096
	maxManifestBytes = 1 << 20
)

type Package struct {
	CFlags []string `json:"cflags"`
	Libs   []string `json:"libs"`
}

// Manifest is the immutable host package contract shared by the configured
// shim and source planner. Its package membership decides --exists exactly;
// compile and link words still flow through the shim's measured action.
type Manifest struct {
	Schema          string             `json:"schema"`
	Packages        map[string]Package `json:"packages"`
	contentIdentity string
}

func (m *Manifest) ContentIdentity() string {
	if m == nil {
		return ""
	}
	return m.contentIdentity
}

func Read(filename string) (*Manifest, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect manifest: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxManifestBytes {
		return nil, fmt.Errorf("manifest is not a regular file or exceeds %d bytes", maxManifestBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	if len(data) > maxManifestBytes {
		return nil, fmt.Errorf("manifest exceeds %d bytes", maxManifestBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("decode manifest: trailing JSON value")
		}
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := validate(manifest); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	manifest.contentIdentity = "sha256-" + hex.EncodeToString(digest[:])
	return &manifest, nil
}

func validate(manifest Manifest) error {
	if manifest.Schema != Schema {
		return fmt.Errorf("manifest schema = %q, want %q", manifest.Schema, Schema)
	}
	if manifest.Packages == nil {
		return fmt.Errorf("manifest packages are required")
	}
	if len(manifest.Packages) > MaxPackages {
		return fmt.Errorf("manifest contains more than %d packages", MaxPackages)
	}
	names := make([]string, 0, len(manifest.Packages))
	for name := range manifest.Packages {
		names = append(names, name)
	}
	sort.Strings(names)
	flags := 0
	for _, name := range names {
		if !ValidPackageName(name) {
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
			if flags > maxFlags {
				return fmt.Errorf("manifest contains more than %d flags", maxFlags)
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

func ValidPackageName(value string) bool {
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
