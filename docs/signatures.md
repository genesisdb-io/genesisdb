# Event signatures

The hash chain proves that the event log is consistent. A signature proves which key committed an event. With a signing key configured, GenesisDB signs every new event with Ed25519 at commit and stores the signature in the event record. Signing is off by default.

## Configuration

```sh
openssl genpkey -algorithm ED25519 -out signing.pem
openssl pkey -in signing.pem -pubout -out signing.pub.pem

export GENESISDB_SIGNING_KEY=/run/secrets/signing.pem          # a path
export GENESISDB_SIGNING_KEY="$(base64 < signing.pem | tr -d '\n')"  # or the file content as base64
```

| Variable | Default | Meaning |
|---|---|---|
| `GENESISDB_SIGNING_KEY` | disabled | Ed25519 private key in PKCS#8 (PEM or DER), as a file path or the file content in base64 |
| `GENESISDB_TRUSTED_SIGNING_KEYS` | recorded keys | Comma separated public keys the audit trusts: file paths (PEM or DER), raw 32 byte keys in base64, or PEM/DER files in base64 |

An invalid key stops the start. The key value is never logged, the log shows the key ID only.

## What is signed

A signature value has the form

```text
genesisdb:signature:v1:<key id>:<signature>
```

- `<key id>`: the first 16 hex digits of SHA-256 over the raw 32 byte public key.
- `<signature>`: the lowercase hex Ed25519 signature over the bytes of `genesisdb:signature:v1:` followed by the `hash` field of the event, taken as text exactly as it appears.

The hash binds metadata, payload, reference key and predecessor (see [storage format](storage-format.md#hash-chain)), so the signature covers all of them. Reference payloads lie outside the hash: erasing them leaves both hash and signature valid.

Signatures are verifiable with standard tools, without GenesisDB:

```sh
printf 'genesisdb:signature:v1:%s' "$HASH" > message
echo "$SIGNATURE_HEX" | xxd -r -p > signature
openssl pkeyutl -verify -pubin -inkey signing.pub.pem -rawin -in message -sigfile signature
```

Verify the hash before the signature: a valid signature over a hash that does not match the event proves nothing about the event.

## Keys and rotation

The public key of every key that committed is recorded in `signing-keys.json` in the data directory before the first signed write. Signatures are persisted, so a rotation does not touch history: old events keep the signature of the key that committed them and stay verifiable. To rotate, restart with the new key. Unsigned events, for example those committed before signing was enabled, stay unsigned.

`GET /api/v2/signing-keys` (read token) lists the recorded keys and the active key ID:

```json
{"active":"c0e1c5b3695965d2","keys":[{"keyId":"c0e1c5b3695965d2","publicKey":"SOBdrYA/9nnygFDEUxEfVRWlG+/Js9GveRfGAYKGcTQ="}]}
```

## Trust

`signing-keys.json` lives in the data directory. Whoever can write the directory can add a key and sign a rewritten history with it. The recorded keys therefore detect damage and identify keys, they do not establish trust. Pin the keys you trust outside the directory with `GENESISDB_TRUSTED_SIGNING_KEYS` or `genesisdb verify --trusted-keys`: signatures of any other key are then reported as signed by an untrusted key. Keep the private key off the database host's data volume, in a secret store, and with access for the GenesisDB process only.

A signature does not prove that an event is complete: removing the newest events is not detected by signatures alone. Keep external checkpoints (ID and hash of the latest event) if truncation matters.

## Verification

`GET /api/v2/admin/audit` checks the hash chain and every signature against the trusted keys, or the recorded keys if none are pinned. An invalid signature fails the audit. On stores with signatures the answer adds a line:

```text
Audit passed. All 2 events integrity checks succeeded.
Signatures: 2 valid, 0 unsigned, 0 signed by an untrusted key.
```

`genesisdb verify` performs the same check on a stopped data directory without changing a byte: no write-ahead log replay, no migration, no index rebuild.

```sh
genesisdb verify --data-dir data --trusted-keys signing.pub.pem --require-signatures
```

Exit code 0 means valid, 1 an integrity problem (or with `--require-signatures` an unsigned or untrusted event), 2 a usage or read error.

## Compatibility

- Without a key nothing changes: records, responses, backups and the audit answer are byte for byte the same.
- `/api/v1` is frozen and never returns signatures. `/api/v2` returns `signature` on signed events and omits it on unsigned ones.
- Directories kept in the 1.x format (`GENESISDB_STORAGE_FORMAT=keep`) are never signed, so 1.x can still read them. A 1.x directory migrated on start keeps its history unsigned and signs new events.
- Backups contain the signatures and the recorded keys (`signingKeys`). A restore keeps a signature when its event hash is unchanged. A signed event whose hash changes (for example a backup restored into a store of another hash version) is signed again with the configured key, or loses its signature without one. Unsigned events are never signed by a restore, it does not claim that a key committed history it did not sign.

## Performance

Without a key the commit path is unchanged. With a key, each event costs one Ed25519 signature (about 30 microseconds on an Apple M1 Pro). Batches of 32 events and more are signed on all cores, a batch of 500 adds about 3 ms CPU time, small batches stay far below the device flush of a commit. The audit verifies signatures on all cores while it scans the log.
