FROM golang:alpine AS build

# Install tools required for project
# Run `docker build --no-cache .` to update dependencies
RUN apk add --no-cache git
RUN mkdir -p /go/src/github.com/ipb-halle/k8sTicket

# List project dependencies with Gopkg.toml and Gopkg.lock
# These layers are only re-built when Gopkg files are updated
COPY go.mod go.sum  /go/src/github.com/ipb-halle/k8sTicket/
WORKDIR /go/src/github.com/ipb-halle/k8sTicket/
# Install library dependencies
RUN go mod download

# Copy the entire project and build it
# This layer is rebuilt when a file changes in the project directory
COPY . /go/src/github.com/ipb-halle/k8sTicket/
RUN GOARCH=amd64 CGO_ENABLED=0 GOOS=linux go build ./cmd/k8sticket
#the exports are necessary https://forums.docker.com/t/getting-panic-spanic-standard-init-linux-go-178-exec-user-process-caused-no-such-file-or-directory-red-while-running-the-docker-image/27318/14
#otherwise docker won't start the container: standard_init_linux.go:211: exec user process caused "no such file or directory"                                                                                                                                                       exit:1

# This results in a single layer image
FROM scratch
COPY --from=build /go/src/github.com/ipb-halle/k8sTicket/k8sticket /app/k8sticket/
#Path detection needs to be fixed in our application!
COPY --from=build /go/src/github.com/ipb-halle/k8sTicket/web/ web/

EXPOSE 9001
#Entypoint with script and cd?
ENTRYPOINT ["/app/k8sticket/k8sticket"]
