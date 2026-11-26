//go:build !shn_coreonly

package app

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
)

// The files a predecessor directory holds, as shn rotate keeps them: the
// manifest naming the holder and the replaced signing key. It holds no
// encryption key, and none is read.
const (
	predecessorManifestFile = "manifest.json"
	predecessorSignKeyFile  = "sign.key"
)

// loadPredecessor reads a predecessor directory: the holder id its manifest
// names and its ed25519 signing key.
func loadPredecessor(dir string) (string, ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(filepath.Join(dir, predecessorManifestFile))
	if err != nil {
		return "", nil, fmt.Errorf("read %s: %w", predecessorManifestFile, err)
	}
	var man struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &man); err != nil {
		return "", nil, fmt.Errorf("parse %s: %w", predecessorManifestFile, err)
	}
	if man.ID == "" {
		return "", nil, fmt.Errorf("%s names no holder id", predecessorManifestFile)
	}
	b64, err := os.ReadFile(filepath.Join(dir, predecessorSignKeyFile))
	if err != nil {
		return "", nil, fmt.Errorf("read %s: %w", predecessorSignKeyFile, err)
	}
	key, err := base64.StdEncoding.DecodeString(string(b64))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return "", nil, errors.New(predecessorSignKeyFile + ": not an ed25519 private key")
	}
	// An ed25519 private key is its seed and its public half: one whose
	// public half is not the seed's names a key other than the one its seed
	// makes, and the predecessor's key id would then name a key nothing
	// signed with.
	priv := ed25519.PrivateKey(key)
	if !bytes.Equal(ed25519.NewKeyFromSeed(priv.Seed()), priv) {
		return "", nil, errors.New(predecessorSignKeyFile + ": its public half is not its seed's")
	}
	return man.ID, priv, nil
}

// subscriptionKeys derives the gateway's topic Subscription keys from its
// identity and, when
// IDENTITY_PREDECESSOR_DIR is set, from the predecessor signing key shn
// rotate kept there, which is used only to open and verify what was made
// under it during the rotation's overlap. The gateway does not start when
// the predecessor cannot be read, names another holder, or holds the current
// signing key.
func subscriptionKeys(cfg config, bundle shnsdk.Bundle) (subscription.Keys, error) {
	pred, err := subscriptionPredecessor(cfg, bundle)
	if err != nil {
		return subscription.Keys{}, err
	}
	k, err := subscription.DeriveKeys(bundle.Identity.HolderID, bundle.Identity.SignPriv, pred)
	if err != nil {
		return subscription.Keys{}, fmt.Errorf("gateway: derive the subscription keys: %v", err)
	}
	return k, nil
}

// subscriptionPredecessor is the predecessor signing key IDENTITY_PREDECESSOR_DIR
// names, checked as subscriptionKeys says, or nil when the setting is unset.
// The engine derives the same keys from it (engine.Config.SubscriptionPredecessorKey).
func subscriptionPredecessor(cfg config, bundle shnsdk.Bundle) (ed25519.PrivateKey, error) {
	var pred ed25519.PrivateKey
	if dir := cfg.IdentityPredecessorDir; dir != "" {
		holder, key, err := loadPredecessor(dir)
		if err != nil {
			return nil, fmt.Errorf("gateway: IDENTITY_PREDECESSOR_DIR (%q) is not a predecessor directory: %v: set it to the directory shn rotate kept the replaced signing key in (<out>/predecessor), or unset it", dir, err)
		}
		if holder != bundle.Identity.HolderID {
			return nil, fmt.Errorf("gateway: IDENTITY_PREDECESSOR_DIR (%q) holds holder %q's key, and this gateway is holder %q: set it to this holder's predecessor directory, or unset it", dir, holder, bundle.Identity.HolderID)
		}
		if bytes.Equal(key.Public().(ed25519.PublicKey), bundle.Identity.SignPub) {
			return nil, fmt.Errorf("gateway: IDENTITY_PREDECESSOR_DIR (%q) holds this gateway's current signing key, not the one a rotation replaced: set it to the directory shn rotate kept the replaced signing key in, or unset it", dir)
		}
		pred = key
	}
	return pred, nil
}
