/* Isolate the optional header guards from platform fallback detection. */
#define Z_SOLO
#undef _LARGEFILE64_SOURCE
#include <zconf.h>

#if defined(Z_HAVE_UNISTD_H) != EXPECT_HEADERS
#error "unexpected unistd.h selection"
#endif
#if defined(Z_HAVE_STDARG_H) != EXPECT_HEADERS
#error "unexpected stdarg.h selection"
#endif

int zlib_header_guards(void) { return 0; }
