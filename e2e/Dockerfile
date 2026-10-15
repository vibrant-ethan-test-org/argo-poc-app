FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" -o /e2e .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /e2e /e2e
ENTRYPOINT ["/e2e"]
