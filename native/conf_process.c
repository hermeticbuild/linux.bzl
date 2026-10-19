/* Native preprocess.c uses popen("r"); bind that process to declared tools. */
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

static FILE *active_stream;
static pid_t active_child;
static int active_error;

FILE *native_config_popen(const char *command, const char *mode)
{
	int descriptors[2], errors[2];
	const char *python = getenv("NATIVE_CONFIG_PYTHON");
	const char *runner = getenv("NATIVE_CONFIG_RUNNER");
	if (strcmp(mode, "r") || active_stream || !python || !runner) {
		errno = EINVAL;
		return NULL;
	}
	if (pipe(descriptors))
		return NULL;
	if (pipe(errors)) {
		close(descriptors[0]);
		close(descriptors[1]);
		return NULL;
	}
	if (fcntl(errors[1], F_SETFD, FD_CLOEXEC) == -1) {
		close(descriptors[0]);
		close(descriptors[1]);
		close(errors[0]);
		close(errors[1]);
		return NULL;
	}
	active_child = fork();
	if (active_child == 0) {
		int error;
		close(descriptors[0]);
		close(errors[0]);
		if (dup2(descriptors[1], STDOUT_FILENO) != -1) {
			close(descriptors[1]);
			execl(python, python, "-B", runner, "probe", command, (char *)NULL);
		}
		error = errno;
		(void)write(errors[1], &error, sizeof(error));
		_exit(127);
	}
	close(descriptors[1]);
	close(errors[1]);
	if (active_child == -1) {
		close(descriptors[0]);
		close(errors[0]);
		return NULL;
	}
	active_error = errors[0];
	active_stream = fdopen(descriptors[0], "r");
	if (!active_stream) {
		int saved = errno, status;
		close(descriptors[0]);
		close(active_error);
		while (waitpid(active_child, &status, 0) == -1 && errno == EINTR)
			;
		errno = saved;
	}
	return active_stream;
}

int native_config_pclose(FILE *stream)
{
	int status, error = 0;
	ssize_t count;
	if (stream != active_stream) {
		errno = EINVAL;
		return -1;
	}
	fclose(stream);
	active_stream = NULL;
	while (waitpid(active_child, &status, 0) == -1)
		if (errno != EINTR) {
			close(active_error);
			return -1;
		}
	do {
		count = read(active_error, &error, sizeof(error));
	} while (count == -1 && errno == EINTR);
	close(active_error);
	if (count != 0) {
		if (count > 0)
			errno = error ? error : EIO;
		return -1;
	}
	return status;
}
