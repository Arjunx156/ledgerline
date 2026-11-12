# The public demo: one static binary in `demo` mode on a distroless base,
# running as a non-root user with no shell. Migrations are not run from
# here; see README, "Deploying the demo".

FROM golang:1.27.4 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# .git comes along so the binary is stamped with its commit, which /healthz
# reports and the console links source from.
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ledgerline ./cmd/ledgerline

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/ledgerline /ledgerline
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/ledgerline"]
CMD ["demo"]
