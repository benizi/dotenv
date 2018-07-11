# Dotenv

Golang version of [Dotenv][dotenv]

# Introduction

I wanted to use `dotenv` without being tied to Ruby, and there were a couple of
behaviors I wanted to modify/remove.

# Compile

## Local

```sh
go get github.com/mattn/go-shellwords
go build
```

# Example


## File w/ vars:
$ cat .env
FOO=bar

## Current env doesn't contain $FOO
$ env | grep FOO

## Default command is `env`
$ dotenv .env | grep FOO
FOO=bar

## To prevent .env-file "detection", start the command after double dash
$ dotenv -- env | grep FOO
FOO=bar

## The whole point: vars are exported to the environment
$ dotenv -- sh -c 'echo FOO is $FOO'
FOO is bar
```

# Features and TODOs

- [x] Parse arguments as `.env` files until one fails
- [x] Allow inline `VAR=val` arguments à la `/usr/bin/env`
- [x] No shell "interpolation"
- [ ] Handle multiple kinds of input:
  - [x] raw: `VAR=val`
  - [x] shell: `VAR="val"` and `VAR='val'`
  - [x] exported shell: e.g. `export VAR='val'`
- [ ] Handle multiline vars
- [ ] Add "filters" à la `jq`
  - [ ] base64: `base64@VAR=encoded value`
  - [ ] url: `VAR@url=unescaped URL`
- [ ] Better builds
  - [x] `go get`-able
  - [ ] Ensure dependency is fetched for `go get`
  - [ ] Dockerize for easier build (no need for local `go`)

## License

Copyright © 2018 Benjamin R. Haskell

Distributed under the [MIT license](LICENSE).

[dotenv]: https://github.com/bkeepers/dotenv
