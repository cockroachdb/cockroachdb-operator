package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

func TestManager_readLocalityForNode(t *testing.T) {
	ctx := context.Background()

	awsNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "aws-node",
			Labels: map[string]string{
				"failure-domain.beta.kubernetes.io/region": "us-east-1",
				"failure-domain.beta.kubernetes.io/zone":   "us-east-1-a",
			},
		},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{
				KubeletVersion: "eks",
			},
		},
	}
	gcpNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gcp-node",
			Labels: map[string]string{
				corev1.LabelTopologyRegion: "us-central1",
				corev1.LabelTopologyZone:   "us-central1-d",
			},
		},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{
				KubeletVersion: "gke",
			},
		},
	}
	awsLocality := cloud.LocalityInfo{Region: "aws-us-east-1", Zone: "aws-us-east-1-a"}
	gcpLocality := cloud.LocalityInfo{Region: "gcp-us-central1", Zone: "gcp-us-central1-d"}
	ctl := kubetest.NewFakeCtl(kubetest.Options{Options: kube.Options{BeforeMutation: kubetest.MakeReady}})
	require.NoError(t, ctl.Apply(ctx, awsNode, gcpNode))
	mgr := &Manager{ctl: ctl}

	assertNodeLocalitiesSize := func(size int) {
		count := 0
		mgr.nodeLocalities.Range(func(_ any, _ any) bool {
			count++
			return true
		})
		require.Equal(t, size, count)
	}

	assertNodeLocalitiesSize(0)

	_, err := mgr.readLocalityForNode(ctx, "unknown-node")
	require.EqualError(t, err, `nodes "unknown-node" not found`)

	assertNodeLocalitiesSize(0)

	// Reading twice should not affect the size of cache.
	for range 2 {
		loc, err := mgr.readLocalityForNode(ctx, awsNode.Name)
		require.NoError(t, err)
		require.Equal(t, awsLocality, loc)

		assertNodeLocalitiesSize(1)
	}

	// Now read another node.
	for range 2 {
		loc, err := mgr.readLocalityForNode(ctx, gcpNode.Name)
		require.NoError(t, err)
		require.Equal(t, gcpLocality, loc)

		assertNodeLocalitiesSize(2)
	}

	// Read back the original node, and the data should still be there.
	loc, err := mgr.readLocalityForNode(ctx, awsNode.Name)
	require.NoError(t, err)
	require.Equal(t, awsLocality, loc)

	assertNodeLocalitiesSize(2)
}

func TestManager_NamespaceScoping(t *testing.T) {
	// Test that Manager correctly configures cache for different namespace scoping modes.
	restCfg := &rest.Config{}

	t.Run("GlobalMode", func(t *testing.T) {
		// Empty watchNamespaces = global mode (watch all namespaces).
		mgr := New(restCfg, "operator-ns", nil)

		// In global mode, the Namespace field should be empty and NewCache should be nil.
		require.Empty(t, mgr.Options.Namespace, "global mode should not set Namespace")
		require.Nil(t, mgr.Options.NewCache, "global mode should not set NewCache")
		require.Empty(t, mgr.watchNamespaces, "watchNamespaces should be empty in global mode")
	})

	t.Run("SingleNamespaceMode", func(t *testing.T) {
		// Single namespace mode uses the built-in Namespace field.
		mgr := New(restCfg, "operator-ns", []string{"prod-a"})

		require.Equal(t, "prod-a", mgr.Options.Namespace, "single-namespace mode should set Namespace")
		require.Nil(t, mgr.Options.NewCache, "single-namespace mode should not set NewCache")
		require.Equal(t, []string{"prod-a"}, mgr.watchNamespaces)
	})

	t.Run("MultiNamespaceMode", func(t *testing.T) {
		// Multi-namespace mode uses MultiNamespacedCacheBuilder.
		mgr := New(restCfg, "operator-ns", []string{"prod-a", "prod-b", "staging"})

		require.Empty(t, mgr.Options.Namespace, "multi-namespace mode should not set Namespace")
		require.NotNil(t, mgr.Options.NewCache, "multi-namespace mode should set NewCache")
		require.Equal(t, []string{"prod-a", "prod-b", "staging"}, mgr.watchNamespaces)
	})

	t.Run("ExplicitEmptySlice", func(t *testing.T) {
		// Empty slice should behave the same as global mode.
		mgr := New(restCfg, "operator-ns", []string{})

		require.Empty(t, mgr.Options.Namespace, "empty slice should behave like global mode")
		require.Nil(t, mgr.Options.NewCache, "empty slice should behave like global mode")
		require.Empty(t, mgr.watchNamespaces)
	})
}

