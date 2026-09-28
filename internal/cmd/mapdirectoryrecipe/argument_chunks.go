package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// Unlike Args.use_param_file, these ordinary declared files survive Bazel 9's
// action-template expansion. JSON preserves empty arguments, whitespace and
// literal @ prefixes; imported arguments are never recursively interpreted.
const maxArgumentChunkBytes = 1 << 20

func validateChunkArguments(arguments []string) error {
	if len(arguments) > maxParameterFileArguments {
		return fmt.Errorf("argument chunks exceed %d arguments", maxParameterFileArguments)
	}
	for _, argument := range arguments {
		if strings.ContainsRune(argument, 0) || !utf8.ValidString(argument) || len(argument) > maxParameterFileLineBytes {
			return fmt.Errorf("argument chunk contains NUL, invalid UTF-8 or an oversized argument")
		}
	}
	return nil
}

func writeArgumentChunk(filename string, arguments []string) error {
	if err := validateChunkArguments(arguments); err != nil {
		return err
	}
	// Encode an empty vector as [], not null.
	if arguments == nil {
		arguments = []string{}
	}
	data, err := json.Marshal(arguments)
	if err != nil {
		return err
	}
	if len(data) > maxArgumentChunkBytes {
		return fmt.Errorf("argument chunk exceeds %d bytes", maxArgumentChunkBytes)
	}
	return os.WriteFile(filename, data, 0o644)
}

func readArgumentChunk(filename string) ([]string, int, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxArgumentChunkBytes {
		return nil, 0, fmt.Errorf("argument chunk must be a regular file of at most %d bytes", maxArgumentChunkBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxArgumentChunkBytes+1))
	if err != nil {
		return nil, 0, err
	}
	if len(data) > maxArgumentChunkBytes {
		return nil, 0, fmt.Errorf("argument chunk exceeds %d bytes", maxArgumentChunkBytes)
	}
	var arguments []string
	if err := json.Unmarshal(data, &arguments); err != nil {
		return nil, 0, fmt.Errorf("decode argument chunk: %w", err)
	}
	if arguments == nil {
		return nil, 0, fmt.Errorf("argument chunk must contain an array")
	}
	canonical, err := json.Marshal(arguments)
	if err != nil || !bytes.Equal(canonical, data) {
		return nil, 0, fmt.Errorf("argument chunk must contain canonically encoded strings")
	}
	if err := validateChunkArguments(arguments); err != nil {
		return nil, 0, err
	}
	return arguments, len(data), nil
}

func expandArgumentChunks(arguments []string) ([]string, error) {
	if len(arguments) == 0 || arguments[0] != "-argument_chunks" {
		return expandParameterFileArguments(arguments)
	}
	if len(arguments) < 2 || len(arguments) > maxParameterFileArguments {
		return nil, fmt.Errorf("argument chunk invocation has no files or too many files")
	}
	var expanded []string
	totalBytes := 0
	for _, filename := range arguments[1:] {
		chunk, size, err := readArgumentChunk(filename)
		if err != nil {
			return nil, fmt.Errorf("argument chunk %q: %w", filename, err)
		}
		totalBytes += size
		if totalBytes > maxParameterFileBytes || len(expanded)+len(chunk) > maxParameterFileArguments {
			return nil, fmt.Errorf("argument chunks exceed the total byte or argument limit")
		}
		expanded = append(expanded, chunk...)
	}
	return expanded, nil
}
