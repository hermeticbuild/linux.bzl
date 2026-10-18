/* Test-only action-built compiler entry point; forward every probe unchanged. */
#include <stdio.h>
#include <unistd.h>

int main(int argc, char **argv)
{
    (void)argc;
    argv[0] = REAL_CC;
    execv(REAL_CC, argv);
    perror(REAL_CC);
    return 127;
}
