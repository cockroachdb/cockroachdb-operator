package tools

// Prevent `go mod tidy` from garbage-collecting this package, which is only
// used by gazelle.
import _ "golang.org/x/tools/go/vcs"
