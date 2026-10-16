/* Minimal x86-64 PID 1 for the QEMU smoke test. No libc or root filesystem. */
.section .text
.global _start
_start:
    mov $1, %eax
    mov $1, %edi
    lea message(%rip), %rsi
    mov $(message_end-message), %edx
    syscall
    mov $169, %eax
    mov $0xfee1dead, %edi
    mov $0x28121969, %esi
    mov $0x01234567, %edx
    syscall
1:  jmp 1b
.section .rodata
message: .ascii "BAZEL_KERNEL_BOOT_OK\n"
message_end:
.section .note.GNU-stack,"",@progbits
