#!/bin/sh
set -eu
# PostgreSQL itself reads POSTGRES_PASSWORD_FILE. This only creates the separate
# Keycloak role/database; credentials are not printed and SQL logging is off.
password=$(cat /run/secrets/keycloak_db_password)
psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --set=ON_ERROR_STOP=1 --set=kc_password="$password" <<'SQL'
SELECT format('CREATE ROLE keycloak LOGIN PASSWORD %L', :'kc_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'keycloak') \gexec
SELECT 'CREATE DATABASE keycloak OWNER keycloak'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'keycloak') \gexec
SQL
