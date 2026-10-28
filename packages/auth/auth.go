// Package auth checks the bearer token of HTTP requests.
package auth

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

// ErrMissingToken reports a request without an Authorization header.
var ErrMissingToken = errors.New("no token provided")

// ErrInvalidToken reports a token that does not match.
var ErrInvalidToken = errors.New("invalid token")

// TokenVerifier compares bearer tokens with the configured token in constant
// time.
type TokenVerifier struct {
	expectedToken []byte
}

// NewTokenVerifier returns a verifier for token. Surrounding spaces are
// ignored.
func NewTokenVerifier(token string) *TokenVerifier {
	return &TokenVerifier{expectedToken: []byte(strings.TrimSpace(token))}
}

// VerifyToken checks an Authorization header value. "Bearer " is optional
// and case insensitive.
func (verifier *TokenVerifier) VerifyToken(header string) error {
	token := strings.TrimSpace(header)
	if token == "" {
		return ErrMissingToken
	}
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		token = strings.TrimSpace(token[len("bearer "):])
	}
	if len(verifier.expectedToken) == 0 || subtle.ConstantTimeCompare([]byte(token), verifier.expectedToken) != 1 {
		return ErrInvalidToken
	}
	return nil
}

// RequireBearerToken rejects requests without a valid token with 401.
// OPTIONS requests pass through for CORS preflight.
func (verifier *TokenVerifier) RequireBearerToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodOptions {
			next.ServeHTTP(writer, request)
			return
		}
		if err := verifier.VerifyToken(request.Header.Get("Authorization")); err != nil {
			message := "Invalid token"
			if errors.Is(err, ErrMissingToken) {
				message = "No token provided"
			}
			http.Error(writer, message, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(writer, request)
	})
}
