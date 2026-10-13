# Build the static API binary, then ship only the binary on a minimal base.
# No source, no toolchain, no shell in the final image.
FROM golang:1.27-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /minipay ./cmd/api

FROM alpine:3.20

RUN apk add --no-cache ca-certificates

COPY --from=build /minipay /minipay

EXPOSE 8080

ENTRYPOINT ["/minipay"]
