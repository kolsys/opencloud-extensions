#!/usr/bin/env bash
# Issue an app token for a user of the stand and print it.
#
# Without Keycloak the administrator issues it through impersonation, which
# the stand enables with AUTH_APP_ENABLE_IMPERSONATION, over basic auth,
# which it enables with PROXY_ENABLE_BASIC_AUTH. Neither belongs on a
# production instance and both are off once dev/oidc.env is in place:
# there the user asks Keycloak for an access token of their own, which needs
# the direct access grant on the client, and issues the app token with it.
#
# Usage: token.sh [user] [expiry]
#   with Keycloak: OIDC_PASSWORD=... token.sh <user>
set -euo pipefail

cd "$(dirname "$0")/.."
# shellcheck disable=SC1091
[ -f .env ] && . ./.env
# shellcheck disable=SC1091
[ -f oidc.env ] && . ./oidc.env

USER_NAME="${1:-alan}"
EXPIRY="${2:-72h}"
OC_URL="${OC_URL:-https://localhost:9200}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-admin}"

field() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p"; }

if [ -n "${OC_OIDC_ISSUER:-}" ]; then
  client=${WEB_OIDC_CLIENT_ID:?set WEB_OIDC_CLIENT_ID in dev/oidc.env}
  password=${OIDC_PASSWORD:?set OIDC_PASSWORD to the Keycloak password of ${USER_NAME}}

  endpoint=$(curl -fsS "${OC_OIDC_ISSUER}/.well-known/openid-configuration" | field token_endpoint)
  [ -n "$endpoint" ] || { echo "token.sh: no token endpoint at ${OC_OIDC_ISSUER}" >&2; exit 1; }

  grant=$(curl -fsS -X POST "$endpoint" \
    --data-urlencode "grant_type=password" \
    --data-urlencode "client_id=${client}" \
    --data-urlencode "scope=openid profile email" \
    --data-urlencode "username=${USER_NAME}" \
    --data-urlencode "password=${password}")
  access=$(printf '%s' "$grant" | field access_token)
  [ -n "$access" ] || { echo "token.sh: no access token in the answer: $grant" >&2; exit 1; }

  response=$(curl -kfsS -X POST \
    -H "Authorization: Bearer ${access}" \
    "${OC_URL}/auth-app/tokens?expiry=${EXPIRY}&label=stand")
else
  response=$(curl -kfsS -X POST \
    -u "admin:${ADMIN_PASSWORD}" \
    "${OC_URL}/auth-app/tokens?expiry=${EXPIRY}&userName=${USER_NAME}&label=stand")
fi

token=$(printf '%s' "$response" | field token)
if [ -z "$token" ]; then
  echo "token.sh: no token in the response: $response" >&2
  exit 1
fi

echo "$token"
