# sysc-clipboard

A clipboard history daemon for Wayland, written in Go. It keeps what you copy, text and images,
encrypted on disk, and serves it to the clipboard panel in [sysc-shell](https://github.com/Nomadcxx/sysc-shell).

## Features

- **History**: the last 100 entries, text and images, up to 256 MiB in total
- **Restore**: puts an entry back on the clipboard with its original MIME type
- **Pins**: pinned entries stay put when you clear the rest
- **Encrypted at rest**: each entry and the manifest are separate encrypted files, and no plaintext
  payload is ever written. The key lives in your keyring, or in a key file for headless setups
- **Survives restarts**: history comes back after a restart or a reboot
- **Private socket**: clients get metadata, previews and thumbnails. The clipboard bytes never leave
  the daemon
- **Compositor support**: any compositor that offers `ext-data-control-v1`, with
  `wlr-data-control-unstable-v1` as the fallback

## Installation

**Requires:** Go 1.26+, a Wayland compositor with data-control, and a Secret Service provider such
as gnome-keyring or KeePassXC (or see [Key and state](#key-and-state) to run without one).

### Build from Source

```bash
git clone https://github.com/Nomadcxx/sysc-clipboard
cd sysc-clipboard
go build -o ~/.local/bin/sysc-clipboard ./cmd/sysc-clipboard
```

### Via Go

```bash
GOBIN="$HOME/.local/bin" go install github.com/Nomadcxx/sysc-clipboard/cmd/sysc-clipboard@latest
```

### Run it as a user service

The unit in `contrib/` starts the daemon at login and restarts it if it dies:

```bash
install -Dm644 contrib/sysc-clipboard.service ~/.config/systemd/user/sysc-clipboard.service
systemctl --user daemon-reload
systemctl --user enable --now sysc-clipboard.service
```

The unit expects the binary at `~/.local/bin/sysc-clipboard`.

## Usage

The daemon has no interface of its own. Copy things as usual and open the clipboard panel in
sysc-shell to browse, restore, pin or delete them.

Check where history lives and whether it will persist, without starting the daemon:

```bash
$ sysc-clipboard --check
state-dir: ~/.local/state/sysc-clipboard
socket: /run/user/1000/sysc-clipboard/control.v1.sock
entries: 1
persistence: durable
```

`durable` means history is encrypted on disk and will come back after a restart. Anything else
means the daemon is running on memory alone, and the journal says why:

```bash
journalctl --user -u sysc-clipboard
```

## Key and state

On first start the daemon creates a random 32-byte key and stores it in your keyring as
`sysc-clipboard encryption key`. After that it reads the same key back on every start.

If your keyring is locked, or there is no Secret Service at all, history still works but only lives in
memory until the daemon stops. For headless sessions, use a key file instead:

```bash
sysc-clipboard --key-file ~/.config/sysc-clipboard/clipboard.key
```

The daemon creates that file with mode `0600` inside a `0700` directory. If the file already exists it
must hold exactly 32 bytes and have those permissions, or the daemon refuses it.

State lives in `$XDG_STATE_HOME/sysc-clipboard`, or `~/.local/state/sysc-clipboard` when
`XDG_STATE_HOME` is unset. Pass `--state-dir` with an absolute path to put it somewhere else.

## Clients

Clients connect to `$XDG_RUNTIME_DIR/sysc-clipboard/control.v1.sock`. The socket and its directory
are private to your user. The protocol carries metadata, previews and thumbnails, plus restore, pin,
delete and clear requests. Go clients can use the versioned `client` package:

```bash
go get github.com/Nomadcxx/sysc-clipboard/client
```

sysc-shell connects as a client. It does not start or supervise the daemon, so run the user service
above.

Primary selection (middle-click paste) is not captured.

Selections a password manager marks as secret (the `x-kde-passwordManagerHint` type, set by KeePassXC,
Bitwarden and others) are never recorded.

## Development

```bash
gofmt -l .
go vet ./...
go test -race -count=1 ./...
```

The Secret Service test runs a private `dbus-daemon` and skips itself when `dbus-daemon` is not
installed.

## License

BSD-3-Clause

---

<a href="https://github.com/Nomadcxx"><img src="https://raw.githubusercontent.com/Nomadcxx/Nomadcxx/main/assets/rama-mark.svg" height="22" alt="RAMA"></a> — terminal-native tooling for the linux desktop.
[More projects →](https://github.com/Nomadcxx) · [Sponsor](https://github.com/sponsors/Nomadcxx) ❤️
