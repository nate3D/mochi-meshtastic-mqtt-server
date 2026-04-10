package meshtastic

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"

	generated "github.com/meshtastic/go/generated"
)

// defaultKey is the well-known 16-byte AES key used by the default Meshtastic
// channel (PSK = [0x01], i.e. base64 "AQ=="). This matches the default_key
// constant embedded in the Meshtastic firmware.
var defaultKey = []byte{
	0xd4, 0xf1, 0xbb, 0x3a, 0x20, 0x29, 0x07, 0x59,
	0xf0, 0xbc, 0xff, 0xab, 0xcf, 0x4e, 0x69, 0x01,
}

// ErrDecryptFailed is returned when a packet cannot be decrypted with the
// provided key (e.g. wrong PSK or corrupt payload).
var ErrDecryptFailed = errors.New("meshtastic: failed to decrypt mesh packet")

// expandPSK converts a raw PSK byte slice into a valid AES key.
// A single-byte PSK of [0x01] is treated as the Meshtastic default key.
// Other PSKs are zero-padded on the right to 16 bytes (AES-128) or left
// unchanged if already 16 or 32 bytes (AES-256).
func expandPSK(psk []byte) ([]byte, error) {
	if len(psk) == 1 && psk[0] == 0x01 {
		return defaultKey, nil
	}

	switch len(psk) {
	case 16, 32:
		return psk, nil
	}

	if len(psk) == 0 || len(psk) > 32 {
		return nil, errors.New("meshtastic: PSK must be 1, 16, or 32 bytes")
	}

	// Pad to 16 bytes with trailing zeros.
	key := make([]byte, 16)
	copy(key, psk)
	return key, nil
}

// buildNonce derives the 16-byte AES-CTR nonce from a MeshPacket.
// Layout: [packetID uint64 LE | fromNode uint64 LE]
// The uint32 fields are zero-extended to uint64 as the firmware does.
func buildNonce(pk *generated.MeshPacket) []byte {
	nonce := make([]byte, 16)
	binary.LittleEndian.PutUint64(nonce[0:8], uint64(pk.GetId()))
	binary.LittleEndian.PutUint64(nonce[8:16], uint64(pk.GetFrom()))
	return nonce
}

// TryDecrypt attempts to decrypt the encrypted payload of a MeshPacket using
// AES-128/256-CTR and returns the raw plaintext bytes on success.
// The caller is responsible for parsing the plaintext as a meshtastic.Data protobuf.
func TryDecrypt(pk *generated.MeshPacket, psk []byte) ([]byte, error) {
	ciphertext := pk.GetEncrypted()
	if len(ciphertext) == 0 {
		return nil, ErrDecryptFailed
	}

	key, err := expandPSK(psk)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	nonce := buildNonce(pk)
	stream := cipher.NewCTR(block, nonce)

	plaintext := make([]byte, len(ciphertext))
	stream.XORKeyStream(plaintext, ciphertext)

	return plaintext, nil
}
