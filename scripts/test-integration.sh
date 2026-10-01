#!/bin/bash
# Own every resource by a random exact name and label; never accept a DB URL.
set -euo pipefail
source "$(dirname "$0")/common.sh"
case "${DB_DRIVER:-}" in
postgres) allowed=(postgres:16 postgres:18) prefix=dm-pg port_spec=5432/tcp ;;
mysql) allowed=(mysql:8.4 mysql:9.7) prefix=dm-my port_spec=3306/tcp ;;
mariadb) allowed=(mariadb:10.11 mariadb:12.3) prefix=dm-my port_spec=3306/tcp ;;
*) allowed=() ;;
esac
if [[ -n "${CI:-}" || "${#allowed[@]}" == 0 || -n "${DATABASE_URL:-}${DB_URL:-}${DB_HOST:-}${DB_PORT:-}" ]]; then
  echo 'Integration requires DB_DRIVER=postgres, mysql or mariadb, no external endpoint, and a non-CI session.' >&2
  exit 1
fi
images=()
for image in "${allowed[@]}"; do
  if [[ -z "${DB_IMAGE:-}" || "$DB_IMAGE" == "$image" ]]; then
    images+=("$image")
  fi
done
if [[ "${#images[@]}" == 0 ]]; then
  printf 'DB_IMAGE must be one of: %s\n' "${allowed[*]}" >&2
  exit 1
