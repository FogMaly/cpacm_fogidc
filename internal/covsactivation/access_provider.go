package covsactivation

import (
	"context"
	"net/http"
	"strings"

	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
)

func RegisterAccessProvider(service *Service) {
	if service == nil {
		sdkaccess.UnregisterProvider(AccessProviderName)
		return
	}
	sdkaccess.RegisterProvider(AccessProviderName, &accessProvider{service: service})
}

type accessProvider struct {
	service *Service
}

func (p *accessProvider) Identifier() string { return AccessProviderName }

func (p *accessProvider) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if p == nil || p.service == nil {
		return nil, sdkaccess.NewNotHandledError()
	}

	candidates := extractRequestCredentialCandidates(r)
	if len(candidates) == 0 {
		return nil, sdkaccess.NewNoCredentialsError()
	}

	for _, candidate := range candidates {
		authResult, ok := p.service.AuthenticateToken(candidate.value)
		if !ok {
			continue
		}
		return &sdkaccess.Result{
			Provider:  AccessProviderName,
			Principal: authResult.Token,
			Metadata: map[string]string{
				"source":       candidate.source,
				"product":      authResult.Product,
				"card_type":    authResult.CardType,
				"model_prefix": authResult.ModelPrefix,
			},
		}, nil
	}

	return nil, sdkaccess.NewInvalidCredentialError()
}

type credentialCandidate struct {
	value  string
	source string
}

func extractRequestCredentialCandidates(r *http.Request) []credentialCandidate {
	if r == nil {
		return nil
	}
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	authHeaderGoogle := strings.TrimSpace(r.Header.Get("X-Goog-Api-Key"))
	authHeaderAnthropic := strings.TrimSpace(r.Header.Get("X-Api-Key"))
	queryKey := ""
	queryAuthToken := ""
	if r.URL != nil {
		queryKey = strings.TrimSpace(r.URL.Query().Get("key"))
		queryAuthToken = strings.TrimSpace(r.URL.Query().Get("auth_token"))
	}
	if authHeader == "" && authHeaderGoogle == "" && authHeaderAnthropic == "" && queryKey == "" && queryAuthToken == "" {
		return nil
	}
	return []credentialCandidate{
		{value: extractBearerToken(authHeader), source: "authorization"},
		{value: authHeaderGoogle, source: "x-goog-api-key"},
		{value: authHeaderAnthropic, source: "x-api-key"},
		{value: queryKey, source: "query-key"},
		{value: queryAuthToken, source: "query-auth-token"},
	}
}

func extractBearerToken(header string) string {
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return header
	}
	return strings.TrimSpace(parts[1])
}
