#!/bin/sh
# Runs once, as the Postgres superuser, on an empty data dir (the standard
# docker-entrypoint-initdb.d hook). It creates the limited app role and then
# applies schema.sql as the owner, so the app can't alter its own schema.
# On an upgrade that changes schema.sql, apply it again by hand as the owner.
set -e

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
  -c "CREATE ROLE aprova LOGIN PASSWORD '${APROVA_DB_PASSWORD}'" \
  -c "GRANT CONNECT ON DATABASE \"${POSTGRES_DB}\" TO aprova"

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" -f /schema.sql
