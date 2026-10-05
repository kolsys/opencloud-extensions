# video-thumbnails

Thumbnails for video files in OpenCloud, in the web client and over HTTP.

The service listens to the upload events of the platform, renders one frame
with ffmpeg, keeps that master in S3 and resizes it per request. It answers
the preview routes of the platform and passes everything that is not a video
on to the webdav service, so the client sees one endpoint. A web app that
makes the client ask for video previews comes with it and is served by it.

## How it works

1. The platform publishes `UploadReady`. The service takes the job from its
   own JetStream stream, so a restart loses nothing.
2. ffmpeg reads a second into the video — over a `Range` request when the
   storage allows it, which is a few megabytes, not the whole file — and the
   frame is stored as one master of 1280 px.
3. A preview request is authorised against the platform with `PROPFIND`, or
   with `HEAD` when it carries the signature of a public link, and answered
   from the master. Sizes are rounded up to a small grid, so the cache stays
   small.
4. While a thumbnail is being made the answer is `425` with `Retry-After`,
   which is what the web client waits on.

Vertical video is fitted onto a blurred copy of itself instead of being
cropped, so a face survives a 16:9 tile.

## Running it

Needs a running OpenCloud (its NATS, its CS3 gateway, its proxy), an S3
bucket and ffmpeg in the image.

```
video-thumbnails serve
```

The proxy of the platform has to send two things to the service: its three
`?preview=1` routes and `/api/video-thumbnails/web/`, the latter unprotected,
because the web client loads the app before anybody is logged in. The web
client loads it through `external_apps` in the configuration of the `web`
service:

```yaml
web:
  config:
    external_apps:
      - id: video-thumbnails
        path: /api/video-thumbnails/web/js/remoteEntry.mjs
```

## Commands

```
serve                                        the service
resync [--space] [--dry-run] [--delete-orphans]
import --space <id|name> --manifest <csv> [--workers] [--dry-run]
version
```

`resync` walks the videos of the platform and the masters in the bucket: it
queues a job for every video without a current master and lists the masters
whose file is gone. Orphans are only deleted with the flag — a file inside a
folder that went to the trash cannot be told from a deleted one, the platform
does not give the ids of trashed children.

`import` stores thumbnails made elsewhere as masters. The manifest is a CSV
with a header naming `path` and `thumb`: the path of the video inside the
space and an HTTP URL of its image. It is RFC 4180, so a path with a comma
goes in quotes. It is read row by row, so its length does not matter; a
malformed row stops the run, and what went through before it stays. Images
are fitted into the master size, never upscaled.

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
| `PLATFORM_INTERNAL_URL` | `https://opencloud:9200` | Proxy of the platform, for `PROPFIND` with the headers of the client. |
| `PLATFORM_WEBDAV_UPSTREAM` | `http://opencloud:9115` | Where previews of everything but video are proxied. |
| `OC_INSECURE` | `false` | Do not verify the certificates of the platform. |
| `OC_LOG_LEVEL`, `VIDEO_THUMBNAILS_LOG_LEVEL` | `info` | Log level. |
| `VIDEO_THUMBNAILS_HTTP_ADDR` | `0.0.0.0:9200` | Address the HTTP server listens on. |
| `VIDEO_THUMBNAILS_WORKERS` | `4` | Workers taking jobs from the queue. |
| `VIDEO_THUMBNAILS_URGENT_WORKERS` | `1` | Workers reserved for previews the web asked for and did not get. |
| `VIDEO_THUMBNAILS_SOURCE` | `auto` | How ffmpeg reads a video: `auto`, `range`, `download`. |
| `VIDEO_THUMBNAILS_TEMP_DIR` | system | Where a downloaded video is put. |
| `VIDEO_THUMBNAILS_MASTER_SIZE` | `1280` | Long side of the master frame. |
| `VIDEO_THUMBNAILS_RESOLUTIONS` | see below | Grid of sizes, `WxH`, comma separated. |
| `VIDEO_THUMBNAILS_VIDEO_EXTENSIONS` | `mp4,mov,m4v,webm,mkv,avi` | Extensions treated as video. |
| `VIDEO_THUMBNAILS_S3_ENDPOINT` | — | URL of the S3 endpoint. |
| `VIDEO_THUMBNAILS_S3_REGION` | `default` | Region of the bucket. |
| `VIDEO_THUMBNAILS_S3_BUCKET` | — | Bucket the masters are kept in. |
| `VIDEO_THUMBNAILS_S3_PREFIX` | `thumbs/` | Key prefix inside the bucket. |
| `VIDEO_THUMBNAILS_S3_ACCESS_KEY`, `..._SECRET_KEY` | — | Credentials for the bucket. |
| `VIDEO_THUMBNAILS_FFMPEG_BIN` | `ffmpeg` | Name or path of the binary. |
| `VIDEO_THUMBNAILS_FFMPEG_TIMEOUT` | `60s` | Time one run may take. |
| `VIDEO_THUMBNAILS_SEEK` | `1s` | Position of the frame taken from the video. |
| `VIDEO_THUMBNAILS_DISK_CACHE_DIR` | `/var/cache/video-thumbnails` | Directory of the disk cache. |
| `VIDEO_THUMBNAILS_DISK_CACHE_BYTES` | `2Gi` | Size of that cache. |

The default grid covers what the web client asks for, from `16x16` to
`7680x4320`, in both orientations; a request is rounded up to the next size on
it. The disk cache is a cache: losing it costs a resize, nothing else.

`/healthz`, `/readyz` and `/metrics` are served on the same address; the
metrics are named `video_thumbnails_*`.
