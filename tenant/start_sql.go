package tenant

import (
	gozip "archive/zip"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"maps"
	"net/http"
	"path/filepath"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/crdbcerts"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// Note(alyshan): This env var is defined in our otel fork
	// See: https://github.com/cockroachdb/opentelemetry-operations-go/pull/1
	otelGcpCredentialsEnvVarName = "CLOUDSDK_AUTH_ACCESS_TOKEN_PATH"
	// otelGcpCredentialsFileName is the file name used to store the credentials.
	otelGcpCredentialsFileName = "access_token_creds"
)

// sendStartRequest sends a start request to the sqlpod-manager process running
// in the pod. The tenant archive sent as part of the start request will include
// tenant information such as certificates, and sidecar-related configurations.
// For sidecars that require credentials, their configurations will only be sent
// if the credentials are present as this would avoid such processes from
// crashing.
//
// NOTE: The pod has to be stamped with the tenant-id label beforehand.
var sendStartRequest = func(
	ctx context.Context,
	ctl *kube.Ctl,
	pod *corev1.Pod,
	tenant *crdbv1beta1.CrdbTenant,
	credentials map[string][]byte,
	nodeLocalityReader cloud.NodeLocalityReaderFunc,
) error {
	tenantID := tenant.Spec.TenantID

	// Validate that the tenant-id label is matches the given tenant.
	if label, ok := pod.Labels[manifests.TenantIDLabel]; !ok || label != tenantID.String() {
		return errors.AssertionFailedf("pod %s is not marked with tenant-id=%d",
			pod.Name, tenantID)
	}

	// Fetch the locality from the reader. Given that node names are tied to
	// their region/zone, this can easily use a caching mechanism under the hood
	// to avoid impacting cold start latency.
	locality, err := nodeLocalityReader(ctx, pod.Spec.NodeName)
	if err != nil {
		return err
	}

	// Retrieve SQL pod data from Kubernetes.
	contents, err := fetchSQLPodData(ctx, ctl, tenant, credentials, locality)
	if err != nil {
		return err
	}

	// Build the sqlstarter archive with tenant data (e.g. certs, otel config).
	buf, err := zip(contents)
	if err != nil {
		return err
	}

	// Send tenant certs in the form of an archive to the sqlstarter process in
	// the stamped pod.
	caCert, _, err := crdbcerts.GetSqlStarterCertBundle(ctl)
	if err != nil {
		return err
	}
	caCerts := x509.NewCertPool()
	if !caCerts.AppendCertsFromPEM(caCert) {
		return errors.Errorf("failed to parse PEM data for sqlstarter CA certificate")
	}
	client := &http.Client{
		Transport: &http.Transport{
			// Make the connection, send the certificates, then shut it down. Don't
			// put the connection into an HTTP connection pool.
			DisableKeepAlives: true,
			TLSClientConfig: &tls.Config{
				RootCAs: caCerts,
				// Verify the name on the server certificate.
				ServerName: manifests.SqlStarterServerName,
			},
		},
		// The default timeout is no timeout. We will set a timeout of 10s here.
		// Ideally, we expect this to be less than 100ms. Setting it too low
		// will trigger frequent retries on the client side. If the pending pod
		// fetched has already been started before this request gets sent, the
		// connection will be refused right away, or closed with an EOF.
		Timeout: 10 * time.Second,
	}

	// Connect using HTTPS.
	sqlstarterURL := fmt.Sprintf("https://%s:%d/start?tenant_id=%d",
		pod.Status.PodIP, manifests.SqlStarterPort, tenantID)
	req, err := http.NewRequest("POST", sqlstarterURL, buf)
	if err != nil {
		return errors.Wrap(err, "creating request to sqlstarter")
	}
	req.Header.Add("Content-Type", "binary/octet-stream")

	// The sqlpod-manager uses the X-Pod-UID header to reject requests that do
	// not have a matching pod UID. This is used to avoid race conditions in
	// the case where a pod crashed, and came back up as an unstamped pod with
	// the same IP. Note that an ideal solution to address this would be to use
	// a unique TLS cert that would verify the ServerName, but it seems too much
	// of a lift, so we resorted to this.
	req.Header.Add("X-Pod-Uid", string(pod.UID))

	// sqlstarter returns a non-200 status in the case of an error
	// It will get 200 if it manages to persist the tenant archive.
	resp, err := SendSqlStarterRequest(client, req, pod)
	if err != nil {
		return errors.Wrap(err, "sending HTTP request")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Read the error message from sqlstarter if any.
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return errors.Wrap(err, "reading HTTP response body")
		}
		return errors.Newf("sqlstarter request status %d error: %s", resp.StatusCode, bodyBytes)
	}
	return nil
}

