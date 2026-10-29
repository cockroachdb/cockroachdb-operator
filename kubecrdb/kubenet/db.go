package kubenet

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/autoscaler"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kuberecord"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pgxsql"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/backoff"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/tracing"
	"go.opentelemetry.io/otel"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
)

// OpenDB establishes a SQL connection to the CRDB cluster hosted in the given K8s
// cluster. The connection may be to a logical tenant cluster or to a physical
// CRDB cluster, depending on option settings:
//
//	CRDB Pod
//	  TenantID = tenant.TenantSystemID
//	  Connects to one of the pods running KV. This works for both dedicated and
//	  host clusters.
//
//	Tenant Pod
//	  TenantID = non-system tenant ID
//	  TenantCluster = ""
//	  Connects to one of the SQL pods for the specified tenant, bypassing the
//	  SQL proxy. If the tenant is suspended, it will be resumed.
//
//	Proxy Pod
//	  TenantID = non-system tenant ID
//	  TenantCluster = tenant cluster name (e.g. "dim-dog")
//	  Connects to one of the SQL proxy pods and through that to a SQL pod for
//	  the specified tenant. If the tenant is suspended, the SQL proxy will
//	  resume it.
//
// OpenDB uses the PodDialer to tunnel into the K8s cluster and connect directly
// to a CRDB, SQL, or SQLProxy pod, rather than going through the cluster's
// external IP address. If there are multiple pods available, then OpenDB will
// pick a random pod to connect to first. If that fails, it will try the second,
// and so on. This provides a form of client-side load balancing.
var OpenDB = func(ctx context.Context, ctl *kube.Ctl, options Options) (_ *pgxsql.DB, returnedErr error) {
	ctx, span := otel.GetTracerProvider().Tracer("kubenet").Start(ctx, "OpenDB")
	defer func() { tracing.EndWithError(span, returnedErr) }()

	pgxOptions, err := getPGXOptions(ctl, &options)
	if err != nil {
		return nil, err
	}
	db, err := pgxsql.Open(pgxOptions)
	if err != nil {
		return nil, errors.Wrap(err, "error opening kubesql connection")
	}
	defer func() {
		// Close the DB connection if we have an error so that the caller does
		// not need to handle it.
		if returnedErr != nil {
			_ = db.DB.Close()
		}
	}()
	if err := maybeWarmupTenant(
		ctx,
		func(ctx context.Context, query string) error {
			_, err := db.ExecContext(ctx, query)
			return err
		},
		options,
	); err != nil {
		return nil, err
	}

	return db, nil
}

// maybeWarmupTenant warms up a tenant if required.
func maybeWarmupTenant(
	ctx context.Context, exec func(ctx context.Context, query string) error, options Options,
) (retErr error) {
	ctx, span := otel.GetTracerProvider().Tracer("kubenet").Start(ctx, "maybeWarmupTenant")
	defer func() { tracing.EndWithError(span, retErr) }()

	// If this was a connection to a SQL pod, ensure it's ready before returning.
	// TODO(andyk): Remove this once we have a better way to ensure that a SQL
	// pod is ready. Right now, it's difficult because we replace the SqlStarter
	// process with the real CRDB SQL process and don't have a way to know when
	// that is ready. This is not needed when connecting through the proxy, since
	// that does its own retries.
	if options.RequiresConnectionPrewarm() {
		err := backoff.Run(
			ctx,
			func() error { return exec(ctx, `SELECT 1`) },
			&backoff.Opts{
				InitialInterval: 10 * time.Millisecond,
				Timeout:         30 * time.Second,
				MaxInterval:     kuberecord.MaxBackOffInterval(time.Second),
				NotifyInterval:  5 * time.Second,
			},
		)
		if err != nil {
			return errors.Wrapf(err, "waiting for SQL tenant pod %d to be ready", options.TenantID)
		}
	}
	return nil
}

