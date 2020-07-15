libovsdb
========
[![Travis-CI](https://travis-ci.org/eBay/libovsdb.svg?branch=master)](https://travis-ci.org/eBay/libovsdb)

An OVSDB Library written in Go

## Purpose of Fork of Original libovsdb

eBay wishes to thank the original authors of libovsdb. The original library is
found here:

https://github.com/socketplane/libovsdb/

This fork has been created to encourage changes and patches to ensure this
version of the library works with [go-ovn](https://github.com/eBay/go-ovn).

## What is OVSDB?

OVSDB is the Open vSwitch Database Protocol.
It's defined in [RFC 7047](http://tools.ietf.org/html/rfc7047)
It's used mainly for managing the configuration of Open vSwitch, but it could also be used to manage your stamp collection. Philatelists Rejoice!

## Running the tests

To run integration tests, you'll need access to docker to run an Open vSwitch container.
Mac users can use [boot2docker](http://boot2docker.io)

    export DOCKER_IP=$(boot2docker ip)

    docker-compose run test /bin/sh
    # make test-local
    ...
    # exit
    docker-compose down

By invoking the command **make**, you will automatically get the same behaviour as what
is shown above. In other words, it will start the two containers and execute
**make test-local** from the test container.

## Dependency Management

We use [godep](https://github.com/tools/godep) for dependency management with rewritten import paths.
This allows the repo to be `go get`able.

To bump the version of a dependency, follow these [instructions](https://github.com/tools/godep#update-a-dependency)

## License

Modifications Copyright 2018-2019 eBay Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use
this file except in compliance with the License. You may obtain a copy of the
License at

https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed
under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.

## Third Party Code Attribution and Licenses

This software contains code licensed by third parties. In particular, see
https://github.com/socketplane/libovsdb.

Original contributors/maintainers: Dave Tucker, Madhu Venugopal, Sukhesh
Halemane

This software consists of voluntary contributions made by many individuals. For
exact contribution history, see the commit history.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use
this file except in compliance with the License. You may obtain a copy of the
License at

https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed
under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.
https://github.com/socketplane/libovsdb/blob/master/LICENSE
