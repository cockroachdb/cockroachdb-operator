package dbconsole_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	httptest "net/http/httptest"
	"net/url"
	"testing"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubenet"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubenet/dbconsole"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pgxsql"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/testenv"
	"github.com/stretchr/testify/require"
)

func TestIntegrationSecureClusterUI(t *testing.T) {
	testutils.MarkIntegrationTest(t)

	ctx := context.Background()
	env := testenv.New(t, testenv.MultiZoneConfig)

	cluster := testenv.CreateCrdbCluster(t, env, "test", &crdbv1beta1.CrdbClusterSpec{
		TLSEnabled: true,
		TenantHost: &crdbv1beta1.TenantHost{},
		Template: crdbv1beta1.CrdbNodeTemplate{
			Spec: crdbv1beta1.CrdbNodeSpec{
				Image: testenv.GetDefaultCockroachImage(),
			},
		},
	})
	ctl := env.DefaultCluster().NewCtl(cluster.Namespace)

	t.Run("system tenant", func(t *testing.T) {
		RunClusterUITest(t, ctl, kubenet.Options{
			TenantID: crdbv1beta1.TenantSystemID,
		})
	})

	t.Run("system tenant with username/password", func(t *testing.T) {
		// First create SQL user by tunneling directly to CRDB pod.
		testenv.ExecKubeSql(t, ctl, kubenet.Options{
			TenantID: crdbv1beta1.TenantSystemID,
		}, "CREATE USER IF NOT EXISTS me WITH PASSWORD 'password'")

		RunClusterUITest(t, ctl, kubenet.Options{
			TenantID: crdbv1beta1.TenantSystemID,
			User:     url.UserPassword("me", "password"),
		})
	})

	t.Run("tenant", func(t *testing.T) {
		const tenantID = crdbv1beta1.TenantID(11)
		tenant := testenv.CreateCrdbTenant(t, ctl, "no-proxy", &crdbv1beta1.CrdbTenantSpec{
			TenantID: tenantID,
			// Explicitly set Pods to 0 as kubesql will automatically trigger
			// resumption.
			Pods: 0,
		})

		// May need to retry a few times since tenant resumption can take O(100ms)
		// to complete.
		RunClusterUITest(t, ctl, kubenet.Options{
			TenantID: tenant.Spec.TenantID,
		})

		require.NoError(t, ctl.Delete(ctx, tenant))
	})
}

func RunClusterUITest(t *testing.T, ctl *kube.Ctl, options kubenet.Options) {
	client, err := dbconsole.NewDBConsoleClient(ctl, options)
	require.NoError(t, err)

	// dbConsoleRoundTripper is unexported but exposes GetTransport so tests
	// can modify HTTP settings. Use an anonymous interface type assertion to
	// gain access to it.
	client.Transport.(interface {
		GetTransport() *http.Transport
	}).GetTransport().DisableKeepAlives = true

	// Assert that we can reach the /_status/statements page which is served on
	// both tenants and hosts. The scheme and host are replaced by the
	// underlying client and may be any valid scheme and host.
	resp, err := client.Get("https://doesnotmatter/_status/sessions")
	require.NoError(t, err)

	defer resp.Body.Close()

	require.Equal(t, 200, resp.StatusCode)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
}

func TestIntegrationUnifiedArchitectureProxy(t *testing.T) {
	testutils.MarkIntegrationTest(t)

	env := testenv.New(t, testenv.SingleZoneConfig)

	cluster := testenv.CreateCrdbCluster(t, env, "test", &crdbv1beta1.CrdbClusterSpec{
		TLSEnabled: true,
		TenantHost: &crdbv1beta1.TenantHost{},
		Template: crdbv1beta1.CrdbNodeTemplate{
			Spec: crdbv1beta1.CrdbNodeSpec{
				Image: testenv.GetDefaultCockroachImage(),
			},
		},
	})
	ctl := env.DefaultCluster().NewCtl(cluster.Namespace)

	// Here, we have not yet set up an application tenant; there is only a
	// system one. Connecting to the cluster and making a request should
	// just work.
	client, err := dbconsole.NewDBConsoleClient(ctl, kubenet.Options{})
	require.NoError(t, err)
	resp, err := client.Get("https://doesnotmatter/_admin/v1/jobs")
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)
	resp.Body.Close()

	applicationClusterName := "application-1234"

	// Start up an application virtual cluster and make it the default.
	testenv.ExecKubeSql(t, ctl, kubenet.Options{},
		fmt.Sprintf("CREATE VIRTUAL CLUSTER '%s'", applicationClusterName))
	testenv.ExecKubeSql(t, ctl, kubenet.Options{},
		fmt.Sprintf("ALTER VIRTUAL CLUSTER '%s' START SERVICE SHARED", applicationClusterName))
	testenv.ExecKubeSql(t, ctl, kubenet.Options{},
		fmt.Sprintf("SET CLUSTER SETTING server.controller.default_target_cluster = '%s'",
			applicationClusterName))

	options := kubenet.Options{
		IsUnifiedArchitecture:  true,
		ApplicationClusterName: applicationClusterName,
	}
	client, err = dbconsole.NewDBConsoleClient(ctl, options)
	require.NoError(t, err)

	// We are routing to a cluster that is specified to be on unified
	// architecture, but we do not provide a tenant to connect to in this
	// request, so we get an error.
	_, err = client.Get("https://doesnotmatter/_admin/v1/jobs")
	require.Error(t, err)
	require.ErrorContains(t, err, "unsupported tenant for unified architecture: \"\"")

	// Explicitly specify in a header that we want to connect to the
	// application virtual cluster.
	req, err := http.NewRequest("GET", "https://doesnotmatter/_admin/v1/jobs", nil)
	req.Header.Add(dbconsole.AuthClusterHeader, "application")
	require.NoError(t, err)

	resp, err = client.Do(req)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)
	resp.Body.Close()

	// Specifying that we want to connect to the system cluster just works
	// as well.
	req, err = http.NewRequest("GET", "https://doesnotmatter/_admin/v1/jobs", nil)
	req.Header.Add(dbconsole.AuthClusterHeader, "system")
	require.NoError(t, err)

	resp, err = client.Do(req)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)
	resp.Body.Close()
}

