ARG GO_IMAGE=golang:1.26.5-bookworm@sha256:1ecb7edf62a0408027bd5729dfd6b1b8766e578e8df93995b225dfd0944eb651
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
      -trimpath \
      -ldflags="-s -w -buildid= -X main.version=${VERSION}" \
      -o /out/lazarus ./cmd/lazarus
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
      ./scripts/third-party-licenses.sh /out/THIRD_PARTY_LICENSES

FROM scratch AS license-files

COPY LICENSE /LICENSE
COPY --from=build /out/THIRD_PARTY_LICENSES /THIRD_PARTY_LICENSES

FROM scratch

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
ARG SOURCE=unknown

LABEL org.opencontainers.image.title="Lazarus" \
      org.opencontainers.image.description="Maintenance-state service for Ansible Automation Platform" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="${SOURCE}" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/lazarus /lazarus
COPY LICENSE /LICENSE
COPY --from=build /out/THIRD_PARTY_LICENSES /THIRD_PARTY_LICENSES

EXPOSE 8080 8443

# OpenShift replaces this value with a namespace-assigned arbitrary UID. Keeping
# a numeric, non-root default also makes the image safe outside OpenShift.
USER 65532:0

ENTRYPOINT ["/lazarus"]
