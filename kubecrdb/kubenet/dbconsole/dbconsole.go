package dbconsole

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubenet"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
)

const (
	// secretLength is the number of random bytes generated for session secrets.
	secretLength = 16
	// SessionCookieName is the name of the cookie used for HTTP auth.
	SessionCookieName = "session"

	// sessionCreationQuery is the SQL used to store user session details.
	sessionCreationQuery = `
	INSERT INTO system.web_sessions ("hashedSecret", username, "expiresAt")
	VALUES($1, $2, $3)
	RETURNING id
	`

	// sessionCreationQueryWithUserID is the SQL used to store user session
	// details. In cockroachdb/cockroach#96896, we added a new user_id column
	// that is non-nullable. Once all clusters are running v23.1.X+, we can
	// remove the old query.
	sessionCreationQueryWithUserID = `
	INSERT INTO system.web_sessions ("hashedSecret", username, "expiresAt", user_id)
	VALUES ($1, $2, $3, (SELECT user_id FROM system.users WHERE username = $2))
	RETURNING id
	`

	// pcrBypassAostQuery is the SQL used to disable the automatic appending of
	// 'AS OF SYSTEM TIME' to queries when accessing a PCR reader VC (which
	// breaks SET TRANSACTION READ WRITE). This will disable reads from
	// non-system tables, but we don't care about that anyways.
	pcrBypassAostQuery = `
	SET SESSION bypass_pcr_reader_catalog_aost = "on"
	`

	// pcrEnableWriteTransactionQuery is the SQL used to enable write access when
	// we want to write to the system table in a PCR reader VC cluster
	pcrEnableWriteTransactionQuery = `
	SET TRANSACTION READ WRITE
	`

	applicationDBTemplate = "cluster:%s/defaultdb"
	systemDB              = "cluster:system/system"

	// AuthClusterHeader specifies which virtual cluster we are
	// authenticating to in a unified architecture cluster.
	//
	// Middleware that makes use of this header MUST require the header.
	// If the header is not required, then it may be possible for someone
	// to spoof the system tenant on a endpoint that does not set the
	// header. By explicitly requiring the header, we ensure that all
	// possible entrypoints to the middleware in question are setting the
	// header value themselves.
	AuthClusterHeader = "X-Cockroach-Auth-Cluster"

	// UAConsoleDisplayHeader specifies if there was a tenant passed in from the
	// SU DB console. This is relevant in the scenario where we authenticate to
	// the system tenant but want to retrieve the DB console page for the
	// application tenant.
	UAConsoleDisplayHeader = "X-Cockroach-UA-Console-Display-Cluster"
)

// NewDBConsoleClient creates an HTTP client that will direct all requests
// towards the DBConsole of the specified tenant or dedicated cluster. User, if
// provided, will be used to access the DBConsole but the root SQL user will
// always be used to mint the DBConsole cookie.
func NewDBConsoleClient(ctl *kube.Ctl, options kubenet.Options) (*http.Client, error) {
	if err := options.InitDefaults(); err != nil {
		return nil, err
	}

	dialer, err := kube.NewPodDialer(ctl.RestConfig(), ctl.Namespace())
	if err != nil {
		return nil, err
	}

	tlsConfig, err := kubenet.CreateUITlsConfig(ctl, &options)
	if err != nil {
		return nil, err
	}

	return &http.Client{
		Transport: &DBConsoleRoundTripper{
			ctl:     ctl,
			Options: options,
			Transport: http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (conn net.Conn, err error) {
					pods, err := kubenet.LocatePods(ctx, ctl, &options)
					if err != nil {
						return nil, errors.Wrap(err, "locating pods for dbconsole connection")
					}

					_, port, err := net.SplitHostPort(addr)
					if err != nil {
						return nil, errors.Wrapf(err, "parsing port from '%s'", port)
					}

					return dialer.DialContext(ctx, "tcp", fmt.Sprintf("%s:%s", pods[0], port))
				},
				TLSClientConfig: tlsConfig,
				IdleConnTimeout: 2 * time.Minute,
			},
			// The actual host doesn't really matter here. The dialer is responsible
			// for routing the request to the correct pod.
			Host:   "dbconsole:8080",
			Scheme: "https",
		},
	}, nil
}

