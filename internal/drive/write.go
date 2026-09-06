package drive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"mime"
	"path"
	"time"

	"github.com/ProtonMail/gopenpgp/v2/crypto"
	proton "github.com/henrybear327/go-proton-api"
)

// CreateDir creates a new folder named name under parent and returns it, so the caller can add
// it to a cached listing instead of re-listing the parent.
func (c *Client) CreateDir(ctx context.Context, parent *Node, name string) (*Node, error) {
	parentKR, err := parent.Keyring()
	if err != nil {
		return nil, err
	}

	nodeKey, nodePassphrase, nodePassphraseSig, err := generateNodeKeys(parentKR, c.signKR)
	if err != nil {
		return nil, err
	}

	req := proton.CreateFolderReq{
		ParentLinkID:            parent.Link.LinkID,
		SignatureAddress:        c.signEmail,
		NodeKey:                 nodeKey,
		NodePassphrase:          nodePassphrase,
		NodePassphraseSignature: nodePassphraseSig,
	}

	if err := req.SetName(name, c.signKR, parentKR); err != nil {
		return nil, err
	}

	parentHashKey, err := parent.Link.GetHashKey(parentKR, c.addrKR)
	if err != nil {
		return nil, err
	}

	if err := req.SetHash(name, parentHashKey); err != nil {
		return nil, err
	}

	folderKR, err := unlockNodeKR(parentKR, c.signKR, nodeKey, nodePassphrase, nodePassphraseSig)
	if err != nil {
		return nil, err
	}

	if err := req.SetNodeHashKey(folderKR); err != nil {
		return nil, err
	}

	res, err := c.api.CreateFolder(ctx, c.shareID, req)
	if err != nil {
		return nil, err
	}

	return c.fetchNode(ctx, res.ID, parent)
}

// moveLinkReq is the PUT .../links/{id}/move payload, matching WebClients' baseRequestBody
// exactly (packages/drive-store/store/_links/useLinksActions.ts, getMoveLinkData) as sent by
// packages/shared/lib/api/drive/share.ts queryMoveLink, typed as MoveLink in
// packages/shared/lib/interfaces/drive/link.ts. Those five fields are the whole schema; go-proton-
// api's own MoveLinkReq (link_file_types.go) additionally carries OriginalHash and
// SignatureAddress, which have no place in the current API -- sending OriginalHash empty (it's
// never populated) is exactly the "This value should not be blank" (Code=2000) the API rejects
// every move/rename with. NewShareID/ContentHash (cross-share moves, photos) aren't in the
// struct: this client never sets them.
//
// WebClients also conditionally adds NodePassphraseSignature + SignatureEmail when the moved
// link's own signatures show it needs re-verification (an anonymously-uploaded or externally-
// signed node, requestBody's case 2/3). This client only ever moves nodes under its own
// account, so that case doesn't arise in practice.
// ponytail: omitted re-sign path (case 2/3); add NodePassphraseSignature/SignatureEmail here,
// re-signed with c.signKR, if this fs ever moves nodes it didn't create/own.
type moveLinkReq struct {
	Name               string
	Hash               string
	ParentLinkID       string
	NodePassphrase     string
	NameSignatureEmail string
}

