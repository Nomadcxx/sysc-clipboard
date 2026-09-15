package store

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Nomadcxx/sysc-clipboard/internal/history"
	"github.com/Nomadcxx/sysc-clipboard/protocol"
)

var (
	ErrInvalidKey      = errors.New("invalid clipboard encryption key")
	ErrAuthentication  = errors.New("clipboard payload authentication failed")
	ErrCorruptManifest = errors.New("corrupt clipboard manifest")
	ErrStorageLimit    = errors.New("clipboard storage file exceeds limit")
)

const (
	manifestVersion = 1
	manifestName    = "manifest.enc"
	entriesName     = "entries"
	manifestPurpose = "sysc-clipboard/manifest/v1"
	payloadPurpose  = "sysc-clipboard/payload/v1"
)

type Store struct {
	root         string
	entriesDir   string
	manifestPath string
	key          []byte
	now          func() time.Time
	writeFile    func(path string, data []byte, perm fs.FileMode) error
	renameFile   func(oldPath, newPath string) error
}

type manifest struct {
	Version int              `json:"version"`
	Entries []protocol.Entry `json:"entries"`
}

type LoadResult struct {
	Items               []history.Item
	Dropped             []string
	ManifestCorrupt     bool
	CorruptManifestPath string
}

func New(root string, key []byte) (*Store, error) {
	if len(key) != KeyBytes {
		return nil, ErrInvalidKey
	}
	if root == "" || !filepath.IsAbs(root) {
		return nil, ErrStateDirectory
	}
	if err := ensurePrivateDir(root); err != nil {
		return nil, err
	}
	entriesDir := filepath.Join(root, entriesName)
	if err := ensurePrivateDir(entriesDir); err != nil {
		return nil, err
	}
	return &Store{
		root:         root,
		entriesDir:   entriesDir,
		manifestPath: filepath.Join(root, manifestName),
		key:          append([]byte(nil), key...),
		now:          time.Now,
	}, nil
}

func (store *Store) Save(items []history.Item) error {
	entries := make([]protocol.Entry, 0, len(items))
	validated := make([]history.Item, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		valid, err := history.NewItem(item.Metadata(), item.Payload())
		if err != nil {
			return err
		}
		metadata := valid.Metadata()
		if _, ok := seen[metadata.ID]; ok {
			return fmt.Errorf("duplicate clipboard entry ID %q", metadata.ID)
		}
		seen[metadata.ID] = struct{}{}
		entries = append(entries, metadata)
		validated = append(validated, valid)
	}
	if len(entries) > protocol.MaxEntries {
		return ErrStorageLimit
	}
	if err := protocol.ValidateSnapshot(protocol.Snapshot{
		Entries:     entries,
		Persistence: protocol.PersistenceDurable,
		Wayland:     protocol.WaylandReady,
	}); err != nil {
		return err
	}

	manifestData, err := json.Marshal(manifest{Version: manifestVersion, Entries: entries})
	if err != nil {
		return fmt.Errorf("encode clipboard manifest: %w", err)
	}
	sealedManifest, err := seal(store.key, manifestPurpose, "", manifestData)
	if err != nil {
		return err
	}

	type stagedFile struct {
		temporary string
		final     string
	}
	staged := make([]stagedFile, 0, len(validated))
	defer func() {
		for _, file := range staged {
			_ = os.Remove(file.temporary)
		}
		_ = os.Remove(store.manifestPath + ".new")
	}()

	for _, item := range validated {
		metadata := item.Metadata()
		final := filepath.Join(store.entriesDir, metadata.ID+".enc")
		info, statErr := os.Lstat(final)
		switch {
		case statErr == nil:
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
				return fmt.Errorf("unsafe clipboard payload file %s", final)
			}
			continue
		case !errors.Is(statErr, fs.ErrNotExist):
			return fmt.Errorf("inspect clipboard payload file: %w", statErr)
		}

		sealed, err := seal(store.key, payloadPurpose, metadata.ID, item.Payload())
		if err != nil {
			return err
		}
		temporary := final + ".new"
		if err := removeIfExists(temporary); err != nil {
			return err
		}
		if err := store.writeStaged(temporary, sealed, 0600); err != nil {
			_ = os.Remove(temporary)
			return fmt.Errorf("stage clipboard payload: %w", err)
		}
		staged = append(staged, stagedFile{temporary: temporary, final: final})
	}

	manifestTemporary := store.manifestPath + ".new"
	if err := removeIfExists(manifestTemporary); err != nil {
		return err
	}
	if err := store.writeStaged(manifestTemporary, sealedManifest, 0600); err != nil {
		_ = os.Remove(manifestTemporary)
		return fmt.Errorf("stage clipboard manifest: %w", err)
	}

	for _, file := range staged {
		if err := store.rename(file.temporary, file.final); err != nil {
			return fmt.Errorf("commit clipboard payload: %w", err)
		}
	}
	if err := store.rename(manifestTemporary, store.manifestPath); err != nil {
		return fmt.Errorf("commit clipboard manifest: %w", err)
	}
	if err := syncDirectory(store.root); err != nil {
		return fmt.Errorf("sync clipboard state directory: %w", err)
	}
	store.garbageCollect(entries)
	return nil
}

