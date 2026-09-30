#include "include/linux/mapped_pending_hints.h"
#define PENDING_OPTION(name) CONFIG_ ## name
#if PENDING_OPTION(USED_FAMILY_OPTION)
#define PENDING_SELECTED 1
#else
#define PENDING_SELECTED 0
#endif
const unsigned char mapped_pending_left_value = 40 + MAPPED_PENDING_CONTEXT +
	__MAPPED_PENDING_A + __MAPPED_PENDING_B + __MAPPED_PENDING_C +
	__MAPPED_PENDING_D + __MAPPED_PENDING_E + __MAPPED_PENDING_F +
	PENDING_SELECTED;
