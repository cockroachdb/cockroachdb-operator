//go:build ubi

package constants

import "fmt"

// operatorVersion is injected at build time via ldflags or Bazel x_defs. This
// is a separate copy from buildinfo.operatorVersion because pkg/constants cannot
// import buildinfo (import layering). Both are stamped from the VERSION file.
var operatorVersion string

const defaultUBIRegistry = "us-docker.pkg.dev/releases-prod/self-hosted"

// Sidecar image references for UBI-based deployments. Stamped builds use
// version tags (e.g. v1.0.0), unstamped dev builds fall back to SHA digests.
var (
	defaultInitContainerImage = initContainerImage()
	defaultReloaderImage      = reloaderImage()
)

func initContainerImage() string {
	if operatorVersion != "" {
		return fmt.Sprintf("%s/init-container:v%s", defaultUBIRegistry, operatorVersion)
	}
	return "us-docker.pkg.dev/releases-prod/self-hosted/init-container@sha256:6c2af8832e5b76d17e6ff35f03efcd2f064f7bc9921d247fa7962f2a1b50da6f"
}

func reloaderImage() string {
	if operatorVersion != "" {
		return fmt.Sprintf("%s/inotifywait:v%s", defaultUBIRegistry, operatorVersion)
	}
	return "us-docker.pkg.dev/releases-prod/self-hosted/inotifywait@sha256:def7535fe0406ae7bba284abd4afe3d08c69aac7b2580b892c9f313f96a157cf"
}