func (store *Store) Load() (LoadResult, error) {
	encrypted, err := readPrivateFile(store.manifestPath, protocol.MaxFrame+32)
	if errors.Is(err, fs.ErrNotExist) {
		return LoadResult{}, nil
	}
	if err != nil {
		return store.corruptManifest(fmt.Errorf("read manifest: %w", err))
	}
	data, err := open(store.key, manifestPurpose, "", encrypted)
	if err != nil {
		return store.corruptManifest(err)
	}
	file, err := decodeManifest(data)
	if err != nil {
		return store.corruptManifest(err)
	}

	result := LoadResult{Items: make([]history.Item, 0, len(file.Entries))}
	for _, metadata := range file.Entries {
		path := filepath.Join(store.entriesDir, metadata.ID+".enc")
		encryptedPayload, err := readPrivateFile(path, int64(protocol.MaxImageBytes)+int64(cipherOverhead()))
		if err != nil {
			result.Dropped = append(result.Dropped, metadata.ID)
			continue
		}
		payload, err := open(store.key, payloadPurpose, metadata.ID, encryptedPayload)
		if err != nil {
			result.Dropped = append(result.Dropped, metadata.ID)
			continue
		}
		item, err := history.NewItem(metadata, payload)
		if err != nil {
			result.Dropped = append(result.Dropped, metadata.ID)
			continue
		}
		result.Items = append(result.Items, item)
	}
	return result, nil
}

func seal(key []byte, purpose, entryID string, plaintext []byte) ([]byte, error) {
	ciphertext, err := newCipher(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, ciphertext.NonceSize())
	if _, err := cryptorand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate clipboard nonce: %w", err)
	}
	sealed := make([]byte, len(nonce))
	copy(sealed, nonce)
	return ciphertext.Seal(sealed, nonce, plaintext, associatedData(purpose, entryID)), nil
}

func open(key []byte, purpose, entryID string, sealed []byte) ([]byte, error) {
	ciphertext, err := newCipher(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < ciphertext.NonceSize()+ciphertext.Overhead() {
		return nil, ErrAuthentication
	}
	nonce := sealed[:ciphertext.NonceSize()]
	plaintext, err := ciphertext.Open(nil, nonce, sealed[ciphertext.NonceSize():], associatedData(purpose, entryID))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAuthentication, err)
	}
	return plaintext, nil
}

func newCipher(key []byte) (cipher.AEAD, error) {
	if len(key) != KeyBytes {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrInvalidKey
	}
	return cipher.NewGCM(block)
}

func associatedData(purpose, entryID string) []byte {
	return []byte(purpose + "\x00" + entryID)
}

func cipherOverhead() int {
	block, _ := aes.NewCipher(make([]byte, KeyBytes))
	ciphertext, _ := cipher.NewGCM(block)
	return ciphertext.NonceSize() + ciphertext.Overhead()
}

func decodeManifest(data []byte) (manifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var file manifest
	if err := decoder.Decode(&file); err != nil {
		return manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return manifest{}, errors.New("manifest contains trailing JSON")
		}
		return manifest{}, fmt.Errorf("decode trailing manifest JSON: %w", err)
	}
	if file.Version != manifestVersion {
		return manifest{}, fmt.Errorf("unsupported manifest version %d", file.Version)
	}
	if err := protocol.ValidateSnapshot(protocol.Snapshot{
		Entries:     file.Entries,
		Persistence: protocol.PersistenceDurable,
		Wayland:     protocol.WaylandReady,
	}); err != nil {
		return manifest{}, err
	}
	return file, nil
}

func (store *Store) corruptManifest(cause error) (LoadResult, error) {
	path, err := store.preserveManifest()
	if err != nil {
		return LoadResult{ManifestCorrupt: true}, errors.Join(fmt.Errorf("%w: %v", ErrCorruptManifest, cause), err)
	}
	return LoadResult{ManifestCorrupt: true, CorruptManifestPath: path}, fmt.Errorf("%w: %v", ErrCorruptManifest, cause)
}

func (store *Store) preserveManifest() (string, error) {
	now := store.now
	if now == nil {
		now = time.Now
	}
	base := store.manifestPath + "." + now().UTC().Format("20060102T150405.000000000Z")
	for index := 0; ; index++ {
		path := base + ".corrupt"
		if index > 0 {
			path = fmt.Sprintf("%s-%d.corrupt", base, index)
		}
		_, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			if err := store.rename(store.manifestPath, path); err != nil {
				return "", err
			}
			return path, nil
		}
		if err != nil {
			return "", err
		}
	}
}

func (store *Store) writeStaged(path string, data []byte, perm fs.FileMode) error {
	if store.writeFile != nil {
		return store.writeFile(path, data, perm)
	}
	return writePrivateFile(path, data, perm)
}

func (store *Store) rename(oldPath, newPath string) error {
	if store.renameFile != nil {
		return store.renameFile(oldPath, newPath)
	}
	return os.Rename(oldPath, newPath)
}

func (store *Store) garbageCollect(entries []protocol.Entry) {
	referenced := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		referenced[entry.ID+".enc"] = struct{}{}
	}
	files, err := os.ReadDir(store.entriesDir)
	if err != nil {
		return
	}
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".enc") {
			continue
		}
		if _, ok := referenced[file.Name()]; ok {
			continue
		}
		_ = os.Remove(filepath.Join(store.entriesDir, file.Name()))
	}
}

func writePrivateFile(path string, data []byte, perm fs.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, perm)
	if err != nil {
		return err
	}
	if err := writeAll(file, data); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func readPrivateFile(path string, maxBytes int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, ErrInvalidKeyFile
	}
	if info.Size() > maxBytes {
		return nil, ErrStorageLimit
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrStorageLimit
	}
	return data, nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err != nil {
		return err
	}
	return closeErr
}
