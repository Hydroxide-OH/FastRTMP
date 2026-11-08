# syntax=docker/dockerfile:1
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/fastrtmp ./cmd/fastrtmp

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -H fastrtmp
COPY --from=build /out/fastrtmp /usr/local/bin/fastrtmp
USER fastrtmp
EXPOSE 1935/tcp 8080/tcp
ENV FASTRTMP_ADDR=:1935 \
    FASTRTMP_HTTP=:8080
ENTRYPOINT ["/usr/local/bin/fastrtmp"]
