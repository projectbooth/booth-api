#!/usr/bin/env sh
# Prints the environment variables that point the test suite at hack/docker-compose.yml's
# PostgreSQL. Usage: eval "$(sh hack/test-env.sh)"
cat <<'VARS'
export BOOTH_TEST_POSTGRES_DSN=postgres://booth_test:booth-test@127.0.0.1:15443/booth_test?sslmode=disable
VARS
