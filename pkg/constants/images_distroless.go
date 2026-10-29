//go:build !ubi

package constants

const (
	// defaultInitContainerImage lags the operator. It is updated by hand, and a
	// change to the init container's command-line surface cannot bump it in the
	// same commit, because the image it would name does not exist until that
	// commit is built and published.
	//
	// A caller needing a newer init container than this pin has to name one
	// itself. When it does not, the container exits on the unknown flag and the
	// pod never starts, which is the failure we want over one that starts and
	// does the wrong thing.
	//
	// Stamped UBI release builds do not have this hazard. They derive the
	// reference from the stamped operator version (see images_ubi.go).
	defaultInitContainerImage = "us-docker.pkg.dev/cockroach-cloud-images/data-plane/init-container:1b34da213707c4a89c7e794a19fbbfef5a1affe8"
	defaultReloaderImage      = "us-docker.pkg.dev/cockroach-cloud-images/data-plane/inotifywait:87edf086db32734c7fa083a62d1055d664900840"
)
