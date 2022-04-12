# Nethopper
This is Nethopper's fork of skupper.  To build nethopper, do the following:
- if you made changes, add & commit & tag this branch with the same version as you merge from upstream skupper (ie. 0.8.7)
- git tag -f 0.8.7

- build a skupper*.tgz
  - make clean
  - make all
  - make package

- Update nethopper/skupper/releases with for linux.tgz skupper-cli-<0.8.7>-linux-amd64.tgz, so that it can be download during nethopper-agent (agent-kube) repo docker build
cp release/linux.tgz release/skupper-cli-<0.8.7>-linux-amd64.tgz (replace version 0.8.7 with current version)

# now you can build nethopper/agent-kube
- follow the instructions in the agent-kube README.md

# Skupper

[![skupper](https://circleci.com/gh/skupperproject/skupper.svg?style=shield)](https://app.circleci.com/pipelines/github/skupperproject/skupper)

Skupper enables cloud communication by enabling you to create a Virtual Application Network.

This application layer network decouples addressing from the underlying network infrastructure.
This enables secure communication without a VPN.

You can use Skupper to create a network from namespaces in one or more Kubernetes clusters as described in the [Getting Started](https://skupper.io/start/index.html).
This guide describes a simple network, however there are no restrictions on the topology created which can include redundant paths.

Connecting one Skupper site to another site enables communication both ways.
Communication can occur using any path available on the network, that is, direct connections are not required to enable communication.

Skupper supports [anycast](https://en.wikipedia.org/wiki/Anycast) and [multicast](https://en.wikipedia.org/wiki/Multicast) communication using the application layer network (VAN), allowing you to configure your topology to match business requirements.

Skupper does not require any special privileges, that is, you do not require the `cluster-admin` role to create networks.

# Useful Links
Using Skupper

* [Getting Started](https://skupper.io/start/index.html)
* [Examples](https://skupper.io/examples/index.html)
* [Documentation](https://skupper.io/docs/index.html)
* [Skupper Docker](https://github.com/skupperproject/skupper-docker) allows you run Skupper on your laptop


Developing Skupper

* [Community](https://skupper.io/community/index.html)
* [Site controller](cmd/site-controller/README.md)
* [CLI](cmd/skupper/README.md) (This replaces the [Skupper CLI repo](https://github.com/skupperproject/skupper-cli))
* [Console](/skupperproject/gilligan)

# Licensing
Skupper uses the [Apache Qpid Dispatch Router](https://github.com/apache/qpid-dispatch) project and is released under the same [Apache License 2.0](https://github.com/skupperproject/skupper/blob/master/LICENSE).
