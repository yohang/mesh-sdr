# MeshSDR

A web SDR for radio hobbyists: listen to your software-defined radios from a browser, on one machine or spread over several.

- **Hub**: web UI, accounts, settings and the database (SQLite). It is the only public entry point.
- **Nodes**: run the SDR devices (RTL-SDR, rtl_tcp) and the DSP, and stream the waterfall and audio to listeners through the hub.
- **One binary**, `meshsdr`, with three roles: `hub`, `node`, or `all` (hub plus a local node, the usual single-box setup).

> Status: early development. The foundations are done (accounts, multi-node grid, spectrum and NFM audio on the node); the receiver page in the browser is next. See the [milestones](https://github.com/yohang/mesh-sdr/milestones).

## Quick start (Docker)

```sh
docker build -f .infra/docker/Dockerfile --target prod -t meshsdr .

# Config lives in /etc/meshsdr (TOML) or MESHSDR_* env vars; data in /var/lib/meshsdr.
docker volume create meshsdr-data
docker run --rm -v meshsdr-data:/var/lib/meshsdr \
  -e MESHSDR_HUB__URL=http://localhost:8073 -e MESHSDR_HUB__ALLOW_INSECURE_URL=true \
  meshsdr hub migrate

docker run --init -p 8073:8073 -v meshsdr-data:/var/lib/meshsdr \
  --device /dev/bus/usb \
  -e MESHSDR_HUB__URL=http://localhost:8073 -e MESHSDR_HUB__ALLOW_INSECURE_URL=true \
  -e MESHSDR_GATEWAY__TLS_MODE=off -e MESHSDR_GATEWAY__HTTP_LISTEN=:8073 \
  meshsdr all
```

On first start the hub prints a one-time **setup link** on its console to create the admin account. Open http://localhost:8073.

For a public station, set `hub.url` to your HTTPS address and use `gateway.tls_mode = "acme"` (Let's Encrypt), `"files"` (your certificate), or `"off"` behind your own reverse proxy. Devices are declared in `node.toml`. Both files are documented in [`.infra/config/`](.infra/config/).

### More nodes

```sh
meshsdr hub node add shack --url https://shack.lan:8074   # prints a single-use token
meshsdr node enroll --ca-fingerprint <printed fingerprint>  # on the node, with the token in its config
meshsdr node
```

Nodes talk to the hub over mutual TLS; browsers only ever talk to the hub.

## Development

Everything runs in Docker; no local Go toolchain is needed.

```sh
make run     # build, generate, migrate, start the dev stack on http://localhost:3000
make test    # tests
make lint    # linters
make help    # everything else
```

The dev stack runs `meshsdr all` with hot reload and a synthetic radio device, so the spectrum works without hardware. VS Code users can "Reopen in Container".

Read [AGENTS.md](AGENTS.md) before contributing: it holds the architecture, conventions and commands. Design decisions are in [docs/adr/](docs/adr/), the product specification in [docs/spec/](docs/spec/).

## Stack

Go, SQLite, templ + htmx, Tailwind CSS. DSP through [csdr](https://github.com/jketterl/csdr) and [owrx_connector](https://github.com/jketterl/owrx_connector).

## Licence

AGPL-3.0-or-later. See [LICENSE](LICENSE).
