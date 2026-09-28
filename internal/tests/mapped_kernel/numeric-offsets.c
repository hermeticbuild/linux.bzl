/* A separate actual target compiler keeps the existing asm-offsets sharing
 * fixture unchanged when this producer enters the conservative initial cut. */
#if CONFIG_USED_FAMILY_OPTION
#define MAPPED_NUMERIC_CONFIG 1
#else
#define MAPPED_NUMERIC_CONFIG 0
#endif

void mapped_numeric_offsets(void)
{
	asm volatile("\n.ascii \"->MAPPED_NUMERIC_VALUE %0 used_family_option\"\n"
		     : : "i" (17 + MAPPED_NUMERIC_CONFIG));
}
