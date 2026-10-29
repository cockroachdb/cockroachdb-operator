package crypto

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDeterministicKeyGenerator tests that the rollover logic in
// deterministicKeyGenerator works.
func TestDeterministicKeyGenerator(t *testing.T) {
	keyGenerator := deterministicKeyGenerator()
	for range 200 {
		_, err := keyGenerator()
		require.NoError(t, err)
	}
}
