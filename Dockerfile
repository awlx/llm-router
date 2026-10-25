FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go metrics.go ./
RUN CGO_ENABLED=0 go build -o /llm-router .

FROM alpine:3.20
COPY --from=build /llm-router /usr/local/bin/llm-router
EXPOSE 1234
ENTRYPOINT ["/usr/local/bin/llm-router"]
