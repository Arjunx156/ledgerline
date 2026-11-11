set windows-shell := ["powershell.exe", "-NoLogo", "-NoProfile", "-Command"]

default: check

# Everything CI runs: format, vet, lint, vulnerabilities, tests
check: fmt-check vet lint vuln test

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

# Race detection needs cgo; CI always has it, Windows only with gcc on PATH
[unix]
test *args:
    go test -race -count=1 ./... {{args}}

[windows]
test *args:
    if (Get-Command gcc -ErrorAction SilentlyContinue) { $env:CGO_ENABLED = "1"; go test -race -count=1 ./... {{args}} } else { Write-Warning "No C compiler on PATH: tests run without -race here. CI always uses -race."; go test -count=1 ./... {{args}} }; exit $LASTEXITCODE

# Build the binary into bin/
build:
    go build -o bin/ ./cmd/ledgerline
