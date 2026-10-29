#!/usr/bin/env bash
set -euo pipefail

# Artifacts that need a human-readable label for the version should use this script.
# The label will contain the date of the commit (not the date the script was run!)
# and the short SHA, to uniquely identify the commit.
#
# %cs - committer date, short format (YYYY-MM-DD)
# %h  - abbreviated commit hash
git show -s --pretty='format:%cs-%h'
