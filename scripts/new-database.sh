#!/bin/sh
# Create an empty PostgreSQL database, apply the migrations and point the
# development stack at it. The previous database is left in place so it can be
# restored by setting MAILQUAY_DB_NAME in .env back to its name.
#
# Usage: scripts/new-database.sh [database_name]
set -eu

cd "$(dirname "$0")/.."
compose="docker compose -f docker-compose-dev.yml"
database="${1:-mailquay_$(date -u +%Y%m%d_%H%M%S)}"
case "$database" in
	''|*[!a-z0-9_]*)
		echo "database name must use lowercase letters, digits and underscores" >&2
		exit 2
		;;
esac
password="${POSTGRES_PASSWORD:-mailquay-dev}"

if ! $compose exec -T postgres psql -U mailquay -d postgres -tAc \
	"SELECT 1 FROM pg_database WHERE datname = '$database'" | grep -q 1; then
	$compose exec -T postgres psql -U mailquay -d postgres -c "CREATE DATABASE \"$database\""
fi

$compose run --rm --no-deps --entrypoint /usr/local/bin/mailquay \
	-e "MAILQUAY_DATABASE_URL=postgres://mailquay:$password@postgres:5432/$database?sslmode=disable" \
	mailquay migrate up

temporary=$(mktemp)
if [ -f .env ]; then
	grep -v '^MAILQUAY_DB_NAME=' .env > "$temporary" || true
fi
printf 'MAILQUAY_DB_NAME=%s\n' "$database" >> "$temporary"
mv "$temporary" .env

$compose up -d --force-recreate mailquay
echo "MailQuay now uses database $database"
