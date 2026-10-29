package crdbcerts

import (
	"bytes"
	"context"
	"encoding/pem"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/crypto"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	corev1 "k8s.io/api/core/v1"
)

// CrdbClusterCertOpts encapsulates various options required for creating all
// the certificates used by a CockroachDB Cluster.
type CrdbClusterCertOpts struct {
	// IsHost indicates whether or not host specific certificates should be
	// signed and installed as well.
	IsHost bool
	// IsCustomerAccount indicates whether or not certificates sensitive to
	// being created in customer cloud accounts should be created or not.
	// Currently, this is the UI certificate.
	IsCustomerAccount bool
	// AlternateClusterDomain is another Kubernetes cluster domain to mint
	// certificates with, in addition to .cluster.local, in order to support
	// cluster domain based DNS chaining.
	// E.g. "cluster.other"
	DeprecatedAlternateClusterDomain string
	AlternateClusterDomain           string
	// AltNodeNames is list of SANs that will be added to the node certificate.
	AltNodeNames []string
	// ExternalCert is a Let's Encrypt, or otherwise trusted, certificate that
	// will be used for Cockroach's HTTP traffic and SQLProxy traffic, if
	// IsHost is true.
	ExternalCert *crypto.Bundle
	// ServerlessDNS specifies the DNS suffix for all Serverless clusters on a
	// host cluster in <host-cluster-short-id>.<region>.<root> format. It is not
	// used if IsHost=false.
	ServerlessDNS string

	// AdditionalSQLClientCA is an optional client-supplied CA certificate used for
	// client authentication. When supplied, this is appended to ca-client.crt on
	// the CRDB nodes allowing the existing (generated) cert as well as this one to
	// be used for authentication.
	//
	// It is expected that this CA be in PEM format.
	AdditionalSQLClientCA []byte
}

// CreateCrdbClusterCerts signs the certificates needed for a CRDB cluster
// (StatefulSet or Operator) using the provided signer, and then installs them
// as secrets in the K8s cluster. If "IsHostCluster" is true, then the SQLProxy
// and SQLStarter certificates are installed in addition to the usual dedicated
// cluster certificates.
func CreateCrdbClusterCerts(
	ctx context.Context, signer crypto.CertificateSigner, ctl *kube.Ctl, opts CrdbClusterCertOpts,
) error {
	namespace := ctl.Namespace()

	secrets := manifests.CrdbClusterCertConfigs(
		namespace,
		opts.DeprecatedAlternateClusterDomain,
		opts.AlternateClusterDomain,
		opts.ExternalCert,
		opts.AltNodeNames,
		opts.IsCustomerAccount,
	)

	if opts.IsHost {
		proxyCertConfig, err := manifests.SqlProxyCertConfig(opts.ServerlessDNS)
		if err != nil {
			return err
		}
		// Note that the same TLS certificate is used by the SQL proxy as by the
		// DB Console UI, since both are external entry points to the cluster.
		secrets = append(secrets, proxyCertConfig)
		secrets = append(secrets, manifests.SqlStarterCertConfig())
		secrets = append(secrets, manifests.TenantDirCertConfig())
	}

	var objects []kube.Object
	for _, secret := range secrets {
		// Some (the proxy) certs are signed through the ACME protocol which
		// may be rate limited. So we check if this is a retry and if there is
		// already an existing cert. If there is already an existing cert with
		// a matching hash, then we can skip regenerating the cert.
		s := &corev1.Secret{}
		err := ctl.Get(ctx, secret.ObjectMeta.Name, s)
		if err != nil && !kubeerrors.IsNotFound(err) {
			return err
		}
		if err == nil {
			revisionHash, err := secret.RevisionHash()
			if err != nil {
				return err
			}
			deployedRevisionHash, existingSecretHasHash := s.Labels[crypto.LabelSecretRevisionHash]
			if existingSecretHasHash && deployedRevisionHash == revisionHash {
				continue
			}
		}
		object, err := secret.ToK8SSecret(ctx, signer)
		if err != nil {
			return errors.Wrapf(err, "unable to generate secret %s", secret.ObjectMeta.Name)
		}
		objects = append(objects, object)
	}

	// If we're making certificates for an operator based cluster, we need to
	// place the signer's CA cert into a ConfigMap as it won't be bundled with our
	// other certs.
	ca, err := signer.Certificate(ctx)
	if err != nil {
		return errors.Wrap(err, "couldn't get signer CA for node")
	}
	caBlocks := pemDecodeAllCertificates(ca)
	if len(caBlocks) == 0 {
		return errors.New("signer CA contained no CERTIFICATE PEM blocks")
	}

	caBundle := new(bytes.Buffer)
	for _, block := range caBlocks {
		if err := pem.Encode(caBundle, block); err != nil {
			return errors.Wrap(err, "encoding default CA")
		}
	}

	// Node CA only contains our CA
	objects = append(objects, manifests.CrdbNodeCA(caBundle.Bytes()))

	additionalCABlocks := pemDecodeAllCertificates(opts.AdditionalSQLClientCA)
	if len(opts.AdditionalSQLClientCA) > 0 && len(additionalCABlocks) == 0 {
		return errors.New("additional SQL client CA did not contain any CERTIFICATE PEM blocks")
	}
	clientCABlocks := append(caBlocks, additionalCABlocks...)

	clientCABundle := new(bytes.Buffer)
	for _, block := range clientCABlocks {
		if err := pem.Encode(clientCABundle, block); err != nil {
			return errors.Wrap(err, "encoding client CA bundle")
		}
	}

	// Client CA contains our CA and an optional additional CA
	objects = append(objects, manifests.CrdbClientCA(clientCABundle.Bytes()))

	return errors.Wrapf(ctl.Apply(ctx, objects...), "applying cluster secrets in %s", ctl.Namespace())
}

// CreateTenantCerts signs the certificates needed for the given tenant using
// the provided signer, and then installs them as secrets in the K8s cluster.
func CreateTenantCerts(
	ctx context.Context,
	signer crypto.CertificateSigner,
	ctl *kube.Ctl,
	tenantID crdbv1beta1.TenantID,
	alternateClusterDomain string,
) error {
	config := manifests.TenantCertConfig(tenantID, ctl.Namespace(), alternateClusterDomain)
	secret, err := config.ToK8SSecret(ctx, signer)
	if err != nil {
		return err
	}
	return ctl.Apply(ctx, secret)
}

func pemDecodeAllCertificates(cert []byte) []*pem.Block {
	var blocks []*pem.Block
	for block, rest := pem.Decode(cert); block != nil; block, rest = pem.Decode(rest) {
		if block.Type == "CERTIFICATE" {
			blocks = append(blocks, block)
		}
	}
	return blocks
}
