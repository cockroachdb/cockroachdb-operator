package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/buildinfo"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/services/init-container/pkg/encryption"
	"github.com/cockroachlabs/cockroach-enterprise-operator/services/init-container/pkg/locality"
	"gopkg.in/yaml.v2"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	defaultLocalityMountPath = "/etc/cockroach-locality"

	// Path at which decrypted store keys will be placed
	defaultDecryptedKeyMountPath = "/etc/cockroach-key"

	// Path at which backup encrypted store keys will be placed
	defaultEncryptedKeyMountPath = "/cockroach/cockroach-data"

	// File name of a cluster's new store key, which will be used to
	// encrypt the cluster
	defaultNewStoreKeyFile = "new_store.key"

	// File name of the cluster's old store key, which is used while rotating
	// away from an existing key or disabling encryption entirely.
	defaultOldStoreKeyFile = "old_store.key"

	// If "1", locality checker will error if required zone/node labels
	// are missing
	envErrorOnMissingLabels = "ERROR_ON_MISSING_LABELS"

	// The k8s node that a cluster is running on
	envKubernetesNode = "KUBERNETES_NODE"

	// If we are using a new store key, this specifies the cloud KMS
	// platform that should be used to decrypt the key. If empty when the
	// use-new-store-key flag is true, this means encryption-at-rest
	// without the envelope encryption layer will be used.
	envCMEKPlatform = "CMEK_PLATFORM"

	// AuthPrincipal is the principal that CC assumes to query the
	// customer's cloud KMS.
	envAuthPrincipal = "AUTH_PRINCIPAL"
	// KeyURI is the unique identifier of the customer's KMS key.
	envKeyURI = "KEY_URI"
	// KeyRegion is the region that the customer's KMS key is in.
	envKeyRegion = "KEY_REGION"

	// Credentials needed to assume the role of our own user on AWS. Once
	// we have assumed our own user's role, we then use this user to
	// assume the customer's role that allows us to use their KMS.
	envAWSAccessKeyId     = "AWS_ACCESS_KEY_ID"
	envAWSSecretAccessKey = "AWS_SECRET_ACCESS_KEY"
	// Needed to assume the customer's role in our setup, in order to
	// avoid the confused deputy problem.
	envAWSExternalId = "AWS_EXTERNAL_ID"

	// Credentials needed to authenticate to our GCP service account. Once
	// we have done this, we use this user to impersonate the customer's
	// service account that allows us to use their KMS.
	envGCPServiceAccountKey = "GCP_SERVICE_ACCOUNT_KEY"
	envAzureCredentialsFile = "COCKROACH_AZURE_APPLICATION_CREDENTIALS_FILE"

	// Old key CMEK env vars, used when the old store key was encrypted with
	// a (possibly different) cloud KMS.
	envOldCMEKPlatform         = "OLD_CMEK_PLATFORM"
	envOldAuthPrincipal        = "OLD_AUTH_PRINCIPAL"
	envOldKeyURI               = "OLD_KEY_URI"
	envOldKeyRegion            = "OLD_KEY_REGION"
	envOldAWSAccessKeyId       = "OLD_AWS_ACCESS_KEY_ID"
	envOldAWSSecretAccessKey   = "OLD_AWS_SECRET_ACCESS_KEY"
	envOldAWSExternalId        = "OLD_AWS_EXTERNAL_ID"
	envOldGCPServiceAccountKey = "OLD_GCP_SERVICE_ACCOUNT_KEY"
	envOldAzureCredentialsFile = "OLD_COCKROACH_AZURE_APPLICATION_CREDENTIALS_FILE"
)

var (
	localityDest     = flag.String("locality-dest", defaultLocalityMountPath, "directory to which locality files are written")
	localityMappings = flag.String("locality-mappings", "", "JSON-encoded locality mappings")
)

var defaultLocalityMappings = []locality.LocalityMapping{
	{NodeLabel: "topology.kubernetes.io/region", LocalityLabel: "region"},
	{NodeLabel: "topology.kubernetes.io/zone", LocalityLabel: "zone"},
}

