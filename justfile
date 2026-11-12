set windows-shell := ["powershell.exe", "-NoLogo", "-NoProfile", "-Command"]
set dotenv-load := true

default: check

# Everything CI runs: format, vet, lint, vulnerabilities, tests
check: fmt-check vet lint vuln test

# Start Postgres and wait until it accepts connections
up:
    docker compose up -d --wait

# Stop Postgres, keeping its data
down:
    docker compose down

# Delete the local database entirely
reset-db:
    docker compose down --volumes

# Apply pending migrations
migrate:
    go run ./cmd/ledgerline migrate up

# Roll back n migrations, or all
migrate-down n="1":
    go run ./cmd/ledgerline migrate down {{n}}

# Create a merchant and print its secret key, once
seed name="Test merchant":
    go run ./cmd/ledgerline seed "{{name}}"

# Run the API
run:
    go run ./cmd/ledgerline serve

# Run the API in demo mode, with sandbox sessions and the trace stream
demo:
    go run ./cmd/ledgerline demo

# Serve the bare demo page; open http://localhost:5173/?api=http://localhost:8080
page:
    uv run --no-project python -m http.server 5173 --bind 127.0.0.1 --directory web/skeleton

# Build the deployable image
image:
    docker build -t ledgerline .

# Open psql on the local database as the owner
psql:
    docker compose exec postgres psql -U ledgerline -d ledgerline

# Format all Go code in place
fmt:
    gofmt -w .

[unix]
fmt-check:
    #!/usr/bin/env sh
    unformatted="$(gofmt -l .)"
    if [ -n "$unformatted" ]; then echo "not gofmt-formatted:"; echo "$unformatted"; exit 1; fi

[windows]
fmt-check:
    $unformatted = gofmt -l .; if ($unformatted) { Write-Output "not gofmt-formatted:" $unformatted; exit 1 }

vet:
    go vet ./...

lint:
    go tool staticcheck ./...

vuln:
    go tool govulncheck ./...

# Tests need Postgres (just up). Race detection needs cgo: CI always has it,
# Windows only with gcc on PATH.
[unix]
test *args:
    go test -race -count=1 ./... {{args}}

[windows]
test *args:
    if (Get-Command gcc -ErrorAction SilentlyContinue) { $env:CGO_ENABLED = "1"; go test -race -count=1 ./... {{args}} } else { Write-Warning "No C compiler on PATH: tests run without -race here. CI always uses -race."; go test -count=1 ./... {{args}} }; exit $LASTEXITCODE

# Build the binary into bin/
build:
    go build -o bin/ ./cmd/ledgerline
