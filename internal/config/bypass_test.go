package config

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPITokenIsAppliedOnlyToTheConfiguredHost(t *testing.T) {
	var publicQuery, localQuery string
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		publicQuery = r.URL.RawQuery
	}))
	defer public.Close()
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localQuery = r.URL.RawQuery
	}))
	defer local.Close()

	previous := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = previous })

	if err := InstallAPIToken(NetworkEnvironment{
		Network:        "fuji",
		PChainAPI:      public.URL,
		PChainAPIToken: "secret",
	}); err != nil {
		t.Fatal(err)
	}

	// The client overwrites RawQuery from its own options, so the token has to
	// survive a request that already carries a query string.
	response, err := http.DefaultClient.Get(public.URL + "/ext/P?existing=1")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if publicQuery != "existing=1&token=secret" {
		t.Fatalf("public query = %q, want the token merged with the existing query", publicQuery)
	}

	response, err = http.DefaultClient.Get(local.URL + "/ext/P")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if localQuery != "" {
		t.Fatalf("token leaked to a non-public host: %q", localQuery)
	}
}

func TestNoTokenLeavesTheDefaultClientAlone(t *testing.T) {
	previous := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = previous })
	http.DefaultClient.Transport = nil

	if err := InstallAPIToken(NetworkEnvironment{
		Network:   "fuji",
		PChainAPI: "https://api.avax-test.network",
	}); err != nil {
		t.Fatal(err)
	}
	if http.DefaultClient.Transport != nil {
		t.Fatal("an empty token must not install a transport")
	}
}

func TestPChainAPIRejectsAQueryString(t *testing.T) {
	_, err := parseNetworkEnvironment(".env", map[string]string{
		"NETWORK":    "fuji",
		"PCHAIN_API": "https://api.avax-test.network?token=secret",
	})
	if err == nil {
		t.Fatal("a token in PCHAIN_API must be rejected: the client overwrites RawQuery")
	}
}
