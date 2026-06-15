#include <errno.h>
#include <inttypes.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>

struct archive_writer {
  FILE *output;
  const char *output_path;
  uint64_t offset;
  uint32_t inode;
};

static const uint32_t MODE_DIRECTORY = 0040000 | 0755;
static const uint32_t MODE_FILE = 0100000 | 0644;
static const uint32_t MODE_EXECUTABLE = 0100000 | 0755;
static const uint32_t MODE_SYMLINK = 0120000 | 0777;
static const uint32_t MODE_CHARACTER_DEVICE = 0020000 | 0666;

static int fail(const char *message, const char *value) {
  if (value)
    fprintf(stderr, "initramfs: %s: %s\n", message, value);
  else
    fprintf(stderr, "initramfs: %s\n", message);
  return 0;
}

static int write_bytes(struct archive_writer *writer, const void *data,
                       size_t size) {
  if (size && fwrite(data, 1, size, writer->output) != size)
    return fail("could not write output", writer->output_path);
  writer->offset += size;
  return 1;
}

static int write_padding(struct archive_writer *writer, uint32_t alignment) {
  static const unsigned char zeros[512];
  size_t size = (size_t)((alignment - writer->offset % alignment) % alignment);
  return write_bytes(writer, zeros, size);
}

static int write_hex(struct archive_writer *writer, uint32_t value) {
  char field[9];
  if (snprintf(field, sizeof(field), "%08" PRIx32, value) != 8)
    return fail("could not format newc header", NULL);
  return write_bytes(writer, field, 8);
}

static int write_header(struct archive_writer *writer, uint32_t mode,
                        uint32_t nlink, uint32_t file_size, uint32_t rdev_major,
                        uint32_t rdev_minor, const char *name) {
  size_t name_size = strlen(name) + 1;
  size_t i;
  if (name_size > UINT32_MAX)
    return fail("archive path is too long", name);
  uint32_t fields[] = {
      writer->inode++,
      mode,
      0,
      0,
      nlink,
      0,
      file_size,
      0,
      0,
      rdev_major,
      rdev_minor,
      (uint32_t)name_size,
      0,
  };
  if (!write_bytes(writer, "070701", 6))
    return 0;
  for (i = 0; i < sizeof(fields) / sizeof(fields[0]); ++i) {
    if (!write_hex(writer, fields[i]))
      return 0;
  }
  return write_bytes(writer, name, name_size) && write_padding(writer, 4);
}

static int source_file_size(FILE *source, const char *path, uint32_t *size) {
  long end;
  if (fseek(source, 0, SEEK_END) != 0)
    return fail("could not seek input", path);
  end = ftell(source);
  if (end < 0)
    return fail("could not measure input", path);
  if ((uintmax_t)end > UINT32_MAX)
    return fail("input is too large for newc", path);
  if (fseek(source, 0, SEEK_SET) != 0)
    return fail("could not rewind input", path);
  *size = (uint32_t)end;
  return 1;
}

static int write_regular_file(struct archive_writer *writer, const char *name,
                              const char *path, uint32_t mode) {
  unsigned char buffer[64 * 1024];
  uint32_t size;
  FILE *source = fopen(path, "rb");
  size_t count;
  int success = 1;
  if (!source)
    return fail("could not open input", path);

  success = source_file_size(source, path, &size) &&
            write_header(writer, mode, 1, size, 0, 0, name);

  while (success && (count = fread(buffer, 1, sizeof(buffer), source)) != 0) {
    success = write_bytes(writer, buffer, count);
  }
  if (success && ferror(source))
    success = fail("could not read input", path);
  if (fclose(source) != 0 && success)
    success = fail("could not close input", path);
  return success && write_padding(writer, 4);
}

static int write_symlink(struct archive_writer *writer, const char *name,
                         const char *target) {
  size_t size = strlen(target);
  if (size > UINT32_MAX)
    return fail("symbolic link target is too long", name);
  return write_header(writer, MODE_SYMLINK, 1, (uint32_t)size, 0, 0, name) &&
         write_bytes(writer, target, size) && write_padding(writer, 4);
}

static int parse_u32(const char *text, uint32_t *value) {
  char *end;
  uintmax_t parsed;
  errno = 0;
  parsed = strtoumax(text, &end, 10);
  if (errno || *text == '\0' || *end != '\0' || parsed > UINT32_MAX)
    return fail("invalid device number", text);
  *value = (uint32_t)parsed;
  return 1;
}

static int write_entries(struct archive_writer *writer, int argc, char **argv) {
  int i;
  for (i = 2; i < argc;) {
    const char *option = argv[i++];
    if (strcmp(option, "--directory") == 0 && i < argc) {
      if (!write_header(writer, MODE_DIRECTORY, 2, 0, 0, 0, argv[i++]))
        return 0;
    } else if (strcmp(option, "--file") == 0 && i + 1 < argc) {
      const char *name = argv[i++];
      if (!write_regular_file(writer, name, argv[i++], MODE_FILE))
        return 0;
    } else if (strcmp(option, "--executable") == 0 && i + 1 < argc) {
      const char *name = argv[i++];
      if (!write_regular_file(writer, name, argv[i++], MODE_EXECUTABLE))
        return 0;
    } else if (strcmp(option, "--symlink") == 0 && i + 1 < argc) {
      const char *name = argv[i++];
      if (!write_symlink(writer, name, argv[i++]))
        return 0;
    } else if (strcmp(option, "--character-device") == 0 && i + 2 < argc) {
      const char *name = argv[i++];
      uint32_t major;
      uint32_t minor;
      if (!parse_u32(argv[i++], &major) || !parse_u32(argv[i++], &minor) ||
          !write_header(writer, MODE_CHARACTER_DEVICE, 1, 0, major, minor,
                        name))
        return 0;
    } else {
      return fail("invalid arguments", option);
    }
  }
  return 1;
}

static int usage(void) {
  fputs("usage: initramfs OUTPUT "
        "[--directory PATH] [--file PATH SOURCE] "
        "[--executable PATH SOURCE] [--symlink PATH TARGET] "
        "[--character-device PATH MAJOR MINOR]\n",
        stderr);
  return 2;
}

int main(int argc, char **argv) {
  int success;
  if (argc < 2)
    return usage();
  struct archive_writer writer = {
      .output = fopen(argv[1], "wb"),
      .output_path = argv[1],
      .inode = 1,
  };
  if (!writer.output) {
    fail("could not open output", argv[1]);
    return 1;
  }

  success = write_entries(&writer, argc, argv) &&
            write_header(&writer, 0, 1, 0, 0, 0, "TRAILER!!!") &&
            write_padding(&writer, 512);
  if (fclose(writer.output) != 0 && success)
    success = fail("could not close output", argv[1]);
  if (!success)
    remove(argv[1]);
  return success ? 0 : 1;
}
