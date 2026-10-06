package main

import (
	"context"
	"encoding/json"
	"net"
)

type netConn = net.Conn

func dialUnix(ctx context.Context, path string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func stripIDs(b []byte) []byte {
	var v any
	_ = json.Unmarshal(b, &v)
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			delete(t, "@id")
			for _, c := range t {
				walk(c)
			}
		case []any:
			for _, c := range t {
				walk(c)
			}
		}
	}
	walk(v)
	out, _ := json.Marshal(v)
	return out
}
