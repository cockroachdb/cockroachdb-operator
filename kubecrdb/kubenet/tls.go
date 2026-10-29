package kubenet

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/crypto"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/crdbcerts"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
)

// createTlsConfig constructs a tls.Config struct that contains the information
// needed for Server authentication (server name and CA certificate) as well as
// Client authentication for the user (using a client certificate), if that is
// needed.
func createTlsConfig(ctl *kube.Ctl, options *Options) (*tls.Config, error) {
	// Select a ServerName depending on what's on the TLS certificate for each
	// kind of pod:
	//
	//   CRDB Pod  : cockroachdb-internal
	//   Proxy Pod : first DNS SAN of the proxy cert, with a leading * replaced by placeholder
	//   Warmpool Tenant Pod: tenant-<tenantId>
	//
	var serverName string
	if options.TenantID.IsSystemID() {
		serverName = manifests.InternalLoadBalancerService
	} else if options.UsesSqlProxy() {
		proxyCerts, err := crdbcerts.GetSqlProxyCertBundle(ctl)
		if err != nil {
			return nil, errors.Wrapf(err, "getting SQLProxy certificates")
		}
		serverName, err = generateServerName(proxyCerts.Certificate)
		if err != nil {
			return nil, errors.Wrapf(err, "generating .ServerName for SQLProxy connection")
		}
	} else {
		serverName = options.TenantID.Tag()
	}

	// Start with any provided TLS config fields and fill in any blanks.
	var tlsConfig *tls.Config
	if options.TLSConfig != nil {
		tlsConfig = options.TLSConfig.Clone()
	} else {
		tlsConfig = &tls.Config{}
	}

	// Determine authentication method.
	needsClientCert := false
	if _, hasPassword := options.User.Password(); hasPassword {
		if tlsConfig.Certificates != nil {
			return nil, errors.AssertionFailedf("cannot have both a password and a client certificate set")
		}
	} else {
		if tlsConfig.Certificates == nil {
			if options.User.Username() != "root" {
				return nil, errors.AssertionFailedf("user must be root if no client certificate is set")
			}
			needsClientCert = true
		}
	}

	if tlsConfig.RootCAs == nil || needsClientCert {
		// Get the CA certificate used to verify the server's TLS certificate
		// and the root certificate that may be used to log in.
		// TODO(andyk): The CA certificate for the SQL proxy is only the same as
		// the root certificate CA in testing scenarios. If we ever need to connect
		// through the SQL proxy in production scenarios, we'll need to add a way
		// to get the SQL proxy CA here.
		serverCACertPEM, rootCertBundle, err := crdbcerts.GetRootClientCertBundle(ctl)
		if err != nil {
			return nil, err
		}

		tempConfig, err := crypto.TLSConfigFromBundle(serverCACertPEM, rootCertBundle, serverName)
		if err != nil {
			return nil, errors.Wrap(err, "creating TLS config from bundle")
		}

		if tlsConfig.RootCAs == nil {
			tlsConfig.RootCAs = tempConfig.RootCAs
		}
		if needsClientCert {
			tlsConfig.Certificates = tempConfig.Certificates
		}
	}

	// Set default value of ServerName, if not specified.
	if tlsConfig.ServerName == "" {
		tlsConfig.ServerName = serverName
	}

	return tlsConfig, nil
}

// CreateUITlsConfig generates a tls.Config suitable for securely connecting to
// a CockroachDB's DB console for either dedicated clusters or tenants.
func CreateUITlsConfig(ctl *kube.Ctl, options *Options) (*tls.Config, error) {
	if options.TenantID.IsSystemID() {
		uiCerts, err := crdbcerts.GetUICertBundle(ctl)
		if err != nil {
			return nil, errors.Wrapf(err, "getting UI certificates")
		}

		serverName, err := generateServerName(uiCerts.Certificate)
		if err != nil {
			return nil, errors.Wrapf(err, "generating .ServerName for SQLProxy connection")
		}

		caCerts := x509.NewCertPool()

		return &tls.Config{
			// TODO(chrisseto): Once we have access to Let Encrypt's ca.crt via
			// Kubernetes secrets, remove InsecureSkipVerify. This matches the
			// current state of production and the underpinning for secure
			// communication are here, we just need LE's certificates provided
			// to us so staging AND production will work.
			InsecureSkipVerify: true,
			RootCAs:            caCerts,
			ServerName:         serverName,
		}, nil
	}

	caCert, _, err := crdbcerts.GetTenantClientCertBundle(ctl, options.TenantID)
	if err != nil {
		return nil, err
	}

	caCerts := x509.NewCertPool()
	if !caCerts.AppendCertsFromPEM(caCert) {
		return nil, errors.Errorf("failed to parse PEM data to pool")
	}

	return &tls.Config{
		RootCAs:    caCerts,
		ServerName: options.TenantID.Tag(),
	}, nil
}

// generateServerName returns a valid .ServerName attribute for a PEM encoded
// certificate. It should be used when connecting to a service with an unknown
// host name via unconventional means, IE the PodDialer.
func generateServerName(pemBytes []byte) (string, error) {
	block, _ := pem.Decode(pemBytes)

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}

	if len(cert.DNSNames) == 0 {
		return "", errors.New("certificate did not contain any DNSNames")
	}

	name := cert.DNSNames[0]
	if name[0] != '*' {
		return name, nil
	}

	return "placeholder" + name[1:], nil
}
