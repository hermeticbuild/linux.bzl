/* One executable: CPython and GNU Make's source-only syntax exporter.
 * Parsers run in fresh processes because their upstream APIs keep global state.
 * Only this executable is exec'd; no parser binaries or DSOs are extracted.
 */
#include <Python.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <limits.h>
#ifdef __APPLE__
#include <mach-o/dyld.h>
#include <mach-o/getsect.h>
#include <mach-o/ldsyms.h>

static char temporary_archive[] = "/tmp/kbuild-exporter-XXXXXX";

static void remove_import_archive(void)
{
    unlink(temporary_archive);
}
#endif

extern char **environ;
int kernel_make_main(int argc, char **argv, char **envp);

int main(int argc, char **argv)
{
    char executable[PATH_MAX];
#ifdef __APPLE__
    uint32_t size = sizeof(executable);
    if (_NSGetExecutablePath(executable, &size) != 0) {
        fputs("Executable path exceeds PATH_MAX\n", stderr);
        return 1;
    }
#else
    ssize_t size = readlink("/proc/self/exe", executable, sizeof(executable) - 1);
    if (size < 0 || size == sizeof(executable) - 1) {
        perror("resolve /proc/self/exe");
        return 1;
    }
    executable[size] = 0;
#endif
    if (argc > 1 && !strcmp(argv[1], "_make")) {
        if (!getenv("LINUX_BZL_MAKE_AST")) {
            fputs("Only GNU Make syntax export is supported\n", stderr);
            return 2;
        }
        argv[1] = executable;
        return kernel_make_main(argc - 1, argv + 1, environ);
    }
    const char *archive_path = executable;
#ifdef __APPLE__
    /* Keep the ZIP inside the signed image. Only the Python process needs a
     * temporary import archive; native parser subprocesses bypass this step. */
    unsigned long archive_size;
    const uint8_t *archive = getsectiondata(&_mh_execute_header, "__TEXT", "__pythonzip", &archive_size);
    if (!archive) {
        fputs("Missing embedded Python archive\n", stderr);
        return 1;
    }
    int archive_fd = mkstemp(temporary_archive);
    if (archive_fd < 0) {
        perror("create Python import archive");
        return 1;
    }
    if (atexit(remove_import_archive) != 0) {
        fputs("Cannot register Python import archive cleanup\n", stderr);
        close(archive_fd);
        unlink(temporary_archive);
        return 1;
    }
    FILE *archive_file = fdopen(archive_fd, "wb");
    if (!archive_file) {
        perror("open Python import archive");
        close(archive_fd);
        unlink(temporary_archive);
        return 1;
    }
    int written = fwrite(archive, 1, archive_size, archive_file) == archive_size;
    if (fclose(archive_file) != 0 || !written) {
        perror("write Python import archive");
        unlink(temporary_archive);
        return 1;
    }
    archive_path = temporary_archive;
#endif
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
    wchar_t *zip_path = Py_DecodeLocale(archive_path, NULL);
    status = PyWideStringList_Append(&config.module_search_paths, zip_path);
    PyMem_RawFree(zip_path);
    if (PyStatus_Exception(status)) Py_ExitStatusException(status);
    const char *bootstrap =
        "import sys, tempfile, zipfile, runpy\n"
        "from pathlib import Path\n"
        "with tempfile.TemporaryDirectory(prefix='kbuild-exporter-') as directory:\n"
        "    with zipfile.ZipFile(sys.path[0]) as archive:\n"
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
    int result = Py_RunMain();
    return result;
}
