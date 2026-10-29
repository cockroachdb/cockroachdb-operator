package kube_test

import (
	"testing"

	. "github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
)

func TestEventEmitter(t *testing.T) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bogus-ns",
		},
	}

	r := record.NewFakeRecorder(2)
	emitter := &EventEmitter{EventRecorder: r}
	emitter.Infof(ns, "Created", "Created the %s namespace", ns.Name)
	emitter.Warnf(ns, "Deleted", "Deleted the %s namespace", ns.Name)

	require.Equal(t, "Normal Created Created the bogus-ns namespace", <-r.Events)
	require.Equal(t, "Warning Deleted Deleted the bogus-ns namespace", <-r.Events)

	t.Run("when recorder is nil", func(t *testing.T) {
		emitter := &EventEmitter{}
		emitter.Infof(nil, "something", "something %s", "arg")
		emitter.Warnf(nil, "something", "something %s", "arg")
	})
}