func TestManager_isRelevantCertEvent(t *testing.T) {
	tests := []struct {
		name     string
		op       fsnotify.Op
		expected bool
	}{
		{
			name:     "WriteEventRelevant",
			op:       fsnotify.Write,
			expected: true,
		},
		{
			name:     "CreateEventRelevant",
			op:       fsnotify.Create,
			expected: true,
		},
		{
			name:     "WriteAndCreateEventRelevant",
			op:       fsnotify.Write | fsnotify.Create,
			expected: true,
		},
		{
			name:     "RemoveEventRelevant",
			op:       fsnotify.Remove,
			expected: true,
		},
		{
			name:     "RenameEventRelevant",
			op:       fsnotify.Rename,
			expected: true,
		},
		{
			name:     "ChmodEventNotRelevant",
			op:       fsnotify.Chmod,
			expected: false,
		},
	}

	mgr := &Manager{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := fsnotify.Event{
				Name: "/certs/ca.crt",
				Op:   tt.op,
			}
			result := mgr.isRelevantCertEvent(event)
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestManager_setupCertWatcher(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		// Create a temporary directory to watch
		tmpDir := t.TempDir()

		mgr := &Manager{}
		mgr.Options.CertDir = tmpDir

		watcher, err := mgr.setupCertWatcher()
		require.NoError(t, err, "setupCertWatcher should succeed with valid directory")
		require.NotNil(t, watcher, "watcher should not be nil")

		// Verify the directory is being watched
		watchList := watcher.WatchList()
		require.Contains(t, watchList, tmpDir, "certDir should be in watch list")

		// Clean up
		require.NoError(t, watcher.Close())
	})

	t.Run("NonExistentDirectory", func(t *testing.T) {
		mgr := &Manager{}
		mgr.Options.CertDir = "/nonexistent/path/to/certs"

		watcher, err := mgr.setupCertWatcher()
		require.Error(t, err, "setupCertWatcher should fail with non-existent directory")
		require.Nil(t, watcher, "watcher should be nil on error")
		require.Contains(t, err.Error(), "failed to watch cert directory", "error should mention cert directory")
	})
}

