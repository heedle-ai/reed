## 1. Install Podman, socat and a Chromium (Linux)

```bash
sudo apt-get install -y podman socat     # Debian, Ubuntu
podman --version && socat -V | head -1
google-chrome --version || chromium --version
```

Any of Google Chrome, Chromium or Chrome's headless shell will do.

## 2. Build a browser image

```bash
git clone https://github.com/heedle-ai/reed && cd reed
bash scripts/build-image.sh
```

It prints the image's ID: 64 hex characters. That ID is what you give a worker, and what pins the browser: a different Chromium is a different image ID.

The image is `FROM scratch`: the browser's own directory, the libraries it links, fonts, and socat. It runs as an unprivileged user. To use a particular browser, pass its path: `bash scripts/build-image.sh /path/to/chrome`. The script copies the whole folder the binary is in, so it refuses a binary whose folder doesn't look like a Chromium's (no `icudtl.dat` and `.pak` files next to it).

## 3. Run the tests

```bash
go test ./...                                    # everything that needs no browser
REED_IMAGE=<image ID> go test -run TestContainer_ -v .
```

`REED_REQUIRE_PODMAN=1` instead of `REED_IMAGE` builds the image first and fails rather than skips if anything is missing; CI does that.

The container tests start their own web servers in Podman: an *allowed* one whose page tries to reach a *blocked* one by name, by IP address, by WebSocket, with an image and with an iframe. They pass only if the blocked server receives no request at all.

## 4. Use it in a program

A loom worker started with the image can run `browse`:

```bash
loom worker -store ~/heedle-store.git -browser-image <image ID>
loom run '(browse "https://example.org/")'
```

`-browser-network NAME` puts the browsers on a particular Podman network. drawdown takes the same two flags for its built-in worker.

In Go, `reed.Register(registry, worker, reed.Browser{Image: id})` adds the command to a draft registry and its activity to a worker; `TestInv_BrowseInAProgram` shows it with Temporal's test environment. Create that worker with `worker.New(client, queue, reed.WorkerOptions(worker.Options{}))` so live frames reach Temporal every 2 seconds.
