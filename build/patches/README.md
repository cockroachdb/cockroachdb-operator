# Patches

Patches is a collection of patch files required for bazel builds to work correctly.

## `io_bazel_rules_go.patch`

This patch was pulled from https://github.com/bazelbuild/rules_go/issues/2927 as a proposed solution.

It corrects the resolution of well known type packages by rules_go. Otherwise
rules_go seems to pick different import paths per package which causes
compilation failures.

## `com_github_gogo_protobuf.patch`

This patch was pulled from `cockroachdb/cockroach`. It ensures that gogoproto
exports `gogo.proto` as a `proto_library` so that we can import it within our
.proto files.
