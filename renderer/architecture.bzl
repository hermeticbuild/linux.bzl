"""Linux architecture inputs resolved from the configured Bazel platform."""

load(":architecture_data.bzl", "CPU_ARCHITECTURES")

KernelArchitectureInfo = provider(doc = "Kernel Architecture Info.", fields = ["cpu", "arch", "srcarch", "bits", "subarch", "header_arch"])

def _impl(ctx):
    matches = [(target, cpu) for target, cpu in ctx.attr.cpus.items() if ctx.target_platform_has_constraint(target[platform_common.ConstraintValueInfo])]
    if len(matches) != 1:
        fail("Kernel requires one supported CPU constraint on the selected Bazel platform; found %s" % [cpu for _, cpu in matches])
    cpu = matches[0][1]
    arch, srcarch, bits = CPU_ARCHITECTURES[cpu]
    header_arch = srcarch
    if ctx.target_platform_has_constraint(ctx.attr._usermode[platform_common.ConstraintValueInfo]):
        if srcarch != "x86":
            fail("This kernel source only provides User Mode Linux for x86")
        arch, srcarch = "um", "um"
    return [KernelArchitectureInfo(
        cpu = cpu,
        arch = arch,
        srcarch = srcarch,
        bits = bits,
        subarch = header_arch,
        header_arch = header_arch,
    )]

kernel_architecture = rule(implementation = _impl, attrs = {
    "cpus": attr.label_keyed_string_dict(default = {Label("@platforms//cpu:" + cpu): cpu for cpu in CPU_ARCHITECTURES}),
    "_usermode": attr.label(default = "//kernel_modes:usermode"),
})
