package manifests

import (
	"encoding/json"
	"testing"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// newTestPod is a test helper function that wraps CrdbNodePod for easier testing.
// It creates a pod from a CrdbNode using a dummy Kubernetes node.
func newTestPod(node *crdbv1beta1.CrdbNode) *corev1.Pod {
	return CrdbNodePod(node, &corev1.Node{})
}

func TestNodePod(t *testing.T) {
	testCases := []struct {
		Name string
		Node crdbv1beta1.CrdbNode
	}{
		{
			Name: "simple",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
					Annotations: map[string]string{
						"crdb.cockroachlabs.com/cloudProvider": "gcp",
					},
					Labels: map[string]string{
						"crdb.cockroachlabs.com/cluster": "crdb-test",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Certificates: crdbv1beta1.Certificates{
						ExternalCertificates: &crdbv1beta1.ExternalCertificates{
							ClientCAConfigMapName:   "client-ca",
							HTTPSecretName:          "ui-certs",
							NodeCAConfigMapName:     "node-ca",
							NodeClientSecretName:    "node-client-certs",
							NodeSecretName:          "node-certs",
							RootSQLClientSecretName: "client-certs",
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "testpvc",
								Namespace: "testns",
							},
						},
					},
					Image:                "testimage",
					ServiceAccountName:   "testsa",
					LoggingConfigMapName: "logging-test",
					LocalityLabels:       []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone},
				},
			},
		},
		{
			Name: "simple-with-initcontainer",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
					Annotations: map[string]string{
						"crdb.cockroachlabs.com/cloudProvider": "gcp",
					},
					Labels: map[string]string{
						"crdb.cockroachlabs.com/cluster": "crdb-test",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Certificates: crdbv1beta1.Certificates{
						ExternalCertificates: &crdbv1beta1.ExternalCertificates{
							ClientCAConfigMapName:   "client-ca",
							HTTPSecretName:          "ui-certs",
							NodeCAConfigMapName:     "node-ca",
							NodeClientSecretName:    "node-client-certs",
							NodeSecretName:          "node-certs",
							RootSQLClientSecretName: "client-certs",
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "testpvc",
								Namespace: "testns",
							},
						},
					},
					Image:                "testimage",
					InitContainerImage:   "us-docker.pkg.dev/cockroach-cloud-images/data-plane/init-container:latest",
					ServiceAccountName:   "testsa",
					LoggingConfigMapName: "logging-test",
					LocalityLabels:       []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone},
				},
			},
		},
		{
			Name: "simple-with-placement",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
					Annotations: map[string]string{
						"crdb.cockroachlabs.com/cloudProvider": "gcp",
					},
					Labels: map[string]string{
						"crdb.cockroachlabs.com/cluster": "crdb-test",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Certificates: crdbv1beta1.Certificates{
						ExternalCertificates: &crdbv1beta1.ExternalCertificates{
							ClientCAConfigMapName:   "client-ca",
							HTTPSecretName:          "ui-certs",
							NodeCAConfigMapName:     "node-ca",
							NodeClientSecretName:    "node-client-certs",
							NodeSecretName:          "node-certs",
							RootSQLClientSecretName: "client-certs",
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "testpvc",
								Namespace: "testns",
							},
						},
					},
					Image:                "testimage",
					ServiceAccountName:   "testsa",
					LoggingConfigMapName: "logging-test",
					Tolerations: []corev1.Toleration{
						{
							Key:      "test",
							Operator: corev1.TolerationOpEqual,
							Effect:   corev1.TaintEffectNoSchedule,
						},
					},
					NodeSelector: map[string]string{
						"label": "value",
					},
					LocalityLabels: []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone},
				},
			},
		},
		{
			Name: "simple-with-affinity",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
					Annotations: map[string]string{
						"crdb.cockroachlabs.com/cloudProvider": "gcp",
					},
					Labels: map[string]string{
						"crdb.cockroachlabs.com/cluster": "crdb-test",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Certificates: crdbv1beta1.Certificates{
						ExternalCertificates: &crdbv1beta1.ExternalCertificates{
							ClientCAConfigMapName:   "client-ca",
							HTTPSecretName:          "ui-certs",
							NodeCAConfigMapName:     "node-ca",
							NodeClientSecretName:    "node-client-certs",
							NodeSecretName:          "node-certs",
							RootSQLClientSecretName: "client-certs",
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "testpvc",
								Namespace: "testns",
							},
						},
					},
					Image:                "testimage",
					ServiceAccountName:   "testsa",
					LoggingConfigMapName: "logging-test",
					Tolerations: []corev1.Toleration{
						{
							Key:      "test",
							Operator: corev1.TolerationOpEqual,
							Effect:   corev1.TaintEffectNoSchedule,
						},
					},
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{
												Key:      corev1.LabelTopologyZone,
												Operator: corev1.NodeSelectorOpIn,
												Values:   []string{"antarctica-east1", "antarctica-west1"},
											},
										},
									},
								},
							},
						},
					},
					LocalityLabels: []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone},
				},
			},
		},
		{
			Name: "simple-with-flags",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
					Annotations: map[string]string{
						"crdb.cockroachlabs.com/cloudProvider": "gcp",
					},
					Labels: map[string]string{
						"crdb.cockroachlabs.com/cluster": "crdb-test",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Certificates: crdbv1beta1.Certificates{
						ExternalCertificates: &crdbv1beta1.ExternalCertificates{
							ClientCAConfigMapName:   "client-ca",
							HTTPSecretName:          "ui-certs",
							NodeCAConfigMapName:     "node-ca",
							NodeClientSecretName:    "node-client-certs",
							NodeSecretName:          "node-certs",
							RootSQLClientSecretName: "client-certs",
						},
					},
					Flags: map[string]string{
						"--max-offsets": "100ms",
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "testpvc",
								Namespace: "testns",
							},
						},
					},
					Image:              "testimage",
					ServiceAccountName: "testsa",
					LocalityLabels:     []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone},
				},
			},
		},
		{
			Name: "simple without cloud prefix",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
					Annotations: map[string]string{
						"crdb.cockroachlabs.com/cloudProvider": "gcp",
					},
					Labels: map[string]string{
						"crdb.cockroachlabs.com/cluster": "crdb-test",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Certificates: crdbv1beta1.Certificates{
						ExternalCertificates: &crdbv1beta1.ExternalCertificates{
							ClientCAConfigMapName:   "client-ca",
							HTTPSecretName:          "ui-certs",
							NodeCAConfigMapName:     "node-ca",
							NodeClientSecretName:    "node-client-certs",
							NodeSecretName:          "node-certs",
							RootSQLClientSecretName: "client-certs",
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "testpvc",
								Namespace: "testns",
							},
						},
					},
					Image:                     "testimage",
					ServiceAccountName:        "testsa",
					NoCloudPrefixedLocalities: true,
					LocalityLabels:            []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone},
				},
			},
		},
		{
			Name: "shared-ca",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
					Annotations: map[string]string{
						"crdb.cockroachlabs.com/cloudProvider": "gcp",
					},
					Labels: map[string]string{
						"crdb.cockroachlabs.com/cluster": "crdb-test",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Certificates: crdbv1beta1.Certificates{
						ExternalCertificates: &crdbv1beta1.ExternalCertificates{
							ClientCAConfigMapName:   "the-single-ca",
							NodeCAConfigMapName:     "the-single-ca",
							HTTPSecretName:          "ui-certs",
							NodeClientSecretName:    "node-client-certs",
							NodeSecretName:          "node-certs",
							RootSQLClientSecretName: "client-certs",
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "testpvc",
								Namespace: "testns",
							},
						},
					},
					Image:              "testimage",
					ServiceAccountName: "testsa",
					LocalityLabels:     []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone},
				},
			},
		},
		{
			Name: "wal-failover-enabled",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
					Annotations: map[string]string{
						"crdb.cockroachlabs.com/cloudProvider": "aws",
					},
					Labels: map[string]string{
						"crdb.cockroachlabs.com/cluster": "crdb-test",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Certificates: crdbv1beta1.Certificates{
						ExternalCertificates: &crdbv1beta1.ExternalCertificates{
							ClientCAConfigMapName:   "client-ca",
							HTTPSecretName:          "ui-certs",
							NodeCAConfigMapName:     "node-ca",
							NodeClientSecretName:    "node-client-certs",
							NodeSecretName:          "node-certs",
							RootSQLClientSecretName: "client-certs",
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "testpvc",
								Namespace: "testns",
							},
						},
					},
					Image:              "testimage",
					ServiceAccountName: "testsa",
					WALFailoverSpec: &crdbv1beta1.CrdbWalFailoverSpec{
						Size:   "10gi",
						Status: crdbv1beta1.WalEnable,
					},
					LocalityLabels: []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone},
					Flags:          map[string]string{},
				},
			},
		},
		{
			Name: "wal-failover-enabled-storage-class-set",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
					Annotations: map[string]string{
						"crdb.cockroachlabs.com/cloudProvider": "aws",
					},
					Labels: map[string]string{
						"crdb.cockroachlabs.com/cluster": "crdb-test",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Certificates: crdbv1beta1.Certificates{
						ExternalCertificates: &crdbv1beta1.ExternalCertificates{
							ClientCAConfigMapName:   "client-ca",
							HTTPSecretName:          "ui-certs",
							NodeCAConfigMapName:     "node-ca",
							NodeClientSecretName:    "node-client-certs",
							NodeSecretName:          "node-certs",
							RootSQLClientSecretName: "client-certs",
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "testpvc",
								Namespace: "testns",
							},
						},
					},
					Image:              "testimage",
					ServiceAccountName: "testsa",
					WALFailoverSpec: &crdbv1beta1.CrdbWalFailoverSpec{
						Size:             "10gi",
						Status:           crdbv1beta1.WalEnable,
						StorageClassName: "wal-cc",
					},
					Flags:          map[string]string{},
					LocalityLabels: []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone},
				},
			},
		},
		{
			Name: "wal-failover-disabled",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
					Annotations: map[string]string{
						"crdb.cockroachlabs.com/cloudProvider": "aws",
					},
					Labels: map[string]string{
						"crdb.cockroachlabs.com/cluster": "crdb-test",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Certificates: crdbv1beta1.Certificates{
						ExternalCertificates: &crdbv1beta1.ExternalCertificates{
							ClientCAConfigMapName:   "client-ca",
							HTTPSecretName:          "ui-certs",
							NodeCAConfigMapName:     "node-ca",
							NodeClientSecretName:    "node-client-certs",
							NodeSecretName:          "node-certs",
							RootSQLClientSecretName: "client-certs",
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "testpvc",
								Namespace: "testns",
							},
						},
					},
					Image:              "testimage",
					ServiceAccountName: "testsa",
					WALFailoverSpec: &crdbv1beta1.CrdbWalFailoverSpec{
						Size:   "10gi",
						Status: crdbv1beta1.WalDisable,
					},
					LocalityLabels: []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone},
					Flags:          map[string]string{},
				},
			},
		},
		{
			Name: "wal-failover-enabled-plus-cmek",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
					Annotations: map[string]string{
						"crdb.cockroachlabs.com/cloudProvider": "aws",
					},
					Labels: map[string]string{
						"crdb.cockroachlabs.com/cluster": "crdb-test",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Certificates: crdbv1beta1.Certificates{
						ExternalCertificates: &crdbv1beta1.ExternalCertificates{
							ClientCAConfigMapName:   "client-ca",
							HTTPSecretName:          "ui-certs",
							NodeCAConfigMapName:     "node-ca",
							NodeClientSecretName:    "node-client-certs",
							NodeSecretName:          "node-certs",
							RootSQLClientSecretName: "client-certs",
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "testpvc",
								Namespace: "testns",
							},
						},
					},
					Image:              "testimage",
					ServiceAccountName: "testsa",
					WALFailoverSpec: &crdbv1beta1.CrdbWalFailoverSpec{
						Size:   "10gi",
						Status: crdbv1beta1.WalEnable,
					},
					EncryptionAtRest: earSpec(),
					LocalityLabels:   []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone},
					Flags:            map[string]string{},
				},
			},
		},
		{
			Name: "wal-failover-enabled-plus-custom-path",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
					Annotations: map[string]string{
						"crdb.cockroachlabs.com/cloudProvider": "aws",
					},
					Labels: map[string]string{
						"crdb.cockroachlabs.com/cluster": "crdb-test",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Certificates: crdbv1beta1.Certificates{
						ExternalCertificates: &crdbv1beta1.ExternalCertificates{
							ClientCAConfigMapName:   "client-ca",
							HTTPSecretName:          "ui-certs",
							NodeCAConfigMapName:     "node-ca",
							NodeClientSecretName:    "node-client-certs",
							NodeSecretName:          "node-certs",
							RootSQLClientSecretName: "client-certs",
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "testpvc",
								Namespace: "testns",
							},
						},
					},
					Image:              "testimage",
					ServiceAccountName: "testsa",
					WALFailoverSpec: &crdbv1beta1.CrdbWalFailoverSpec{
						Size:   "10gi",
						Status: crdbv1beta1.WalEnable,
						Path:   "/wal/test/",
					},
					EncryptionAtRest: earSpec(),
					LocalityLabels:   []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone},
					Flags:            map[string]string{},
				},
			},
		},
		{
			Name: "env-vars",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Env: []corev1.EnvVar{
						{Name: "Foo", Value: "Bar"},
					},
					NoCloudPrefixedLocalities: true,
					LocalityLabels:            []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone},
				},
			},
		},
		{
			Name: "override-cache-and-sql-mem-setting",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Env: []corev1.EnvVar{
						{Name: "Foo", Value: "Bar"},
					},
					NoCloudPrefixedLocalities: true,
					Flags: map[string]string{
						"--cache":          "50%",
						"--max-sql-memory": "50%",
					},
					LocalityLabels: []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone},
				},
			},
		},
		{
			Name: "simple-azure",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
					Annotations: map[string]string{
						"crdb.cockroachlabs.com/cloudProvider": "azure",
					},
					Labels: map[string]string{
						"crdb.cockroachlabs.com/cluster": "crdb-test",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Certificates: crdbv1beta1.Certificates{
						ExternalCertificates: &crdbv1beta1.ExternalCertificates{
							ClientCAConfigMapName:   "client-ca",
							HTTPSecretName:          "ui-certs",
							NodeCAConfigMapName:     "node-ca",
							NodeClientSecretName:    "node-client-certs",
							NodeSecretName:          "node-certs",
							RootSQLClientSecretName: "client-certs",
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "testpvc",
								Namespace: "testns",
							},
						},
					},
					Image:                "testimage",
					ServiceAccountName:   "testsa",
					LoggingConfigMapName: "logging-test",
					LocalityLabels:       []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone},
				},
			},
		},
		{
			Name: "simple-with-pod-template",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
					Annotations: map[string]string{
						"crdb.cockroachlabs.com/cloudProvider": "gcp",
					},
					Labels: map[string]string{
						"crdb.cockroachlabs.com/cluster": "crdb-test",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Certificates: crdbv1beta1.Certificates{
						ExternalCertificates: &crdbv1beta1.ExternalCertificates{
							ClientCAConfigMapName:   "client-ca",
							HTTPSecretName:          "ui-certs",
							NodeCAConfigMapName:     "node-ca",
							NodeClientSecretName:    "node-client-certs",
							NodeSecretName:          "node-certs",
							RootSQLClientSecretName: "client-certs",
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "testpvc",
								Namespace: "testns",
							},
						},
					},
					Image:              "testimage",
					ServiceAccountName: "testsa",
					PodTemplate: &crdbv1beta1.PodTemplateSpec{
						Metadata: crdbv1beta1.PodMeta{
							Labels: map[string]string{
								"new-label": "new-label-value",
							},
							Annotations: map[string]string{
								"new-annotation": "new-annotation-value",
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "cockroachdb",
									Image: "new-image",
									Env: []corev1.EnvVar{
										{
											Name:  "NEW_ENV",
											Value: "new-env-value",
										},
									},
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "new-volume",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			Name: "insecure-cluster",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
					Annotations: map[string]string{
						"crdb.cockroachlabs.com/cloudProvider": "gcp",
					},
					Labels: map[string]string{
						"crdb.cockroachlabs.com/cluster": "crdb-test",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: false,
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "testpvc",
								Namespace: "testns",
							},
						},
					},
					Image:                "testimage",
					ServiceAccountName:   "testsa",
					LoggingConfigMapName: "logging-test",
					LocalityLabels:       []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone},
				},
			},
		},
		{
			Name: "logs-store-with-default-path",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
					Annotations: map[string]string{
						"crdb.cockroachlabs.com/cloudProvider": "gcp",
					},
					Labels: map[string]string{
						"crdb.cockroachlabs.com/cluster": "crdb-test",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Certificates: crdbv1beta1.Certificates{
						ExternalCertificates: &crdbv1beta1.ExternalCertificates{
							ClientCAConfigMapName:   "client-ca",
							NodeCAConfigMapName:     "node-ca",
							NodeSecretName:          "node-certs",
							RootSQLClientSecretName: "client-certs",
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "testpvc",
								Namespace: "testns",
							},
						},
					},
					LogsStore: &crdbv1beta1.CrdbLogsStoreSpec{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "logspvc",
								Namespace: "testns",
							},
						},
					},
					Image:              "testimage",
					ServiceAccountName: "testsa",
					LocalityLabels:     []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone},
					Flags:              map[string]string{},
				},
			},
		},
		{
			Name: "logs-store-with-custom-path",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
					Annotations: map[string]string{
						"crdb.cockroachlabs.com/cloudProvider": "gcp",
					},
					Labels: map[string]string{
						"crdb.cockroachlabs.com/cluster": "crdb-test",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Certificates: crdbv1beta1.Certificates{
						ExternalCertificates: &crdbv1beta1.ExternalCertificates{
							ClientCAConfigMapName:   "client-ca",
							NodeCAConfigMapName:     "node-ca",
							NodeSecretName:          "node-certs",
							RootSQLClientSecretName: "client-certs",
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "testpvc",
								Namespace: "testns",
							},
						},
					},
					LogsStore: &crdbv1beta1.CrdbLogsStoreSpec{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "logspvc",
								Namespace: "testns",
							},
						},
						MountPath: "/cockroach/custom-logs",
					},
					Image:              "testimage",
					ServiceAccountName: "testsa",
					LocalityLabels:     []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone},
					Flags:              map[string]string{},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			mani := CrdbNodePod(&tc.Node, &corev1.Node{})
			kubetest.RequireValidPodSpec(t, mani)
			actual, err := json.MarshalIndent(mani, "", "\t")
			assert.NoError(t, err)
			testutils.AssertGoldenJSON(t, tc.Name, actual)
		})
	}
}

