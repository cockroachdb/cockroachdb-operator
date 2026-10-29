package manager_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manager"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestWebhookHandler_Handle(t *testing.T) {
	host := crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "multi-tenant-ns",
			Name:      "host-cluster",
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			TenantHost: &crdbv1beta1.TenantHost{
				TenantPools: []crdbv1beta1.TenantPool{
					{Name: "free", MaxPods: new(int32(1))},
					{Name: "paid"},
				},
			},
		},
	}
	testConfigMap := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config-map",
			Namespace: "multi-tenant-ns",
		},
	}
	testSecret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "multi-tenant-ns",
		},
	}

	ctl := kubetest.NewFakeCtl(kubetest.Options{}, &host, &testConfigMap, &testSecret)

	region := "us-east1"
	provider := cloud.K3D
	handler := manager.NewWebhook(ctl, cloud.Environment{
		CloudRegion:   region,
		CloudProvider: provider,
	})

	invokeHandler := makeInvokeHandler(t, handler)

	t.Run("valid basalt object is admitted by the webhook", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: string(provider),
						Code:          region,
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
						StartFlags: &crdbv1beta1.Flags{
							Upsert: []string{"--max-offset=200ms"},
						},
					},
				},
			},
		}, admissionv1.Create)
		require.True(t, resp.Allowed)
	})

	const basaltTenantUUID = "5e0c10f6-5bc7-5be1-bbb7-78cec71d8570"
	basaltCluster := func(mut func(*crdbv1beta1.BasaltStoreSpec)) *crdbv1beta1.CrdbCluster {
		spec := &crdbv1beta1.BasaltStoreSpec{
			ControllerAddrs: []string{"10.0.0.2:26256"},
			TenantID:        basaltTenantUUID,
			TLS: &crdbv1beta1.BasaltTLSSpec{
				ClientSecretName: "basalt-tenant-client",
				CAConfigMapName:  "basalt-server-ca",
			},
		}
		if mut != nil {
			mut(spec)
		}
		return &crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{CloudProvider: string(provider), Code: region},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						Image:      "cockroachdb/cockroach:v26.3.0-basalt",
						DataStore:  crdbv1beta1.DataStore{Basalt: spec},
						StartFlags: &crdbv1beta1.Flags{},
					},
				},
			},
		}
	}

	// admitBasalt and rejectBasalt apply a tweak to the base basalt spec and
	// assert the verdict. A nil tweak leaves the base spec untouched.
	admitBasalt := func(t *testing.T, mut func(*crdbv1beta1.BasaltStoreSpec)) {
		t.Helper()
		resp := invokeHandler(basaltCluster(mut), admissionv1.Create)
		require.True(t, resp.Allowed, admissionMessage(resp))
	}
	rejectBasalt := func(t *testing.T, mut func(*crdbv1beta1.BasaltStoreSpec), wantMsg string) {
		t.Helper()
		resp := invokeHandler(basaltCluster(mut), admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Contains(t, admissionMessage(resp), wantMsg)
	}

	t.Run("valid basalt cluster is admitted", func(t *testing.T) {
		admitBasalt(t, nil)
	})

	t.Run("basalt with a volume claim template is rejected", func(t *testing.T) {
		c := basaltCluster(nil)
		c.Spec.Template.Spec.DataStore.VolumeClaimTemplate = &corev1.PersistentVolumeClaim{}
		resp := invokeHandler(c, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Contains(t, resp.Result.Message, "exactly one of VolumeClaimTemplate, VolumeSource, or Basalt")
	})

	t.Run("basalt without a tenant id is rejected", func(t *testing.T) {
		rejectBasalt(t, func(s *crdbv1beta1.BasaltStoreSpec) {
			s.TenantID = ""
		}, "tenantID is required")
	})

	t.Run("basalt with no controller addrs is rejected", func(t *testing.T) {
		rejectBasalt(t, func(s *crdbv1beta1.BasaltStoreSpec) {
			s.ControllerAddrs = nil
		}, "controllerAddrs must not be empty")
	})

	t.Run("basalt insecure is admitted without tenant secret and configmap", func(t *testing.T) {
		admitBasalt(t, func(s *crdbv1beta1.BasaltStoreSpec) {
			s.Insecure = new(true)
			s.TLS = nil
		})
	})

	// A cluster that predates secure mode carries neither field and still has to
	// be admitted, otherwise the operator upgrade rejects an unchanged spec.
	t.Run("basalt with neither insecure nor tls is admitted", func(t *testing.T) {
		admitBasalt(t, func(s *crdbv1beta1.BasaltStoreSpec) {
			s.Insecure = nil
			s.TLS = nil
		})
	})

	t.Run("basalt with tls and no insecure field is admitted", func(t *testing.T) {
		admitBasalt(t, func(s *crdbv1beta1.BasaltStoreSpec) {
			s.Insecure = nil
		})
	})

	t.Run("basalt with insecure false and no tls is rejected", func(t *testing.T) {
		rejectBasalt(t, func(s *crdbv1beta1.BasaltStoreSpec) {
			s.Insecure = new(false)
			s.TLS = nil
		}, "tls is required when insecure is false")
	})

	t.Run("basalt insecure with tls is rejected", func(t *testing.T) {
		rejectBasalt(t, func(s *crdbv1beta1.BasaltStoreSpec) {
			s.Insecure = new(true)
		}, "tls must not be set when insecure is true")
	})

	t.Run("basalt secure without a client secret is rejected", func(t *testing.T) {
		rejectBasalt(t, func(s *crdbv1beta1.BasaltStoreSpec) {
			s.TLS.ClientSecretName = ""
		}, "clientSecretName is required")
	})

	t.Run("basalt secure without a ca configmap is rejected", func(t *testing.T) {
		rejectBasalt(t, func(s *crdbv1beta1.BasaltStoreSpec) {
			s.TLS.CAConfigMapName = ""
		}, "caConfigMapName is required")
	})

	// An operator that starts insecure can be secured later, the same way a
	// cockroach cluster can. The webhook must not treat the flip as immutable.
	t.Run("basalt insecure cluster can be secured on update", func(t *testing.T) {
		invokeWithOld := makeInvokeHandlerWithOldObject(t, handler)
		old := basaltCluster(func(s *crdbv1beta1.BasaltStoreSpec) {
			s.Insecure = new(true)
			s.TLS = nil
		})
		updated := basaltCluster(nil)
		resp := invokeWithOld(updated, old, admissionv1.Update)
		require.True(t, resp.Allowed)
	})

	keySecret := "ear-store-us-east-1-v-0"

	t.Run("basalt with an unwrapped cluster key is admitted", func(t *testing.T) {
		c := basaltCluster(nil)
		c.Spec.Regions[0].EncryptionAtRest = &crdbv1beta1.EncryptionAtRest{
			KeySecretName: &keySecret,
			Platform:      crdbv1beta1.EncryptionPlatformUnknown,
		}
		resp := invokeHandler(c, admissionv1.Create)
		require.True(t, resp.Allowed)
	})

	// The init container unwraps the KMS envelope before writing the key file,
	// on a path that does not branch on store type, so a wrapped key reaches
	// --cluster-key-dir as plaintext exactly as an unwrapped one does.
	t.Run("basalt with a KMS-wrapped key is admitted", func(t *testing.T) {
		c := basaltCluster(nil)
		c.Spec.Regions[0].EncryptionAtRest = &crdbv1beta1.EncryptionAtRest{
			KeySecretName: &keySecret,
			Platform:      crdbv1beta1.EncryptionPlatformAwsKms,
		}
		resp := invokeHandler(c, admissionv1.Create)
		require.True(t, resp.Allowed)
	})

	// Whether the KMS key belongs to CRL or to the customer is not expressible
	// in this spec, since both are a principal plus a key reference. The webhook
	// must admit credentials rather than guess at ownership.
	t.Run("basalt with CMEK credentials is admitted", func(t *testing.T) {
		creds := "cmek-user-creds"
		c := basaltCluster(nil)
		c.Spec.Regions[0].EncryptionAtRest = &crdbv1beta1.EncryptionAtRest{
			KeySecretName:             &keySecret,
			Platform:                  crdbv1beta1.EncryptionPlatformAwsKms,
			CMEKCredentialsSecretName: &creds,
		}
		resp := invokeHandler(c, admissionv1.Create)
		require.True(t, resp.Allowed)
	})

	// A second key is what a rotation stages. The key directory renders the same
	// either way, so the operator can carry it before the control plane knows how
	// to sequence one.
	t.Run("basalt with an old key secret is admitted", func(t *testing.T) {
		oldSecret := "ear-store-us-east-1-v-0"
		c := basaltCluster(nil)
		c.Spec.Regions[0].EncryptionAtRest = &crdbv1beta1.EncryptionAtRest{
			KeySecretName:    &keySecret,
			OldKeySecretName: &oldSecret,
		}
		resp := invokeHandler(c, admissionv1.Create)
		require.True(t, resp.Allowed)
	})

	t.Run("basalt with a non-UUID tenant id is rejected", func(t *testing.T) {
		resp := invokeHandler(basaltCluster(func(s *crdbv1beta1.BasaltStoreSpec) {
			s.TenantID = "not-a-uuid"
		}), admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Contains(t, resp.Result.Message, "must be a valid UUID")
	})

	t.Run("basalt with a malformed controller addr is rejected", func(t *testing.T) {
		resp := invokeHandler(basaltCluster(func(s *crdbv1beta1.BasaltStoreSpec) {
			s.ControllerAddrs = []string{"10.0.0.2:26256, 10.0.0.3:26256"}
		}), admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Contains(t, resp.Result.Message, "host:port")
	})

	// TODO(prod-orch): remove this test when multi-region basalt is supported.
	t.Run("basalt multi-region cluster is rejected for now", func(t *testing.T) {
		c := basaltCluster(nil)
		c.Spec.Regions = append(c.Spec.Regions, crdbv1beta1.CrdbClusterRegion{
			CloudProvider: string(provider), Code: "us-west1",
		})
		resp := invokeHandler(c, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Contains(t, resp.Result.Message, "multi-region clusters are not yet supported with basalt")
	})

	// TODO(prod-orch): remove this test when basalt-compatible WAL failover is supported.
	t.Run("basalt with WAL failover is rejected for now", func(t *testing.T) {
		c := basaltCluster(nil)
		c.Spec.Template.Spec.WALFailoverSpec = &crdbv1beta1.CrdbWalFailoverSpec{
			Name: "wal", Size: "10Gi", StorageClassName: "fast", Status: crdbv1beta1.WalEnable,
		}
		resp := invokeHandler(c, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Contains(t, resp.Result.Message, "WAL failover is not yet supported with basalt")
	})

	// TODO(prod-orch): remove this test when multi-store basalt is supported.
	t.Run("basalt storeCount > 1 is rejected on create", func(t *testing.T) {
		c := basaltCluster(func(s *crdbv1beta1.BasaltStoreSpec) {
			s.StoreCount = new(int32(3))
		})
		resp := invokeHandler(c, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Contains(t, resp.Result.Message, "multiple stores per node are not yet supported")
	})

	t.Run("basalt rejects --store in startFlags.upsert", func(t *testing.T) {
		c := basaltCluster(nil)
		c.Spec.Template.Spec.StartFlags = &crdbv1beta1.Flags{
			Upsert: []string{`--store=path=/data`},
		}
		resp := invokeHandler(c, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Contains(t, resp.Result.Message, "--store flags are generated by the operator")
	})

	t.Run("basalt rejects --store in startFlags.omit", func(t *testing.T) {
		c := basaltCluster(nil)
		c.Spec.Template.Spec.StartFlags = &crdbv1beta1.Flags{
			Omit: []string{"--store"},
		}
		resp := invokeHandler(c, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Contains(t, resp.Result.Message, "--store flags are generated by the operator")
	})

	t.Run("basalt tenantID is immutable on update", func(t *testing.T) {
		invokeWithOld := makeInvokeHandlerWithOldObject(t, handler)
		old := basaltCluster(nil)
		updated := basaltCluster(func(s *crdbv1beta1.BasaltStoreSpec) {
			s.TenantID = "11111111-2222-3333-4444-555555555555"
		})
		resp := invokeWithOld(updated, old, admissionv1.Update)
		require.False(t, resp.Allowed)
		require.Contains(t, resp.Result.Message, "tenantID is immutable")
	})

	// Both directions brick the cluster on the next pod restart. Adding a key
	// leaves already-written plaintext unreadable by the fail-closed
	// range-shared FS, and removing one makes the encrypted data unreadable.
	t.Run("basalt encryptionAtRest cannot be added on update", func(t *testing.T) {
		invokeWithOld := makeInvokeHandlerWithOldObject(t, handler)
		secret := "ear-store-us-east-1-v-0"
		old := basaltCluster(nil)
		updated := basaltCluster(nil)
		updated.Spec.Regions[0].EncryptionAtRest = &crdbv1beta1.EncryptionAtRest{
			KeySecretName: &secret,
		}
		resp := invokeWithOld(updated, old, admissionv1.Update)
		require.False(t, resp.Allowed)
		require.Contains(t, resp.Result.Message, "encryptionAtRest is immutable")
	})

	t.Run("basalt encryptionAtRest cannot be removed on update", func(t *testing.T) {
		invokeWithOld := makeInvokeHandlerWithOldObject(t, handler)
		secret := "ear-store-us-east-1-v-0"
		old := basaltCluster(nil)
		old.Spec.Regions[0].EncryptionAtRest = &crdbv1beta1.EncryptionAtRest{
			KeySecretName: &secret,
		}
		updated := basaltCluster(nil)
		resp := invokeWithOld(updated, old, admissionv1.Update)
		require.False(t, resp.Allowed)
		require.Contains(t, resp.Result.Message, "encryptionAtRest is immutable")
	})

	t.Run("basalt encryptionAtRest unchanged is allowed on update", func(t *testing.T) {
		invokeWithOld := makeInvokeHandlerWithOldObject(t, handler)
		secret := "ear-store-us-east-1-v-0"
		withKey := func() *crdbv1beta1.CrdbCluster {
			c := basaltCluster(nil)
			c.Spec.Regions[0].EncryptionAtRest = &crdbv1beta1.EncryptionAtRest{
				KeySecretName: &secret,
			}
			return c
		}
		resp := invokeWithOld(withKey(), withKey(), admissionv1.Update)
		require.True(t, resp.Allowed)
	})

	// The basalt EAR immutability rule must not leak onto volume-backed
	// clusters. Those legitimately gain EncryptionAtRest when CMEK is enabled
	// and lose it when CMEK is disabled, and their store-local registry
	// tolerates files written before encryption.
	t.Run("volume-backed clusters can still enable and disable CMEK", func(t *testing.T) {
		invokeWithOld := makeInvokeHandlerWithOldObject(t, handler)
		secret := "cmek-store-us-east-1-v-0"
		creds := "cmek-user-creds"
		pvcCluster := func(ear *crdbv1beta1.EncryptionAtRest) *crdbv1beta1.CrdbCluster {
			return &crdbv1beta1.CrdbCluster{
				Spec: crdbv1beta1.CrdbClusterSpec{
					Regions: []crdbv1beta1.CrdbClusterRegion{
						{CloudProvider: string(provider), Code: region, EncryptionAtRest: ear},
					},
					Template: crdbv1beta1.CrdbNodeTemplate{
						Spec: crdbv1beta1.CrdbNodeSpec{
							DataStore: crdbv1beta1.DataStore{
								VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
							},
						},
					},
				},
			}
		}
		withCMEK := &crdbv1beta1.EncryptionAtRest{
			KeySecretName:             &secret,
			Platform:                  crdbv1beta1.EncryptionPlatformAwsKms,
			CMEKCredentialsSecretName: &creds,
		}

		t.Run("enable", func(t *testing.T) {
			resp := invokeWithOld(pvcCluster(withCMEK), pvcCluster(nil), admissionv1.Update)
			require.True(t, resp.Allowed, resp.Result.String())
		})

		t.Run("disable", func(t *testing.T) {
			resp := invokeWithOld(pvcCluster(nil), pvcCluster(withCMEK), admissionv1.Update)
			require.True(t, resp.Allowed, resp.Result.String())
		})
	})

	t.Run("basalt storeCount is immutable on update", func(t *testing.T) {
		invokeWithOld := makeInvokeHandlerWithOldObject(t, handler)
		old := basaltCluster(nil)
		updated := basaltCluster(func(s *crdbv1beta1.BasaltStoreSpec) {
			s.StoreCount = new(int32(3))
		})
		resp := invokeWithOld(updated, old, admissionv1.Update)
		require.False(t, resp.Allowed)
		require.Contains(t, resp.Result.Message, "storeCount is immutable")
	})

	t.Run("storage backend is immutable on update", func(t *testing.T) {
		invokeWithOld := makeInvokeHandlerWithOldObject(t, handler)
		old := basaltCluster(nil)
		updated := basaltCluster(nil)
		updated.Spec.Template.Spec.DataStore = crdbv1beta1.DataStore{
			VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
		}
		resp := invokeWithOld(updated, old, admissionv1.Update)
		require.False(t, resp.Allowed)
		require.Contains(t, resp.Result.Message, "data store backend is immutable")
	})

	t.Run("host clusters do not support insecure clusters", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				TenantHost: &crdbv1beta1.TenantHost{},
			},
		}, admissionv1.Update)
		require.False(t, resp.Allowed)
		require.Equal(t, "host clusters (TenantHost!=nil) do not support insecure clusters (TLSEnabled=false)", resp.Result.Message)
	})

	t.Run("host clusters may not have duplicate tenant pools", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: true,
				TenantHost: &crdbv1beta1.TenantHost{
					TenantPools: []crdbv1beta1.TenantPool{
						{Name: "free"},
						{Name: "free"},
					},
				},
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Equal(t, "duplicate tenant pool free", resp.Result.Message)
	})

	t.Run("HostReplicaMultiplier should be non negative", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: true,
				TenantHost: &crdbv1beta1.TenantHost{
					TenantPools: []crdbv1beta1.TenantPool{
						{Name: "free", HostReplicaMultiplier: new(float32(-0.3))},
					},
				},
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Equal(t, "HostReplicaMultiplier has to be non-negative (>= 0)", resp.Result.Message)
	})

	t.Run("tenant is admitted", func(t *testing.T) {
		const tenantID = crdbv1beta1.TenantID(10)
		resp := invokeHandler(&crdbv1beta1.CrdbTenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tenantID.Tag(),
				Namespace: "multi-tenant-ns",
			},
			Spec: crdbv1beta1.CrdbTenantSpec{
				TenantID:                tenantID,
				Pods:                    10,
				TenantPool:              new("paid"),
				AllowedCIDRRanges:       []string{"1.1.1.1/32"},
				AllowedPrivateEndpoints: []string{"test"},
				OtelCollector: &crdbv1beta1.OtelCollector{
					ConfigMapName: "test-config-map",
					SecretName:    "test-secret",
				},
			},
		}, admissionv1.Create)
		require.True(t, resp.Allowed)
	})

	t.Run("tenant's name does not match ID", func(t *testing.T) {
		const tenantID = crdbv1beta1.TenantID(10)
		resp := invokeHandler(&crdbv1beta1.CrdbTenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "non-matching-name",
				Namespace: "multi-tenant-ns",
			},
			Spec: crdbv1beta1.CrdbTenantSpec{
				TenantID:   tenantID,
				Pods:       1,
				TenantPool: new("free"),
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		errText := fmt.Sprintf("tenant name must be 'tenant-%d', not 'non-matching-name'", tenantID)
		require.Equal(t, errText, resp.Result.Message)
	})

	t.Run("tenant cannot have more than Spec.MaxPods", func(t *testing.T) {
		const tenantID = crdbv1beta1.TenantID(10)
		resp := invokeHandler(&crdbv1beta1.CrdbTenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tenantID.Tag(),
				Namespace: "multi-tenant-ns",
			},
			Spec: crdbv1beta1.CrdbTenantSpec{
				TenantID:   tenantID,
				Pods:       6,
				MaxPods:    new(int32(5)),
				TenantPool: new("paid"),
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Equal(t, "tenant cannot have more than 5 pod(s)", resp.Result.Message)
	})

	t.Run("tenant cannot have more than TenantHost.MaxPods", func(t *testing.T) {
		const tenantID = crdbv1beta1.TenantID(10)
		resp := invokeHandler(&crdbv1beta1.CrdbTenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tenantID.Tag(),
				Namespace: "multi-tenant-ns",
			},
			Spec: crdbv1beta1.CrdbTenantSpec{
				TenantID:   tenantID,
				Pods:       2,
				TenantPool: new("free"),
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Equal(t, "tenant cannot have more than 1 pod(s)", resp.Result.Message)
	})

	t.Run("tenant cannot have empty private endpoint entries", func(t *testing.T) {
		const tenantID = crdbv1beta1.TenantID(10)
		resp := invokeHandler(&crdbv1beta1.CrdbTenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tenantID.Tag(),
				Namespace: "multi-tenant-ns",
			},
			Spec: crdbv1beta1.CrdbTenantSpec{
				TenantID:                tenantID,
				TenantPool:              new("free"),
				AllowedPrivateEndpoints: []string{"test", ""},
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Equal(t, "tenant cannot have private endpoint IDs which are empty", resp.Result.Message)
	})

	t.Run("tenant cannot have invalid cidr ranges", func(t *testing.T) {
		const tenantID = crdbv1beta1.TenantID(10)
		resp := invokeHandler(&crdbv1beta1.CrdbTenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tenantID.Tag(),
				Namespace: "multi-tenant-ns",
			},
			Spec: crdbv1beta1.CrdbTenantSpec{
				TenantID:          tenantID,
				TenantPool:        new("free"),
				AllowedCIDRRanges: []string{"1.1.1.1/32", "not-cidr-range"},
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Equal(t, "tenant cannot have invalid cidr: invalid CIDR address: not-cidr-range", resp.Result.Message)
	})

	t.Run("OtelCollector", func(t *testing.T) {
		const tenantID = crdbv1beta1.TenantID(10)
		tenantObj := &crdbv1beta1.CrdbTenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tenantID.Tag(),
				Namespace: "multi-tenant-ns",
			},
			Spec: crdbv1beta1.CrdbTenantSpec{
				TenantID:   tenantID,
				TenantPool: new("free"),
			},
		}
		t.Run("ConfigMap name cannot be empty", func(t *testing.T) {
			tenantObj.Spec.OtelCollector = &crdbv1beta1.OtelCollector{
				ConfigMapName: "",
				SecretName:    "",
			}
			resp := invokeHandler(tenantObj, admissionv1.Create)
			require.False(t, resp.Allowed)
			require.Equal(t, "ConfigMap name for otelCollector cannot be empty", resp.Result.Message)
		})
		t.Run("ConfigMap points to an invalid entry", func(t *testing.T) {
			tenantObj.Spec.OtelCollector = &crdbv1beta1.OtelCollector{
				ConfigMapName: "invalid-cm",
				SecretName:    "",
			}
			resp := invokeHandler(tenantObj, admissionv1.Create)
			require.False(t, resp.Allowed)
			require.Equal(t, "invalid-cm ConfigMap not found", resp.Result.Message)
		})
		t.Run("Secret points to an invalid entry", func(t *testing.T) {
			tenantObj.Spec.OtelCollector = &crdbv1beta1.OtelCollector{
				ConfigMapName: "test-config-map",
				SecretName:    "invalid-secret",
			}
			resp := invokeHandler(tenantObj, admissionv1.Create)
			require.False(t, resp.Allowed)
			require.Equal(t, "invalid-secret Secret not found", resp.Result.Message)
		})
		t.Run("admitted without a secret", func(t *testing.T) {
			tenantObj.Spec.OtelCollector = &crdbv1beta1.OtelCollector{
				ConfigMapName: "test-config-map",
				SecretName:    "",
			}
			resp := invokeHandler(tenantObj, admissionv1.Create)
			require.True(t, resp.Allowed)
		})
	})

	t.Run("Fluentbit", func(t *testing.T) {
		const tenantID = crdbv1beta1.TenantID(10)
		tenantObj := &crdbv1beta1.CrdbTenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tenantID.Tag(),
				Namespace: "multi-tenant-ns",
			},
			Spec: crdbv1beta1.CrdbTenantSpec{
				TenantID:   tenantID,
				TenantPool: new("free"),
			},
		}
		t.Run("ConfigMap name cannot be empty", func(t *testing.T) {
			tenantObj.Spec.Fluentbit = &crdbv1beta1.Fluentbit{
				ConfigMapName: "",
			}
			resp := invokeHandler(tenantObj, admissionv1.Create)
			require.False(t, resp.Allowed)
			require.Equal(t, "ConfigMap name for fluentbit cannot be empty", resp.Result.Message)
		})
		t.Run("ConfigMap points to an invalid entry", func(t *testing.T) {
			tenantObj.Spec.Fluentbit = &crdbv1beta1.Fluentbit{
				ConfigMapName: "invalid-cm",
			}
			resp := invokeHandler(tenantObj, admissionv1.Create)
			require.False(t, resp.Allowed)
			require.Equal(t, "invalid-cm ConfigMap not found", resp.Result.Message)
		})
		t.Run("admitted", func(t *testing.T) {
			tenantObj.Spec.Fluentbit = &crdbv1beta1.Fluentbit{
				ConfigMapName: "test-config-map",
			}
			resp := invokeHandler(tenantObj, admissionv1.Create)
			require.True(t, resp.Allowed)
		})
	})

	t.Run("create tenant with no host", func(t *testing.T) {
		const tenantID = crdbv1beta1.TenantID(7)
		resp := invokeHandler(&crdbv1beta1.CrdbTenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tenantID.Tag(),
				Namespace: "some-other-ns",
			},
			Spec: crdbv1beta1.CrdbTenantSpec{
				TenantID: tenantID,
				Pods:     1,
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Equal(t, "no host cluster found in namespace some-other-ns", resp.Result.Message)
	})

	t.Run("update tenant with no host", func(t *testing.T) {
		const tenantID = crdbv1beta1.TenantID(7)
		resp := invokeHandler(&crdbv1beta1.CrdbTenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tenantID.Tag(),
				Namespace: "some-other-ns",
			},
			Spec: crdbv1beta1.CrdbTenantSpec{
				TenantID: tenantID,
				Pods:     1,
			},
		}, admissionv1.Update)
		require.True(t, resp.Allowed)
	})

	t.Run("tenant with default tenantpool", func(t *testing.T) {
		const tenantID = crdbv1beta1.TenantID(7)
		resp := invokeHandler(&crdbv1beta1.CrdbTenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tenantID.Tag(),
				Namespace: "multi-tenant-ns",
			},
			Spec: crdbv1beta1.CrdbTenantSpec{
				TenantID: tenantID,
				Pods:     1,
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Equal(t, "tenant pool default does not exist", resp.Result.Message)
	})

	t.Run("tenant with missing tenantpool", func(t *testing.T) {
		const tenantID = crdbv1beta1.TenantID(7)
		resp := invokeHandler(&crdbv1beta1.CrdbTenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tenantID.Tag(),
				Namespace: "multi-tenant-ns",
			},
			Spec: crdbv1beta1.CrdbTenantSpec{
				TenantID:   tenantID,
				Pods:       1,
				TenantPool: new("MAXIMUM"),
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Equal(t, "tenant pool MAXIMUM does not exist", resp.Result.Message)
	})

	// TODO(nishanth): remove or update these ownerRef bypass tests once all
	// tenant ownerRefs have been migrated to v1beta1 and the bypass in
	// handleTenant is removed.
	invokeWithOld := makeInvokeHandlerWithOldObject(t, handler)

	t.Run("ownerRef-only update with stale pool is rejected", func(t *testing.T) {
		const tenantID = crdbv1beta1.TenantID(99)
		oldTenant := &crdbv1beta1.CrdbTenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tenantID.Tag(),
				Namespace: "multi-tenant-ns",
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "crdb.cockroachlabs.com/v1alpha1",
						Kind:       "CrdbCluster",
						Name:       "host-cluster",
						UID:        "abc-123",
					},
				},
			},
			Spec: crdbv1beta1.CrdbTenantSpec{
				TenantID:   tenantID,
				TenantPool: new("GONE"),
			},
		}
		newTenant := oldTenant.DeepCopy()
		newTenant.OwnerReferences[0].APIVersion = "crdb.cockroachlabs.com/v1beta1"

		resp := invokeWithOld(newTenant, oldTenant, admissionv1.Update)
		require.False(t, resp.Allowed, "ownerRef-only update with stale pool should be rejected")
		require.Equal(t, "tenant pool GONE does not exist", resp.Result.Message)
	})

	t.Run("unchanged ownerRef with stale pool is still rejected", func(t *testing.T) {
		const tenantID = crdbv1beta1.TenantID(99)
		oldTenant := &crdbv1beta1.CrdbTenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tenantID.Tag(),
				Namespace: "multi-tenant-ns",
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "crdb.cockroachlabs.com/v1beta1",
						Kind:       "CrdbCluster",
						Name:       "host-cluster",
						UID:        "abc-123",
					},
				},
			},
			Spec: crdbv1beta1.CrdbTenantSpec{
				TenantID:   tenantID,
				TenantPool: new("GONE"),
			},
		}
		newTenant := oldTenant.DeepCopy()

		resp := invokeWithOld(newTenant, oldTenant, admissionv1.Update)
		require.False(t, resp.Allowed, "unchanged ownerRef with stale pool should be rejected")
		require.Equal(t, "tenant pool GONE does not exist", resp.Result.Message)
	})

	t.Run("spec change with stale pool is still rejected", func(t *testing.T) {
		const tenantID = crdbv1beta1.TenantID(99)
		oldTenant := &crdbv1beta1.CrdbTenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tenantID.Tag(),
				Namespace: "multi-tenant-ns",
			},
			Spec: crdbv1beta1.CrdbTenantSpec{
				TenantID:   tenantID,
				Pods:       0,
				TenantPool: new("GONE"),
			},
		}
		newTenant := oldTenant.DeepCopy()
		newTenant.Spec.Pods = 1

		resp := invokeWithOld(newTenant, oldTenant, admissionv1.Update)
		require.False(t, resp.Allowed, "spec change with stale pool should be rejected")
		require.Equal(t, "tenant pool GONE does not exist", resp.Result.Message)
	})

	t.Run("ownerRef change with spec change is still rejected", func(t *testing.T) {
		const tenantID = crdbv1beta1.TenantID(99)
		oldTenant := &crdbv1beta1.CrdbTenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tenantID.Tag(),
				Namespace: "multi-tenant-ns",
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "crdb.cockroachlabs.com/v1alpha1",
						Kind:       "CrdbCluster",
						Name:       "host-cluster",
						UID:        "abc-123",
					},
				},
			},
			Spec: crdbv1beta1.CrdbTenantSpec{
				TenantID:   tenantID,
				Pods:       0,
				TenantPool: new("GONE"),
			},
		}
		// The update bumps the ownerRef APIVersion but also mutates the spec.
		// The bypass must not fire, so full pool validation still rejects it.
		newTenant := oldTenant.DeepCopy()
		newTenant.OwnerReferences[0].APIVersion = "crdb.cockroachlabs.com/v1beta1"
		newTenant.Spec.Pods = 1

		resp := invokeWithOld(newTenant, oldTenant, admissionv1.Update)
		require.False(t, resp.Allowed, "ownerRef change bundled with a spec change should be rejected")
		require.Equal(t, "tenant pool GONE does not exist", resp.Result.Message)
	})

	t.Run("cluster with no ExternalCertificates when TLSEnabled is true", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: true,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: string(provider),
						Code:          region,
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
						StartFlags: &crdbv1beta1.Flags{
							Upsert: []string{"--max-offset=200ms"},
						},
					},
				},
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Equal(t, "one of selfSigned or externalCertificates must be provided when tlsEnabled is true", resp.Result.Message)
	})

	t.Run("cluster with ExternalCertificates set when TLSEnabled is false", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: string(provider),
						Code:          region,
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
						Certificates: crdbv1beta1.Certificates{
							ExternalCertificates: &crdbv1beta1.ExternalCertificates{
								NodeCAConfigMapName:     "node-ca",
								ClientCAConfigMapName:   "client-ca",
								NodeSecretName:          "node-certs",
								NodeClientSecretName:    "node-client-certs",
								RootSQLClientSecretName: "client-certs",
								HTTPSecretName:          "ui-certs",
							},
						},
					},
				},
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Equal(t, "certificates must not be configured when tlsEnabled is false", resp.Result.Message)
	})

	t.Run("cluster with selfSigned and tlsEnabled is valid", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: true,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: string(provider),
						Code:          region,
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
						Certificates: crdbv1beta1.Certificates{
							SelfSigned: true,
						},
					},
				},
			},
		}, admissionv1.Create)
		require.True(t, resp.Allowed)
	})

	t.Run("cluster with selfSigned and externalCertificates both set is invalid", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: true,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: string(provider),
						Code:          region,
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
						Certificates: crdbv1beta1.Certificates{
							SelfSigned: true,
							ExternalCertificates: &crdbv1beta1.ExternalCertificates{
								NodeCAConfigMapName:     "node-ca",
								ClientCAConfigMapName:   "client-ca",
								NodeSecretName:          "node-certs",
								NodeClientSecretName:    "node-client-certs",
								RootSQLClientSecretName: "client-certs",
								HTTPSecretName:          "ui-certs",
							},
						},
					},
				},
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Equal(t, "cannot set both selfSigned and externalCertificates", resp.Result.Message)
	})

	t.Run("cluster with selfSigned set when TLSEnabled is false is invalid", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: string(provider),
						Code:          region,
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
						Certificates: crdbv1beta1.Certificates{
							SelfSigned: true,
						},
					},
				},
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Equal(t, "certificates must not be configured when tlsEnabled is false", resp.Result.Message)
	})

	t.Run("cluster with both Flags and StartFlags set", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: string(provider),
						Code:          region,
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
						Flags: map[string]string{
							"--max-offset": "100ms",
						},
						StartFlags: &crdbv1beta1.Flags{
							Upsert: []string{"--max-offset=200ms"},
						},
					},
				},
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Equal(t, "cannot set both Flags and StartFlags; use StartFlags only as Flags is deprecated", resp.Result.Message)
	})

	t.Run("cluster with no DataStore specified", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: string(provider),
						Code:          region,
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						// DataStore is not specified
					},
				},
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Equal(t, "exactly one of VolumeClaimTemplate, VolumeSource, or Basalt must be specified", resp.Result.Message)
	})

	t.Run("cluster WAL failover can be added during create", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: true,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: string(provider),
						Code:          region,
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
						Certificates: crdbv1beta1.Certificates{
							ExternalCertificates: &crdbv1beta1.ExternalCertificates{
								NodeCAConfigMapName:     "node-ca",
								ClientCAConfigMapName:   "client-ca",
								NodeSecretName:          "node-certs",
								NodeClientSecretName:    "node-client-certs",
								RootSQLClientSecretName: "client-certs",
								HTTPSecretName:          "ui-certs",
							},
						},
						WALFailoverSpec: &crdbv1beta1.CrdbWalFailoverSpec{
							Name:             "test-wal",
							Size:             "10Gi",
							StorageClassName: "fast-ssd",
							Status:           crdbv1beta1.WalEnable,
							Path:             "/custom/wal/path",
						},
					},
				},
			},
		}, admissionv1.Create)
		require.True(t, resp.Allowed)
	})

	t.Run("encryption at rest with OldPlatform but no OldKeySecretName is rejected", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: string(provider),
						Code:          region,
						EncryptionAtRest: &crdbv1beta1.EncryptionAtRest{
							Platform:    crdbv1beta1.EncryptionPlatformAwsKms,
							OldPlatform: crdbv1beta1.EncryptionPlatformGcpCloudKms,
						},
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
					},
				},
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Equal(t, "OldPlatform must not be set when OldKeySecretName is nil", resp.Result.Message)
	})

	t.Run("encryption at rest with old CMEK but missing credentials is rejected", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: string(provider),
						Code:          region,
						EncryptionAtRest: &crdbv1beta1.EncryptionAtRest{
							Platform:         crdbv1beta1.EncryptionPlatformAwsKms,
							OldKeySecretName: new("old-key"),
							OldPlatform:      crdbv1beta1.EncryptionPlatformGcpCloudKms,
						},
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
					},
				},
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Contains(t, resp.Result.Message, "OldCMEKCredentialsSecretName is required when switching providers")
	})

	t.Run("encryption at rest with valid old key config is admitted", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: string(provider),
						Code:          region,
						EncryptionAtRest: &crdbv1beta1.EncryptionAtRest{
							Platform:                     crdbv1beta1.EncryptionPlatformAwsKms,
							CMEKCredentialsSecretName:    new("creds"),
							OldKeySecretName:             new("old-key"),
							OldPlatform:                  crdbv1beta1.EncryptionPlatformAwsKms,
							OldCMEKCredentialsSecretName: new("old-creds"),
						},
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
					},
				},
			},
		}, admissionv1.Create)
		require.True(t, resp.Allowed)
	})

	t.Run("same-provider key rotation uses fallback and is admitted", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: string(provider),
						Code:          region,
						EncryptionAtRest: &crdbv1beta1.EncryptionAtRest{
							Platform:                  crdbv1beta1.EncryptionPlatformAwsKms,
							CMEKCredentialsSecretName: new("creds"),
							OldKeySecretName:          new("old-key"),
						},
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
					},
				},
			},
		}, admissionv1.Create)
		require.True(t, resp.Allowed)
	})

	t.Run("cross-provider rotation without explicit old credentials is rejected", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: string(provider),
						Code:          region,
						EncryptionAtRest: &crdbv1beta1.EncryptionAtRest{
							Platform:                  crdbv1beta1.EncryptionPlatformGcpCloudKms,
							CMEKCredentialsSecretName: new("gcp-creds"),
							OldKeySecretName:          new("old-key"),
							OldPlatform:               crdbv1beta1.EncryptionPlatformAwsKms,
						},
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
					},
				},
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Contains(t, resp.Result.Message, "OldCMEKCredentialsSecretName is required when switching providers")
	})

	t.Run("cross-provider rotation with explicit old credentials is admitted", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: string(provider),
						Code:          region,
						EncryptionAtRest: &crdbv1beta1.EncryptionAtRest{
							Platform:                     crdbv1beta1.EncryptionPlatformGcpCloudKms,
							CMEKCredentialsSecretName:    new("gcp-creds"),
							OldKeySecretName:             new("old-key"),
							OldPlatform:                  crdbv1beta1.EncryptionPlatformAwsKms,
							OldCMEKCredentialsSecretName: new("aws-creds"),
						},
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
					},
				},
			},
		}, admissionv1.Create)
		require.True(t, resp.Allowed)
	})

	t.Run("cross-provider Azure to AWS rotation is admitted without OldCMEKCredentialsSecretName", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: string(provider),
						Code:          region,
						EncryptionAtRest: &crdbv1beta1.EncryptionAtRest{
							Platform:                  crdbv1beta1.EncryptionPlatformAwsKms,
							CMEKCredentialsSecretName: new("aws-creds"),
							OldKeySecretName:          new("old-key"),
							OldPlatform:               crdbv1beta1.EncryptionPlatformAzureKeyVault,
						},
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
					},
				},
			},
		}, admissionv1.Create)
		require.True(t, resp.Allowed)
	})

	t.Run("old Azure with OldCMEKCredentialsSecretName is rejected", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: string(provider),
						Code:          region,
						EncryptionAtRest: &crdbv1beta1.EncryptionAtRest{
							Platform:                     crdbv1beta1.EncryptionPlatformAzureKeyVault,
							OldKeySecretName:             new("old-key"),
							OldPlatform:                  crdbv1beta1.EncryptionPlatformAzureKeyVault,
							OldCMEKCredentialsSecretName: new("old-creds"),
						},
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
					},
				},
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Contains(t, resp.Result.Message, "OldCMEKCredentialsSecretName must not be set when OldPlatform is Azure Key Vault")
	})

	t.Run("loggingConfigMapName references non-existent ConfigMap", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: string(provider),
						Code:          region,
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
						LoggingConfigMapName: "non-existent-cm",
					},
				},
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Contains(t, resp.Result.Message, `logging ConfigMap "non-existent-cm" referenced by loggingConfigMapName not found`)
	})

	t.Run("logsStore with loggingConfigMapName requires logsMountPath", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{Namespace: "multi-tenant-ns"},
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: string(provider),
						Code:          region,
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
						LogsStore: &crdbv1beta1.CrdbLogsStoreSpec{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
							// MountPath intentionally not set
						},
						LoggingConfigMapName: "test-config-map",
					},
				},
			},
		}, admissionv1.Create)
		require.False(t, resp.Allowed)
		require.Contains(t, resp.Result.Message, "logsStore.mountPath must be set when both logsStore and loggingConfigMapName are configured")
	})

	t.Run("logsStore with loggingConfigMapName and mountPath is admitted", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{Namespace: "multi-tenant-ns"},
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: string(provider),
						Code:          region,
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
						LogsStore: &crdbv1beta1.CrdbLogsStoreSpec{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
							MountPath:           "/cockroach/custom-logs",
						},
						LoggingConfigMapName: "test-config-map",
					},
				},
			},
		}, admissionv1.Create)
		require.True(t, resp.Allowed)
	})

	t.Run("logsStore without loggingConfigMapName is admitted", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: string(provider),
						Code:          region,
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
						LogsStore: &crdbv1beta1.CrdbLogsStoreSpec{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
						// No loggingConfigMapName, so logsMountPath not required
					},
				},
			},
		}, admissionv1.Create)
		require.True(t, resp.Allowed)
	})

	t.Run("delete invalid tenant", func(t *testing.T) {
		resp := invokeHandler(&crdbv1beta1.CrdbTenant{
			ObjectMeta: metav1.ObjectMeta{},
			Spec:       crdbv1beta1.CrdbTenantSpec{},
		}, admissionv1.Delete)
		require.True(t, resp.Allowed)
	})

	t.Run("removing logsStore while loggingConfigMapName is set is rejected", func(t *testing.T) {
		oldCluster := &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{Namespace: "multi-tenant-ns"},
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{CloudProvider: string(provider), Code: region},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
						LogsStore: &crdbv1beta1.CrdbLogsStoreSpec{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
							MountPath:           "/cockroach/custom-logs",
						},
						LoggingConfigMapName: "test-config-map",
					},
				},
			},
		}
		newCluster := oldCluster.DeepCopy()
		newCluster.Spec.Template.Spec.LogsStore = nil
		oldRaw, err := json.Marshal(oldCluster)
		require.NoError(t, err)
		newRaw, err := json.Marshal(newCluster)
		require.NoError(t, err)
		resp := handler.Handle(context.Background(), admission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				Kind:      metav1.GroupVersionKind{Group: crdbv1beta1.GroupVersion.Group, Version: crdbv1beta1.GroupVersion.Version, Kind: "CrdbCluster"},
				Namespace: "multi-tenant-ns",
				Operation: admissionv1.Update,
				Object:    runtime.RawExtension{Raw: newRaw},
				OldObject: runtime.RawExtension{Raw: oldRaw},
			},
		})
		require.False(t, resp.Allowed)
		require.Contains(t, resp.Result.Message, "cannot remove logsStore while loggingConfigMapName")
	})

	t.Run("removing logsStore and loggingConfigMapName together is admitted", func(t *testing.T) {
		oldCluster := &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{Namespace: "multi-tenant-ns"},
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{CloudProvider: string(provider), Code: region},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
						LogsStore: &crdbv1beta1.CrdbLogsStoreSpec{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
							MountPath:           "/cockroach/custom-logs",
						},
						LoggingConfigMapName: "test-config-map",
					},
				},
			},
		}
		newCluster := oldCluster.DeepCopy()
		newCluster.Spec.Template.Spec.LogsStore = nil
		newCluster.Spec.Template.Spec.LoggingConfigMapName = ""
		oldRaw, err := json.Marshal(oldCluster)
		require.NoError(t, err)
		newRaw, err := json.Marshal(newCluster)
		require.NoError(t, err)
		resp := handler.Handle(context.Background(), admission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				Kind:      metav1.GroupVersionKind{Group: crdbv1beta1.GroupVersion.Group, Version: crdbv1beta1.GroupVersion.Version, Kind: "CrdbCluster"},
				Namespace: "multi-tenant-ns",
				Operation: admissionv1.Update,
				Object:    runtime.RawExtension{Raw: newRaw},
				OldObject: runtime.RawExtension{Raw: oldRaw},
			},
		})
		require.True(t, resp.Allowed)
	})

	t.Run("removing logsStore when loggingConfigMapName was never set is admitted", func(t *testing.T) {
		oldCluster := &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{Namespace: "multi-tenant-ns"},
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: false,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{CloudProvider: string(provider), Code: region},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
						LogsStore: &crdbv1beta1.CrdbLogsStoreSpec{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
					},
				},
			},
		}
		newCluster := oldCluster.DeepCopy()
		newCluster.Spec.Template.Spec.LogsStore = nil
		oldRaw, err := json.Marshal(oldCluster)
		require.NoError(t, err)
		newRaw, err := json.Marshal(newCluster)
		require.NoError(t, err)
		resp := handler.Handle(context.Background(), admission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				Kind:      metav1.GroupVersionKind{Group: crdbv1beta1.GroupVersion.Group, Version: crdbv1beta1.GroupVersion.Version, Kind: "CrdbCluster"},
				Namespace: "multi-tenant-ns",
				Operation: admissionv1.Update,
				Object:    runtime.RawExtension{Raw: newRaw},
				OldObject: runtime.RawExtension{Raw: oldRaw},
			},
		})
		require.True(t, resp.Allowed)
	})

	t.Run("update from Disabled to MutableOnly is rejected with mismatched region", func(t *testing.T) {
		oldCluster := &crdbv1beta1.CrdbCluster{
			Spec: crdbv1beta1.CrdbClusterSpec{
				Mode:       new(crdbv1beta1.Disabled),
				TLSEnabled: true,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{CloudProvider: "gcp", Code: "us-central1"},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
						Certificates: crdbv1beta1.Certificates{
							ExternalCertificates: &crdbv1beta1.ExternalCertificates{
								NodeSecretName:          "node-certs",
								RootSQLClientSecretName: "client-certs",
							},
						},
					},
				},
			},
		}
		newCluster := oldCluster.DeepCopy()
		newCluster.Spec.Mode = new(crdbv1beta1.MutableOnly)

		oldRaw, err := json.Marshal(oldCluster)
		require.NoError(t, err)
		newRaw, err := json.Marshal(newCluster)
		require.NoError(t, err)

		resp := handler.Handle(context.Background(), admission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				Kind:      metav1.GroupVersionKind{Group: crdbv1beta1.GroupVersion.Group, Version: crdbv1beta1.GroupVersion.Version, Kind: "CrdbCluster"},
				Operation: admissionv1.Update,
				Object:    runtime.RawExtension{Raw: newRaw},
				OldObject: runtime.RawExtension{Raw: oldRaw},
			},
		})
		require.False(t, resp.Allowed, "Disabled→MutableOnly with mismatched region should be rejected so migration cannot complete until spec is valid")
	})
}

