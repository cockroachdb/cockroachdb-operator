#!/usr/bin/env bash

set -euxo pipefail

# files_unchanged_from_upstream takes file globs as arguments and checks
# whether all these files are unchanged from the upstream master branch.
# It is best effort.
# This optimization can be disabled by setting env var
# CC_BAZEL_FORCE_GENERATE=1.
# "Borrowed" from https://github.com/cockroachdb/cockroach/blob/26192d2283c78f43e0de862017fa7a132a11bb05/build/bazelutil/bazel-generate.sh#L5-L41
files_unchanged_from_upstream() {
  if [ "${CC_BAZEL_FORCE_GENERATE:-}" = 1 ]; then
    return 1
  fi

  if ! which git >/dev/null; then
    return 1
  fi

  # First, figure out the correct remote.
  UPSTREAM=$(git remote -v | grep 'github.com[/:]cockroachlabs/cockroach-enterprise-operator.*(fetch)' | awk '{print $1}') || return 1
  if [ -z "$UPSTREAM" ]; then
    return 1
  fi

  # Find the upstream commit which this branch is based on.
  BASE=$(git merge-base $UPSTREAM/master HEAD 2>/dev/null) || return 1
  if [ -z "$BASE" ]; then
    return 1
  fi

  # Check if the files are unchanged.
  DIFF=$(git diff --no-ext-diff --name-only $BASE -- "$@") || return 1
  if [ -z "$DIFF" ]; then
    # No diffs.
    return 0
  fi
  return 1
}

# Mark private cockroachlabs repositories as private, forcing pulls via git
# (bypassing proxy.golang.org / sum.golang.org). In CI the companion workflow
# step configures a git insteadOf rewrite with an installation token so these
# fetches authenticate against the private repos.
go env -w GOPRIVATE="github.com/cockroachlabs/cockroach-enterprise-operator/*,github.com/cockroachlabs/version"

# Generate go BUILD.bazel files. BUILD file generation is quite fast, so
# there's no need to gate on go files changing.
# NB: While BUILD.bazel files are not being checked in, this must be run before
# executing update-repos otherwise bazel will see many of our dependencies as
# non-packages.
bazel run //:gazelle $@

# If go.mod, go.sum, or DEPS.bzl have changed, we need to synchronize changes
# via running gazelle update-repos.
if files_unchanged_from_upstream go.mod go.sum DEPS.bzl; then
  echo "Skipping bazel run //:gazelle-update-repos (no changes to go.mod go.sum DEPS.bzl)"
else
  bazel run //:gazelle-update-repos $@
fi

# Generate a list of all generated files that need to be hoisted from the bazel
# sandbox.
bazel run //build/gen/genbzl --run_under="cd $PWD && " -- --out-dir build/gen/
