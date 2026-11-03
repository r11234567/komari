package plugin

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/komari-monitor/komari/database/models"
)

// Package admission is serialized independently of runtime locks. Callers must
// verify a human proof inside DecidePackage's callback, after authoritative
// package/active-revision validation and before any authority or code changes.
var packageMu sync.Mutex

const packageReceipt = ".komari-package.json"
const maxPackageBytes int64 = 100 << 20
const maxPackageStoreBytes int64 = 512 << 20
const maxStoredPackages = 64

type PackageReview struct {
	Plugin          models.Plugin `json:"plugin"`
	Digest          string        `json:"digest"`
	Source          string        `json:"source"`
	PermissionsHash string        `json:"permissions_hash"`
	CreatedAt       time.Time     `json:"created_at"`
	ApprovedAt      *time.Time    `json:"approved_at,omitempty"`
	ApprovedBy      string        `json:"approved_by,omitempty"`
	Revoked         bool          `json:"revoked"`
	Status          string        `json:"status"`
	CurrentDigest   string        `json:"current_digest"`
}

func (p PackageReview) Targets() []string {
	return []string{p.Plugin.Short, p.Digest, p.Plugin.Version, p.PermissionsHash, p.CurrentDigest}
}

func packageDir() string { return filepath.Join(DataDir, ".packages") }
func validDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == sha256.Size && s == strings.ToLower(s)
}
func packagePath(digest, suffix string) string { return filepath.Join(packageDir(), digest+suffix) }
func packagePermissions(p models.PluginPermissions) string {
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func atomicPackageFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".write-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncPackageDirectory(filepath.Dir(path))
}

