circular
========

Package circular implements an efficient thread-safe circular byte buffer to
keep in-memory logs. It implements both io.Writer and io.WriterTo.

[![GoDoc](https://godoc.org/github.com/maruel/circular?status.svg)](https://godoc.org/github.com/maruel/circular)
[![Build Status](https://travis-ci.org/maruel/circular.svg?branch=master)](https://travis-ci.org/maruel/circular)
[![Coverage Status](https://img.shields.io/coveralls/maruel/circular.svg)](https://coveralls.io/r/maruel/circular?branch=master)


Features
--------

  - No allocation during Write.
  - Full concurrent streaming (reading) support via multiple readers.
  - Automatically lock-less flushes for writers supporting http.Flusher.
  - Full test coverage.
