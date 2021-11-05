package model

import (
	"bytes"
	"io"
)

type BodyRecorder struct {
	c io.Closer
	r io.Reader
	b bytes.Buffer
	x chan bool
}

func (r *BodyRecorder) Close() error {
	close(r.x)
	return r.c.Close()
}
func (r *BodyRecorder) Read(p []byte) (n int, err error) {
	return r.r.Read(p)
}
func (r *BodyRecorder) Body() []byte {
	<-r.x
	return r.b.Bytes()
}

func NewBodyRecorder(body io.ReadCloser) *BodyRecorder {
	r := &BodyRecorder{
		c: body,
		x: make(chan bool),
	}
	r.r = io.TeeReader(body, &r.b)
	return r
}
