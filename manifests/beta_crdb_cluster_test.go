package manifests

import (
	"fmt"
	"strings"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func assertJoin(t *testing.T, cluster *crdbv1beta1.CrdbCluster, join string) {
	t.Helper()
	node := BetaCrdbClusterNode(cluster, cluster.Region(), "")
	// Save ourselves some typing by always including the service prefix and
	// port here.
	spl := strings.Split(join, ",")
	for i := range spl {
		spl[i] = fmt.Sprintf("%s-join.%s:26258", cluster.Name, spl[i])
	}
	require.Equal(t, node.Spec.Join, strings.Join(spl, ","))
}

func TestBetaCrdbClusterNodeJoin(t *testing.T) {
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "name",
			Namespace: "namespace",
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Nodes: 3},
			},
		},
	}

	// Minimal case for easy single region clusters.
	assertJoin(t, cluster, "namespace.svc.cluster.local")

	// Respect .Code, if provided,
	cluster.Spec.Regions[0].Code = "us-east-1"
	assertJoin(t, cluster, "us-east-1.svc.cluster.local")

	// Respect .Domain, if provided,
	cluster.Spec.Regions[0].Domain = "domain-1"
	assertJoin(t, cluster, "us-east-1.svc.domain-1")

	// Prefer .Namespace, if provided.
	cluster.Spec.Regions[0].Namespace = "namespace-1"
	assertJoin(t, cluster, "namespace-1.svc.domain-1")

	// Minimal case with two regions, this is technically valid but probably
	// shouldn't be and likely won't work.
	cluster.Spec.Regions = append(cluster.Spec.Regions, crdbv1beta1.CrdbClusterRegion{})
	assertJoin(t, cluster, "namespace-1.svc.domain-1,namespace.svc.cluster.local")

	// Same tests as above but asserting that they only change the second entry in the string.

	// Respect .Code, if provided,
	cluster.Spec.Regions[1].Code = "us-east-2"
	assertJoin(t, cluster, "namespace-1.svc.domain-1,us-east-2.svc.cluster.local")

	// Respect .Domain, if provided,
	cluster.Spec.Regions[1].Domain = "domain-2"
	assertJoin(t, cluster, "namespace-1.svc.domain-1,us-east-2.svc.domain-2")

	// Prefer .Namespace, if provided.
	cluster.Spec.Regions[1].Namespace = "namespace-2"
	assertJoin(t, cluster, "namespace-1.svc.domain-1,namespace-2.svc.domain-2")
}

func TestUpdateBetaCrdbClusterNode(t *testing.T) {
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "name",
			Namespace: "namespace",
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Nodes: 3},
			},
		},
	}

	node := BetaCrdbClusterNode(cluster, cluster.Region(), "")

	node.MarkForDecommission()
	UpdateBetaCrdbClusterNode(cluster, node, nil, "")
	require.True(t, node.IsMarkedForDecommission())

	node.Decommission(1)
	UpdateBetaCrdbClusterNode(cluster, node, nil, "")
	require.True(t, node.IsDecommissioning())
}

func TestTopologySpreadConstraintsBetaCrdbClusterNode(t *testing.T) {

	topologySpreadConstraint := []corev1.TopologySpreadConstraint{
		{
			TopologyKey:       CrdbClusterNodeTopologyConstraintKey,
			MaxSkew:           1,
			WhenUnsatisfiable: corev1.DoNotSchedule,
			LabelSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					BetaCrdbClusterNodeOwnerLabelKey: "name",
				},
			},
		},
	}
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "name",
			Namespace: "namespace",
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Nodes: 3},
			},
			Template: crdbv1beta1.CrdbNodeTemplate{
				Spec: crdbv1beta1.CrdbNodeSpec{
					TopologySpreadConstraints: topologySpreadConstraint,
				},
			},
		},
	}

	node := BetaCrdbClusterNode(cluster, cluster.Region(), "")

	require.Equal(t, node.Spec.TopologySpreadConstraints, topologySpreadConstraint)
}

func TestBetaCrdbClusterNodeLocalityMappings(t *testing.T) {
	legacyDefault := []crdbv1beta1.LocalityMapping{
		{NodeLabel: corev1.LabelTopologyRegion, LocalityLabel: "region"},
		{NodeLabel: corev1.LabelTopologyZone, LocalityLabel: "zone"},
	}

	for _, tc := range []struct {
		name                      string
		provider                  string
		noCloudPrefixedLocalities bool
		basalt                    bool
		mappings                  []crdbv1beta1.LocalityMapping
		want                      []crdbv1beta1.LocalityMapping
	}{
		{
			name:     "empty defaults to region and zone with provider prefix",
			provider: "gcp",
			want: []crdbv1beta1.LocalityMapping{
				{NodeLabel: corev1.LabelTopologyRegion, LocalityLabel: "region", Prefix: "gcp-"},
				{NodeLabel: corev1.LabelTopologyZone, LocalityLabel: "zone", Prefix: "gcp-"},
			},
		},
		{
			name:     "legacy default mappings get provider prefix backfilled",
			provider: "gcp",
			mappings: legacyDefault,
			want: []crdbv1beta1.LocalityMapping{
				{NodeLabel: corev1.LabelTopologyRegion, LocalityLabel: "region", Prefix: "gcp-"},
				{NodeLabel: corev1.LabelTopologyZone, LocalityLabel: "zone", Prefix: "gcp-"},
			},
		},
		{
			name:                      "legacy mappings unchanged when cloud prefix disabled",
			provider:                  "gcp",
			noCloudPrefixedLocalities: true,
			mappings:                  legacyDefault,
			want:                      legacyDefault,
		},
		{
			name:     "legacy mappings unchanged when no provider",
			mappings: legacyDefault,
			want:     legacyDefault,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := &crdbv1beta1.CrdbCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "name", Namespace: "namespace"},
				Spec: crdbv1beta1.CrdbClusterSpec{
					Regions: []crdbv1beta1.CrdbClusterRegion{{Nodes: 3, CloudProvider: tc.provider}},
				},
			}
			cluster.Spec.Template.Spec.NoCloudPrefixedLocalities = tc.noCloudPrefixedLocalities
			cluster.Spec.Template.Spec.LocalityMappings = tc.mappings
			if tc.basalt {
				cluster.Spec.Template.Spec.DataStore.Basalt = &crdbv1beta1.BasaltStoreSpec{}
			}

			node := BetaCrdbClusterNode(cluster, &cluster.Spec.Regions[0], "")
			require.Equal(t, tc.want, node.Spec.LocalityMappings)
		})
	}
}
