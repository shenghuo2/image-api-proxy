package proxy

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

type keyVault struct {
	key [32]byte
}

func newKeyVault(adminKey string) keyVault {
	return keyVault{key: sha256.Sum256([]byte("novelai-api-proxy:key-vault:v1:" + adminKey))}
}

func (v keyVault) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(v.key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (v keyVault) seal(raw string) (string, error) {
	aead, err := v.aead()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, []byte(raw), nil)
	return "v1:" + base64.RawStdEncoding.EncodeToString(sealed), nil
}

func (v keyVault) open(value string) (string, error) {
	encoded, ok := strings.CutPrefix(value, "v1:")
	if !ok {
		return "", errors.New("unsupported key ciphertext")
	}
	data, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	aead, err := v.aead()
	if err != nil {
		return "", err
	}
	if len(data) < aead.NonceSize() {
		return "", errors.New("invalid key ciphertext")
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], nil)
	return string(plain), err
}
