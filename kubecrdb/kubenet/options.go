package kubenet

import (
	"crypto/tls"
	"net/url"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"k8s.io/apimachinery/pkg/labels"
)

// Options specifies the settings needed to connect to a CRDB database hosted in
// a K8s cluster. Connections tunnel into the cluster over a Pod Dialer
// connection. See the Open comment for more usage details.
type Options struct {
	// Driver is the name of the Golang SQL driver used to connect. This must
	// be either "pgx", or a testing/linting driver that wraps the PGX driver.
	// If not set, defaults to "pgx".
	Driver string

	// TenantID is the numeric identifier of the tenant cluster to which to
	// connect. If this is not set, it defaults to the system tenant
	// (TenantID = 1), and a SQL connection is made directly to a CRDB pod.
	// Otherwise, a connection is made to a SQL pod owned by this tenant.
	// NOTE: If the tenant is suspended, it will be resumed.
	TenantID crdbv1beta1.TenantID

	// TenantCluster is the human-friendly name of this tenant cluster originally
	// picked by the user. TenantCluster is required for connections that pass
	// through the SQL proxy. If not specified, then a connection is made
	// directly to a SQL tenant pod.
	TenantCluster string

	// IsStandbyCluster denotes if the current cluster is a PCR standby. A
	// standby cluster needs special logic to deal with its readonly VC when
	// connecting to it.
	IsStandbyCluster bool

	// User is the name of PG user to connect as, along with an optional
	// password (if using password authentication).
	// If not set, and if no client certificate was provided, the user defaults
	// to "root" using certificate authentication.
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

	// Selector is the label selector used to find pods associated with this
	// cluster. It will default to `app=cockroachdb` when TenantID.IsSystemID()
	// is true, `app=sqlproxy` when UsesSqlProxy() is true.
	Selector labels.Selector

	// TLSConfig defines options for TLS configuration. If supplied, this is used
	// to configure the underlying TLS connection to pods. The caller can specify
	// any subset of the tls.Config fields:
	//
	//   - If RootCAs is nil,then we'll default to the cluster's CA.
	//   - If Certificates is nil, and no password was specified in the User
	//     field, then Certificates will default to the "root" client certificate.
	//   - If ServerName is empty, then it will default to the DNS name of the
	//     target service or pod.
	TLSConfig *tls.Config

	// OtherOptions are additional options passed to pgxsql that are added as the options
	// parameter in the connection URI.
	OtherOptions []string

	// PreferSimpleProtocol is propagated to pgxsql.Options. See the
	// pgxsql.Options.PreferSimpleProtocol doc for semantics.
	PreferSimpleProtocol bool

	// IsUnifiedArchitecture is true iff the CRDB cluster is on unified
	// architecture.
	IsUnifiedArchitecture bool

	// ApplicationClusterName is the name of the application cluster on a
	// unified architecture CRDB cluster.
	ApplicationClusterName string

	// OpenTimeout is the max wait time for connecting to the database.
	OpenTimeout time.Duration

	// ReadyPodsOnly, when true, filters out pods that are not passing
	// their readiness checks. Use this for connections (like DB Console)
	// where connecting to a draining node causes errors.
	ReadyPodsOnly bool
}

// InitDefaults initializes defaults on the options.
func (o *Options) InitDefaults() error {
	// Default to a connection to a CRDB node as the system tenant. Note this
	// works for both dedicated and host clusters.
	if o.TenantID == 0 {
		o.TenantID = crdbv1beta1.TenantSystemID
	}

	// Default to "root" user if not otherwise specified.
	if o.User == nil {
		o.User = url.User("root")
	}

	if o.UsesSqlProxy() {
		if pwd, ok := o.User.Password(); !ok || pwd == "" {
			return errors.New("Options.Password must be specified for SQL proxy connections")
		}
		if o.TenantID.IsSystemID() {
			return errors.New("Options.TenantID must be specified for SQL proxy connections")
		}
	}

	if o.Selector == nil {
		if o.TenantID.IsSystemID() {
			o.Selector = kube.ParseLabelOrDie("app=cockroachdb")
		} else if o.UsesSqlProxy() {
			o.Selector = kube.ParseLabelOrDie("app=sqlproxy")
		} else {
			// By default, only connect to stamped pods that are in the RUNNING state.
			// New connections should not be routed to DRAINING pods.
			o.Selector = manifests.MakeRunningStampedPodSelector(o.TenantID)
		}
	}

	if o.Application == "" {
		o.Application = "$ internal-cockroach-operator"
	}

	return nil
}

// UsesSqlProxy is true if the connection will be made through a SQL proxy pod.
func (o Options) UsesSqlProxy() bool {
	return o.TenantCluster != ""
}

// RequiresConnectionPrewarm returns true if the given options may require
// pre-warming the connection to avoid TLS errors.
func (o Options) RequiresConnectionPrewarm() bool {
	return o.TenantID != crdbv1beta1.TenantSystemID && !o.UsesSqlProxy()
}
