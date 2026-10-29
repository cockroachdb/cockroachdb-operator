package kube

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/json"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

var testScheme *runtime.Scheme

func init() {
	testScheme = runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(testScheme)
}

var nginxDeployment = &appsv1.Deployment{
	ObjectMeta: metav1.ObjectMeta{
		Name: "nginx",
	},
	Spec: appsv1.DeploymentSpec{
		Replicas: proto.Int32(1),
		Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"app": "nginx",
			},
		},
		Template: apiv1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"app": "nginx",
				},
			},
			Spec: apiv1.PodSpec{
				Containers: []apiv1.Container{
					{
						Name:  "nginx",
						Image: "nginx:1.14.2",
					},
				},
			},
		},
	},
}

var tenantCrd = &apiextv1.CustomResourceDefinition{
	ObjectMeta: metav1.ObjectMeta{
		Name: "crdbtenants.crdb.cockroachlabs.com",
	},
	Spec: apiextv1.CustomResourceDefinitionSpec{
		Group: "crdb.cockroachlabs.com",
		Names: apiextv1.CustomResourceDefinitionNames{
			Kind:     "CrdbTenant",
			ListKind: "CrdbTenantList",
			Plural:   "crdbtenants",
			Singular: "crdbtenant",
		},
		Scope: "Namespaced",
		Versions: []apiextv1.CustomResourceDefinitionVersion{
			{
				Name: "v1beta1",
				Schema: &apiextv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextv1.JSONSchemaProps{
						Description: "CrdbTenant is the schema for the crdbtenants API",
					},
				},
			},
		},
	},
}

