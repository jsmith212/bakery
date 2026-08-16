package cli

// Server and credential resolution.
//
// Two chains, both spelled out here so a single place answers "why did this
// command talk to THAT server, using THIS credential" -- everything else
// (Client.authorize, push.go, commands.go) just calls in.

// resolveServer implements the server precedence: --server > BAKERY_SERVER >
// credentials.json's default_server > http://localhost:8080.
//
// Kong has already folded BAKERY_SERVER into flagServer by the time this runs
// (config.CLI.Server carries `env:"BAKERY_SERVER"`, and Kong only consults the
// env var when the flag itself was not passed) -- so the first two links are
// ALREADY one value on entry here, and this only has to resolve the last two.
// It lives in Go rather than a Kong `default:""` tag because the third link
// needs a file read, which a struct tag cannot express.
func resolveServer(flagServer string, tokens *TokenStore) string {
	if flagServer != "" {
		return flagServer
	}

	if tokens != nil {
		if s, ok := tokens.DefaultServer(); ok {
			return s
		}
	}

	return "http://localhost:8080"
}

// resolveCacheCredential implements the /cache credential precedence (spec
// §5): --key flag / BAKERY_API_KEY env (already folded into flagKey by Kong,
// exactly like --server/BAKERY_SERVER above) > a stored project key for
// {server, "org/project"} > a stored org/robot token for {server, "org"} > a
// stored personal access token for {server} > the logged-in OIDC session
// bearer > ErrNeedsLogin.
//
// The last two links are NOT decided here: an empty Key already falls back to
// Client.authorize's bearer path today, and that path already raises
// ErrNeedsLogin on a cold cache -- see cache.go's authorize. Reproducing that
// logic here would be the second place it could drift from the first.
func resolveCacheCredential(tokens *TokenStore, server, org, project, flagKey string) cacheCredential {
	if flagKey != "" {
		return cacheCredential{Key: flagKey}
	}

	if tokens == nil {
		return cacheCredential{}
	}

	if key, ok := tokens.GetKey(server, org+"/"+project); ok {
		return cacheCredential{Key: key}
	}

	if key, ok := tokens.GetKey(server, org); ok {
		return cacheCredential{Key: key}
	}

	if tok, ok := tokens.GetUserToken(server); ok {
		return cacheCredential{Key: tok}
	}

	return cacheCredential{}
}
