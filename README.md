# sysc-clipboard

`sysc-clipboard` is a per-user Wayland clipboard-history daemon. It captures
regular clipboard selections, keeps a bounded history, restores the original
MIME type, and stores entries as encrypted files.

The daemon owns clipboard bytes. Its Unix socket exposes metadata, previews,
and actions such as restore, pin, delete, clear, and thumbnail. Clients never
receive raw history payloads.

## Install

Build the daemon and place it on the user path:

```sh
GOBIN="$HOME/.local/bin" go install github.com/Nomadcxx/sysc-clipboard/cmd/sysc-clipboard@latest
```

Install the user service, then start it:

```sh
mkdir -p ~/.config/systemd/user
cp contrib/sysc-clipboard.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now sysc-clipboard.service
```

The service expects the binary at `~/.local/bin/sysc-clipboard`, which is the
default destination used by `go install` when `GOBIN` is not set.

## Key and state

The daemon uses a Secret Service item named `sysc-clipboard encryption key` by
default. Headless sessions can use a private key file instead:

```sh
sysc-clipboard --key-file ~/.config/sysc-clipboard/clipboard.key
```

The daemon creates that file with mode `0600` inside a `0700` directory. An
existing key file must contain exactly 32 bytes and meet those permissions.

State lives under `$XDG_STATE_HOME/sysc-clipboard`, or
`$HOME/.local/state/sysc-clipboard` when `XDG_STATE_HOME` is unset. Override it
with an absolute path using `--state-dir`.

If the key store or state directory is unavailable, the daemon keeps usable
history in memory and reports persistence as unavailable or volatile. It does
not write plaintext payloads.

Check the configured paths and persistence status without starting the daemon:

```sh
sysc-clipboard --check
```

## Socket and compositor

Clients connect to `$XDG_RUNTIME_DIR/sysc-clipboard/control.v1.sock`. The
socket and its parent directory are private to the current user.

The daemon needs a Wayland `wl_seat` and either
`ext_data_control_manager_v1` or `zwlr_data_control_manager_v1`. It prefers the
ext protocol and falls back to the wlroots protocol. Primary selection is not
captured.

`sysc-shell` connects as a client and does not launch or supervise this
daemon. Other metadata-only clients can use the versioned Go `client` package.

## Development

```sh
gofmt -w .
go test -race -count=1 ./...
go vet ./...
go build ./...
```
