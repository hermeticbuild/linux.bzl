#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

struct expected_entry {
  const char *name;
  uint32_t mode;
  uint32_t nlink;
  const char *body;
  uint32_t rdev_major;
  uint32_t rdev_minor;
};

static const struct expected_entry expected[] = {
    {"bin", 0040755, 2, "", 0, 0},
    {"dev", 0040755, 2, "", 0, 0},
    {"empty", 0040755, 2, "", 0, 0},
    {"empty/dir", 0040755, 2, "", 0, 0},
    {"etc", 0040755, 2, "", 0, 0},
    {"usr", 0040755, 2, "", 0, 0},
    {"usr/bin", 0040755, 2, "", 0, 0},
    {"etc/config", 0100644, 1, "config\n", 0, 0},
    {"bin/tool", 0100755, 1, "tool\n", 0, 0},
    {"init", 0100755, 1, "tool\n", 0, 0},
    {"usr/bin/tool", 0100755, 1, "tool\n", 0, 0},
    {"bin/config", 0120777, 1, "/etc/config", 0, 0},
    {"dev/null", 0020666, 1, "", 1, 3},
    {"TRAILER!!!", 0, 1, "", 0, 0},
};

static int fail(const char *message, const char *name) {
  fprintf(stderr, "initramfs_test: %s: %s\n", message, name);
  return 0;
}

static int read_exact(FILE *file, void *data, size_t size) {
  return fread(data, 1, size, file) == size;
}

static int read_hex(const char *text, uint32_t *value) {
  char field[9];
  char *end;
  unsigned long parsed;
  memcpy(field, text, 8);
  field[8] = '\0';
  parsed = strtoul(field, &end, 16);
  if (*end != '\0' || parsed > UINT32_MAX)
    return 0;
  *value = (uint32_t)parsed;
  return 1;
}

static int skip_padding(FILE *file, uint64_t *offset, uint32_t alignment) {
  unsigned char padding[512];
  size_t size = (size_t)((alignment - *offset % alignment) % alignment);
  size_t i;
  if (!read_exact(file, padding, size))
    return 0;
  for (i = 0; i < size; ++i) {
    if (padding[i] != 0)
      return 0;
  }
  *offset += size;
  return 1;
}

static int check_entry(FILE *file, uint64_t *offset, size_t index) {
  const struct expected_entry *want = &expected[index];
  char header[110];
  uint32_t fields[13];
  uint32_t name_size;
  uint32_t body_size;
  char *name;
  char *body;
  size_t i;

  if (!read_exact(file, header, sizeof(header)) ||
      memcmp(header, "070701", 6) != 0)
    return fail("invalid newc header", want->name);
  *offset += sizeof(header);
  for (i = 0; i < 13; ++i) {
    if (!read_hex(header + 6 + i * 8, &fields[i]))
      return fail("invalid newc field", want->name);
  }
  name_size = fields[11];
  body_size = fields[6];
  name = malloc(name_size ? name_size : 1);
  body = malloc(body_size ? body_size : 1);
  if (!name || !body)
    return fail("out of memory", want->name);
  if (!name_size || !read_exact(file, name, name_size) ||
      name[name_size - 1] != '\0')
    return fail("invalid entry name", want->name);
  *offset += name_size;
  if (!skip_padding(file, offset, 4) ||
      (body_size && !read_exact(file, body, body_size)))
    return fail("truncated entry", want->name);
  *offset += body_size;
  if (!skip_padding(file, offset, 4))
    return fail("invalid entry padding", want->name);

  if (name_size != strlen(want->name) + 1 || strcmp(name, want->name) != 0 ||
      fields[0] != (uint32_t)index + 1 || fields[1] != want->mode ||
      fields[2] != 0 || fields[3] != 0 || fields[4] != want->nlink ||
      fields[5] != 0 || fields[7] != 0 || fields[8] != 0 ||
      fields[9] != want->rdev_major || fields[10] != want->rdev_minor ||
      fields[12] != 0 || body_size != strlen(want->body) ||
      memcmp(body, want->body, body_size) != 0) {
    free(name);
    free(body);
    return fail("entry does not match", want->name);
  }
  free(name);
  free(body);
  return 1;
}

static int compare_archives(const char *first_path, const char *second_path) {
  FILE *first = fopen(first_path, "rb");
  FILE *second = fopen(second_path, "rb");
  int first_byte;
  int second_byte;
  if (!first || !second) {
    if (first)
      fclose(first);
    if (second)
      fclose(second);
    return fail("could not open archive for comparison", second_path);
  }
  do {
    first_byte = fgetc(first);
    second_byte = fgetc(second);
  } while (first_byte != EOF && first_byte == second_byte);
  if (ferror(first) || ferror(second) || first_byte != second_byte) {
    fclose(first);
    fclose(second);
    return fail("reordered inputs produced different archives", second_path);
  }
  if (fclose(first) != 0 || fclose(second) != 0)
    return fail("could not close compared archives", second_path);
  return 1;
}

int main(int argc, char **argv) {
  FILE *file;
  uint64_t offset = 0;
  size_t i;
  int byte;
  if (argc != 3) {
    fail("expected two archive paths", "argv");
    return 1;
  }
  file = fopen(argv[1], "rb");
  if (!file) {
    fail("could not open archive", argv[1]);
    return 1;
  }
  for (i = 0; i < sizeof(expected) / sizeof(expected[0]); ++i) {
    if (!check_entry(file, &offset, i)) {
      fclose(file);
      return 1;
    }
  }
  while ((byte = fgetc(file)) != EOF) {
    if (byte != 0) {
      fclose(file);
      fail("nonzero final padding", argv[1]);
      return 1;
    }
    ++offset;
  }
  if (ferror(file) || offset % 512 != 0) {
    fclose(file);
    fail("invalid archive size", argv[1]);
    return 1;
  }
  if (fclose(file) != 0) {
    fail("could not close archive", argv[1]);
    return 1;
  }
  return compare_archives(argv[1], argv[2]) ? 0 : 1;
}
