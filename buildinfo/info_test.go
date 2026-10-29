package buildinfo

import (
	"bytes"
	"fmt"
	"runtime"
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/stretchr/testify/require"
)

func TestIsRelease(t *testing.T) {
	t.Run("non-release", func(t *testing.T) {
		defer testutils.HookGlobal(&typ, "")()
		require.False(t, IsRelease())
	})

	t.Run("release", func(t *testing.T) {
		defer testutils.HookGlobal(&typ, "release")()
		require.True(t, IsRelease())
	})
}

func TestPrint(t *testing.T) {
	defer testutils.HookGlobal(&rev, "some-sha")()
	defer testutils.HookGlobal(&tag, "a-tag")()
	defer testutils.HookGlobal(&typ, "release")()
	defer testutils.HookGlobal(&utcTime, "2006/01/02 15:04:05")()
	defer testutils.HookGlobal(&version, "the-version")()
	defer testutils.HookGlobal(&operatorVersion, "1.0.0")()

	expected := fmt.Sprintf(`Binary: my-binary
Operator Version: 1.0.0
Build Time: 2006/01/02 15:04:05
Go Version: %s
Is Release: true
Platform: %s %s
Revision: some-sha
Tag: a-tag
Build Version: the-version
`, runtime.Version(), runtime.GOOS, runtime.GOARCH)

	var buf bytes.Buffer

	printTo(&buf, "my-binary")

	require.Equal(t, expected, buf.String())
}

func TestOperatorVersion(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		defer testutils.HookGlobal(&operatorVersion, "")()
		require.Equal(t, "dev", OperatorVersion())
	})

	t.Run("set", func(t *testing.T) {
		defer testutils.HookGlobal(&operatorVersion, "1.0.0")()
		require.Equal(t, "1.0.0", OperatorVersion())
	})
}

func TestContainerImage(t *testing.T) {
	t.Run("non-release", func(t *testing.T) {
		defer testutils.HookGlobal(&typ, "")()
		require.Equal(t, "nginx:latest", ContainerImage("nginx", "latest"))
	})

	t.Run("release-with-operator-version", func(t *testing.T) {
		defer testutils.HookGlobal(&typ, "release")()
		defer testutils.HookGlobal(&rev, "cafebabe")()
		defer testutils.HookGlobal(&operatorVersion, "1.0.0")()
		require.Equal(t, "nginx:v1.0.0", ContainerImage("nginx", "latest"))
	})

	t.Run("release-without-operator-version", func(t *testing.T) {
		defer testutils.HookGlobal(&rev, "cafebabe")()
		defer testutils.HookGlobal(&typ, "release")()
		defer testutils.HookGlobal(&operatorVersion, "")()
		require.Equal(t, "nginx:cafebabe", ContainerImage("nginx", "latest"))
	})
}
