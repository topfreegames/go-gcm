GCM Library for Go [![GoDoc][godoc image]][godoc] [![Build Status][travis image]][travis] [![Coverage Status][codecov image]][codecov]
--

Provides the following functionality for Google Cloud Messaging:

1. Sending messages via HTTP or XMPP.
2. Listening to receiving messages from XMPP.

Documentation: http://godoc.org/github.com/rounds/go-gcm

## Installation

    $ go get github.com/rounds/go-gcm

## Status

This is a rework of [go-gcm library](https://github.com/google/go-gcm). It has the following improvements:
* code refactored, HTTP and XMPP clients separated
* monitors XMPP connection with Pings, redials when ping fails
* handles CONNECTION_DRAINING properly
* graceful close
* improved logging with [logrus](https://github.com/Sirupsen/logrus)
* [ginkgo](https://onsi.github.io/ginkgo/) tests
* various govet/golint fixes
* automatic builds with [Travis][travis] and coverage with [Codecov][codecov] 

This library is in Beta. We will make an effort to support the library, but we reserve the right to make incompatible changes when necessary.

## Feedback

Please read CONTRIBUTING and raise issues here in Github.


[godoc]: https://godoc.org/github.com/rounds/go-gcm
[godoc image]: https://godoc.org/github.com/rounds/go-gcm?status.svg

[travis image]: https://travis-ci.org/rounds/go-gcm.svg
[travis]: https://travis-ci.org/rounds/go-gcm

[codecov image]: https://codecov.io/gh/rounds/go-gcm/branch/master/graph/badge.svg
[codecov]: https://codecov.io/gh/rounds/go-gcm