func TestManager_registerWebhooksWithCtl(t *testing.T) {
	ctx := context.Background()

	// Helper to create operator deployment with app label for detectOperatorAppLabel
	createOperatorDeployment := func(ctl *kube.Ctl, namespace string) {
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      manifests.OperatorName,
				Namespace: namespace,
			},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app": manifests.OperatorAppLabel,
						},
					},
				},
			},
		}
		_ = ctl.Create(ctx, deployment)
	}

	t.Run("ReadsCAFromFilesystem", func(t *testing.T) {
		tmpDir := t.TempDir()
		caCertPath := filepath.Join(tmpDir, "ca.crt")
		expectedCAContent := []byte("test-ca-certificate-content")

		// Write CA cert to filesystem
		err := os.WriteFile(caCertPath, expectedCAContent, 0644)
		require.NoError(t, err)

		mgr := &Manager{
			namespace: "test-ns",
		}
		mgr.Options.CertDir = tmpDir

		// Create a fake kube.Ctl
		ctl := kubetest.NewFakeCtl(kubetest.Options{Options: kube.Options{BeforeMutation: kubetest.MakeReady}})
		createOperatorDeployment(ctl, "test-ns")

		// Call registerWebhooksWithCtl
		_, err = mgr.registerWebhooksWithCtl(ctx, ctl)
		require.NoError(t, err)

		// Verify the webhook was created with the CA from filesystem
		validatingWebhook := &admissionregistrationv1.ValidatingWebhookConfiguration{}
		err = ctl.Get(ctx, "cockroach-webhook-config", validatingWebhook)
		require.NoError(t, err)
		require.NotEmpty(t, validatingWebhook.Webhooks)
		require.Equal(t, expectedCAContent, validatingWebhook.Webhooks[0].ClientConfig.CABundle,
			"CA bundle should match content from filesystem")
	})

	t.Run("FallsBackToSecretWhenFileNotFound", func(t *testing.T) {
		tmpDir := t.TempDir()
		// Don't create ca.crt - it should fall back to secret

		mgr := &Manager{
			namespace: "test-ns",
		}
		mgr.Options.CertDir = tmpDir

		// Create a fake kube.Ctl with a secret containing the CA cert
		ctl := kubetest.NewFakeCtl(kubetest.Options{Options: kube.Options{BeforeMutation: kubetest.MakeReady}})
		createOperatorDeployment(ctl, "test-ns")

		secretCAContent := []byte("secret-ca-certificate-content")
		tlsSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      manifests.OperatorCertSecretName,
				Namespace: "test-ns",
			},
			Data: map[string][]byte{
				"ca.crt": secretCAContent,
			},
		}
		err := ctl.Create(ctx, tlsSecret)
		require.NoError(t, err)

		// Call registerWebhooksWithCtl - should fall back to secret
		_, err = mgr.registerWebhooksWithCtl(ctx, ctl)
		require.NoError(t, err)

		// Verify the webhook was created with the CA from secret
		validatingWebhook := &admissionregistrationv1.ValidatingWebhookConfiguration{}
		err = ctl.Get(ctx, "cockroach-webhook-config", validatingWebhook)
		require.NoError(t, err)
		require.NotEmpty(t, validatingWebhook.Webhooks)
		require.Equal(t, secretCAContent, validatingWebhook.Webhooks[0].ClientConfig.CABundle,
			"CA bundle should match content from K8s secret when file not found")
	})

	t.Run("ReturnsErrorWhenBothFilesystemAndSecretMissing", func(t *testing.T) {
		tmpDir := t.TempDir()
		// Don't create ca.crt and don't create secret

		mgr := &Manager{
			namespace: "test-ns",
		}
		mgr.Options.CertDir = tmpDir

		// Create a fake kube.Ctl without the secret
		ctl := kubetest.NewFakeCtl(kubetest.Options{Options: kube.Options{BeforeMutation: kubetest.MakeReady}})
		createOperatorDeployment(ctl, "test-ns")

		// Call registerWebhooksWithCtl - should fail
		_, err := mgr.registerWebhooksWithCtl(ctx, ctl)
		require.Error(t, err)
		require.Contains(t, err.Error(), "error getting operator TLS secret",
			"should return error when both filesystem and secret are missing")
	})
}

func TestManager_handleCertChangeUpdatesWebhookCABundles(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	newCA := []byte("new-ca")
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "ca.crt"), newCA, 0644))

	clusterCRD := makeCRD()
	require.NoError(t, configureCRDConversion(ctx, clusterCRD, makeCRD(), true, true, []byte("old-ca"), "operator-ns"))

	ctl := kubetest.NewFakeCtl(kubetest.Options{Options: kube.Options{BeforeMutation: kubetest.MakeReady}})
	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true, NoLastAppliedAnnotation: true}, clusterCRD))
	mgr := &Manager{
		EnableMigrationController: true,
		ctl:                       ctl,
		namespace:                 "operator-ns",
		v1alpha1InStorage:         true,
	}
	mgr.Options.CertDir = tmpDir

	mgr.handleCertChange(ctx)

	validatingWebhook := &admissionregistrationv1.ValidatingWebhookConfiguration{}
	require.NoError(t, ctl.WithNamespace("operator-ns").Get(ctx, manifests.ValidatingWebhookConfigNameGlobal, validatingWebhook))
	require.NotEmpty(t, validatingWebhook.Webhooks)
	require.Equal(t, newCA, validatingWebhook.Webhooks[0].ClientConfig.CABundle)
	mutatingWebhook := &admissionregistrationv1.MutatingWebhookConfiguration{}
	require.NoError(t, ctl.WithNamespace("operator-ns").Get(ctx, manifests.MutatingWebhookConfigNameGlobal, mutatingWebhook))
	require.NotEmpty(t, mutatingWebhook.Webhooks)
	require.Equal(t, newCA, mutatingWebhook.Webhooks[0].ClientConfig.CABundle)

	updatedCRD := &apiextv1.CustomResourceDefinition{}
	require.NoError(t, ctl.Get(ctx, clusterCRD.Name, updatedCRD))
	require.Equal(t, newCA, updatedCRD.Spec.Conversion.Webhook.ClientConfig.CABundle)
}

