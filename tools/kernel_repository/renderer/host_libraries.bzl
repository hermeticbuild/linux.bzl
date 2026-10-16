"""Library API contracts paired with the CcInfo that supplies those APIs."""

load("@rules_cc//cc/common:cc_info.bzl", "CcInfo")

KernelHostLibraryInfo = provider(doc = "Kernel Host Library Info.", fields = ["apis", "probe_answers"])

def _impl(ctx):
    return [
        ctx.attr.library[CcInfo],
        ctx.attr.library[DefaultInfo],
        KernelHostLibraryInfo(apis = ctx.attr.apis, probe_answers = ctx.attr.probe_answers),
    ]

kernel_host_library = rule(implementation = _impl, attrs = {
    "library": attr.label(mandatory = True, providers = [CcInfo]),
    "apis": attr.string_list(),
    "probe_answers": attr.string_dict(),
})
