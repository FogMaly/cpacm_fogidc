package auth

import cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"

// CodexRequestProtocol returns the logical protocol requested for a Codex route.
func CodexRequestProtocol(opts cliproxyexecutor.Options) string {
	return codexRequestProtocol(opts)
}

// CodexAuthSupportsProtocol reports whether the auth can serve the requested protocol.
func CodexAuthSupportsProtocol(auth *Auth, protocol string) bool {
	return codexAuthSupportsProtocol(auth, protocol)
}

// CodexAuthSupportsNativeResponses reports whether the auth should use native Responses.
func CodexAuthSupportsNativeResponses(auth *Auth) bool {
	return codexAuthSupportsNativeResponses(auth)
}

// CodexAuthExplicitlySupportsResponses reports whether explicit protocol config enables Responses.
func CodexAuthExplicitlySupportsResponses(auth *Auth) bool {
	protocols, hasExplicit := codexExplicitProtocols(auth)
	return hasExplicit && protocols["responses"]
}
