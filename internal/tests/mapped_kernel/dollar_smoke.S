/* The configured compiler must preprocess the identifier directly after '$'.
 * Pasting discovers the option name; an absent option contributes zero using
 * the same placeholder/second-argument technique as Linux's is-defined macro.
 * Neither the source nor its command selects a compiler-specific dollar mode.
 */
#define MAPPED_DEFINED_1 0,
#define MAPPED_SECOND(first, second, ...) second
#define MAPPED_IS_DEFINED_2(argument) MAPPED_SECOND(argument 1, 0, 0)
#define MAPPED_IS_DEFINED_1(value) MAPPED_IS_DEFINED_2(MAPPED_DEFINED_ ## value)
#define MAPPED_IS_DEFINED(value) MAPPED_IS_DEFINED_1(value)
#define MAPPED_DOLLAR_CONFIG(name) (113 + MAPPED_IS_DEFINED(CONFIG_ ## name))

.text
.globl mapped_dollar_config_value
.type mapped_dollar_config_value, @function
mapped_dollar_config_value:
	movl $MAPPED_DOLLAR_CONFIG(USED_FAMILY_OPTION), %eax
	ret
.size mapped_dollar_config_value, .-mapped_dollar_config_value

.section .note.GNU-stack,"",@progbits
