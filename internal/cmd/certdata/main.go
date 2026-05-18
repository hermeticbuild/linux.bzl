// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	config := flag.String("config", "", "Resolved Linux .config input")
	signingKeyOut := flag.String("signing_key_out", "", "Generated certs/signing_key.x509 output")
	certListOut := flag.String("cert_list_out", "", "Generated certs/x509_certificate_list output")
	flag.Parse()

	if *config == "" || *signingKeyOut == "" || *certListOut == "" {
		fmt.Fprintln(os.Stderr, "-config, -signing_key_out and -cert_list_out are required")
		os.Exit(2)
	}
	if err := run(*config, *signingKeyOut, *certListOut); err != nil {
		fmt.Fprintf(os.Stderr, "certdata: %v\n", err)
		os.Exit(1)
	}
}

func run(config, signingKeyOut, certListOut string) error {
	values, err := readConfig(config)
	if err != nil {
		return err
	}
	if key := unquote(values["CONFIG_MODULE_SIG_KEY"]); key != "" {
		return fmt.Errorf("CONFIG_MODULE_SIG_KEY=%q requires X.509 extraction support before certs/signing_key.x509 can be generated", key)
	}
	if keys := unquote(values["CONFIG_SYSTEM_TRUSTED_KEYS"]); keys != "" {
		return fmt.Errorf("CONFIG_SYSTEM_TRUSTED_KEYS=%q requires X.509 extraction support before certs/x509_certificate_list can be generated", keys)
	}

	if err := writeEmpty(signingKeyOut); err != nil {
		return err
	}
	return writeEmpty(certListOut)
}

func readConfig(path string) (map[string]string, error) {
	input, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer input.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return values, nil
}

func unquote(value string) string {
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return value[1 : len(value)-1]
	}
	return value
}

func writeEmpty(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, nil, 0o644)
}
