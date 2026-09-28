package store

import (
	"bufio"
	"bytes"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestSecretServiceKeyIsCreatedOnceAndReadBack(t *testing.T) {
	fake := startFakeSecretService(t)

	created, err := loadSecretServiceKey()
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if len(created) != KeyBytes {
		t.Fatalf("created key has %d bytes, want %d", len(created), KeyBytes)
	}
	read, err := loadSecretServiceKey()
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if !bytes.Equal(read, created) {
		t.Fatal("second load returned a different key")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.items) != 1 {
		t.Fatalf("keyring holds %d items, want 1", len(fake.items))
	}
	if fake.opened != 2 || fake.closed != 2 {
		t.Fatalf("sessions opened %d, closed %d; want 2 and 2", fake.opened, fake.closed)
	}
}

// fakeSecretService is the part of the freedesktop Secret Service API the
// key loader uses, served on a private session bus: plain sessions, item
// search, item creation in the default collection, and secret retrieval.
type fakeSecretService struct {
	conn   *dbus.Conn
	mu     sync.Mutex
	items  map[dbus.ObjectPath]fakeItem
	opened int
	closed int
}

type fakeItem struct {
	attributes map[string]string
	secret     []byte
}

type fakeCollection struct{ service *fakeSecretService }

type fakeSession struct{ service *fakeSecretService }

type fakeItemObject struct {
	service *fakeSecretService
	path    dbus.ObjectPath
}

func startFakeSecretService(t *testing.T) *fakeSecretService {
	t.Helper()
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("dbus-daemon is not installed")
	}
	command := exec.Command("dbus-daemon", "--session", "--nofork", "--print-address=1")
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil {
		t.Fatalf("private bus did not report an address: %v", err)
	}
	address := strings.TrimSpace(line)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", address)

	conn, err := dbus.Connect(address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	fake := &fakeSecretService{conn: conn, items: make(map[dbus.ObjectPath]fakeItem)}
	if err := conn.Export(fake, secretObjectPath, secretServiceIface); err != nil {
		t.Fatal(err)
	}
	if err := conn.Export(fakeCollection{fake}, secretDefaultCollection, secretCollectionIface); err != nil {
		t.Fatal(err)
	}
	reply, err := conn.RequestName(secretBusName, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("own %s: %v, %v", secretBusName, reply, err)
	}
	return fake
}

func (s *fakeSecretService) OpenSession(algorithm string, _ dbus.Variant) (dbus.Variant, dbus.ObjectPath, *dbus.Error) {
	if algorithm != "plain" {
		return dbus.Variant{}, "", dbus.MakeFailedError(dbus.ErrMsgInvalidArg)
	}
	s.mu.Lock()
	s.opened++
	path := dbus.ObjectPath("/org/freedesktop/secrets/session/s" + string(rune('0'+s.opened)))
	s.mu.Unlock()
	if err := s.conn.Export(fakeSession{s}, path, "org.freedesktop.Secret.Session"); err != nil {
		return dbus.Variant{}, "", dbus.MakeFailedError(err)
	}
	return dbus.MakeVariant(""), path, nil
}

func (s *fakeSecretService) SearchItems(attributes map[string]string) ([]dbus.ObjectPath, []dbus.ObjectPath, *dbus.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlocked := []dbus.ObjectPath{}
	for path, item := range s.items {
		matches := true
		for key, value := range attributes {
			if item.attributes[key] != value {
				matches = false
			}
		}
		if matches {
			unlocked = append(unlocked, path)
		}
	}
	return unlocked, []dbus.ObjectPath{}, nil
}

func (c fakeCollection) CreateItem(properties map[string]dbus.Variant, secret secretValue, _ bool) (dbus.ObjectPath, dbus.ObjectPath, *dbus.Error) {
	attributes, ok := properties[secretItemIface+".Attributes"].Value().(map[string]string)
	if !ok {
		return "", "", dbus.MakeFailedError(dbus.ErrMsgInvalidArg)
	}
	c.service.mu.Lock()
	path := dbus.ObjectPath("/org/freedesktop/secrets/collection/login/i" + string(rune('0'+len(c.service.items)+1)))
	c.service.items[path] = fakeItem{attributes: attributes, secret: append([]byte(nil), secret.Value...)}
	c.service.mu.Unlock()
	if err := c.service.conn.Export(fakeItemObject{c.service, path}, path, secretItemIface); err != nil {
		return "", "", dbus.MakeFailedError(err)
	}
	return path, "/", nil
}

func (i fakeItemObject) GetSecret(session dbus.ObjectPath) (secretValue, *dbus.Error) {
	i.service.mu.Lock()
	defer i.service.mu.Unlock()
	return secretValue{Session: session, Value: i.service.items[i.path].secret, ContentType: "application/octet-stream"}, nil
}

func (s fakeSession) Close() *dbus.Error {
	s.service.mu.Lock()
	s.service.closed++
	s.service.mu.Unlock()
	return nil
}
