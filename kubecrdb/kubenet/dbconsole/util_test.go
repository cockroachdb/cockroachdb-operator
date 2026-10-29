package dbconsole

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIsCookieValid(t *testing.T) {
	now := time.Now()

	testCases := []struct {
		cookie *http.Cookie
		valid  bool
	}{
		{
			cookie: &http.Cookie{Expires: now.Add(-1 * time.Hour)},
			valid:  false,
		},
		{
			cookie: &http.Cookie{Expires: now},
			valid:  false,
		},
		{
			cookie: &http.Cookie{Expires: now.Add(15 * time.Minute)},
			valid:  false,
		},
		{
			cookie: &http.Cookie{Expires: now.Add(time.Hour)},
			valid:  true,
		},
	}

	for _, tc := range testCases {
		require.Equal(t, tc.valid, isCookieValid(tc.cookie, now))
	}
}