func TestCrdbNodeNodeName(t *testing.T) {
	testCases := []struct {
		NodeName             string
		ExpectedNodeSelector map[string]string
	}{
		{NodeName: "", ExpectedNodeSelector: map[string]string{}},
		{NodeName: "foo-bar-baz", ExpectedNodeSelector: map[string]string{
			corev1.LabelHostname: "foo-bar-baz",
		}},
	}

	for _, tc := range testCases {
		node := &crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"crdb.cockroachlabs.com/cloudProvider": "gcp",
				},
			},
			Spec: crdbv1beta1.CrdbNodeSpec{
				NodeName: tc.NodeName,
			},
		}
		require.Equal(t, tc.ExpectedNodeSelector, CrdbNodePod(node, &corev1.Node{}).Spec.NodeSelector)
		require.Equal(t, "", CrdbNodePod(node, &corev1.Node{}).Spec.NodeName)
	}
}

func TestCrdbNodeConditionalResources(t *testing.T) {
	type testCase struct {
		name                    string
		capacity                corev1.ResourceList
		requirements            corev1.ResourceRequirements
		conditionalRequirements crdbv1beta1.ConditionalResourceRequirements
		result                  corev1.ResourceRequirements
	}

	tests := []testCase{
		{
			name: "requirements are the default",
			requirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					"cpu": resource.MustParse("1"),
					"ram": resource.MustParse("2G"),
				},
				Limits: corev1.ResourceList{
					"cpu": resource.MustParse("2"),
					"ram": resource.MustParse("4G"),
				},
			},
			result: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					"cpu":    resource.MustParse("1"),
					"ram":    resource.MustParse("2G"),
					"memory": resource.MustParse("0Gi"),
				},
				Limits: corev1.ResourceList{
					"cpu":    resource.MustParse("2"),
					"ram":    resource.MustParse("4G"),
					"memory": resource.MustParse("8"),
				},
			},
			capacity: corev1.ResourceList{
				"memory": resource.MustParse("8"),
			},
		},
		{
			name: "conditional requirements are ignored if there is no capacity",
			requirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					"cpu": resource.MustParse("1"),
					"ram": resource.MustParse("2G"),
				},
			},
			conditionalRequirements: crdbv1beta1.ConditionalResourceRequirements{
				Requests: map[corev1.ResourceName][]crdbv1beta1.ConditionalQuantity{
					"cpu": {
						{
							RequiredCapacity: resource.MustParse("1"),
							Quantity:         resource.MustParse("0.5"),
						},
					},
				},
			},
			result: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					"cpu":    resource.MustParse("1"),
					"ram":    resource.MustParse("2G"),
					"memory": resource.MustParse("0Gi"),
				},
				Limits: corev1.ResourceList{
					"memory": resource.MustParse("8"),
				},
			},
			capacity: corev1.ResourceList{
				"memory": resource.MustParse("8"),
			},
		},
		{
			name: "conditional overwrite requirements",
			requirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					"cpu": resource.MustParse("1"),
					"ram": resource.MustParse("2G"),
				},
				Limits: corev1.ResourceList{
					"cpu": resource.MustParse("2"),
					"ram": resource.MustParse("4G"),
				},
			},
			conditionalRequirements: crdbv1beta1.ConditionalResourceRequirements{
				Requests: map[corev1.ResourceName][]crdbv1beta1.ConditionalQuantity{
					"cpu": {
						{
							RequiredCapacity: resource.MustParse("1"),
							Quantity:         resource.MustParse("0.5"),
						},
						{
							RequiredCapacity: resource.MustParse("4"),
							Quantity:         resource.MustParse("3"),
						},
					},
				},
				Limits: map[corev1.ResourceName][]crdbv1beta1.ConditionalQuantity{
					"cpu": {
						{
							RequiredCapacity: resource.MustParse("1"),
							Quantity:         resource.MustParse("1"),
						},
						{
							RequiredCapacity: resource.MustParse("4"),
							Quantity:         resource.MustParse("4"),
						},
					},
				},
			},
			capacity: corev1.ResourceList{
				"cpu":    resource.MustParse("8"),
				"memory": resource.MustParse("8"),
			},
			result: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					"cpu":    resource.MustParse("3"),
					"ram":    resource.MustParse("2G"),
					"memory": resource.MustParse("0Gi"),
				},
				Limits: corev1.ResourceList{
					"cpu":    resource.MustParse("4"),
					"ram":    resource.MustParse("4G"),
					"memory": resource.MustParse("8"),
				},
			},
		},
		{
			name: "conditional requirements do not need default requirements",
			conditionalRequirements: crdbv1beta1.ConditionalResourceRequirements{
				Requests: map[corev1.ResourceName][]crdbv1beta1.ConditionalQuantity{
					"cpu": {
						{
							RequiredCapacity: resource.MustParse("4"),
							Quantity:         resource.MustParse("3"),
						},
					},
				},
				Limits: map[corev1.ResourceName][]crdbv1beta1.ConditionalQuantity{
					"cpu": {
						{
							RequiredCapacity: resource.MustParse("4"),
							Quantity:         resource.MustParse("4"),
						},
					},
				},
			},
			capacity: corev1.ResourceList{
				"cpu":    resource.MustParse("8"),
				"memory": resource.MustParse("8"),
			},
			result: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					"cpu":    resource.MustParse("3"),
					"memory": resource.MustParse("0Gi"),
				},
				Limits: corev1.ResourceList{
					"cpu":    resource.MustParse("4"),
					"memory": resource.MustParse("8"),
				},
			},
		},
		{
			name:                    "memory defaults to allocatable",
			conditionalRequirements: crdbv1beta1.ConditionalResourceRequirements{},
			capacity: corev1.ResourceList{
				"memory": resource.MustParse("8"),
			},
			result: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					"memory": resource.MustParse("0Gi"),
				},
				Limits: corev1.ResourceList{
					"memory": resource.MustParse("8"),
				},
			},
		},
	}
	for _, test := range tests {
		pod := CrdbNodePod(
			&crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
					Annotations: map[string]string{
						"crdb.cockroachlabs.com/cloudProvider": "gcp",
					},
					Labels: map[string]string{
						"crdb.cockroachlabs.com/cluster": "crdb-test",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					ResourceRequirements:            test.requirements,
					ConditionalResourceRequirements: &test.conditionalRequirements,
				},
			},
			&corev1.Node{
				Status: corev1.NodeStatus{
					Allocatable: test.capacity,
				},
			},
		)
		require.Equal(t, test.result, pod.Spec.Containers[0].Resources)
	}
}

