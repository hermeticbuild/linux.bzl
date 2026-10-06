/* Minimal stand-in for lib/raid6/int.uc: one repeated line and one $# use. */
#include <linux/raid/pq.h>

void source_versions_unroll_$#(void)
{
	unsigned long v$$ = $$;
	(void)v$$;
}
