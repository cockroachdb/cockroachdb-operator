package hashutil_test

import (
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	. "github.com/cockroachlabs/cockroach-enterprise-operator/util/hashutil"
	"github.com/stretchr/testify/require"
)

func TestFNV(t *testing.T) {
	testValue := struct {
		Bool   bool
		Int    int
		String string
	}{
		Bool:   true,
		Int:    1020,
		String: "some value",
	}

	tests := []struct {
		desc  string
		given any
	}{
		{desc: "bool", given: true},
		{desc: "int", given: 102},
		{desc: "object", given: testValue},
		{desc: "string", given: "some string"},
		{
			desc: "map",
			// This implicitly tests out of order keys, since Go's implementation
			// does not keep map keys in insert order.
			given: map[string]any{
				"string": "some string",
				"int":    1,
				"bool":   true,
				"object": map[string]string{
					"k1": "v1",
					"k2": "v2",
				},
			},
		},
	}

	for _, tt := range tests {
		raw, err := FNV(tt.given)
		require.NoError(t, err)
		testutils.AssertGolden(t, tt.desc, []byte(raw))
	}
}

func TestMustFNV(t *testing.T) {
	testObj := struct {
		Foo func()
	}{
		Foo: func() {},
	}
	require.Panics(t, func() {
		// json: unsupported type: func()
		_ = MustFNV(testObj)
	})
}
