package mtls

import (
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// GrpcClientCredentials creates client credentials from certificates in the
// provided directory and file prefix.
//
// The directory is expected to contain a ca.crt, <keyname>.key, and
// <keyname>.crt files.
func GrpcClientCredentials(directory string, keyname string) (grpc.DialOption, error) {
	// TODO(jeffswenson): support certificate rotation without restarting the
	// pod. The idea is to create a credentials wrapper that checks if the cert
	// needs to be rotated and creates a new set of credentials if so.
	ca, cert, err := readCert(directory, keyname)
	if err != nil {
		return nil, err
	}
	return grpc.WithTransportCredentials(credentials.NewTLS(ClientTLSConfig(cert, ca))), nil
}

// GrpcClientCredentials creates server credentials from certificates in the
// provided directory and file prefix.
//
// The directory is expected to contain a ca.crt, <keyname>.key, and
// <keyname>.crt files.
func GrpcServerCredentials(
	directory string, keyname string, authorizedClientURIs []string,
) (grpc.ServerOption, error) {
	// TODO(jeffswenson): support certificate rotation without restarting the
	// pod. The idea is to create a credentials wrapper that checks if the cert
	// needs to be rotated and creates a new set of credentials if so.
	ca, cert, err := readCert(directory, keyname)
	if err != nil {
		return nil, err
	}
	return grpc.Creds(credentials.NewTLS(ServerTLSConfig(cert, ca, authorizedClientURIs))), nil
}
