#include "required_local_header.h"

#ifndef MAP_DIRECTORY_RECIPE_REPLAYED
#error "the mapped action must replay flags from the emitted Kbuild recipe"
#endif

int map_directory_selected(void)
{
	return MAP_DIRECTORY_REQUIRED_HEADER_VALUE;
}