// getPGXOptions returns the pgxsql.Options from the given ctl and options.
// The given options parameter may be modified with default values set.
func getPGXOptions(ctl *kube.Ctl, options *Options) (pgxsql.Options, error) {
	if err := options.InitDefaults(); err != nil {
		return pgxsql.Options{}, err
	}

	pgxOptions := pgxsql.Options{
		Driver:               options.Driver,
		User:                 options.User,
		Port:                 options.Port,
		Database:             options.Database,
		Application:          options.Application,
		OtherOptions:         options.OtherOptions,
		PreferSimpleProtocol: options.PreferSimpleProtocol,
		OpenTimeout:          options.OpenTimeout,
	}

	if options.UsesSqlProxy() {
		// Tunneling to a SQL proxy pod, so set the --cluster option in order to
		// communicate the tenant's name (tenant cluster name + tenant ID).
		pgxOptions.OtherOptions = []string{
			fmt.Sprintf("--cluster=%s-%d", options.TenantCluster, options.TenantID),
		}
	}

	tlsConfig, err := createTlsConfig(ctl, options)
	if err != nil {
		return pgxsql.Options{}, err
	}
	pgxOptions.TLSConfig = tlsConfig

	// Override the pgx driver's LookupFunc in order to return the set of tenant
	// or CRDB pod names, in random order. The pgx driver will try connecting to
	// the first pod in the list. If that fails, it will try the second, and so
	// on. This provides a form of client-side load balancing.
	//
	// NOTE: Pod location must be done inside the LookupFunc, since this will be
	// called every time a new connection is established, and the list of pods
	// can change between calls (e.g. if tenant pod goes away).
	//
	// NOTE: Unfortunately, doing pod lookup inside LookupFunc causes problems
	// with copyist + kuberecord, since LookupFunc never gets called during
	// copyist playback (since the real database is not contacted). So it means
	// kuberecord will add extra records during recording that are never used
	// during playback, which can cause failures in playback that weren't present
	// during recording. Work around this be fetching pod locations once if
	// copyist is in use. This workaround will break any test that tries to
	// reuse the sql.DB to connect to a pod that was created later on, since the
	// new pod name won't be in the cached list of names. However, this is not
	// a big limitation, since that's very rare in our tests.
	pgxOptions.LookupFunc = func(ctx context.Context, _ string) (addrs []string, err error) {
		return LocatePods(ctx, ctl, options)
	}

	// Tunnel into the cluster using the pod dialer.
	dialer, err := kube.NewPodDialer(ctl.RestConfig(), ctl.Namespace())
	if err != nil {
		return pgxsql.Options{}, errors.Wrapf(err, "error creating pod dialer for namespace %s", ctl.Namespace())
	}
	pgxOptions.DialFunc = dialer.DialContext
	return pgxOptions, nil
}

// LocatePods returns the names of all pods that can be used to connect to the
// given tenant (can be regular or system tenant). If opts.UsesSqlProxy is true,
// then the connection passes through SQL proxy pods; otherwise, it goes
// directly to SQL pods (if a regular tenant) or KV pods (if a system tenant).
func LocatePods(ctx context.Context, ctl *kube.Ctl, opts *Options) (names []string, err error) {
	ctx, span := otel.GetTracerProvider().Tracer("kubenet").Start(ctx, "LocatePods")
	defer func() { tracing.EndWithError(span, err) }()

	const retryDelay = 100 * time.Millisecond

	// Ensure that pod is in the Running state.
	listOptions := &kube.ListOptions{
		LabelSelector: opts.Selector,
		FieldSelector: fields.ParseSelectorOrDie("status.phase=Running"),
	}

	var pods corev1.PodList
	for {
		if err = ctl.List(ctx, listOptions, &pods); err != nil {
			return nil, err
		}

		// We only want sqlproxy pods that are ready.
		if opts.UsesSqlProxy() && !opts.TenantID.IsSystemID() {
			readyPods := pods.Items[:0]
			for _, p := range pods.Items {
				if kube.IsReady(&p) == nil {
					readyPods = append(readyPods, p)
				}
			}
			pods.Items = readyPods

			if len(pods.Items) != 0 {
				break
			}

			// List the pods again after some time. In practice/production, we
			// should not hit this case, and there should be at least 1 ready
			// pod.
			_ = kube.SleepContext(ctx, retryDelay)
		} else {
			if len(pods.Items) != 0 || opts.TenantID.IsSystemID() {
				break
			}

			// If no pods are available and we're connecting directly to a
			// tenant, scale the tenant up to at least 1 pod and re-list. Do so
			// in a loop to ensure there are no race conditions. ensureTenantPod
			// will error out if any real errors occur.
			if err := autoscaler.EnsureTenantResumed(ctx, ctl, opts.TenantID); err != nil {
				return nil, err
			}
		}
	}

	// This can occur if we're connecting to the system tenant, and there are
	// no running CRDB pods. We should only return an empty error if there is at
	// least one pod present.
	if len(pods.Items) == 0 {
		return nil, errors.Newf("no available pods for tenant %d", opts.TenantID)
	}

	// Filter for ready pods when explicitly requested (e.g. DB Console proxy).
	// This prevents connecting to pods where CRDB is draining but the
	// Kubernetes pod phase is still Running.
	if opts.ReadyPodsOnly {
		readyPods := pods.Items[:0]
		for _, p := range pods.Items {
			if kube.IsReady(&p) == nil {
				readyPods = append(readyPods, p)
			}
		}
		pods.Items = readyPods

		if len(pods.Items) == 0 {
			return nil, errors.New("ready pods only required but there were none")
		}
	}

	names = make([]string, len(pods.Items))
	for i, pod := range pods.Items {
		names[i] = pod.Name
	}

	// Shuffle the names slice, so that connection is made to a random CRDB
	// node or SQL instance.
	rand.Shuffle(len(names), func(i, j int) {
		names[i], names[j] = names[j], names[i]
	})

	return names, nil
}
