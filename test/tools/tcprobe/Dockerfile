FROM golang:1.19 as build

WORKDIR /workspace
COPY go.sum go.mod main.go .
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o tcprobe

FROM scratch

COPY --from=build /workspace/tcprobe /
ENTRYPOINT ["/tcprobe"]
