package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfig_SigningKeyFromFileOrBase64(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 3
	private := ed25519.NewKeyFromSeed(seed)
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	path := filepath.Join(t.TempDir(), "signing.pem")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	publicDER, _ := x509.MarshalPKIXPublicKey(private.Public())
	publicPath := filepath.Join(t.TempDir(), "public.pem")
	if err := os.WriteFile(publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	rawPublic := base64.StdEncoding.EncodeToString(private.Public().(ed25519.PublicKey))

	for name, value := range map[string]string{
		"path":       path,
		"base64 pem": base64.StdEncoding.EncodeToString(encoded),
		"base64 der": base64.StdEncoding.EncodeToString(der),
	} {
		t.Run(name, func(t *testing.T) {
			environment := map[string]string{
				"GENESISDB_AUTH_TOKEN":           strings.Repeat("a", 32),
				"GENESISDB_SIGNING_KEY":          value,
				"GENESISDB_TRUSTED_SIGNING_KEYS": publicPath + ", " + rawPublic,
			}
			config, err := loadConfig(func(name string) (string, bool) { v, ok := environment[name]; return v, ok }, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			if config.Signer == nil || config.Signer.PublicKey().PublicKey != rawPublic {
				t.Fatal("wrong signing key")
			}
			if len(config.TrustedSigningKeys) != 2 || config.TrustedSigningKeys[0] != config.Signer.PublicKey() || config.TrustedSigningKeys[1] != config.Signer.PublicKey() {
				t.Fatalf("trusted keys %v", config.TrustedSigningKeys)
			}
		})
	}
}

func TestConfig_RevokedSigningKeys(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	lookup := func(environment map[string]string) func(string) (string, bool) {
		return func(name string) (string, bool) { v, ok := environment[name]; return v, ok }
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	config, err := loadConfig(lookup(map[string]string{
		"GENESISDB_AUTH_TOKEN":           strings.Repeat("a", 32),
		"GENESISDB_REVOKED_SIGNING_KEYS": "0123456789abcdef, fedcba9876543210:" + hash,
	}), logger)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.RevokedSigningKeys) != 2 || config.RevokedSigningKeys[1].ValidThroughHash != hash {
		t.Fatalf("%+v", config.RevokedSigningKeys)
	}
	for _, value := range []string{"nope", "0123456789abcdef:", "0123456789abcdef,0123456789abcdef"} {
		_, err := loadConfig(lookup(map[string]string{"GENESISDB_AUTH_TOKEN": strings.Repeat("a", 32), "GENESISDB_REVOKED_SIGNING_KEYS": value}), logger)
		if err == nil || !strings.Contains(err.Error(), "GENESISDB_REVOKED_SIGNING_KEYS") {
			t.Errorf("%q: %v", value, err)
		}
	}
}

// Regression: a mistyped key path was reported as "neither a readable file
// nor base64", which did not say that the file was missing.
func TestConfig_SigningKeyErrorsNameTheProblem(t *testing.T) {
	directory := t.TempDir()
	for value, expected := range map[string]string{
		"/run/secrets/signing.pem": "key file /run/secrets/signing.pem does not exist",
		"./signing.pem":            "key file ./signing.pem does not exist",
		"signing.pem":              "key file signing.pem does not exist",
		directory:                  directory + " is not a regular file",
		"%%%":                      "neither the path of an existing file nor valid base64",
	} {
		_, err := loadSigningKey(value)
		if err == nil || !strings.Contains(err.Error(), expected) {
			t.Errorf("%q: %v", value, err)
		}
	}
	_, err := loadTrustedSigningKeys("/missing/public.pem")
	if err == nil || !strings.Contains(err.Error(), "key file /missing/public.pem does not exist") {
		t.Errorf("trusted keys: %v", err)
	}
}

func TestConfig_InvalidSigningKeyStopsTheStart(t *testing.T) {
	for _, value := range []string{"/does/not/exist", "bm90IGEga2V5", "%%%"} {
		environment := map[string]string{"GENESISDB_AUTH_TOKEN": strings.Repeat("a", 32), "GENESISDB_SIGNING_KEY": value}
		_, err := loadConfig(func(name string) (string, bool) { v, ok := environment[name]; return v, ok }, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err == nil || !strings.Contains(err.Error(), "GENESISDB_SIGNING_KEY") {
			t.Errorf("%q: %v", value, err)
		}
		// The key material must never appear in the error.
		if err != nil && value != "/does/not/exist" && strings.Contains(err.Error(), value) {
			t.Errorf("error leaks the key value: %v", err)
		}
	}
	environment := map[string]string{"GENESISDB_AUTH_TOKEN": strings.Repeat("a", 32)}
	config, err := loadConfig(func(name string) (string, bool) { v, ok := environment[name]; return v, ok }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || config.Signer != nil {
		t.Fatalf("signing must be off by default: %v", err)
	}
}
