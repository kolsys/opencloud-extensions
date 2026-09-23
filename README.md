# OpenCloud extensions

Two services that run beside [OpenCloud](https://github.com/opencloud-eu/opencloud)
and add what it does not do itself. No patches to the platform, no database:
state lives in S3 and in the JetStream the platform already runs.

| Module | What it adds |
|---|---|
| [video-thumbnails](video-thumbnails/) | Thumbnails for video files, in the web client and over HTTP. |
| [file-activity](file-activity/) | An API for tracking file changes, and a backup of the tree into S3. |
| `common` | Shared code: config, observability, CS3 gateway, S3, JetStream, HTTP. |

## video-thumbnails

**Adds thumbnails for video files to OpenCloud**, in the web client and over
HTTP.

- A frame is rendered with ffmpeg when an upload finishes, kept as one master
  in S3 and resized per request.
- A web app for the client comes with the service and is served by it, so
  nothing is mounted into the platform.
- It answers in the shape the web expects: `425` with `Retry-After` while a
  thumbnail is being made, `304`, the DAV error document on `404`.
- Public links work: a signed request is authorised with `HEAD`, which is what
  the platform accepts for a link.
- Vertical video keeps its face — a frame that would lose too much to a crop
  is fitted onto a blurred background instead.
- `import` takes thumbnails made elsewhere, `resync` finds videos without one.

## file-activity

**Adds an API for tracking file changes in OpenCloud**, and a backup of the
tree into S3 on the fly.

- One entry per change — created, moved, trashed, restored, purged — with the
  path, the id, the size, the checksum and the blob key. A deleted folder
  becomes one entry per file inside it.
- The entries survive a client being offline: they are kept in a stream of
  their own, not handed out once as the events of the platform are.
- A cursor, not a subscription: a client reads from where it stopped. A cursor
  older than the retention is answered with `410` instead of silently skipping.
- Optional push over a webhook, signed with HMAC-SHA256.
- **Backup of the tree into S3 as it happens**: an object per file with its
  blob key, which is the only place that key stays readable after the upload.
  With it `restore` writes a space back into any WebDAV endpoint even when the
  metadata of the platform is gone, and `resync` rebuilds the copy from the
  platform.

## Building

Go 1.26 and, for the web app of video-thumbnails, node 22. The repository is a
workspace of three modules; the root is not one, so every target walks them in
turn.

```
make build        # binaries into bin/
make test lint    # tests and golangci-lint for all three modules
make web          # the web app of video-thumbnails
make images       # docker images; the image builds the web app itself
```

The build context of an image is the root of the repository, because every
service is built together with `common`:

```
docker build -f video-thumbnails/Dockerfile -t video-thumbnails .
```

## Running them

Both services need a running OpenCloud: its NATS for events, its CS3 gateway
for a service account, its proxy for authenticating a request. Each module's
README lists the environment, the commands and the routes the proxy has to
carry. `dev/` holds the development stand — the platform, minio and both
services in docker compose — which is the quickest way to see them work.

## Contributing

Issues and pull requests are welcome. [AGENTS.md](AGENTS.md) has the layout,
the commands and the conventions: `make test lint` is the gate, and a change
that rests on how OpenCloud behaves names the file it was checked against.

## Licence

MIT, see [LICENSE](LICENSE).
