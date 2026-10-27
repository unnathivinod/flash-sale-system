# ---- build stage: compile both Go programs ----
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
# Downloads + verifies libraries, then builds small static binaries.
RUN go mod tidy && CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/api ./cmd/api \
 && CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/worker ./cmd/worker

# ---- run stage: tiny image with just the binaries + frontend ----
FROM alpine:3.20
RUN adduser -D -H app
COPY --from=build /out/api /out/worker /usr/local/bin/
COPY frontend /static
USER app
EXPOSE 8080
CMD ["api"]