// SendSqlStarterRequest sends the given HTTP request to a pod running the SQL
// starter process, which will in turn start up the SQL-only CRDB instance.
// NOTE: This is an exported variable to make it easy to hook in tests.
var SendSqlStarterRequest = func(
	client *http.Client, req *http.Request, pod *corev1.Pod,
) (*http.Response, error) {
	return client.Do(req)
}

// fetchSQLPodData retrieves everything needed to construct the tenant archive
// used for stamping.
//
// WARNING: We should make sure that anything that we add here is backwards
// compatible since the operator may still be sending stamping requests to
// old tenantpool pods.
func fetchSQLPodData(
	ctx context.Context,
	ctl *kube.Ctl,
	tenant *crdbv1beta1.CrdbTenant,
	credentials map[string][]byte,
	locality cloud.LocalityInfo,
) (map[string][]byte, error) {
	crdbCertsData, err := constructCrdbCerts(ctx, ctl, tenant, locality)
	if err != nil {
		return nil, errors.Wrap(err, "constructing cockroach-certs")
	}

	otelData, err := constructOtelCollectorData(ctx, ctl, tenant, credentials)
	if err != nil {
		return nil, errors.Wrap(err, "constructing otelcol-data")
	}

	fluentbitData, err := constructFluentbitData(ctx, ctl, tenant)
	if err != nil {
		return nil, errors.Wrap(err, "constructing fluent-bit-data")
	}

	data := make(map[string][]byte)
	maps.Copy(data, crdbCertsData)
	maps.Copy(data, otelData)
	maps.Copy(data, fluentbitData)
	return data, nil
}

// constructCrdbCerts retrieves all the tenant certificates needed by the CRDB
// process.
//
// WARNING: We should make sure that anything that we add here is backwards
// compatible since the operator may still be sending stamping requests to
// old tenantpool pods.
func constructCrdbCerts(
	ctx context.Context, ctl *kube.Ctl, tenant *crdbv1beta1.CrdbTenant, locality cloud.LocalityInfo,
) (map[string][]byte, error) {
	tenantID := tenant.Spec.TenantID
	data := make(map[string][]byte)

	// Retrieve tenant certs. We expect the following:
	// - ca.crt
	// - client-tenant.<tenant_id>.crt
	// - client-tenant.<tenant_id>.key
	// - client.root.crt
	// - client.root.key
	// - tenant-signing.<tenant_id>.crt
	// - tenant-signing.<tenant_id>.key
	var sqlClientCerts corev1.Secret
	if err := ctl.Get(
		ctx, manifests.MakeTenantClientCertSecretName(tenantID), &sqlClientCerts,
	); err != nil {
		return nil, err
	}
	for k, v := range sqlClientCerts.Data {
		data[makeCrdbCertsRelPath(k)] = v
	}

	// Write the locality file (--locality-file) for cockroachdb. We put this in
	// the same directory as the tenant-id-file to ensure that the moment the
	// tenant-id-file is written, the locality-file is up-to-date. Using a
	// different directory does not guarantee that behavior, and we cannot rely
	// on the tenant-id-file to refresh the locality-file in the cockroach
	// process.
	//
	// The newline character is unnecessary here, but given that the database
	// handles it, we will write it for readability, in case someone reads the
	// file directly.
	data[makeCrdbCertsRelPath(manifests.TenantPodLocalityFileName)] = []byte(locality.String() + "\n")

	// Write checkpoint file for cockroachdb. This will be used as --tenant-id-file.
	tenantIDData := []byte(tenantID.String() + "\n")
	data[makeCrdbCertsRelPath(manifests.TenantPodCheckpointFileName)] = tenantIDData

	return data, nil
}

