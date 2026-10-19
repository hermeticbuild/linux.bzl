"""Decode the small JSON shapes shared by native bootstrap and configuration."""

from collections.abc import Mapping


def strings(value: object) -> dict[str, str]:
    if not isinstance(value, dict) or not all(isinstance(k, str) and isinstance(v, str) for k, v in value.items()):
        raise ValueError(f"Expected a string mapping, got {value!r}")
    return value


def string_list(value: object) -> list[str]:
    if not isinstance(value, list) or not all(isinstance(item, str) for item in value):
        raise ValueError(f"Expected a string list, got {value!r}")
    return value


def text_field(value: Mapping[str, object], name: str) -> str:
    match value[name]:
        case str(result):
            return result
        case other:
            raise ValueError(f"{name}: expected a string, got {other!r}")


def mapping(value: object) -> dict[str, object]:
    if not isinstance(value, dict) or not all(isinstance(key, str) for key in value):
        raise ValueError(f"Expected an object, got {value!r}")
    return value
