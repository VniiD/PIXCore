#!/usr/bin/env bash
# Gera PKI e segredos APENAS PARA DESENVOLVIMENTO em deploy/.dev (ignorado pelo git).
# Produção: use a PKI da organização (ex.: cert-manager/Vault) e rotação automática.
set -euo pipefail
out="$(cd "$(dirname "$0")/.." && pwd)/deploy/.dev"
mkdir -p "$out/certs" "$out/secrets"
cd "$out/certs"
umask 077

if [ ! -f ca.key ]; then
  openssl ecparam -name prime256v1 -genkey -noout -out ca.key
  openssl req -x509 -new -key ca.key -sha256 -days 30 -subj "/CN=pixcore-dev-ca" -out ca.crt
fi

issue() { # nome  extKeyUsage  [SAN]
  local name="$1" eku="$2" san="${3:-}"
  [ -f "$name.crt" ] && return
  openssl ecparam -name prime256v1 -genkey -noout -out "$name.key"
  openssl req -new -key "$name.key" -subj "/CN=$name" -out "$name.csr"
  { echo "extendedKeyUsage=$eku"; echo "keyUsage=digitalSignature"; [ -n "$san" ] && echo "subjectAltName=$san"; } > "$name.ext"
  openssl x509 -req -in "$name.csr" -CA ca.crt -CAkey ca.key -CAcreateserial -days 30 -sha256 -extfile "$name.ext" -out "$name.crt" 2>/dev/null
  rm -f "$name.csr" "$name.ext"
}
issue ledger-service serverAuth "DNS:ledger-service,DNS:localhost,IP:127.0.0.1"
issue admin clientAuth
issue payment-service clientAuth
# Somente dev: leitura liberada para o usuário não-root dos contêineres.
chmod 644 ./*.crt ./*.key

cd "$out/secrets"
gen() { [ -f "$1" ] || { head -c 24 /dev/urandom | base64 | tr -d '=+/\n' > "$1"; }; }
gen owner_password; gen app_password
if [ ! -f hmac_keys.json ]; then
  printf '{"signing_key_id":"k1","keys":{"k1":"%s"}}' "$(head -c 32 /dev/urandom | base64)" > hmac_keys.json
fi
printf 'postgres://ledger_app:%s@postgres:5432/ledger?sslmode=disable' "$(cat app_password)" > database_url
printf 'postgres://ledger_owner:%s@postgres:5432/ledger?sslmode=disable' "$(cat owner_password)" > migrate_database_url
chmod 644 ./*
echo "dev certs and secrets written to $out"
