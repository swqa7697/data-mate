#!/bin/bash
# Own every resource by a random exact name and label; never accept a DB URL.
set -euo pipefail
source "$(dirname "$0")/common.sh"
if [[ -n "${CI:-}" || "${DB_DRIVER:-}" != postgres || -n "${DATABASE_URL:-}${DB_URL:-}${DB_HOST:-}${DB_PORT:-}" ]]; then
  echo 'Integration requires DB_DRIVER=postgres, no external endpoint, and a non-CI session.' >&2
  exit 1
fi
case "${DB_IMAGE:-}" in
'') images=(postgres:16 postgres:18) ;;
postgres:16 | postgres:18) images=("$DB_IMAGE") ;;
*)
  echo 'DB_IMAGE must be postgres:16 or postgres:18.' >&2
  exit 1
  ;;
esac
need docker 'Install and start Docker.'
need openssl 'Install the Command Line Tools.'
need python3 'Install Python 3 for fixture manifests.'
docker info >/dev/null
fixture_dir="$(mktemp -d /tmp/data-mate-pg.XXXXXXXX)"
fixture_id="dm-pg-$(openssl rand -hex 12)"
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
for image in "${images[@]}"; do
  docker pull "$image" >/dev/null
  docker image inspect "$image" --format 'Fixture image: {{.Id}} {{json .RepoDigests}}'
  docker network create --label "$label" "$network_name" >/dev/null
  docker volume create --label "$label" "$volume_name" >/dev/null
  docker run -d --name "$container_name" --label "$label" --network "$network_name" \
    --publish 127.0.0.1::5432 --mount "type=volume,src=$volume_name,dst=/var/lib/postgresql" \
    --mount "type=bind,src=$fixture_dir,dst=/fixture,readonly" \
    -e POSTGRES_PASSWORD_FILE=/fixture/admin-password -e POSTGRES_DB=fixture -e PGDATA=/var/lib/postgresql/data \
    --entrypoint /bin/bash "$image" -c 'mkdir -p /tmp/tls; cp /fixture/server.key /fixture/server.crt /tmp/tls/; chown -R postgres:postgres /tmp/tls; chmod 600 /tmp/tls/server.key; exec docker-entrypoint.sh postgres -c ssl=on -c ssl_cert_file=/tmp/tls/server.crt -c ssl_key_file=/tmp/tls/server.key -c log_statement=none' >/dev/null
  ready=0
  for ((i = 0; i < 90; i++)); do
    if docker exec "$container_name" pg_isready -h 127.0.0.1 -U postgres -d fixture >/dev/null 2>&1; then
      ready=1
      break
    fi
    sleep 1
  done
  if [[ "$ready" != 1 ]]; then
    echo 'Owned PostgreSQL fixture failed to become ready.' >&2
    exit 1
  fi
  # Explicit export mode creates review candidates, never updates embedded policy
  # or substitutes for integration acceptance. Reuse the owned fixture lifecycle.
  if [[ -n "${CATALOG_OUTPUT_DIR:-}" ]]; then
    python3 - "$CATALOG_OUTPUT_DIR" "$container_name" "$image" "$project_dir/internal/database/postgres/sqlpolicy/catalog.sql" <<'PYEXPORT'
import json, pathlib, subprocess, sys
out, container, image, sql_path = sys.argv[1:]
root = pathlib.Path(out).resolve(strict=True)
if not root.is_dir() or not root.is_relative_to(pathlib.Path('/tmp').resolve()) or root == pathlib.Path('/tmp').resolve():
    raise SystemExit('CATALOG_OUTPUT_DIR must be an existing directory beneath /tmp')
def query(sql):
    return subprocess.check_output(['docker', 'exec', '-i', container, 'psql', '-X', '-qAt', '-v', 'ON_ERROR_STOP=1', '-U', 'postgres', '-d', 'fixture'], input=sql, text=True).strip()
version = int(query('SHOW server_version_num;'))
major = version // 10000
if major not in (16, 18):
    raise SystemExit('Unaudited fixture major')
raw = query('SET search_path=pg_catalog;\n' + pathlib.Path(sql_path).read_text() + ';')
# Preserve sorted object keys and query-defined semantic row ordering.
with (root / f'catalog{major}.json').open('x') as f:
    document = json.loads(raw)
    sections = []
    for key in sorted(document):
        rows = ',\n'.join('  ' + json.dumps(row, separators=(',', ':'), sort_keys=True) for row in document[key])
        sections.append(' ' + json.dumps(key) + ': [\n' + rows + '\n ]')
    f.write('{\n' + ',\n'.join(sections) + '\n}\n')
metadata = dict(server_version_num=version, image=image, image_metadata=json.loads(subprocess.check_output(['docker', 'image', 'inspect', image], text=True))[0]['RepoDigests'])
with (root / f'catalog{major}.provenance.json').open('x') as f:
    json.dump(metadata, f, indent=2)
    f.write('\n')
print(f'Exported PostgreSQL {major} candidates to {root}; integration tests not run in export mode')
PYEXPORT
    remove_resources
    continue
  fi
  port="$(docker port "$container_name" 5432/tcp)"
  port="${port##*:}"
  python3 - "$fixture_dir" "$port" "$container_name" "$fixture_id" "$image" <<'PY'
import json,pathlib,sys
root,port,container,owner,image=sys.argv[1:]
p=pathlib.Path(root)
(p/'manifest.json').write_text(json.dumps(dict(port=int(port),root=root,container=container,owner=owner,image=image)))
PY
  printf 'Running owned fixture %s (%s)\n' "$fixture_id" "$image"
  # Job control gives the go runner and its test subprocess one owned group.
  set -m
  DATA_MATE_PG_FIXTURE="$fixture_dir/manifest.json" GOPROXY=off GOSUMDB=off \
    go test -mod=readonly -count=1 -timeout=3m -v -run '^TestPostgresIntegration$' ./internal/database/postgres &
  test_pid=$!
  wait "$test_pid"
  test_pid=''
  remove_resources
done
