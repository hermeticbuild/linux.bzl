// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	cpioModeDir   = 0o040000
	cpioModeFile  = 0o100000
	cpioModeChar  = 0o020000
	cpioModeBlock = 0o060000
	cpioModeLink  = 0o120000
)

type entry struct {
	name      string
	body      []byte
	mode      int64
	uid       int64
	gid       int64
	nlink     int64
	rdevMajor int64
	rdevMinor int64
}

func main() {
	in := flag.String("in", "", "gen_init_cpio input list")
	out := flag.String("out", "", "Generated initramfs_inc_data output")
	flag.Parse()

	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "-in and -out are required")
		os.Exit(2)
	}
	if err := run(*in, *out); err != nil {
		fmt.Fprintf(os.Stderr, "initramfsdata: %v\n", err)
		os.Exit(1)
	}
}

func run(in, out string) error {
	entries, err := parseList(in)
	if err != nil {
		return err
	}
	entries = append(entries, entry{name: "TRAILER!!!", mode: 0, nlink: 1})

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	file, err := os.Create(out)
	if err != nil {
		return err
	}
	defer file.Close()

	for i, entry := range entries {
		if err := writeNewc(file, int64(i+1), entry); err != nil {
			return err
		}
	}
	if err := writePaddingTo(file, 512); err != nil {
		return err
	}
	return nil
}

func parseList(path string) ([]entry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var entries []entry
	scanner := bufio.NewScanner(file)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		switch fields[0] {
		case "dir":
			if len(fields) != 5 {
				return nil, fmt.Errorf("%s:%d: dir expects 4 arguments", path, lineNo)
			}
			mode, uid, gid, err := parseModeUIDGID(fields[2], fields[3], fields[4])
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			entries = append(entries, entry{name: archiveName(fields[1]), mode: cpioModeDir | mode, uid: uid, gid: gid, nlink: 2})
		case "nod":
			if len(fields) != 8 {
				return nil, fmt.Errorf("%s:%d: nod expects 7 arguments", path, lineNo)
			}
			mode, uid, gid, err := parseModeUIDGID(fields[2], fields[3], fields[4])
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			nodeType, err := nodeMode(fields[5])
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			major, err := strconv.ParseInt(fields[6], 0, 64)
			if err != nil {
				return nil, fmt.Errorf("%s:%d: invalid device major %q", path, lineNo, fields[6])
			}
			minor, err := strconv.ParseInt(fields[7], 0, 64)
			if err != nil {
				return nil, fmt.Errorf("%s:%d: invalid device minor %q", path, lineNo, fields[7])
			}
			entries = append(entries, entry{name: archiveName(fields[1]), mode: nodeType | mode, uid: uid, gid: gid, nlink: 1, rdevMajor: major, rdevMinor: minor})
		case "slink":
			if len(fields) != 6 {
				return nil, fmt.Errorf("%s:%d: slink expects 5 arguments", path, lineNo)
			}
			mode, uid, gid, err := parseModeUIDGID(fields[3], fields[4], fields[5])
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			entries = append(entries, entry{name: archiveName(fields[1]), body: []byte(fields[2]), mode: cpioModeLink | mode, uid: uid, gid: gid, nlink: 1})
		case "file":
			if len(fields) != 6 {
				return nil, fmt.Errorf("%s:%d: file expects 5 arguments", path, lineNo)
			}
			mode, uid, gid, err := parseModeUIDGID(fields[3], fields[4], fields[5])
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			body, err := os.ReadFile(resolveListPath(filepath.Dir(path), fields[2]))
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			entries = append(entries, entry{name: archiveName(fields[1]), body: body, mode: cpioModeFile | mode, uid: uid, gid: gid, nlink: 1})
		default:
			return nil, fmt.Errorf("%s:%d: unsupported initramfs entry type %q", path, lineNo, fields[0])
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return entries, nil
}

func parseModeUIDGID(modeText, uidText, gidText string) (int64, int64, int64, error) {
	mode, err := strconv.ParseInt(modeText, 8, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid mode %q", modeText)
	}
	uid, err := strconv.ParseInt(uidText, 0, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid uid %q", uidText)
	}
	gid, err := strconv.ParseInt(gidText, 0, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid gid %q", gidText)
	}
	return mode, uid, gid, nil
}

func nodeMode(kind string) (int64, error) {
	switch kind {
	case "c":
		return cpioModeChar, nil
	case "b":
		return cpioModeBlock, nil
	default:
		return 0, fmt.Errorf("unsupported device node type %q", kind)
	}
}

func archiveName(name string) string {
	name = filepath.ToSlash(name)
	name = strings.TrimPrefix(name, "/")
	if name == "" {
		return "."
	}
	return name
}

func resolveListPath(base, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}

func writeNewc(w io.Writer, ino int64, entry entry) error {
	name := []byte(entry.name)
	header := fmt.Sprintf(
		"070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		ino,
		entry.mode,
		entry.uid,
		entry.gid,
		entry.nlink,
		0,
		len(entry.body),
		0,
		0,
		entry.rdevMajor,
		entry.rdevMinor,
		len(name)+1,
		0,
	)
	if _, err := io.WriteString(w, strings.ToLower(header)); err != nil {
		return err
	}
	if _, err := w.Write(name); err != nil {
		return err
	}
	if _, err := w.Write([]byte{0}); err != nil {
		return err
	}
	if err := writePadding(w, 110+len(name)+1); err != nil {
		return err
	}
	if len(entry.body) != 0 {
		if _, err := w.Write(entry.body); err != nil {
			return err
		}
		if err := writePadding(w, len(entry.body)); err != nil {
			return err
		}
	}
	return nil
}

func writePadding(w io.Writer, size int) error {
	padding := (4 - (size % 4)) % 4
	if padding == 0 {
		return nil
	}
	_, err := w.Write(make([]byte, padding))
	return err
}

func writePaddingTo(w io.Writer, align int) error {
	if align == 0 {
		return nil
	}
	if seeker, ok := w.(interface {
		Seek(offset int64, whence int) (int64, error)
	}); ok {
		off, err := seeker.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		padding := (align - int(off)%align) % align
		if padding == 0 {
			return nil
		}
		_, err = w.Write(make([]byte, padding))
		return err
	}
	return fmt.Errorf("writer does not support seeking")
}
