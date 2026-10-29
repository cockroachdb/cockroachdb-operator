package denylist

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
)

func TestDenyListFileParsing(t *testing.T) {
	t.Run("test custom marshal code", func(t *testing.T) {
		cases := []struct {
			t        Type
			expected string
		}{
			{IPAddrType, "ip"},
			{ClusterType, "cluster"},
		}
		for _, tc := range cases {
			s, err := tc.t.MarshalYAML()
			require.NoError(t, err)
			require.Equal(t, tc.expected, s)
		}
	})

	t.Run("test custom unmarshal code", func(t *testing.T) {
		cases := []struct {
			raw      string
			expected Type
		}{
			{"ip", IPAddrType},
			{"IP", IPAddrType},
			{"Ip", IPAddrType},
			{"Cluster", ClusterType},
			{"cluster", ClusterType},
			{"CLUSTER", ClusterType},
			{"random text", UnknownType},
		}
		for _, tc := range cases {
			var parsed Type
			err := yaml.UnmarshalStrict([]byte(tc.raw), &parsed)
			require.NoError(t, err)
			require.Equal(t, tc.expected, parsed)
		}
	})

	t.Run("end to end testing of file parsing", func(t *testing.T) {
		expirationTimeString := "2021-01-01T15:20:39Z"
		var expirationTime time.Time
		require.NoError(t, expirationTime.UnmarshalText([]byte(expirationTimeString)))

		testCases := []struct {
			input    string
			expected File
		}{
			{"", File{}},
			{"SequenceNumber: 7", File{
				Seq: 7,
			}},
			{
				fmt.Sprintf(`
SequenceNumber: 9
denylist:
- entity: {"item":"1.2.3.4", "type": "ip"}
  expiration: %s
  reason: over quota`,
					expirationTimeString,
				),
				File{
					Seq: 9,
					Denylist: []*DenyEntry{
						{
							Entity: DenyEntity{
								Item: "1.2.3.4",
								Type: IPAddrType,
							},
							Expiration: expirationTime,
							Reason:     "over quota",
						},
					},
				},
			},
		}

		// use cancel to prevent leaked goroutines from file watches
		for _, tc := range testCases {
			file, err := Deserialize([]byte(tc.input))
			require.NoError(t, err)
			require.Equal(t, &tc.expected, file, "should return expected parsed file for %s",
				tc.input)
		}
	})

	t.Run("test Ser/De of File", func(t *testing.T) {
		expirationTimeString := "2021-01-01T15:20:39Z"
		var expirationTime time.Time
		require.NoError(t, expirationTime.UnmarshalText([]byte(expirationTimeString)))

		file := File{
			Seq: 72,
			Denylist: []*DenyEntry{
				{
					DenyEntity{"63", ClusterType},
					expirationTime,
					"over usage",
				},
				{
					DenyEntity{"8.8.8.8", IPAddrType},
					expirationTime,
					"malicious IP",
				},
			},
		}

		raw, err := file.Serialize()
		require.NoError(t, err)
		deserialized, err := Deserialize(raw)
		require.NoError(t, err)
		require.EqualValues(t, file, *deserialized)
	})
}
