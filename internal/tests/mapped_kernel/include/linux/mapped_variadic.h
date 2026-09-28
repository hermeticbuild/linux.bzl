/* This literal include comes after the earlier guard-dependent headers in
 * smoke.c. Grammar discovery must not wait for ordered replay to reach it.
 * The independent compiler reference keeps its own definitions. */
#define MAPPED_ARG_COUNT(_0, _1, _2, _3, _4, _5, _6, _7, _8, _9, _10, _11, _12, _13, _14, _15, N, ...) N
#define MAPPED_COUNT_ARGS(...) MAPPED_ARG_COUNT(0, ##__VA_ARGS__, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1, 0)
#define MAPPED_NAMED_COUNT(args...) MAPPED_ARG_COUNT(0, ##args, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1, 0)