func syncPackageDirectory(path string) error {
	// Windows does not support fsync on directory handles. On Unix persist
	// the journal/state rename before allowing newly approved code to execute.
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func deleteRetainedPackages(short string) error {
	entries, err := os.ReadDir(packageDir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || strings.HasPrefix(entry.Name(), "journal-") {
			continue
		}
		p, err := readPackage(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return err
		}
		if p.Plugin.Short != short {
			continue
		}
		if err = os.Remove(packagePath(p.Digest, ".zip")); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err = os.Remove(packagePath(p.Digest, ".json")); err != nil {
			return err
		}
	}
	return syncPackageDirectory(packageDir())
}
func savePackage(p PackageReview) error {
	b, e := json.Marshal(p)
	if e != nil {
		return e
	}
	return atomicPackageFile(packagePath(p.Digest, ".json"), b)
}
func readPackage(digest string) (PackageReview, error) {
	var p PackageReview
	if !validDigest(digest) {
		return p, errors.New("invalid package digest")
	}
	b, e := os.ReadFile(packagePath(digest, ".json"))
	if e != nil {
		return p, e
	}
	if len(b) > 2*maxPluginManifestSize {
		return p, errors.New("package metadata too large")
	}
	if e = json.Unmarshal(b, &p); e != nil {
		return p, e
	}
	if p.Digest != digest || !validShort(p.Plugin.Short) || p.PermissionsHash != packagePermissions(p.Plugin.Permissions) {
		return p, errors.New("invalid package metadata")
	}
	return p, nil
}
func digestFile(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	n, e := io.Copy(h, io.LimitReader(f, maxPackageBytes+1))
	if e != nil {
		return "", e
	}
	if n > maxPackageBytes {
		return "", errors.New("plugin package exceeds 100 MiB")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// unpackPackage performs complete validation in a private disposable directory;
// no JavaScript or installation hook executes during staging/inspection.
func unpackPackage(path string) (models.Plugin, string, error) {
	var info models.Plugin
	r, e := zip.OpenReader(path)
	if e != nil {
		return info, "", e
	}
	defer r.Close()
	if e = validatePluginArchive(r.File); e != nil {
		return info, "", e
	}
	for _, entry := range r.File {
		if entry.Name != manifestFile {
			continue
		}
		f, err := entry.Open()
		if err != nil {
			return info, "", err
		}
		b, err := io.ReadAll(io.LimitReader(f, maxPluginManifestSize+1))
		_ = f.Close()
		if err != nil {
			return info, "", err
		}
		if len(b) > maxPluginManifestSize {
			return info, "", errors.New("plugin manifest too large")
		}
		if err = json.Unmarshal(b, &info); err != nil {
			return info, "", err
		}
	}
	if e = validateManifest(&info); e != nil {
		return info, "", e
	}
	if e = CheckKomariVersion(info.Komari); e != nil {
		return info, "", e
	}
	if e = os.MkdirAll(DataDir, 0700); e != nil {
		return info, "", e
	}
	dir, e := os.MkdirTemp(DataDir, ".activate-")
	if e != nil {
		return info, "", e
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(dir)
		}
	}()
	if e = extractPluginArchive(r.File, dir); e != nil {
		return info, "", e
	}
	files := []string{info.Entry}
	for _, page := range info.Pages {
		if page.Type == models.PageTypeIframe {
			files = append(files, page.File)
		}
	}
	for _, name := range files {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			return info, "", err
		}
		if !st.Mode().IsRegular() {
			return info, "", errors.New("plugin entry/page must be a regular file")
		}
	}
	ok = true
	return info, dir, nil
}

// StageZip stores a content-addressed, bounded package without replacing or
// stopping the active version. Expected identity is checked before retention.
func StageZip(path, source, expectedShort, expectedVersion string) (models.Plugin, error) {
	packageMu.Lock()
	defer packageMu.Unlock()
	return stageZip(path, source, expectedShort, expectedVersion)
}
func stageZip(path, source, expectedShort, expectedVersion string) (models.Plugin, error) {
	var empty models.Plugin
	if len(source) > 2048 {
		return empty, errors.New("package source too long")
	}
	if e := os.MkdirAll(packageDir(), 0700); e != nil {
		return empty, e
	}
	// Hash the retained copy, never an attacker-changeable source after review.
	f, e := os.CreateTemp(packageDir(), ".incoming-")
	if e != nil {
		return empty, e
	}
	defer os.Remove(f.Name())
	src, e := os.Open(path)
	if e != nil {
		f.Close()
		return empty, e
	}
	n, e := io.Copy(f, io.LimitReader(src, maxPackageBytes+1))
	_ = src.Close()
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return empty, e
	}
	if n > maxPackageBytes {
		return empty, errors.New("plugin package exceeds 100 MiB")
	}
	digest, e := digestFile(f.Name())
	if e != nil {
		return empty, e
	}
	info, dir, e := unpackPackage(f.Name())
	if e != nil {
		return empty, e
	}
	defer os.RemoveAll(dir)
	if (expectedShort != "" && info.Short != expectedShort) || (expectedVersion != "" && info.Version != expectedVersion) {
		return empty, errors.New("plugin manifest does not match the selected catalog identity")
	}
	if existing, err := readPackage(digest); err == nil {
		return existing.Plugin, nil
	} else if !os.IsNotExist(err) {
		return empty, err
	}
	entries, e := os.ReadDir(packageDir())
	if e != nil {
		return empty, e
	}
	count := 0
	var used int64
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".zip") {
			st, err := entry.Info()
			if err != nil {
				return empty, err
			}
			count++
			used += st.Size()
		}
	}
	if count >= maxStoredPackages || used+n > maxPackageStoreBytes {
		return empty, errors.New("plugin package storage limit reached; discard unused packages first")
	}
	if e = os.Rename(f.Name(), packagePath(digest, ".zip")); e != nil {
		return empty, e
	}
	review := PackageReview{Plugin: info, Digest: digest, Source: source, PermissionsHash: packagePermissions(info.Permissions), CreatedAt: time.Now().UTC()}
	if e = savePackage(review); e != nil {
		_ = os.Remove(packagePath(digest, ".zip"))
		return empty, e
	}
	return info, nil
}

