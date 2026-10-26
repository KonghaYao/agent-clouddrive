#!/bin/sh
set -eu
export IDM_ADMIN_PASSWORD="$(cat /run/secrets/ocis_admin_password)"
if [ "$STORAGE_USERS_DRIVER" = s3ng ]; then
  export STORAGE_USERS_S3NG_ACCESS_KEY="$(cat /run/secrets/s3_access_key)"
  export STORAGE_USERS_S3NG_SECRET_KEY="$(cat /run/secrets/s3_secret_key)"
fi
# Only initialize a genuinely empty config. Never hide initialization errors.
if [ ! -f /etc/ocis/ocis.yaml ]; then ocis init; fi
# Do not invoke revisions purge, trash-bin purge, or DELETE maintenance jobs.
exec ocis server
