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

## 5. The published image

You don't have to build an image to use reed through heedle. `ghcr.io/heedle-ai/reed-browser` is built by `.github/workflows/image.yml`, for Linux amd64 and arm64 (the architecture of a Mac's Podman machine), and heedle pins it by digest, so `heedle up` downloads it once and gives it to its worker.

What's in it is what `build-image.sh` puts in any image, taken from Debian bookworm rather than from your machine: Debian's `chromium` package (Chrome for Testing has no Linux arm64 build), the Debian libraries it links, fontconfig with the DejaVu and Liberation fonts, and socat. It runs as an unprivileged user, from scratch, with nothing else. The Debian container it's assembled in is pinned by digest in the workflow; the packages are Debian's on the day it runs, so two runs give different images, and what heedle pins is the digest of the one that was tested and published.

The workflow runs on a tag `image-v*`, using the workflow in the commit the tag names, and, once it is on `main`, from the Actions tab (**Image → Run workflow**). Each architecture is built on its own runner and must pass the container tests above (`REED_REQUIRE_PODMAN=1`) before it is saved; only then are both pushed by digest, under one manifest list. The run's summary names the manifest list's digest, which is what heedle pins. The tag pushed with it (the `image-v*` tag, or `run-<run ID>`) is only a label.

To assemble an image the same way, `build-image.sh` can write its build context to an empty folder instead of building, so the browser and libraries can come from one machine (the workflow's Debian container) and the build happen on another:

```bash
mkdir context
BUILD_IMAGE_CONTEXT=$PWD/context bash scripts/build-image.sh
podman build --quiet -f context/Containerfile context
```

To use your own image instead of the published one, give its ID to the worker: `-browser-image <image ID>` as in step 4, or after `--` with heedle (`heedle up -- -browser-image <image ID>`).
