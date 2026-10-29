package kuberecord

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/cenkalti/backoff"
)

var modeString = flag.String("kuberecord", "replay", "sets the global mode for kuberecord; valid values are disabled, record, and replay")

// Mode returns the globally configured recordMode for kuberecord based on the
// -record CLI flag and the KUBERECORD environment variable. If an unexpected
// value is encountered, Mode will panic.
// Valid values are "", record, replay, disable, and disabled.
func Mode() recordMode {
	// Use environment variable if it exists, else fall back to flag.
	mode, ok := os.LookupEnv("KUBERECORD")
	if !ok {
		mode = *modeString
	}

	switch mode {
	case "record", "":
		return ModeRecording
	case "replay":
		return ModeReplaying
	case "disabled", "disable":
		return ModeDisabled
	default:
		panic(fmt.Sprintf("unknown mode: %s", mode))
	}
}

// IsEnabled is a helper method that returns true if Mode() is not disabled.
func IsEnabled() bool {
	return Mode() != ModeDisabled
}

// MaxBackOffInterval is a convenience method that returns the amount of time
// that backoff logic in tests should sleep between polling attempts (e.g. to
// see if a K8s resource is ready). In ModeReplaying, this function returns 0,
// so that replays will run as fast as possible. Otherwise, it returns the
// specified default value.
func MaxBackOffInterval(defaultVal time.Duration) time.Duration {
	if Mode() == ModeReplaying {
		return 0
	}
	return defaultVal
}

// BackOff is a convenience method meant to be used with kube.Options.BackOff.
// In ModeReplaying, a backoff function that does not ever sleep is returned.
// Otherwise, nil is returned, indicating that the default backoff option should
// be used.
func BackOff() func() backoff.BackOff {
	if Mode() == ModeReplaying {
		return zeroBackOffFunc
	}
	return nil
}

// backoff.ZeroBackOff is thread-safe, so return global instance.
var zeroBackOff = &backoff.ZeroBackOff{}
var zeroBackOffFunc = func() backoff.BackOff {
	return zeroBackOff
}
