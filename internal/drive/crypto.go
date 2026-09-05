package drive

import (
	"encoding/base64"

	"github.com/ProtonMail/gopenpgp/v2/crypto"
	"github.com/ProtonMail/gopenpgp/v2/helper"
)

// generateNodeKeys creates a fresh node key pair for a new file or folder: an x25519 private
// key plus its passphrase, encrypted to parentKR and signed by signKR, matching what the API
// expects in CreateFileReq/CreateFolderReq.NodeKey/NodePassphrase/NodePassphraseSignature.
func generateNodeKeys(parentKR, signKR *crypto.KeyRing) (nodeKey, nodePassphrase, nodePassphraseSig string, err error) {
	passphrase, err := generatePassphrase()
	if err != nil {
		return "", "", "", err
	}

	// all hardcoded values from Proton's own clients
	key, err := helper.GenerateKey("Drive key", "noreply@protonmail.com", []byte(passphrase), "x25519", 0)
	if err != nil {
		return "", "", "", err
	}

	encPassphrase, sig, err := encryptWithSignature(parentKR, signKR, []byte(passphrase))
	if err != nil {
		return "", "", "", err
	}

	return key, encPassphrase, sig, nil
}

func generatePassphrase() (string, error) {
	token, err := crypto.RandomToken(32)
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(token), nil
}

// encryptWithSignature encrypts b to kr and separately signs it with signKR, both armored.
func encryptWithSignature(kr, signKR *crypto.KeyRing, b []byte) (encArmored, sigArmored string, err error) {
	enc, err := kr.Encrypt(crypto.NewPlainMessage(b), nil)
	if err != nil {
		return "", "", err
	}

	encArmored, err = enc.GetArmored()
	if err != nil {
		return "", "", err
	}

	sig, err := signKR.SignDetached(crypto.NewPlainMessage(b))
	if err != nil {
		return "", "", err
	}

	sigArmored, err = sig.GetArmored()
	if err != nil {
		return "", "", err
	}

	return encArmored, sigArmored, nil
}

// unlockNodeKR unlocks a node key pair just produced by generateNodeKeys, the same way
// Link.GetKeyRing unlocks one fetched from the API.
func unlockNodeKR(parentKR, signKR *crypto.KeyRing, nodeKey, nodePassphrase, nodePassphraseSig string) (*crypto.KeyRing, error) {
	enc, err := crypto.NewPGPMessageFromArmored(nodePassphrase)
	if err != nil {
		return nil, err
	}

	dec, err := parentKR.Decrypt(enc, nil, crypto.GetUnixTime())
	if err != nil {
		return nil, err
	}

	sig, err := crypto.NewPGPSignatureFromArmored(nodePassphraseSig)
	if err != nil {
		return nil, err
	}

	if err := signKR.VerifyDetached(dec, sig, crypto.GetUnixTime()); err != nil {
		return nil, err
	}

	lockedKey, err := crypto.NewKeyFromArmored(nodeKey)
	if err != nil {
		return nil, err
	}

	unlockedKey, err := lockedKey.Unlock(dec.GetBinary())
	if err != nil {
		return nil, err
	}

	return crypto.NewKeyRing(unlockedKey)
}

// reencryptPassphrase re-wraps a node's passphrase key packet for a new parent keyring, used on
// Move. The signed data packet (and so the original NodePassphraseSignature) is untouched.
func reencryptPassphrase(oldParentKR, newParentKR *crypto.KeyRing, passphrase string) (string, error) {
	old, err := crypto.NewPGPSplitMessageFromArmored(passphrase)
	if err != nil {
		return "", err
	}

	sessionKey, err := oldParentKR.DecryptSessionKey(old.KeyPacket)
	if err != nil {
		return "", err
	}

	newKeyPacket, err := newParentKR.EncryptSessionKey(sessionKey)
	if err != nil {
		return "", err
	}

	return crypto.NewPGPSplitMessage(newKeyPacket, old.DataPacket).GetArmored()
}
