# Cross-compiles on the build machine's arch, so multi-arch builds (amd64 + arm64) stay fast.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY src/ .
ARG VERSION=dev
ARG COMMIT=unknown
ARG UNHEALTHY_AFTER=
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.unhealthyAfter=${UNHEALTHY_AFTER}" -o /app .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /app /app
EXPOSE 8080
ENTRYPOINT ["/app"]