// DBConsoleRoundTripper masquerades as an http.RoundTripper to seamlessly
// proxy requests through a standard http.Client. It handles pod locating,
// authn, TLS, and routine requests through a PodDialer. HTTP transactions are
// excuted via the embedded http.Transport.
type DBConsoleRoundTripper struct {
	http.Transport
	ctl     *kube.Ctl
	Options kubenet.Options
	Host    string
	Scheme  string
	mu      struct {
		sync.Mutex
		cookie            *http.Cookie
		cookieLastCleared time.Time
	}
}

var _ http.RoundTripper = &DBConsoleRoundTripper{}

// clearCookie clears the cookie in DBConsoleRoundTripper by setting it to
// nil. This will result in a new cluster UI session being minted.
// debounceDuration should be provided to debounce how often the cookie
// will be cleared.
func (r *DBConsoleRoundTripper) clearCookie(debounceDuration time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if time.Since(r.mu.cookieLastCleared) > debounceDuration {
		r.mu.cookie = nil
		r.mu.cookieLastCleared = time.Now()
	}
}

// drainBody discards and closes a http.Response body. This
// will allow the http connection to be reused. This should
// be used when retrying requests.
func drainBody(resp *http.Response) error {
	if resp.Body != nil {
		_, err := io.Copy(io.Discard, resp.Body)
		if err != nil {
			return err
		}
		err = resp.Body.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// RoundTrip implements http.RoundTripper. It wraps roundTrip with retry logic
// in the case that a 401 unauthorized response is returned from DB console. This
// may happen if web sessions in DB console are cleared or deleted, resulting in
// an invalid cookie being used for all requests.
func (r *DBConsoleRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	var bodyBytes []byte
	if req.Body != nil {
		bodyBytes, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
	}
	resp, err := r.roundTrip(req)
	if err != nil {
		return resp, err
	}
	if resp.StatusCode == 401 {
		err = drainBody(resp)
		if err != nil {
			return nil, err
		}
		if req.Body != nil {
			req.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		}

		r.clearCookie(time.Second * 10)
		resp, err = r.roundTrip(req)
	}

	return resp, err
}

// roundTrip prepares the incoming request by overrides the host, port, and
// cookie of the given request before passing it off to the embedded
// http.Transport.
func (r *DBConsoleRoundTripper) roundTrip(req *http.Request) (*http.Response, error) {
	tenantHeader := req.Header.Get(AuthClusterHeader)
	cookie, err := r.MaybeRefreshCookie(req.Context(), tenantHeader)
	if err != nil {
		return nil, errors.Wrap(err, "refreshing cookie")
	}

	// Remove any existing cookies as this client may be attached to a reverse
	// proxy.
	req.Header.Del("Cookie")
	// Inject our clusterUI auth cookie.
	req.AddCookie(cookie)

	if r.Options.IsUnifiedArchitecture {
		var tenantCookieValue string
		switch tenantHeader {
		case "application":
			tenantCookieValue = r.Options.ApplicationClusterName
		case "system":
			tenantCookieValue = "system"
		default:
			// This makes the X-Cockroach-Auth-Cluster required for a
			// successful request on UA clusters. See the comment on the
			// AuthClusterHeader const to understand why we need this.
			return nil, errors.Newf("unsupported tenant for unified architecture: %q", tenantHeader)
		}

		// If we've received a tenant from console, then we should respect
		// that tenant. This may be different from the value specified in
		// the X-Cockroach-Auth-Cluster header if, for example, we
		// authenticate to the system tenant but want to retrieve the DB
		// console page for the application tenant.
		dbConsoleHeader := req.Header.Get(UAConsoleDisplayHeader)
		// The only time it's possible for the session cookie to not contain the
		// tenant from X-Cockroach-UA-Console-Display-Cluster is when the superuser
		// console was displaying the non-system VC and the cluster toggles PCR
		// status.
		// That is, if a su is viewing the `app-readonly` tenant, and the cluster
		// was failed-over to, then we should ignore the tenant from console and
		// reset it to the default.
		if dbConsoleHeader != "" && sessionCookieHasTenant(dbConsoleHeader, cookie.Value) {
			tenantCookieValue = dbConsoleHeader
		}

		// Add the tenant cookie to the request so that CRDB knows which
		// tenant to display for DB console.
		req.AddCookie(&http.Cookie{
			Name:  "tenant",
			Value: tenantCookieValue,
		})
	}

	req.URL.Host = r.Host
	req.URL.Scheme = r.Scheme
	return r.Transport.RoundTrip(req)
}

func (r *DBConsoleRoundTripper) MaybeRefreshCookie(
	ctx context.Context, cockroachTenant string,
) (*http.Cookie, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var tenantChanged bool
	var tenantName string
	var applicationDBName string

	readOnlyCluster := r.Options.IsStandbyCluster
	if r.Options.IsUnifiedArchitecture {
		applicationDBName = fmt.Sprintf(applicationDBTemplate, r.Options.ApplicationClusterName)
		switch cockroachTenant {
		case "application":
			tenantName = r.Options.ApplicationClusterName
			if r.Options.Database != applicationDBName {
				tenantChanged = true
				r.Options.Database = applicationDBName
			}
		case "system":
			tenantName = "system"
			readOnlyCluster = false
			if r.Options.Database != systemDB {
				tenantChanged = true
				r.Options.Database = systemDB
			}
		default:
			return nil, errors.Newf("unsupported tenant for unified architecture: %q", cockroachTenant)
		}
	}

	if isCookieValid(r.mu.cookie, time.Now()) && !tenantChanged {
		// Our cookie is still valid, return it.
		return r.mu.cookie, nil
	}

	options := r.Options
	// We need to be root to successfully mint a new session.
	options.User = nil

	db, err := kubenet.OpenDB(ctx, r.ctl, options)
	if err != nil {
		return nil, errors.Wrap(err, "establishing DB connection")
	}
	defer db.Close()

	// NOTE: r.options.User.Username can be safely accessed if it is nil.
	username := r.Options.User.Username()
	if username == "" {
		username = "root"
	}

	cookie, err := MintClusterUISession(
		ctx, db.DB, username,
		timeutil.Now().Add(24*time.Hour),
		tenantName, readOnlyCluster,
	)
	if err != nil {
		return nil, errors.Wrap(err, "minting cluster UI cookie")
	}

	// If we are connecting to the system tenant, then we also want to
	// mint a session with the application tenant, because on the SU DB
	// console, we authenticate to the system tenant and should be able to
	// switch between the system and application tenants. At this point,
	// we already have a cluster UI session for the system tenant. Here,
	// we mint one for the application tenant as well.
	if r.Options.IsUnifiedArchitecture && cockroachTenant == "system" {
		applicationOptions := r.Options
		// We need to be root to successfully mint a new session.
		applicationOptions.User = nil
		applicationOptions.Database = applicationDBName
		applicationDB, err := kubenet.OpenDB(ctx, r.ctl, applicationOptions)
		if err != nil {
			return nil, errors.Wrap(err, "establishing application DB connection")
		}
		defer applicationDB.Close()

		applicationCookie, err := MintClusterUISession(
			ctx, applicationDB.DB, username,
			timeutil.Now().Add(24*time.Hour),
			applicationOptions.ApplicationClusterName, applicationOptions.IsStandbyCluster,
		)
		if err != nil {
			return nil, errors.Wrap(err, "minting cluster UI cookie")
		}

		// Within the session cookie's value, we can specify multiple
		// secrets for multiple tenants, indicating that we have valid
		// credentials for multiple tenants. In conjunction with the tenant
		// cookie, CRDB is able to determine which tenant to display at the
		// moment, and switch to another tenant's session if the tenant
		// cookie value changes.
		cookie.Value = fmt.Sprintf("%s,%s", cookie.Value, applicationCookie.Value)
	}

	r.mu.cookie = cookie
	return cookie, nil
}

// GetTransport returns the underlying http.Transport used to execute HTTP
// requests. It is used by tests to modify the DisableKeepAlives attribute.
// http.Transports keep connections around for a timeout when DisableKeepAlives
// is false, which looks like goroutine leaks to test cases.
func (r *DBConsoleRoundTripper) GetTransport() *http.Transport {
	return &r.Transport
}

// MintClusterUISession creates a session cookie for the given user and its expiration.
var MintClusterUISession = func(ctx context.Context, db *sql.DB, username string, sessionExpires time.Time, tenant string, readOnlyCluster bool,
) (*http.Cookie, error) {
	if username == "" {
		return nil, errors.New("username must be specified")
	}

	secret, hashed, err := createAuthSecret()
	if err != nil {
		return nil, err
	}

	var sessionID int64
	// It is mandatory that sessionExpires is provided in UTC as the expiresAt column in the web_sessions table
	// does not have a time zone and assumes all times are in UTC.
	sessionExpires = sessionExpires.UTC()

	// The disaster recovery team notes that in the long term, the session logic
	// should really be put behind an internal API of some sort. That way we can
	// avoid all this read only bypassing and older version query checking logic.
	// But for now we're stuck mucking with system.web_sessions table directly.
	if readOnlyCluster {
		if _, err = db.Exec(pcrBypassAostQuery); err != nil {
			return nil, err
		}

		var tx *sql.Tx
		tx, err = db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		if _, err = tx.Exec(pcrEnableWriteTransactionQuery); err != nil {
			return nil, err
		}

		if err = tx.QueryRowContext(
			ctx, sessionCreationQueryWithUserID, hashed, username, sessionExpires.UTC(),
		).Scan(&sessionID); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
	} else {
		if err = db.QueryRowContext(
			ctx, sessionCreationQueryWithUserID, hashed, username, sessionExpires.UTC(),
		).Scan(&sessionID); err != nil {
			// Fallback to the old query. This will be used by < v23.1 clusters.
			//
			// TODO(jaylim-crl): Once all clusters are running v23.1.X+, we can
			// remove this fallback.
			if subErr := db.QueryRowContext(
				ctx, sessionCreationQuery, hashed, username, sessionExpires.UTC(),
			).Scan(&sessionID); subErr != nil {
				return nil, errors.CombineErrors(subErr, err)
			}
		}
	}

	cookie := &SessionCookie{ID: sessionID, Secret: secret}
	cookieBytes, err := cookie.Marshal()
	if err != nil {
		return nil, err
	}

	cookieValue := base64.StdEncoding.EncodeToString(cookieBytes)
	if tenant != "" {
		cookieValue = fmt.Sprintf("%s,%s", cookieValue, tenant)
	}

	return &http.Cookie{
		Name:    SessionCookieName,
		Value:   cookieValue,
		Expires: sessionExpires,
	}, nil
}

func sessionCookieHasTenant(tenant, sessionCookie string) bool {
	parts := strings.Split(sessionCookie, ",")
	// realistically we can just use slices.Contains, but in the extremely
	// astronomically unlikely chance the tenant name is a base64 encoding of a
	// uuid, we should just check by position
	switch len(parts) {
	case 2:
		return tenant == parts[1] // system tenant
	case 4:
		return tenant == parts[1] || tenant == parts[3] // system or application tenant
	default:
		return false
	}
}

// createAuthSecret creates a secret, hash pair to populate a session auth token.
func createAuthSecret() (secret, hashedSecret []byte, err error) {
	secret = make([]byte, secretLength)
	if _, err := rand.Read(secret); err != nil {
		return nil, nil, err
	}

	hasher := sha256.New()
	_, err = hasher.Write(secret)
	if err != nil {
		return nil, nil, err
	}
	hashedSecret = hasher.Sum(nil)
	return secret, hashedSecret, nil
}