func TestMaybeRefreshCookie(t *testing.T) {
	defer testutils.HookGlobal(&kubenet.OpenDB,
		func(context.Context, *kube.Ctl, kubenet.Options) (*pgxsql.DB, error) {
			return &pgxsql.DB{}, nil
		},
	)()

	calledWithReadOnly := &stack[bool]{}
	defer testutils.HookGlobal(&dbconsole.MintClusterUISession,
		func(_ context.Context, _ *sql.DB, _ string, _ time.Time, tenant string, readOnlyCluster bool) (*http.Cookie, error) {

			calledWithReadOnly.Push(readOnlyCluster)
			cookieValue := "secret"
			if tenant != "" {
				cookieValue = fmt.Sprintf("secret,%s", tenant)
			}
			return &http.Cookie{Value: cookieValue}, nil
		},
	)()

	r := dbconsole.DBConsoleRoundTripper{
		Options: kubenet.Options{
			ApplicationClusterName: "application",
			IsUnifiedArchitecture:  true,
			IsStandbyCluster:       false,
		},
	}

	cookie, err := r.MaybeRefreshCookie(context.Background(), "")
	require.Nil(t, cookie)
	require.Error(t, err)
	require.ErrorContains(t, err, "unsupported tenant for unified architecture: \"\"")

	cookie, err = r.MaybeRefreshCookie(context.Background(), "application")
	require.NotNil(t, cookie)
	require.Equal(t, "secret,application", cookie.Value)
	require.NoError(t, err)
	require.Equal(t, calledWithReadOnly.Len(), 1)
	require.False(t, calledWithReadOnly.Pop())

	cookie, err = r.MaybeRefreshCookie(context.Background(), "system")
	require.NotNil(t, cookie)
	require.Equal(t, "secret,system,secret,application", cookie.Value)
	require.NoError(t, err)
	require.Equal(t, calledWithReadOnly.Len(), 2)
	require.False(t, calledWithReadOnly.Pop()) // application session
	require.False(t, calledWithReadOnly.Pop()) // system session

	r.Options.IsStandbyCluster = true
	cookie, err = r.MaybeRefreshCookie(context.Background(), "application")
	require.NotNil(t, cookie)
	require.Equal(t, "secret,application", cookie.Value)
	require.NoError(t, err)
	require.Equal(t, calledWithReadOnly.Len(), 1)
	require.True(t, calledWithReadOnly.Pop())

	cookie, err = r.MaybeRefreshCookie(context.Background(), "system")
	require.NotNil(t, cookie)
	require.Equal(t, "secret,system,secret,application", cookie.Value)
	require.NoError(t, err)
	require.Equal(t, calledWithReadOnly.Len(), 2)
	require.True(t, calledWithReadOnly.Pop())  // application session
	require.False(t, calledWithReadOnly.Pop()) // system session
}

