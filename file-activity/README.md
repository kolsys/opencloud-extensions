# file-activity

An API for tracking file changes in OpenCloud, and a backup of the tree into
S3 as the changes happen.

The service consumes the file events of the platform, writes one entry per
change into a JetStream stream of its own and hands them out over HTTP with a
cursor, so a client that was offline picks up where it stopped.

## The feed

```
GET /api/file-activity?since=<cursor>&limit=<n>
GET /api/file-activity/head
```

An answer carries the entries and a `next`; the client stores `next` and passes
it as `since` the following time, and an empty page means it has caught up.
Entries older than the retention are dropped, and a cursor that points before
the oldest one is answered with `410`: the client has to resync its own state,
because continuing from the oldest entry would silently skip what is missing
between.

A user reads the spaces it is a member of — its personal space and the project
spaces, as `/graph/v1.0/me/drives` of the platform lists them — and the answer
names them in `spaces`. Shares the user received are not followed. Every entry
is published to `file-activity.events.<space>`, so the server of the stream
does the filtering, and once nothing more of those spaces follows `next` jumps
to the head of the feed: a client of quiet spaces keeps its cursor inside the
retention. Membership is the one of the moment of the request, cached for a
minute: a space joined later shows from then on, a space left drops out of
`spaces`. The users in `FILE_ACTIVITY_FULL_FEED_USERS` read every space.

An entry has the type of the change, the path, the space, the id of the file,
its size, mime, mtime, checksums and the blob key. A folder that is deleted or
moved becomes one entry per file inside it, because a consumer that stores
files has nothing to do with folders.

The contract is in [api/openapi.yaml](api/openapi.yaml). Requests go through
the proxy of the platform, which authenticates them; an app token is the usual
way. Set `FILE_ACTIVITY_ALLOWED_USERS` to limit who may read.

Changes can also be pushed: with `FILE_ACTIVITY_WEBHOOK_URL` every batch is
posted to that endpoint, signed with HMAC-SHA256 over the body in
`X-File-Activity-Signature` when a secret is set. The push carries every
space. The feed stays the source of truth — the push is a hint that something
happened.

## The tree in S3

With `FILE_ACTIVITY_TREE_S3_ENDPOINT` set, the service keeps a copy of the
tree of the platform in a bucket:

```
tree/{space}{path}      one object per file: id, blob key, size, mime, etag, mtime, checksums
trash/{space}/{file_id} what went to the trash
spaces/{space}          the spaces themselves
```

This is the only place where the blob key of a file stays readable. The
platform hands it out once, in the upload event, and no API returns it
afterwards, so without this copy a bucket full of blobs cannot be mapped back
onto paths. With it, `restore` writes a space back into any WebDAV endpoint,
reading the blobs straight from the bucket of the platform.

Without the setting the tree is not kept and the feed works as before.

## Running it

Needs a running OpenCloud: its NATS for events, its CS3 gateway for a service
account, its proxy for authenticating a request.

```
file-activity serve
```

The proxy has to send `/api/file-activity` to the service.

## Commands

```
serve                                                  the service
resync [--space <id>] [--metadata <root>] [--dry-run]   rebuild the tree from the platform
restore --to <webdav-url> [--space] [--dir] [--prefix] [--skip-existing] [--dry-run]
tap [--from-start]                                     print the feed as it grows
version
```

`resync` compares the tree with the platform through the gateway and rewrites
what drifted. It takes one space at a time and writes as it goes, so an
interrupted run keeps what it wrote and a rerun reports it as kept; it holds
about 100 bytes per file of the tree, whatever the size of the space. A gateway
or bucket that goes away is waited for with backoff; a space that still fails
is logged, the run goes on with the next one and exits non-zero at the end.
Progress is logged per space and every 10k files. Files that were uploaded
while the service was not running have no blob key in the feed; `--metadata`
points at the metadata directory of the platform, mounted read only, and the
key is read from there — the same `.mpk` attribute the platform's own
consistency check uses. The content of a blob is never read. When the command
runs as a one-off container from the image, `docker run --no-healthcheck`
keeps it from showing unhealthy: the healthcheck of the image probes the HTTP
server, which only `serve` starts.