func TestManager_syncCRDConversionCABundleDoesNotUpdateAnotherOperator(t *testing.T) {
	ctx := context.Background()
	clusterCRD := makeCRD()
	require.NoError(t, configureCRDConversion(ctx, clusterCRD, makeCRD(), true, true, []byte("other-ca"), "other-ns"))

	ctl := kubetest.NewFakeCtl(kubetest.Options{})
	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true, NoLastAppliedAnnotation: true}, clusterCRD))
	mgr := &Manager{
		EnableMigrationController: true,
		ctl:                       ctl,
		namespace:                 "operator-ns",
		v1alpha1InStorage:         true,
	}

	require.NoError(t, mgr.syncCRDConversionCABundle(ctx, []byte("new-ca")))

	updatedCRD := &apiextv1.CustomResourceDefinition{}
	require.NoError(t, ctl.Get(ctx, clusterCRD.Name, updatedCRD))
	require.Equal(t, []byte("other-ca"), updatedCRD.Spec.Conversion.Webhook.ClientConfig.CABundle)
}

// certWatcherTestSetup encapsulates common test setup for cert watcher tests.
type certWatcherTestSetup struct {
	t                 *testing.T
	tmpDir            string
	testFile          string
	mgr               *Manager
	watcher           *fsnotify.Watcher
	certChangeCounter *atomic.Int32
	ctx               context.Context
	cancel            context.CancelFunc
	done              chan struct{}
}

// setupCertWatcherTest creates a Manager with temp directory, cert file, and watcher.
func setupCertWatcherTest(t *testing.T, debounceDelay time.Duration) *certWatcherTestSetup {
	t.Helper()

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "ca.crt")

	// Create the file before starting the watcher to avoid Create+Write events.
	// This makes tests only trigger Write events, making them more deterministic.
	err := os.WriteFile(testFile, []byte("initial"), 0644)
	require.NoError(t, err)

	mgr := &Manager{}
	mgr.Options.CertDir = tmpDir
	mgr.certDebounceDelay = debounceDelay

	watcher, err := fsnotify.NewWatcher()
	require.NoError(t, err)
	require.NoError(t, watcher.Add(tmpDir))

	return &certWatcherTestSetup{
		t:        t,
		tmpDir:   tmpDir,
		testFile: testFile,
		mgr:      mgr,
		watcher:  watcher,
	}
}

// setupCertChangeHandler configures the manager's cert change handler with a counter.
func (s *certWatcherTestSetup) setupCertChangeHandler(handlerFn func(ctx context.Context) error) {
	s.t.Helper()

	if handlerFn == nil {
		// Default handler just increments the counter
		counter := &atomic.Int32{}
		s.certChangeCounter = counter
		s.mgr.SetCertChangeHandler(func(ctx context.Context) error {
			counter.Add(1)
			return nil
		})
	} else {
		// Custom handler that also increments the counter
		counter := &atomic.Int32{}
		s.certChangeCounter = counter
		s.mgr.SetCertChangeHandler(func(ctx context.Context) error {
			counter.Add(1)
			return handlerFn(ctx)
		})
	}
}

