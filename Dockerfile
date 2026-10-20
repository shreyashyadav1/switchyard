# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/switchyard ./cmd/switchyard

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/switchyard /switchyard
EXPOSE 8080
ENTRYPOINT ["/switchyard"]
CMD ["serve"]
