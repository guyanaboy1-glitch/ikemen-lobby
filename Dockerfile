# Builds the Ikemen Online Lobby server (not the Ikemen Party app).
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /lobbyserver ./cmd/lobbyserver

FROM gcr.io/distroless/static-debian12
COPY --from=build /lobbyserver /lobbyserver
# Cloud hosts sit behind a proxy; this makes netplay hosts' public IPs correct.
ENV TRUST_PROXY=1
EXPOSE 8080
ENTRYPOINT ["/lobbyserver"]
