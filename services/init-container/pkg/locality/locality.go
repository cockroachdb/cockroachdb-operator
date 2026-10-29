package locality

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/cockroachdb/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type LocalityChecker struct {
	// The clientset for interacting with the Kubernetes API.
	Clientset kubernetes.Interface

	// The name of the Kubernetes node the container is running on.
	NodeName string

	// The directory to write locality information.
	WritePath string

	// Whether to error if node does not have locality labels.
	ErrorOnMissingLabels bool

	Mappings []LocalityMapping
}

type LocalityMapping struct {
	NodeLabel     string `json:"nodeLabel,omitempty"`
	LocalityLabel string `json:"localityLabel,omitempty"`
	Prefix        string `json:"prefix,omitempty"`
}

func (l *LocalityChecker) WriteLocality(ctx context.Context) error {
	if len(l.Mappings) == 0 {
		return nil
	}
	labels, err := l.getLabels(ctx)
	if err != nil {
		return errors.Wrapf(err, "getting locality labels")
	}
	return l.writeLabels(labels)
}

func (l *LocalityChecker) getLabels(ctx context.Context) ([]string, error) {
	node, err := l.Clientset.CoreV1().Nodes().Get(ctx, l.NodeName, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "node not found")
	}
	labels := node.GetObjectMeta().GetLabels()

	localityLabels := []string{}
	for _, mapping := range l.Mappings {
		if value, ok := labels[mapping.NodeLabel]; ok && value != "" {
			localityLabels = append(localityLabels, fmt.Sprintf("%s=%s", mapping.LocalityLabel, mapping.Prefix+value))
		} else {
			if l.ErrorOnMissingLabels {
				return nil, errors.Newf("node label %s not found", mapping.NodeLabel)
			}
		}
	}

	return localityLabels, nil
}

func (l *LocalityChecker) writeLabels(labels []string) error {
	path := fmt.Sprintf("%s/locality", l.WritePath)
	content := fmt.Sprintf("--locality=%s", strings.Join(labels, ","))
	return os.WriteFile(path, []byte(content), 0644)
}
