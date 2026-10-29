FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /readtheroom .

FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=build /readtheroom /readtheroom
COPY --from=build /src/config /app/config
ENV READTHEROOM_DATA=/app/data
EXPOSE 8080
ENTRYPOINT ["/readtheroom"]
