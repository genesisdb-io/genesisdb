package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifyToken(t *testing.T) {
	verifier := NewTokenVerifier(" secret ")
	for _, header := range []string{"Bearer secret", "bearer secret", "secret", "  Bearer   secret  "} {
		if err := verifier.VerifyToken(header); err != nil {
			t.Errorf("%q rejected: %v", header, err)
		}
	}
	if err := verifier.VerifyToken(""); !errors.Is(err, ErrMissingToken) {
		t.Errorf("empty header: %v", err)
	}
	if err := verifier.VerifyToken("Bearer wrong"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("wrong token: %v", err)
	}
	if err := NewTokenVerifier("").VerifyToken("Bearer "); err == nil {
		t.Error("an empty configured token must never match")
	}
}

func TestRequireBearerToken(t *testing.T) {
	handler := NewTokenVerifier("secret").RequireBearerToken(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusTeapot)
	}))
	cases := []struct {
		method, header string
		status         int
	}{
		{http.MethodGet, "Bearer secret", http.StatusTeapot},
		{http.MethodGet, "", http.StatusUnauthorized},
		{http.MethodGet, "Bearer wrong", http.StatusUnauthorized},
		{http.MethodOptions, "", http.StatusTeapot},
	}
	for _, testCase := range cases {
		request := httptest.NewRequest(testCase.method, "/", nil)
		if testCase.header != "" {
			request.Header.Set("Authorization", testCase.header)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != testCase.status {
			t.Errorf("%s %q: %d", testCase.method, testCase.header, recorder.Code)
		}
	}
}
