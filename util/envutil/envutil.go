// Package envutil provides helpers for overriding configuration defaults
// with environment variables.
package envutil

import (
	"context"
	"os"
	"strconv"
	"time"

	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
)

// load overwrites *target with the parsed value of envKey. An unset or empty
// variable leaves the target untouched. A value that fails to parse also
// leaves it untouched.
func load[T any](ctx context.Context, envKey string, target *T, parse func(string) (T, error)) {
	envValue := os.Getenv(envKey)
	if envValue == "" {
		return
	}
	parsed, err := parse(envValue)
	if err != nil {
		logging.Warnf(ctx, "invalid %s value %q, keeping default %v: %v", envKey, envValue, *target, err)
		return
	}
	*target = parsed
	logging.Infof(ctx, "%s overridden to %v", envKey, *target)
}

// LoadInt overrides *target with the integer value of envKey, if set.
func LoadInt(ctx context.Context, envKey string, target *int) {
	load(ctx, envKey, target, strconv.Atoi)
}

// LoadBool overrides *target with the boolean value of envKey, if set.
func LoadBool(ctx context.Context, envKey string, target *bool) {
	load(ctx, envKey, target, strconv.ParseBool)
}

// LoadDurationSeconds overrides *target with the value of envKey interpreted
// as a whole number of seconds, if set.
func LoadDurationSeconds(ctx context.Context, envKey string, target *time.Duration) {
	load(ctx, envKey, target, func(value string) (time.Duration, error) {
		seconds, err := strconv.Atoi(value)
		return time.Duration(seconds) * time.Second, err
	})
}
