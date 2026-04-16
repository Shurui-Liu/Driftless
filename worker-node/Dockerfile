FROM golang:1.25-alpine AS build
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /worker .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build /worker /worker
ENTRYPOINT ["/worker"]
