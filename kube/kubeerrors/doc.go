/*
kubeerrors is a subset of k8s.io/apimachinery/pkg/api/errors with support
for error unwrapping. At the time of writing, the version of the Kubernetes
SDK that we use does not support error unwrapping and the various utilities
may report false positives. Instead this package should be used. When the
SDK is finally updated, this package may be replaces with the Kubernetes
errors package.
*/
package kubeerrors
