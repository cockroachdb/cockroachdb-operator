package kube_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/testenv"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	corev1 "k8s.io/api/core/v1"
)

func TestIntegrationPodDialer(t *testing.T) {
	testutils.MarkIntegrationTest(t)

	// Create insecure CRDB cluster in order to test HTTP connection below.
	env := testenv.New(t, testenv.SingleZoneConfig)
	cluster := testenv.CreateCrdbCluster(t, env, "test", &crdbv1beta1.CrdbClusterSpec{
		TLSEnabled: true,
	})
	namespace := cluster.Namespace
	kubeCluster := env.DefaultCluster()
	ctl := kubeCluster.NewCtl(namespace)

	// Tunnel into an operator pod using the pod dialer.
	dialer, err := kube.NewPodDialer(ctl.RestConfig(), namespace)
	require.NoError(t, err)

	// Construct HTTP client to scrape metrics.
	client := http.Client{
		Transport: &http.Transport{
			DialContext: dialer.DialContext,
			// Rather than reusing TCP connections, we will close them once the
			// HTTP request is done to prevent accidental leakage of goroutines.
			DisableKeepAlives: true,
			// TLS validation is not being tested here, we just want to show
			// that we can make an HTTPS connection with the cluster. It is
			// tested elsewhere.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	var podList corev1.PodList
	require.NoError(t, ctl.List(
		context.Background(),
		&kube.ListOptions{LabelSelector: kube.ParseLabelOrDie("app=cockroachdb")},
		&podList))
	require.Len(t, podList.Items, 1)
	podName := podList.Items[0].GetName()
	fmt.Println(podName)

	resp, err := client.Get(fmt.Sprintf("http://%s:8080/", podName))
	require.NoError(t, err)
	defer resp.Body.Close()

	// To be fairly resilient to changes in CockroachDB, search for an element that we expect to be pretty unique to and
	// stable across versions of CockroachDB. Of course, if this string does change we'll need to update this test.
	doc, err := html.Parse(resp.Body)
	require.NoError(t, err)

	var foundTitle = ""
	var findTitle func(n *html.Node)
	findTitle = func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == atom.Title {
			foundTitle = n.FirstChild.Data
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			findTitle(c)
		}
	}
	findTitle(doc)
	require.Equal(t, "Cockroach Console", foundTitle)
}