// Move relocates and/or renames n from oldParent to newParent as newName, and returns n as it
// now exists there: same link, new name, and a passphrase re-encrypted to the new parent, so a
// caller can move the entry between two cached listings without re-listing either.
func (c *Client) Move(ctx context.Context, n *Node, oldParent, newParent *Node, newName string) (*Node, error) {
	oldParentKR, err := oldParent.Keyring()
	if err != nil {
		return nil, err
	}

	newParentKR, err := newParent.Keyring()
	if err != nil {
		return nil, err
	}

	// SetName/SetHash do the crypto (encrypt the new name, hash it under the new parent's hash
	// key); go-proton-api's own MoveLinkReq is used only as a place to call them, its request is
	// never sent.
	req := proton.MoveLinkReq{
		ParentLinkID: newParent.Link.LinkID,
	}

	if err := req.SetName(newName, c.signKR, newParentKR); err != nil {
		return nil, err
	}

	newParentHashKey, err := newParent.Link.GetHashKey(newParentKR, c.addrKR)
	if err != nil {
		return nil, err
	}

	if err := req.SetHash(newName, newParentHashKey); err != nil {
		return nil, err
	}

	nodePassphrase, err := reencryptPassphrase(oldParentKR, newParentKR, n.Link.NodePassphrase)
	if err != nil {
		return nil, err
	}

	body := moveLinkReq{
		Name:               req.Name,
		Hash:               req.Hash,
		ParentLinkID:       req.ParentLinkID,
		NodePassphrase:     nodePassphrase,
		NameSignatureEmail: c.signEmail,
	}

	if err := c.putJSON(ctx, "/drive/shares/"+c.shareID+"/links/"+n.Link.LinkID+"/move", body); err != nil {
		return nil, err
	}

	link := n.Link
	link.Name = req.Name
	link.Hash = req.Hash
	link.ParentLinkID = req.ParentLinkID
	link.NodePassphrase = nodePassphrase

	moved := c.newNode(link, newName, newParentKR)
	moved.inheritAttrs(n)

	return moved, nil
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
//
// The committed link is read back and returned, so the caller can patch it into a cached listing
// rather than re-list the parent folder. A nil node with a nil error means the upload itself
// succeeded but the read-back did not; the caller has to expire its listing instead.
func (c *Client) Upload(ctx context.Context, parent *Node, name string, existing *Node, r io.Reader, size int64, modTime time.Time) (*Node, error) {
	newFile := existing == nil

	c.beginTransfer()
	defer c.endTransfer()

	linkID, revisionID, nodeKR, sessionKey, err := c.startRevision(ctx, parent, name, existing)
	if err != nil {
		return nil, err
	}

	blockSizes, hashes, err := c.uploadBlocks(ctx, linkID, revisionID, sessionKey, nodeKR, r)
	if err != nil {
		c.cleanupFailedUpload(ctx, parent, linkID, revisionID, newFile)
		return nil, err
	}

	if err := c.commitRevision(ctx, linkID, revisionID, nodeKR, blockSizes, hashes, size, modTime); err != nil {
		c.cleanupFailedUpload(ctx, parent, linkID, revisionID, newFile)
		return nil, err
	}

	uploaded, err := c.fetchNode(ctx, linkID, parent)
	if err != nil {
		log.Printf("drive: reading back uploaded link %s (%q): %v", linkID, name, err)
		return nil, nil
	}

	return uploaded, nil
}

// startRevision creates a fresh file (existing == nil) or a new revision on an existing file,
// returning the link/revision IDs and the keyring/session key needed to upload blocks against
// it.
func (c *Client) startRevision(ctx context.Context, parent *Node, name string, existing *Node) (linkID, revisionID string, nodeKR *crypto.KeyRing, sessionKey *crypto.SessionKey, err error) {
	if existing == nil {
		return c.createFile(ctx, parent, name)
	}

	existingKR, err := existing.Keyring()
	if err != nil {
		return "", "", nil, nil, err
	}

	sessionKey, err = existing.Link.GetSessionKey(existingKR)
	if err != nil {
		return "", "", nil, nil, err
	}

	res, err := c.api.CreateRevision(ctx, c.shareID, existing.Link.LinkID)
	if err != nil {
		return "", "", nil, nil, err
	}

	return existing.Link.LinkID, res.ID, existingKR, sessionKey, nil
}

// createFile creates a new, empty file (an initial draft revision) under parent and returns
// the identifiers and keys needed to upload its content.
func (c *Client) createFile(ctx context.Context, parent *Node, name string) (linkID, revisionID string, nodeKR *crypto.KeyRing, sessionKey *crypto.SessionKey, err error) {
	parentKR, err := parent.Keyring()
	if err != nil {
		return "", "", nil, nil, err
	}

	nodeKey, nodePassphrase, nodePassphraseSig, err := generateNodeKeys(parentKR, c.signKR)
	if err != nil {
		return "", "", nil, nil, err
	}

	nodeKR, err = unlockNodeKR(parentKR, c.signKR, nodeKey, nodePassphrase, nodePassphraseSig)
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

	if err := req.SetName(name, c.signKR, parentKR); err != nil {
		return "", "", nil, nil, err
	}

	parentHashKey, err := parent.Link.GetHashKey(parentKR, c.addrKR)
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
		if err := c.api.DeleteChildren(ctx, c.shareID, parent.Link.LinkID, linkID); err != nil {
			log.Printf("drive: cleanup after failed upload of %q: %v", linkID, err)
		}
		return
	}

	if err := c.api.DeleteRevision(ctx, c.shareID, linkID, revisionID); err != nil {
		log.Printf("drive: cleanup after failed upload of %q: %v", linkID, err)
	}
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
