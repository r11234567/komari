// Package diagnosticrecords stores sealed records and issues durable receipts.
package diagnosticrecords

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"time"

	"github.com/r11234567/komari-proto/diagnostics"
)

var (
	ErrIdentity = errors.New("diagnostic envelope does not match the authenticated node")
	ErrConflict = errors.New("diagnostic record sequence already has a different envelope")
	bundleName  = regexp.MustCompile(`^[0-9a-f]{32}-[0-9a-f]{32}-[0-9]{20}\.tar\.gz$`)
)

type Entry struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	StoredAt time.Time `json:"stored_at"`
}

func ValidName(name string) bool { return bundleName.MatchString(name) }

// Archive writes unique immutable entries on a local filesystem. A trusted
// deployment protects this directory and configures separate off-host retention.
type Archive struct {
	dir     *os.Root
	signing ed25519.PrivateKey
}

func Open(path string, signing ed25519.PrivateKey) (*Archive, error) {
	if len(signing) != ed25519.PrivateKeySize {
		return nil, diagnostics.ErrReceipt
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	info, err := root.Stat(".")
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		_ = root.Close()
		return nil, errors.New("diagnostic archive requires a private directory")
	}
	return &Archive{dir: root, signing: bytes.Clone(signing)}, nil
}

func (a *Archive) Close() error {
	clear(a.signing)
	return a.dir.Close()
}

// Put authenticates the node binding before writing any data. A retry of the
// same ciphertext returns exactly the original receipt.
func (a *Archive) Put(node [16]byte, envelope []byte) (diagnostics.Receipt, error) {
	header, err := diagnostics.Inspect(envelope)
	if err != nil {
		return diagnostics.Receipt{}, err
	}
	if header.Node != node {
		return diagnostics.Receipt{}, ErrIdentity
	}
	name := recordName(header)
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return diagnostics.Receipt{}, err
	}
	stage := ".incoming-" + hex.EncodeToString(random[:])
	file, err := a.dir.OpenFile(stage, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return diagnostics.Receipt{}, err
	}
	defer a.dir.Remove(stage)
	receipt, err := diagnostics.SignReceipt(a.signing, envelope, time.Now())
	if err != nil {
		_ = file.Close()
		return diagnostics.Receipt{}, err
	}
	if err = errors.Join(writeBundle(file, header, receipt, envelope), file.Sync(), file.Close()); err != nil {
		return diagnostics.Receipt{}, err
	}
	if err := a.dir.Link(stage, name); errors.Is(err, os.ErrExist) {
		old, previous, err := a.Read(header)
		if err != nil {
			return diagnostics.Receipt{}, err
		}
		if !bytes.Equal(old, envelope) {
			return diagnostics.Receipt{}, ErrConflict
		}
		receipt = previous
	} else if err != nil {
		return diagnostics.Receipt{}, err
	}
	dir, err := a.dir.Open(".")
	if err != nil {
		return diagnostics.Receipt{}, err
	}
	if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
		return diagnostics.Receipt{}, err
	}
	return receipt, nil
}

func (a *Archive) Read(header diagnostics.Header) ([]byte, diagnostics.Receipt, error) {
	file, err := a.dir.Open(recordName(header))
	if err != nil {
		return nil, diagnostics.Receipt{}, err
	}
	defer file.Close()
	stored, receipt, envelope, err := VerifyBundle(file, a.signing.Public().(ed25519.PublicKey))
	if err != nil || stored.Node != header.Node || stored.Stream != header.Stream || stored.Sequence != header.Sequence {
		return nil, diagnostics.Receipt{}, errors.New("stored diagnostic identity mismatch")
	}
	return envelope, receipt, nil
}

func (a *Archive) List() ([]Entry, error) {
	directory, err := a.dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	files, err := directory.ReadDir(1025)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(files) > 1024 {
		return nil, errors.New("diagnostic archive exceeds browse limit")
	}
	result := make([]Entry, 0, len(files))
	for _, file := range files {
		if !file.Type().IsRegular() || !ValidName(file.Name()) {
			continue
		}
		info, err := file.Info()
		if err != nil || info.Size() > MaxBundleSize {
			continue
		}
		result = append(result, Entry{Name: file.Name(), Size: info.Size(), StoredAt: info.ModTime().UTC()})
	}
	slices.SortFunc(result, func(a, b Entry) int { return b.StoredAt.Compare(a.StoredAt) })
	return result, nil
}

func (a *Archive) Download(name string) ([]byte, error) {
	if !ValidName(name) {
		return nil, os.ErrInvalid
	}
	file, err := a.dir.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxBundleSize {
		return nil, errors.New("invalid stored diagnostic archive")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxBundleSize+1))
	if err != nil || len(data) > MaxBundleSize {
		return nil, errors.New("invalid stored diagnostic archive")
	}
	header, _, _, err := VerifyBundle(bytes.NewReader(data), a.signing.Public().(ed25519.PublicKey))
	if err != nil || recordName(header) != name {
		return nil, errors.New("diagnostic archive failed verification")
	}
	return data, nil
}

func recordName(h diagnostics.Header) string {
	return fmt.Sprintf("%x-%x-%020d.tar.gz", h.Node, h.Stream, h.Sequence)
}