fi
need docker 'Install and start Docker.'
need openssl 'Install the Command Line Tools.'
need python3 'Install Python 3 for fixture manifests.'
docker info >/dev/null
fixture_dir="$(mktemp -d "/tmp/data-mate-$DB_DRIVER.XXXXXXXX")"
fixture_id="$prefix-$(openssl rand -hex 12)"
container_name="${fixture_id}-db"
network_name="${fixture_id}-net"
volume_name="${fixture_id}-data"
label="com.data-mate.fixture=$fixture_id"
test_pid=''
remove_resources() {
  # An unavailable daemon is not evidence that resources have disappeared.
  docker info >/dev/null 2>&1 || return 1
  local kind name template actual
  local failed=0
  for kind in container volume network; do
    case "$kind" in
    container)
      name="$container_name"
      template='{{index .Config.Labels "com.data-mate.fixture"}}'
      ;;
    volume)
      name="$volume_name"
      template='{{index .Labels "com.data-mate.fixture"}}'
      ;;
    network)
      name="$network_name"
      template='{{index .Labels "com.data-mate.fixture"}}'
      ;;
    esac
    if actual="$(docker "$kind" inspect --format "$template" "$name" 2>/dev/null)"; then
      if [[ "$actual" != "$fixture_id" ]]; then
        printf 'Refusing cleanup of resource with different ownership: %s\n' "$name" >&2
        failed=1
      elif [[ "$kind" == container ]]; then
        docker rm -f -v "$name" >/dev/null || failed=1
      else
        docker "$kind" rm "$name" >/dev/null || failed=1
      fi
    fi
  done
  docker info >/dev/null 2>&1 || failed=1
  return "$failed"
}
cleanup() {
  local result=$?
  trap - EXIT INT TERM
  if [[ -n "$test_pid" ]]; then
    kill -TERM -- "-$test_pid" 2>/dev/null || true
    wait "$test_pid" 2>/dev/null || true
  fi
  if remove_resources; then
    rm -rf "$fixture_dir"
    printf 'Fixture cleanup complete: %s\n' "$fixture_id"
  else
    printf 'Fixture cleanup failed; retry exact resources %s and directory %s\n' "$fixture_id" "$fixture_dir" >&2
    result=1
  fi
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
umask 077
openssl rand -hex 32 >"$fixture_dir/admin-password"
openssl rand -hex 32 >"$fixture_dir/reader-password"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=data-mate-fixture-ca -keyout "$fixture_dir/ca.key" -out "$fixture_dir/ca.crt" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -subj /CN=localhost -keyout "$fixture_dir/server.key" -out "$fixture_dir/server.csr" >/dev/null 2>&1
printf 'subjectAltName=DNS:localhost\nextendedKeyUsage=serverAuth\n' >"$fixture_dir/extensions"
openssl x509 -req -in "$fixture_dir/server.csr" -CA "$fixture_dir/ca.crt" -CAkey "$fixture_dir/ca.key" -CAcreateserial -days 1 -extfile "$fixture_dir/extensions" -out "$fixture_dir/server.crt" >/dev/null 2>&1
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=untrusted -keyout "$fixture_dir/bad.key" -out "$fixture_dir/bad.crt" >/dev/null 2>&1
# The in-container client reads the synthetic admin password from an option
# file in the private fixture directory, never from process arguments.
printf '[client]\nuser=root\npassword=%s\nhost=127.0.0.1\nprotocol=TCP\n' "$(cat "$fixture_dir/admin-password")" >"$fixture_dir/client.cnf"
# start_fixture runs the owned server for one image with verified TLS enabled.
start_fixture() {
  local image="$1" tls='mkdir -p /tmp/tls; cp /fixture/ca.crt /fixture/server.key /fixture/server.crt /tmp/tls/'
  case "$DB_DRIVER" in
  postgres)
    docker run -d --name "$container_name" --label "$label" --network "$network_name" \
      --publish 127.0.0.1::5432 --mount "type=volume,src=$volume_name,dst=/var/lib/postgresql" \
      --mount "type=bind,src=$fixture_dir,dst=/fixture,readonly" \
      -e POSTGRES_PASSWORD_FILE=/fixture/admin-password -e POSTGRES_DB=fixture -e PGDATA=/var/lib/postgresql/data \
      --entrypoint /bin/bash "$image" -c "$tls; chown -R postgres:postgres /tmp/tls; chmod 600 /tmp/tls/server.key; exec docker-entrypoint.sh postgres -c ssl=on -c ssl_cert_file=/tmp/tls/server.crt -c ssl_key_file=/tmp/tls/server.key -c log_statement=none -c shared_preload_libraries=pg_stat_statements" >/dev/null
    ;;
  mysql)
    docker run -d --name "$container_name" --label "$label" --network "$network_name" \
      --publish 127.0.0.1::3306 --mount "type=volume,src=$volume_name,dst=/var/lib/mysql" \
      --mount "type=bind,src=$fixture_dir,dst=/fixture,readonly" -e MYSQL_ROOT_PASSWORD_FILE=/fixture/admin-password \
      --entrypoint /bin/bash "$image" -c "$tls; chown -R mysql:mysql /tmp/tls; chmod 600 /tmp/tls/server.key; exec docker-entrypoint.sh mysqld --ssl-ca=/tmp/tls/ca.crt --ssl-cert=/tmp/tls/server.crt --ssl-key=/tmp/tls/server.key" >/dev/null
    ;;
  mariadb)
    docker run -d --name "$container_name" --label "$label" --network "$network_name" \
      --publish 127.0.0.1::3306 --mount "type=volume,src=$volume_name,dst=/var/lib/mysql" \
      --mount "type=bind,src=$fixture_dir,dst=/fixture,readonly" -e MARIADB_ROOT_PASSWORD_FILE=/fixture/admin-password \
      --entrypoint /bin/bash "$image" -c "$tls; chown -R mysql:mysql /tmp/tls; chmod 600 /tmp/tls/server.key; exec docker-entrypoint.sh mariadbd --ssl-ca=/tmp/tls/ca.crt --ssl-cert=/tmp/tls/server.crt --ssl-key=/tmp/tls/server.key" >/dev/null
    ;;
  esac
}
# fixture_ready checks the final server over TCP; image initialization runs a
# temporary server that does not listen on TCP.
fixture_ready() {
  case "$DB_DRIVER" in
  postgres) docker exec "$container_name" pg_isready -h 127.0.0.1 -U postgres -d fixture ;;
  mysql) docker exec "$container_name" mysql --defaults-extra-file=/fixture/client.cnf -e 'SELECT 1' ;;
  mariadb) docker exec "$container_name" mariadb --defaults-extra-file=/fixture/client.cnf -e 'SELECT 1' ;;
  esac
}
for image in "${images[@]}"; do
  docker pull "$image" >/dev/null
  docker image inspect "$image" --format 'Fixture image: {{.Id}} {{json .RepoDigests}}'
  docker network create --label "$label" "$network_name" >/dev/null
  docker volume create --label "$label" "$volume_name" >/dev/null
  start_fixture "$image"
  ready=0
  for ((i = 0; i < 120; i++)); do
    if fixture_ready >/dev/null 2>&1; then
      ready=1
      break
    fi
    sleep 1
  done
  if [[ "$ready" != 1 ]]; then
    printf 'Owned %s fixture failed to become ready.\n' "$DB_DRIVER" >&2
    exit 1
  fi
  port="$(docker port "$container_name" "$port_spec")"
  port="${port##*:}"
  python3 - "$fixture_dir" "$port" "$container_name" "$fixture_id" "$image" "$DB_DRIVER" <<'PY'
import json,pathlib,sys
root,port,container,owner,image,driver=sys.argv[1:]
p=pathlib.Path(root)
(p/'manifest.json').write_text(json.dumps(dict(port=int(port),root=root,container=container,owner=owner,image=image,driver=driver)))
PY
  printf 'Running owned fixture %s (%s)\n' "$fixture_id" "$image"
  test_timeout=3m
  if [[ "${DATA_MATE_AGENT_TEST:-}" == 1 ]]; then
    test_timeout=15m
  fi
  # Job control gives the go runner and its test subprocess one owned group.
  set -m
  if [[ "$DB_DRIVER" == postgres ]]; then
    DATA_MATE_PG_FIXTURE="$fixture_dir/manifest.json" GOPROXY=off GOSUMDB=off \
      go test -mod=readonly -count=1 -timeout="$test_timeout" -v -run '^TestPostgresIntegration$' ./internal/database/postgres &
  else
    DATA_MATE_MYSQL_FIXTURE="$fixture_dir/manifest.json" GOPROXY=off GOSUMDB=off \
      go test -mod=readonly -count=1 -timeout="$test_timeout" -v -run '^TestMySQLIntegration$' ./internal/database/mysql &
  fi
  test_pid=$!
  wait "$test_pid"
  test_pid=''
  remove_resources
done
