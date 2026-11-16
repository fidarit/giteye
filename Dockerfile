FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/giteye ./cmd/giteye

FROM alpine:3.22
RUN addgroup -S giteye && adduser -S -G giteye giteye && apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/giteye /usr/local/bin/giteye
USER giteye
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/giteye"]
