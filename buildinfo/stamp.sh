#!/usr/bin/env bash

# This command is used by bazel as the workspace_status_command
# to implement build stamping with git information.
# See https://docs.bazel.build/versions/main/user-manual.html#workspace-status
# Usage: ./build/stamp.sh [release]

# NB: Any values NOT prefixed with STABLE_ will not cause
# cache busts within bazel. Concretely, this means that a
# cached version of a package that uses COMMIT_SHA could
# accidentally use a different SHA. For a deployment,
# this would result in deploying containers that don't exist
# or worse yet, out of date containers.
#
# For now, we mark all values in this file as STABLE_ and
# force bazel to rebuild everything that relies on these
# values.
#
# A, possibly better, alternative would be to only pass these values into the
# container images as environment variables which would minimize the number of
# go packages that have to get rebuilt. There may also be some magic we can do
# with different build configurations if we treat master and branched builds
# differently.

# We try our best guessing the current branch.
# First we try the BRANCH_NAME environment variable, which CI may set to
# the branch, tag, or PR number.
THIS_BRANCH=${BRANCH_NAME:-}
# For local builds we use `git branch --show-current`. This works only if
# the checkout was made using a branch name.
if [[ $THIS_BRANCH == "" ]]; then
  THIS_BRANCH="$(git branch --show-current)"
fi
# Container images may be pushed from the merge queue, which contains characters
# in the branch name which are not valid in container image tags.
if [[ "$THIS_BRANCH" =~ ^gh-readonly-queue/ ]]; then
  THIS_BRANCH="merge-queue-$(git rev-parse --short HEAD)"
fi
# For all other cases, e.g. detached head checkouts made using SHAs or tag
# names, we fallback to "unknown".
if [[ $THIS_BRANCH == "" ]]; then
  THIS_BRANCH="unknown"
fi

echo "STABLE_BRANCH_NAME $THIS_BRANCH"
echo "STABLE_BUILD_TAG $(git describe --tags)"
echo "STABLE_BUILD_TYPE $1"
echo "STABLE_BUILD_VERSION $(scripts/version.sh)"
echo "STABLE_COMMIT_SHA $(git rev-parse HEAD)"

# BUILD_TIME is inherently unstable, and shouldn't be marked as such. This is
# congruent with the default BUILD_TIMESTAMP and FORMATTED_DATE volatile keys
# that Bazel produces for every build, but with a slightly different format.
echo "BUILD_TIME $(date -u '+%Y/%m/%d %H:%M:%S')"
