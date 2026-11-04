package main

import (
	"net/http"
	"strings"

	"github.com/heliodex/better-deploy/wt/packages/v2/oauth"
)

// newOAuthClient reads the provider settings from the environment. The defaults point at the public Duende demo, which accepts any redirect URI for its interactive.public client.
func newOAuthClient(httpClient *http.Client) (*oauth.Client, string, error) {
	client, err := oauth.New(oauth.Config{
		Issuer:       envDefault("OAUTH_ISSUER", "https://demo.duendesoftware.com"),
		ClientID:     envDefault("OAUTH_CLIENT_ID", "interactive.public"),
		ClientSecret: envDefault("OAUTH_CLIENT_SECRET", ""),
		Scopes:       strings.Fields(envDefault("OAUTH_SCOPES", "openid profile email")),
		HTTPClient:   httpClient,
	})
	if err != nil {
		return nil, "", err
	}

	return client, envDefault("OAUTH_PROVIDER_NAME", "Duende IdentityServer demo"), nil
}
