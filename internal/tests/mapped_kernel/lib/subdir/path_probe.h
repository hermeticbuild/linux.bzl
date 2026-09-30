#ifndef MAPPED_KERNEL_PATH_PROBE_H
#define MAPPED_KERNEL_PATH_PROBE_H

/* Capture at the header definition site, not at a later macro expansion. */
static const char mapped_header_file[] = __FILE__;
static const char mapped_header_base_file[] = __BASE_FILE__;

/* Keep an actual header code location even when macro-string checks fold. */
static __attribute__((noinline)) int mapped_header_line_probe(volatile int *value)
{
	return *value;
}

#endif