// startWatcher starts the cert watcher in a goroutine.
func (s *certWatcherTestSetup) startWatcher() {
	s.t.Helper()

	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.done = make(chan struct{})

	go func() {
		s.mgr.runCertWatcher(s.ctx, s.watcher)
		close(s.done)
	}()

	// Give the watcher time to start
	time.Sleep(50 * time.Millisecond)
}

// stopWatcher cancels the context and waits for the watcher to stop.
func (s *certWatcherTestSetup) stopWatcher() {
	s.t.Helper()

	s.cancel()
	<-s.done
}

// writeFile writes content to the test file, triggering a file system event.
func (s *certWatcherTestSetup) writeFile(content string) {
	s.t.Helper()

	err := os.WriteFile(s.testFile, []byte(content), 0644)
	require.NoError(s.t, err)
}

func TestManager_runCertWatcher(t *testing.T) {
	t.Run("ProcessesWriteEvent", func(t *testing.T) {
		s := setupCertWatcherTest(t, 100*time.Millisecond)
		s.setupCertChangeHandler(nil)
		s.startWatcher()
		defer s.stopWatcher()

		// Trigger a write event by modifying the existing file
		s.writeFile("updated")

		// Wait for the debounce timer to expire and handler to be called.
		// With trailing-edge debounce, we need to wait for debounceDelay + some buffer.
		require.Eventually(t, func() bool {
			return s.certChangeCounter.Load() > 0
		}, 500*time.Millisecond, 20*time.Millisecond, "certChangeHandler should be called after debounce timer expires")

		require.Equal(t, int32(1), s.certChangeCounter.Load(), "certChangeHandler should be called once")
	})

	t.Run("StopsOnContextCancellation", func(t *testing.T) {
		s := setupCertWatcherTest(t, 100*time.Millisecond)
		s.startWatcher()

		// Cancel immediately and verify watcher stops
		s.cancel()

		require.Eventually(t, func() bool {
			select {
			case <-s.done:
				return true
			default:
				return false
			}
		}, 1*time.Second, 10*time.Millisecond, "runCertWatcher should stop after context cancellation")
	})

	t.Run("DebouncingPreventsRapidUpdates", func(t *testing.T) {
		s := setupCertWatcherTest(t, 300*time.Millisecond)
		s.setupCertChangeHandler(nil)
		s.startWatcher()
		defer s.stopWatcher()

		// Trigger multiple rapid write events (each event resets the timer)
		for i := range 5 {
			s.writeFile(fmt.Sprintf("test%d", i))
			time.Sleep(50 * time.Millisecond) // Short delay, well within debounce window
		}

		// With trailing-edge debounce, handler should be called AFTER the last event.
		// Wait for debounce delay + buffer after last write.
		require.Eventually(t, func() bool {
			return s.certChangeCounter.Load() == 1
		}, 1*time.Second, 20*time.Millisecond, "handler should be called once after events stop")

		// Verify no additional calls
		time.Sleep(200 * time.Millisecond)
		require.Equal(t, int32(1), s.certChangeCounter.Load(), "trailing-edge debounce should call handler only once after burst")
	})

	t.Run("HandlesErrorsGracefully", func(t *testing.T) {
		s := setupCertWatcherTest(t, 100*time.Millisecond)
		s.setupCertChangeHandler(func(ctx context.Context) error {
			return errors.New("simulated error")
		})
		s.startWatcher()
		defer s.stopWatcher()

		// Trigger event
		s.writeFile("test")

		// With retry logic, the handler will be called multiple times (up to 5 retries).
		// Just verify it gets called at least once (first attempt).
		require.Eventually(t, func() bool {
			return s.certChangeCounter.Load() >= 1
		}, 2*time.Second, 100*time.Millisecond, "handler should be called even if it returns error")

		// Wait for first event's retries to start (but not complete - that would take ~15s).
		// We just want to verify the watcher doesn't crash and can still process new events.
		time.Sleep(500 * time.Millisecond)

		// Trigger a second event while the first is still retrying
		s.writeFile("test2")

		// Verify we got at least one more call (from second event's first attempt).
		// The exact count depends on timing - retries from first event may still be running.
		require.Eventually(t, func() bool {
			return s.certChangeCounter.Load() >= 2
		}, 2*time.Second, 100*time.Millisecond, "watcher should continue processing new events even during retries")
	})

	t.Run("RetriesOnTransientErrors", func(t *testing.T) {
		s := setupCertWatcherTest(t, 100*time.Millisecond)

		// Simulate transient error: fail first 2 attempts, then succeed
		var attemptCount atomic.Int32
		s.setupCertChangeHandler(func(ctx context.Context) error {
			count := attemptCount.Add(1)
			if count <= 2 {
				return errors.New("simulated transient error")
			}
			return nil
		})

		s.startWatcher()
		defer s.stopWatcher()

		// Trigger event
		s.writeFile("test")

		// Should retry and eventually succeed after 3 attempts (2 failures + 1 success).
		// With exponential backoff: 1s + 2s = 3s total wait time for retries.
		// Plus debounce delay of 100ms, so total should be < 5s.
		require.Eventually(t, func() bool {
			// Handler was called 3 times (via attemptCount), but certChangeCounter
			// increments on each call regardless of error
			return s.certChangeCounter.Load() >= 3
		}, 5*time.Second, 100*time.Millisecond, "should retry on transient errors and eventually succeed")

		// Verify we stopped retrying after success
		time.Sleep(500 * time.Millisecond)
		require.Equal(t, int32(3), attemptCount.Load(), "should stop retrying after success")
	})
}

