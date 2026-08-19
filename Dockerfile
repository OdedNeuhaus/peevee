# ── Build ─────────────────────────────────────────────────────
# Pinned to the build machine's own architecture. The Go build below
# cross-compiles instead, so no stage ever executes target-architecture code and
# a multi-arch build needs no QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
# Supplied by BuildKit per platform being built. The fallback keeps a plain
# `docker build` on the legacy builder, where it is unset, from passing an empty
# GOARCH to the compiler.
ARG TARGETARCH

WORKDIR /src

# Copy the module files first so dependency download is cached independently
# of source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO is off so the result is a static binary that runs on a scratch base.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-$(go env GOARCH)} go build \
      -trimpath \
      -ldflags="-s -w \
        -X github.com/OdedNeuhaus/peevee/internal/version.Version=${VERSION} \
        -X github.com/OdedNeuhaus/peevee/internal/version.Commit=${COMMIT} \
        -X github.com/OdedNeuhaus/peevee/internal/version.Date=${DATE}" \
      -o /out/peevee ./cmd/peevee

# ── Runtime ───────────────────────────────────────────────────
# Distroless static: no shell, no package manager, nothing to exploit. The UI is
# embedded in the binary, so there is nothing else to copy in.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/peevee /peevee

USER 65532:65532
EXPOSE 8080

ENTRYPOINT ["/peevee"]
