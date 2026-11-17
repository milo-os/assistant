#!/usr/bin/env bash
# Starts a throwaway Postgres 17 for the Go test job and exports
# TEST_DATABASE_URL through $GITHUB_ENV.
#
# Why not a `services:` container: same-repo jobs run on the milo-linux ARC
# runner, an unprivileged pod with no Docker daemon and no usable sudo, so
# service containers cannot start there. The zonky embedded-postgres builds
# are relocatable Postgres binaries that run as an ordinary user, and they
# behave the same on GitHub-hosted runners, which fork PRs use.
set -euo pipefail

PG_VERSION=17.11.0
PG_PORT=5432

# tar needs xz to unpack the .txz; check both before downloading anything.
for tool in curl python3 sha256sum tar xz; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "start-postgres: $tool is required but not on PATH" >&2
    exit 1
  fi
done

case "$(uname -s)/$(uname -m)" in
  Linux/x86_64)
    artifact=linux-amd64
    sha256=0dd7b72b6f335b8ecfb355fa24c5781e8a93edd09880bb77eb52ebbf29b3e96d
    ;;
  Linux/aarch64 | Linux/arm64)
    artifact=linux-arm64v8
    sha256=8b042e0ea418b1927d95399207950da0734d27fe01e29503ade3bdc464ad4c2b
    ;;
  *)
    echo "start-postgres: no pinned Postgres build for $(uname -s)/$(uname -m)" >&2
    exit 1
    ;;
esac

root="${RUNNER_TEMP:?RUNNER_TEMP must be set}/postgres"
mkdir -p "$root/dist"

jar="$root/postgres.jar"
url="https://repo1.maven.org/maven2/io/zonky/test/postgres/embedded-postgres-binaries-${artifact}/${PG_VERSION}/embedded-postgres-binaries-${artifact}-${PG_VERSION}.jar"
curl --proto '=https' --proto-redir '=https' -fsSL --retry 3 -o "$jar" "$url"
echo "${sha256}  ${jar}" | sha256sum -c -

# The jar holds a single .txz. python3 is on both runner images; unzip is not
# guaranteed on the ARC image.
python3 -c 'import sys, zipfile; zipfile.ZipFile(sys.argv[1]).extractall(sys.argv[2])' "$jar" "$root/jar"
shopt -s nullglob
archives=("$root"/jar/*.txz)
shopt -u nullglob
if [ "${#archives[@]}" -ne 1 ]; then
  echo "start-postgres: expected exactly one .txz in $jar, found ${#archives[@]}" >&2
  exit 1
fi
tar -xJf "${archives[0]}" -C "$root/dist"

bin="$root/dist/bin"
data="$root/data"
"$bin/initdb" -D "$data" -U postgres --auth=trust --encoding=UTF8 --locale=C >/dev/null
"$bin/pg_ctl" -D "$data" -l "$root/postgres.log" -w -t 60 \
  -o "-c listen_addresses=127.0.0.1 -p ${PG_PORT} -c unix_socket_directories=''" start

echo "TEST_DATABASE_URL=postgres://postgres@127.0.0.1:${PG_PORT}/postgres?sslmode=disable" >>"${GITHUB_ENV:?GITHUB_ENV must be set}"
