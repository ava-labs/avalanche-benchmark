package config

import (
	"net/http"
	"net/url"
	"path/filepath"
)

// InstallAPITokenFromRoot installs the bypass token declared in the workspace
// .env, if there is one. A missing or malformed .env is ignored here on
// purpose: every command that actually needs the file loads and validates it
// moments later and reports the real error there.
func InstallAPITokenFromRoot(root string) error {
	environment, err := LoadNetworkEnvironment(filepath.Join(root, ".env"))
	if err != nil {
		return nil
	}
	return InstallAPIToken(environment)
}

// InstallAPIToken makes every request to the public API host carry the
// rate-limit bypass token as a URI query argument.
//
// It has to happen in the HTTP transport rather than in the URL because
// AvalancheGo's rpc.SendJSONRequest assigns `uri.RawQuery` unconditionally
// from its own options, so a token baked into PCHAIN_API is silently dropped
// before the request is issued. Per-call rpc.WithQueryParam would reach the
// direct platformvm clients but not the wallet built by primary.MakePWallet,
// which constructs its own clients internally. Every one of those paths ends
// at http.DefaultClient, so that is the single place the token can be applied
// once and cover all of them.
//
// The token is matched by a Cloudflare rule on the query argument named
// "token", so a header would not work.
//
// Scoping to one host is deliberate: the fleet's own P-chain node is reached
// over plain HTTP and must never receive the secret.
func InstallAPIToken(environment NetworkEnvironment) error {
	if environment.PChainAPIToken == "" {
		return nil
	}
	parsed, err := url.Parse(environment.PChainAPI)
	if err != nil {
		return err
	}
	http.DefaultClient.Transport = tokenTransport{
		base:  http.DefaultTransport,
		host:  parsed.Host,
		token: environment.PChainAPIToken,
	}
	return nil
}

type tokenTransport struct {
	base  http.RoundTripper
	host  string
	token string
}

func (t tokenTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Host != t.host {
		return t.base.RoundTrip(request)
	}
	// RoundTrip must not modify the request it is given.
	request = request.Clone(request.Context())
	query := request.URL.Query()
	query.Set("token", t.token)
	request.URL.RawQuery = query.Encode()
	return t.base.RoundTrip(request)
}