func TestGetConditionalQuantity(t *testing.T) {
	quantity, found := getConditionalQuantity(
		resource.MustParse("10"),
		[]crdbv1beta1.ConditionalQuantity{
			{Quantity: resource.MustParse("4"), RequiredCapacity: resource.MustParse("6")},
			{Quantity: resource.MustParse("6"), RequiredCapacity: resource.MustParse("8")},
			{Quantity: resource.MustParse("8"), RequiredCapacity: resource.MustParse("10")},
			// The smaller valid quantity of '4' is ignored
			{Quantity: resource.MustParse("4"), RequiredCapacity: resource.MustParse("10")},
			{Quantity: resource.MustParse("10"), RequiredCapacity: resource.MustParse("12")},
		})
	require.True(t, found)
	require.Equal(t, quantity, resource.MustParse("8"))

	_, found = getConditionalQuantity(
		resource.MustParse("5"),
		[]crdbv1beta1.ConditionalQuantity{
			{Quantity: resource.MustParse("4"), RequiredCapacity: resource.MustParse("6")},
			{Quantity: resource.MustParse("6"), RequiredCapacity: resource.MustParse("8")},
			{Quantity: resource.MustParse("8"), RequiredCapacity: resource.MustParse("10")},
			{Quantity: resource.MustParse("10"), RequiredCapacity: resource.MustParse("12")},
		})
	require.False(t, found)
}

