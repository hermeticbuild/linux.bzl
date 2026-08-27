//! Sibling source fixture for the public `linux_module` rule.

/// Marker referenced by the crate root so this sibling must be staged and compiled.
pub(crate) const SIBLING_MARKER: usize = 1;
