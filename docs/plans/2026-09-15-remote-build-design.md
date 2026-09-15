# oak deploy --remote (build on the box)

`oak deploy` builds the image locally (`docker build --platform linux/amd64`)
and pushes it to the box registry over an `ssh -L 5000` forward. On an Apple
Silicon Mac that build runs under QEMU emulation: slow, and for Elixir/BEAM it
crashes outright (the iagocavalcante.com migration had to be built on the box
by hand). `--remote` moves the build to the box: native amd64, no emulation,
no port-forward.

## Flow

`oak deploy --remote`:
1. Parse oak.toml (as today).
2. Tar the build context (cwd), honoring `.dockerignore`.
3. `POST /apps/{name}/build` with the tar as the body and the dockerfile path
   + build args as headers/query. oakd streams build output back and ends
   with `image <ref>` on success or `error: <msg>`.
4. On success, call the existing `POST /apps/{name}/deploy` with that image ref
   and the oak.toml text — unchanged, so the release-command/boot path is
   identical to a local build.

Local build stays the default; `--remote` is opt-in.

## CLI

- `--remote` flag on `deploy`.
- Tar cwd honoring `.dockerignore` using `github.com/moby/patternmatcher`
  (+ its `ignorefile` reader) — the canonical Docker implementation, so ignore
  semantics match `docker build` exactly. Skip `.git` and the dockerignore
  file itself per Docker's rules the reader already encodes.
- Stream the tar to `/build`; read the streamed output; parse the final
  `image <ref>` / `error:` line, same contract style as `Deploy`.
- Then reuse `client.Deploy` with the returned image ref.

## oakd

New `POST /apps/{name}/build` handler:
- Validate the app name.
- Read the dockerfile path (default `Dockerfile`) and build args (JSON) from
  headers.
- Extract the tar body into a fresh temp dir under the data dir, with a
  path-traversal guard (reject entries escaping the root); remove the dir when
  done (defer).
- Image ref: `<registry>/<app>:<unixnano>` (registry from oakd config,
  `localhost:5000`).
- Run `docker build --platform linux/amd64 -f <ctx>/<dockerfile>
  --build-arg K=V ... -t <image> <ctx>` then `docker push <image>`, streaming
  combined output to the response. oakd runs as root with docker on the box,
  and the registry is local (127.0.0.1:5000), so no auth/forward.
- Finish with `image <image>` or `error: <msg>`.
- Same bearer-auth on the TCP listener / trusted on the unix socket as every
  other route.

## Decisions
- **Opt-in `--remote`**, local default (chosen).
- **Registry push happens on the box** to the local registry; the client never
  needs the `ssh -L 5000` forward for a remote build.
- Build platform stays pinned to `linux/amd64` (the box + guest arch).

## Out of scope
Auto-selecting remote by host arch; caching/layer reuse tuning; buildkit
secrets; multi-arch. A future `install`-style helper could sync large contexts
more efficiently, but a tar stream is fine for these repos (tens of MB).