func TestCrdbNodeStoragePVCName(t *testing.T) {
	testCases := []struct {
		UnprefixedName string
		PVC            *corev1.PersistentVolumeClaim
		ExpectedName   string
	}{
		{
			UnprefixedName: "scoobydoo",
			PVC:            &corev1.PersistentVolumeClaim{},
			ExpectedName:   "scoobydoo",
		},
		{
			UnprefixedName: "shaggytoo",
			PVC: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "scoobydoo",
				},
			},
			ExpectedName: "scoobydoo-shaggytoo",
		},
	}

	for _, tc := range testCases {
		assert.Equal(t, tc.ExpectedName, CrdbNodeStoragePVCName(tc.UnprefixedName, tc.PVC))
	}
}

func TestNodeFQDN(t *testing.T) {
	node := &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "majortom",
			Namespace: "tincan",
			Labels: map[string]string{
				BetaCrdbClusterNodeOwnerLabelKey: "groundcontrol",
			},
		},
		Spec: crdbv1beta1.CrdbNodeSpec{},
	}
	assert.Equal(t, "majortom.groundcontrol.tincan.svc.cluster.local", nodeFQDN(node))

	node.Spec.Domain = "solarsystem"
	assert.Equal(t, "majortom.groundcontrol.tincan.svc.solarsystem", nodeFQDN(node))
}

func TestResolveLogsMountPath(t *testing.T) {
	// Empty string returns default
	assert.Equal(t, DefaultLogsMountPath, resolveLogsMountPath(""))

	// Custom path returns that path
	assert.Equal(t, "/cockroach/custom-logs", resolveLogsMountPath("/cockroach/custom-logs"))
}

func TestCrdbNodeLogsStoragePVCName(t *testing.T) {
	testCases := []struct {
		UnprefixedName string
		PVC            *corev1.PersistentVolumeClaim
		ExpectedName   string
	}{
		{
			UnprefixedName: "node-0",
			PVC:            &corev1.PersistentVolumeClaim{},
			ExpectedName:   "logsdir-node-0",
		},
		{
			UnprefixedName: "node-0",
			PVC: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "custom-logs",
				},
			},
			ExpectedName: "custom-logs-node-0",
		},
	}

	for _, tc := range testCases {
		assert.Equal(t, tc.ExpectedName, CrdbNodeLogsStoragePVCName(tc.UnprefixedName, tc.PVC))
	}
}

func TestHostConditionalResourceRequirements(t *testing.T) {
	type testCase struct {
		capacity resource.Quantity
		expect   resource.Quantity
	}

	// 16 and 32 are the size of hosts we currently deploy in production
	testCases := []testCase{
		{capacity: resource.MustParse("16"), expect: resource.MustParse("10")},
		{capacity: resource.MustParse("32"), expect: resource.MustParse("19")},
	}

	for _, tc := range testCases {
		crdbNode := &crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test",
				Annotations: map[string]string{
					"crdb.cockroachlabs.com/cloudProvider": "gcp",
				},
			},
			Spec: crdbv1beta1.CrdbNodeSpec{
				ConditionalResourceRequirements: HostConditionalResourceRequirements(),
			},
		}
		k8sNode := &corev1.Node{
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"cpu": tc.capacity,
				},
			},
		}
		pod := CrdbNodePod(crdbNode, k8sNode)
		require.Equal(t, tc.expect, pod.Spec.Containers[0].Resources.Requests["cpu"])
		require.Equal(t, tc.expect, pod.Spec.Containers[0].Resources.Limits["cpu"])
	}
}

