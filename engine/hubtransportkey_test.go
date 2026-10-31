package engine

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/nacl/box"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// unreadSoR is a system of record nothing reads: New refuses before it is
// called.
type unreadSoR struct{ SystemOfRecord }

// A gateway that mounts the Hub-facing inbound refuses to start with a Hub
// transport key that is not an Ed25519 public key: missing, short or long. A
// wrong key is a configuration error said at boot, never a refusal of every
// request the Hub sends.
func TestNew_AHubTransportKeyOfTheWrongLengthRefusesToStart(t *testing.T) {
	authzPub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, signPriv, _ := ed25519.GenerateKey(rand.Reader)
	encPub, encPriv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for name, key := range map[string]ed25519.PublicKey{
		"none":  nil,
		"short": authzPub[:ed25519.PublicKeySize-1],
		"long":  append(append(ed25519.PublicKey{}, authzPub...), 0),
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				r := recover()
				if msg, _ := r.(string); !strings.Contains(msg, "HubTransportPub") {
					t.Fatalf("New with a %s Hub transport key: %v, want a refusal to start naming HubTransportPub", name, r)
				}
			}()
			_, _ = New(Config{
				Role:            "payer",
				HolderID:        "payer",
				Identity:        shnsdk.Identity{HolderID: "payer", SignPriv: signPriv, EncPub: encPub, EncPriv: encPriv},
				AuthzURL:        "http://stub.test",
				AuthzPub:        authzPub,
				HubTransportPub: key,
				Reg:             shnsdk.NewRegistry(),
				Validator:       shnsdk.NewFakeValidator(),
				SoR:             unreadSoR{},
				Store:           NewMemStore(),
			})
		})
	}
}
