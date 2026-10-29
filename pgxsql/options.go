package pgxsql

import (
	"context"
	"crypto/tls"
	"net"
	"net/url"
	"time"
)

// Options specifies the settings needed to connect to a SQL database using the
// PGX driver.
type Options struct {
	// Driver is the name of the Golang SQL driver used to connect. This must
	// be either "pgx", or a testing/linting driver that wraps the PGX driver.
	// If not set, defaults to "pgx".
	Driver string

	// Host is the name or IP address of the pod that hosts the database.
	Host string

	// User is the name of PG user to connect as, along with an optional
	// password (if using password authentication).
	User *url.Userinfo

	// Port is the network port on which the database is listening.
	// If not set, defaults to 26257 (CRDB port).
	Port int

	// Database is the database being connected to.
	// If not set, defaults to "system", which is guaranteed to exist in CRDB
	// clusters.
	Database string

	// Application is the application name passed as part of the connection,
	// used in statement statistics to categorize activity.
	Application string

	// OtherOptions are additional options accepted by Postgres-compatible
	// databases in a command-line flag format like this:
	//
	//   --cluster=dim-dog-2
	//
	// pgxsql will pass these other options to the PGX driver as part of the
	// "options" parameter in the connection string URL, with a single space
	// separating each of the strings in OtherOptions.
	OtherOptions []string

	// PreferSimpleProtocol disables the use of the PostgreSQL extended
	// query protocol. When true, queries are sent as plain text via the
	// simple protocol, which allows multi-statement Exec calls but
	// disables server-side prepared statements and parameter binding.
	// Default false. Enable only for codepaths that need to send
	// multi-statement strings (e.g. post-init SQL scripts).
	PreferSimpleProtocol bool

	// TLSConfig describes the TLS configuration for this database connection.
	// If TLSConfig is nil, TLS connections are disabled. Otherwise, CA certs
	// should be specified via TLSConfig.RootCAs, and client certs should be
	// specified via TLSConfig.Certificates.
	TLSConfig *tls.Config

	// DialFunc is used by the PGX driver to make a network connection to the
	// database port.
	// If not set, defaults to net.Dialer.DialContext. Set this to
	// kube.PodDialer.DialContext if connecting to a Kubernetes pod.
	DialFunc func(ctx context.Context, net string, addr string) (conn net.Conn, err error)

	// LookupFunc is used by the PGX driver to lookup addresses for the given
	// host name. It defaults to IdentityLookup but can be set to a more
	// conventional lookup function like net.DefaultResolver.LookupHost.
	LookupFunc func(ctx context.Context, host string) (addrs []string, err error)

	// OpenTimeout is the max wait time for connecting to the database.
	OpenTimeout time.Duration
}

// SSLMode returns the SSL mode in the format needed for the PG "sslmode"
// connection option.
func (c Options) SSLMode() string {
	if c.TLSConfig == nil {
		return "disable"
	}
	if c.TLSConfig.InsecureSkipVerify {
		return "verify-ca"
	}
	return "verify-full"
}

// IdentityLookup implements net.Resolver.Lookup by just returning the given
// hostname. It's useful when connecting to a non-resolvable hostname, such as a
// Kubernetes pod name.
func IdentityLookup(ctx context.Context, host string) ([]string, error) {
	return []string{host}, nil
}
