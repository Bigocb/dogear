# build stage
FROM golang:1.27-alpine AS build
RUN apk add --no-cache gcc musl-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=1 go build -o /out/dogear .

# runtime stage
FROM alpine:3.20
RUN apk add --no-cache ca-certificates sqlite
WORKDIR /app
COPY --from=build /out/dogear /app/dogear
COPY web /app/web
ENV DOGEAR_DB=/data/dogear.db \
    DOGEAR_LIBRARY=/library \
    DOGEAR_WEB=/app/web \
    DOGEAR_ADDR=:8090
VOLUME ["/data", "/library"]
EXPOSE 8090
ENTRYPOINT ["/app/dogear"]