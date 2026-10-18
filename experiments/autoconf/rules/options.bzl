"""Construct custom Kconfig records for measured probe answers."""

_Y = [["lit", "y", "tristate"]]
_N = [["lit", "n", "tristate"]]

def kernel_bool(name, default = "n", depends = None, probe = False):
    dependency = [["sym", depends]] if depends else _Y
    value = [["sym", default]] if probe else [["lit", default, "tristate"]]
    return dict(
        symbol = name,
        type = "bool",
        parent_choice = 0,
        choice = False,
        modules = False,
        transitional = False,
        depends_on = dependency,
        direct = dependency,
        reverse = _N,
        implied = _N,
        properties = [dict(kind = "default", value = value, when = dependency)],
    )