func TestGeneratePatch(t *testing.T) {
	t.Run("NoChange", func(t *testing.T) {
		current := nginxDeployment.DeepCopy()

		patchType, patchBytes, err := GeneratePatch(current, current, false)
		require.NoError(t, err)
		require.Equal(t, types.StrategicMergePatchType, patchType)
		require.Equal(t, string(emptyPatch), string(patchBytes))
	})

	t.Run("RemoveAnnotations", func(t *testing.T) {
		current := nginxDeployment.DeepCopy()
		modified := nginxDeployment.DeepCopy()

		// Initial config has annotations.
		current.Annotations = map[string]string{
			"foo": "bar",
		}

		// Set the last-applied annotation so we'll do a true 3-way merge.
		require.NoError(t, setLastApplied(current))

		// Clear out our annotations on the new version. Empty or nil works.
		modified.Annotations = map[string]string{}
		require.NoError(t, setLastApplied(modified))

		patchType, patchBytes, err := GeneratePatch(current, modified, false)
		require.NoError(t, err)
		require.Equal(t, types.StrategicMergePatchType, patchType)
		requirePatchEquals(t,
			patchBytes,
			modified,
			`{"metadata":{"annotations":{"foo":null}}}`,
		)
	})

	t.Run("NoAnnotation", func(t *testing.T) {
		current := nginxDeployment.DeepCopy()
		modified := nginxDeployment.DeepCopy()

		// Simulate a patch upgrade with current object having no annotation.
		modified.Spec.Template.Spec.Containers[0].Image = "nginx:1.14.3"
		require.NoError(t, setLastApplied(modified))

		patchType, patchBytes, err := GeneratePatch(current, modified, false)
		require.NoError(t, err)
		require.Equal(t, types.StrategicMergePatchType, patchType)
		requirePatchEquals(t,
			patchBytes,
			modified,
			`{"metadata":{"annotations":{}},"spec":{"template":{"spec":{"$setElementOrder/containers":[{"name":"nginx"}],"containers":[{"image":"nginx:1.14.3","name":"nginx"}]}}}}`,
		)
	})

	t.Run("NoAnnotationService", func(t *testing.T) {
		svc := corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name: "my-svc",
			},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"app": "myapp"},
				Ports: []corev1.ServicePort{
					{Port: 32521, TargetPort: intstr.FromInt(32521)},
				},
			},
		}

		current := svc.DeepCopy()
		// Make a change that would result in a patch actually being generated.
		current.Labels = map[string]string{"foo": "bar"}
		// Emulate this service existing in a cluster by giving it a server set
		// ClusterIP and HealthCheckNodePort. Both are read only fields and
		// should NOT show up in our patch.
		current.Spec.ClusterIP = "127.0.0.1"
		current.Spec.HealthCheckNodePort = 4091

		modified := svc.DeepCopy()
		require.NoError(t, setLastApplied(modified))

		patchType, patchBytes, err := GeneratePatch(current, modified, false)
		require.NoError(t, err)
		require.Equal(t, types.StrategicMergePatchType, patchType)
		requirePatchEquals(t,
			patchBytes,
			modified,
			`{"metadata":{"annotations":{},"labels":null},"spec":{}}`,
		)
	})

	t.Run("PVCNoVolumeName", func(t *testing.T) {
		pvc := corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name: "my-pvc",
			},
			Spec: corev1.PersistentVolumeClaimSpec{},
		}

		current := pvc.DeepCopy()
		// Make a change that would result in a patch actually being generated.
		current.Labels = map[string]string{"foo": "bar"}
		// Emulate this PVC existing in a cluster by giving it a server-set
		// VolumeName. This field should NOT show up in our patch.
		current.Spec.VolumeName = "its-a-snake"

		modified := pvc.DeepCopy()
		require.NoError(t, setLastApplied(modified))

		patchType, patchBytes, err := GeneratePatch(current, modified, false)
		require.NoError(t, err)
		require.Equal(t, types.StrategicMergePatchType, patchType)
		requirePatchEquals(t,
			patchBytes,
			modified,
			`{"metadata":{"annotations":{},"labels":null},"spec":{}}`,
		)
	})

	t.Run("PVCWithVolumeName", func(t *testing.T) {
		pvc := corev1.PersistentVolumeClaim{}

		current := pvc.DeepCopy()

		modified := pvc.DeepCopy()
		modified.ObjectMeta.Name = "badgerbadgerbadger"
		modified.Spec.VolumeName = "mushroommushroom"
		require.NoError(t, setLastApplied(modified))

		patchType, patchBytes, err := GeneratePatch(current, modified, false)
		require.NoError(t, err)
		require.Equal(t, types.StrategicMergePatchType, patchType)
		requirePatchEquals(t,
			patchBytes,
			modified,
			`{"metadata":{"annotations":{},"name":"badgerbadgerbadger"},"spec":{"volumeName":"mushroommushroom"}}`,
		)
	})

	t.Run("WithLastApplied", func(t *testing.T) {
		current := nginxDeployment.DeepCopy()
		modified := nginxDeployment.DeepCopy()

		// Simulate a patch upgrade, but with the current object having the
		// last-applied annotation.
		modified.Spec.Template.Spec.Containers[0].Image = "nginx:1.14.3"

		require.NoError(t, setLastApplied(current))
		require.NoError(t, setLastApplied(modified))

		patchType, patchBytes, err := GeneratePatch(current, modified, false)
		require.NoError(t, err)
		require.Equal(t, types.StrategicMergePatchType, patchType)
		requirePatchEquals(t,
			patchBytes,
			modified,
			`{"metadata":{"annotations":{}},"spec":{"template":{"spec":{"$setElementOrder/containers":[{"name":"nginx"}],"containers":[{"image":"nginx:1.14.3","name":"nginx"}]}}}}`,
		)
	})

	t.Run("Unstructured", func(t *testing.T) {
		current, err := convertToUnstructured(nginxDeployment.DeepCopy())
		require.NoError(t, err)
		modified, err := convertToUnstructured(nginxDeployment.DeepCopy())
		require.NoError(t, err)

		// Update the name of the deployment.
		err = unstructured.SetNestedField(modified.Object, "new-name", "metadata", "name")
		require.NoError(t, err)

		require.NoError(t, setLastApplied(current))
		require.NoError(t, setLastApplied(modified))

		patchType, patchBytes, err := GeneratePatch(current, modified, false)
		require.NoError(t, err)
		require.Equal(t, types.MergePatchType, patchType)
		requirePatchEquals(t,
			patchBytes,
			modified,
			`{"metadata":{"annotations":{},"name":"new-name"}}`,
		)
	})

	t.Run("WithAnnotations", func(t *testing.T) {
		current := nginxDeployment.DeepCopy()
		modified := nginxDeployment.DeepCopy()

		// Simulate a patch upgrade.
		modified.Spec.Template.Spec.Containers[0].Image = "nginx:1.14.3"

		require.NoError(t, setLastApplied(current))
		require.NoError(t, setLastApplied(modified))

		// Add a second annotation.
		current.Annotations["My"] = "Annotation"

		patchType, patchBytes, err := GeneratePatch(current, modified, false)
		require.NoError(t, err)
		require.Equal(t, types.StrategicMergePatchType, patchType)
		requirePatchEquals(t,
			patchBytes,
			modified,
			`{"metadata":{"annotations":{}},"spec":{"template":{"spec":{"$setElementOrder/containers":[{"name":"nginx"}],"containers":[{"image":"nginx:1.14.3","name":"nginx"}]}}}}`,
		)
	})

	t.Run("WithServerFields", func(t *testing.T) {
		current := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"annotations":{"deployment.kubernetes.io/revision":"1","kubectl.kubernetes.io/last-applied-configuration":"{\"apiVersion\":\"apps/v1\",\"kind\":\"Deployment\",\"metadata\":{\"annotations\":{},\"name\":\"nginx-deployment\",\"namespace\":\"default\"},\"spec\":{\"replicas\":2,\"selector\":{\"matchLabels\":{\"app\":\"nginx\"}},\"template\":{\"metadata\":{\"labels\":{\"app\":\"nginx\"}},\"spec\":{\"containers\":[{\"image\":\"nginx:1.14.5\",\"name\":\"nginx\",\"ports\":[{\"containerPort\":80}]}]}}}}\n"},"creationTimestamp":"2020-07-23T19:57:35Z","generation":1,"managedFields":[{"apiVersion":"apps/v1","fieldsType":"FieldsV1","fieldsV1":{"f:metadata":{"f:annotations":{"f:deployment.kubernetes.io/revision":{}}},"f:status":{"f:conditions":{".":{},"k:{\"type\":\"Available\"}":{".":{},"f:lastTransitionTime":{},"f:lastUpdateTime":{},"f:message":{},"f:reason":{},"f:status":{},"f:type":{}},"k:{\"type\":\"Progressing\"}":{".":{},"f:lastTransitionTime":{},"f:lastUpdateTime":{},"f:message":{},"f:reason":{},"f:status":{},"f:type":{}}},"f:observedGeneration":{},"f:replicas":{},"f:unavailableReplicas":{},"f:updatedReplicas":{}}},"manager":"k3s","operation":"Update","time":"2020-07-23T19:57:35Z"},{"apiVersion":"apps/v1","fieldsType":"FieldsV1","fieldsV1":{"f:metadata":{"f:annotations":{".":{},"f:kubectl.kubernetes.io/last-applied-configuration":{}}},"f:spec":{"f:progressDeadlineSeconds":{},"f:replicas":{},"f:revisionHistoryLimit":{},"f:selector":{"f:matchLabels":{".":{},"f:app":{}}},"f:strategy":{"f:rollingUpdate":{".":{},"f:maxSurge":{},"f:maxUnavailable":{}},"f:type":{}},"f:template":{"f:metadata":{"f:labels":{".":{},"f:app":{}}},"f:spec":{"f:containers":{"k:{\"name\":\"nginx\"}":{".":{},"f:image":{},"f:imagePullPolicy":{},"f:name":{},"f:ports":{".":{},"k:{\"containerPort\":80,\"protocol\":\"TCP\"}":{".":{},"f:containerPort":{},"f:protocol":{}}},"f:resources":{},"f:terminationMessagePath":{},"f:terminationMessagePolicy":{}}},"f:dnsPolicy":{},"f:restartPolicy":{},"f:schedulerName":{},"f:securityContext":{},"f:terminationGracePeriodSeconds":{}}}}},"manager":"kubectl-client-side-apply","operation":"Update","time":"2020-07-23T19:57:35Z"}],"name":"nginx-deployment","namespace":"default","resourceVersion":"2918","selfLink":"/apis/apps/v1/namespaces/default/deployments/nginx-deployment","uid":"8dbf6b57-dbfc-4d46-9935-3b85bcd52895"},"spec":{"progressDeadlineSeconds":600,"replicas":2,"revisionHistoryLimit":10,"selector":{"matchLabels":{"app":"nginx"}},"strategy":{"rollingUpdate":{"maxSurge":"25%","maxUnavailable":"25%"},"type":"RollingUpdate"},"template":{"metadata":{"creationTimestamp":null,"labels":{"app":"nginx"}},"spec":{"containers":[{"image":"nginx:1.14.5","imagePullPolicy":"IfNotPresent","name":"nginx","ports":[{"containerPort":80,"protocol":"TCP"}],"resources":{},"terminationMessagePath":"/dev/termination-log","terminationMessagePolicy":"File"}],"dnsPolicy":"ClusterFirst","restartPolicy":"Always","schedulerName":"default-scheduler","securityContext":{},"terminationGracePeriodSeconds":30}}},"status":{"conditions":[{"lastTransitionTime":"2020-07-23T19:57:35Z","lastUpdateTime":"2020-07-23T19:57:35Z","message":"Deployment does not have minimum availability.","reason":"MinimumReplicasUnavailable","status":"False","type":"Available"},{"lastTransitionTime":"2020-07-23T19:57:35Z","lastUpdateTime":"2020-07-23T19:57:35Z","message":"ReplicaSet \"nginx-deployment-7b9cf55d7f\" is progressing.","reason":"ReplicaSetUpdated","status":"True","type":"Progressing"}],"observedGeneration":1,"replicas":2,"unavailableReplicas":2,"updatedReplicas":2}}`
		modified := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"apiVersion\":\"apps/v1\",\"kind\":\"Deployment\",\"metadata\":{\"annotations\":{},\"name\":\"nginx-deployment\",\"namespace\":\"default\"},\"spec\":{\"replicas\":2,\"selector\":{\"matchLabels\":{\"app\":\"nginx\"}},\"template\":{\"metadata\":{\"labels\":{\"app\":\"nginx\"}},\"spec\":{\"containers\":[{\"image\":\"nginx:1.14.5\",\"name\":\"nginx\",\"ports\":[{\"containerPort\":80}]}]}}}}\n"},"name":"nginx-deployment","namespace":"default"},"spec":{"replicas":2,"selector":{"matchLabels":{"app":"nginx"}},"template":{"metadata":{"labels":{"app":"nginx"}},"spec":{"containers":[{"image":"nginx:1.15.5","name":"nginx","ports":[{"containerPort":80}]}]}}}}`

		decoder := serializer.NewCodecFactory(testScheme).UniversalDecoder()
		var currentDeployment appsv1.Deployment
		require.NoError(t, runtime.DecodeInto(decoder, []byte(current), &currentDeployment))
		var modifiedDeployment appsv1.Deployment
		require.NoError(t, runtime.DecodeInto(decoder, []byte(modified), &modifiedDeployment))

		// No annotation on current object, only on modified.
		require.NoError(t, setLastApplied(&modifiedDeployment))

		patchType, patchBytes, err := GeneratePatch(&currentDeployment, &modifiedDeployment, false)
		require.NoError(t, err)
		require.Equal(t, types.StrategicMergePatchType, patchType)
		requirePatchEquals(t,
			patchBytes,
			&modifiedDeployment,
			`{"metadata":{"annotations":{},"resourceVersion":"2918"},"spec":{"template":{"spec":{"$setElementOrder/containers":[{"name":"nginx"}],"containers":[{"image":"nginx:1.15.5","name":"nginx"}]}}}}`,
		)
	})

	t.Run("Overwrite", func(t *testing.T) {
		// These values were pulled from a k3d cluster and simulates this
		// scenario:
		//   1. The original deployment used nginx 1.13.0
		//   2. An operator manually upgraded to nginx 1.14.2
		//   3. Apply is now used to upgrade to nginx 1.19.0
		current := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"annotations":{"deployment.kubernetes.io/revision":"1","kubectl.kubernetes.io/last-applied-configuration":"{\"apiVersion\":\"apps/v1\",\"kind\":\"Deployment\",\"metadata\":{\"name\":\"nginx\"},\"spec\":{\"replicas\":1,\"selector\":{\"matchLabels\":{\"app\":\"nginx\"}},\"template\":{\"metadata\":{\"creationTimestamp\":null,\"labels\":{\"app\":\"nginx\"}},\"spec\":{\"containers\":[{\"image\":\"nginx:1.13.0\",\"name\":\"nginx\",\"resources\":{}}]}}}}\n"},"generation":1,"managedFields":[{"apiVersion":"apps/v1","fieldsType":"FieldsV1","fieldsV1":{"f:metadata":{"f:annotations":{".":{},"f:kubectl.kubernetes.io/last-applied-configuration":{}}},"f:spec":{"f:progressDeadlineSeconds":{},"f:replicas":{},"f:revisionHistoryLimit":{},"f:selector":{"f:matchLabels":{".":{},"f:app":{}}},"f:strategy":{"f:rollingUpdate":{".":{},"f:maxSurge":{},"f:maxUnavailable":{}},"f:type":{}},"f:template":{"f:metadata":{"f:labels":{".":{},"f:app":{}}},"f:spec":{"f:containers":{"k:{\"name\":\"nginx\"}":{".":{},"f:image":{},"f:imagePullPolicy":{},"f:name":{},"f:resources":{},"f:terminationMessagePath":{},"f:terminationMessagePolicy":{}}},"f:dnsPolicy":{},"f:restartPolicy":{},"f:schedulerName":{},"f:securityContext":{},"f:terminationGracePeriodSeconds":{}}}}},"manager":"Go-http-client","operation":"Update","time":"2020-07-27T19:33:59Z"},{"apiVersion":"apps/v1","fieldsType":"FieldsV1","fieldsV1":{"f:metadata":{"f:annotations":{"f:deployment.kubernetes.io/revision":{}}},"f:status":{"f:conditions":{".":{},"k:{\"type\":\"Available\"}":{".":{},"f:lastTransitionTime":{},"f:lastUpdateTime":{},"f:message":{},"f:reason":{},"f:status":{},"f:type":{}},"k:{\"type\":\"Progressing\"}":{".":{},"f:lastTransitionTime":{},"f:lastUpdateTime":{},"f:message":{},"f:reason":{},"f:status":{},"f:type":{}}},"f:observedGeneration":{},"f:replicas":{},"f:unavailableReplicas":{},"f:updatedReplicas":{}}},"manager":"k3s","operation":"Update","time":"2020-07-27T19:33:59Z"}],"name":"nginx"},"spec":{"progressDeadlineSeconds":600,"replicas":1,"revisionHistoryLimit":10,"selector":{"matchLabels":{"app":"nginx"}},"strategy":{"rollingUpdate":{"maxSurge":"25%","maxUnavailable":"25%"},"type":"RollingUpdate"},"template":{"metadata":{"creationTimestamp":null,"labels":{"app":"nginx"}},"spec":{"containers":[{"image":"nginx:1.14.2","imagePullPolicy":"IfNotPresent","name":"nginx","resources":{},"terminationMessagePath":"/dev/termination-log","terminationMessagePolicy":"File"}],"dnsPolicy":"ClusterFirst","restartPolicy":"Always","schedulerName":"default-scheduler","securityContext":{},"terminationGracePeriodSeconds":30}}}}`
		modified := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"annotations":{"deployment.kubernetes.io/revision":"1"},"generation":1,"name":"nginx","namespace":"not-default"},"spec":{"progressDeadlineSeconds":600,"replicas":1,"revisionHistoryLimit":10,"selector":{"matchLabels":{"app":"nginx"}},"strategy":{"rollingUpdate":{"maxSurge":"25%","maxUnavailable":"25%"},"type":"RollingUpdate"},"template":{"metadata":{"creationTimestamp":null,"labels":{"app":"nginx"}},"spec":{"containers":[{"image":"nginx:1.19.0","imagePullPolicy":"IfNotPresent","name":"nginx","resources":{},"terminationMessagePath":"/dev/termination-log","terminationMessagePolicy":"File"}],"dnsPolicy":"ClusterFirst","restartPolicy":"Always","schedulerName":"default-scheduler","securityContext":{},"terminationGracePeriodSeconds":30}}}}`

		decoder := serializer.NewCodecFactory(testScheme).UniversalDecoder()
		var currentDeployment appsv1.Deployment
		require.NoError(t, runtime.DecodeInto(decoder, []byte(current), &currentDeployment))
		var modifiedDeployment appsv1.Deployment
		require.NoError(t, runtime.DecodeInto(decoder, []byte(modified), &modifiedDeployment))

		// No annotation on current object, only on modified.
		require.NoError(t, setLastApplied(&modifiedDeployment))

		// First attempt fails b/c there's a merge conflict on nginx version.
		_, _, err := GeneratePatch(&currentDeployment, &modifiedDeployment, false)
		require.Error(t, err)
		require.Contains(t, err.Error(), "conflicts with changes")

		// Overwriting allows this patch to be generated successfully
		patchType, patchBytes, err := GeneratePatch(&currentDeployment, &modifiedDeployment, true)
		require.NoError(t, err)
		require.Equal(t, types.StrategicMergePatchType, patchType)
		// Assert that we've updated the nginx image
		require.Contains(t, string(patchBytes), `"containers":[{"image":"nginx:1.19.0","name":"nginx"}]`)
		// Assert that the last applied config annotation is also being updated
		require.Contains(t, string(patchBytes), `{"annotations":{"kubectl.kubernetes.io/last-applied-configuration"`)
	})

	t.Run("MergePatchType", func(t *testing.T) {
		current := tenantCrd.DeepCopy()
		modified := tenantCrd.DeepCopy()

		// Simulate an update to the description.
		modified.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "modified desc"

		require.NoError(t, setLastApplied(current))
		require.NoError(t, setLastApplied(modified))

		// Update current without updating last-applied.
		current.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "current desc"

		// Expect conflict to be detected.
		_, _, err := GeneratePatch(current, modified, false)
		require.Error(t, err)
		require.Contains(t, err.Error(), "conflicts with changes")

		// Now set last-applied again and expect no conflict this time.
		require.NoError(t, setLastApplied(current))

		patchType, patchBytes, err := GeneratePatch(current, modified, false)
		require.NoError(t, err)
		require.Equal(t, types.MergePatchType, patchType)
		requirePatchEquals(t,
			patchBytes,
			modified,
			`{"metadata":{"annotations":{}},"spec":{"versions":[{"name":"v1beta1","schema":{"openAPIV3Schema":{"description":"modified desc"}},"served":false,"storage":false}]}}`,
		)
	})

	t.Run("WebhookMatchPolicyUpgradeConflict", func(t *testing.T) {
		sideEffects := admissionregistrationv1.SideEffectClassNone
		failurePolicy := admissionregistrationv1.Fail
		matchEquivalent := admissionregistrationv1.Equivalent
		matchExact := admissionregistrationv1.Exact
		mutatePath := "/mutate"

		// When matchPolicy was not previously set, the API server defaults it
		// to Equivalent on the live object while last-applied (client-side)
		// omits it. Adding an explicit matchPolicy: Exact causes a three-way
		// merge conflict: original=nil, current=Equivalent, modified=Exact.
		base := admissionregistrationv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: "cockroach-mutating-webhook-config",
			},
			Webhooks: []admissionregistrationv1.MutatingWebhook{
				{
					Name:                    "mutating.cockroach-webhook.cockroachlabs.com",
					AdmissionReviewVersions: []string{"v1"},
					SideEffects:             &sideEffects,
					FailurePolicy:           &failurePolicy,
					ClientConfig: admissionregistrationv1.WebhookClientConfig{
						Service: &admissionregistrationv1.ServiceReference{
							Name:      "cockroach-webhook-service",
							Namespace: "cockroach-ns",
							Path:      &mutatePath,
						},
					},
					Rules: []admissionregistrationv1.RuleWithOperations{
						{
							Operations: []admissionregistrationv1.OperationType{"CREATE", "UPDATE"},
							Rule: admissionregistrationv1.Rule{
								APIGroups:   []string{"crdb.cockroachlabs.com"},
								APIVersions: []string{"v1beta1"},
								Resources:   []string{"crdbclusters"},
							},
						},
					},
				},
			},
		}

		// current = live object with server-defaulted matchPolicy: Equivalent
		// and last-applied from old version (no matchPolicy)
		current := base.DeepCopy()
		current.Webhooks[0].MatchPolicy = &matchEquivalent
		oldVersion := base.DeepCopy()
		require.NoError(t, setLastApplied(oldVersion))
		current.Annotations = oldVersion.Annotations

		// modified = new version that explicitly sets matchPolicy: Exact
		modified := base.DeepCopy()
		modified.Webhooks[0].MatchPolicy = &matchExact
		require.NoError(t, setLastApplied(modified))

		// Without overwrite, the three-way merge detects a conflict between
		// the server-defaulted Equivalent and our desired Exact.
		_, _, err := GeneratePatch(current, modified, false)
		require.Error(t, err)
		require.Contains(t, err.Error(), "conflicts with changes")

		// With overwrite, the patch succeeds.
		patchType, patchBytes, err := GeneratePatch(current, modified, true)
		require.NoError(t, err)
		require.Equal(t, types.StrategicMergePatchType, patchType)
		require.Contains(t, string(patchBytes), `"matchPolicy":"Exact"`)
	})
}

// requirePatchEquals verifies a patch's correctness.
func requirePatchEquals(t *testing.T, patch []byte, modified Object, expected string) {
	var patchMap map[string]any
	require.NoError(t, json.Unmarshal(patch, &patchMap))

	// Save the existing value of the last-applied annotation.
	md := patchMap["metadata"].(map[string]any)
	anns := md["annotations"].(map[string]any)
	existingLastApplied := anns[corev1.LastAppliedConfigAnnotation].(string)

	// Now remove the annotation and create new patch without it.
	unstructured.RemoveNestedField(patchMap, "metadata", "annotations", corev1.LastAppliedConfigAnnotation)
	newPatch, err := json.Marshal(patchMap)
	require.NoError(t, err)
	require.Equal(t, expected, string(newPatch))

	// Verify that the lastApplied annotation matches the state of the modified
	// object.
	expectedLastApplied, err := lastApplied(modified)
	require.NoError(t, err)
	require.Equal(t, string(expectedLastApplied), existingLastApplied)
}
