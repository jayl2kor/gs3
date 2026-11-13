package output

import (
	"bufio"
	"io"
)

func newTestWriter(w io.Writer) *bufio.Writer { return bufio.NewWriter(w) }
