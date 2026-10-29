package envutil

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoadInt(t *testing.T) {
	const envKey = "ENVUTIL_TEST_INT"
	cases := []struct {
		name     string
		envValue string
		want     int
	}{
		{name: "unset keeps current value", envValue: "", want: 10},
		{name: "valid value overrides", envValue: "40", want: 40},
		{name: "invalid value keeps current value", envValue: "not-a-number", want: 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envKey, tc.envValue)
			target := 10
			LoadInt(context.Background(), envKey, &target)
			require.Equal(t, tc.want, target)
		})
	}
}

func TestLoadBool(t *testing.T) {
	const envKey = "ENVUTIL_TEST_BOOL"
	cases := []struct {
		name     string
		envValue string
		want     bool
	}{
		{name: "unset keeps current value", envValue: "", want: false},
		{name: "true overrides", envValue: "true", want: true},
		{name: "numeric form overrides", envValue: "1", want: true},
		{name: "invalid value keeps current value", envValue: "yes", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envKey, tc.envValue)
			target := false
			LoadBool(context.Background(), envKey, &target)
			require.Equal(t, tc.want, target)
		})
	}
}

func TestLoadDurationSeconds(t *testing.T) {
	const envKey = "ENVUTIL_TEST_DURATION"
	cases := []struct {
		name     string
		envValue string
		want     time.Duration
	}{
		{name: "unset keeps current value", envValue: "", want: time.Minute},
		{name: "seconds override", envValue: "90", want: 90 * time.Second},
		{name: "duration string keeps current value", envValue: "1m", want: time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envKey, tc.envValue)
			target := time.Minute
			LoadDurationSeconds(context.Background(), envKey, &target)
			require.Equal(t, tc.want, target)
		})
	}
}
