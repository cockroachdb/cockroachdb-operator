package v1beta1

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTenantID_IsSystemID(t *testing.T) {
	for tID, value := range map[TenantID]bool{
		TenantID(1):   true,
		TenantMinID:   false,
		TenantMaxID:   false,
		TenantNilID:   false,
		TenantID(999): false,
	} {
		require.Equal(t, value, tID.IsSystemID())
	}
}

func TestTenantID_Tag(t *testing.T) {
	for tID, value := range map[TenantID]string{
		TenantSystemID: "tenant-1",
		TenantMinID:    "tenant-2",
		TenantMaxID:    "tenant-18446744073709551615",
		TenantNilID:    "tenant-0",
		TenantID(999):  "tenant-999",
	} {
		require.Equal(t, value, tID.Tag())
	}
}

func TestTenantID_ToUint64(t *testing.T) {
	for tID, value := range map[TenantID]uint64{
		TenantID(1):   uint64(1),
		TenantID(999): uint64(999),
		TenantMinID:   uint64(2),
		TenantMaxID:   uint64(18446744073709551615),
		TenantNilID:   uint64(0),
	} {
		require.Equal(t, value, tID.ToUint64())
	}
}

func TestTenantID_String(t *testing.T) {
	for tID, expStr := range map[TenantID]string{
		TenantID(1):   "1",
		TenantID(999): "999",
		TenantMinID:   "2",
		TenantMaxID:   "18446744073709551615",
		TenantNilID:   "0",
	} {
		require.Equal(t, expStr, tID.String())
	}
}

func TestTenantID_IsValid(t *testing.T) {
	for tID, valid := range map[TenantID]bool{
		TenantID(1):   true,
		TenantID(999): true,
		TenantMinID:   true,
		TenantMaxID:   true,
		TenantNilID:   false,
	} {
		require.Equal(t, valid, tID.IsValid())
	}
}

func TestParseTenantID(t *testing.T) {
	for _, tc := range []struct {
		expStr string
		tID    TenantID
		err    string
	}{
		{"-1", TenantNilID, "parsing TenantID"},
		{"0", TenantNilID, "TenantID is invalid"},
		{"2", TenantMinID, ""},
		{"18446744073709551615", TenantMaxID, ""},
		{"999", TenantID(999), ""},
	} {
		result, err := ParseTenantID(tc.expStr)
		if tc.err != "" {
			require.Regexp(t, tc.err, err)
		} else {
			require.NoError(t, err)
		}
		require.Equal(t, tc.tID, result)
	}
}

func TestParseTenantTag(t *testing.T) {
	for _, tc := range []struct {
		expStr string
		tID    TenantID
		err    string
	}{
		{"10", TenantNilID, "starting with tenant-"},
		{"tenant-", TenantNilID, "parsing TenantID"},
		{"tenant--1", TenantNilID, "parsing TenantID"},
		{"tenant-0", TenantNilID, "TenantID is invalid"},
		{"tenant-2", TenantMinID, ""},
		{"tenant-18446744073709551615", TenantMaxID, ""},
		{"tenant-999", TenantID(999), ""},
	} {
		result, err := ParseTenantTag(tc.expStr)
		if tc.err != "" {
			require.Regexp(t, tc.err, err)
		} else {
			require.NoError(t, err)
		}
		require.Equal(t, tc.tID, result)
	}
}
