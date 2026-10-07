# fargo
[![](https://img.shields.io/badge/hudl-OSS-orange.svg)](http://hudl.github.io/)

Netflix Eureka client in golang. Named for the show Eureka.

```go
c = fargo.NewConn("http://127.0.0.1:8080/eureka/v2")
c.GetApps() // returns a map[String]fargo.Application
```

# Testing

Tests can be executed using docker container. See the below section to build and start 
all the required containers. Once the Eureka containers are running, and fargo image is built then you can run the command as follows:

Run:
```
docker run --rm -v "$PWD":/go/src/github.com/hudl/fargo -w /go/src/github.com/hudl/fargo hudloss/fargo:master go test -v ./...
```
Note: If you are running bash for Windows add `MSYS_NO_PATHCONV=1 ` at the beginning.

The tests may need to be run a couple of times to allow changes to propagate
between the two Eureka servers. If the tests are failing, try running them again
approximately 30 seconds later.

If you are adding new packages to godep you may want to update the `hudloss/fargo` image first.

# Known Issues

Until [this PR](https://github.com/mitchellh/vagrant/pull/2742) is in an
official vagrant release, the Fedora 19 opscode box will fail when setting up
private networks in vagrant 1.2.5 and later.

# FAQ

Q: Is this a full client?

A: Not yet. It's very much a work in progress, and it's also being built with
consideration for Go idioms, which means some Java-isms will never be included.

Q: Does it cache?

A: No, it does not support caching records.

Q: Can I integrate this into my Go app and have it manage hearbeats to Eureka?

A: Glad you asked, of course you can. Just grab an application (for this example,
"TESTAPP")

```go
// register a couple instances, and then set up to only heartbeat one of them
e, _ := fargo.NewConnFromConfigFile("/etc/fargo.gcfg")
app, _ := e.GetApp("TESTAPP")
// starts a goroutine that updates the application on poll interval
e.UpdateApp(&app)
for {
    for _, ins := range app.Instances {
        fmt.Printf("%s, ", ins.HostName)
    }
    fmt.Println(len(app.Instances))
    <-time.After(10 * time.Second)
}
// You'll see all the instances at first, and within a minute or two all the
// ones that aren't heartbeating will disappear from the list. Note that after
// calling `UpdateApp` there's no need to manually update
```

# TODO

* Actually do something with AWS availability zone info
* Currently the load balancing is random, and does not give preference to
  servers within a zone.
* Make releases available on [gopkg.in](http://gopkg.in)

# Hacking

## Just Let Me Import Already

`go get github.com/hudl/fargo`

```go

package main

import (
    "github.com/hudl/fargo"
)

func main() {
    e, _ := fargo.NewConnFromConfigFile("/etc/fargo.gcfg")
    e.AppWatchChannel
}

```

## Adding Stuff

`go test` is your friend. I use the excellent [goconvey](http://goconvey.co/)
library in addition to the standard lib's `testing` package. If you add
something, write a test. If something is broken, write a test that reproduces
your issue or post repro steps to the issues on this repository. The tests
require that you have a eureka install and are designed to run against the
included [vagrant](http://vagrantup.com) specs.

## Verifying Releases

We're on semver and tag releases accordingly. The releases are signed and can
be verified with `git tag --verify vA.B.C`.

## Using Docker

Fargo is tested against two eureka versions, v1.1.147 and v1.3.1. To support
testing, we provide Docker containers that supply Eureka locally. Here's how to
get started.

1. Clone Fargo
1. If you don't have it, [install Docker](https://docs.docker.com/).
1. Change into the `docker` directory of this repository.

```bash
# Build the image, change "1.3.1" to 1.1.147 for the older version
VERSION=1.3.1
docker build -f Dockerfile-v$VERSION -t hudloss/eureka:$VERSION .
# Run two copies of the image (the containers will communicate with each other)
docker run -d --name eureka1 hudloss/eureka:$VERSION
docker run -d --name eureka2 hudloss/eureka:$VERSION

docker build -f Dockerfile-fargo-master -t hudloss/fargo:master .

# Run fargo container which launches shell
docker run --name fargo -it hudloss/fargo:master
# Once shell is attached, checkout your branch and run `go test ./...`
```

Once all containers are running, please check that the IP addresses for 
`eureka1` and `eureka2` are `172.17.0.2` and `172.17.0.3`. Eureka apps 
will be available at those IPs on port 8080 from inside containers. 
To expose the ports to your local machine, start containers using: 

```bash
# forward port 8080 to local port 48000
docker run -d -p 48000:8080 --name eureka1 hudloss/eureka:1.1.147
# forward port 8080 to local port 49000
docker run -d -p 49000:8080 --name eureka2 hudloss/eureka:1.3.1
```

# Contributors

* Ryan S. Brown (ryansb)
* Carl Quinn (cquinn)

# MIT License

```
The MIT License (MIT)

Copyright (c) 2013 Hudl <@Hudl>

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to
deal in the Software without restriction, including without limitation the
rights to use, copy, modify, merge, publish, distribute, sublicense, and/or
sell copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING
FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS
IN THE SOFTWARE.
```
