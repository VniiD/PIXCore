#!/bin/sh
# Cria o papel de aplicação sem privilégios de dono. O dono (POSTGRES_USER) roda as migrações.
set -eu
APP_PASSWORD="$(cat /run/secrets/app_password)"
psql -v ON_ERROR_STOP=1 -v app_pw="$APP_PASSWORD" --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
CREATE ROLE ledger_app LOGIN PASSWORD :'app_pw' NOSUPERUSER NOCREATEDB NOCREATEROLE;
GRANT CONNECT ON DATABASE ledger TO ledger_app;
GRANT USAGE ON SCHEMA public TO ledger_app;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
SQL
