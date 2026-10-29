package kube

import (
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

// GetProxyURL returns a URL prefix that can be used to proxy requests to a
// service via the k8s API server.
//
// https://kubernetes.io/docs/tasks/access-application-cluster/access-cluster/#manually-constructing-apiserver-proxy-urls
//
// If the service has a non-default port, it can be reached by appending the
// portname to the service name.
//
// Example Usage:
// prometheusURL,err := GetProxyUrl(config, "region-monitoring","prometheus:http", "prometheus")
func GetProxyURL(
	config *rest.Config, namespace string, service string, subResources ...string,
) (string, error) {
	sr := []string{"proxy"}
	sr = append(sr, subResources...)

	// We create a client and request just so we can extract the URL. This
	// doesn't actually trigger a requests to the API server.
	client, err := corev1.NewForConfig(config)
	if err != nil {
		return "", err
	}

	req := client.RESTClient().
		Get().
		Resource("services").
		Namespace(namespace).
		Name(service).
		SubResource(sr...)

	return req.URL().String(), nil
}
