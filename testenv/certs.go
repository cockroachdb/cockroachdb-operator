package testenv

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/crypto"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CreateExternalCertsForBetaCluster creates external certs for the beta cluster.
func CreateExternalCertsForBetaCluster(env *Env, cluster *crdbv1beta1.CrdbCluster) error {
	ctl := env.DefaultCluster().Ctl
	ctx := context.Background()
	if cluster.Spec.Template.Spec.Certificates.ExternalCertificates == nil {
		return nil
	}
	signer, err := crypto.FakeSigner()
	if err != nil {
		return err
	}
	nodeBundle, err := signer.Sign(ctx, crypto.CertificateSigningRequest{
		Role:       "node",
		CommonName: "node",
		DNSNames: []string{
			cluster.Name,
			fmt.Sprintf("%s.%s", cluster.Name, cluster.Namespace),
			fmt.Sprintf("%s.%s.svc.cluster.local", cluster.Name, cluster.Namespace),
			fmt.Sprintf("*.%s", cluster.Name),
			fmt.Sprintf("*.%s.%s", cluster.Name, cluster.Namespace),
			fmt.Sprintf("*.%s.%s.svc.cluster.local", cluster.Name, cluster.Namespace),
			"localhost",
		},
		IPAddresses: []string{"127.0.0.1"},
		TTL:         365 * 24 * 60 * time.Minute,
	})
	if err != nil {
		return errors.Wrap(err, "failed to sign node bundle")
	}
	// httpBundle is signed with the same signing request as node bundle for now.
	// We need to fix this before the console web UI can be accessed.
	// TODO(ye): fix http bundle's signing request.
	httpBundle, err := signer.Sign(ctx,
		crypto.CertificateSigningRequest{
			Role:       "node",
			CommonName: "node",
			DNSNames: []string{
				cluster.Name,
				fmt.Sprintf("%s.%s", cluster.Name, cluster.Namespace),
				fmt.Sprintf("%s.%s.svc.cluster.local", cluster.Name, cluster.Namespace),
				fmt.Sprintf("*.%s", cluster.Name),
				fmt.Sprintf("*.%s.%s", cluster.Name, cluster.Namespace),
				fmt.Sprintf("*.%s.%s.svc.cluster.local", cluster.Name, cluster.Namespace),
				"localhost",
			},
			IPAddresses: []string{"127.0.0.1"},
			TTL:         365 * 24 * 60 * time.Minute,
		})
	if err != nil {
		return errors.Wrap(err, "failed to sign http bundle")
	}

	rootClientBundle, err := signer.Sign(ctx,
		crypto.CertificateSigningRequest{
			Role: "root",
			TTL:  365 * 24 * 60 * time.Minute,
		})
	if err != nil {
		return errors.Wrap(err, "failed to sign http bundle")
	}
	externalCerts := cluster.Spec.Template.Spec.Certificates.ExternalCertificates
	nodeCerts := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      externalCerts.NodeSecretName,
			Namespace: cluster.Namespace,
		},
		Data: kube.StringData(map[string]string{
			"tls.crt": string(nodeBundle.Certificate),
			"tls.key": string(nodeBundle.PrivateKey),
		}),
	}
	httpCerts := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      externalCerts.HTTPSecretName,
			Namespace: cluster.Namespace,
		},
		Data: kube.StringData(map[string]string{
			"tls.crt": string(httpBundle.Certificate),
			"tls.key": string(httpBundle.PrivateKey),
		}),
	}
	rootCerts := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      externalCerts.RootSQLClientSecretName,
			Namespace: cluster.Namespace,
		},
		Data: kube.StringData(map[string]string{
			"tls.crt": string(rootClientBundle.Certificate),
			"tls.key": string(rootClientBundle.PrivateKey),
		}),
	}
	caCert, err := signer.Certificate(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to get ca cert")
	}
	nodeCA := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      externalCerts.NodeCAConfigMapName,
			Namespace: cluster.Namespace,
		},
		Data: map[string]string{
			"ca.crt": string(caCert),
		},
	}
	ctlNS := ctl.WithNamespace(cluster.Namespace)
	return ctlNS.Apply(ctx, &nodeCerts, &httpCerts, &nodeCA, &rootCerts)
}