`restore` writes files back over WebDAV, keeping mtime and checksum. The
target is a template, so spaces can be mapped onto folders of one endpoint.
Anything more exotic — restoring into S3, into a local disk — is `rclone serve
webdav` in front of it.

## Configuration

Everything is environment. The platform block is shared by both extensions.

| Variable | Default | What |
|---|---|---|
| `OC_EVENTS_ENDPOINT` | `opencloud:9233` | NATS of the platform. |
| `OC_EVENTS_CLUSTER` | `opencloud-cluster` | Cluster id of that NATS. |
| `OC_EVENTS_ENABLE_TLS`, `OC_EVENTS_TLS_INSECURE` | `false` | TLS to the NATS. |
| `OC_EVENTS_AUTH_USERNAME`, `OC_EVENTS_AUTH_PASSWORD` | empty | Credentials for the NATS. |
| `OC_GATEWAY_GRPC_ADDR` | `opencloud:9142` | CS3 gateway of the platform. |
| `OC_SERVICE_ACCOUNT_ID`, `OC_SERVICE_ACCOUNT_SECRET` | — | The account the service authenticates with. |
| `PLATFORM_INTERNAL_URL` | `https://opencloud:9200` | Proxy of the platform. |
| `OC_INSECURE` | `false` | Do not verify the certificates of the platform. |
| `OC_LOG_LEVEL`, `FILE_ACTIVITY_LOG_LEVEL` | `info` | Log level. |
| `FILE_ACTIVITY_HTTP_ADDR` | `0.0.0.0:9201` | Address the HTTP server listens on. |
| `FILE_ACTIVITY_ALLOWED_USERS` | empty | User names allowed to read the feed. Empty means everyone. |
| `FILE_ACTIVITY_FULL_FEED_USERS` | empty | User names that read every space. Everyone else reads the spaces they are a member of. |
| `FILE_ACTIVITY_STREAM` | `file-activity` | Name of the stream of the feed. |
| `FILE_ACTIVITY_SUBJECT` | `file-activity.events` | Prefix of the subjects of the feed; the space is the last token. |
| `FILE_ACTIVITY_MAX_AGE` | `2160h` | How long an entry stays readable. |
| `FILE_ACTIVITY_MAX_BYTES` | `10Gi` | Size limit of the stream. |
| `FILE_ACTIVITY_TREE_S3_ENDPOINT` | empty | URL of the S3 endpoint. Empty disables the tree. |
| `FILE_ACTIVITY_TREE_S3_REGION` | `default` | Region of the bucket. |
| `FILE_ACTIVITY_TREE_S3_BUCKET` | — | Bucket the tree is kept in. |
| `FILE_ACTIVITY_TREE_S3_PREFIX` | empty | Key prefix inside the bucket. |
| `FILE_ACTIVITY_TREE_S3_ACCESS_KEY`, `..._SECRET_KEY` | — | Credentials for the bucket. |
| `FILE_ACTIVITY_WEBHOOK_URL` | empty | Endpoint the events are posted to. Empty disables the push. |
| `FILE_ACTIVITY_WEBHOOK_SECRET` | empty | Key the signature of the body is made with. |

`restore` takes two more blocks. The bucket of the platform, where the blobs
are, is `RESTORE_BLOBS_S3_BUCKET` and, when they differ from the tree,
`RESTORE_BLOBS_S3_ENDPOINT`, `..._REGION`, `..._ACCESS_KEY`, `..._SECRET_KEY`.
The endpoint it writes to takes `RESTORE_DEST_USER` and `RESTORE_DEST_PASSWORD`,
or `RESTORE_DEST_BEARER`, and `RESTORE_DEST_INSECURE`.

`/healthz`, `/readyz` and `/metrics` are served on the same address; the
metrics are named `file_activity_*`.