// Existing installations are snapshotted once for explicit migration review.
// Deterministic headers/path ordering keep unchanged snapshots stable.
func snapshotInstalled(short string) error {
	dir := filepath.Join(DataDir, short)
	temp, e := os.CreateTemp("", "komari-plugin-review-*.zip")
	if e != nil {
		return e
	}
	defer os.Remove(temp.Name())
	w := zip.NewWriter(temp)
	var total int64
	count := 0
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("cannot review a symlink in an existing plugin")
		}
		st, err := entry.Info()
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return errors.New("non-regular plugin file")
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == packageReceipt {
			return nil
		}
		count++
		total += st.Size()
		if count > maxPluginArchiveFiles || st.Size() > maxPluginFileSize || total > maxPluginExtractedSize {
			return errors.New("existing plugin snapshot exceeds limits")
		}
		dst, err := w.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(dst, io.LimitReader(f, maxPluginFileSize+1))
		_ = f.Close()
		return err
	})
	closeErr := w.Close()
	fileErr := temp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if fileErr != nil {
		return fileErr
	}
	_, e = stageZip(temp.Name(), "Existing installation snapshot", short, "")
	return e
}

func receiptDigest(short string) string {
	var receipt struct {
		Digest string `json:"digest"`
	}
	b, e := os.ReadFile(filepath.Join(DataDir, short, packageReceipt))
	if e != nil || len(b) > 1024 {
		return ""
	}
	if json.Unmarshal(b, &receipt) != nil || !validDigest(receipt.Digest) {
		return ""
	}
	return receipt.Digest
}
func writeReceipt(dir, digest string) error {
	b, _ := json.Marshal(map[string]string{"digest": digest})
	return atomicPackageFile(filepath.Join(dir, packageReceipt), b)
}
func packageApproved(short string, st PluginState) bool {
	return st.ApprovedPackageDigest != "" && receiptDigest(short) == st.ApprovedPackageDigest
}

// Explicitly trusted Go callers may approve existing directories. No transport
// forwards an approval flag to this path; existing production grants must go
// through snapshot staging and a scoped human proof instead.
func approveTrustedDirectory(short string) error {
	if e := snapshotInstalled(short); e != nil {
		return e
	}
	entries, e := os.ReadDir(packageDir())
	if e != nil {
		return e
	}
	var newest PackageReview
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || strings.HasPrefix(entry.Name(), "journal-") {
			continue
		}
		p, err := readPackage(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return err
		}
		if p.Plugin.Short == short && (newest.Digest == "" || p.CreatedAt.After(newest.CreatedAt)) {
			newest = p
		}
	}
	if newest.Digest == "" {
		return errors.New("existing plugin snapshot not found")
	}
	if e = writeReceipt(filepath.Join(DataDir, short), newest.Digest); e != nil {
		return e
	}
	st := global.stateStore().get(short)
	st.ApprovedPackageDigest = newest.Digest
	return global.stateStore().set(short, st)
}