func TestDbConsoleRoundTripper(t *testing.T) {
	t.Run("GET success", func(t *testing.T) {
		cookies := []*http.Cookie{{Value: "secret"}}
		defer setupStubs(func(i int) (*http.Cookie, error) {
			// Only one session should be minted
			if i >= 1 {
				t.Fail()
			}
			return cookies[i], nil

		})()

		i := 0
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			// Server should only receive one request
			if i >= 1 {
				t.Fail()
			}
			writer.WriteHeader(200)
			writer.Write([]byte("success"))
			i++
		}))
		defer server.Close()

		rt := makeClient(t, server.URL)
		resp, err := rt.Get(server.URL)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	})
	t.Run("POST success", func(t *testing.T) {
		cookies := []*http.Cookie{{Value: "secret"}}
		jsonBody := `{"someField": 1}`
		defer setupStubs(func(i int) (*http.Cookie, error) {
			// Only one session should be minted
			if i >= 1 {
				t.Fail()
			}
			return cookies[i], nil

		})()

		i := 0
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			defer request.Body.Close()
			// Server should only receive one request
			if i >= 1 {
				t.Fail()
			}
			body, err := io.ReadAll(request.Body)
			require.NoError(t, err)
			require.Equal(t, jsonBody, string(body))
			writer.WriteHeader(200)
			writer.Write([]byte("success"))
			i++
		}))
		defer server.Close()

		rt := makeClient(t, server.URL)
		resp, err := rt.Post(server.URL, "application/json", bytes.NewBuffer([]byte(jsonBody)))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	})
	t.Run("error 401 unauthorized retries GET successfully", func(t *testing.T) {
		cookies := []*http.Cookie{{Value: "secret"}, {Value: "newSecret"}}
		defer setupStubs(func(i int) (*http.Cookie, error) {
			return cookies[i], nil

		})()

		i := 0
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			responses := []func(w http.ResponseWriter){
				func(w http.ResponseWriter) { http.Error(w, "OOPS", http.StatusUnauthorized) },
				func(w http.ResponseWriter) {
					writer.WriteHeader(200)
					writer.Write([]byte("success"))
				},
			}
			responses[i](writer)
			i++
		}))
		defer server.Close()

		rt := makeClient(t, server.URL)
		resp, err := rt.Get(server.URL)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, i, 2)
	})
	t.Run("error 401 unauthorized retries POST successfully", func(t *testing.T) {
		cookies := []*http.Cookie{{Value: "secret"}, {Value: "newSecret"}}
		jsonBody := `{"someField": 1}`
		defer setupStubs(func(i int) (*http.Cookie, error) {
			return cookies[i], nil

		})()

		i := 0
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			responses := []func(w http.ResponseWriter){
				func(w http.ResponseWriter) { http.Error(w, "OOPS", http.StatusUnauthorized) },
				func(w http.ResponseWriter) {
					body, err := io.ReadAll(request.Body)
					require.NoError(t, err)
					require.Equal(t, jsonBody, string(body))
					writer.WriteHeader(200)
					writer.Write([]byte("success"))
				},
			}
			responses[i](writer)
			i++
		}))
		defer server.Close()

		rt := makeClient(t, server.URL)
		resp, err := rt.Post(server.URL, "application/json", bytes.NewBuffer([]byte(jsonBody)))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, i, 2)
	})
	t.Run("error non 401 unauthorized does not retry", func(t *testing.T) {
		cookies := []*http.Cookie{{Value: "secret"}}
		defer setupStubs(func(i int) (*http.Cookie, error) {
			// Only one session should be minted
			if i >= 1 {
				t.Fail()
			}
			return cookies[i], nil
		})()

		i := 0
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			// Server should only receive one request
			if i >= 1 {
				t.Fail()
			}
			http.Error(writer, "OOPS", http.StatusNotFound)
			i++
		}))
		defer server.Close()
		rt := makeClient(t, server.URL)
		resp, err := rt.Get(server.URL)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
		require.Equal(t, i, 1)
	})
}

func makeClient(t *testing.T, u string) http.Client {
	serverUrl, err := url.Parse(u)
	require.NoError(t, err)
	return http.Client{
		Transport: &dbconsole.DBConsoleRoundTripper{
			Host:   serverUrl.Host,
			Scheme: serverUrl.Scheme,
		},
	}
}

func setupStubs(mintCookieStub func(inv int) (*http.Cookie, error)) func() {
	invocationNum := 0
	resetOpenDb := testutils.HookGlobal(&kubenet.OpenDB,
		func(context.Context, *kube.Ctl, kubenet.Options) (*pgxsql.DB, error) {
			return &pgxsql.DB{}, nil
		},
	)

	resetMintClusterUISession := testutils.HookGlobal(&dbconsole.MintClusterUISession,
		func(_ context.Context, _ *sql.DB, _ string, _ time.Time, _ string, _ bool) (*http.Cookie, error) {
			c, err := mintCookieStub(invocationNum)
			invocationNum++
			return c, err
		})

	return func() {
		resetOpenDb()
		resetMintClusterUISession()
	}
}

type stack[T any] struct {
	keys []T
}

func (stack *stack[T]) Push(key T) {
	stack.keys = append(stack.keys, key)
}

func (stack *stack[T]) Len() int {
	return len(stack.keys)
}

func (stack *stack[T]) Pop() T {
	var x T
	if stack.Len() == 0 {
		panic("attempting to pop from empty stack")
	}

	x, stack.keys = stack.keys[len(stack.keys)-1], stack.keys[:len(stack.keys)-1]
	return x
}
