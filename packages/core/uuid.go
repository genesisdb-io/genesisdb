package core

import (
	"crypto/rand"
	"encoding/hex"
)

// NewUUID returns a random RFC 4122 version 4 UUID in its canonical lowercase
// form, for example "1c4e7c1e-3b1a-4b9e-9f43-2d5f1f0a6c11". GenesisDB uses
// it for event IDs, index builds and commit groups.
func NewUUID() string {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic("core: reading random bytes failed: " + err.Error())
	}
	random[6] = (random[6] & 0x0f) | 0x40
	random[8] = (random[8] & 0x3f) | 0x80

	var text [36]byte
	hex.Encode(text[0:8], random[0:4])
	text[8] = '-'
	hex.Encode(text[9:13], random[4:6])
	text[13] = '-'
	hex.Encode(text[14:18], random[6:8])
	text[18] = '-'
	hex.Encode(text[19:23], random[8:10])
	text[23] = '-'
	hex.Encode(text[24:], random[10:])
	return string(text[:])
}
