package drive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"path"
	"time"

	"github.com/ProtonMail/gopenpgp/v2/crypto"
	proton "github.com/henrybear327/go-proton-api"
)

// CreateDir creates a new folder named name under parent.
func (c *Client) CreateDir(ctx context.Context, parent *Node, name string) error {
	nodeKey, nodePassphrase, nodePassphraseSig, err := generateNodeKeys(parent.KR, c.signKR)
	if err != nil {
		return err
	}

	req := proton.CreateFolderReq{
		ParentLinkID:            parent.Link.LinkID,
		SignatureAddress:        c.signEmail,
		NodeKey:                 nodeKey,
		NodePassphrase:          nodePassphrase,
		NodePassphraseSignature: nodePassphraseSig,
	}

	if err := req.SetName(name, c.signKR, parent.KR); err != nil {
		return err
	}

	parentHashKey, err := parent.Link.GetHashKey(parent.KR, c.addrKR)
	if err != nil {
		return err
	}

	if err := req.SetHash(name, parentHashKey); err != nil {
		return err
	}

	folderKR, err := unlockNodeKR(parent.KR, c.signKR, nodeKey, nodePassphrase, nodePassphraseSig)
	if err != nil {
		return err
	}

	if err := req.SetNodeHashKey(folderKR); err != nil {
		return err
	}

	_, err = c.api.CreateFolder(ctx, c.shareID, req)
	return err
}

// Move relocates and/or renames n from oldParent to newParent as newName.
func (c *Client) Move(ctx context.Context, n *Node, oldParent, newParent *Node, newName string) error {
	req := proton.MoveLinkReq{
		ParentLinkID:     newParent.Link.LinkID,
		OriginalHash:     n.Link.Hash,
		SignatureAddress: c.signEmail,
	}

	if err := req.SetName(newName, c.signKR, newParent.KR); err != nil {
		return err
	}

	newParentHashKey, err := newParent.Link.GetHashKey(newParent.KR, c.addrKR)
	if err != nil {
		return err
	}

	if err := req.SetHash(newName, newParentHashKey); err != nil {
		return err
	}

	nodePassphrase, err := reencryptPassphrase(oldParent.KR, newParent.KR, n.Link.NodePassphrase)
	if err != nil {
		return err
	}

	req.NodePassphrase = nodePassphrase
	req.NodePassphraseSignature = n.Link.NodePassphraseSignature

	return c.api.MoveLink(ctx, c.shareID, n.Link.LinkID, req)
}

// Trash moves n (a file or folder) into the trash. Proton trashes non-empty folders
// recursively, so callers may pass a directory Node with children.
func (c *Client) Trash(ctx context.Context, parent *Node, n *Node) error {
	return c.api.TrashChildren(ctx, c.shareID, parent.Link.LinkID, n.Link.LinkID)
}

// Upload streams r as a new file under parent (existing == nil) or as a new revision of
// existing, in blockSize plaintext chunks: each chunk is encrypted, signed and uploaded as one
// block, then the revision is committed with a signed manifest and XAttr metadata (size and
// modTime).  On any failure after the link/revision was created, the partial upload is
// best-effort cleaned up.
func (c *Client) Upload(ctx context.Context, parent *Node, name string, existing *Node, r io.Reader, size int64, modTime time.Time) error {
	newFile := existing == nil

	linkID, revisionID, nodeKR, sessionKey, err := c.startRevision(ctx, parent, name, existing)
	if err != nil {
		return err
	}

	blockSizes, hashes, err := c.uploadBlocks(ctx, linkID, revisionID, sessionKey, nodeKR, r)
	if err != nil {
		c.cleanupFailedUpload(ctx, parent, linkID, revisionID, newFile)
		return err
	}

	if err := c.commitRevision(ctx, linkID, revisionID, nodeKR, blockSizes, hashes, size, modTime); err != nil {
		c.cleanupFailedUpload(ctx, parent, linkID, revisionID, newFile)
		return err
	}

	return nil
}

// startRevision creates a fresh file (existing == nil) or a new revision on an existing file,
// returning the link/revision IDs and the keyring/session key needed to upload blocks against
// it.
func (c *Client) startRevision(ctx context.Context, parent *Node, name string, existing *Node) (linkID, revisionID string, nodeKR *crypto.KeyRing, sessionKey *crypto.SessionKey, err error) {
	if existing == nil {
		return c.createFile(ctx, parent, name)
	}

	sessionKey, err = existing.Link.GetSessionKey(existing.KR)
	if err != nil {
		return "", "", nil, nil, err
	}

	res, err := c.api.CreateRevision(ctx, c.shareID, existing.Link.LinkID)
	if err != nil {
		return "", "", nil, nil, err
	}

	return existing.Link.LinkID, res.ID, existing.KR, sessionKey, nil
}

// createFile creates a new, empty file (an initial draft revision) under parent and returns
// the identifiers and keys needed to upload its content.
func (c *Client) createFile(ctx context.Context, parent *Node, name string) (linkID, revisionID string, nodeKR *crypto.KeyRing, sessionKey *crypto.SessionKey, err error) {
	nodeKey, nodePassphrase, nodePassphraseSig, err := generateNodeKeys(parent.KR, c.signKR)
	if err != nil {
		return "", "", nil, nil, err
	}

	nodeKR, err = unlockNodeKR(parent.KR, c.signKR, nodeKey, nodePassphrase, nodePassphraseSig)
	if err != nil {
		return "", "", nil, nil, err
	}

	req := proton.CreateFileReq{
		ParentLinkID:            parent.Link.LinkID,
		SignatureAddress:        c.signEmail,
		NodeKey:                 nodeKey,
		NodePassphrase:          nodePassphrase,
		NodePassphraseSignature: nodePassphraseSig,
		MIMEType:                mimeTypeFor(name),
	}

	sessionKey, err = req.SetContentKeyPacketAndSignature(nodeKR)
	if err != nil {
		return "", "", nil, nil, err
	}

	if err := req.SetName(name, c.signKR, parent.KR); err != nil {
		return "", "", nil, nil, err
	}

	parentHashKey, err := parent.Link.GetHashKey(parent.KR, c.addrKR)
	if err != nil {
		return "", "", nil, nil, err
	}

	if err := req.SetHash(name, parentHashKey); err != nil {
		return "", "", nil, nil, err
	}

	res, err := c.api.CreateFile(ctx, c.shareID, req)
	if err != nil {
		return "", "", nil, nil, err
	}

	return res.ID, res.RevisionID, nodeKR, sessionKey, nil
}

