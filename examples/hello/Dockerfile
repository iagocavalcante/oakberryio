FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /hello .

FROM scratch
COPY --from=build /hello /hello
ENTRYPOINT ["/hello"]
