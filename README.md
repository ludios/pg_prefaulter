This is a brutally butchered version of Joyent's [pg-prefaulter](https://github.com/joyent/pg_prefaulter).

Metrics removed, go modules added and some minor edits to the code to pass _staticcheck_ linting.

To build,

    go build -o pg_prefaulter main.go

Please see the [original README](https://github.com/joyent/pg_prefaulter/README.md) for motivation and further usage instructions.
