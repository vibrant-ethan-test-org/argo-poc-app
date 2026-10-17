FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY . .
# E2E_FAIL=<stage> makes the checks fail in that stage (promotion-controller e2e tests); empty for real releases.
ARG E2E_FAIL=
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.failStage=${E2E_FAIL}" -o /e2e .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /e2e /e2e
ENTRYPOINT ["/e2e"]
