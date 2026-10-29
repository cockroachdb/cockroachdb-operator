package kube

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

func TestGetProxyURL(t *testing.T) {
	config := &rest.Config{}
	config.TLSClientConfig.CAData = []byte(`
-----BEGIN CERTIFICATE-----
MIIDazCCAlOgAwIBAgIUELXhTkWZszHwCva4ll+vD+gnnAUwDQYJKoZIhvcNAQEL
BQAwRTELMAkGA1UEBhMCQVUxEzARBgNVBAgMClNvbWUtU3RhdGUxITAfBgNVBAoM
GEludGVybmV0IFdpZGdpdHMgUHR5IEx0ZDAeFw0yMzAzMTQyMTE3NThaFw0yMzA0
MTMyMTE3NThaMEUxCzAJBgNVBAYTAkFVMRMwEQYDVQQIDApTb21lLVN0YXRlMSEw
HwYDVQQKDBhJbnRlcm5ldCBXaWRnaXRzIFB0eSBMdGQwggEiMA0GCSqGSIb3DQEB
AQUAA4IBDwAwggEKAoIBAQCreBUXElkxnQoQ9NqvvEKZf40Ry3CnCBFikPtrUhTE
GWMtU06Ho6v+hvStQlPqJ3NqhHigL3AWSKJP8krQQ6DzxgHao+E1qAF4pNDV81iV
21Sg6w2Jx0qZ5omuEPJZVVMYFt1gTNn0a3g1cdHqdJ3V4j5qUOxbPZkCiOT7rxOw
3oioMbG5LHA9zj5Et8t2hIWxaLMumuZoir6XNgImvD+wd+0+Lf6EthIsAEz4HTfW
YFiQvOpL/f5sKwXaUBFfO1n8B+EWmCWl4T7dI9XgMUY6G5hAsanLQjp8YiBgWCaG
AdWPT9sRh7XS/04avTzM+7s8r7LrSXBbzRO7qS0LS7SJAgMBAAGjUzBRMB0GA1Ud
DgQWBBTd14HZdYWtRyaVjs0cDTPAKnM6tjAfBgNVHSMEGDAWgBTd14HZdYWtRyaV
js0cDTPAKnM6tjAPBgNVHRMBAf8EBTADAQH/MA0GCSqGSIb3DQEBCwUAA4IBAQAD
Z6GgvWpSCysRPqoYJ14opOdGUXrzCVDs+6FW3rSjfZw60LQdIdTrVl4HJU91Zp65
mxvxye2vDW7gVIVF1uMxaOrwYhn8cppZmoMo/rS5lFhwHOAs8HqzTQAZOQhn1eNr
NnEqrB+X4f/Oq9hZrV3QRUwQh4n1KyDL8yAHaE24oXn/q82hQznfo/7fVO4lS/jS
bdhXla349DHUasopsXPcQYMzSSTb/pyERRxnlFzsPT+4dkIo63TK3BifEGo6/4Ge
JA+Ct138bQw3Ez1PhXU7pXI67C2kiL8qP9MVySkTUBr8oa52Ndu5yC0pVv4W95HA
WAwEdQw7jFerG+8N+PS8
-----END CERTIFICATE-----`) /*provide valid CA data to trigger https*/
	config.Host = "1.2.3.4"
	config.APIPath = "api/v1"

	url, err := GetProxyURL(config, "monitoring-namespace", "prometheus:http", "prometheus")
	require.NoError(t, err)
	require.Equal(t, "https://1.2.3.4/api/v1/namespaces/monitoring-namespace/services/prometheus:http/proxy/prometheus", url)
}