func TestManager_cleanupWebhookConfigs(t *testing.T) {
	ctx := context.Background()
	operatorNS := "operator-ns"

	// webhookService returns a Service matching what the operator applies at startup.
	webhookService := func() *corev1.Service {
		return &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      manifests.WebhookServiceName,
				Namespace: operatorNS,
			},
		}
	}

	t.Run("GlobalMode", func(t *testing.T) {
		// In global mode, webhook configs use the fixed names.
		validating := &admissionregistrationv1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: manifests.ValidatingWebhookConfigNameGlobal},
		}
		mutating := &admissionregistrationv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: manifests.MutatingWebhookConfigNameGlobal},
		}

		ctl := kubetest.NewFakeCtl(kubetest.Options{Options: kube.Options{BeforeMutation: kubetest.MakeReady}})
		require.NoError(t, ctl.Apply(ctx, validating, mutating, webhookService()))

		mgr := &Manager{ctl: ctl, namespace: operatorNS}
		mgr.cleanupWebhookConfigs()

		// All three resources should be gone.
		err := ctl.Get(ctx, manifests.ValidatingWebhookConfigNameGlobal, &admissionregistrationv1.ValidatingWebhookConfiguration{})
		require.Error(t, err, "validating webhook config should be deleted")
		err = ctl.Get(ctx, manifests.MutatingWebhookConfigNameGlobal, &admissionregistrationv1.MutatingWebhookConfiguration{})
		require.Error(t, err, "mutating webhook config should be deleted")
		svc := &corev1.Service{}
		svc.Namespace = operatorNS
		err = ctl.Get(ctx, manifests.WebhookServiceName, svc)
		require.Error(t, err, "webhook service should be deleted")
	})

	t.Run("ScopedMode", func(t *testing.T) {
		// In scoped mode, webhook configs have namespace-suffixed names.
		scopedValidatingName := manifests.GetWebhookConfigName(false, operatorNS, false)
		scopedMutatingName := manifests.GetWebhookConfigName(false, operatorNS, true)

		validating := &admissionregistrationv1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: scopedValidatingName},
		}
		mutating := &admissionregistrationv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: scopedMutatingName},
		}

		ctl := kubetest.NewFakeCtl(kubetest.Options{Options: kube.Options{BeforeMutation: kubetest.MakeReady}})
		require.NoError(t, ctl.Apply(ctx, validating, mutating, webhookService()))

		mgr := &Manager{
			ctl:             ctl,
			namespace:       operatorNS,
			watchNamespaces: []string{"prod-a"},
		}
		mgr.cleanupWebhookConfigs()

		// Scoped-name resources should be deleted.
		err := ctl.Get(ctx, scopedValidatingName, &admissionregistrationv1.ValidatingWebhookConfiguration{})
		require.Error(t, err, "scoped validating webhook config should be deleted")
		err = ctl.Get(ctx, scopedMutatingName, &admissionregistrationv1.MutatingWebhookConfiguration{})
		require.Error(t, err, "scoped mutating webhook config should be deleted")
	})

	t.Run("ScopedMode_DoesNotDeleteGlobalWebhooks", func(t *testing.T) {
		// A scoped operator must not delete the global webhook configs owned by
		// a different global-mode operator.
		globalValidating := &admissionregistrationv1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: manifests.ValidatingWebhookConfigNameGlobal},
		}
		globalMutating := &admissionregistrationv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: manifests.MutatingWebhookConfigNameGlobal},
		}

		ctl := kubetest.NewFakeCtl(kubetest.Options{Options: kube.Options{BeforeMutation: kubetest.MakeReady}})
		require.NoError(t, ctl.Apply(ctx, globalValidating, globalMutating))

		mgr := &Manager{
			ctl:             ctl,
			namespace:       operatorNS,
			watchNamespaces: []string{"prod-a"},
		}
		mgr.cleanupWebhookConfigs()

		// Global webhook configs should still exist because this is a scoped operator.
		err := ctl.Get(ctx, manifests.ValidatingWebhookConfigNameGlobal, &admissionregistrationv1.ValidatingWebhookConfiguration{})
		require.NoError(t, err, "global validating webhook should not be deleted by scoped operator")
		err = ctl.Get(ctx, manifests.MutatingWebhookConfigNameGlobal, &admissionregistrationv1.MutatingWebhookConfiguration{})
		require.NoError(t, err, "global mutating webhook should not be deleted by scoped operator")
	})

	t.Run("AlreadyDeleted", func(t *testing.T) {
		// Cleanup should not error when the resources are already gone.
		ctl := kubetest.NewFakeCtl(kubetest.Options{Options: kube.Options{BeforeMutation: kubetest.MakeReady}})
		mgr := &Manager{ctl: ctl, namespace: operatorNS}

		// Should not panic or error.
		mgr.cleanupWebhookConfigs()
	})

	t.Run("SkipsCleanupDuringRollingRestart", func(t *testing.T) {
		// If the operator Deployment still exists, this is a rolling restart
		// or upgrade, not an uninstall. Cleanup should be skipped entirely.
		validating := &admissionregistrationv1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: manifests.ValidatingWebhookConfigNameGlobal},
		}
		mutating := &admissionregistrationv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: manifests.MutatingWebhookConfigNameGlobal},
		}
		operatorDeployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      manifests.OperatorName,
				Namespace: operatorNS,
			},
		}

		ctl := kubetest.NewFakeCtl(kubetest.Options{Options: kube.Options{BeforeMutation: kubetest.MakeReady}})
		require.NoError(t, ctl.Apply(ctx, validating, mutating, webhookService(), operatorDeployment))

		mgr := &Manager{ctl: ctl, namespace: operatorNS}
		mgr.cleanupWebhookConfigs()

		// All resources should still exist because the Deployment is present.
		err := ctl.Get(ctx, manifests.ValidatingWebhookConfigNameGlobal, &admissionregistrationv1.ValidatingWebhookConfiguration{})
		require.NoError(t, err, "validating webhook should survive rolling restart")
		err = ctl.Get(ctx, manifests.MutatingWebhookConfigNameGlobal, &admissionregistrationv1.MutatingWebhookConfiguration{})
		require.NoError(t, err, "mutating webhook should survive rolling restart")
		svc := &corev1.Service{}
		svc.Namespace = operatorNS
		err = ctl.Get(ctx, manifests.WebhookServiceName, svc)
		require.NoError(t, err, "webhook service should survive rolling restart")
	})
}