// constructOtelCollectorData retrieves everything needed by the data directory
// for the OpenTelemetry Collector process. This will return (nil, nil) if the
// collector process isn't enabled, or if we're not ready to start it yet.
//
// WARNING: We should make sure that anything that we add here is backwards
// compatible since the operator may still be sending stamping requests to
// old tenantpool pods.
func constructOtelCollectorData(
	ctx context.Context, ctl *kube.Ctl, tenant *crdbv1beta1.CrdbTenant, credentials map[string][]byte,
) (map[string][]byte, error) {
	spec := tenant.Spec.OtelCollector

	// The collector process isn't enabled.
	if spec == nil {
		return nil, nil
	}

	// This directory requires credentials, but isn't supplied one, so skip for
	// now. We will only start the process if the credentials are present. The
	// drawback here is that metrics/logs will be delayed by a few seconds, but
	// we'd imagine this to be < 1-2 seconds in practice. To add on, we only
	// scrape CRDB once every 30 seconds, so the delay is insignificant.
	if (spec.AWSMetricsRoleARN != "" || spec.AWSLogsRoleARN != "" || spec.GCPLogsServiceAccount != "") && credentials == nil {
		return nil, nil
	}

	data := make(map[string][]byte)

	// We expect that the config map is present, since it was validated. If we
	// couldn't find it at this point, assume that is not expected, and return
	// an error. The same argument applies to the secret.
	var otelConfig corev1.ConfigMap
	if err := ctl.Get(ctx, spec.ConfigMapName, &otelConfig); err != nil {
		return nil, err
	}
	data[makeOtelCollectorRelPath(manifests.OtelCollectorConfigFileName)] = []byte(otelConfig.Data["config"])

	if spec.SecretName != "" {
		var otelSecret corev1.Secret
		if err := ctl.Get(ctx, spec.SecretName, &otelSecret); err != nil {
			return nil, err
		}
		// All secrets are written as files with .env so that they are
		// injected as environment variables into the sidecar process.
		for k, v := range otelSecret.Data {
			data[makeOtelCollectorRelPath(fmt.Sprintf("%s.env", k))] = v
		}
	}

	// AWS metrics credentials.
	if spec.AWSMetricsRoleARN != "" {
		metricsCred, ok := credentials[spec.AWSMetricsRoleARN]
		if !ok {
			return nil, errors.Newf(`could not locate AWS metrics credentials for "%s"`, spec.AWSMetricsRoleARN)
		}
		data[makeOtelCollectorRelPath(manifests.OtelCollectorAWSMetricsCredentialsFileName)] = metricsCred
	}
	// AWS logs credentials.
	if spec.AWSLogsRoleARN != "" {
		logsCred, ok := credentials[spec.AWSLogsRoleARN]
		if !ok {
			return nil, errors.Newf(`could not locate AWS logs credentials for "%s"`, spec.AWSLogsRoleARN)
		}
		data[makeOtelCollectorRelPath(manifests.OtelCollectorAWSLogsCredentialsFileName)] = logsCred
	}

	// GCP logs credentials.
	if spec.GCPLogsServiceAccount != "" {
		logsCred, ok := credentials[spec.GCPLogsServiceAccount]
		if !ok {
			return nil, errors.Newf(`could not locate GCP logs credentials for "%s"`, spec.GCPLogsServiceAccount)
		}
		// Set the environment variable to the file path where we will store the credentials.
		data[makeOtelCollectorRelPath(fmt.Sprintf("%s.env", otelGcpCredentialsEnvVarName))] = []byte(filepath.Join(manifests.OtelCollectorVolumePath, otelGcpCredentialsFileName))
		// Store the credentials at the file path.
		data[makeOtelCollectorRelPath(otelGcpCredentialsFileName)] = logsCred
	}

	// Write checkpoint file for otelcol-agent. This will be used as --tenant-id-file.
	tenantIDData := []byte(tenant.Spec.TenantID.String() + "\n")
	data[makeOtelCollectorRelPath(manifests.TenantPodCheckpointFileName)] = tenantIDData

	return data, nil
}

