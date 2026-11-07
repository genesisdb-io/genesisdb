package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/store"
)

// loadSigningKey reads the Ed25519 private key of GENESISDB_SIGNING_KEY.
// The value is either a path to a PKCS#8 PEM or DER file, or the content of
// such a file encoded in base64, which suits secret stores that inject
// environment variables. An empty value disables signing.
func loadSigningKey(value string) (*core.EventSigner, error) {
	if value == "" {
		return nil, nil
	}
	content, err := readKeyValue(value)
	if err != nil {
		return nil, err
	}
	return core.ParseSigningKey(content)
}

// loadTrustedSigningKeys reads GENESISDB_TRUSTED_SIGNING_KEYS: a comma
// separated list of public keys, each a path to a PEM or DER file, a base64
// raw 32 byte key, or a base64 encoded PEM or DER file.
func loadTrustedSigningKeys(value string) ([]core.SigningPublicKey, error) {
	var keys []core.SigningPublicKey
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		content, err := readKeyValue(item)
		if err != nil {
			return nil, err
		}
		key, err := core.ParseSigningPublicKey(content)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// loadRevokedSigningKeys reads GENESISDB_REVOKED_SIGNING_KEYS: a comma
// separated list of "<key id>" (every signature of the key is revoked) or
// "<key id>:<event hash>" (signatures up to and including that event stay
// trusted).
func loadRevokedSigningKeys(value string) ([]core.SigningKeyRevocation, error) {
	var revocations []core.SigningKeyRevocation
	for _, item := range strings.Split(value, ",") {
		if strings.TrimSpace(item) == "" {
			continue
		}
		revocation, err := core.ParseSigningKeyRevocation(item)
		if err != nil {
			return nil, err
		}
		revocations = append(revocations, revocation)
	}
	if _, err := core.NewSigningKeyRevocations(revocations); err != nil {
		return nil, err
	}
	return revocations, nil
}

// runVerify implements "genesisdb verify". Exit code 0 means the chain and
// every signature verified, 1 an integrity problem, 2 a usage or read error.
// With -require-signatures, unsigned events and events signed by an
// untrusted key fail the check as well.
func runVerify(arguments []string, logger *slog.Logger) int {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	dataDirectory := flags.String("data-dir", os.Getenv("GENESISDB_DATA_DIR"), "stopped data directory to verify (default: GENESISDB_DATA_DIR or ./data)")
	trusted := flags.String("trusted-keys", os.Getenv("GENESISDB_TRUSTED_SIGNING_KEYS"), "comma separated trusted public keys, files or base64 (default: the keys recorded in the directory)")
	revoked := flags.String("revoked-keys", os.Getenv("GENESISDB_REVOKED_SIGNING_KEYS"), "comma separated revoked keys, <key id> or <key id>:<last trusted event hash>")
	requireSignatures := flags.Bool("require-signatures", false, "fail unless every event is signed by a trusted, unrevoked key")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *dataDirectory == "" {
		*dataDirectory = "data"
	}
	keys, err := loadTrustedSigningKeys(*trusted)
	if err != nil {
		logger.Error("invalid trusted keys", "error", err)
		return 2
	}
	revocations, err := loadRevokedSigningKeys(*revoked)
	if err != nil {
		logger.Error("invalid revoked keys", "error", err)
		return 2
	}
	report, err := store.VerifyStoppedDirectory(context.Background(), *dataDirectory, keys, revocations)
	if err != nil {
		logger.Error("verification could not run", "error", err)
		return 2
	}
	fmt.Printf("Events: %d (%d with legacy hashes)\n", report.CheckedEvents, report.LegacyEvents)
	fmt.Printf("Signatures: %d valid, %d unsigned, %d signed by an untrusted key, %d signed by a revoked key\n", report.SignedEvents, report.UnsignedEvents, report.UnknownKeyEvents, report.RevokedEvents)
	system := report.SystemSignatures
	fmt.Printf("System event signatures: %d valid, %d unsigned, %d signed by an untrusted key, %d signed by a revoked key\n", system.Signed, system.Unsigned, system.UnknownKey, system.Revoked)
	if !report.IsTrustPinned {
		fmt.Println("Warning: no --trusted-keys given, signatures were verified against the keys recorded in the directory, which a writer of the directory can extend")
	}
	if !report.IsValid() {
		fmt.Println("FAILED: " + report.Problem)
		return 1
	}
	if *requireSignatures && !report.IsTrustPinned {
		fmt.Println("FAILED: --require-signatures needs --trusted-keys, the keys recorded in the directory prove nothing")
		return 1
	}
	if *requireSignatures && (report.UnsignedEvents > 0 || report.UnknownKeyEvents > 0 || report.RevokedEvents > 0 || system.Unsigned > 0 || system.UnknownKey > 0 || system.Revoked > 0) {
		fmt.Println("FAILED: not every event is signed by a trusted, unrevoked key")
		return 1
	}
	fmt.Println("OK")
	return 0
}

// looksLikePath reports whether a key value was meant as a file path. Base64
// key material starts with letters ("LS0t" for PEM, "MC" for DER), so a
// leading path element or a key file extension identifies a path.
func looksLikePath(value string) bool {
	for _, prefix := range []string{"/", "./", "../", "~"} {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	for _, extension := range []string{".pem", ".der", ".key", ".pub"} {
		if strings.HasSuffix(strings.ToLower(value), extension) {
			return true
		}
	}
	return false
}

// readKeyValue returns the file content when value names an existing file,
// otherwise the base64 decoded value. A raw base64 public key is returned
// as text, the key parser accepts it directly.
//
// Errors name the path when the value looks like one, so a mistyped path is
// recognizable. They never contain a value that may be key material.
func readKeyValue(value string) ([]byte, error) {
	info, statErr := os.Stat(value)
	switch {
	case statErr == nil && info.Mode().IsRegular():
		content, err := os.ReadFile(value)
		if err != nil {
			return nil, fmt.Errorf("%w: cannot read key file %s: %v", core.ErrInvalidSigningKey, value, err)
		}
		return content, nil
	case statErr == nil:
		return nil, fmt.Errorf("%w: %s is not a regular file", core.ErrInvalidSigningKey, value)
	case !errors.Is(statErr, os.ErrNotExist) && looksLikePath(value):
		return nil, fmt.Errorf("%w: cannot access key file %s: %v", core.ErrInvalidSigningKey, value, statErr)
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		if looksLikePath(value) {
			return nil, fmt.Errorf("%w: key file %s does not exist", core.ErrInvalidSigningKey, value)
		}
		return nil, fmt.Errorf("%w: the value is neither the path of an existing file nor valid base64", core.ErrInvalidSigningKey)
	}
	if len(decoded) == 32 {
		return []byte(value), nil
	}
	return decoded, nil
}
