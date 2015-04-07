// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package circular

import (
	"bytes"
	"testing"

	"github.com/maruel/ut"
)

func TestAutoFlushShallow(t *testing.T) {
	b := &bytes.Buffer{}
	a := AutoFlush(b, 0)
	a.Flush()
}

func TestAutoFlush(t *testing.T) {
	f := &flusherWriter{}
	a := AutoFlush(f, 0)
	a.Flush()
	ut.AssertEqual(t, true, f.flushed)
}
