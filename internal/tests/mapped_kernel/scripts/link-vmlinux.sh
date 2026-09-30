#!/bin/sh
set -eu

# Family execution stores generated inputs below nodes/<id>/<slot>, but a
# Kbuild WorkingTrees consumer must see only its exact logical object-tree
# projection. Seeing this directory would expose physical storage paths (and
# potentially leaves belonging only to another configuration).
test ! -e nodes
test -f include/generated/autoconf.h

cp "$1" "$4"
# The first line comes only from the compiled helper, after it has read the
# real compiler depfile and ELF object. Do not append the saved command itself:
# private execution paths are not image contents.
sed -n '1p' "$3" >> "$4"
# This source script must see the same explicit metadata on every executor.
printf '# mapped-build-owner:%s@%s\n' "$KBUILD_BUILD_USER" "$KBUILD_BUILD_HOST" >> "$4"
# The host executable's output must actually reach each selected image. This
# exposes both a missing relative source include and stale CONFIG reuse.
cat "$2" >> "$4"
touch System.map
