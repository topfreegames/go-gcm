GCM Library for Go [![GoDoc][godoc image]][godoc] [![Build Status][travis image]][travis] [![Coverage Status][codecov image]][codecov]
--

Provides the following functionality for Google Cloud Messaging:

1. Sending messages.
2. Listening to receiving messages.

Documentation: http://godoc.org/github.com/rounds/go-gcm

## Installation

    $ go get github.com/rounds/go-gcm

## Status

This is a rework of [go-gcm library](https://github.com/google/go-gcm). It has the following improvements:
* code refactored, http and xmpp clients separated
* monitors xmpp connection with xmpp pings, redials when it fails
* handles CONNECTION_DRAINING properly
* graceful close
* improved logging with logrus
* various govet/golint fixes
* [Travis][travis] and [Codecov][codecov] badges added

This library is in Beta. We will make an effort to support the library, but we reserve the right to make incompatible changes when necessary.

## Feedback

Please read CONTRIBUTING and raise issues here in Github.


[godoc]: https://godoc.org/github.com/rounds/go-gcm
[godoc image]: https://godoc.org/github.com/rounds/go-gcm?status.svg

[travis image]: https://travis-ci.org/rounds/go-gcm.svg
[travis]: https://travis-ci.org/rounds/go-gcm

[codecov image]: https://codecov.io/gh/rounds/go-gcm/branch/master/graph/badge.svg
[codecov]: https://codecov.io/gh/rounds/go-gcm