func TestBetaWebhookHandler_Handle(t *testing.T) {
	ctl := kubetest.NewFakeCtl(kubetest.Options{})
	region := "us-east1"
	provider := cloud.GCP
	handler := manager.NewWebhook(ctl, cloud.Environment{
		CloudRegion:   region,
		CloudProvider: provider,
	})

	invokeHandler := makeInvokeHandler(t, handler)

	tc := []struct {
		regions []crdbv1beta1.CrdbClusterRegion
		comment string
		allowed bool
	}{
		{
			regions: []crdbv1beta1.CrdbClusterRegion{
				// Empty region.
			},
			comment: "empty regions list",
			allowed: false,
		},
		{
			regions: []crdbv1beta1.CrdbClusterRegion{
				{
					CloudProvider: string(provider),
					Code:          region,
				},
			},
			allowed: true,
		},
		{
			regions: []crdbv1beta1.CrdbClusterRegion{
				{
					CloudProvider: "",
					Code:          "",
				},
			},
			comment: "unspecified region struct",
			allowed: false,
		},
		{
			regions: []crdbv1beta1.CrdbClusterRegion{
				{
					CloudProvider: string(provider),
					Code:          region,
				},
				{
					CloudProvider: string(provider),
					Code:          "us-east2",
				},
			},
			comment: "correct multi-region",
			allowed: true,
		},
		{
			regions: []crdbv1beta1.CrdbClusterRegion{
				{
					CloudProvider: string(provider),
					Code:          "us-east3",
				},
				{
					CloudProvider: string(provider),
					Code:          "us-east2",
				},
			},
			comment: "incorrect multi-region",
			allowed: false,
		},
	}

	for i := range tc {
		t.Run(tc[i].comment, func(t *testing.T) {
			resp := invokeHandler(&crdbv1beta1.CrdbCluster{
				ObjectMeta: metav1.ObjectMeta{},
				Spec: crdbv1beta1.CrdbClusterSpec{
					Regions: tc[i].regions,
					Template: crdbv1beta1.CrdbNodeTemplate{
						Spec: crdbv1beta1.CrdbNodeSpec{
							DataStore: crdbv1beta1.DataStore{
								VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
							},
							StartFlags: &crdbv1beta1.Flags{
								Upsert: []string{"--max-offset=200ms"},
							},
						},
					},
				},
			}, admissionv1.Create)
			require.Equal(t, tc[i].allowed, resp.Allowed)
		})
	}
}

