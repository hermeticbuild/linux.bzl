/* The first include stops initial source replay before the later one is entered.
 * Candidate lookahead may request names, but only real compiler answers permit
 * final replay and configuration sharing. */
#include "include/linux/mapped_literal_first.h"
#include "include/linux/mapped_literal_later.h"
#define LITERAL_OPTION(name) CONFIG_ ## name
#if LITERAL_OPTION(USED_FAMILY_OPTION)
#define LITERAL_SELECTED 1
#else
#define LITERAL_SELECTED 0
#endif
const unsigned char mapped_literal_hint_value =
	MAPPED_LITERAL_FIRST_VALUE + MAPPED_LITERAL_LATER_VALUE + LITERAL_SELECTED;

/* Match the GNU named-variadic/stringification form used by Linux without
 * granting any macro a special spelling-based exception in the scanner. */
#define MAPPED_STRINGIFY_RAW(args...) #args
#define MAPPED_STRINGIFY(args...) MAPPED_STRINGIFY_RAW(args)
const char mapped_stringified_config[] = MAPPED_STRINGIFY(LITERAL_SELECTED);
const char mapped_stringified_raw[] = MAPPED_STRINGIFY_RAW(CONFIG_UNUSED_FAMILY_OPTION);
const char mapped_stringified_spacing[] = MAPPED_STRINGIFY_RAW(a+b, c);
const char mapped_stringified_escaped[] = MAPPED_STRINGIFY_RAW("a\\b");
const char mapped_stringified_empty[] = MAPPED_STRINGIFY_RAW();
