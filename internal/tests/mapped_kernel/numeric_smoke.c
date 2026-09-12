#include <generated/mapped_numeric_offsets.h>

/* The real compiler receives -U__MAPPED_NUMERIC_ABSENT. A virtual numeric
 * summary loses that absence; the executed header must restore exact bytes. */
#if defined(__MAPPED_NUMERIC_ABSENT)
#define MAPPED_NUMERIC_ABSENT_BRANCH 1
#else
#define MAPPED_NUMERIC_ABSENT_BRANCH 0
#endif

#define MAPPED_NUMERIC_CONFIG(name) CONFIG_ ## name
#if MAPPED_NUMERIC_CONFIG(USED_FAMILY_OPTION)
#define MAPPED_NUMERIC_PASTED_BRANCH 1
#else
#define MAPPED_NUMERIC_PASTED_BRANCH 0
#endif

/* Nonzero initializers keep every witness in file-backed ELF data. */
const unsigned char mapped_numeric_header_value = MAPPED_NUMERIC_VALUE;
const unsigned char mapped_numeric_absence_value = 1 + MAPPED_NUMERIC_ABSENT_BRANCH;
const unsigned char mapped_numeric_pasted_value = 1 + MAPPED_NUMERIC_PASTED_BRANCH;
