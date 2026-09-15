package store

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/godbus/dbus/v5"
)

const KeyBytes = 32

var (
	ErrInvalidKeyFile    = errors.New("invalid clipboard key file")
	ErrSecretUnavailable = errors.New("secret service unavailable")
	ErrSecretLocked      = errors.New("secret service key is locked")
	ErrStateDirectory    = errors.New("invalid clipboard state directory")
)

const (
	secretBusName        = "org.freedesktop.secrets"
	secretObjectPath     = dbus.ObjectPath("/org/freedesktop/secrets")
	secretServiceIface   = "org.freedesktop.Secret.Service"
	secretItemIface      = "org.freedesktop.Secret.Item"
	secretKeyLabel       = "sysc-clipboard encryption key"
	secretAttributeName  = "application"
	secretAttributeValue = "sysc-clipboard"
	secretPurposeName    = "purpose"
	secretPurposeValue   = "clipboard-history-v1"
)

type secretValue struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

func DefaultStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return resolveStateDir(os.Getenv("XDG_STATE_HOME"), home)
}

func resolveStateDir(xdgStateHome, home string) (string, error) {
	base := xdgStateHome
	if base == "" {
		if home == "" || !filepath.IsAbs(home) {
			return "", ErrStateDirectory
		}
		base = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(base) {
		return "", ErrStateDirectory
	}
	return filepath.Join(base, "sysc-clipboard"), nil
}

func LoadKey(path string) ([]byte, error) {
	if path != "" {
		return LoadOrCreateKeyFile(path)
	}
	return loadSecretServiceKey()
}

func LoadKeyFile(path string) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: empty path", ErrInvalidKeyFile)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := checkPrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, fmt.Errorf("%w: %s must be a regular 0600 file", ErrInvalidKeyFile, path)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open clipboard key file: %w", err)
	}
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat clipboard key file: %w", statErr)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		_ = file.Close()
		return nil, fmt.Errorf("%w: key file changed while opening", ErrInvalidKeyFile)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, KeyBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read clipboard key file: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close clipboard key file: %w", closeErr)
	}
	if len(data) != KeyBytes {
		return nil, fmt.Errorf("%w: want %d bytes, got %d", ErrInvalidKeyFile, KeyBytes, len(data))
	}
	return append([]byte(nil), data...), nil
}

func LoadOrCreateKeyFile(path string) ([]byte, error) {
	key, err := LoadKeyFile(path)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	key = make([]byte, KeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate clipboard key: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if errors.Is(err, fs.ErrExist) {
		return LoadKeyFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("create clipboard key file: %w", err)
	}
	if err := writeAll(file, key); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("write clipboard key file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("sync clipboard key file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("close clipboard key file: %w", err)
	}
	return append([]byte(nil), key...), nil
}

func ensurePrivateDir(path string) error {
	if path == "" {
		return ErrStateDirectory
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("create private directory: %w", err)
	}
	return checkPrivateDir(path)
}

func checkPrivateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat private directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("%w: %s must be a private directory", ErrStateDirectory, path)
	}
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if written < 0 || written > len(data) {
			return io.ErrShortWrite
		}
		data = data[written:]
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func loadSecretServiceKey() ([]byte, error) {
	connection, err := dbus.SessionBus()
	if err != nil {
		return nil, fmt.Errorf("%w: connect session bus: %v", ErrSecretUnavailable, err)
	}
	defer connection.Close()
	service := connection.Object(secretBusName, secretObjectPath)
	var output dbus.Variant
	var session dbus.ObjectPath
	var prompt dbus.ObjectPath
	if err := service.Call(secretServiceIface+".OpenSession", 0, "plain", dbus.MakeVariant(""), &output, &session, &prompt).Err; err != nil {
		return nil, fmt.Errorf("%w: open session: %v", ErrSecretUnavailable, err)
	}
	defer service.Call(secretServiceIface+".CloseSession", 0, session)
	if prompt != "" && prompt != "/" {
		return nil, ErrSecretLocked
	}

	attributes := map[string]string{
		secretAttributeName: secretAttributeValue,
		secretPurposeName:   secretPurposeValue,
	}
	var unlocked []dbus.ObjectPath
	var locked []dbus.ObjectPath
	if err := service.Call(secretServiceIface+".SearchItems", 0, attributes, &unlocked, &locked).Err; err != nil {
		return nil, fmt.Errorf("%w: search key: %v", ErrSecretUnavailable, err)
	}
	if len(unlocked) == 0 {
		if len(locked) != 0 {
			return nil, ErrSecretLocked
		}
		key := make([]byte, KeyBytes)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate clipboard key: %w", err)
		}
		properties := map[string]dbus.Variant{
			secretItemIface + ".Label":      dbus.MakeVariant(secretKeyLabel),
			secretItemIface + ".Attributes": dbus.MakeVariant(attributes),
		}
		value := secretValue{Session: session, Value: key, ContentType: "application/octet-stream"}
		var item dbus.ObjectPath
		if err := service.Call(secretServiceIface+".CreateItem", 0, properties, value, false, &item, &prompt).Err; err != nil {
			return nil, fmt.Errorf("%w: create key: %v", ErrSecretUnavailable, err)
		}
		if prompt != "" && prompt != "/" {
			return nil, ErrSecretLocked
		}
		return key, nil
	}

	var value secretValue
	item := connection.Object(secretBusName, unlocked[0])
	if err := item.Call(secretItemIface+".GetSecret", 0, session, &value).Err; err != nil {
		return nil, fmt.Errorf("%w: read key: %v", ErrSecretUnavailable, err)
	}
	if len(value.Value) != KeyBytes {
		return nil, fmt.Errorf("%w: stored key has wrong length", ErrInvalidKeyFile)
	}
	return append([]byte(nil), value.Value...), nil
}
