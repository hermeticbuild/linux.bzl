int main(void)
{
#ifdef MCOUNT_SORT_ENABLED
	return EXPECT_MCOUNT_SORT != 1;
#else
	return EXPECT_MCOUNT_SORT != 0;
#endif
}
