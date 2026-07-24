"""Host filename selection for the repository graph generator."""

visibility("//...")

def kconfig_tool_filename(platform, tool):
    if platform.startswith("windows_"):
        return tool + ".exe"
    return tool
