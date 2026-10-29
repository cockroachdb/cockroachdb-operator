package betaclusterctrl_test

import (
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	. "github.com/cockroachlabs/cockroach-enterprise-operator/controllers/betaclusterctrl"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestPostInitSQLCollect(t *testing.T) {
	tests := []struct {
		desc       string
		spec       *crdbv1beta1.PostInitSQL
		configMap  string // body returned by the fake resolver for ConfigMapRef
		secret     string // body returned by the fake resolver for SecretRef
		expectSrcs []string
		expectSQLs []string
	}{
		{
			desc:       "nil spec returns no units",
			spec:       nil,
			expectSrcs: nil,
			expectSQLs: nil,
		},
		{
			desc: "inline only preserves order",
			spec: &crdbv1beta1.PostInitSQL{
				Inline: []string{"CREATE DATABASE a", "CREATE DATABASE b"},
			},
			expectSrcs: []string{"inline[0]", "inline[1]"},
			expectSQLs: []string{"CREATE DATABASE a", "CREATE DATABASE b"},
		},
		{
			desc: "all three sources: secret blob, configmap blob, inline items",
			spec: &crdbv1beta1.PostInitSQL{
				Inline:       []string{"CREATE DATABASE c"},
				ConfigMapRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "cm"}, Key: "init.sql"},
				SecretRef:    &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "lic.sql"},
			},
			secret:    "SELECT crdb_internal.set_license('xxx;yyy');",
			configMap: "CREATE DATABASE b;\nCREATE USER bob;\n",
			expectSrcs: []string{
				"secret/s/lic.sql",
				"configmap/cm/init.sql",
				"inline[0]",
			},
			expectSQLs: []string{
				"SELECT crdb_internal.set_license('xxx;yyy');",
				"CREATE DATABASE b;\nCREATE USER bob;\n",
				"CREATE DATABASE c",
			},
		},
		{
			desc: "empty inline items are skipped",
			spec: &crdbv1beta1.PostInitSQL{
				Inline: []string{"CREATE DATABASE a", "", "   ", "CREATE DATABASE b"},
			},
			expectSrcs: []string{"inline[0]", "inline[3]"},
			expectSQLs: []string{"CREATE DATABASE a", "CREATE DATABASE b"},
		},
		{
			desc: "whitespace-only configmap body produces no unit",
			spec: &crdbv1beta1.PostInitSQL{
				ConfigMapRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "cm"}, Key: "init.sql"},
			},
			configMap:  "   \n  \t  \n",
			expectSrcs: nil,
			expectSQLs: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			pis := &PostInitSQL{
				Spec: tt.spec,
				ResolveConfigMap: func(_ *corev1.ConfigMapKeySelector) (string, error) {
					return tt.configMap, nil
				},
				ResolveSecret: func(_ *corev1.SecretKeySelector) (string, error) {
					return tt.secret, nil
				},
			}

			stmts, err := pis.Collect()
			require.NoError(t, err)

			var srcs, sqls []string
			for _, s := range stmts {
				srcs = append(srcs, s.Source)
				sqls = append(sqls, s.SQL)
			}
			require.Equal(t, tt.expectSrcs, srcs)
			require.Equal(t, tt.expectSQLs, sqls)
		})
	}
}