// uploadBlocks reads r in blockSize plaintext chunks, uploading each as one encrypted, signed
// block, and returns the per-block plaintext sizes and raw sha256 digests in upload order —
// the inputs to the revision manifest.
func (c *Client) uploadBlocks(ctx context.Context, linkID, revisionID string, sessionKey *crypto.SessionKey, nodeKR *crypto.KeyRing, r io.Reader) ([]int64, [][]byte, error) {
	var (
		blockSizes []int64
		hashes     [][]byte
		buf        = make([]byte, blockSize)
		index      = 1
	)

	for {
		n, readErr := io.ReadFull(r, buf)
		if n > 0 {
			hash, err := c.uploadBlock(ctx, linkID, revisionID, index, sessionKey, nodeKR, buf[:n])
			if err != nil {
				return nil, nil, err
			}

			blockSizes = append(blockSizes, int64(n))
			hashes = append(hashes, hash)
			index++
		}

		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			return blockSizes, hashes, nil
		}
		if readErr != nil {
			return nil, nil, readErr
		}
	}
}

// uploadBlock encrypts, signs, hashes and uploads one plaintext chunk as block index, returning
// the raw (non-armored) sha256 digest of the encrypted block.
func (c *Client) uploadBlock(ctx context.Context, linkID, revisionID string, index int, sessionKey *crypto.SessionKey, nodeKR *crypto.KeyRing, chunk []byte) ([]byte, error) {
	plain := crypto.NewPlainMessage(chunk)

	encrypted, err := sessionKey.Encrypt(plain)
	if err != nil {
		return nil, err
	}

	sig, err := c.signKR.SignDetachedEncrypted(plain, nodeKR)
	if err != nil {
		return nil, err
	}

	sigArmored, err := sig.GetArmored()
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256(encrypted)

	req := proton.BlockUploadReq{
		AddressID:  c.addressID,
		ShareID:    c.shareID,
		LinkID:     linkID,
		RevisionID: revisionID,
		BlockList: []proton.BlockUploadInfo{{
			Index:        index,
			Size:         int64(len(encrypted)),
			EncSignature: sigArmored,
			Hash:         base64.StdEncoding.EncodeToString(sum[:]),
			// ponytail: no block Verifier token; add if Proton starts rejecting uploads
		}},
	}

	links, err := c.api.RequestBlockUpload(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(links) != 1 {
		return nil, errors.New("unexpected number of block upload links")
	}

	if err := c.api.UploadBlock(ctx, links[0].BareURL, links[0].Token, bytes.NewReader(encrypted)); err != nil {
		return nil, err
	}

	return sum[:], nil
}

// commitRevision signs the block manifest and commits the revision with size/modTime XAttr
// metadata.
func (c *Client) commitRevision(ctx context.Context, linkID, revisionID string, nodeKR *crypto.KeyRing, blockSizes []int64, hashes [][]byte, size int64, modTime time.Time) error {
	manifest := blockManifest(hashes)

	sig, err := c.signKR.SignDetached(crypto.NewPlainMessage(manifest))
	if err != nil {
		return err
	}

	sigArmored, err := sig.GetArmored()
	if err != nil {
		return err
	}

	req := proton.CommitRevisionReq{
		ManifestSignature: sigArmored,
		SignatureAddress:  c.signEmail,
	}

	xattr := proton.RevisionXAttrCommon{
		ModificationTime: modTime.Format(time.RFC3339),
		Size:             size,
		BlockSizes:       blockSizes,
	}

	if err := req.SetEncXAttrString(c.signKR, nodeKR, &xattr); err != nil {
		return err
	}

	return c.api.CommitRevision(ctx, c.shareID, linkID, revisionID, req)
}

// cleanupFailedUpload best-effort discards a link (newFile) or revision (existing file) that
// failed partway through Upload, so it doesn't linger as a broken draft.
func (c *Client) cleanupFailedUpload(ctx context.Context, parent *Node, linkID, revisionID string, newFile bool) {
	if newFile {
		_ = c.api.DeleteChildren(ctx, c.shareID, parent.Link.LinkID, linkID)
		return
	}

	_ = c.api.DeleteRevision(ctx, c.shareID, linkID, revisionID)
}

// blockManifest concatenates block digests in upload order to build the plaintext that gets
// signed as CommitRevisionReq.ManifestSignature.
func blockManifest(hashes [][]byte) []byte {
	var out []byte
	for _, h := range hashes {
		out = append(out, h...)
	}
	return out
}

// blockCount returns how many blockSize chunks a file of size bytes splits into. A zero or
// negative size has zero blocks, matching uploadBlocks never emitting a block for an empty read.
func blockCount(size int64) int {
	if size <= 0 {
		return 0
	}
	return int((size + blockSize - 1) / blockSize)
}

// mimeTypeFor guesses a MIME type from name's extension, falling back to a generic binary type.
func mimeTypeFor(name string) string {
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}
