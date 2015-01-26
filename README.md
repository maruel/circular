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


Examples
--------

It is recommended to use multiple techniques simultaneously. For example, using
synchronous write to file coupled with web server.


Web
===

    import (
      "log"
      "net/http"
      "sync"

      "github.com/maruel/circular"
    )

    // Serves the circular log buffer over HTTP. This can be coupled with the
    // other techniques.
    logBuffer := circular.MakeBuffer(10 * 1024 * 1024)
    log.SetOutput(logBuffer)
    log.Printf("This line is not lost")
    http.HandleFunc("/",
        func(w http.ResponseWriter, r *http.Request) {
            w.Header().Set("Content-Type", "text/plain; charset=utf-8")
            logBuffer.WriteTo(w)
        })
    var wg sync.WaitGroup
    wg.Add(1)
    go func() {
        wg.Done()
        http.ListenAndServe(":6060", nil)
    }()
    wg.Wait()
    log.Printf("One more line")
    logBuffer.Close()


Asynchronous
============

    import (
      "log"
      "os"
      "sync"

      "github.com/maruel/circular"
    )

    // Saves the log to disk asynchrously. The main drawback, which is
    // significant with this use-case, is that logs will be partially lost on
    // panic().
    logBuffer := MakeBuffer(10 * 1024 * 1024)
    log.SetOutput(logBuffer)
    log.Printf("This line is not lost")
    f, err := os.OpenFile("app.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
    if err != nil {
        panic(err)
    }
    var wg sync.WaitGroup
    wg.Add(1)
    go func() {
        wg.Done()
        logBuffer.WriteTo(f)
    }()
    wg.Wait()
    log.Printf("One more line")
    logBuffer.Flush()
    logBuffer.Close()
    f.Close()


Stderr
======

    import (
      "log"
      "os"
      "sync"

      "github.com/maruel/circular"
    )

    // Prints the content asynchronously to stderr in addition to keeping the
    // log in memory.
    logBuffer := MakeBuffer(10 * 1024 * 1024)
    log.SetOutput(logBuffer)
    log.Printf("This line is not lost")
    var wg sync.WaitGroup
    wg.Add(1)
    go func() {
        wg.Done()
        logBuffer.WriteTo(os.Stderr)
    }()
    wg.Wait()
    log.Printf("One more line")
    logBuffer.Close()


Synchronous
===========

    import (
      "io"
      "log"
      "os"

      "github.com/maruel/circular"
    )
    // Safely writes to disk synchronously in addition to keeping a circular
    // buffer.
    logBuffer := MakeBuffer(10 * 1024 * 1024)
    log.SetOutput(logBuffer)
    log.Printf("This line is lost!")
    f, err := os.OpenFile("app.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
    if err != nil {
        panic(err)
    }
    log.SetOutput(io.MultiWriter(logBuffer, f))
    log.Printf("One more line")
    logBuffer.Close()
    f.Close()