// constructFluentbitData retrieves everything needed by the data directory
// for the fluentbit process.
//
// WARNING: We should make sure that anything that we add here is backwards
// compatible since the operator may still be sending stamping requests to
// old tenantpool pods.
func constructFluentbitData(
	ctx context.Context, ctl *kube.Ctl, tenant *crdbv1beta1.CrdbTenant,
) (map[string][]byte, error) {
	spec := tenant.Spec.Fluentbit
	data := make(map[string][]byte)

	// We need the CRDB process to run in unstamped SQL pods as part of reducing
	// cold starts. However, it is not trivial to conditionally set the CRDB
	// logging config as the logging configuration for CRDB is initialized at
	// startup time, and does not currently support reloading.
	//
	// To address the above, we will configure SQL pods to push logs to the
	// fluentbit sidecar. This sidecar will then either forward logs to the
	// OpenTelemetry Collector process if log export is enabled, or discard the
	// logs if log export is disabled. Due to that, this sidecar will always
	// run.
	fluentbitConfig := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			// The no-op configmap configures fluentbit to always discards logs.
			Name: manifests.TenantNoopConfigMapName,
		},
	}
	if spec != nil {
		fluentbitConfig.Name = spec.ConfigMapName
	}
	if err := ctl.Get(ctx, fluentbitConfig.Name, fluentbitConfig); err != nil {
		return nil, err
	}
	for fileName := range fluentbitConfig.Data {
		data[makeFluentBitCollectorRelPath(fileName)] = []byte(fluentbitConfig.Data[fileName])
	}

	// Write checkpoint file for fluentbit. This will be used as --tenant-id-file.
	tenantIDData := []byte(tenant.Spec.TenantID.String() + "\n")
	data[makeFluentBitCollectorRelPath(manifests.TenantPodCheckpointFileName)] = tenantIDData

	return data, nil
}

// makeCrdbCertsRelPath returns a path relative to root for the cockroachdb
// container.
func makeCrdbCertsRelPath(filename string) string {
	return fmt.Sprintf("%s/%s", manifests.CockroachCertsTenantVolumePath[1:], filename)
}

// makeOtelCollectorRelPath returns a path relative to root for the
// otelcol-agent container.
func makeOtelCollectorRelPath(filename string) string {
	return fmt.Sprintf("%s/%s", manifests.OtelCollectorVolumePath[1:], filename)
}

// makeFluentBitCollectorRelPath returns a path relative to root for the
// fluent-bit container.
func makeFluentBitCollectorRelPath(filename string) string {
	return fmt.Sprintf("%s/%s", manifests.FluentbitVolumePath[1:], filename)
}

// zip writes contents into a new zip archive. Note that this only supports
// zipping archives with top-level files.
func zip(contents map[string][]byte) (io.Reader, error) {
	buf := new(bytes.Buffer)
	zipWriter := gozip.NewWriter(buf)
	for filename, data := range contents {
		f, err := zipWriter.Create(filename)
		if err != nil {
			return nil, errors.Wrap(err, "creating file")
		}
		if _, err = f.Write(data); err != nil {
			return nil, errors.Wrap(err, "writing bytes")
		}
	}
	if err := zipWriter.Close(); err != nil {
		return nil, errors.Wrap(err, "closing zip writer")
	}
	return buf, nil
}
