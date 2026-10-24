/* Test-only action-built compiler entry point. */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <limits.h>
#include <unistd.h>

int main(int argc, char **argv)
{
    /* Distinguish the selected compiler's Kconfig probes from default Clang. */
    for (int i = 1; i < argc; i++) {
        if (strcmp(argv[i], "-ftrivial-auto-var-init=pattern") == 0)
            return 1;
    }
    char path[PATH_MAX];
    const char *root = getenv("KBUILD_EXEC_ROOT");
    const char *compiler = REAL_CC;
    if (compiler[0] != '/') {
        int length;
        if (!root) {
            fputs("KBUILD_EXEC_ROOT is required for the declared compiler\n", stderr);
            return 127;
        }
        length = snprintf(path, sizeof(path), "%s/%s", root, compiler);
        if (length < 0 || (size_t)length >= sizeof(path))
            return 127;
        compiler = path;
    }
    argv[0] = (char *)compiler;
    execv(compiler, argv);
    perror(REAL_CC);
    return 127;
}