func ListPackages() ([]PackageReview, error) {
	packageMu.Lock()
	defer packageMu.Unlock()
	for _, installed := range global.list() {
		digest := receiptDigest(installed.Short)
		if digest == "" {
			if e := snapshotInstalled(installed.Short); e != nil {
				return nil, e
			}
		} else if _, e := readPackage(digest); os.IsNotExist(e) {
			if e = snapshotInstalled(installed.Short); e != nil {
				return nil, e
			}
		}
	}
	entries, e := os.ReadDir(packageDir())
	if os.IsNotExist(e) {
		return []PackageReview{}, nil
	}
	if e != nil {
		return nil, e
	}
	result := []PackageReview{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || strings.HasPrefix(entry.Name(), "journal-") {
			continue
		}
		p, err := readPackage(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		p.CurrentDigest = receiptDigest(p.Plugin.Short)
		p.Status = "pending"
		if p.ApprovedAt != nil {
			p.Status = "retained"
		}
		if p.Revoked {
			p.Status = "revoked"
		}
		if p.CurrentDigest == p.Digest {
			p.Status = "installed"
			if packageApproved(p.Plugin.Short, global.stateStore().get(p.Plugin.Short)) {
				p.Status = "approved"
			}
		}
		if p.Revoked {
			p.Status = "revoked"
		}
		result = append(result, p)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Plugin.Short != result[j].Plugin.Short {
			return result[i].Plugin.Short < result[j].Plugin.Short
		}
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result, nil
}

// DecidePackage is the only untrusted-input admission path. The verifier is
// supplied by trusted Go transport code, not serialized in an RPC request.
func DecidePackage(short, digest, current, action, actor string, verify func(PackageReview) error) error {
	packageMu.Lock()
	defer packageMu.Unlock()
	if !validShort(short) || actor == "" || verify == nil {
		return errors.New("human package approval is required")
	}
	if action != "activate" && action != "revoke" && action != "discard" {
		return errors.New("invalid package action")
	}
	p, e := readPackage(digest)
	if e != nil {
		return e
	}
	if p.Plugin.Short != short {
		return errors.New("package identity mismatch")
	}
	p.CurrentDigest = receiptDigest(short)
	if current != p.CurrentDigest {
		return errors.New("active package changed; review again")
	}
	var prepared string
	if action == "activate" {
		actual, err := digestFile(packagePath(digest, ".zip"))
		if err != nil {
			return err
		}
		if actual != digest {
			return errors.New("staged package content changed")
		}
		// Verify actual archive identity/permissions before granting execution.
		// Revocation must remain available even if retained bytes are damaged.
		info, dir, err := unpackPackage(packagePath(digest, ".zip"))
		if err != nil {
			return err
		}
		prepared = dir
		defer os.RemoveAll(prepared)
		p.Plugin = info
		p.PermissionsHash = packagePermissions(info.Permissions)
		if info.Short != short {
			return errors.New("archive identity mismatch")
		}
	}
	if action == "discard" && p.CurrentDigest == digest {
		return errors.New("cannot discard an installed package")
	}
	if e = verify(p); e != nil {
		return e
	}
	if action == "discard" {
		if e = os.Remove(packagePath(digest, ".zip")); e != nil && !os.IsNotExist(e) {
			return e
		}
		return os.Remove(packagePath(digest, ".json"))
	}
	now := time.Now().UTC()
	if action == "revoke" {
		if p.CurrentDigest == digest {
			st := global.stateStore().get(short)
			st.Enabled = false
			st.ApprovedPackageDigest = ""
			if e = global.stateStore().set(short, st); e != nil {
				return e
			}
			e = global.unload(short)
			if errors.Is(e, errNotLoaded) {
				e = nil
			}
		}
		p.Revoked = true
		return errors.Join(e, savePackage(p))
	}
	old := global.stateStore().get(short)
	if p.CurrentDigest == digest && packageApproved(short, old) {
		p.ApprovedAt = &now
		p.ApprovedBy = actor
		p.Revoked = false
		if err := savePackage(p); err != nil {
			return err
		}
		return global.setEnabled(short, true, false)
	}
	if e = writeReceipt(prepared, digest); e != nil {
		return e
	}
	p.ApprovedAt = &now
	p.ApprovedBy = actor
	p.Revoked = false
	if e = savePackage(p); e != nil {
		return e
	}
	return activatePrepared(short, digest, prepared, old)
}

type packageJournal struct {
	Short, Prepared, Backup, Digest string
	Old                             PluginState
	HadOld                          bool
}

func journalPath(short string) string { return filepath.Join(packageDir(), "journal-"+short+".json") }
func activatePrepared(short, digest, prepared string, old PluginState) error {
	active := filepath.Join(DataDir, short)
	backup, e := os.MkdirTemp(DataDir, ".rollback-")
	if e != nil {
		return e
	}
	if e = os.Remove(backup); e != nil {
		return e
	}
	_, statErr := os.Stat(active)
	if statErr != nil && !os.IsNotExist(statErr) {
		return statErr
	}
	j := packageJournal{Short: short, Prepared: filepath.Base(prepared), Backup: filepath.Base(backup), Digest: digest, Old: old, HadOld: statErr == nil}
	data, _ := json.Marshal(j)
	if e = atomicPackageFile(journalPath(short), data); e != nil {
		return e
	}
	restore := func(cause error) error {
		_ = global.unload(short)
		if err := restorePackageJournal(j); err != nil {
			return errors.Join(cause, err)
		}
		if old.Enabled {
			if err := global.restartPlugin(short); err != nil {
				return errors.Join(cause, err)
			}
		}
		return cause
	}
	if e = global.unload(short); e != nil && !errors.Is(e, errNotLoaded) {
		_ = os.Remove(journalPath(short))
		if old.Enabled {
			_ = global.restartPlugin(short)
		}
		return e
	}
	if j.HadOld {
		if e = os.Rename(active, backup); e != nil {
			_ = os.Remove(journalPath(short))
			if old.Enabled {
				_ = global.restartPlugin(short)
			}
			return e
		}
	}
	if e = os.Rename(prepared, active); e != nil {
		return restore(e)
	}
	if e = syncPackageDirectory(DataDir); e != nil {
		return restore(e)
	}
	st := old
	st.Enabled = false
	st.LastError = ""
	st.ApprovedPackageDigest = digest
	info, e := readManifest(short)
	if e != nil {
		return restore(e)
	}
	st.ApprovedPermissionsHash = approvalPermissionsHash(info.Permissions)
	if e = global.stateStore().set(short, st); e != nil {
		return restore(e)
	}
	if e = global.setEnabled(short, true, false); e != nil {
		return restore(e)
	}
	if e = os.Remove(journalPath(short)); e != nil {
		return restore(e)
	}
	if e = syncPackageDirectory(packageDir()); e != nil {
		return e
	}
	_ = os.RemoveAll(backup)
	return nil
}
func restorePackageJournal(j packageJournal) error {
	if !validShort(j.Short) || !validDigest(j.Digest) || !strings.HasPrefix(j.Backup, ".rollback-") || filepath.Base(j.Backup) != j.Backup || !strings.HasPrefix(j.Prepared, ".activate-") || filepath.Base(j.Prepared) != j.Prepared {
		return errors.New("invalid plugin recovery journal")
	}
	active := filepath.Join(DataDir, j.Short)
	backup := filepath.Join(DataDir, j.Backup)
	if _, e := os.Stat(backup); e == nil {
		if e = os.RemoveAll(active); e != nil {
			return e
		}
		if e = os.Rename(backup, active); e != nil {
			return e
		}
	} else if !os.IsNotExist(e) {
		return e
	} else if !j.HadOld && receiptDigest(j.Short) == j.Digest {
		if e = os.RemoveAll(active); e != nil {
			return e
		}
	}
	if e := global.stateStore().set(j.Short, j.Old); e != nil {
		return e
	}
	_ = os.RemoveAll(filepath.Join(DataDir, j.Prepared))
	return os.Remove(journalPath(j.Short))
}
func recoverPackageTransactions() error {
	entries, e := os.ReadDir(packageDir())
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "journal-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(packageDir(), entry.Name()))
		if err != nil {
			return err
		}
		var j packageJournal
		if json.Unmarshal(b, &j) != nil {
			return errors.New("invalid plugin recovery journal")
		}
		if entry.Name() != "journal-"+j.Short+".json" {
			return errors.New("plugin journal name mismatch")
		}
		if err = restorePackageJournal(j); err != nil {
			return fmt.Errorf("recover plugin package: %w", err)
		}
	}
	return nil
}
