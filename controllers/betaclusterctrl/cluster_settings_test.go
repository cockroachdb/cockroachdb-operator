package betaclusterctrl_test

import (
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	. "github.com/cockroachlabs/cockroach-enterprise-operator/controllers/betaclusterctrl"
	"github.com/stretchr/testify/require"
)

func TestClusterSettings(t *testing.T) {
	tests := []struct {
		desc     string
		spec     map[string]string
		status   map[string]string
		expected map[string]string
	}{
		{
			desc:     "No diff",
			spec:     map[string]string{"key": "value"},
			status:   map[string]string{"key": "value"},
			expected: make(map[string]string),
		},
		{
			desc:     "No settings in status",
			spec:     map[string]string{"key": "value"},
			expected: map[string]string{"key": "value"},
		},
		{
			desc:     "No settings in spec",
			status:   map[string]string{"key": "value"},
			expected: map[string]string{"key": "DEFAULT"},
		},
		{
			desc:     "Different values",
			spec:     map[string]string{"key": "value"},
			status:   map[string]string{"key": "oldValue"},
			expected: map[string]string{"key": "value"},
		},
		{
			desc: "override, new, and old values",
			spec: map[string]string{
				"key": "value",
				"new": "setting",
			},
			status: map[string]string{
				"key": "oldValue",
				"old": "setting",
			},
			expected: map[string]string{
				"key": "value",
				"new": "setting",
				"old": "DEFAULT",
			},
		},
	}

	for _, tt := range tests {
		cs := &ClusterSettings{
			Cluster: &v1beta1.CrdbCluster{
				Spec: v1beta1.CrdbClusterSpec{
					ClusterSettings: tt.spec,
				},
				Status: v1beta1.CrdbClusterStatus{Settings: tt.status},
			},
		}

		require.Equal(t, tt.expected, cs.Diff(), tt.desc)
		require.Equal(t, len(tt.expected) > 0, cs.IsStale(), tt.desc)
	}
}