func TestCrdbNodeStartFlags(t *testing.T) {
	testCases := []struct {
		Name string
		Node crdbv1beta1.CrdbNode
	}{
		// Basic scenarios - no flags/startFlags
		{
			Name: "no-flags-no-startflags",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "majortom",
					Namespace: "flags",
					Labels: map[string]string{
						BetaCrdbClusterNodeOwnerLabelKey: "groundcontrol",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					StartFlags: &crdbv1beta1.Flags{},
					Join:       "test-join",
				},
			},
		},
		{
			Name: "logging-configmap-no-flags",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "majortom",
					Namespace: "flags",
					Labels: map[string]string{
						BetaCrdbClusterNodeOwnerLabelKey: "groundcontrol",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled:           true,
					Join:                 "test-join",
					LoggingConfigMapName: "test-logging",
					StartFlags:           &crdbv1beta1.Flags{},
				},
			},
		},

		// Special characters and complex flag values
		{
			Name: "special-characters-in-flags",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "majortom",
					Namespace: "flags",
					Labels: map[string]string{
						BetaCrdbClusterNodeOwnerLabelKey: "groundcontrol",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					StartFlags: &crdbv1beta1.Flags{
						Upsert: []string{
							"--log='sinks: {stderr: {channels: {INFO: \"HEALTH,OPS\", WARNING: \"STORAGE,DEV\"}}}'",
							"--locality=region=us-east-1,zone=us-east-1a",
							"--cluster-name=my-prod-cluster:staging",
							"--join=host1.example.com:26257,host2.example.com:26257",
						},
					},
					Join: "test-join",
				},
			},
		},

		// Upsert functionality - override defaults and add custom flags
		{
			Name: "upsert-override-defaults",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-node",
					Namespace: "test-ns",
					Labels: map[string]string{
						BetaCrdbClusterNodeOwnerLabelKey: "test-cluster",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Image:      "cockroachdb/cockroach:v23.1.0",
					GRPCPort:   proto.Int32(26258), // First-class field should be overridden
					StartFlags: &crdbv1beta1.Flags{
						Upsert: []string{
							"--listen-addr=0.0.0.0:9999", // Should override first-class GRPCPort
							"--cache=50%",                // Should override default 25%
							"--max-sql-memory=75%",       // Should override default 25%
							"--custom-flag=custom-value", // Should be added
						},
					},
					Join: "test-join",
				},
			},
		},

		// Omit functionality - remove default flags
		{
			Name: "omit-default-flags",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-node",
					Namespace: "test-ns",
					Labels: map[string]string{
						BetaCrdbClusterNodeOwnerLabelKey: "test-cluster",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Image:      "cockroachdb/cockroach:v23.1.0",
					StartFlags: &crdbv1beta1.Flags{
						Omit: []string{
							"--cache",          // Should remove default cache setting
							"--max-sql-memory", // Should remove the default max-sql-memory setting
						},
						Upsert: []string{
							"--custom-performance=high", // Add custom flag
						},
					},
					Join: "test-join",
				},
			},
		},

		// Complex scenarios with WAL failover and EAR
		{
			Name: "complex-upsert-omit-with-wal-ear",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "majortom",
					Namespace: "flags",
					Labels: map[string]string{
						BetaCrdbClusterNodeOwnerLabelKey: "groundcontrol",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					StartFlags: &crdbv1beta1.Flags{
						Upsert: []string{
							"--max-offset=100ms",
							"--cache=50%",
							"--listen-addr=0.0.0.0:8888", // Override first-class ports
							"--http-addr=0.0.0.0:9090",
						},
						Omit: []string{
							"--max-sql-memory",
							"--certs-dir",
						},
					},
					HTTPPort:             proto.Int32(8080), // Should be overridden by startFlags
					LoggingConfigMapName: "test-logging",
					Join:                 "test-join",
					WALFailoverSpec: &crdbv1beta1.CrdbWalFailoverSpec{
						Size:             "10Gi",
						StorageClassName: "test-storage-class",
						Status:           crdbv1beta1.WalEnable,
					},
					EncryptionAtRest: earSpec(),
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name: "data-volume",
							},
						},
					},
				},
			},
		},

		// Basalt-backed storage (insecure)
		{
			Name: "basalt-storage-insecure",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "majortom",
					Namespace: "flags",
					Labels: map[string]string{
						BetaCrdbClusterNodeOwnerLabelKey: "groundcontrol",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Join:       "test-join",
					Image:      "cockroachdb/cockroach:v26.3.0-alpha.1",
					StartFlags: &crdbv1beta1.Flags{},
					DataStore: crdbv1beta1.DataStore{
						Basalt: &crdbv1beta1.BasaltStoreSpec{
							ControllerAddrs: []string{"10.0.0.2:26256", "10.0.0.3:26256", "10.0.0.4:26256"},
							TenantID:        "5e0c10f6-5bc7-5be1-bbb7-78cec71d8570",
							Insecure:        new(true),
						},
					},
				},
			},
		},

		// Basalt-backed storage with mTLS. Every --store must carry the same
		// certs-dir query or cockroach refuses to build the basalt connection.
		{
			Name: "basalt-storage-secure",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "majortom",
					Namespace: "flags",
					Labels: map[string]string{
						BetaCrdbClusterNodeOwnerLabelKey: "groundcontrol",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Join:       "test-join",
					Image:      "cockroachdb/cockroach:v26.3.0-alpha.1",
					StartFlags: &crdbv1beta1.Flags{},
					DataStore: crdbv1beta1.DataStore{
						Basalt: &crdbv1beta1.BasaltStoreSpec{
							ControllerAddrs: []string{"10.0.0.2:26256", "10.0.0.3:26256", "10.0.0.4:26256"},
							TenantID:        "5e0c10f6-5bc7-5be1-bbb7-78cec71d8570",
							StoreCount:      proto.Int32(2),
							TLS: &crdbv1beta1.BasaltTLSSpec{
								ClientSecretName: "basalt-tenant-client",
								CAConfigMapName:  "basalt-server-ca",
							},
						},
					},
				},
			},
		},

		// Basalt-backed storage on the deprecated flags path (no startFlags set).
		// The basalt flags must still render so the node uses basalt, not disk.
		{
			Name: "basalt-storage-no-startflags",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "majortom",
					Namespace: "flags",
					Labels: map[string]string{
						BetaCrdbClusterNodeOwnerLabelKey: "groundcontrol",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Join:  "test-join",
					Image: "cockroachdb/cockroach:v26.3.0-alpha.1",
					DataStore: crdbv1beta1.DataStore{
						Basalt: &crdbv1beta1.BasaltStoreSpec{
							ControllerAddrs: []string{"10.0.0.2:26256", "10.0.0.3:26256", "10.0.0.4:26256"},
							TenantID:        "5e0c10f6-5bc7-5be1-bbb7-78cec71d8570",
							Insecure:        new(true),
						},
					},
				},
			},
		},

		// Basalt-backed storage with a startFlags upsert overriding a basalt flag
		{
			Name: "basalt-storage-upsert-override",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "majortom",
					Namespace: "flags",
					Labels: map[string]string{
						BetaCrdbClusterNodeOwnerLabelKey: "groundcontrol",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Join:  "test-join",
					Image: "cockroachdb/cockroach:v26.3.0-alpha.1",
					StartFlags: &crdbv1beta1.Flags{
						Upsert: []string{"--cache=40%"},
					},
					DataStore: crdbv1beta1.DataStore{
						Basalt: &crdbv1beta1.BasaltStoreSpec{
							ControllerAddrs: []string{"10.0.0.2:26256"},
							TenantID:        "5e0c10f6-5bc7-5be1-bbb7-78cec71d8570",
							Insecure:        new(true),
						},
					},
				},
			},
		},

		// WAL failover disabled scenarios
		{
			Name: "wal-failover-disabled-with-upserts",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "majortom",
					Namespace: "flags",
					Labels: map[string]string{
						BetaCrdbClusterNodeOwnerLabelKey: "groundcontrol",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					StartFlags: &crdbv1beta1.Flags{
						Upsert: []string{"--max-offset=100ms", "--store=path=/mnt/xyz"},
					},
					Join: "test-join",
					WALFailoverSpec: &crdbv1beta1.CrdbWalFailoverSpec{
						Size:             "10Gi",
						StorageClassName: "test-storage-class",
						Status:           crdbv1beta1.WalDisable,
					},
				},
			},
		},

		// Multiple attributes handling
		{
			Name: "multiple-attrs-flags",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "majortom",
					Namespace: "flags",
					Labels: map[string]string{
						BetaCrdbClusterNodeOwnerLabelKey: "groundcontrol",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					StartFlags: &crdbv1beta1.Flags{
						Upsert: []string{
							"--attrs=ssd",
							"--attrs=highio",
						},
					},
					Join: "test-join",
				},
			},
		},

		// Environment variable references
		{
			Name: "env-var-reference-in-upsert",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "majortom",
					Namespace: "flags",
					Labels: map[string]string{
						BetaCrdbClusterNodeOwnerLabelKey: "groundcontrol",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					StartFlags: &crdbv1beta1.Flags{
						Upsert: []string{
							"--max-offset=${MAX_OFFSET}",
							"--store=path=/mnt/xyz",
						},
					},
					Join: "test-join",
					Env: []corev1.EnvVar{
						{
							Name:  "MAX_OFFSET",
							Value: "100ms",
						},
					},
				},
			},
		},

		// DataStore with volume template and omit flags
		{
			Name: "datastore-volume-template-with-omit",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "majortom",
					Namespace: "flags",
					Labels: map[string]string{
						BetaCrdbClusterNodeOwnerLabelKey: "groundcontrol",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					StartFlags: &crdbv1beta1.Flags{
						Omit: []string{"--max-sql-memory"},
					},
					Join: "test-join",
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name: "data-volume",
							},
						},
					},
				},
			},
		},

		// EAR enabled with WAL disabled
		{
			Name: "ear-enabled-wal-disabled",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "majortom",
					Namespace: "flags",
					Labels: map[string]string{
						BetaCrdbClusterNodeOwnerLabelKey: "groundcontrol",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Join:       "test-join",
					StartFlags: &crdbv1beta1.Flags{},
					WALFailoverSpec: &crdbv1beta1.CrdbWalFailoverSpec{
						Size:             "10Gi",
						StorageClassName: "test-storage-class",
						Status:           crdbv1beta1.WalDisable,
					},
					EncryptionAtRest: earSpec(),
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name: "data-volume",
							},
						},
					},
				},
			},
		},

		// Both EAR and WAL enabled
		{
			Name: "ear-enabled-wal-enabled",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "majortom",
					Namespace: "flags",
					Labels: map[string]string{
						BetaCrdbClusterNodeOwnerLabelKey: "groundcontrol",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Join:       "test-join",
					StartFlags: &crdbv1beta1.Flags{},
					WALFailoverSpec: &crdbv1beta1.CrdbWalFailoverSpec{
						Size:             "10Gi",
						StorageClassName: "test-storage-class",
						Status:           crdbv1beta1.WalEnable,
					},
					EncryptionAtRest: earSpec(),
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name: "data-volume",
							},
						},
					},
				},
			},
		},

		// WAL enabled with custom path set
		{
			Name: "wal-enabled-with-custom-path",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "majortom",
					Namespace: "flags",
					Labels: map[string]string{
						BetaCrdbClusterNodeOwnerLabelKey: "groundcontrol",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Certificates: crdbv1beta1.Certificates{
						ExternalCertificates: &crdbv1beta1.ExternalCertificates{
							ClientCAConfigMapName:   "client-ca",
							HTTPSecretName:          "ui-certs",
							NodeCAConfigMapName:     "node-ca",
							NodeClientSecretName:    "node-client-certs",
							NodeSecretName:          "node-certs",
							RootSQLClientSecretName: "client-certs",
						},
					},
					Join: "test-join",
					WALFailoverSpec: &crdbv1beta1.CrdbWalFailoverSpec{
						Size:             "10Gi",
						StorageClassName: "test-storage-class",
						Status:           crdbv1beta1.WalEnable,
						Path:             "/mnt/wal",
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name: "data-volume",
							},
						},
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			startCmd := crdbNodeCmd(&tc.Node)
			testutils.AssertGoldenString(t, tc.Name, []byte(startCmd))
		})
	}
}

