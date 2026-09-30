/* A small fixture implementation of fixdep's depfile/target/command protocol.
 * It validates actual compiler outputs and emits a command record. It is not a
 * substitute for the kernel's dependency scanner or a production tool policy.
 */
#include <stdio.h>
#include <string.h>

int main(int argc, char **argv)
{
	FILE *input;
	unsigned char magic[4];
	int ch, saw_colon = 0;

	if (argc != 4 || !argv[1][0] || !argv[2][0] || !argv[3][0])
		return 2;
	input = fopen(argv[1], "rb");
	if (!input)
		return 3;
	while ((ch = fgetc(input)) != EOF)
		if (ch == ':')
			saw_colon = 1;
	if (ferror(input) || fclose(input) || !saw_colon)
		return 4;
	input = fopen(argv[2], "rb");
	if (!input)
		return 5;
	if (fread(magic, 1, sizeof(magic), input) != sizeof(magic) ||
	    memcmp(magic, "\177ELF", sizeof(magic)) || fclose(input))
		return 6;
	if (puts("# mapped-executed-helper:elf-depfile") < 0 ||
	    printf("savedcmd_%s := %s\n", argv[2], argv[3]) < 0)
		return 7;
	return 0;
}
