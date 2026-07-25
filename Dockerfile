# ---- build ----
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/bot ./cmd/bot

# ---- runtime ----
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/bot /app/bot
# SQLite file lives on a mounted volume in compose.
EXPOSE 8090
USER nonroot:nonroot
ENTRYPOINT ["/app/bot"]
