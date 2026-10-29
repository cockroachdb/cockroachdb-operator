package stringutil

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStringSet(t *testing.T) {
	ss := []string{}

	require.True(t, StringSetAdd(&ss, "foo"))
	require.False(t, StringSetAdd(&ss, "foo"))
	require.Contains(t, ss, "foo")

	require.True(t, StringSetRemove(&ss, "foo"))
	require.False(t, StringSetRemove(&ss, "foo"))
	require.Empty(t, ss)

	require.True(t, StringSetAdd(&ss, "foo"))
	require.True(t, StringSetAdd(&ss, "bar"))
	require.True(t, StringSetAdd(&ss, "baz"))
	require.Contains(t, ss, "bar")
	require.True(t, StringSetRemove(&ss, "bar"))
	require.Equal(t, []string{"foo", "baz"}, ss)

	require.True(t, StringSetAdd(&ss, "bat"))
	require.True(t, StringSetRemove(&ss, "foo"))
	require.Equal(t, []string{"baz", "bat"}, ss)
}