// TestPodTemplateAndFirstClassFieldsComprehensive tests all major podTemplate merging scenarios,
// startFlags integration, and first-class field conflict resolution with proper precedence:
// podTemplate > startFlags > first-class fields > operator defaults.
func TestPodTemplateAndFirstClassFieldsComprehensive(t *testing.T) {
	testCases := []struct {
		Name string
		Node crdbv1beta1.CrdbNode
	}{
		// Basic podTemplate scenarios
		{
			Name: "podtemplate-metadata-merge",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "test-ns",
					Labels: map[string]string{
						"cluster": "test-cluster",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Image:      "cockroachdb/cockroach:v23.1.0",
					// First-class fields - should be merged with precedence to podTemplate
					PodLabels: map[string]string{
						"existing-label": "existing-value",
						"shared-label":   "from-deprecated", // Should be overridden by template
					},
					PodAnnotations: map[string]string{
						"existing-annotation": "existing-value",
						"shared-annotation":   "from-deprecated", // Should be overridden by template
					},
					PodTemplate: &crdbv1beta1.PodTemplateSpec{
						Metadata: crdbv1beta1.PodMeta{
							Labels: map[string]string{
								"template-label": "template-value",
								"override-label": "overridden",
								"shared-label":   "from-template", // Should override first-class field
							},
							Annotations: map[string]string{
								"template-annotation": "template-value",
								"shared-annotation":   "from-template", // Should override first-class field
							},
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{Name: "test-pvc"},
						},
					},
				},
			},
		},

		{
			Name: "podtemplate-container-merging-comprehensive",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "test-ns",
					Labels: map[string]string{
						"cluster": "test-cluster",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Image:      "cockroachdb/cockroach:v23.1.0",
					// First-class fields - should be merged/overridden by podTemplate
					Env: []corev1.EnvVar{
						{Name: "EXISTING_ENV", Value: "existing-value"},
						{Name: "SHARED_ENV", Value: "from-deprecated"},
					},
					ResourceRequirements: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("100m"), // Should be merged
						},
					},
					PodTemplate: &crdbv1beta1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "cockroachdb",
									Image: "cockroachdb/cockroach:v24.1.0", // Should override first-class field
									Env: []corev1.EnvVar{
										{Name: "TEMPLATE_ENV", Value: "template-value"},
										{Name: "SHARED_ENV", Value: "from-template"}, // Should override first-class field
									},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceMemory: resource.MustParse("1Gi"), // Should merge with a first-class field CPU resource
										},
									},
									VolumeMounts: []corev1.VolumeMount{
										{Name: "template-mount", MountPath: "/template"},
									},
									Ports: []corev1.ContainerPort{
										{Name: "custom-port", ContainerPort: 9999},
									},
								},
								{
									Name:  "custom-sidecar",
									Image: "sidecar:latest",
									Env: []corev1.EnvVar{
										{Name: "SIDECAR_ENV", Value: "sidecar-value"},
									},
								},
							},
							InitContainers: []corev1.Container{
								{
									Name:  "custom-init",
									Image: "init:latest",
								},
							},
							EphemeralContainers: []corev1.EphemeralContainer{
								{
									EphemeralContainerCommon: corev1.EphemeralContainerCommon{
										Name:    "debug-container",
										Image:   "busybox:latest",
										Command: []string{"sleep", "infinity"},
									},
								},
							},
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{Name: "test-pvc"},
						},
					},
				},
			},
		},

		{
			Name: "first-class-sidecars-vs-podtemplate",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sidecar-conflict-test",
					Namespace: "test-ns",
					Labels: map[string]string{
						"cluster": "test-cluster",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Image:      "cockroachdb/cockroach:v23.1.0",
					// First-class SideCars field
					SideCars: crdbv1beta1.CrdbNodeSideCars{
						InitContainers: []corev1.Container{
							{
								Name:  "deprecated-init",
								Image: "deprecated-init:v1.0.0",
								Env: []corev1.EnvVar{
									{Name: "DEPRECATED_INIT_ENV", Value: "deprecated-value"},
								},
							},
						},
						Containers: []corev1.Container{
							{
								Name:  "deprecated-sidecar",
								Image: "deprecated-sidecar:v1.0.0",
								Env: []corev1.EnvVar{
									{Name: "DEPRECATED_SIDECAR_ENV", Value: "deprecated-value"},
								},
								Ports: []corev1.ContainerPort{
									{Name: "deprecated-port", ContainerPort: 8888},
								},
							},
						},
						Volumes: []corev1.Volume{
							{
								Name: "deprecated-volume",
								VolumeSource: corev1.VolumeSource{
									EmptyDir: &corev1.EmptyDirVolumeSource{},
								},
							},
						},
					},
					// PodTemplate should override/merge with SideCars
					PodTemplate: &crdbv1beta1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							InitContainers: []corev1.Container{
								{
									Name:  "template-init",
									Image: "template-init:v2.0.0",
									Env: []corev1.EnvVar{
										{Name: "TEMPLATE_INIT_ENV", Value: "template-value"},
									},
								},
								{
									Name:  "deprecated-init", // Same name - should override
									Image: "overridden-init:v3.0.0",
									Env: []corev1.EnvVar{
										{Name: "OVERRIDDEN_INIT_ENV", Value: "overridden-value"},
									},
								},
							},
							Containers: []corev1.Container{
								{
									Name:  "template-sidecar",
									Image: "template-sidecar:v2.0.0",
									Env: []corev1.EnvVar{
										{Name: "TEMPLATE_SIDECAR_ENV", Value: "template-value"},
									},
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "template-volume",
									VolumeSource: corev1.VolumeSource{
										ConfigMap: &corev1.ConfigMapVolumeSource{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: "template-config",
											},
										},
									},
								},
								{
									Name: "deprecated-volume", // Same name - should override
									VolumeSource: corev1.VolumeSource{
										Secret: &corev1.SecretVolumeSource{
											SecretName: "overridden-secret",
										},
									},
								},
							},
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{Name: "test-pvc"},
						},
					},
				},
			},
		},

		{
			Name: "podtemplate-scheduling-and-first-class-fields",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "scheduling-test",
					Namespace: "test-ns",
					Labels: map[string]string{
						"cluster": "test-cluster",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Image:      "cockroachdb/cockroach:v23.1.0",
					// First-class scheduling fields
					NodeSelector: map[string]string{
						"existing-selector": "existing-value",
						"shared-selector":   "from-deprecated",
					},
					Tolerations: []corev1.Toleration{
						{Key: "existing-taint", Operator: corev1.TolerationOpExists},
					},
					TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
						{
							MaxSkew:           1,
							TopologyKey:       "topology.kubernetes.io/zone",
							WhenUnsatisfiable: corev1.DoNotSchedule,
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"deprecated": "constraint",
								},
							},
						},
					},
					ServiceAccountName:     "deprecated-sa",
					TerminationGracePeriod: &metav1.Duration{Duration: time.Minute * 5}, // Should be overridden
					PodTemplate: &crdbv1beta1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Volumes: []corev1.Volume{
								{
									Name: "template-volume",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
							},
							NodeSelector: map[string]string{
								"template-selector": "template-value",
								"shared-selector":   "from-template", // Should override first-class field
							},
							Tolerations: []corev1.Toleration{
								{Key: "template-taint", Operator: corev1.TolerationOpExists},
							},
							TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
								{
									MaxSkew:           2,
									TopologyKey:       "topology.kubernetes.io/zone", // Same key - should override
									WhenUnsatisfiable: corev1.ScheduleAnyway,         // Different behavior
									LabelSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"template": "constraint",
										},
									},
								},
								{
									MaxSkew:           3,
									TopologyKey:       "topology.kubernetes.io/region", // New key
									WhenUnsatisfiable: corev1.DoNotSchedule,
									LabelSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"template": "region-constraint",
										},
									},
								},
							},
							ServiceAccountName:            "template-sa",    // override first-class field
							TerminationGracePeriodSeconds: proto.Int64(600), // override first-class field
							PriorityClassName:             "high-priority",
							SecurityContext: &corev1.PodSecurityContext{
								RunAsUser:  proto.Int64(2000),
								RunAsGroup: proto.Int64(2000),
							},
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{Name: "test-pvc"},
						},
					},
				},
			},
		},

		// Tests that podTemplate affinity completely overrides first-class affinity (all affinity types)
		{
			Name: "podtemplate-affinity-complete-override",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "affinity-test",
					Namespace: "test-ns",
					Labels: map[string]string{
						"cluster": "test-cluster",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Image:      "cockroachdb/cockroach:v23.1.0",
					// First-class Affinity with all affinity types (node, pod affinity, pod anti-affinity)
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{Key: "first-class-node", Operator: corev1.NodeSelectorOpExists},
										},
									},
								},
							},
						},
						PodAffinity: &corev1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
								{
									TopologyKey: "topology.kubernetes.io/zone",
									LabelSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"first-class": "pod-affinity",
										},
									},
								},
							},
						},
						PodAntiAffinity: &corev1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
								{
									TopologyKey: "kubernetes.io/hostname",
									LabelSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"first-class": "pod-anti-affinity",
										},
									},
								},
							},
						},
					},
					PodTemplate: &crdbv1beta1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{Key: "template-node", Operator: corev1.NodeSelectorOpExists},
												},
											},
										},
									},
								},
								PodAffinity: &corev1.PodAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
										{
											TopologyKey: "topology.kubernetes.io/region",
											LabelSelector: &metav1.LabelSelector{
												MatchLabels: map[string]string{
													"template": "pod-affinity",
												},
											},
										},
									},
								},
								PodAntiAffinity: &corev1.PodAntiAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
										{
											TopologyKey: "topology.kubernetes.io/zone",
											LabelSelector: &metav1.LabelSelector{
												MatchLabels: map[string]string{
													"template": "pod-anti-affinity",
												},
											},
										},
									},
								},
							},
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{Name: "test-pvc"},
						},
					},
				},
			},
		},

		// Complete precedence validation: podTemplate > startFlags > first-class fields > operator defaults
		{
			Name: "complete-precedence-validation",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "precedence-test",
					Namespace: "test-ns",
					Labels: map[string]string{
						"cluster": "test-cluster",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					// First-class fields (should be overridden by startFlags and podTemplate)
					Image:              "first-class-image:v1.0.0",
					ServiceAccountName: "first-class-sa",
					GRPCPort:           proto.Int32(26258),
					HTTPPort:           proto.Int32(8080),
					SQLPort:            proto.Int32(26257),
					// First-class fields (should be overridden by startFlags and podTemplate)
					Env: []corev1.EnvVar{
						{Name: "DEPRECATED_ENV", Value: "deprecated-value"},
						{Name: "SHARED_ENV", Value: "from-deprecated"},
					},
					ResourceRequirements: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("100m"),
						},
					},
					NodeSelector: map[string]string{
						"deprecated-selector": "deprecated-value",
						"shared-selector":     "from-deprecated",
					},

					// StartFlags (should override default flags but be overridden by podTemplate)
					StartFlags: &crdbv1beta1.Flags{
						Upsert: []string{
							"--listen-addr=0.0.0.0:9999", // Should override first-class GRPCPort
							"--http-addr=0.0.0.0:9090",   // Should override first-class HTTPPort
							"--cache=startflags-cache-value",
							"--startflags-only=startflags-value",
						},
						Omit: []string{
							"--max-sql-memory",
						},
					},

					// PodTemplate (should override everything)
					PodTemplate: &crdbv1beta1.PodTemplateSpec{
						Metadata: crdbv1beta1.PodMeta{
							Labels: map[string]string{
								"podtemplate-label": "podtemplate-value",
								"shared-selector":   "from-podtemplate", // Should override first-class field
							},
							Annotations: map[string]string{
								"podtemplate-annotation": "podtemplate-value",
							},
						},
						Spec: corev1.PodSpec{
							ServiceAccountName: "podtemplate-sa", // Should override first-class
							NodeSelector: map[string]string{
								"podtemplate-selector": "podtemplate-value",
								"shared-selector":      "from-podtemplate", // Should override first-class field
							},
							Containers: []corev1.Container{
								{
									Name:  "cockroachdb",
									Image: "podtemplate-image:v2.0.0", // Should override first-class
									Env: []corev1.EnvVar{
										{Name: "PODTEMPLATE_ENV", Value: "podtemplate-value"},
										{Name: "SHARED_ENV", Value: "from-podtemplate"}, // Should override first-class field Env
									},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceMemory: resource.MustParse("2Gi"), // Should merge with first-class field CPU resources
										},
									},
								},
								{
									Name:  "podtemplate-sidecar",
									Image: "sidecar:latest",
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "podtemplate-volume",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
							},
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{Name: "test-pvc"},
						},
					},
				},
			},
		},

		// PodTemplate command overrides startFlags completely
		{
			Name: "podtemplate-command-overrides-startflags",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "command-override-test",
					Namespace: "test-ns",
					Labels: map[string]string{
						"cluster": "test-cluster",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Image:      "base-image:v1.0.0",

					// StartFlags should be ignored when podTemplate specifies the command
					StartFlags: &crdbv1beta1.Flags{
						Upsert: []string{
							"--cache=10%",
							"--custom-flag=test",
						},
					},

					// PodTemplate with command should override startFlags
					PodTemplate: &crdbv1beta1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:    "cockroachdb",
									Image:   "custom-image:v2.0.0",
									Command: []string{"/custom/entrypoint"},
									Args:    []string{"--custom-arg=value"},
									Env: []corev1.EnvVar{
										{Name: "CUSTOM_ENV", Value: "custom-value"},
									},
								},
							},
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{Name: "test-pvc"},
						},
					},
				},
			},
		},
		// Set log_config_vars if loggingConfigMapName and HOST_IP env var is configured via podTemplate or first-class env field.
		{
			Name: "log-config-vars-with-host-ip",
			Node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "command-override-test",
					Namespace: "test-ns",
					Labels: map[string]string{
						"cluster": "test-cluster",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image:                "base-image:v1.0.0",
					LoggingConfigMapName: "test-logging-config",
					LoggingConfigVars:    []string{"HOST_IP", "LOG_LEVEL"},
					StartFlags: &crdbv1beta1.Flags{
						Upsert: []string{
							"--log-config-vars=MAX_SYNC_DURATION", // verifies StartFlags --log-config-vars overrides LoggingConfigVars field
						},
					},
					Env: []corev1.EnvVar{
						{Name: "HOST_IP", Value: "127.0.0.1"},
					},
					// Setting HOST_IP env var in podTemplate
					PodTemplate: &crdbv1beta1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name: "cockroachdb",
									Env: []corev1.EnvVar{
										{Name: "HOST_IP", Value: "127.0.0.2"},
									},
								},
							},
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{Name: "test-pvc"},
						},
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			pod := newTestPod(&tc.Node)
			actual, err := json.MarshalIndent(pod, "", "  ")
			require.NoError(t, err)
			testutils.AssertGoldenJSON(t, tc.Name, actual)
		})
	}
}