var (
	useNewStoreKey      = flag.Bool("use-new-store-key", false, "k8s secret in which new store key information is stored")
	version             = flag.Bool("version", false, "prints version information")
	skipLocalityCompute = flag.Bool("skip-locality-compute", false, "if true, locality information will not be computed and written to the destination path")

	// CockroachDB expects different names depending on how the store consumes
	// the keys. A conventional store reads the paths named verbatim in
	// --enterprise-encryption, while a basalt store is handed the directory via
	// --cluster-key-dir and looks for "current" and "old" inside it.
	newStoreKeyFile = flag.String("new-store-key-file", defaultNewStoreKeyFile,
		"file name to write the decrypted new store key under")
	oldStoreKeyFile = flag.String("old-store-key-file", defaultOldStoreKeyFile,
		"file name to write the decrypted old store key under")
)

func main() {
	flag.Parse()

	if *version {
		buildinfo.Print("init-container")
		return
	}

	ctx := context.Background()

	nodeName := os.Getenv(envKubernetesNode)
	if nodeName == "" {
		log.Fatalf("%s must be set", envKubernetesNode)
	}
	clientset, err := buildClientset()
	if err != nil {
		log.Fatalf("error building clientset: %v", err)
	}

	if !*skipLocalityCompute {
		errorOnMissingLabels := os.Getenv(envErrorOnMissingLabels)

		var mappings []locality.LocalityMapping
		if *localityMappings != "" {
			if err := json.Unmarshal([]byte(*localityMappings), &mappings); err != nil {
				log.Fatalf("error unmarshalling locality mappings: %v", err)
			}
		} else {
			mappings = defaultLocalityMappings
		}

		l := locality.LocalityChecker{
			Clientset:            clientset,
			NodeName:             nodeName,
			WritePath:            *localityDest,
			ErrorOnMissingLabels: errorOnMissingLabels == "1",
			Mappings:             mappings,
		}
		if err := l.WriteLocality(ctx); err != nil {
			log.Fatalf("error writing locality: %v", err)
		}
	}

	// If we are using a new store key, ensure it's all set up.
	if *useNewStoreKey {
		setupCMEK(ctx)
	}
}

func setupCMEK(ctx context.Context) {
	kmsClient := buildKMSClient(kmsConfig{
		Platform:             os.Getenv(envCMEKPlatform),
		AuthPrincipal:        os.Getenv(envAuthPrincipal),
		KeyURI:               os.Getenv(envKeyURI),
		KeyRegion:            os.Getenv(envKeyRegion),
		AWSAccessKeyID:       os.Getenv(envAWSAccessKeyId),
		AWSSecretAccessKey:   os.Getenv(envAWSSecretAccessKey),
		AWSExternalID:        os.Getenv(envAWSExternalId),
		GCPServiceAccountKey: os.Getenv(envGCPServiceAccountKey),
		AzureCredentialsFile: os.Getenv(envAzureCredentialsFile),
	})

	// Build a KMSClient for decrypting the old store key. If no old CMEK
	// platform is set, the old key is not CMEK-encrypted and oldKMSClient
	// stays nil so the handler falls back to raw base64 decode.
	var oldKMSClient encryption.KMSClient
	oldCMEKPlatform := os.Getenv(envOldCMEKPlatform)
	if oldCMEKPlatform != "" {
		oldKMSClient = buildKMSClient(kmsConfig{
			Platform:             oldCMEKPlatform,
			AuthPrincipal:        os.Getenv(envOldAuthPrincipal),
			KeyURI:               os.Getenv(envOldKeyURI),
			KeyRegion:            os.Getenv(envOldKeyRegion),
			AWSAccessKeyID:       os.Getenv(envOldAWSAccessKeyId),
			AWSSecretAccessKey:   os.Getenv(envOldAWSSecretAccessKey),
			AWSExternalID:        os.Getenv(envOldAWSExternalId),
			GCPServiceAccountKey: os.Getenv(envOldGCPServiceAccountKey),
			AzureCredentialsFile: os.Getenv(envOldAzureCredentialsFile),
		})
	}

	eh := encryption.EncryptionHandler{
		DecryptedKeyMountPath: defaultDecryptedKeyMountPath,
		EncryptedKeyMountPath: defaultEncryptedKeyMountPath,
		NewStoreKeyFile:       *newStoreKeyFile,
		OldStoreKeyFile:       *oldStoreKeyFile,
		KMSClient:             kmsClient,
		OldKMSClient:          oldKMSClient,
	}
	if err := eh.WriteNewStoreKey(ctx); err != nil {
		log.Fatalf("error writing new store key: %v", err)
	}
}

