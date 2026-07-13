# syntax=docker/dockerfile:1

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/helm-render-service .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/helm-render-service /helm-render-service
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/helm-render-service"]