// makeInvokeHandler creates a generic handler function that works for both validating and mutating webhooks.
// admissionMessage returns the rejection reason from a response, or the empty
// string when the webhook set no Result. Reading it directly would panic on an
// admitted response.
func admissionMessage(resp admission.Response) string {
	if resp.Result == nil {
		return ""
	}
	return resp.Result.Message
}

func makeInvokeHandler(
	t *testing.T, handler interface {
		admission.Handler
		admission.DecoderInjector
	},
) func(object runtime.Object, op admissionv1.Operation) admission.Response {
	ctx := context.Background()
	decoder, err := admission.NewDecoder(crdbv1beta1.Scheme)
	require.NoError(t, err)
	require.NoError(t, handler.InjectDecoder(decoder))
	return func(object runtime.Object, op admissionv1.Operation) admission.Response {
		gvk := metav1.GroupVersionKind{
			Group:   crdbv1beta1.GroupVersion.Group,
			Version: crdbv1beta1.GroupVersion.Version,
			Kind:    reflect.TypeOf(object).Elem().Name(),
		}

		key := client.ObjectKeyFromObject(object.(kube.Object))

		raw, err := json.Marshal(object)
		require.NoError(t, err)

		return handler.Handle(ctx, admission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				Kind:      gvk,
				Name:      key.Name,
				Namespace: key.Namespace,
				Operation: op,
				Object: runtime.RawExtension{
					Raw: raw,
				},
			},
		})
	}
}

