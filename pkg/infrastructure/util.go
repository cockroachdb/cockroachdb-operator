package infrastructure

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/labelutil"
	corev1 "k8s.io/api/core/v1"
)

func NodeToVM(node *corev1.Node) (*VM, error) {
	zone, err := labelutil.GetZone(node.GetLabels())
	if err != nil {
		return nil, errors.Wrap(err, "unable to determine node zone")
	}

	return &VM{
		Name:             node.Name,
		AvailabilityZone: AvailabilityZone(zone),
	}, nil
}

// ParseAzureZoneIDFromAvailabilityZone extracts the zone ID from an Azure
// AvailabilityZone. Note that this function panics if the provided AZ is not a
// valid Azure AZ.
func ParseAzureZoneIDFromAvailabilityZone(az AvailabilityZone) int {
	regex := regexp.MustCompile(`^[a-zA-Z0-9]+-(\d+)$`)
	matches := regex.FindStringSubmatch(string(az))
	if len(matches) != 2 {
		panic(fmt.Sprintf("cannot parse zone ID from given Azure availability zone: %s", az))
	}

	zoneID, err := strconv.Atoi(matches[1])
	if err != nil {
		panic(fmt.Sprintf("cannot convert zone ID string to int: %s", matches[1]))
	}

	return zoneID
}
