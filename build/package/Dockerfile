FROM golang:alpine AS build

# Install tools required for project
# Run `docker build --no-cache .` to update dependencies
RUN apk add --no-cache git
RUN go get github.com/golang/dep/cmd/dep
RUN mkdir -p /go/src/github.com/culpinnis/k8sTicket

# List project dependencies with Gopkg.toml and Gopkg.lock
# These layers are only re-built when Gopkg files are updated
COPY Gopkg.toml Gopkg.lock  /go/src/github.com/culpinnis/k8sTicket/
WORKDIR /go/src/github.com/culpinnis/k8sTicket/
# Install library dependencies
RUN dep ensure --vendor-only

# Copy the entire project and build it
# This layer is rebuilt when a file changes in the project directory
COPY main.go /go/src/github.com/culpinnis/k8sTicket/
RUN GOARCH=amd64 CGO_ENABLED=0 GOOS=linux go build -o /bin/k8sTicket
#the exports are necessary https://forums.docker.com/t/getting-panic-spanic-standard-init-linux-go-178-exec-user-process-caused-no-such-file-or-directory-red-while-running-the-docker-image/27318/14
#otherwise docker won't start the container: standard_init_linux.go:211: exec user process caused "no such file or directory"                                                                                                                                                       exit:1

# This results in a single layer image
FROM scratch
COPY --from=build /bin/k8sTicket /bin/k8sTicket

EXPOSE 9690
ENTRYPOINT ["/bin/k8sTicket"]