// makeInvokeHandlerWithOldObject is like makeInvokeHandler but includes
// OldObject in the admission request, needed for testing UPDATE logic
// that compares old and new objects.
func makeInvokeHandlerWithOldObject(
	t *testing.T, handler interface {
		admission.Handler
		admission.DecoderInjector
	},
) func(newObj, oldObj runtime.Object, op admissionv1.Operation) admission.Response {
	ctx := context.Background()
	decoder, err := admission.NewDecoder(crdbv1beta1.Scheme)
	require.NoError(t, err)
	require.NoError(t, handler.InjectDecoder(decoder))
	return func(newObj, oldObj runtime.Object, op admissionv1.Operation) admission.Response {
		gvk := metav1.GroupVersionKind{
			Group:   crdbv1beta1.GroupVersion.Group,
			Version: crdbv1beta1.GroupVersion.Version,
			Kind:    reflect.TypeOf(newObj).Elem().Name(),
		}
		key := client.ObjectKeyFromObject(newObj.(kube.Object))
		newRaw, err := json.Marshal(newObj)
		require.NoError(t, err)
		oldRaw, err := json.Marshal(oldObj)
		require.NoError(t, err)

		return handler.Handle(ctx, admission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				Kind:      gvk,
				Name:      key.Name,
				Namespace: key.Namespace,
				Operation: op,
				Object:    runtime.RawExtension{Raw: newRaw},
				OldObject: runtime.RawExtension{Raw: oldRaw},
			},
		})
	}
}
