# Bazel Generated File Hoisting

This directory contains the bazel targets for implementing file hoisting and a
go program for automatically generating rules.

Bazel's preference is to generate files on the fly within its sandbox and then
use those to build packages. This will cause language services and linters to
break as they don't see the generated files. To work around this issue, this
package will "hoist" generated files into workspace which allows non-bazel
tooling to work as expected.

For the immediate future, generated files will continue to be checked into the
repository but we may gitignore them once all file generation has been moved
into bazel.

## Examples

### Protocol Buffers (Golang)

Thankfully, there's nothing to do here! Simply create a `.proto` file and run
`make generate`. [Gazelle](https://github.com/bazelbuild/bazel-gazelle) will
generate the correct go targets which will then be picked up by `genbzl`.

### Protocol Buffers (Typescript)

Typescript Protocol Buffer generation requires manually editting `BUILD.bazel`
files. Before anything, create your `.proto` files and run `make
generate-bazel` to create the `proto_library` targets.

For the moment, we have a customized Typescript toolchain that is implemented
in `./proto_compile.bzl`. It can be imported as `load("//build:defs.bzl", "ts_proto_library")`.

```
ts_proto_library(
    name = "{PACKAGE}_ts_proto",
	# The location that TS files should go to. You probably want the default.
    prefix_path = "console/ui/cc-console/assets/js",
	# Dependencies!
    protos = [
        ":consolepb_proto",
        "//pkg/auditlog/auditlogpb:auditlogpb_proto",
        "//pkg/cmek/cmekpb:cmekpb_proto",
        "//pkg/commonpb:commonpb_proto",
        "//pkg/logexport/logexportpb:logexportpb_proto",
        "@com_github_mwitkow_go_proto_validators//:validator_proto",
    ],
	# Ensure to set visibility to public so that build/gen can invoke this package.
    visibility = ["//visibility:public"],
)
```

NOTE: If there's a companion `go_proto_library`, you can copy the `deps` clause into
`protos`. Otherwise, you'll have to fill out the list yourself.

Once in place, invoke `make generate`.

### Go Mocks (mockgen)

Gomock generation is a fairly manual process and varies depending on how the
mocks are being used.

#### Internal Mocks

If you're generating mocks for just your package, follow along here.

Within the `BUILD.bazel` file, you'll need to add a gomock rule for each
package that contains interfaces you want to mock.

Import the gomock rule. 
```
load("@bazel_gomock//:gomock.bzl", "gomock")
```

For each package, add a `gomock` rule following the template below. If you're
only generating mocks for a single package, you may omit the package name and
just call the rule "mocks".

```
gomock(
    name = "<package>_mocks",
    out = "<package_mocks>_mocks_generated_test.go",
    interfaces = ["InterfaceToMock"],
    library = "bazel target of the go_library",
    package = "this_package_name(_test)",
    self_package = "github.com/cockroachlabs/cockroach-enterprise-operator/pkg/this_package_name",
    visibility = ["//visibility:public"],
)
```

Finally, add each `gomock` rule and any additional imports to the `srcs` of the
`go_test` rule, suffixed with `# keep`.

```
go_test(
    name = "mypackage_test",
    srcs = [
        ":otherpackage_mocks",  # keep
        # ...
    ],
    embed = [":mypackage"],
    deps = [
        "@com_github_golang_mock//gomock",  # keep
        # ...
    ],
)
```


#### Exported Mocks

If you're generating mocks for interfaces in your package and exporting them
for others to use, follow along here.

NOTE: Packages that export mocks CAN NOT mix tests using `package <package>` and `package <package>_test`
due to a limitation in either gazelle or go_rules.
[See this issue for more details](https://github.com/bazelbuild/bazel-gazelle/issues/1078).

Within the `BUILD.bazel` file, you'll need to add a gomock rule, wrapper
`go_library`, and configure gazelle to use the wrapper.

Import the gomock rule. 
```
# Import the go mock rule. 
load("@bazel_gomock//:gomock.bzl", "gomock")
```

Add the gomock rule.

```
gomock(
    name = "<package>_mocks",
    out = "mocks_generated.go",
    interfaces = ["InterfaceToMock"],
    library = ":<package>",
    package = "<package>",
    self_package = "github.com/cockroachlabs/cockroach-enterprise-operator/pkg/<package>",
    visibility = ["//visibility:public"],
)
```

Add the library wrapper.
```
go_library(
    name = "<package>_with_mocks",
    srcs = [":mocks"],
    embed = [":<package>"],
    importpath = "copy this from the main library",
    visibility = ["//visibility:public"],
    deps = [ 
        "@com_github_golang_mock//gomock",  # keep
    ],
)
```

Set the main library to private so gazelle will always import the version with mocks.
```
go_library(
    name = "<package>",
    visibility = ["//visibility:private"], # keep
)
```


#### Converting from go:generate

Gomock generation must be added manually and the generated file names MUST have
the suffix `_generated.go` or `_generated_test.go`.

Let's translate from an example `go:generate` directive to a bazel target.

```
//go:generate mockgen -package=crypto -destination=./mocks.go . CertificateSigner
```

The above directive will create a `mocks.go` file for the `CertifcateSigner`
interface that's defined in the current package.

First, run `make generate-bazel` to ensure that all your `BUILD.bazel` files
are up to date. Then navigate the to `BUILD.bazel` in the directory that you
_want the mocks to be placed into_.

```
# Import the gomock definition.
load("@bazel_gomock//:gomock.bzl", "gomock")

# Define the gomock target.
gomock(
    name = "<PACKAGE>_mocks",
	# This the name of the file that will be generated. Again, it must have the
	# suffix _generated.go or generated_test.go.
    out = "mocks_generated.go",
	# List out any interface that you want mocks generated for.
    interfaces = ["CertificateSigner"],
	# This is the go_library that contains the interface you're looking to
	# mock. If you're targetting an interface in the package you're modifying,
	# you can use :PACKAGE. Otherwise you a bazel label, such as //pkg/clustersql.
    library = ":<PACKAGE>",
	# This is the package that the mock will use. This is usually PACKAGE or
	# PACKAGE_TEST.
    package = "PACKAGE",
	# self_package is the go import path of the package that this mock will
	# consider itself part of. It let's mockgen know that it might not have to
	# import the given package.
    self_package = "github.com/cockroachlabs/cockroach-enterprise-operator/pkg/clustersql",
	# As with everything else, ensure that visiblity is public so genbzl can find this rule.
    visibility = ["//visibility:public"],
)
```

Remove the `go:generate` directive, the files it generates, and run `make generate`!
