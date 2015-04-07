// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// +build !race

package circular

// Racy behavior only starts to show with large number of concurrent
// goroutines, generally in the range of 256~512. On the other hand this
// makes go test -race unbearably slow.
const ConcurrentRacers = 512
