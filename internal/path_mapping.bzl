"""Helpers for actions that support Bazel's stripped output paths."""

visibility("//internal/...")

def path_mapped_run(actions, **kwargs):
    execution_requirements = dict(kwargs.get("execution_requirements", {}))
    execution_requirements["supports-path-mapping"] = "1"
    kwargs["execution_requirements"] = execution_requirements
    actions.run(**kwargs)
