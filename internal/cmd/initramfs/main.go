package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
)

const (
	modeDirectory       = uint32(0o040000 | 0o755)
	modeFile            = uint32(0o100000 | 0o644)
	modeExecutable      = uint32(0o100000 | 0o755)
	modeSymlink         = uint32(0o120000 | 0o777)
	modeCharacterDevice = uint32(0o020000 | 0o666)
	maxUint32           = 1<<32 - 1
)

const usage = "usage: initramfs OUTPUT " +
	"[--directory PATH] [--file PATH SOURCE] " +
	"[--executable PATH SOURCE] [--symlink PATH TARGET] " +
	"[--character-device PATH MAJOR MINOR]"

type archiveWriter struct {
	output     *os.File
	outputPath string
	offset     uint64
	inode      uint32
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "initramfs: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	outputPath := args[0]
	output, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("could not open output: %s", outputPath)
	}

	success := false
	defer func() {
		if !success {
			_ = output.Close()
			_ = os.Remove(outputPath)
		}
	}()

	writer := archiveWriter{
		output:     output,
		outputPath: outputPath,
		inode:      1,
	}
	if err := writer.writeEntries(args[1:]); err != nil {
		return err
	}
	if err := writer.writeHeader(0, 1, 0, 0, 0, "TRAILER!!!"); err != nil {
		return err
	}
	if err := writer.writePadding(512); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("could not close output: %s", outputPath)
	}
	success = true
	return nil
}

func (w *archiveWriter) writeEntries(args []string) error {
	for len(args) != 0 {
		option := args[0]
		args = args[1:]
		switch option {
		case "--directory":
			if len(args) < 1 {
				return fmt.Errorf("invalid arguments: %s", option)
			}
			if err := w.writeHeader(modeDirectory, 2, 0, 0, 0, args[0]); err != nil {
				return err
			}
			args = args[1:]
		case "--file", "--executable":
			if len(args) < 2 {
				return fmt.Errorf("invalid arguments: %s", option)
			}
			mode := modeFile
			if option == "--executable" {
				mode = modeExecutable
			}
			if err := w.writeRegularFile(args[0], args[1], mode); err != nil {
				return err
			}
			args = args[2:]
		case "--symlink":
			if len(args) < 2 {
				return fmt.Errorf("invalid arguments: %s", option)
			}
			if err := w.writeSymlink(args[0], args[1]); err != nil {
				return err
			}
			args = args[2:]
		case "--character-device":
			if len(args) < 3 {
				return fmt.Errorf("invalid arguments: %s", option)
			}
			major, err := parseUint32(args[1])
			if err != nil {
				return err
			}
			minor, err := parseUint32(args[2])
			if err != nil {
				return err
			}
			if err := w.writeHeader(modeCharacterDevice, 1, 0, major, minor, args[0]); err != nil {
				return err
			}
			args = args[3:]
		default:
			return fmt.Errorf("invalid arguments: %s", option)
		}
	}
	return nil
}

func (w *archiveWriter) writeRegularFile(name, path string, mode uint32) error {
	source, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("could not open input: %s", path)
	}
	closed := false
	defer func() {
		if !closed {
			_ = source.Close()
		}
	}()

	end, err := source.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("could not seek input: %s", path)
	}
	if end < 0 {
		return fmt.Errorf("could not measure input: %s", path)
	}
	if end > maxUint32 {
		return fmt.Errorf("input is too large for newc: %s", path)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("could not rewind input: %s", path)
	}
	if err := w.writeHeader(mode, 1, uint32(end), 0, 0, name); err != nil {
		return err
	}

	buffer := make([]byte, 64*1024)
	for {
		count, readErr := source.Read(buffer)
		if count != 0 {
			if err := w.writeBytes(buffer[:count]); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("could not read input: %s", path)
		}
	}
	if err := source.Close(); err != nil {
		return fmt.Errorf("could not close input: %s", path)
	}
	closed = true
	return w.writePadding(4)
}

func (w *archiveWriter) writeSymlink(name, target string) error {
	if uint64(len(target)) > maxUint32 {
		return fmt.Errorf("symbolic link target is too long: %s", name)
	}
	if err := w.writeHeader(modeSymlink, 1, uint32(len(target)), 0, 0, name); err != nil {
		return err
	}
	if err := w.writeBytes([]byte(target)); err != nil {
		return err
	}
	return w.writePadding(4)
}

func (w *archiveWriter) writeHeader(mode, nlink, fileSize, rdevMajor, rdevMinor uint32, name string) error {
	if uint64(len(name))+1 > maxUint32 {
		return fmt.Errorf("archive path is too long: %s", name)
	}
	fields := [...]uint32{
		w.inode,
		mode,
		0,
		0,
		nlink,
		0,
		fileSize,
		0,
		0,
		rdevMajor,
		rdevMinor,
		uint32(len(name) + 1),
		0,
	}
	w.inode++

	if err := w.writeBytes([]byte("070701")); err != nil {
		return err
	}
	for _, field := range fields {
		if err := w.writeBytes([]byte(fmt.Sprintf("%08x", field))); err != nil {
			return err
		}
	}
	if err := w.writeBytes([]byte(name)); err != nil {
		return err
	}
	if err := w.writeBytes([]byte{0}); err != nil {
		return err
	}
	return w.writePadding(4)
}

func (w *archiveWriter) writePadding(alignment uint64) error {
	size := (alignment - w.offset%alignment) % alignment
	if size == 0 {
		return nil
	}
	return w.writeBytes(make([]byte, size))
}

func (w *archiveWriter) writeBytes(data []byte) error {
	count, err := w.output.Write(data)
	w.offset += uint64(count)
	if err != nil || count != len(data) {
		return fmt.Errorf("could not write output: %s", w.outputPath)
	}
	return nil
}

func parseUint32(text string) (uint32, error) {
	value, err := strconv.ParseUint(text, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid device number: %s", text)
	}
	return uint32(value), nil
}
