FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/vibedock ./cmd/server

FROM alpine:3.22
RUN apk add --no-cache ca-certificates docker-cli tzdata
COPY --from=build /out/vibedock /usr/local/bin/vibedock
EXPOSE 8787 8788
ENTRYPOINT ["/usr/local/bin/vibedock"]
