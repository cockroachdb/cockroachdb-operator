package labelutil

import (
	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
)

const (
	ZoneKey           = corev1.LabelTopologyZone
	DeprecatedZoneKey = corev1.LabelFailureDomainBetaZone
)

func GetZone(labels map[string]string) (string, error) {
	return getFirstValue(labels, []string{
		ZoneKey,
		DeprecatedZoneKey,
	})
}

func getFirstValue(haystack map[string]string, needles []string) (string, error) {
	for _, needle := range needles {
		if value, ok := haystack[needle]; ok && value != "" {
			return value, nil
		}
	}
	return "", errors.New("value not found")
}