func earSpec() *crdbv1beta1.EncryptionAtRest {
	earSpec := &crdbv1beta1.EncryptionAtRest{}

	keySecretName := "foo"
	earSpec.KeySecretName = &keySecretName

	cmekCredName := "secret_name"
	earSpec.CMEKCredentialsSecretName = &cmekCredName

	earSpec.OldKeySecretName = nil
	earSpec.Platform = "AWS_KMS"

	return earSpec
}

func TestGetNodeReadinessProbe(t *testing.T) {
	testCases := []struct {
		name          string
		customProbe   *corev1.Probe
		expectedProbe *corev1.Probe
	}{
		{
			name:        "nil-custom-probe-uses-defaults",
			customProbe: nil,
			expectedProbe: &corev1.Probe{
				FailureThreshold:    2,
				InitialDelaySeconds: 10,
				PeriodSeconds:       10,
				SuccessThreshold:    1,
				TimeoutSeconds:      10,
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path:   "/health?ready=1",
						Port:   intstr.FromString("http"),
						Scheme: corev1.URISchemeHTTPS,
					},
				},
			},
		},
		{
			name: "custom-probe-with-all-parameters",
			customProbe: &corev1.Probe{
				FailureThreshold:    20,
				InitialDelaySeconds: 15,
				PeriodSeconds:       5,
				SuccessThreshold:    2,
				TimeoutSeconds:      15,
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path:   "/health?ready=1",
						Port:   intstr.FromString("http"),
						Scheme: corev1.URISchemeHTTPS,
					},
				},
			},
			expectedProbe: &corev1.Probe{
				FailureThreshold:    20,
				InitialDelaySeconds: 15,
				PeriodSeconds:       5,
				SuccessThreshold:    2,
				TimeoutSeconds:      15,
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path:   "/health?ready=1",
						Port:   intstr.FromString("http"),
						Scheme: corev1.URISchemeHTTPS,
					},
				},
			},
		},
		{
			name: "custom-probe-with-partial-parameters",
			customProbe: &corev1.Probe{
				FailureThreshold:    30,
				InitialDelaySeconds: 5,
				// PeriodSeconds, SuccessThreshold, TimeoutSeconds not set - should be zero values
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path:   "/health?ready=1",
						Port:   intstr.FromString("http"),
						Scheme: corev1.URISchemeHTTPS,
					},
				},
			},
			expectedProbe: &corev1.Probe{
				FailureThreshold:    30,
				InitialDelaySeconds: 5,
				PeriodSeconds:       0, // zero value
				SuccessThreshold:    0, // zero value
				TimeoutSeconds:      0, // zero value
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path:   "/health?ready=1",
						Port:   intstr.FromString("http"),
						Scheme: corev1.URISchemeHTTPS,
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := getNodeReadinessProbe(tc.customProbe, true)
			assert.Equal(t, tc.expectedProbe, result)
		})
	}
}

func TestCrdbNodePodWithCustomReadinessProbe(t *testing.T) {
	testCases := []struct {
		name        string
		node        crdbv1beta1.CrdbNode
		expectProbe *corev1.Probe
	}{
		{
			name: "node-without-custom-readiness-probe",
			node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
					Annotations: map[string]string{
						"crdb.cockroachlabs.com/cloudProvider": "gcp",
					},
					Labels: map[string]string{
						"crdb.cockroachlabs.com/cluster": "crdb-test",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Certificates: crdbv1beta1.Certificates{
						ExternalCertificates: &crdbv1beta1.ExternalCertificates{
							ClientCAConfigMapName:   "client-ca",
							HTTPSecretName:          "ui-certs",
							NodeCAConfigMapName:     "node-ca",
							NodeClientSecretName:    "node-client-certs",
							NodeSecretName:          "node-certs",
							RootSQLClientSecretName: "client-certs",
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "testpvc",
								Namespace: "testns",
							},
						},
					},
					Image:                "testimage",
					ServiceAccountName:   "testsa",
					LoggingConfigMapName: "logging-test",
					LocalityLabels:       []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone},
					// ReadinessProbe is nil - should use defaults
				},
			},
			expectProbe: &corev1.Probe{
				FailureThreshold:    2,
				InitialDelaySeconds: 10,
				PeriodSeconds:       10,
				SuccessThreshold:    1,
				TimeoutSeconds:      10,
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path:   "/health?ready=1",
						Port:   intstr.FromString("http"),
						Scheme: corev1.URISchemeHTTPS,
					},
				},
			},
		},
		{
			name: "node-with-custom-readiness-probe",
			node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
					Annotations: map[string]string{
						"crdb.cockroachlabs.com/cloudProvider": "gcp",
					},
					Labels: map[string]string{
						"crdb.cockroachlabs.com/cluster": "crdb-test",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					TLSEnabled: true,
					Certificates: crdbv1beta1.Certificates{
						ExternalCertificates: &crdbv1beta1.ExternalCertificates{
							ClientCAConfigMapName:   "client-ca",
							HTTPSecretName:          "ui-certs",
							NodeCAConfigMapName:     "node-ca",
							NodeClientSecretName:    "node-client-certs",
							NodeSecretName:          "node-certs",
							RootSQLClientSecretName: "client-certs",
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "testpvc",
								Namespace: "testns",
							},
						},
					},
					Image:                "testimage",
					ServiceAccountName:   "testsa",
					LoggingConfigMapName: "logging-test",
					LocalityLabels:       []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone},
					ReadinessProbe: &corev1.Probe{
						FailureThreshold:    25,
						InitialDelaySeconds: 20,
						PeriodSeconds:       5,
						SuccessThreshold:    2,
						TimeoutSeconds:      15,
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path:   "/health?ready=1",
								Port:   intstr.FromString("http"),
								Scheme: corev1.URISchemeHTTPS,
							},
						},
					},
				},
			},
			expectProbe: &corev1.Probe{
				FailureThreshold:    25,
				InitialDelaySeconds: 20,
				PeriodSeconds:       5,
				SuccessThreshold:    2,
				TimeoutSeconds:      15,
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path:   "/health?ready=1",
						Port:   intstr.FromString("http"),
						Scheme: corev1.URISchemeHTTPS,
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pod := newTestPod(&tc.node)

			// Find the cockroachdb container
			var cockroachContainer *corev1.Container
			for i := range pod.Spec.Containers {
				if pod.Spec.Containers[i].Name == CRDBContainerName {
					cockroachContainer = &pod.Spec.Containers[i]
					break
				}
			}

			require.NotNil(t, cockroachContainer, "cockroachdb container should exist")
			require.NotNil(t, cockroachContainer.ReadinessProbe, "readiness probe should be set")

			assert.Equal(t, tc.expectProbe, cockroachContainer.ReadinessProbe)
		})
	}
}