type kmsConfig struct {
	Platform             string
	AuthPrincipal        string
	KeyURI               string
	KeyRegion            string
	AWSAccessKeyID       string
	AWSSecretAccessKey   string
	AWSExternalID        string
	GCPServiceAccountKey string
	AzureCredentialsFile string
}

// buildKMSClient constructs a KMSClient for the given platform and credentials.
// TODO: when platform is empty we return a NullKMSClient that does a no-op
// passthrough decrypt. WriteNewStoreKey could skip the KMS path entirely in
// that case. Also consider splitting WriteNewStoreKey into Decrypt and Write.
func buildKMSClient(cfg kmsConfig) encryption.KMSClient {
	switch cfg.Platform {
	case "":
		return &encryption.NullKMSClient{}
	case "AWS_KMS":
		return &encryption.AWSKMSClient{
			CloudKMSClient: encryption.CloudKMSClient{
				AuthPrincipal: cfg.AuthPrincipal,
				KeyURI:        cfg.KeyURI,
				KeyRegion:     cfg.KeyRegion,
			},
			AccessKeyID:     cfg.AWSAccessKeyID,
			SecretAccessKey: cfg.AWSSecretAccessKey,
			ExternalID:      cfg.AWSExternalID,
		}
	case "GCP_CLOUD_KMS":
		return &encryption.GCPKMSClient{
			CloudKMSClient: encryption.CloudKMSClient{
				AuthPrincipal: cfg.AuthPrincipal,
				KeyURI:        cfg.KeyURI,
			},
			EncodedServiceAccountKey: cfg.GCPServiceAccountKey,
		}
	case "AZURE_KEY_VAULT":
		return &encryption.AzureKMSWorkloadIdentityClient{
			CloudKMSClient: encryption.CloudKMSClient{
				AuthPrincipal: cfg.AuthPrincipal,
				KeyURI:        cfg.KeyURI,
			},
			ClusterIdentityClientID: getClusterIdentityClientIDFromFile(cfg.AzureCredentialsFile),
		}
	case "NULL_KMS":
		return &encryption.NullKMSClient{}
	default:
		log.Fatalf("unsupported CMEK platform: %s", cfg.Platform)
		return nil
	}
}

type azureCredentialsYAML struct {
	AzureTenantID     string `yaml:"azure_tenant_id"`
	AzureClientID     string `yaml:"azure_client_id"`
	AzureClientSecret string `yaml:"azure_client_secret"`
}

// getClusterIdentityClientIDFromFile reads an Azure application credentials
// YAML file and returns the client ID. If fileName is empty, it falls back to
// the COCKROACH_AZURE_APPLICATION_CREDENTIALS_FILE env var.
func getClusterIdentityClientIDFromFile(fileName string) string {
	if fileName == "" {
		fileName = os.Getenv(envAzureCredentialsFile)
	}

	data, err := os.ReadFile(fileName)
	if err != nil {
		log.Fatalf("failed to read credentials file %s: %v. Please ensure the Azure credentials secret '%s' is properly configured with valid credentials", fileName, err, manifests.AzureCredentialsSecretName)
	}

	var credsYAML azureCredentialsYAML
	if err := yaml.UnmarshalStrict(data, &credsYAML); err != nil {
		log.Fatalf("failed to unmarshal credentials from %s: %v", fileName, err)
	}

	if credsYAML.AzureClientID == "" {
		log.Fatalf("missing client ID in credentials file %s", fileName)
	}

	return credsYAML.AzureClientID
}

func buildClientset() (kubernetes.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, errors.Wrap(err, "error building kubernetes cluster config")
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(err, "building clientset failed")
	}
	return clientset, nil
}
