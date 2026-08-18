# ── Build ─────────────────────────────────────────────────────
FROM golang:1.24-alpine AS build

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

WORKDIR /src

# Copy the module files first so dependency download is cached independently
# of source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO is off so the result is a static binary that runs on a scratch base.
RUN CGO_ENABLED=0 GOOS=linux go build \
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
