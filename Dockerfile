# The public demo: one static binary in `demo` mode on a distroless base,
# running as a non-root user with no shell. Migrations are not run from
# here; see README, "Deploying the demo".

FROM golang:1.27.4 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# The binary reports its commit on /healthz, and the console links source
# from it. Go stamps it from .git when the context has one; Render's doesn't,
# but passes the commit as this build arg when it is declared.
ARG RENDER_GIT_COMMIT
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/Arjunx156/ledgerline/internal/buildinfo.stamp=${RENDER_GIT_COMMIT}" \
    -o /out/ledgerline ./cmd/ledgerline

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/ledgerline /ledgerline
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/ledgerline"]
CMD ["demo"]
