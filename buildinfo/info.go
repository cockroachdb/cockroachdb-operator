// build is a collection of static variables defined at build time via the linker -X flag.
// It was "inspired" by CRDB's build package.
// https://github.com/cockroachdb/cockroach/blob/2571d617600182963f84d05d256bb678b577356b/pkg/build/info.go
// Binarys that make use of the build package should execute
// `build.Print(binaryName)` when provided with the -version flag.
//
//	var printVersion bool
//	flag.BoolVar(&printVersion, "version", false, "Prints version information")
//	flag.Parse()
//	if printVersion {
//		build.Print("my-binary")
//		return
//	}
//
// When incorporating this package into a binary it is necessary to setup the
// appropriate build arguments in a Makefile and Dockerfile. The following
// snippet and updates to cloudbuild.yaml should be usable for most cases.
// * Make targets that construct a binary will require the addition of -ldflags '$(LDFLAGS)'.
// * Make targets that construct a docker image will require the addition of $(BUILD_ARGS).
//
//	# Build arguments to populate the build package.
//	ARG BUILD_TAG
//	ARG BUILD_TYPE
//	ARG BUILD_VERSION
//	ARG COMMIT_SHA
//
//	# BUILD_TAG, BUILD_TAG, BUILD_VERSION and COMMIT_SHA are build metadata
//	# variables that will be injected into the binary via -ldflags. They will
//	# default the environment variables, if they are set.  When building via CI,
//	# ie cloudbuild.yaml, the various values are passed in as docker build
//	# arguments via the args array and which are then read as environment
//	# variables.
//	BUILD_TAG ?= $(shell git describe --tags)
//	BUILD_TYPE ?=
//	BUILD_VERSION ?= $(shell scripts/version.sh)
//	COMMIT_SHA ?= $(shell git rev-parse HEAD)
//
//	# BUILD_ARGS forwards the various build metadata variables into a Dockerfile as
//	# build arguments.
//	BUILD_ARGS := --build-arg=COMMIT_SHA=$(COMMIT_SHA) --build-arg=BUILD_TAG=$(BUILD_TAG) --build-arg=BUILD_TYPE=$(BUILD_TYPE) --build-arg=BUILD_VERSION=$(BUILD_VERSION)
//
//	# LDFLAGS is passed into go build's -ldflags parameters and is used to populate
//	# build metadata in the build package.
//	LDFLAGS := \
//		-X "github.com/cockroachlabs/cockroach-enterprise-operator/build.typ=$(BUILD_TYPE)" \
//		-X "github.com/cockroachlabs/cockroach-enterprise-operator/build.tag=$(BUILD_TAG)" \
//		-X "github.com/cockroachlabs/cockroach-enterprise-operator/build.rev=$(COMMIT_SHA)" \
//		-X "github.com/cockroachlabs/cockroach-enterprise-operator/build.version=$(BUILD_VERSION)" \
//		-X "github.com/cockroachlabs/cockroach-enterprise-operator/build.utcTime=$(shell date -u '+%Y/%m/%d %H:%M:%S')" \
package buildinfo

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"time"
)

// timeFormat is the reference format for build.Time. Make sure it stays in sync
// with the string passed to the linker in the root Makefile.
const timeFormat = "2006/01/02 15:04:05"

var (
	// These variables are initialized via the linker -X flag in the
	// top-level Makefile when compiling release binaries.
	tag             string // Tag of this build (git describe --tags w/ optional '-dirty' suffix)
	utcTime         string // Build time in UTC (year/month/day hour:min:sec)
	rev             string // SHA-1 of this build (git rev-parse)
	typ             string // Type of this build (release or "")
	version         string // Version of this build as determined by scripts/version.sh
	operatorVersion string // Semantic version from the VERSION file (e.g. "1.0.0")
	platform        = fmt.Sprintf("%s %s", runtime.GOOS, runtime.GOARCH)
)

func init() {
	if IsRelease() && rev == "" {
		panic(`build package misconfigured. typ was set to "release" but rev is empty.`)
	}
}

// IsRelease indicates if this binary is intended to run in non-developer
// environments. All images built through CI should report IsRelease as true.
// Local build may build in release mode by specifying BUILD_TYPE=release in
// their respective make commands.
func IsRelease() bool {
	return typ == "release"
}

// Platform is the OS and ARCH this binary was targeting. It should always be
// "linux amd64" when deployed.
func Platform() string {
	return platform
}

// BuildTime is the UTC time that this binary was built.
func BuildTime() time.Time {
	t, _ := time.Parse(utcTime, timeFormat)
	return t
}

// Revision is the commit SHA that this binary was produced at, optional with
// a -dirty suffix.
func Revision() string {
	return rev
}

// BuildVersion is the name of the build version as determined by the
// scripts/version.sh script
func BuildVersion() string {
	return version
}

// Tag returns the build tag of this binary. It will either be a branch name,
// tag name, or a git delta to the most recent tag. For example, "master",
// "release-2020-01-01", or "release-2021-07-29-0-93-g936d40ca9'
func Tag() string {
	if tag == "" {
		return "unknown"
	}
	return tag
}

// OperatorVersion returns the semantic version of the operator (e.g. "1.0.0")
// as read from the VERSION file at build time. Returns "dev" for local builds
// that were not stamped.
func OperatorVersion() string {
	if operatorVersion == "" {
		return "dev"
	}
	return operatorVersion
}

// ContainerImage constructs an image reference from the repo and build info.
// Release builds prefer the operator version tag (v1.0.0), falling back to the
// commit SHA if no version is stamped. Non-release builds use defaultTag.
func ContainerImage(repo, defaultTag string) string {
	tag := defaultTag
	if IsRelease() {
		// use the raw variable instead of OperatorVersion() to avoid the
		// "dev" fallback, since "vdev" is not a valid image tag
		if operatorVersion != "" {
			tag = "v" + operatorVersion
		} else {
			tag = Revision()
		}
	}
	return fmt.Sprintf("%s:%s", repo, tag)
}

// Print logs out all build information directory to standard out. In most
// cases, Print should be invoked when the -version flag is specified.
//
//	var printVersion bool
//	flag.BoolVar(&printVersion, "version", false, "Prints version information")
//	flag.Parse()
//	if printVersion {
//		build.Print("my-binary")
//		return
//	}
func Print(name string) {
	printTo(os.Stdout, name)
}

func printTo(out io.Writer, name string) {
	fmt.Fprintf(out, "Binary: %s\n", name)
	fmt.Fprintf(out, "Operator Version: %s\n", OperatorVersion())
	fmt.Fprintf(out, "Build Time: %s\n", utcTime)
	fmt.Fprintf(out, "Go Version: %s\n", runtime.Version())
	fmt.Fprintf(out, "Is Release: %v\n", IsRelease())
	fmt.Fprintf(out, "Platform: %s\n", Platform())
	fmt.Fprintf(out, "Revision: %s\n", Revision())
	fmt.Fprintf(out, "Tag: %s\n", Tag())
	fmt.Fprintf(out, "Build Version: %s\n", BuildVersion())
}
