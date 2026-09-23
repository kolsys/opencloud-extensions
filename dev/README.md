# The stand

A local installation of the platform with both extensions, for development and
for checking a change before it is promoted. It is not a production
installation: that one lives in its own repository with its own copies of the
configuration, and nothing here reaches it by itself.

| Path | What |
|---|---|
| `docker-compose.yml` | Platform, minio, both extensions, built from this repository. |
| `.env.example` | Versions, URLs, secrets, buckets. Copy to `.env`. |
| `stand.env` | Basic auth and impersonation, so the scripts can issue a token. |
| `oidc.env.example` | Optional: run the stand against Keycloak instead of the builtin provider. |
| `opencloud/proxy.yaml` | Routes of the proxy plus the role mapping. **Generated**, see below. |
| `opencloud/web.yaml` | The web loads the plugin of video-thumbnails. |
| `opencloud/csp.yaml` | The page may reach the identity provider. |
| `opencloud/role-mapping.yaml` | Which claim value becomes which role of the platform. |
| `scripts/gen-proxy-routes.sh` | Regenerates `opencloud/proxy.yaml` from the platform source. |
| `scripts/token.sh` | Issues an app token for a user of the stand. |

## Running it

`make up` from the root of the repository builds the images and starts
everything at `https://localhost:9200` with a self signed certificate.

Without `oidc.env` the stand runs on the builtin identity provider with the
demo users of the platform — `alan`, `mary`, `margaret`, `dennis`, `lynn`,
password `demo`, and `admin` with `ADMIN_PASSWORD`. That is the mode to
develop in: basic auth works, so `scripts/token.sh [user] [expiry]` issues an
app token without a browser.

## Against Keycloak

`cp oidc.env.example oidc.env` and restart the platform. The file switches the
issuer, the autoprovisioning, the role and group claim, disables the builtin
provider, and turns basic auth off together with impersonation — basic auth
cannot live next to autoprovisioning, the claims the platform builds for such
a request carry no `name` and every one of them fails. `token.sh` then asks
Keycloak for a token of the user through the direct access grant, which the
client has to allow:

```
OIDC_PASSWORD=… scripts/token.sh <user>
```

The redirect URIs of the stand have to be registered in the client next to
those of production: `https://localhost:9200/oidc-callback.html`,
`…/oidc-silent-redirect.html`, post logout `https://localhost:9200/`, web
origin `https://localhost:9200`.

## Regenerating `opencloud/proxy.yaml`

The proxy takes over a default route only through the full `policies` block,
so the file lists every route of the pinned release with ours applied. After a
bump of `OC_DOCKER_TAG` in `.env`:

```
scripts/gen-proxy-routes.sh
```

It reads the routes from the source of the platform on GitHub at that tag,
refuses to write if the three preview routes or the root route moved, and
appends `opencloud/role-mapping.yaml`, which the proxy reads from a file only.
Never edit `proxy.yaml` by hand: change the script or the mapping.

## What the stand is good for

- The plugin of video-thumbnails in a real web client, previews of video,
  public links, the 425 while a thumbnail is being made.
- The feed of file-activity, the tree copy in minio, `resync` and `restore`.
- The mapping of roles and groups, once `oidc.env` is in place.
- Measuring: the platform writes what it does to the log of the container, and
  minio shows every object the extensions write.

What it cannot show: the storage class of a bucket, since minio has none, the
behaviour of a cold read, and anything about the production host.

Branding is not here: a theme belongs to an installation. To see one, put its
files under `opencloud/theme`, mount that at `/etc/opencloud/themes` and set
`WEB_ASSET_THEMES_PATH` with `WEB_UI_THEME_PATH`.
