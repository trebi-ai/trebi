package cloudlink

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/trebi-ai/trebi/internal/secrets"
)

// Identity is one device keypair plus the cloud-assigned device id. The
// private key is an Ed25519 seed kept in the secrets directory (0600); the
// device id lives only in the cloud_link row and never in the file.
type Identity struct {
	deviceID string
	key      ed25519.PrivateKey
}

const deviceKeySecret = "cloud_device_key"

// MintIdentity generates the device keypair and saves the seed. The device
// id arrives from Register afterwards.
func MintIdentity() (Identity, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return Identity{}, fmt.Errorf("cloud identity: %w", err)
	}
	if err := secrets.Set(deviceKeySecret, base64.StdEncoding.EncodeToString(seed)); err != nil {
		return Identity{}, fmt.Errorf("cloud identity: %w", err)
	}
	return Identity{key: ed25519.NewKeyFromSeed(seed)}, nil
}

// LoadIdentity reads the saved seed back for one device id.
func LoadIdentity(deviceID string) (Identity, error) {
	value, err := secrets.Get(deviceKeySecret)
	if err != nil {
		return Identity{}, fmt.Errorf("cloud identity: %w", err)
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(value)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return Identity{}, errors.New("cloud identity: stored key is not a 32-byte Ed25519 seed")
	}
	return Identity{deviceID: deviceID, key: ed25519.NewKeyFromSeed(seed)}, nil
}

// DeleteIdentity removes the saved seed.
func DeleteIdentity() error {
	if err := secrets.Delete(deviceKeySecret); err != nil {
		return fmt.Errorf("cloud identity: %w", err)
	}
	return nil
}

// DeviceID is the cloud-assigned id, empty before Register.
func (i Identity) DeviceID() string { return i.deviceID }

// PublicKeyB64 is the standard base64 of the 32 raw public-key bytes.
func (i Identity) PublicKeyB64() string {
	pub, ok := i.key.Public().(ed25519.PublicKey)
	if !ok {
		return ""
	}
	return base64.StdEncoding.EncodeToString(pub)
}

// Sign returns the standard base64 Ed25519 signature over payload.
func (i Identity) Sign(payload []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(i.key, payload))
}

// ForDevice returns the same keypair named with a device id.
func (i Identity) ForDevice(deviceID string) Identity {
	i.deviceID = deviceID
	return i
}
