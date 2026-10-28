# github.com/genesisdb-io/genesisdb/v2/packages/auth

Bearer token verification for HTTP requests. Standard library only.

```sh
go get github.com/genesisdb-io/genesisdb/v2/packages/auth
```

```go
verifier := auth.NewTokenVerifier(os.Getenv("API_TOKEN"))

// Middleware: answers 401 without a valid token, OPTIONS passes for CORS preflight.
http.Handle("/api/", verifier.RequireBearerToken(apiHandler))

// Direct check of an Authorization header value.
switch err := verifier.VerifyToken(request.Header.Get("Authorization")); {
case errors.Is(err, auth.ErrMissingToken):
	// no header
case errors.Is(err, auth.ErrInvalidToken):
	// wrong token
}
```

- Tokens are compared in constant time.
- The `Bearer ` prefix is optional and case insensitive. Surrounding spaces are trimmed.
- A verifier created with an empty token rejects every request, so an unset token never opens access.
- The package checks a single token. Roles (administrator, read, write) are composed by `server/httpserver` from several verifiers.

The CLI requires tokens of at least 32 bytes. Embedding applications should enforce a comparable minimum, see [SECURITY.md](../../SECURITY.md).
