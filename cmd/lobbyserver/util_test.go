package main

import (
	"io"
	"net/http"
)

func ioCopy(w io.Writer, resp *http.Response) (int64, error) {
	defer resp.Body.Close()
	return io.Copy(w, resp.Body)
}
