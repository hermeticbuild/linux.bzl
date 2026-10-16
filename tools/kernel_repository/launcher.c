/* One ELF: musl, CPython, GNU Make's syntax exporter and Linux's Kconfig parser.
 * Parsers run in fresh processes because their upstream APIs keep global state.
 * Only this executable is exec'd; no parser binaries or DSOs are extracted.
 */
#include <Python.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <limits.h>

extern char **environ;
int kernel_make_main(int argc, char **argv, char **envp);
int kernel_kconfig_main(int argc, char **argv);

int main(int argc, char **argv)
{
    char executable[PATH_MAX];
    ssize_t size = readlink("/proc/self/exe", executable, sizeof(executable) - 1);
    if (size < 0 || size == sizeof(executable) - 1) {
        perror("resolve /proc/self/exe");
        return 1;
    }
    executable[size] = 0;
    if (argc > 1 && !strcmp(argv[1], "_make")) {
        if (!getenv("LINUX_BZL_MAKE_AST")) {
            fputs("Only GNU Make syntax export is supported\n", stderr);
            return 2;
        }
        argv[1] = executable;
        return kernel_make_main(argc - 1, argv + 1, environ);
    }
    if (argc > 1 && !strcmp(argv[1], "_kconfig")) {
        argv[1] = executable;
        return kernel_kconfig_main(argc - 1, argv + 1);
    }
    PyPreConfig preconfig;
    PyPreConfig_InitIsolatedConfig(&preconfig);
    preconfig.utf8_mode = 1;
    PyStatus prestatus = Py_PreInitialize(&preconfig);
    if (PyStatus_Exception(prestatus)) Py_ExitStatusException(prestatus);
    PyConfig config;
    PyConfig_InitIsolatedConfig(&config);
    config.site_import = 0;
    config.write_bytecode = 0;
    config.module_search_paths_set = 1;
    PyStatus status = PyConfig_SetBytesString(&config, &config.executable, executable);
    if (PyStatus_Exception(status)) Py_ExitStatusException(status);
    wchar_t *zip_path = Py_DecodeLocale(executable, NULL);
    status = PyWideStringList_Append(&config.module_search_paths, zip_path);
    PyMem_RawFree(zip_path);
    if (PyStatus_Exception(status)) Py_ExitStatusException(status);
    const char *bootstrap =
        "import sys, tempfile, zipfile, runpy\n"
        "from pathlib import Path\n"
        "with tempfile.TemporaryDirectory(prefix='kernel-repository-') as directory:\n"
        "    with zipfile.ZipFile(sys.executable) as archive:\n"
        "        for name in archive.namelist():\n"
        "            if name.startswith('resources/'):\n"
        "                archive.extract(name, directory)\n"
        "    renderer = Path(directory) / 'resources/renderer'\n"
        "    sys.path.insert(0, str(renderer))\n"
        "    runpy.run_path(str(renderer / 'repository_main.py'), run_name='__main__')\n";
    status = PyConfig_SetBytesArgv(&config, argc, argv);
    if (PyStatus_Exception(status)) Py_ExitStatusException(status);
    status = PyConfig_SetBytesString(&config, &config.run_command, bootstrap);
    if (PyStatus_Exception(status)) Py_ExitStatusException(status);
    status = Py_InitializeFromConfig(&config);
    PyConfig_Clear(&config);
    if (PyStatus_Exception(status)) Py_ExitStatusException(status);
    return Py_RunMain();
}
