# The service previously ran from a developer checkout. A separate build stage now
# produces a static binary so the runtime image contains no compiler or source tree.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
COPY web ./web

ARG TARGETOS
ARG TARGETARCH

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
	-trimpath \
	-ldflags='-s -w' \
	-o /out/rozeta-dashboard \
	.

FROM scratch

# The application calls Rozeta over HTTPS, so the minimal runtime still needs the
# builder's CA bundle. It now runs as an unprivileged numeric user instead of root.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/rozeta-dashboard /rozeta-dashboard

USER 65532:65532

EXPOSE 8080

# Account credentials were previously read directly from the checkout. They remain
# runtime-only data and are now mounted as /data/account.csv instead of entering the image.
ENTRYPOINT ["/rozeta-dashboard"]
CMD ["-account", "/data/account.csv"]
