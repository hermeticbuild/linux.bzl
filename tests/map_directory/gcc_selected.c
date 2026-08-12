#ifndef MAP_DIRECTORY_TOOLCHAIN_PREFIX_REPLAYED
#error "the configured GCC toolchain prefix was not replayed"
#endif

#ifndef MAP_DIRECTORY_GCC_RECIPE_REPLAYED
#error "the GCC-selected mapped action must replay its emitted Kbuild flags"
#endif

int map_directory_gcc_selected(void)
{
	return 15;
}
