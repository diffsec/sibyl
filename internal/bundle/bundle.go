// Package bundle reads and checks an offline update bundle.
//
// A bundle is a directory:
//
//	manifest.json       the images, with the SHA-256 of each archive
//	manifest.json.sig   cosign sign-blob signature of manifest.json, base64
//	*.oci.tar           one OCI archive per image
//
// CI signs manifest.json with the project key, the same key that signs the
// images. Verify checks that signature against the host's public key and
// then the SHA-256 of every archive, so a bundle carries the same trust as
// an online pull.
package bundle

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Store says where an image goes on the host.
type Store string

const (
	// OS is the bootc host image.
	OS Store = "os"
	// Sibyl is the sibyl user's container storage, which the Quadlet units use.
	Sibyl Store = "sibyl"
	// TUI is the console interface, which each operator runs from their own storage.
	TUI Store = "tui"
)

// Image is one archive in the bundle.
type Image struct {
	Ref    string `json:"ref"`    // ghcr.io/owner/name:tag or ghcr.io/owner/name@sha256:...
	File   string `json:"file"`   // archive name in the bundle directory
	SHA256 string `json:"sha256"` // of the archive file
	Size   int64  `json:"size"`
	Store  Store  `json:"store"`
}

// Manifest is manifest.json.
type Manifest struct {
	Version int     `json:"version"`
	Tag     string  `json:"tag"`     // image tag, such as sha-93a977a
	Variant string  `json:"variant"` // amd, nvidia or intel
	Created string  `json:"created"`
	Images  []Image `json:"images"`
}

var (
	fileRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}\.oci\.tar$`)
	refRE    = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)+(:[A-Za-z0-9._-]{1,128}|@sha256:[0-9a-f]{64})$`)
	sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Validate checks the manifest's fields.
func (m Manifest) Validate() error {
	if m.Version != 1 {
		return fmt.Errorf("manifest version %d is not supported", m.Version)
	}
	if len(m.Images) == 0 {
		return errors.New("the manifest lists no images")
	}
	oses := 0
	seen := map[string]bool{}
	for i, im := range m.Images {
		switch {
		case !fileRE.MatchString(im.File):
			return fmt.Errorf("images[%d]: file %q is not a plain archive name", i, im.File)
		case seen[im.File]:
			return fmt.Errorf("images[%d]: file %q is listed twice", i, im.File)
		case !refRE.MatchString(im.Ref):
			return fmt.Errorf("images[%d]: %q is not an image reference", i, im.Ref)
		case !sha256RE.MatchString(im.SHA256):
			return fmt.Errorf("images[%d]: sha256 %q is not a SHA-256", i, im.SHA256)
		}
		switch im.Store {
		case OS:
			oses++
		case Sibyl, TUI:
		default:
			return fmt.Errorf("images[%d]: unknown store %q", i, im.Store)
		}
		seen[im.File] = true
	}
	if oses != 1 {
		return fmt.Errorf("the manifest lists %d OS images, not 1", oses)
	}
	return nil
}

// OSImage returns the bootc host image.
func (m Manifest) OSImage() Image {
	for _, im := range m.Images {
		if im.Store == OS {
			return im
		}
	}
	return Image{}
}

// LoadPublicKey reads a PEM ECDSA public key, such as
// /etc/pki/containers/sibyl-cosign.pub.
func LoadPublicKey(path string) (*ecdsa.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	ec, ok := key.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an ECDSA key", path)
	}
	return ec, nil
}

// ReadManifest reads manifest.json and checks its signature. It does not
// check the archives.
func ReadManifest(dir string, key *ecdsa.PublicKey) (Manifest, error) {
	var m Manifest
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return m, err
	}
	sig, err := os.ReadFile(filepath.Join(dir, "manifest.json.sig"))
	if err != nil {
		return m, err
	}
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil {
		return m, errors.New("manifest.json.sig is not base64")
	}
	sum := sha256.Sum256(data)
	if !ecdsa.VerifyASN1(key, sum[:], der) {
		return m, errors.New("manifest.json.sig does not match the project key: the bundle is not from Sibyl's CI, or it was changed")
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("manifest.json: %w", err)
	}
	return m, m.Validate()
}

// Verify checks the manifest signature and the SHA-256 of every archive.
// progress, if set, is called before each archive.
func Verify(dir string, key *ecdsa.PublicKey, progress func(Image)) (Manifest, error) {
	m, err := ReadManifest(dir, key)
	if err != nil {
		return m, err
	}
	for _, im := range m.Images {
		if progress != nil {
			progress(im)
		}
		got, size, err := fileSHA256(filepath.Join(dir, im.File))
		if err != nil {
			return m, err
		}
		if got != im.SHA256 || (im.Size != 0 && size != im.Size) {
			return m, fmt.Errorf("%s does not match the manifest: the file is damaged or incomplete", im.File)
		}
	}
	return m, nil
}

func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// RepoName returns the repository part of a reference, without the tag or
// digest. A digest reference is stored under its repository name, and the
// image keeps its digest.
func RepoName(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i]
	}
	return ref
}
