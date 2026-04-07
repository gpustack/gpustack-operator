package peer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"

	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	libp2phost "github.com/libp2p/go-libp2p/core/host"
	libp2ppeer "github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"

	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// GeneratePrivateKey generates a private key from the given data.
func GeneratePrivateKey(seed string, seeds ...string) libp2pcrypto.PrivKey {
	// Hash the seed and the additional seeds.
	h := sha256.New()
	h.Write(stringx.ToBytes(&seed))
	for _, s := range seeds {
		h.Write(stringx.ToBytes(&s))
	}

	// Generate the private key.
	prvKey, _, _ := libp2pcrypto.GenerateEd25519Key(bytes.NewBuffer(h.Sum(nil)))
	return prvKey
}

// GenerateID generates a peer ID from the given private key.
func GenerateID(prvKey libp2pcrypto.PrivKey) libp2ppeer.ID {
	peerID, _ := libp2ppeer.IDFromPrivateKey(prvKey)
	return peerID
}

// GenerateIDFromSeed generates a peer ID from the given seed and additional seeds.
func GenerateIDFromSeed(seed string, seeds ...string) libp2ppeer.ID {
	prvKey := GeneratePrivateKey(seed, seeds...)
	return GenerateID(prvKey)
}

func SendJSONMessage(ctx context.Context, host libp2phost.Host, peerID libp2ppeer.ID, protocolID protocol.ID, message any) (err error) {
	stream, err := host.NewStream(ctx, peerID, protocolID)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = stream.Reset()
		}
		_ = stream.Close()
	}()

	if err = json.NewEncoder(stream).Encode(message); err != nil {
		return err
	}
	return nil
}