// TestAzureSecretVolumesLogic tests the Azure secret volume configuration for CMEK/EAR scenarios
func TestAzureSecretVolumesLogic(t *testing.T) {
	testCases := []struct {
		name             string
		cloudProvider    string
		encryptionAtRest *crdbv1beta1.EncryptionAtRest
	}{
		{
			name:             "Azure without CMEK - secret should be optional",
			cloudProvider:    cloud.Azure.String(),
			encryptionAtRest: nil,
		},
		{
			name:          "Azure with CMEK enabled - secret should be optional",
			cloudProvider: cloud.Azure.String(),
			encryptionAtRest: &crdbv1beta1.EncryptionAtRest{
				Platform: crdbv1beta1.EncryptionPlatformAzureKeyVault,
				CMEKCredentialsSecretName: func() *string {
					s := "azure-cmek-secret"
					return &s
				}(),
			},
		},
		{
			name:          "Azure with encryption but non-Azure platform - secret should be optional",
			cloudProvider: cloud.Azure.String(),
			encryptionAtRest: &crdbv1beta1.EncryptionAtRest{
				Platform: crdbv1beta1.EncryptionPlatformAwsKms,
				CMEKCredentialsSecretName: func() *string {
					s := "aws-cmek-secret"
					return &s
				}(),
			},
		},
		{
			name:             "GCP provider - no Azure volume should exist",
			cloudProvider:    "gcp",
			encryptionAtRest: nil,
		},
		{
			name:             "AWS provider - no Azure volume should exist",
			cloudProvider:    "aws",
			encryptionAtRest: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			node := &crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
					Annotations: map[string]string{
						"crdb.cockroachlabs.com/cloudProvider": tc.cloudProvider,
					},
					Labels: map[string]string{
						"crdb.cockroachlabs.com/cluster": "crdb-test",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Certificates: crdbv1beta1.Certificates{
						ExternalCertificates: &crdbv1beta1.ExternalCertificates{
							ClientCAConfigMapName:   "client-ca",
							HTTPSecretName:          "ui-certs",
							NodeCAConfigMapName:     "node-ca",
							NodeClientSecretName:    "node-client-certs",
							NodeSecretName:          "node-certs",
							RootSQLClientSecretName: "client-certs",
						},
					},
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "testpvc",
								Namespace: "testns",
							},
						},
					},
					Image:                "testimage",
					ServiceAccountName:   "testsa",
					LoggingConfigMapName: "logging-test",
					LocalityLabels:       []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone},
					EncryptionAtRest:     tc.encryptionAtRest,
				},
			}

			pod := CrdbNodePod(node, &corev1.Node{})
			require.NotNil(t, pod)

			// Find Azure credentials volume
			var azureVolume *corev1.Volume
			for i, volume := range pod.Spec.Volumes {
				if volume.Name == "cluster-identity-credentials" {
					azureVolume = &pod.Spec.Volumes[i]
					break
				}
			}

			// For Azure providers, volume should always exist with Optional=true
			// For non-Azure providers, volume should not exist
			if tc.cloudProvider == cloud.Azure.String() {
				require.NotNil(t, azureVolume, "Azure credentials volume should exist for Azure provider")
				require.NotNil(t, azureVolume.Secret, "Azure volume should have secret source")
				require.Equal(t, AzureCredentialsSecretName, azureVolume.Secret.SecretName)
				require.True(t, *azureVolume.Secret.Optional, "Azure credentials secret should be optional")
			} else {
				require.Nil(t, azureVolume, "Azure credentials volume should not exist for non-Azure providers")
			}
		})
	}
}

// TestNodeHashRevisionExcludesBasaltScratchSize verifies that the basalt scratch
// PVC size is excluded from the pod revision hash, so changing it resizes the
// PVC in place rather than triggering a rolling update. The pod references the
// scratch PVC by name, so its size never appears in the pod spec.
func TestNodeHashRevisionExcludesBasaltScratchSize(t *testing.T) {
	basaltNode := func(size string) *crdbv1beta1.CrdbNode {
		return &crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
			Spec: crdbv1beta1.CrdbNodeSpec{
				Image: "v1",
				DataStore: crdbv1beta1.DataStore{
					Basalt: &crdbv1beta1.BasaltStoreSpec{
						ControllerAddrs: []string{"10.0.0.1:26256"},
						TenantID:        "t",
						ScratchVolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							Spec: corev1.PersistentVolumeClaimSpec{
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
								},
							},
						},
					},
				},
			},
		}
	}

	require.Equal(t, GetNodeHashRevisionNew(basaltNode("40Gi")), GetNodeHashRevisionNew(basaltNode("80Gi")),
		"scratch PVC size must be excluded from the new revision hash so it resizes in place")
}

// findVolume returns the pod volume with the given name, or nil.
func findVolume(pod *corev1.Pod, name string) *corev1.Volume {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}

// TestCrdbNodePodBasaltScratchVolume verifies that a Basalt-backed node's local
// scratch dir (/cockroach/cockroach-data) is backed by a PVC when a
// ScratchVolumeClaimTemplate is configured, and falls back to an ephemeral
// EmptyDir otherwise.
func TestCrdbNodePodBasaltScratchVolume(t *testing.T) {
	basaltNode := func(scratch *corev1.PersistentVolumeClaim) *crdbv1beta1.CrdbNode {
		return &crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test",
				Namespace:   "testns",
				Annotations: map[string]string{"crdb.cockroachlabs.com/cloudProvider": "gcp"},
				Labels:      map[string]string{"crdb.cockroachlabs.com/cluster": "crdb-test"},
			},
			Spec: crdbv1beta1.CrdbNodeSpec{
				TLSEnabled: true,
				Certificates: crdbv1beta1.Certificates{
					ExternalCertificates: &crdbv1beta1.ExternalCertificates{
						ClientCAConfigMapName:   "client-ca",
						HTTPSecretName:          "ui-certs",
						NodeCAConfigMapName:     "node-ca",
						NodeClientSecretName:    "node-client-certs",
						NodeSecretName:          "node-certs",
						RootSQLClientSecretName: "client-certs",
					},
				},
				DataStore: crdbv1beta1.DataStore{
					Basalt: &crdbv1beta1.BasaltStoreSpec{
						ControllerAddrs:            []string{"10.0.0.1:26256"},
						TenantID:                   "11111111-1111-1111-1111-111111111111",
						ScratchVolumeClaimTemplate: scratch,
					},
				},
				Image:              "testimage",
				ServiceAccountName: "testsa",
			},
		}
	}

	t.Run("without scratch template falls back to EmptyDir", func(t *testing.T) {
		pod := newTestPod(basaltNode(nil))
		vol := findVolume(pod, CockroachDataDirVolumeName)
		require.NotNil(t, vol, "datadir volume must exist")
		require.NotNil(t, vol.EmptyDir, "basalt datadir should be an EmptyDir when no scratch PVC is configured")
		require.Nil(t, vol.PersistentVolumeClaim)
	})

	t.Run("with scratch template uses a PVC", func(t *testing.T) {
		scratch := &corev1.PersistentVolumeClaim{
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("40Gi")},
				},
			},
		}
		pod := newTestPod(basaltNode(scratch))
		vol := findVolume(pod, CockroachDataDirVolumeName)
		require.NotNil(t, vol, "datadir volume must exist")
		require.Nil(t, vol.EmptyDir, "basalt datadir should not be an EmptyDir when a scratch PVC is configured")
		require.NotNil(t, vol.PersistentVolumeClaim, "basalt datadir should be backed by a PVC")
		require.Equal(t, CrdbNodeScratchPVCName("test", scratch), vol.PersistentVolumeClaim.ClaimName)
	})
}
