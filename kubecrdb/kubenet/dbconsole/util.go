package dbconsole

import (
	"net/http"
	"time"
)

func isCookieValid(cookie *http.Cookie, now time.Time) bool {
	// Consider any cookies with a lifetime under 30 minutes as invalid. This
	// will prevent any race conditions were the cookie expires right before we
	// use it. 10 minutes would be sufficient, but why not be extra safe?
	return cookie != nil && cookie.Expires.Sub(now) >= 30*time.Minute
}
