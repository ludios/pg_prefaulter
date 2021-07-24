# pg_prefaulter

This is a butchered version of [pg_prefaulter](https://github.com/joyent/pg_prefaulter), developed by Joyent / @sean-.

Metrics have been removed, go modules added and some minor edits to the code made to pass _staticcheck_ linting.

# Building

To build,

    go build -o pg_prefaulter main.go

# Usage

Please see the [original README](https://github.com/joyent/pg_prefaulter/blob/master/README.adoc) for motivation and further usage instructions.

# Notes

* Fixed an issue where in pg10+, the code would attempt to prefault files just ahead of the WAL files most recently received, instead of files just ahead of latest WAL files most recently replayed.

* Using an Intel Optane device, I seem to get best results with a small number of IO threads (4 or 8).

* I see regular errors due to SQL conflicts with recovery. WAL prefetch would be much better handled inside postgres itself. They're [thinking about it](https://www.postgresql.org/message-id/flat/20200324223152.v5qrjmjjo4aukktk%40alap3.anarazel.de#9214c5715fdd613bd62abf58f7b6b15e), but it seems it won't land until at least pg15.

* There is a version which uses `posix_fadvise()` instead of `pread()` on the _2021-07/posix_fadvise_ branch. Unfortunately, it turns out that ZFS does not actually support `posix_fadvise()`.
