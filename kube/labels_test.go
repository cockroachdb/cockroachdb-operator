package kube

import (
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestLabelObjects(t *testing.T) {
	var replicas int32 = 3
	cases := []struct {
		desc     string
		inObject Object
		inLabels map[string]string
		want     Object
	}{
		{
			"deployment",
			&appsv1.Deployment{
				ObjectMeta: v12.ObjectMeta{
					Labels: map[string]string{
						"bang":      "baz",
						"overwrite": "old",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
				},
			},
			map[string]string{
				"foo":       "bar",
				"overwrite": "new",
			},
			&appsv1.Deployment{
				ObjectMeta: v12.ObjectMeta{
					Labels: map[string]string{
						"bang":      "baz",
						"overwrite": "new",
						"foo":       "bar",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
				},
			},
		},
		{
			"service",
			&apiv1.Service{}, // no labels set
			map[string]string{
				"apple": "sauce",
				"big":   "cat",
			},
			&apiv1.Service{
				ObjectMeta: v12.ObjectMeta{
					Labels: map[string]string{
						"apple": "sauce",
						"big":   "cat",
					},
				},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			gotErr := AddLabels(c.inLabels, c.inObject)
			require.NoError(t, gotErr)
			require.Equal(t, c.want, c.inObject) // AddLabels modifies passed in objects
		})
	}
}
