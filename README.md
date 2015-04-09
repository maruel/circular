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
  - Automatic lock-less flushes for readers supporting http.Flusher.
  - Full test coverage.


Example
-------

  - Safely writes log to disk synchronously in addition to keeping a circular
    buffer.
  - Serves the circular log buffer over HTTP asychronously.
  - Writes log to stderr asynchronously.

This permits keeping all the output in case of a panic() on disk. Note that
panic() output itself is only written to stderr since it uses print() builtin.

    import (
      "log"
      "net/http"
      "sync"

      "github.com/maruel/circular"
    )

    func main() {
      logBuffer := circular.MakeBuffer(10 * 1024 * 1024)
      defer func() {
        // Flush ensures all readers have caught up.
        logBuffer.Flush()
        // Close gracefully closes the readers.
        logBuffer.Close()
      }()
      f, err := os.OpenFile("app.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
      if err != nil {
          panic(err)
      }
      defer f.Close()

      // Send to both circular buffer and file.
      log.SetOutput(io.MultiWriter(logBuffer, f))

      // Asynchronously write to stderr.
      go logBuffer.WriteTo(os.Stderr)

      log.Printf("This line is served over HTTP; file and stderr")

      http.HandleFunc("/",
          func(w http.ResponseWriter, r *http.Request) {
              w.Header().Set("Content-Type", "text/plain; charset=utf-8")
              // Streams the log buffer over HTTP until Close() is called.
              logBuffer.WriteTo(w)
          })
      http.ListenAndServe(":6060", nil)
    }
